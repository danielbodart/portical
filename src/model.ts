/**
 * The vocabulary the rest of Portical is written in.
 *
 * The important distinction here is Rule vs Mapping. A Rule is something we
 * *want* to exist, read off a container label. A Mapping is something that
 * *does* exist, read back off the gateway. Reconciliation is entirely a matter
 * of comparing the two sets, so keeping them as separate types stops the two
 * ideas from being conflated the way they were in the shell version.
 */

export type Protocol = "tcp" | "udp";

export const PROTOCOLS: readonly Protocol[] = ["tcp", "udp"];

/** A port forward we want to exist, derived from a container's label. */
export interface Rule {
  readonly externalPort: number;
  readonly internalPort: number;
  readonly protocol: Protocol;
}

/** A port forward that currently exists on the internet gateway. */
export interface Mapping {
  readonly externalPort: number;
  readonly protocol: Protocol;
  readonly internalClient: string;
  readonly internalPort: number;
  readonly description: string;
  readonly remoteHost: string;
  /** Seconds until the gateway drops this mapping; 0 means it never expires. */
  readonly leaseDuration: number;
}

/**
 * The identity of a forward, as the gateway itself understands it.
 *
 * An IGD keys its mapping table on (RemoteHost, ExternalPort, Protocol), so
 * two rules that agree on these collide no matter what else they say. v1
 * decided "does this rule already exist?" by substring-matching the free-text
 * description against `upnpc -l` output, which is why issue #6 happens: any
 * router that truncates or rewrites the description makes every rule look
 * missing, so every pass deletes and re-adds it. Comparing identity instead of
 * prose is the fix.
 *
 * RemoteHost is left out because Portical only ever writes wildcard rules.
 */
export function identity(forward: { externalPort: number; protocol: Protocol }): string {
  return `${forward.protocol}/${forward.externalPort}`;
}

/** The marker that tells us a mapping on the gateway is one of ours. */
export const DESCRIPTION_PREFIX = "portical:";

/**
 * Descriptions are generated in exactly v1's format, deliberately.
 *
 * Anyone upgrading has live rules on their router carrying the old text, and
 * ownership is decided by the prefix. Changing the format would orphan every
 * pre-existing rule - Portical would neither recognise nor clean them up, and
 * would sit alongside its own duplicates.
 */
export function describe(rule: Rule, machine: string): string {
  return `${DESCRIPTION_PREFIX} (${rule.externalPort}:${rule.internalPort}/${rule.protocol}) ${machine}`;
}

/** Whether a mapping on the gateway was put there by Portical. */
export function isOurs(mapping: Mapping): boolean {
  return mapping.description.startsWith(DESCRIPTION_PREFIX);
}
