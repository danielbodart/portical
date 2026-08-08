import type { Container, ContainerNetwork } from "./docker.ts";
import type { Rule } from "./model.ts";
import type { DesiredForward, Target } from "./reconcile.ts";
import { parseLabel } from "./rules.ts";

export const FORWARD_LABEL = "portical.upnp.forward";

/**
 * Names which network to forward to, for containers attached to several.
 *
 * Portical can usually pick on its own, but "usually" is not good enough when
 * the wrong choice sends traffic to an address the gateway cannot reach, so
 * there has to be a way to say so explicitly.
 */
export const NETWORK_LABEL = "portical.upnp.network";

export interface ResolveOptions {
  readonly label?: string;
  readonly networkLabel?: string;
  /**
   * The Docker host's address on the LAN, as reported by the gateway.
   *
   * Used as the target for bridge and host networking. Optional because it is
   * only known once the gateway has been contacted, and an unknown address is
   * better than a guessed one - see DesiredForward.internalClient.
   */
  readonly hostAddress?: string;
}

export interface Resolution {
  readonly forwards: readonly DesiredForward[];
  readonly warnings: readonly string[];
}

/**
 * Which network driver to prefer when a container is on more than one.
 *
 * macvlan and ipvlan come first because they give the container its own
 * address on the LAN, which the gateway can forward to directly. Bridge is
 * last: it works, but only via a second hop through the host's published
 * ports, so it is the fallback rather than the choice.
 */
const PREFERENCE: readonly string[] = ["macvlan", "ipvlan", "host", "bridge"];

/** Turn running containers into the set of forwards that ought to exist. */
export function resolve(
  containers: readonly Container[],
  options: ResolveOptions = {},
): Resolution {
  const label = options.label ?? FORWARD_LABEL;
  const networkLabel = options.networkLabel ?? NETWORK_LABEL;
  const forwards: DesiredForward[] = [];
  const warnings: string[] = [];

  for (const container of containers) {
    const value = container.labels[label];
    if (value === undefined || value.trim() === "") continue;

    const terms = parseLabel(value);
    for (const error of terms.errors) warnings.push(`${container.name}: ${error}`);

    const chosen = selectNetwork(container, container.labels[networkLabel]);
    if ("error" in chosen) {
      warnings.push(`${container.name}: ${chosen.error}`);
      continue;
    }
    for (const warning of chosen.warnings) warnings.push(`${container.name}: ${warning}`);

    const { target, internalClient } = describeTarget(chosen.network, options.hostAddress);

    // Explicit rules come first so that they win any collision with a rule
    // derived from `published`, which is the more specific intent.
    for (const rule of collapse([...terms.rules, ...(terms.published ? fromPublished(container) : [])])) {
      forwards.push({ rule, container: container.name, target, internalClient });
    }
  }

  return { forwards, warnings };
}

type Selection =
  | { readonly network: ContainerNetwork; readonly warnings: readonly string[] }
  | { readonly error: string };

/**
 * Choose which of a container's networks the forward should point at.
 *
 * Issue #1 is here. v1 read the network with the Go template
 * `{{range $key, $_ := .NetworkSettings.Networks}}{{ $key }}{{end}}`, which
 * concatenates every network name with no separator - a container on four
 * networks yielded one nonsense name like "networkanetworkbnetworkc". The
 * lookup then failed, the driver came back empty, and every rule was skipped
 * as "Unsupported network driver: ". Reported by two people, and it makes
 * Portical unusable with exactly the reverse-proxy setups it is most useful
 * for.
 */
function selectNetwork(container: Container, requested: string | undefined): Selection {
  if (container.networks.length === 0) {
    return { error: "is not attached to any network, skipping" };
  }

  if (requested !== undefined && requested.trim() !== "") {
    const named = container.networks.find((network) => network.name === requested.trim());
    if (!named) {
      const available = container.networks.map((network) => network.name).join(", ");
      return { error: `is not attached to network '${requested.trim()}' (has: ${available}), skipping` };
    }
    if (!PREFERENCE.includes(named.driver)) {
      return { error: `network '${named.name}' has unsupported driver '${named.driver}', skipping` };
    }
    return { network: named, warnings: [] };
  }

  const supported = container.networks
    .filter((network) => PREFERENCE.includes(network.driver))
    // Sorted by name within a driver so that a container on two macvlans
    // resolves the same way on every pass. An unstable choice would flip the
    // target address back and forth and rewrite the rule forever.
    .sort((a, b) => PREFERENCE.indexOf(a.driver) - PREFERENCE.indexOf(b.driver) || a.name.localeCompare(b.name));

  const best = supported[0];
  if (!best) {
    const drivers = [...new Set(container.networks.map((network) => network.driver))].join(", ");
    return { error: `has no supported network driver (found: ${drivers}), skipping` };
  }

  const warnings: string[] = [];
  const rivals = supported.filter((network) => network.driver === best.driver);
  if (rivals.length > 1) {
    warnings.push(
      `is on ${rivals.length} ${best.driver} networks (${rivals.map((n) => n.name).join(", ")}), ` +
        `forwarding to '${best.name}' - set ${NETWORK_LABEL} to choose`,
    );
  }

  return { network: best, warnings };
}

function describeTarget(
  network: ContainerNetwork,
  hostAddress: string | undefined,
): { target: Target; internalClient: string | undefined } {
  // macvlan and ipvlan put the container on the LAN in its own right, so the
  // gateway forwards straight to it and we know the address from Docker.
  if (network.driver === "macvlan" || network.driver === "ipvlan") {
    return {
      target: { kind: "container", container: network.name },
      internalClient: network.ipAddress === "" ? undefined : network.ipAddress,
    };
  }

  // bridge and host both land on the Docker host, and Docker forwards on from
  // there. The container's own bridge address is not routable from the
  // gateway, so it deliberately plays no part here.
  return { target: { kind: "host" }, internalClient: hostAddress };
}

/**
 * The rules implied by `published`.
 *
 * External and internal port are both the *host* port, matching v1. The
 * gateway forwards to the host, and Docker's own published-port mapping takes
 * it the rest of the way to the container - so the container port is Docker's
 * business, not the router's.
 */
function fromPublished(container: Container): Rule[] {
  return container.published.map((port) => ({
    externalPort: port.hostPort,
    internalPort: port.hostPort,
    protocol: port.protocol,
  }));
}

/** Drop later rules that would fight an earlier one for the same gateway slot. */
function collapse(rules: readonly Rule[]): Rule[] {
  const byIdentity = new Map<string, Rule>();
  for (const rule of rules) {
    const key = `${rule.protocol}/${rule.externalPort}`;
    if (!byIdentity.has(key)) byIdentity.set(key, rule);
  }
  return [...byIdentity.values()];
}
