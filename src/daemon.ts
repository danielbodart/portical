import type { DockerClient } from "./docker.ts";
import { describe } from "./model.ts";
import { reconcile, type Action, type DesiredForward } from "./reconcile.ts";
import { resolve } from "./resolve.ts";
import { UpnpError, type Gateway } from "./upnp.ts";

export interface Options {
  readonly label: string;
  readonly networkLabel: string;
  /** Seconds between reconcile passes. */
  readonly interval: number;
  /** Rewrite a mapping whose remaining lease is below this many seconds. */
  readonly renewWithin: number;
  /** Ask the gateway for this lease, in seconds. 0 means never expire. */
  readonly leaseDuration: number;
  readonly force: boolean;
  readonly steal: boolean;
  readonly dryRun: boolean;
  /** Remove our mappings on shutdown, rather than leaving them in place. */
  readonly cleanupOnExit: boolean;
  /**
   * Manage every mapping carrying Portical's description, wherever it points.
   *
   * Off by default, because it is only safe when there is exactly one Portical
   * on the network - see managedAddresses in reconcile.
   */
  readonly manageAll: boolean;
  /** The Docker host's address on the LAN, for bridge and host networking. */
  readonly hostAddress?: string;
}

export const DEFAULTS: Options = {
  label: "portical.upnp.forward",
  networkLabel: "portical.upnp.network",
  interval: 15,
  // Half a day, so a mapping the router downgraded to a 24 hour lease gets
  // renewed with plenty of room, without re-pushing every rule constantly.
  renewWithin: 43200,
  leaseDuration: 0,
  force: false,
  steal: false,
  dryRun: false,
  cleanupOnExit: false,
  manageAll: false,
};

export type Log = (message: string) => void;

/**
 * One reconcile pass, and the loop that repeats it.
 *
 * v1 offered `listen` and `poll` as alternatives, which is the wrong shape:
 * `listen` reacted to container starts but never renewed a lease or noticed a
 * container stopping, so its mappings quietly expired; `poll` renewed but
 * reacted no faster than its interval. They are not alternatives at all.
 * Watching tells you when something changed; the interval catches lease expiry
 * and any drift the events missed. This runs both into the same reconcile.
 */
export class Portical {
  constructor(
    private readonly docker: DockerClient,
    private readonly gateway: Gateway,
    private readonly options: Options,
    private readonly log: Log = console.log,
    /**
     * The same gateway, reached from inside a container's network namespace.
     *
     * Only used when asking directly has been refused because of the address
     * the rule points at - see secure_mode in namespace.ts.
     */
    private readonly asContainer?: (container: string) => Gateway,
  ) {}

  /**
   * Addresses this instance may remove mappings for.
   *
   * Seeded with our own host address and everything we have written this run,
   * so a container that stops while we are up is still cleaned up even though
   * it is no longer around to tell us its address.
   */
  private readonly written = new Set<string>();

  private lastSummary?: string;

  /**
   * Bring the gateway into line with the containers, once.
   *
   * Container listing failures propagate rather than being logged and skipped.
   * Removals are derived from absence, so reconciling against a container list
   * we failed to fetch would read as "everything stopped" and tear down every
   * live mapping.
   */
  async once(): Promise<Action[]> {
    const containers = await this.docker.containers(this.options.label);
    const { forwards, warnings } = resolve(containers, this.options);
    for (const warning of warnings) this.log(`Warning: ${warning}`);

    const actions = reconcile(forwards, await this.gateway.mappings(), {
      ...this.options,
      managedAddresses: this.options.manageAll ? undefined : this.addresses(forwards),
    });

    for (const action of actions) await this.apply(action);

    // Reported only when it changes, so a daemon reconciling every 15 seconds
    // says something the first time and then stays quiet until it has news.
    const summary = summarise(actions);
    if (summary !== this.lastSummary) {
      this.log(summary);
      this.lastSummary = summary;
    }

    return actions;
  }

  private addresses(forwards: readonly { internalClient?: string }[]): ReadonlySet<string> {
    const addresses = new Set(this.written);
    if (this.options.hostAddress) addresses.add(this.options.hostAddress);
    for (const forward of forwards) {
      if (forward.internalClient) addresses.add(forward.internalClient);
    }
    return addresses;
  }

