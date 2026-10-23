import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DEFAULTS, Portical } from "../src/daemon.ts";
import { DbusError, MessageType, Variant } from "../src/dbus.ts";
import {
  assignments,
  DbusSystemdClient,
  declaration,
  IN_FLIGHT,
  listening,
  machine,
  parseListeners,
  SystemdSource,
  toEvent,
  unitName,
  unitPath,
} from "../src/systemd.ts";
import { UpnpGateway } from "../src/upnp.ts";
import { FakeBus } from "./fakes/bus.ts";
import { FakeGateway } from "./fakes/gateway.ts";
import { FakeHost, FakeSystemd, unit } from "./fakes/systemd.ts";

const HOST = "192.168.1.5";

/** `ss -Hlntu -e` on the development host, user sessions left out. */
const SS = await Bun.file(new URL("./fixtures/ss-host.txt", import.meta.url)).text();

describe("assignments", () => {
  test("finds the key in each section, with its line", () => {
    const text = [
      "[Unit]",
      "Description=Minecraft",
      "X-Portical-Forward=25565",
      "",
      "[Service]",
      "ExecStart=/bin/minecraft",
      "X-Portical-Forward = 19132/udp ",
    ].join("\n");
    expect(assignments(text, "X-Portical-Forward")).toEqual([
      { section: "Unit", line: 3, value: "25565" },
      { section: "Service", line: 7, value: "19132/udp" },
    ]);
  });

  test("skips both kinds of comment", () => {
    const text = "[Unit]\n# X-Portical-Forward=1\n; X-Portical-Forward=2\n  #X-Portical-Forward=3\n";
    expect(assignments(text, "X-Portical-Forward")).toEqual([]);
  });

  test("joins continued lines with a space, skipping comments inside them", () => {
    const text = "[Unit]\nX-Portical-Forward=25565,\\\n# a comment\n  19132/udp\nAfter=x\n";
    expect(assignments(text, "X-Portical-Forward")).toEqual([
      { section: "Unit", line: 2, value: "25565, 19132/udp" },
    ]);
  });

  test("is case sensitive about the key, as systemd is", () => {
    expect(assignments("[Unit]\nx-portical-forward=1\n", "X-Portical-Forward")).toEqual([]);
  });

  test("keeps an empty assignment, which means reset", () => {
    expect(assignments("[Unit]\nX-Portical-Forward=\n", "X-Portical-Forward")).toEqual([
      { section: "Unit", line: 2, value: "" },
    ]);
  });
});

