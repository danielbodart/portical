import { beforeEach, describe, expect, test } from "bun:test";
import { DEFAULTS, Portical, type Options } from "../src/daemon.ts";
import { FORWARD_LABEL, NETWORK_LABEL } from "../src/resolve.ts";
import { UpnpGateway } from "../src/upnp.ts";
import { container, FakeDocker, network } from "./fakes/docker.ts";
import { FakeGateway, type Quirks } from "./fakes/gateway.ts";

const HOST = "192.168.1.5";

/**
 * The whole daemon, wired to a Docker and a router that exist only in memory.
 *
 * No process, no socket, no port. This is what the uniform Handler interface
 * buys: the tests below are the real reconcile loop, the real UPnP SOAP client
 * and the real Engine API shapes, with only the far end replaced.
 */
async function portical(quirks: Quirks = {}, overrides: Partial<Options> = {}) {
  const docker = new FakeDocker();
  const router = new FakeGateway(quirks);
  const gateway = await UpnpGateway.at(router.handler, router.rootUrl);
  const log: string[] = [];
  const daemon = new Portical(
    docker,
    gateway,
    { ...DEFAULTS, hostAddress: HOST, ...overrides },
    (message) => log.push(message),
  );
  return { docker, router, daemon, log };
}

const nginx = (label = "9999:80/tcp") => container("nginx", { [FORWARD_LABEL]: label });

describe("a container that wants a port forwarded", () => {
  test("gets one", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx()];

    await daemon.once();

    expect(router.mappings).toEqual([
      {
        externalPort: 9999,
        protocol: "tcp",
        internalPort: 80,
        internalClient: HOST,
        description: "portical: (9999:80/tcp) nginx",
        remoteHost: "",
        leaseDuration: 0,
      },
    ]);
  });

  test("gets both protocols when it does not name one", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx("25565")];
    await daemon.once();
    expect(router.mappings.map((m) => m.protocol).sort()).toEqual(["tcp", "udp"]);
  });
});

/**
 * Issue #6, end to end.
 *
 * v1 asked "is my description a substring of the gateway's listing?" and any
 * router that answered differently from how it was asked made every rule look
 * missing. It then deleted and re-added on every pass - dropping live
 * connections each interval, and failing with 714 when there was nothing to
 * delete. These assert on the SOAP the router actually receives, because
 * "nothing changed" is precisely the property that was broken.
 */
describe("a rule that is already correct", () => {
  test("is not touched on the next pass", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx()];
    await daemon.once();

    router.actions.length = 0;
    await daemon.once();

    expect(router.actions).not.toContain("DeletePortMapping");
    expect(router.actions).not.toContain("AddPortMapping");
  });

  test("is not touched even after twenty passes", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx()];
    await daemon.once();

    router.actions.length = 0;
    for (let pass = 0; pass < 20; pass++) await daemon.once();

    expect(router.actions.filter((a) => a === "AddPortMapping")).toEqual([]);
    expect(router.mappings).toHaveLength(1);
  });

  // The router in issue #6 stores a shortened description. v1 could never
  // match it again, so it churned forever.
  test("survives a router that truncates the description", async () => {
    const { docker, router, daemon } = await portical({ descriptionLimit: 22 });
    docker.running = [container("qbittorrent", { [FORWARD_LABEL]: "6881/udp" })];
    await daemon.once();
    expect(router.mappings[0]?.description).toBe("portical: (6881:6881/u");

    router.actions.length = 0;
    await daemon.once();

    expect(router.actions).not.toContain("DeletePortMapping");
  });

  test("survives a router that ends its table with 501 rather than 713", async () => {
    const { docker, router, daemon } = await portical({ endOfTableCode: 501 });
    docker.running = [nginx()];
    await daemon.once();

    router.actions.length = 0;
    await daemon.once();

    expect(router.actions).not.toContain("AddPortMapping");
    expect(router.mappings).toHaveLength(1);
  });

  test("is renamed without being rewritten when its container is renamed", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx()];
    await daemon.once();

    docker.running = [container("nginx-renamed", { [FORWARD_LABEL]: "9999:80/tcp" })];
    router.actions.length = 0;
    await daemon.once();

    expect(router.actions).not.toContain("AddPortMapping");
  });
});

