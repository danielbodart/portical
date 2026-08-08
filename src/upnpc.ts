import type { Mapping, Protocol } from "./model.ts";

/**
 * Everything worth keeping from a run of `upnpc -l`.
 *
 * The addresses come for free in the same output, and both are wanted: the
 * local address is what a bridge-networked forward has to point at, and the
 * external address is the single most useful thing to show a user who is
 * trying to work out whether their forward actually reached the internet.
 */
export interface Listing {
  readonly mappings: readonly Mapping[];
  readonly localAddress?: string;
  readonly externalAddress?: string;
}

/**
 * miniupnpc prints its redirection table with `printf("%2d %s %5s->%s:%-5s
 * '%s' '%s' %s\n", ...)`, giving lines like:
 *
 *     0 TCP  9999->192.168.1.10:8080  'portical: (9999:8080/tcp) nginx' '' 0
 *
 * The description is matched greedily and the remote host is not, so that a
 * description containing its own quote characters still parses - the trailing
 * lease number anchors the whole thing to the end of the line. Descriptions are
 * user-supplied by way of other tools sharing the router, so they cannot be
 * assumed to be well behaved.
 */
const MAPPING =
  /^\s*\d+\s+(TCP|UDP)\s+(\d+)->([0-9a-f.:]+):(\d+)\s+'(.*)'\s+'([^']*)'\s+(\d+)\s*$/i;

const LOCAL_ADDRESS = /^Local LAN ip address\s*:\s*(\S+)/i;
const EXTERNAL_ADDRESS = /^ExternalIPAddress\s*=\s*(\S+)/i;

/**
 * Read the output of `upnpc -l`.
 *
 * Anything unrecognised is skipped rather than treated as an error. The banner,
 * the discovery block and the connection status all share this stream, routers
 * vary in what they report, and a new line in miniupnpc's preamble must not be
 * able to stop Portical reading the mapping table.
 */
export function parseListing(output: string): Listing {
  const mappings: Mapping[] = [];
  let localAddress: string | undefined;
  let externalAddress: string | undefined;

  for (const line of output.split("\n")) {
    const mapping = MAPPING.exec(line);
    if (mapping) {
      const [, protocol, externalPort, internalClient, internalPort, description, remoteHost, leaseDuration] = mapping;
      mappings.push({
        protocol: protocol!.toLowerCase() as Protocol,
        externalPort: Number(externalPort),
        internalClient: internalClient!,
        internalPort: Number(internalPort),
        description: description!,
        remoteHost: remoteHost!,
        leaseDuration: Number(leaseDuration),
      });
      continue;
    }

    localAddress ??= LOCAL_ADDRESS.exec(line)?.[1];
    externalAddress ??= EXTERNAL_ADDRESS.exec(line)?.[1];
  }

  return { mappings, localAddress, externalAddress };
}