describe("declaration", () => {
  const minecraft = { name: "minecraft.service" };
  const file = (path: string, text: string) => ({ path, text });

  test("reads the unit file", () => {
    expect(declaration(minecraft, [file("/u", "[Unit]\nX-Portical-Forward=25565\n")]).value).toBe("25565");
  });

  test("accepts the unit type's own section", () => {
    expect(declaration(minecraft, [file("/u", "[Service]\nX-Portical-Forward=25565\n")]).value).toBe("25565");
    expect(declaration({ name: "dns.socket" }, [file("/u", "[Socket]\nX-Portical-Forward=53\n")]).value).toBe("53");
  });

  test("ignores another section, and says where", () => {
    const declared = declaration(minecraft, [file("/etc/x.service", "[Install]\nX-Portical-Forward=25565\n")]);
    expect(declared.value).toBe("");
    expect(declared.warnings).toEqual([
      "ignoring X-Portical-Forward in [Install] at /etc/x.service:2 - put it in [Unit] or [Service]",
    ]);
  });

  test("adds every assignment, in the unit file and its drop-ins", () => {
    const declared = declaration(minecraft, [
      file("/u", "[Unit]\nX-Portical-Forward=25565\nX-Portical-Forward=25575/tcp\n"),
      file("/u.d/a.conf", "[Unit]\nX-Portical-Forward=19132/udp\n"),
    ]);
    expect(declared.value).toBe("25565,25575/tcp,19132/udp");
  });

  test("lets a drop-in turn a unit's forwards off", () => {
    const declared = declaration(minecraft, [
      file("/u", "[Unit]\nX-Portical-Forward=25565\n"),
      file("/u.d/off.conf", "[Unit]\nX-Portical-Forward=\n"),
    ]);
    expect(declared.value).toBe("");
  });

  test("lets a drop-in replace a unit's forwards", () => {
    const declared = declaration(minecraft, [
      file("/u", "[Unit]\nX-Portical-Forward=25565\n"),
      file("/u.d/mine.conf", "[Unit]\nX-Portical-Forward=\nX-Portical-Forward=25566:25565/tcp\n"),
    ]);
    expect(declared.value).toBe("25566:25565/tcp");
  });

  test("applies the files in the order given, so a later reset only clears what came before", () => {
    const declared = declaration(minecraft, [
      file("/u.d/a.conf", "[Unit]\nX-Portical-Forward=1\n"),
      file("/u.d/b.conf", "[Unit]\nX-Portical-Forward=\nX-Portical-Forward=2\n"),
      file("/u.d/c.conf", "[Unit]\nX-Portical-Forward=3\n"),
    ]);
    expect(declared.value).toBe("2,3");
  });

  test("reads a renamed key", () => {
    expect(declaration(minecraft, [file("/u", "[Unit]\nX-Forward=80\n")], "X-Forward").value).toBe("80");
  });
});

describe("parseListeners", () => {
  test("reads the development host's sockets", () => {
    const listeners = parseListeners(SS);
    expect(listeners).toContainEqual({
      protocol: "tcp",
      address: "0.0.0.0",
      port: 22,
      cgroup: "/system.slice/sshd.service",
      v6only: false,
    });
    expect(listeners).toContainEqual({
      protocol: "tcp",
      address: "::",
      port: 22,
      cgroup: "/system.slice/sshd.service",
      v6only: true,
    });
    expect(listeners).toContainEqual({
      protocol: "udp",
      address: "127.0.0.53%lo",
      port: 53,
      cgroup: "/system.slice/systemd-resolved.service",
      v6only: false,
    });
    expect(listeners).toContainEqual({
      protocol: "udp",
      address: "fe80::c8bb:f868:b170:f12e%enp4s0",
      port: 546,
      cgroup: "/system.slice/NetworkManager.service",
      v6only: true,
    });
  });

  test("leaves the cgroup out where ss does not print one", () => {
    expect(parseListeners("tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* ino:1 sk:2 <->")).toEqual([
      { protocol: "tcp", address: "0.0.0.0", port: 22, cgroup: undefined, v6only: false },
    ]);
  });
});

