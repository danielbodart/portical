import { identity, isOurs, type Mapping, type Rule } from "./model.ts";

/**
 * Where the traffic for a forward should land.
 *
 * `host` covers the bridge and host network drivers, where the gateway
 * forwards to the Docker host and Docker takes it from there. `container`
 * covers macvlan and ipvlan, where the container has its own address on the
 * LAN and the gateway forwards straight to it.
 */
export type Target =
  | { readonly kind: "host" }
  | { readonly kind: "container"; readonly network: string };

/** A rule, resolved against the container it came from. */
export interface DesiredForward {
  readonly rule: Rule;
  readonly container: string;
  readonly target: Target;
  /**
   * The address the mapping should point at, when we can work it out.
   *
   * Left undefined when it cannot be determined, in which case the address is
   * not compared. Deliberately: an unknown address must never be read as a
   * mismatch, because a mismatch means "replace", and replacing a working rule
   * on every pass is exactly the churn issue #6 reports.
   */
  readonly internalClient?: string;
}

export type Action =
  | { readonly kind: "add"; readonly forward: DesiredForward; readonly reason: string }
  | { readonly kind: "replace"; readonly forward: DesiredForward; readonly existing: Mapping; readonly reason: string }
  /**
   * Extend a mapping's lease without disturbing it.
   *
   * Kept apart from `replace` because how it is carried out matters. A rule
   * that is merely being renewed is rewritten in place with AddPortMapping,
   * never deleted first: the gateway's redirect only governs *new* flows, so
   * removing it briefly is invisible to a connection already established -
   * until the moment someone tries to join, which is when a game server
   * appears to have gone down for no reason.
   */
  | { readonly kind: "renew"; readonly forward: DesiredForward; readonly existing: Mapping; readonly reason: string }
  | { readonly kind: "remove"; readonly mapping: Mapping; readonly reason: string }
  | { readonly kind: "orphan"; readonly mapping: Mapping; readonly reason: string }
  | { readonly kind: "keep"; readonly forward: DesiredForward; readonly mapping: Mapping }
  | { readonly kind: "conflict"; readonly forward: DesiredForward; readonly existing: Mapping; readonly reason: string };

export interface ReconcileOptions {
  /** Rewrite every rule even if it already looks correct. */
  readonly force?: boolean;
  /**
   * Rewrite a mapping whose remaining lease is below this many seconds.
   *
   * Portical asks for an unlimited lease, but plenty of routers silently
   * downgrade that to a finite one, and a `listen`-only daemon in v1 had
   * nothing that would ever notice the mapping quietly expiring.
   */
  readonly renewWithin?: number;
  /**
   * Take over a matching mapping that another tool appears to own.
   *
   * Off by default. Two tools that both believe they own an external port will
   * otherwise overwrite each other forever, and a fight between daemons is far
   * harder to diagnose than a logged conflict.
   */
  readonly steal?: boolean;
  /**
   * The addresses this Portical is entitled to forward to.
   *
   * Ownership cannot be decided by the description alone. Every Portical on a
   * network writes the same "portical:" prefix, so a second one - on another
   * host, or run by hand from a laptop to look at something - sees the first's
   * rules as its own, finds no container asking for them, and deletes them.
   * The two then delete each other's rules forever.
   *
   * Restricting removals to mappings that point somewhere this instance could
   * legitimately have sent traffic makes that impossible. An empty set means
   * manage everything, which is the old behaviour and what --manage-all asks
   * for.
   */
  readonly managedAddresses?: ReadonlySet<string>;
}

/**
 * Work out what has to change on the gateway.
 *
 * Pure, and deliberately so - this is where every interesting decision in
 * Portical is made, so it should be testable without a router, a daemon or a
 * clock.
 *
 * The caller must pass a *complete* set of desired forwards. Removals are
 * derived from absence, so reconciling against a partial list would read as
 * "these containers are gone" and tear down live rules. Anything that fails to
 * enumerate containers must abort rather than reconcile with what it managed
 * to collect.
 */
