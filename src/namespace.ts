import type { DockerClient } from "./docker.ts";
import type { Handler } from "./http.ts";
import { encodeRelay, parseRelayOutput, toRelayedRequest } from "./relay.ts";

/**
 * The image used for the throwaway relay container.
 *
 * It is Portical's own image running Portical's own `relay` subcommand - there
 * is no second image to build or publish. This constant is only the fallback
 * for when we cannot work out what we are running as, which matters to anyone
 * on a fork, a pinned tag or a locally built image.
 */
export const DEFAULT_HELPER_IMAGE = "danielbodart/portical:latest";

/**
 * The image this process is running as, if it can be worked out.
 *
 * A container cannot simply ask what image it came from, and Portical usually
 * runs with host networking, so its hostname is the host's rather than its own
 * container id. But Docker bind-mounts /etc/hostname and friends from
 * /var/lib/docker/containers/<id>/, and those paths are visible in
 * /proc/self/mountinfo whatever the network mode - so the id is recoverable,
 * and the Engine API turns it into an image name.
 */
export async function ownImage(docker: DockerClient): Promise<string | undefined> {
  try {
    const mountinfo = await Bun.file("/proc/self/mountinfo").text();
    const id = /\/(?:docker\/)?containers\/([0-9a-f]{64})\//.exec(mountinfo)?.[1];
    return id === undefined ? undefined : await docker.imageOf(id);
  } catch {
    // Not in a container, no such file, or Docker declined to say. The default
    // is a perfectly good answer, so this is not worth reporting.
    return undefined;
  }
}

/**
 * A Handler that makes its requests from inside a container's network
 * namespace.
 *
 * Because everything in Portical reaches the network through a Handler, this
 * is all it takes to speak to the gateway *as* a macvlan container:
 * UpnpGateway, reconcile and the daemon are unchanged and unaware.
 *
 * It works by starting a throwaway container from Portical's own image, joined
 * to the target's network namespace, which performs the one request and prints
 * the result. That is v1's trick, with an HTTP request relayed instead of a
 * command line.
 *
 * It costs several Docker API calls and a container start per request, so it
 * is used only when asking directly has been refused - see the fallback in
 * daemon.ts.
 */
export function viaContainer(
  docker: DockerClient,
  container: string,
  image: string = DEFAULT_HELPER_IMAGE,
): Handler {
  return async (request) => {
    const encoded = encodeRelay(await toRelayedRequest(request));
    const output = await docker.runInNetworkOf(container, image, ["relay", encoded]);
    const { status, body } = parseRelayOutput(output);
    return new Response(body, {
      status,
      // SOAP faults arrive as HTTP 500 with the reason in the body, and
      // Response refuses to be constructed with a null-body status, so the
      // status is passed through as-is only when it can carry one.
      headers: { "Content-Type": 'text/xml; charset="utf-8"' },
    });
  };
}