describe("listening", () => {
  const sockets = (text: string, ephemeral: [number, number] = [32768, 60999]) => ({
    listeners: parseListeners(text),
    ephemeral,
  });

  test("finds sshd's port on the development host, once for both address families", () => {
    expect(listening(sockets(SS), ["/system.slice/sshd.service"], "10.0.0.219")).toEqual([
      { externalPort: 22, internalPort: 22, protocol: "tcp" },
    ]);
  });

  test("leaves out loopback and interface-scoped binds", () => {
    // resolved listens on 5355 everywhere, and on 53 only on loopback.
    expect(listening(sockets(SS), ["/system.slice/systemd-resolved.service"], "10.0.0.219")).toEqual([
      { externalPort: 5355, internalPort: 5355, protocol: "udp" },
      { externalPort: 5355, internalPort: 5355, protocol: "tcp" },
    ]);
  });

  test("takes a socket bound to the host's own address", () => {
    const text = "tcp LISTEN 0 128 192.168.1.5:8080 0.0.0.0:* cgroup:/system.slice/web.service <->\n" +
      "tcp LISTEN 0 128 192.168.7.1:8081 0.0.0.0:* cgroup:/system.slice/web.service <->\n";
    expect(listening(sockets(text), ["/system.slice/web.service"], HOST)).toEqual([
      { externalPort: 8080, internalPort: 8080, protocol: "tcp" },
    ]);
  });

  test("takes a dual-stack IPv6 wildcard, but not a v6only one", () => {
    const text = "tcp LISTEN 0 128 *:25565 *:* cgroup:/system.slice/mc.service <->\n" +
      "tcp LISTEN 0 128 [::]:25575 [::]:* cgroup:/system.slice/mc.service <->\n" +
      "tcp LISTEN 0 128 [::]:25576 [::]:* cgroup:/system.slice/mc.service v6only:1 <->\n";
    expect(listening(sockets(text), ["/system.slice/mc.service"], HOST).map((r) => r.externalPort)).toEqual([
      25565, 25575,
    ]);
  });

  test("includes sockets in the unit's sub-cgroups, but not a sibling's with a longer name", () => {
    const text = "tcp LISTEN 0 128 0.0.0.0:1 0.0.0.0:* cgroup:/system.slice/mc.service/worker <->\n" +
      "tcp LISTEN 0 128 0.0.0.0:2 0.0.0.0:* cgroup:/system.slice/mc.service2 <->\n";
    expect(listening(sockets(text), ["/system.slice/mc.service"], HOST).map((r) => r.externalPort)).toEqual([1]);
  });

  test("leaves out UDP on ephemeral ports, which are client sockets", () => {
    const text = "udp UNCONN 0 0 0.0.0.0:19132 0.0.0.0:* cgroup:/system.slice/mc.service <->\n" +
      "udp UNCONN 0 0 0.0.0.0:41234 0.0.0.0:* cgroup:/system.slice/mc.service <->\n" +
      "tcp LISTEN 0 128 0.0.0.0:41235 0.0.0.0:* cgroup:/system.slice/mc.service <->\n";
    expect(listening(sockets(text), ["/system.slice/mc.service"], HOST)).toEqual([
      { externalPort: 19132, internalPort: 19132, protocol: "udp" },
      { externalPort: 41235, internalPort: 41235, protocol: "tcp" },
    ]);
  });
});

describe("unit object paths", () => {
  test("are escaped as systemd escapes them", () => {
    expect(unitPath("sshd.service")).toBe("/org/freedesktop/systemd1/unit/sshd_2eservice");
    expect(unitPath("frisket-dns.socket")).toBe("/org/freedesktop/systemd1/unit/frisket_2ddns_2esocket");
    expect(unitPath("1password.service")).toBe("/org/freedesktop/systemd1/unit/_31password_2eservice");
  });

  test("read back to the unit's name", () => {
    for (const name of ["sshd.service", "frisket-dns.socket", "1password.service", "game@25565.service"]) {
      expect(unitName(unitPath(name).split("/").pop()!)).toBe(name);
    }
  });
});

describe("toEvent", () => {
  const changed = (path: string, iface: string, properties: Record<string, unknown>, invalidated: string[] = []) => ({
    type: MessageType.Signal,
    serial: 1,
    path,
    interface: "org.freedesktop.DBus.Properties",
    member: "PropertiesChanged",
    body: [iface, properties, invalidated],
  });

  test("a unit's ActiveState changing", () => {
    expect(toEvent(changed("/org/freedesktop/systemd1/unit/sshd_2eservice", "org.freedesktop.systemd1.Unit", {
      ActiveState: "active",
      SubState: "running",
    }))).toEqual({ unit: "sshd.service", state: "active" });
  });

  test("ignores a change that leaves ActiveState alone", () => {
    expect(toEvent(changed("/org/freedesktop/systemd1/unit/sshd_2eservice", "org.freedesktop.systemd1.Unit", {
      SubState: "running",
    }))).toBeUndefined();
    expect(toEvent(changed("/org/freedesktop/systemd1/unit/sshd_2eservice", "org.freedesktop.systemd1.Service", {
      MainPID: 1,
    }))).toBeUndefined();
  });

  test("a reload finishing, but not starting", () => {
    const reloading = (done: boolean) => ({
      type: MessageType.Signal,
      serial: 1,
      path: "/org/freedesktop/systemd1",
      interface: "org.freedesktop.systemd1.Manager",
      member: "Reloading",
      body: [!done],
    });
    expect(toEvent(reloading(true))).toEqual({});
    expect(toEvent(reloading(false))).toBeUndefined();
  });
});