/** Issue #2, which falls out of reconciling rather than watching for stops. */
describe("a container that goes away", () => {
  test("has its forward removed", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx()];
    await daemon.once();

    docker.running = [];
    await daemon.once();

    expect(router.mappings).toEqual([]);
  });

  test("leaves other containers' forwards in place", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx(), container("plex", { [FORWARD_LABEL]: "32400/tcp" })];
    await daemon.once();

    docker.running = [container("plex", { [FORWARD_LABEL]: "32400/tcp" })];
    await daemon.once();

    expect(router.mappings.map((m) => m.externalPort)).toEqual([32400]);
  });

  test("leaves another tool's forwards alone", async () => {
    const { docker, router, daemon } = await portical();
    router.given({ externalPort: 32400, protocol: "tcp", description: "plex" });
    docker.running = [];

    await daemon.once();

    expect(router.mappings).toHaveLength(1);
  });
});

/** Issue #1: the reverse-proxy setup that v1 could not resolve at all. */
describe("a container on several networks", () => {
  const traefik = (labels: Record<string, string> = {}) =>
    container("traefik", { [FORWARD_LABEL]: "80/tcp,443/tcp", ...labels }, [
      network("networka", "bridge", "172.18.0.2"),
      network("networkb", "bridge", "172.19.0.2"),
      network("lan", "macvlan", "192.168.1.40"),
    ]);

  test("is forwarded to its address on the LAN", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [traefik()];

    await daemon.once();

    expect(router.mappings).toHaveLength(2);
    expect(router.mappings.every((m) => m.internalClient === "192.168.1.40")).toBe(true);
  });

  test("can be told which network to use", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [traefik({ [NETWORK_LABEL]: "networkb" })];

    await daemon.once();

    expect(router.mappings.every((m) => m.internalClient === HOST)).toBe(true);
  });
});

describe("leases", () => {
  test("a mapping the router downgraded is renewed as it nears expiry", async () => {
    const { docker, router, daemon } = await portical({ leaseLimit: 600 }, { renewWithin: 3600 });
    docker.running = [nginx()];
    await daemon.once();
    expect(router.mappings[0]?.leaseDuration).toBe(600);

    router.actions.length = 0;
    await daemon.once();

    expect(router.actions).toContain("AddPortMapping");
  });

  test("a mapping that never expires is left alone", async () => {
    const { docker, router, daemon } = await portical({}, { renewWithin: 86400 });
    docker.running = [nginx()];
    await daemon.once();

    router.actions.length = 0;
    await daemon.once();

    expect(router.actions).not.toContain("AddPortMapping");
  });
});

describe("conflicts", () => {
  const wanted = () => container("jellyfin", { [FORWARD_LABEL]: "32400/tcp" });

  test("another tool's mapping is reported and left alone", async () => {
    const { docker, router, daemon, log } = await portical();
    router.given({ externalPort: 32400, protocol: "tcp", description: "plex" });
    docker.running = [wanted()];

    await daemon.once();

    expect(router.mappings[0]?.description).toBe("plex");
    expect(log.join("\n")).toContain("Use --steal to take it over.");
  });

  test("and is taken over when asked", async () => {
    const { docker, router, daemon } = await portical({}, { steal: true });
    router.given({ externalPort: 32400, protocol: "tcp", description: "plex" });
    docker.running = [wanted()];

    await daemon.once();

    expect(router.mappings[0]?.description).toBe("portical: (32400:32400/tcp) jellyfin");
  });
});

