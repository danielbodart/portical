import { describe, expect, test } from "bun:test";
import { toContainer } from "../src/docker.ts";

const drivers = new Map([
  ["bridge", "bridge"],
  ["lan", "macvlan"],
  ["proxy", "bridge"],
  ["internal", "bridge"],
]);

describe("toContainer", () => {
  test("strips Docker's leading slash from the name", () => {
    expect(toContainer({ Id: "abc", Names: ["/nginx"] }, drivers).name).toBe("nginx");
  });

  test("falls back to the id when a container has no name", () => {
    expect(toContainer({ Id: "abc" }, drivers).name).toBe("abc");
  });

  test("joins each network to its driver", () => {
    const container = toContainer(
      {
        Id: "abc",
        NetworkSettings: { Networks: { lan: { IPAddress: "192.168.1.40" }, proxy: { IPAddress: "172.18.0.2" } } },
      },
      drivers,
    );
    expect(container.networks).toEqual([
      { name: "lan", driver: "macvlan", ipAddress: "192.168.1.40" },
      { name: "proxy", driver: "bridge", ipAddress: "172.18.0.2" },
    ]);
  });

  // A network attached by id, or removed between listing containers and
  // listing networks, has no driver. Calling that "bridge" would send traffic
  // somewhere nobody asked for, so it stays unknown and gets skipped.
  test("marks a network with no known driver as unknown", () => {
    const container = toContainer(
      { Id: "abc", NetworkSettings: { Networks: { vanished: {} } } },
      drivers,
    );
    expect(container.networks[0]).toEqual({ name: "vanished", driver: "unknown", ipAddress: "" });
  });

  describe("published ports", () => {
    test("reads host and container port with protocol", () => {
      const container = toContainer(
        { Id: "abc", Ports: [{ PrivatePort: 80, PublicPort: 8888, Type: "tcp" }] },
        drivers,
      );
      expect(container.published).toEqual([{ hostPort: 8888, containerPort: 80, protocol: "tcp" }]);
    });

    // Docker reports one `-p 8888:80` twice, once for 0.0.0.0 and once for ::.
    // Left alone that is a duplicate rule for every published port.
    test("collapses the IPv4 and IPv6 entries for one binding", () => {
      const container = toContainer(
        {
          Id: "abc",
          Ports: [
            { PrivatePort: 80, PublicPort: 8888, Type: "tcp" },
            { PrivatePort: 80, PublicPort: 8888, Type: "tcp" },
          ],
        },
        drivers,
      );
      expect(container.published).toHaveLength(1);
    });

    test("ignores a port that is exposed but not published", () => {
      const container = toContainer(
        { Id: "abc", Ports: [{ PrivatePort: 80, Type: "tcp" }] },
        drivers,
      );
      expect(container.published).toEqual([]);
    });

    test("keeps tcp and udp on the same host port apart", () => {
      const container = toContainer(
        {
          Id: "abc",
          Ports: [
            { PrivatePort: 53, PublicPort: 53, Type: "tcp" },
            { PrivatePort: 53, PublicPort: 53, Type: "udp" },
          ],
        },
        drivers,
      );
      expect(container.published).toHaveLength(2);
    });

    test("ignores a protocol that is neither tcp nor udp", () => {
      const container = toContainer(
        { Id: "abc", Ports: [{ PrivatePort: 132, PublicPort: 132, Type: "sctp" }] },
        drivers,
      );
      expect(container.published).toEqual([]);
    });
  });

  test("defaults labels and networks when Docker omits them", () => {
    const container = toContainer({ Id: "abc" }, drivers);
    expect(container.labels).toEqual({});
    expect(container.networks).toEqual([]);
    expect(container.published).toEqual([]);
  });
});