describe("SystemdSource", () => {
  function fixture() {
    const systemd = new FakeSystemd();
    const host = new FakeHost();
    return { systemd, host, source: new SystemdSource(systemd, { hostAddress: HOST }, host) };
  }

  test("forwards what an active unit declares, to this host", async () => {
    const { systemd, host, source } = fixture();
    host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565/tcp\n";
    systemd.running = [unit("mc.service")];

    expect(await source.desired()).toEqual({
      forwards: [
        {
          rule: { externalPort: 25565, internalPort: 25565, protocol: "tcp" },
          container: "mc.service",
          target: { kind: "host" },
          internalClient: HOST,
        },
      ],
      warnings: [],
    });
  });

  test("wants nothing from a unit that declares nothing, or has no file", async () => {
    const { systemd, host, source } = fixture();
    host.files["/etc/systemd/system/sshd.service"] = "[Service]\nExecStart=/bin/sshd\n";
    systemd.running = [unit("sshd.service"), unit("dev-sda.device", "")];
    expect(await source.desired()).toEqual({ forwards: [], warnings: [] });
  });

  test("reads drop-ins after the unit file", async () => {
    const { systemd, host, source } = fixture();
    host.files["/nix/store/x-mc.service"] = "[Unit]\nX-Portical-Forward=25565/tcp\n";
    host.files["/etc/systemd/system/mc.service.d/off.conf"] = "[Unit]\nX-Portical-Forward=\n";
    systemd.running = [
      unit("mc.service", "/nix/store/x-mc.service", { dropInPaths: ["/etc/systemd/system/mc.service.d/off.conf"] }),
    ];
    expect((await source.desired()).forwards).toEqual([]);
  });

  test("reports a bad term against the unit", async () => {
    const { systemd, host, source } = fixture();
    host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565/tcp,%i/udp\n";
    systemd.running = [unit("mc.service")];
    const desired = await source.desired();
    expect(desired.forwards).toHaveLength(1);
    expect(desired.warnings).toEqual(["mc.service: ignoring unrecognised rule '%i/udp'"]);
  });

  test("warns when the files have changed since systemd loaded them", async () => {
    const { systemd, host, source } = fixture();
    host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565/tcp\n";
    systemd.running = [unit("mc.service", undefined, { needDaemonReload: true })];
    expect((await source.desired()).warnings).toEqual([
      "mc.service: its unit files have changed since systemd loaded them - forwarding what is " +
        "on disk now. Run 'systemctl daemon-reload'.",
    ]);
  });

  test("fails the whole pass when a file cannot be read", async () => {
    const { systemd, host, source } = fixture();
    host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565/tcp\n";
    systemd.running = [unit("mc.service"), unit("gone.service")];
    await expect(source.desired()).rejects.toThrow(/gone\.service/);
  });

  test("fails the whole pass when systemd cannot be asked", async () => {
    const { systemd, source } = fixture();
    systemd.failWith = new Error("D-Bus connection closed");
    await expect(source.desired()).rejects.toThrow("D-Bus connection closed");
  });

  describe("published", () => {
    test("forwards what the unit listens on, with explicit terms winning", async () => {
      const { systemd, host, source } = fixture();
      host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=published,25565:25566/tcp\n";
      host.ss = "tcp LISTEN 0 128 0.0.0.0:25565 0.0.0.0:* cgroup:/system.slice/mc.service <->\n" +
        "udp UNCONN 0 0 0.0.0.0:19132 0.0.0.0:* cgroup:/system.slice/mc.service <->\n" +
        "tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* cgroup:/system.slice/sshd.service <->\n";
      systemd.running = [unit("mc.service")];
      systemd.groups = { "mc.service": "/system.slice/mc.service" };

      expect((await source.desired()).forwards.map((f) => f.rule)).toEqual([
        { externalPort: 25565, internalPort: 25566, protocol: "tcp" },
        { externalPort: 19132, internalPort: 19132, protocol: "udp" },
      ]);
    });

    test("includes the sockets of the socket unit that activates it", async () => {
      const { systemd, host, source } = fixture();
      host.files["/etc/systemd/system/dns.service"] = "[Unit]\nX-Portical-Forward=published\n";
      host.ss = "udp UNCONN 0 0 0.0.0.0:53 0.0.0.0:* cgroup:/system.slice/dns.socket <->\n";
      systemd.running = [unit("dns.service", undefined, { triggeredBy: ["dns.socket"] })];
      systemd.groups = { "dns.service": "/system.slice/dns.service", "dns.socket": "/system.slice/dns.socket" };

      expect((await source.desired()).forwards.map((f) => f.rule)).toEqual([
        { externalPort: 53, internalPort: 53, protocol: "udp" },
      ]);
    });

    test("does not run ss unless some unit asks for published", async () => {
      const { systemd, host, source } = fixture();
      host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565\n";
      systemd.running = [unit("mc.service")];
      await source.desired();
      expect(host.ssRuns).toBe(0);
    });

    test("warns when ss cannot say which cgroup a socket belongs to", async () => {
      const { systemd, host, source } = fixture();
      host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=published\n";
      host.ss = "tcp LISTEN 0 128 0.0.0.0:25565 0.0.0.0:* ino:1 sk:2 <->\n";
      systemd.running = [unit("mc.service")];
      systemd.groups = { "mc.service": "/system.slice/mc.service" };

      const desired = await source.desired();
      expect(desired.forwards).toEqual([]);
      expect(desired.warnings).toEqual([expect.stringContaining("ss reports no cgroup")]);
    });
  });

  test("logs where a unit that declares forwards settles, and nothing else", async () => {
    const { systemd, host, source } = fixture();
    host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565\n";
    systemd.running = [unit("mc.service")];
    await source.desired();

    const controller = new AbortController();
    const seen: unknown[] = [];
    const reading = (async () => {
      for await (const event of source.events(controller.signal)) seen.push(event);
    })();
    systemd.emit({ unit: "mc.service", state: "deactivating" });
    systemd.emit({ unit: "mc.service", state: "inactive", previous: "deactivating" });
    systemd.emit({ unit: "cron.service", state: "active", previous: "activating" });
    systemd.emit({});
    await Bun.sleep(10);
    controller.abort();
    await reading;

    expect(seen).toEqual([{}, { description: "Unit mc.service inactive" }, {}, {}]);
  });
});