  private async apply(action: Action): Promise<void> {
    switch (action.kind) {
      case "keep":
        return;

      case "conflict":
        this.log(
          `Conflict: ${action.forward.container} wants ${port(action)} but it is ` +
            `${action.reason.replace("already forwarded by something else ", "")}. ` +
            `Use --steal to take it over.`,
        );
        return;

      case "orphan":
        this.log(
          `Leaving ${action.mapping.protocol}/${action.mapping.externalPort} alone - ` +
            `${action.reason}. It will be reclaimed if that container returns, or ` +
            `expire on its own if the gateway set a lease.`,
        );
        return;

      case "remove": {
        this.log(`Removing ${action.mapping.protocol}/${action.mapping.externalPort} - ${action.reason}`);
        if (this.options.dryRun) return;
        await this.attempt(() =>
          this.gateway.remove(action.mapping.externalPort, action.mapping.protocol),
        );
        return;
      }

      case "add":
      case "renew":
      case "replace": {
        const { forward } = action;
        const verb = { add: "Adding", renew: "Renewing", replace: "Replacing" }[action.kind];
        this.log(`${verb} ${port(action)} for ${forward.container} - ${action.reason}`);
        if (this.options.dryRun) return;

        await this.attempt(async () => {
          const gateway = this.gatewayFor(forward);

          // Removed first only when the mapping points somewhere else, which
          // is the one case that needs it: several firmwares answer 718 rather
          // than overwriting when the internal address changes.
          //
          // Never for a renewal. AddPortMapping on an unchanged target updates
          // the lease in place, whereas deleting first leaves the port briefly
          // unforwarded. Established flows would not notice - they are carried
          // by conntrack, not by the redirect - but anyone trying to connect in
          // that window is refused, which on a game server reads as the server
          // having gone down for no reason.
          if (action.kind === "replace") {
            await gateway.remove(forward.rule.externalPort, forward.rule.protocol);
          }

          const internalClient = forward.internalClient ?? this.options.hostAddress;
          if (internalClient === undefined) {
            throw new Error(
              "cannot tell which address to forward to - set --root so the host's " +
                "address on the LAN can be worked out",
            );
          }

          const mapping = {
            externalPort: forward.rule.externalPort,
            protocol: forward.rule.protocol,
            internalPort: forward.rule.internalPort,
            internalClient,
            description: describe(forward.rule, forward.container),
            leaseDuration: this.options.leaseDuration,
          };

          await gateway.add(mapping);

          // Remembered so this mapping stays ours to clean up after the
          // container it belongs to has gone and can no longer be asked.
          this.written.add(internalClient);
        }, forward.target.kind === "container" ? relayHint : undefined);
        return;
      }
    }
  }

  /**
   * Which vantage point to ask the gateway from.
   *
   * Decided by the network driver, which is known up front, rather than by
   * trying from the host and reacting to a refusal. A rule for a macvlan or
   * ipvlan container names that container's own address, so the request has to
   * come from there - gateways running miniupnpd with secure_mode on only
   * allow a client to map to itself.
   *
   * Not inferred from the error code, because the code that comes back is 718
   * ConflictInMappingEntry, which also means a genuine collision with someone
   * else's mapping. Treating it as "wrong vantage point" would start a
   * container to retry a conflict that no vantage point can fix, and would
   * hide the real reason. The driver says which case this is with certainty.
   */
  private gatewayFor(forward: DesiredForward): Gateway {
    return forward.target.kind === "container" && this.asContainer
      ? this.asContainer(forward.container)
      : this.gateway;
  }

  /**
   * Report a failed rule instead of ending the run.
   *
   * One rule the router will not accept must not stop the other rules being
   * applied, and must not take the daemon down. v1 exited the whole process on
   * any upnpc failure, which is why a single stale mapping could stop every
   * forward on the host.
   */
  private async attempt(
    work: () => Promise<void>,
    hint?: (error: Error) => string | undefined,
  ): Promise<void> {
    try {
      await work();
    } catch (error) {
      this.log(`  FAILED: ${(error as Error).message}`);
      const explanation = hint?.(error as Error);
      if (explanation) this.log(`  ${explanation}`);
    }
  }

