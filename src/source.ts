import { Channel } from "./channel.ts";
import { identity } from "./model.ts";
import type { DesiredForward } from "./reconcile.ts";

/**
 * The forwards a source wants, and what it had to say about getting them.
 *
 * Warnings travel with the forwards rather than being logged by the source,
 * so the daemon decides how they are reported and the tests can read them.
 */
export interface Desired {
  readonly forwards: readonly DesiredForward[];
  readonly warnings: readonly string[];
}

/**
 * Something happened that may have changed what a source wants.
 *
 * Carries nothing the daemon acts on. Every event means the same thing -
 * reconcile again - and the next pass asks desired() afresh, so the only use
 * for what is in here is the line it puts in the log.
 *
 * The line is optional so a source can ask for a pass without saying so. A
 * source that has to listen to everything to catch the few things it cares
 * about - systemd announces every unit on the host - would otherwise fill the
 * log with units that have nothing to do with port forwarding.
 */
export interface SourceEvent {
  readonly description?: string;
}

/**
 * Where the forwards come from.
 *
 * Docker containers are one source and systemd units another; the daemon and
 * the reconcile underneath it do not care which. A source answers two
 * questions: what should exist right now, and a stream that says when that
 * answer may have changed. The interval pass covers anything the stream
 * misses, so a source whose stream is lossy, or absent, is still correct - only
 * slower.
 */
export interface Source {
  /**
   * Every forward this source wants, or a rejection.
   *
   * Must never resolve with a partial list. Removals are derived from
   * absence, so a source that skipped what it failed to read would tear down
   * the live forwards it could not see - fail the whole pass instead.
   */
  desired(): Promise<Desired>;

  /**
   * Changes that may affect desired(), until the signal aborts.
   *
   * Allowed to end or throw: the daemon reopens it after a short backoff.
   */
  events(signal: AbortSignal): AsyncIterable<SourceEvent>;
}

/**
 * Several sources as one.
 *
 * Portical should run every source on a host from one instance. Two instances
 * on the same host forward to the same address, so each takes the other's
 * mappings for its own, finds nothing asking for them, and removes them.
 *
 * desired() fails if any source fails, for the reason desired() must never be
 * partial. Two sources asking for the same slot on the gateway would rewrite
 * each other on every pass, so the first source named keeps it and the other
 * is warned about - the same first-wins rule as within a label.
 */
export function combine(sources: readonly Source[]): Source {
  if (sources.length === 1) return sources[0]!;

  return {
    async desired() {
      const answers = await Promise.all(sources.map((source) => source.desired()));
      const forwards: DesiredForward[] = [];
      const warnings: string[] = [];
      const claimed = new Map<string, DesiredForward>();

      for (const answer of answers) {
        warnings.push(...answer.warnings);
        // Claims are recorded per source, after it has been read, so that a
        // source's own forwards never count as a clash with each other -
        // two containers on one port is the reconcile's business, as it was
        // before there were several sources.
        const mine: [string, DesiredForward][] = [];
        for (const forward of answer.forwards) {
          const slot = identity(forward.rule);
          const owner = claimed.get(slot);
          if (owner) {
            warnings.push(
              `${forward.container} and ${owner.container} both ask for ${slot}, ` +
                `forwarding it for ${owner.container}`,
            );
            continue;
          }
          forwards.push(forward);
          mine.push([slot, forward]);
        }
        for (const [slot, forward] of mine) if (!claimed.has(slot)) claimed.set(slot, forward);
      }

      return { forwards, warnings };
    },

    events(signal) {
      return merge(sources.map((source) => (inner: AbortSignal) => source.events(inner)), signal);
    },
  };
}

/**
 * Every stream's events as one, until the signal aborts.
 *
 * Ends when any of them ends, and throws when any of them throws, after
 * stopping the rest: the daemon then reopens the whole set, which is simpler
 * than reopening one and costs nothing the interval does not cover.
 */
async function* merge(
  streams: readonly ((signal: AbortSignal) => AsyncIterable<SourceEvent>)[],
  signal: AbortSignal,
): AsyncIterable<SourceEvent> {
  const inner = new AbortController();
  const stop = () => inner.abort();
  signal.addEventListener("abort", stop);
  const merged = new Channel<SourceEvent>();

  for (const stream of streams) {
    (async () => {
      for await (const event of stream(inner.signal)) merged.push(event);
    })().then(
      () => merged.end(),
      (error: Error) => merged.fail(error),
    );
  }

  try {
    yield* merged;
  } finally {
    signal.removeEventListener("abort", stop);
    inner.abort();
  }
}