/**
 * The daemon, driven by systemd rather than Docker: the same reconcile, the
 * same UPnP client, with a unit's lifecycle in place of a container's.
 */
describe("Portical with the systemd source", () => {
  async function portical() {
    const systemd = new FakeSystemd();
    const host = new FakeHost();
    const router = new FakeGateway();
    const gateway = await UpnpGateway.at(router.handler, router.rootUrl);
    const log: string[] = [];
    const options = { ...DEFAULTS, hostAddress: HOST, reconnectBackoff: 5 };
    const daemon = new Portical(
      new SystemdSource(systemd, { hostAddress: HOST }, host),
      gateway,
      options,
      (message) => log.push(message),
    );
    host.files["/etc/systemd/system/mc.service"] = "[Unit]\nX-Portical-Forward=25565/tcp\n";
    return { systemd, host, router, daemon, log };
  }

  test("forwards a unit's port to this host, named after the unit", async () => {
    const { systemd, router, daemon } = await portical();
    systemd.running = [unit("mc.service")];
    await daemon.once();
    expect(router.mappings).toEqual([
      {
        externalPort: 25565,
        protocol: "tcp",
        internalPort: 25565,
        internalClient: HOST,
        description: "portical: (25565:25565/tcp) mc.service",
        remoteHost: "",
        leaseDuration: 0,
      },
    ]);
  });

  test("removes it when the unit stops", async () => {
    const { systemd, router, daemon } = await portical();
    systemd.running = [unit("mc.service")];
    await daemon.once();
    systemd.running = [];
    await daemon.once();
    expect(router.mappings).toEqual([]);
  });

  test("removes it when a drop-in resets the key", async () => {
    const { systemd, host, router, daemon } = await portical();
    systemd.running = [unit("mc.service")];
    await daemon.once();
    host.files["/etc/systemd/system/mc.service.d/off.conf"] = "[Unit]\nX-Portical-Forward=\n";
    systemd.running = [unit("mc.service", undefined, { dropInPaths: ["/etc/systemd/system/mc.service.d/off.conf"] })];
    await daemon.once();
    expect(router.mappings).toEqual([]);
  });

  test("removes nothing when systemd cannot be asked", async () => {
    const { systemd, router, daemon } = await portical();
    systemd.running = [unit("mc.service")];
    await daemon.once();
    systemd.failWith = new Error("D-Bus connection closed");
    await expect(daemon.once()).rejects.toThrow();
    expect(router.mappings).toHaveLength(1);
  });

  test("reacts to a unit starting without waiting for the interval, and survives the bus dropping", async () => {
    const { systemd, router, daemon, log } = await portical();
    const controller = new AbortController();
    const running = daemon.run(controller.signal);
    await Bun.sleep(10);
    expect(router.mappings).toEqual([]);

    systemd.dropStream();
    await Bun.sleep(30);
    systemd.start(unit("mc.service"));
    await Bun.sleep(30);

    expect(router.mappings.map((m) => m.externalPort)).toEqual([25565]);
    // Not declared on the pass before it started, so not worth a line yet.
    expect(log).not.toContain("Unit mc.service active");

    systemd.stop("mc.service");
    await Bun.sleep(30);
    expect(router.mappings).toEqual([]);
    expect(log).toContain("Unit mc.service inactive");

    controller.abort();
    await running;
  });
});