  /**
   * Reconcile continuously until the signal aborts.
   *
   * The event stream and the interval both do nothing but ask for another
   * pass, so a burst of container activity collapses into one reconcile rather
   * than one per event.
   */
  async run(signal: AbortSignal): Promise<void> {
    let pending: Promise<void> = Promise.resolve();
    let queued = false;

    const pass = () => {
      if (queued) return pending;
      queued = true;
      pending = pending.then(async () => {
        queued = false;
        try {
          await this.once();
        } catch (error) {
          // A pass that fails is not fatal: Docker may be restarting, or the
          // router may be briefly unreachable. The next tick tries again.
          this.log(`Error: ${(error as Error).message}`);
        }
      });
      return pending;
    };

    await pass();

    const ticker = setInterval(pass, this.options.interval * 1000);
    signal.addEventListener("abort", () => clearInterval(ticker), { once: true });

    this.log(
      `Watching for container changes, reconciling every ${this.options.interval}s...`,
    );

    try {
      // Reconnect rather than exit if the event stream ends. Losing the stream
      // is not fatal: the interval ticker keeps reconciling, so the worst case
      // is degrading to poll-only until a fresh stream opens. Earlier this
      // rethrew, so any dropped stream took the whole process down and
      // `restart: unless-stopped` bounced the container in a loop.
      //
      // The common case is not an error at all: Bun's fetch idle-times-out the
      // long-poll after a few minutes with no container events, so on a quiet
      // host the stream ends routinely. Reconnecting is therefore silent - the
      // interval pass still logs any real Docker trouble (its containers() call
      // fails and is reported), so there is nothing here worth a line every
      // few minutes.
      while (!signal.aborted) {
        try {
          for await (const event of this.docker.events(this.options.label, signal)) {
            if (signal.aborted) break;
            this.log(`Container ${event.container} ${event.action}`);
            await pass();
          }
        } catch {
          // Swallowed on purpose: reopen below unless we are shutting down.
        }
        if (signal.aborted) break;
        // A short backoff, so a stream that fails to open immediately - Docker
        // still coming up - does not spin. Interrupted at once on shutdown.
        await new Promise<void>((resolve) => {
          const timer = setTimeout(resolve, 1000);
          signal.addEventListener("abort", () => { clearTimeout(timer); resolve(); }, { once: true });
        });
      }
    } finally {
      clearInterval(ticker);
      await pending;
    }
  }

  /**
   * Remove every mapping we own.
   *
   * Only run when asked. A restarting Portical should not take down the
   * forwards it is about to put straight back, so leaving them in place is the
   * default and this is opt-in.
   */
  async cleanup(): Promise<void> {
    this.log("Removing Portical's mappings before exit...");
    const actions = reconcile([], await this.gateway.mappings(), {
      managedAddresses: this.options.manageAll ? undefined : this.addresses([]),
    });
    for (const action of actions) await this.apply(action);
  }
}

/**
 * Explain a failure that happened while asking as the container.
 *
 * A rule for a macvlan or ipvlan container is created by starting a throwaway
 * container joined to its network - see namespace.ts - so the things that can
 * go wrong are different from an ordinary request, and none of them are
 * obvious from the message Docker or the gateway returns.
 *
 * 718 keeps its plain meaning here. Because the request was already made from
 * the address the rule names, a conflict really is a conflict rather than the
 * gateway objecting to who asked.
 */
function relayHint(error: Error): string | undefined {
  if (error instanceof UpnpError) {
    return error.code === 718
      ? "The gateway says another mapping already claims that external port. " +
          "Portical asked as the container itself, so this is a real conflict " +
          "rather than the gateway objecting to who asked."
      : undefined;
  }
  return (
    "This rule points at the container's own address, so Portical asks the " +
    "gateway from inside that container's network by briefly running its own " +
    "image there. Check the Docker socket is writable and that Portical's " +
    "image can be pulled on this host, or name it with --helper-image."
  );
}

/**
 * One line saying what the pass did.
 *
 * "Nothing to do" is worth saying out loud. It is the normal state, and in v1
 * it was unreachable - every pass rewrote every rule - so seeing it is how you
 * know the churn is gone.
 */
function summarise(actions: readonly Action[]): string {
  const count = (kind: Action["kind"]) => actions.filter((action) => action.kind === kind).length;
  const parts = [
    ["added", count("add")],
    ["renewed", count("renew")],
    ["replaced", count("replace")],
    ["removed", count("remove")],
    ["in conflict", count("conflict")],
  ] as const;

  const changes = parts.filter(([, n]) => n > 0).map(([name, n]) => `${n} ${name}`);
  const orphans = count("orphan");
  if (orphans > 0) changes.push(`${orphans} left alone`);
  const correct = count("keep");

  if (changes.length === 0) {
    return correct === 0
      ? "No rules to manage"
      : `${correct} rule${correct === 1 ? "" : "s"} already correct, nothing to do`;
  }
  return [...changes, `${correct} already correct`].join(", ");
}

function port(action: Extract<Action, { forward: unknown }>): string {
  const { rule } = action.forward;
  return `${rule.externalPort}:${rule.internalPort}/${rule.protocol}`;
}