describe("failure", () => {
  // v1 exited the process on any upnpc failure, so one rule the router would
  // not accept stopped every other forward on the host.
  test("one rule the router refuses does not stop the others", async () => {
    const { docker, router, daemon, log } = await portical({ secureMode: true });
    docker.running = [
      container("direct", { [FORWARD_LABEL]: "80/tcp" }, [network("lan", "macvlan", "192.168.1.40")]),
      nginx(),
    ];

    await daemon.once();

    expect(router.mappings.map((m) => m.externalPort)).toEqual([9999]);
    expect(log.join("\n")).toContain("718");
  });

  // Removals are inferred from absence, so reconciling against a container
  // list we failed to fetch would read as "everything stopped".
  test("a Docker outage never tears down live mappings", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [nginx()];
    await daemon.once();

    docker.failWith = new Error("Cannot connect to the Docker daemon");
    expect(daemon.once()).rejects.toThrow(/Docker daemon/);

    expect(router.mappings).toHaveLength(1);
  });
});

describe("dry run", () => {
  let harness: Awaited<ReturnType<typeof portical>>;
  beforeEach(async () => { harness = await portical({}, { dryRun: true }); });

  test("changes nothing", async () => {
    harness.docker.running = [nginx()];
    await harness.daemon.once();
    expect(harness.router.mappings).toEqual([]);
  });

  test("still says what it would do", async () => {
    harness.docker.running = [nginx()];
    await harness.daemon.once();
    expect(harness.log.join("\n")).toContain("Adding 9999:80/tcp for nginx");
  });
});

/**
 * Every Portical writes the same "portical:" prefix, so the description alone
 * cannot say *which* Portical wrote a rule. Without this, a second instance -
 * on another host, or run by hand from a laptop - sees the first's rules,
 * finds no container wanting them, and deletes them. The two then delete each
 * other's rules forever.
 *
 * Found by pointing this at a live gateway: a dry run from a machine with no
 * labelled containers proposed removing all eight of another host's mappings.
 */
describe("another Portical on the network", () => {
  const elsewhere = () => ({
    externalPort: 25565,
    protocol: "tcp" as const,
    internalClient: "192.168.1.99",
    description: "portical: (25565:25565/tcp) minecraft",
  });

  test("has its mappings left alone", async () => {
    const { docker, router, daemon } = await portical();
    router.given(elsewhere());
    docker.running = [];

    await daemon.once();

    expect(router.mappings).toHaveLength(1);
  });

  test("is not fought over when we want the same port", async () => {
    const { docker, router, daemon, log } = await portical();
    router.given(elsewhere());
    docker.running = [container("mine", { [FORWARD_LABEL]: "25565:25565/tcp" })];

    await daemon.once();

    expect(router.mappings[0]?.internalClient).toBe("192.168.1.99");
    expect(log.join("\n")).toContain("already forwarded by another Portical to 192.168.1.99");
  });

  test("does not stop us cleaning up our own", async () => {
    const { docker, router, daemon } = await portical();
    router.given(elsewhere());
    docker.running = [nginx()];
    await daemon.once();

    docker.running = [];
    await daemon.once();

    expect(router.mappings.map((m) => m.internalClient)).toEqual(["192.168.1.99"]);
  });

  test("is managed anyway when explicitly asked", async () => {
    const { docker, router, daemon } = await portical({}, { manageAll: true });
    router.given(elsewhere());
    docker.running = [];

    await daemon.once();

    expect(router.mappings).toEqual([]);
  });

  // A macvlan container that stops takes its address with it, so the address
  // has to be remembered from when the mapping was written.
  test("does not stop us cleaning up a stopped container's own address", async () => {
    const { docker, router, daemon } = await portical();
    docker.running = [
      container("direct", { [FORWARD_LABEL]: "8080/tcp" }, [network("lan", "macvlan", "192.168.1.40")]),
    ];
    await daemon.once();
    expect(router.mappings).toHaveLength(1);

    docker.running = [];
    await daemon.once();

    expect(router.mappings).toEqual([]);
  });
});

describe("cleanup on exit", () => {
  test("removes our mappings and only ours", async () => {
    const { docker, router, daemon } = await portical();
    router.given({ externalPort: 32400, protocol: "tcp", description: "plex" });
    docker.running = [nginx()];
    await daemon.once();

    await daemon.cleanup();

    expect(router.mappings.map((m) => m.description)).toEqual(["plex"]);
  });
});

