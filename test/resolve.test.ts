import { describe, expect, test } from "bun:test";
import type { Container, ContainerNetwork, PublishedPort } from "../src/docker.ts";
import { FORWARD_LABEL, NETWORK_LABEL, resolve } from "../src/resolve.ts";

function net(name: string, driver: string, ipAddress = ""): ContainerNetwork {
  return { name, driver, ipAddress };
}

function container(
  name: string,
  labels: Record<string, string>,
  networks: ContainerNetwork[] = [net("bridge", "bridge")],
  published: PublishedPort[] = [],
): Container {
  return { id: `id-${name}`, name, labels, networks, published };
}

describe("resolve", () => {
  test("ignores a container without the label", () => {
    expect(resolve([container("other", {})]).forwards).toEqual([]);
  });

  test("ignores a container whose label is blank", () => {
    expect(resolve([container("other", { [FORWARD_LABEL]: "  " })]).forwards).toEqual([]);
  });

  test("turns a label into a forward against the host", () => {
    const { forwards } = resolve([container("nginx", { [FORWARD_LABEL]: "9999:80/tcp" })], {
      hostAddress: "192.168.1.5",
    });
    expect(forwards).toEqual([
      {
        rule: { externalPort: 9999, internalPort: 80, protocol: "tcp" },
        container: "nginx",
        target: { kind: "host" },
        internalClient: "192.168.1.5",
      },
    ]);
  });

  test("reports a rule it could not parse without dropping the container", () => {
    const { forwards, warnings } = resolve([
      container("nginx", { [FORWARD_LABEL]: "nonsense,8080/tcp" }),
    ]);
    expect(forwards).toHaveLength(1);
    expect(warnings).toEqual(["nginx: ignoring unrecognised rule 'nonsense'"]);
  });

  describe("published", () => {
    test("forwards the host port to itself", () => {
      const { forwards } = resolve([
        container("minecraft", { [FORWARD_LABEL]: "published" }, [net("bridge", "bridge")], [
          { hostPort: 25565, containerPort: 25565, protocol: "tcp" },
        ]),
      ]);
      expect(forwards[0]?.rule).toEqual({
        externalPort: 25565,
        internalPort: 25565,
        protocol: "tcp",
      });
    });

    // The gateway forwards to the host; Docker's own published-port mapping
    // takes it on to the container. The container port is Docker's business.
    test("ignores the container port when it differs from the host port", () => {
      const { forwards } = resolve([
        container("nginx", { [FORWARD_LABEL]: "published" }, [net("bridge", "bridge")], [
          { hostPort: 8888, containerPort: 80, protocol: "tcp" },
        ]),
      ]);
      expect(forwards[0]?.rule).toEqual({ externalPort: 8888, internalPort: 8888, protocol: "tcp" });
    });

    test("yields nothing when the container publishes nothing", () => {
      expect(resolve([container("nginx", { [FORWARD_LABEL]: "published" })]).forwards).toEqual([]);
    });

    test("an explicit rule wins a collision with a published one", () => {
      const { forwards } = resolve([
        container("nginx", { [FORWARD_LABEL]: "published,8888:80/tcp" }, [net("bridge", "bridge")], [
          { hostPort: 8888, containerPort: 80, protocol: "tcp" },
        ]),
      ]);
      expect(forwards).toHaveLength(1);
      expect(forwards[0]?.rule.internalPort).toBe(80);
    });
  });

  describe("network drivers", () => {
    test("forwards straight to a macvlan container's own address", () => {
      const { forwards } = resolve(
        [container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [net("lan", "macvlan", "192.168.1.40")])],
        { hostAddress: "192.168.1.5" },
      );
      expect(forwards[0]).toMatchObject({
        target: { kind: "container", network: "lan" },
        internalClient: "192.168.1.40",
      });
    });

    test("forwards an ipvlan container the same way", () => {
      const { forwards } = resolve([
        container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [net("lan", "ipvlan", "192.168.1.41")]),
      ]);
      expect(forwards[0]?.target).toEqual({ kind: "container", network: "lan" });
    });

    test("forwards a host-networked container to the host", () => {
      const { forwards } = resolve(
        [container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [net("host", "host")])],
        { hostAddress: "192.168.1.5" },
      );
      expect(forwards[0]).toMatchObject({
        target: { kind: "host" },
        internalClient: "192.168.1.5",
      });
    });

    // The bridge address is not routable from the gateway, so it must never
    // become the target - the host is.
    test("never targets a bridge container's own address", () => {
      const { forwards } = resolve([
        container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [net("bridge", "bridge", "172.17.0.2")]),
      ]);
      expect(forwards[0]?.internalClient).toBeUndefined();
    });

    test("skips a container on an unsupported driver, saying which", () => {
      const { forwards, warnings } = resolve([
        container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [net("mesh", "overlay")]),
      ]);
      expect(forwards).toEqual([]);
      expect(warnings).toEqual([
        "nginx: has no supported network driver (found: overlay), skipping",
      ]);
    });

    test("skips a container attached to no network at all", () => {
      const { forwards, warnings } = resolve([
        container("nginx", { [FORWARD_LABEL]: "80/tcp" }, []),
      ]);
      expect(forwards).toEqual([]);
      expect(warnings).toEqual(["nginx: is not attached to any network, skipping"]);
    });
  });

  // Issue #1. v1's Go template concatenated every network name into one
  // nonsense string, the driver lookup failed, and every rule was skipped with
  // "Unsupported network driver: ". This is the reverse-proxy setup from that
  // issue: a container on many networks, one of which reaches the LAN.
  describe("a container on several networks", () => {
    const traefik = (labels: Record<string, string> = {}) =>
      container("traefik", { [FORWARD_LABEL]: "80/tcp,443/tcp", ...labels }, [
        net("networka", "bridge", "172.18.0.2"),
        net("networkb", "bridge", "172.19.0.2"),
        net("lan", "macvlan", "192.168.1.40"),
        net("networkc", "bridge", "172.20.0.2"),
      ]);

    test("resolves instead of failing", () => {
      const { forwards, warnings } = resolve([traefik()]);
      expect(forwards).toHaveLength(2);
      expect(warnings).toEqual([]);
    });

    test("prefers the network that reaches the LAN", () => {
      expect(resolve([traefik()]).forwards[0]).toMatchObject({
        target: { kind: "container", network: "lan" },
        internalClient: "192.168.1.40",
      });
    });

    test("falls back to the host when every network is a bridge", () => {
      const { forwards } = resolve(
        [
          container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [
            net("networka", "bridge", "172.18.0.2"),
            net("networkb", "bridge", "172.19.0.2"),
          ]),
        ],
        { hostAddress: "192.168.1.5" },
      );
      expect(forwards[0]).toMatchObject({ target: { kind: "host" }, internalClient: "192.168.1.5" });
    });

    test("honours an explicitly named network", () => {
      const { forwards } = resolve([traefik({ [NETWORK_LABEL]: "networkb" })]);
      expect(forwards[0]?.target).toEqual({ kind: "host" });
    });

    test("skips the container when the named network is not attached", () => {
      const { forwards, warnings } = resolve([traefik({ [NETWORK_LABEL]: "absent" })]);
      expect(forwards).toEqual([]);
      expect(warnings[0]).toBe(
        "traefik: is not attached to network 'absent' (has: networka, networkb, lan, networkc), skipping",
      );
    });

    test("warns and picks deterministically when two networks tie", () => {
      const both = container("nginx", { [FORWARD_LABEL]: "80/tcp" }, [
        net("zeta", "macvlan", "192.168.1.50"),
        net("alpha", "macvlan", "192.168.1.51"),
      ]);
      const { forwards, warnings } = resolve([both]);
      expect(forwards[0]?.internalClient).toBe("192.168.1.51");
      expect(warnings[0]).toBe(
        `nginx: is on 2 macvlan networks (alpha, zeta), forwarding to 'alpha' - set ${NETWORK_LABEL} to choose`,
      );
    });

    // An unstable choice would flip the target between passes and rewrite the
    // rule forever, which is the churn issue #6 is about arriving another way.
    test("makes the same choice however Docker orders the networks", () => {
      const forwards = (networks: ContainerNetwork[]) =>
        resolve([container("nginx", { [FORWARD_LABEL]: "80/tcp" }, networks)]).forwards[0];
      const a = net("alpha", "macvlan", "192.168.1.51");
      const z = net("zeta", "macvlan", "192.168.1.50");
      expect(forwards([a, z])).toEqual(forwards([z, a])!);
    });
  });

  test("resolves several containers in one pass", () => {
    const { forwards } = resolve([
      container("nginx", { [FORWARD_LABEL]: "80/tcp" }),
      container("minecraft", { [FORWARD_LABEL]: "25565" }),
    ]);
    expect(forwards.map((forward) => forward.container)).toEqual([
      "nginx",
      "minecraft",
      "minecraft",
    ]);
  });
});
