import type { DockerClient } from "./docker.ts";
import { describe } from "./model.ts";
import { reconcile, type Action } from "./reconcile.ts";
import { resolve } from "./resolve.ts";
import type { Gateway } from "./upnp.ts";

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
  ) {}

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

    const actions = reconcile(forwards, await this.gateway.mappings(), this.options);

    for (const action of actions) await this.apply(action);
    return actions;
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

      case "remove": {
        this.log(`Removing ${action.mapping.protocol}/${action.mapping.externalPort} - ${action.reason}`);
        if (this.options.dryRun) return;
        await this.attempt(() =>
          this.gateway.remove(action.mapping.externalPort, action.mapping.protocol),
        );
        return;
      }

      case "add":
      case "replace": {
        const { forward } = action;
        this.log(
          `${action.kind === "add" ? "Adding" : "Replacing"} ${port(action)} for ` +
            `${forward.container} - ${action.reason}`,
        );
        if (this.options.dryRun) return;

        await this.attempt(async () => {
          // Removed first only when replacing. Several firmwares answer 718
          // rather than overwriting when the internal address changes, and a
          // rule that is already correct is never routed through here, so
          // this cannot become the churn of issue #6.
          if (action.kind === "replace") {
            await this.gateway.remove(forward.rule.externalPort, forward.rule.protocol);
          }

          const internalClient = forward.internalClient ?? this.options.hostAddress;
          if (internalClient === undefined) {
            throw new Error(
              "cannot tell which address to forward to - set --root so the host's " +
                "address on the LAN can be worked out",
            );
          }

          await this.gateway.add({
            externalPort: forward.rule.externalPort,
            protocol: forward.rule.protocol,
            internalPort: forward.rule.internalPort,
            internalClient,
            description: describe(forward.rule, forward.container),
            leaseDuration: this.options.leaseDuration,
          });
        });
        return;
      }
    }
  }

  /**
   * Report a failed rule instead of ending the run.
   *
   * One rule the router will not accept must not stop the other rules being
   * applied, and must not take the daemon down. v1 exited the whole process on
   * any upnpc failure, which is why a single stale mapping could stop every
   * forward on the host.
   */
  private async attempt(work: () => Promise<void>): Promise<void> {
    try {
      await work();
    } catch (error) {
      this.log(`  FAILED: ${(error as Error).message}`);
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
      for await (const event of this.docker.events(this.options.label, signal)) {
        if (signal.aborted) break;
        this.log(`Container ${event.container} ${event.action}`);
        await pass();
      }
    } catch (error) {
      if (!signal.aborted) throw error;
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
    const actions = reconcile([], await this.gateway.mappings());
    for (const action of actions) await this.apply(action);
  }
}

function port(action: Extract<Action, { forward: unknown }>): string {
  const { rule } = action.forward;
  return `${rule.externalPort}:${rule.internalPort}/${rule.protocol}`;
}
