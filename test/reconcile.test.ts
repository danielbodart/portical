import { describe, expect, test } from "bun:test";
import { describe as description, type Mapping, type Rule } from "../src/model.ts";
import { reconcile, type Action, type DesiredForward } from "../src/reconcile.ts";

function rule(externalPort: number, internalPort = externalPort, protocol: "tcp" | "udp" = "tcp"): Rule {
  return { externalPort, internalPort, protocol };
}

function want(r: Rule, container = "nginx", internalClient?: string): DesiredForward {
  return { rule: r, container, target: { kind: "host" }, internalClient };
}

/** A mapping on the gateway that Portical put there. */
function ours(r: Rule, container = "nginx", overrides: Partial<Mapping> = {}): Mapping {
  return {
    externalPort: r.externalPort,
    protocol: r.protocol,
    internalClient: "192.168.1.5",
    internalPort: r.internalPort,
    description: description(r, container),
    remoteHost: "",
    leaseDuration: 0,
    ...overrides,
  };
}

const kinds = (actions: Action[]) => actions.map((action) => action.kind);

describe("reconcile", () => {
  test("adds a forward that is not on the gateway", () => {
    const actions = reconcile([want(rule(8080))], []);
    expect(kinds(actions)).toEqual(["add"]);
  });

  test("removes a mapping of ours that nothing asks for", () => {
    const actions = reconcile([], [ours(rule(8080))]);
    expect(kinds(actions)).toEqual(["remove"]);
  });

  test("leaves a mapping alone that is not ours", () => {
    const plex = { ...ours(rule(32400)), description: "plex" };
    expect(reconcile([], [plex])).toEqual([]);
  });

  // Issue #6. v1 compared the free-text description, so a router that
  // truncated or rewrote it made a perfectly good rule look missing and
  // deleted it - every interval, dropping live connections each time.
  describe("does not churn a rule that is already correct", () => {
    test("when the description matches exactly", () => {
      expect(kinds(reconcile([want(rule(8080))], [ours(rule(8080))]))).toEqual(["keep"]);
    });

    test("when the router truncated the description", () => {
      const truncated = ours(rule(6881, 6881, "udp"), "qbittorrent", {
        description: "portical: (6881:6881/ud",
      });
      expect(kinds(reconcile([want(rule(6881, 6881, "udp"))], [truncated]))).toEqual(["keep"]);
    });

    test("when the router rewrote the description in its own words", () => {
      const rewritten = ours(rule(8080), "nginx", { description: "portical: rule 1" });
      expect(kinds(reconcile([want(rule(8080))], [rewritten]))).toEqual(["keep"]);
    });

    test("when the container it came from was renamed", () => {
      const renamed = ours(rule(8080), "nginx-old");
      expect(kinds(reconcile([want(rule(8080), "nginx-new")], [renamed]))).toEqual(["keep"]);
    });
  });

  describe("replaces a mapping that has drifted", () => {
    test("when it points at the wrong internal port", () => {
      const drifted = ours(rule(8080), "nginx", { internalPort: 9090 });
      const actions = reconcile([want(rule(8080, 80))], [drifted]);
      expect(kinds(actions)).toEqual(["replace"]);
      expect(actions[0]).toMatchObject({ reason: "points at port 9090, expected 80" });
    });

    test("when it points at the wrong address", () => {
      const drifted = ours(rule(8080), "nginx", { internalClient: "192.168.1.99" });
      const actions = reconcile([want(rule(8080), "nginx", "192.168.1.5")], [drifted]);
      expect(kinds(actions)).toEqual(["replace"]);
    });

    // An address we could not determine must not read as a mismatch, or every
    // pass replaces a working rule.
    test("but not when the wanted address is unknown", () => {
      const mapping = ours(rule(8080), "nginx", { internalClient: "192.168.1.99" });
      expect(kinds(reconcile([want(rule(8080))], [mapping]))).toEqual(["keep"]);
    });
  });

  describe("leases", () => {
    test("renews one that is about to expire", () => {
      const expiring = ours(rule(8080), "nginx", { leaseDuration: 300 });
      const actions = reconcile([want(rule(8080))], [expiring], { renewWithin: 3600 });
      expect(kinds(actions)).toEqual(["replace"]);
      expect(actions[0]).toMatchObject({ reason: "lease expires in 300s" });
    });

    test("leaves one with plenty of time left", () => {
      const fresh = ours(rule(8080), "nginx", { leaseDuration: 86400 });
      expect(kinds(reconcile([want(rule(8080))], [fresh], { renewWithin: 3600 }))).toEqual(["keep"]);
    });

    // 0 means "never expires", not "expired an infinitely long time ago".
    test("never treats an unlimited lease as expiring", () => {
      const forever = ours(rule(8080), "nginx", { leaseDuration: 0 });
      expect(kinds(reconcile([want(rule(8080))], [forever], { renewWithin: 86400 }))).toEqual(["keep"]);
    });
  });

  describe("conflicts with another tool", () => {
    const plex = { ...ours(rule(32400)), description: "plex" };

    test("are reported rather than stolen", () => {
      const actions = reconcile([want(rule(32400), "jellyfin")], [plex]);
      expect(kinds(actions)).toEqual(["conflict"]);
      expect(actions[0]).toMatchObject({
        reason: "already forwarded by something else (plex)",
      });
    });

    // Rewritten even though it already points the right way: ownership lives
    // in the description, so a mapping still carrying "plex" is one we would
    // refuse to touch next pass and never clean up.
    test("are taken over when asked, claiming the description", () => {
      const actions = reconcile([want(rule(32400))], [plex], { steal: true });
      expect(kinds(actions)).toEqual(["replace"]);
      expect(actions[0]).toMatchObject({ reason: "taking over from plex" });
    });

    test("describe an unlabelled owner usefully", () => {
      const anonymous = { ...plex, description: "  " };
      expect(reconcile([want(rule(32400))], [anonymous])[0]).toMatchObject({
        reason: "already forwarded by something else (192.168.1.5:32400, no description)",
      });
    });
  });

  test("force rewrites a rule that is already correct", () => {
    const actions = reconcile([want(rule(8080))], [ours(rule(8080))], { force: true });
    expect(kinds(actions)).toEqual(["replace"]);
    expect(actions[0]).toMatchObject({ reason: "forced" });
  });

  test("treats the two protocols on one port as separate mappings", () => {
    const actions = reconcile(
      [want(rule(25565, 25565, "tcp")), want(rule(25565, 25565, "udp"))],
      [ours(rule(25565, 25565, "tcp"))],
    );
    expect(kinds(actions)).toEqual(["keep", "add"]);
  });

  test("handles a mixed pass without disturbing the settled rules", () => {
    const actions = reconcile(
      [want(rule(8080)), want(rule(9090))],
      [ours(rule(8080)), ours(rule(7070)), { ...ours(rule(32400)), description: "plex" }],
    );
    expect(kinds(actions)).toEqual(["keep", "add", "remove"]);
    expect(actions.find((a) => a.kind === "remove")).toMatchObject({
      mapping: { externalPort: 7070 },
    });
  });
});