describe("machine", () => {
  let directory: string;
  afterEach(() => rmSync(directory, { recursive: true, force: true }));

  test("reads a unit file, and sees it change", async () => {
    directory = mkdtempSync(join(tmpdir(), "portical-units-"));
    const path = join(directory, "mc.service");
    writeFileSync(path, "[Unit]\nX-Portical-Forward=25565\n");
    const host = machine();
    expect(await host.read(path)).toBe("[Unit]\nX-Portical-Forward=25565\n");
    writeFileSync(path, "[Unit]\nX-Portical-Forward=25566/tcp\n");
    expect(await host.read(path)).toBe("[Unit]\nX-Portical-Forward=25566/tcp\n");
  });

  test("reads a file deleted since systemd loaded it as empty, rather than failing every pass", async () => {
    directory = mkdtempSync(join(tmpdir(), "portical-units-"));
    const path = join(directory, "mc.service");
    writeFileSync(path, "[Unit]\nX-Portical-Forward=25565\n");
    const host = machine();
    await host.read(path);
    rmSync(path);
    expect(await host.read(path)).toBe("");
    expect(await host.read(join(directory, "gone.service.d", "forward.conf"))).toBe("");
    expect(await host.read(join(path, "under-a-file.conf"))).toBe("");
  });

  test("still fails on a file it may not read", async () => {
    directory = mkdtempSync(join(tmpdir(), "portical-units-"));
    // A directory where a file should be: readable by stat, not by read.
    await expect(machine().read(directory)).rejects.toThrow();
  });

  test("lets the source forward nothing for a unit whose file is gone", async () => {
    directory = mkdtempSync(join(tmpdir(), "portical-units-"));
    const systemd = new FakeSystemd();
    systemd.running = [unit("gone.service", join(directory, "gone.service"), { needDaemonReload: true })];
    const desired = await new SystemdSource(systemd, {}, machine()).desired();
    expect(desired.forwards).toEqual([]);
  });
});

