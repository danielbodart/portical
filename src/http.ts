/**
 * The uniform interface: everything that talks HTTP is this one function type.
 *
 * The Docker Engine API and the internet gateway are both just handlers, so
 * both can be swapped for an in-memory implementation with no server, no port
 * and no socket. That is what makes the daemon testable end to end - the fake
 * gateway in the tests is a function, not a process.
 *
 * It is also why nothing below this line knows whether it is talking over a
 * unix socket or TCP.
 */
export type Handler = (request: Request) => Promise<Response>;

/** Talks to a real server over TCP. */
export const http: Handler = (request) => fetch(request);

/**
 * Talks to a real server over a unix socket.
 *
 * Bun's fetch speaks unix sockets natively, so reaching the Docker Engine API
 * needs no client library and no `docker` binary in the image.
 */
export function overUnixSocket(path: string): Handler {
  return (request) => fetch(request, { unix: path });
}

/** Fail a request that takes too long, so a silent gateway cannot hang a pass. */
export function withTimeout(handler: Handler, milliseconds: number): Handler {
  return async (request) => {
    // The incoming request may already carry a signal - a daemon shutting down
    // aborts its event stream that way - so both have to be honoured.
    const signal = request.signal
      ? AbortSignal.any([request.signal, AbortSignal.timeout(milliseconds)])
      : AbortSignal.timeout(milliseconds);
    return handler(new Request(request, { signal }));
  };
}

/** Throw with the server's own words rather than a bare status code. */
export async function ok(response: Response): Promise<Response> {
  if (!response.ok) {
    const body = (await response.text()).trim();
    throw new Error(`${response.status} ${response.statusText}${body === "" ? "" : `: ${body}`}`);
  }
  return response;
}