export function reconcile(
  desired: readonly DesiredForward[],
  actual: readonly Mapping[],
  options: ReconcileOptions = {},
): Action[] {
  const { force = false, renewWithin = 0, steal = false, managedAddresses } = options;

  /** Ours, and pointing somewhere we could legitimately have sent traffic. */
  const managed = (mapping: Mapping): boolean =>
    isOurs(mapping) &&
    (managedAddresses === undefined ||
      managedAddresses.size === 0 ||
      managedAddresses.has(mapping.internalClient));

  const existing = new Map(actual.map((mapping) => [identity(mapping), mapping]));
  const wanted = new Set(desired.map((forward) => identity(forward.rule)));
  const actions: Action[] = [];

  for (const forward of desired) {
    const mapping = existing.get(identity(forward.rule));

    if (!mapping) {
      actions.push({ kind: "add", forward, reason: "no mapping on the gateway" });
      continue;
    }

    // Another Portical's rule, on another host. Reported rather than taken:
    // silently reassigning it would break whatever is running over there, and
    // the two instances would then fight over the port indefinitely.
    if (isOurs(mapping) && !managed(mapping)) {
      actions.push({
        kind: "conflict",
        forward,
        existing: mapping,
        reason: `already forwarded by another Portical to ${mapping.internalClient}`,
      });
      continue;
    }

    if (!isOurs(mapping)) {
      if (!steal) {
        actions.push({
          kind: "conflict",
          forward,
          existing: mapping,
          reason: `already forwarded by something else (${describeOwner(mapping)})`,
        });
        continue;
      }
      // Rewritten even when it already points the right way, so that our
      // description replaces the other tool's. Ownership is decided by that
      // description, so a mapping left carrying someone else's text is one we
      // would refuse to touch on the next pass and never clean up.
      actions.push({
        kind: "replace",
        forward,
        existing: mapping,
        reason: `taking over from ${describeOwner(mapping)}`,
      });
      continue;
    }

    // Drift is the only thing that justifies taking a rule down. Where the
    // mapping already points at the right place, it is rewritten in place -
    // there is nothing to move it away from.
    const drift = mismatch(forward, mapping);
    if (drift) {
      actions.push({ kind: "replace", forward, existing: mapping, reason: drift });
      continue;
    }

    if (force) {
      actions.push({ kind: "renew", forward, existing: mapping, reason: "forced" });
      continue;
    }

    if (expiring(mapping, renewWithin)) {
      actions.push({
        kind: "renew",
        forward,
        existing: mapping,
        reason: `lease expires in ${mapping.leaseDuration}s`,
      });
      continue;
    }

    actions.push({ kind: "keep", forward, mapping });
  }

  // Anything of ours the containers no longer ask for. This is the whole of
  // issue #2 - a stopped container simply stops contributing desired forwards,
  // so its rules fall out here without any need to watch for stop events.
  for (const mapping of actual) {
    if (wanted.has(identity(mapping)) || !isOurs(mapping)) continue;

    if (managed(mapping)) {
      actions.push({ kind: "remove", mapping, reason: "no container asks for it" });
      continue;
    }

    // Ours by description, but pointing somewhere we could not have sent
    // traffic. Either another Portical wrote it, or it belonged to a macvlan
    // container of ours that has since gone and taken its address with it.
    //
    // Reported rather than removed, and rather than passed over in silence.
    // We cannot tell those two cases apart, and deleting on a guess would
    // break another host's forwarding. Left alone, such a rule is reclaimed
    // if the container returns to the same address, and expires on its own if
    // the gateway granted a finite lease - which most do.
    actions.push({
      kind: "orphan",
      mapping,
      reason: `points at ${mapping.internalClient}, which is not ours to manage`,
    });
  }

  return actions;
}

/**
 * Why an existing mapping does not match what we want, or undefined if it does.
 *
 * Note what is *not* compared: the description. v1 decided this by substring
 * matching the description against the gateway's listing, so any router that
 * truncated or rewrote it made every rule look absent and every pass deleted
 * and re-added it - dropping live connections every interval, and failing with
 * code 714 when there was nothing there to delete. That is issue #6.
 */
function mismatch(forward: DesiredForward, mapping: Mapping): string | undefined {
  if (mapping.internalPort !== forward.rule.internalPort) {
    return `points at port ${mapping.internalPort}, expected ${forward.rule.internalPort}`;
  }
  if (forward.internalClient !== undefined && mapping.internalClient !== forward.internalClient) {
    return `points at ${mapping.internalClient}, expected ${forward.internalClient}`;
  }
  return undefined;
}

/** A lease of 0 means the mapping never expires, so it is never expiring. */
function expiring(mapping: Mapping, renewWithin: number): boolean {
  return mapping.leaseDuration > 0 && mapping.leaseDuration <= renewWithin;
}

function describeOwner(mapping: Mapping): string {
  return mapping.description.trim() === ""
    ? `${mapping.internalClient}:${mapping.internalPort}, no description`
    : mapping.description;
}