describe("run", () => {
  test("reconciles on a container event without waiting for the interval", async () => {
    const { docker, router, daemon } = await portical({}, { interval: 3600 });
    const controller = new AbortController();
    const running = daemon.run(controller.signal);

    docker.start(nginx());
    await Bun.sleep(20);

    expect(router.mappings).toHaveLength(1);

    docker.stop("nginx");
    await Bun.sleep(20);

    expect(router.mappings).toEqual([]);

    controller.abort();
    await running;
  });

  test("keeps going after a pass fails", async () => {
    const { docker, router, daemon, log } = await portical({}, { interval: 3600 });
    const controller = new AbortController();
    const running = daemon.run(controller.signal);

    docker.failWith = new Error("Cannot connect to the Docker daemon");
    docker.start(nginx());
    await Bun.sleep(20);
    expect(log.join("\n")).toContain("Cannot connect to the Docker daemon");

    docker.failWith = undefined;
    docker.start(container("plex", { [FORWARD_LABEL]: "32400/tcp" }));
    await Bun.sleep(20);

    // Both containers, including the one whose start event was lost to the
    // outage - the interval pass reconciles from scratch rather than replaying
    // events, so nothing has to be remembered across the failure.
    expect(router.mappings.map((m) => m.externalPort).sort((a, b) => a - b)).toEqual([9999, 32400]);

    controller.abort();
    await running;
  });
});

describe("summary", () => {
  test("says when there is nothing to do", async () => {
    const { docker, daemon, log } = await portical();
    docker.running = [nginx()];
    await daemon.once();

    log.length = 0;
    await daemon.once();

    expect(log).toEqual(["1 rule already correct, nothing to do"]);
  });

  // A daemon reconciling every 15 seconds should say it has settled once, and
  // then not repeat itself forever.
  test("stays quiet while nothing changes", async () => {
    const { docker, daemon, log } = await portical();
    docker.running = [nginx()];
    await daemon.once();
    await daemon.once();

    log.length = 0;
    for (let pass = 0; pass < 5; pass++) await daemon.once();

    expect(log).toEqual([]);
  });

  test("speaks up again when something changes", async () => {
    const { docker, daemon, log } = await portical();
    docker.running = [nginx()];
    await daemon.once();
    log.length = 0;

    docker.running = [];
    await daemon.once();

    expect(log).toContain("1 removed, 0 already correct");
  });

  test("counts a mixed pass", async () => {
    const { docker, router, daemon, log } = await portical();
    router.given({ externalPort: 7070, protocol: "tcp", description: "portical: (7070:7070/tcp) gone" });
    docker.running = [nginx(), container("plex", { [FORWARD_LABEL]: "32400/tcp" })];

    await daemon.once();

    expect(log).toContain("2 added, 1 removed, 0 already correct");
  });
});

/**
 * miniupnpd's secure_mode - the OpenWrt default - only lets a client map to
 * its own address. A macvlan container's mapping names the container's
 * address, not Portical's, so the gateway refuses with 718, which otherwise
 * reads as an ordinary port collision and sends people hunting for a conflict
 * that does not exist.
 */
describe("a gateway in secure mode", () => {
  test("explains why a macvlan container's rule was refused", async () => {
    const { docker, daemon, log } = await portical({ secureMode: true });
    docker.running = [
      container("direct", { [FORWARD_LABEL]: "8080/tcp" }, [network("lan", "macvlan", "192.168.1.40")]),
    ];

    await daemon.once();

    expect(log.join("\n")).toContain("718");
    expect(log.join("\n")).toContain("secure_mode");
  });

  // A bridge container forwards to the host, which *is* the requester, so it
  // works - and must not be given a misleading explanation.
  test("says nothing extra about a bridge container that succeeded", async () => {
    const { docker, daemon, log } = await portical({ secureMode: true });
    docker.running = [nginx()];

    await daemon.once();

    expect(log.join("\n")).not.toContain("secure_mode");
  });
});
