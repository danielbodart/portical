/**
 * Making an HTTP request from inside another container's network namespace.
 *
 * Needed because macvlan and ipvlan containers have their own address on the
 * LAN, and gateways running miniupnpd with `secure_mode` on - the OpenWrt
 * default - only let a client create or delete a mapping pointing at the
 * address it is asking *from*. Portical asks from the host, so a rule naming
 * the container's address is refused with 718.
 *
 * v1 solved this by running `upnpc` in a throwaway container sharing the
 * target's network namespace. The same trick works here, except that what is
 * relayed is an ordinary HTTP request rather than a command line - so the
 * result is just another Handler, and UpnpGateway is entirely unaware that
 * anything unusual is happening.
 */

export interface RelayedRequest {
  readonly url: string;
  readonly method: string;
  readonly headers: Record<string, string>;
  readonly body: string;
}

export interface RelayedResponse {
  readonly status: number;
  readonly body: string;
}

/**
 * Encode as base64 so the whole request survives being a single argv entry.
 *
 * SOAP bodies contain quotes, angle brackets and newlines, and this value
 * passes through the Docker API into a container's command line.
 */
export function encodeRelay(value: RelayedRequest): string {
  return Buffer.from(JSON.stringify(value), "utf8").toString("base64");
}

export function decodeRelay(value: string): RelayedRequest {
  return JSON.parse(Buffer.from(value, "base64").toString("utf8")) as RelayedRequest;
}

export async function toRelayedRequest(request: Request): Promise<RelayedRequest> {
  return {
    url: request.url,
    method: request.method,
    headers: Object.fromEntries(request.headers.entries()),
    body: request.method === "GET" || request.method === "HEAD" ? "" : await request.text(),
  };
}

/**
 * Perform a relayed request and report the result on stdout.
 *
 * This runs inside the throwaway container, where there is no Docker socket
 * and nothing else to do. The result is framed between markers because the
 * runtime may print its own diagnostics to the same stream, and because with a
 * TTY attached Docker gives back one undifferentiated pipe.
 */
export async function runRelay(encoded: string): Promise<number> {
  const result = await relay(encoded);
  process.stdout.write(`${START}${JSON.stringify(result)}${END}`);
  return 0;
}

async function relay(encoded: string): Promise<RelayedResponse> {
  try {
    const request = decodeRelay(encoded);
    const response = await fetch(request.url, {
      method: request.method,
      headers: request.headers,
      body: request.body === "" ? undefined : request.body,
      signal: AbortSignal.timeout(10_000),
    });
    return { status: response.status, body: await response.text() };
  } catch (error) {
    // Reported as a response rather than thrown, so the caller sees why the
    // gateway could not be reached from inside the container - which is a
    // different and more interesting failure than the container not starting.
    return { status: 599, body: `relay failed: ${(error as Error).message}` };
  }
}

const START = "<<<PORTICAL-RELAY";
const END = "PORTICAL-RELAY>>>";

/** Recover the result from whatever else the container printed. */
export function parseRelayOutput(output: string): RelayedResponse {
  const start = output.indexOf(START);
  const end = output.indexOf(END, start);
  if (start === -1 || end === -1) {
    throw new Error(`relay produced no usable result: ${output.trim().slice(0, 500)}`);
  }
  return JSON.parse(output.slice(start + START.length, end)) as RelayedResponse;
}