describe("DbusSystemdClient", () => {
  let bus: FakeBus;
  afterEach(async () => bus?.stop());

  const SSHD = "/org/freedesktop/systemd1/unit/sshd_2eservice";

  function systemd(call: { member?: string; path?: string; body?: readonly unknown[] }) {
    switch (call.member) {
      case "ListUnitsByPatterns":
        return {
          signature: "a(ssssssouso)",
          body: [[["sshd.service", "SSH Daemon", "loaded", "active", "running", "", SSHD, 0, "", "/"]]],
        };
      case "GetAll":
        return {
          signature: "a{sv}",
          body: [{
            Id: new Variant("s", "sshd.service"),
            FragmentPath: new Variant("s", "/etc/systemd/system/sshd.service"),
            DropInPaths: new Variant("as", ["/etc/systemd/system/sshd.service.d/forward.conf"]),
            TriggeredBy: new Variant("as", []),
            NeedDaemonReload: new Variant("b", false),
            ActiveEnterTimestamp: new Variant("t", 1n),
          }],
        };
      case "Get":
        return call.body?.[0] === "org.freedesktop.systemd1.Service"
          ? { signature: "v", body: [new Variant("s", "/system.slice/sshd.service")] }
          : new DbusError("org.freedesktop.DBus.Error.UnknownProperty", "no");
      default:
        return {};
    }
  }

  test("lists active units with their files", async () => {
    bus = await FakeBus.start();
    bus.answer = systemd;
    const client = new DbusSystemdClient(bus.path);

    expect(await client.units()).toEqual([
      {
        name: "sshd.service",
        fragmentPath: "/etc/systemd/system/sshd.service",
        dropInPaths: ["/etc/systemd/system/sshd.service.d/forward.conf"],
        triggeredBy: [],
        needDaemonReload: false,
      },
    ]);
    expect(bus.calls.find((c) => c.member === "ListUnitsByPatterns")?.body).toEqual([["active", "reloading", "refreshing"], []]);
    expect(bus.calls.find((c) => c.member === "GetAll")).toMatchObject({
      path: SSHD,
      body: ["org.freedesktop.systemd1.Unit"],
    });
  });

  test("keeps the calls it has waiting under dbus-daemon's limit, however many units there are", async () => {
    bus = await FakeBus.start();
    bus.maxPending = 128;
    const units = Array.from({ length: 300 }, (_, index) => `unit${index}.service`);
    bus.answer = async (call) => {
      if (call.member === "ListUnitsByPatterns") {
        return {
          signature: "a(ssssssouso)",
          body: [units.map((name) => [name, "", "loaded", "active", "running", "", unitPath(name), 0, "", "/"])],
        };
      }
      if (call.member === "GetAll") {
        await Bun.sleep(1);
        return { signature: "a{sv}", body: [{ FragmentPath: new Variant("s", `/etc/systemd/system/${call.path}`) }] };
      }
      return {};
    };
    const client = new DbusSystemdClient(bus.path);

    const listed = await client.units();
    expect(listed.map((unit) => unit.name)).toEqual(units);
    expect(bus.mostPending).toBeLessThanOrEqual(IN_FLIGHT);
  });

  test("reads a unit's cgroup from its type's interface", async () => {
    bus = await FakeBus.start();
    bus.answer = systemd;
    const client = new DbusSystemdClient(bus.path);
    expect(await client.controlGroup("sshd.service")).toBe("/system.slice/sshd.service");
    expect(await client.controlGroup("multi-user.target")).toBeUndefined();
  });

  test("reconnects for the next query after the bus drops", async () => {
    bus = await FakeBus.start();
    bus.answer = systemd;
    const client = new DbusSystemdClient(bus.path);
    await client.units();
    bus.disconnect();
    await Bun.sleep(10);
    expect(await client.units()).toHaveLength(1);
  });

  test("subscribes, and turns state changes into events until aborted", async () => {
    bus = await FakeBus.start();
    bus.answer = systemd;
    const client = new DbusSystemdClient(bus.path);
    const controller = new AbortController();
    const events: unknown[] = [];
    const reading = (async () => {
      for await (const event of client.events(controller.signal)) {
        events.push(event);
        if (events.length === 2) controller.abort();
      }
    })();

    while (!bus.calls.some((c) => c.member === "Subscribe")) await Bun.sleep(5);
    expect(bus.calls.filter((c) => c.member === "AddMatch").map((c) => c.body?.[0])).toEqual([
      expect.stringContaining("member='PropertiesChanged'"),
      expect.stringContaining("member='Reloading'"),
    ]);

    bus.signal({
      path: SSHD,
      interface: "org.freedesktop.DBus.Properties",
      member: "PropertiesChanged",
      signature: "sa{sv}as",
      body: ["org.freedesktop.systemd1.Unit", { ActiveState: new Variant("s", "deactivating") }, []],
    });
    // The same state again, as systemd sends with any property change.
    bus.signal({
      path: SSHD,
      interface: "org.freedesktop.DBus.Properties",
      member: "PropertiesChanged",
      signature: "sa{sv}as",
      body: ["org.freedesktop.systemd1.Unit", { ActiveState: new Variant("s", "deactivating") }, []],
    });
    bus.signal({
      path: "/org/freedesktop/systemd1",
      interface: "org.freedesktop.systemd1.Manager",
      member: "Reloading",
      signature: "b",
      body: [false],
    });
    await reading;

    expect(events).toEqual([{ unit: "sshd.service", state: "deactivating" }, {}]);
  });

  test("says where a unit was before, once it has been seen", async () => {
    bus = await FakeBus.start();
    bus.answer = systemd;
    const client = new DbusSystemdClient(bus.path);
    const controller = new AbortController();
    const events: unknown[] = [];
    const reading = (async () => {
      for await (const event of client.events(controller.signal)) {
        events.push(event);
        if (events.length === 2) controller.abort();
      }
    })();
    while (!bus.calls.some((c) => c.member === "Subscribe")) await Bun.sleep(5);
    for (const state of ["deactivating", "inactive"]) {
      bus.signal({
        path: SSHD,
        interface: "org.freedesktop.DBus.Properties",
        member: "PropertiesChanged",
        signature: "sa{sv}as",
        body: ["org.freedesktop.systemd1.Unit", { ActiveState: new Variant("s", state) }, []],
      });
    }
    await reading;
    expect(events).toEqual([
      { unit: "sshd.service", state: "deactivating" },
      { unit: "sshd.service", state: "inactive", previous: "deactivating" },
    ]);
    await Bun.sleep(10);
    expect(bus.connected).toBe(0);
  });

  test("ends the stream with an error when the bus drops", async () => {
    bus = await FakeBus.start();
    bus.answer = systemd;
    const client = new DbusSystemdClient(bus.path);
    const reading = (async () => {
      for await (const _ of client.events(new AbortController().signal)) { /* none */ }
    })();
    while (!bus.calls.some((c) => c.member === "Subscribe")) await Bun.sleep(5);
    bus.disconnect();
    await expect(reading).rejects.toThrow(/closed/);
  });
});
