import { describe, expect, test } from "bun:test";
import { DEFAULTS, Portical } from "../src/daemon.ts";
import { DEFAULT_HELPER_IMAGE, viaContainer } from "../src/namespace.ts";
import { decodeRelay, encodeRelay, parseRelayOutput, toRelayedRequest } from "../src/relay.ts";
import { FORWARD_LABEL } from "../src/resolve.ts";
import { UpnpGateway } from "../src/upnp.ts";
import { container, FakeDocker, network } from "./fakes/docker.ts";
import { FakeGateway } from "./fakes/gateway.ts";

const HOST = "192.168.1.5";
const CONTAINER_IP = "192.168.1.40";

describe("relay encoding", () => {
  test("survives a SOAP body with quotes, angle brackets and newlines", async () => {
    const body = `<?xml version="1.0"?>\n<s:Envelope><u:Add x="1">it's</u:Add></s:Envelope>`;
    const request = new Request("http://192.168.1.1:5000/ctl/IPConn", {
      method: "POST",
      headers: { SOAPAction: `"urn:x#AddPortMapping"` },
      body,
    });

    const round = decodeRelay(encodeRelay(await toRelayedRequest(request)));

    expect(round.body).toBe(body);
    expect(round.method).toBe("POST");
    expect(round.headers.soapaction).toBe(`"urn:x#AddPortMapping"`);
  });

  test("reads the result out of surrounding noise", () => {
    const output = `some runtime warning\n<<<PORTICAL-RELAY{"status":200,"body":"ok"}PORTICAL-RELAY>>>\ntrailing`;
    expect(parseRelayOutput(output)).toEqual({ status: 200, body: "ok" });
  });

  test("complains usefully when the container printed nothing usable", () => {
    expect(() => parseRelayOutput("exec: not found\n")).toThrow(/no usable result: exec: not found/);
  });
});

/**
 * The relay handler is an ordinary Handler, so a gateway built on it behaves
 * exactly like one talking over TCP. That is the whole point: UpnpGateway,
 * reconcile and the daemon never learn that a container was involved.
 */
describe("a gateway reached through a container", () => {
  function relaying() {
    const docker = new FakeDocker();
    docker.relay = () => framed(200, "<ok/>");
    return { docker };
  }

  test("performs the request from inside the named container", async () => {
    const { docker } = relaying();
    const handler = viaContainer(docker, "nginx");
    await handler(new Request("http://192.168.1.1:5000/ctl/IPConn", { method: "POST", body: "<x/>" }));

    expect(docker.relayed).toHaveLength(1);
    expect(docker.relayed[0]?.container).toBe("nginx");
    expect(docker.relayed[0]?.image).toBe(DEFAULT_HELPER_IMAGE);
    expect(docker.relayed[0]?.command[0]).toBe("relay");
  });

  test("uses the image it was given", async () => {
    const { docker } = relaying();
    const handler = viaContainer(docker, "nginx", "ghcr.io/someone/portical:dev");
    await handler(new Request("http://x/", { method: "POST", body: "" }));
    expect(docker.relayed[0]?.image).toBe("ghcr.io/someone/portical:dev");
  });

  test("passes the relayed status back through", async () => {
    const { docker } = relaying();
    docker.relay = () => framed(500, "<fault/>");
    const response = await viaContainer(docker, "nginx")(new Request("http://x/", { method: "POST", body: "" }));
    expect(response.status).toBe(500);
    expect(await response.text()).toBe("<fault/>");
  });
});

/**
 * The end of issue #1 and #6's story for macvlan.
 *
 * miniupnpd's secure_mode - the OpenWrt default, confirmed on a real gateway -
 * only lets a client map to its own address. A macvlan container's rule names
 * the container's address, so asking from the host is refused with 718. v1
 * sidestepped this by running upnpc inside the container; v2 asks directly
 * first and only pays for a container when the gateway insists.
 */
describe("a macvlan container behind a gateway in secure mode", () => {
  async function harness(secureMode = false) {
    const router = new FakeGateway({ secureMode }, undefined, HOST);
    const docker = new FakeDocker();
    const direct = await UpnpGateway.at(router.handler, router.rootUrl);
    const log: string[] = [];

    docker.relay = async (call) => {
      const relayed = decodeRelay(call.command[1]!);
      // The helper container reaches the same router, but its request arrives
      // from the container's own address - which is exactly what secure_mode
      // cares about.
      const response = await router.handlerFrom(CONTAINER_IP)(
        new Request(relayed.url, {
          method: relayed.method,
          headers: relayed.headers,
          body: relayed.body,
        }),
      );
      return framed(response.status, await response.text());
    };

    const asContainer = (name: string) =>
      new UpnpGateway(viaContainer(docker, name), direct.controlUrl, direct.serviceType);

    const daemon = new Portical(
      docker,
      direct,
      { ...DEFAULTS, hostAddress: HOST },
      (message) => log.push(message),
      asContainer,
    );

    docker.running = [
      container("direct", { [FORWARD_LABEL]: "8080/tcp" }, [network("lan", "macvlan", CONTAINER_IP)]),
    ];
    return { router, docker, daemon, log };
  }

  test("is forwarded by asking from inside the container", async () => {
    const { router, daemon } = await harness(true);

    await daemon.once();

    expect(router.mappings).toHaveLength(1);
    expect(router.mappings[0]?.internalClient).toBe(CONTAINER_IP);
  });

  test("relays through the container it belongs to, not some other one", async () => {
    const { docker, daemon } = await harness(true);
    await daemon.once();
    expect(docker.relayed.map((call) => call.container)).toEqual(["direct"]);
  });

  // Decided by the driver, not by trying and reacting to a refusal. 718 also
  // means a genuine conflict with someone else's mapping, so using it to mean
  // "wrong vantage point" would start a container to retry something no
  // vantage point can fix, and would hide the real reason.
  test("is asked from the container whether or not the gateway insists", async () => {
    const { router, docker, daemon } = await harness(false);

    await daemon.once();

    expect(router.mappings[0]?.internalClient).toBe(CONTAINER_IP);
    expect(docker.relayed.map((call) => call.container)).toEqual(["direct"]);
  });

  test("never starts a container for a bridge-networked rule", async () => {
    const { docker, daemon, router } = await harness(true);
    docker.running = [container("web", { [FORWARD_LABEL]: "8081/tcp" }, [network("br", "bridge", "172.17.0.9")])];

    await daemon.once();

    expect(router.mappings[0]?.internalClient).toBe(HOST);
    expect(docker.relayed).toEqual([]);
  });

  test("does not churn once the rule is in place", async () => {
    const { router, docker, daemon } = await harness(true);
    await daemon.once();

    router.actions.length = 0;
    docker.relayed.length = 0;
    await daemon.once();

    expect(router.actions).not.toContain("AddPortMapping");
    expect(docker.relayed).toEqual([]);
  });
});

/** The framing runRelay puts around its result. */
function framed(status: number, body: string): string {
  return `<<<PORTICAL-RELAY${JSON.stringify({ status, body })}PORTICAL-RELAY>>>`;
}
