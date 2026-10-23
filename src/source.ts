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
