import { afterEach, describe, expect, test } from "bun:test";
import {
  DbusConnection,
  DbusError,
  decode,
  encode,
  MessageType,
  splitSignature,
  systemBusPath,
  Variant,
  type Message,
} from "../src/dbus.ts";
import { FakeBus } from "./fakes/bus.ts";

function hex(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("hex");
}

describe("the wire format", () => {
  /**
   * Worked out by hand from the specification rather than by running the
   * encoder, so it says something the round trips below cannot: that the
   * header fields are padded to eight and the header to eight after them.
   */
  test("writes a header the way the specification lays it out", () => {
    const bytes = encode({ type: MessageType.MethodCall, serial: 1, path: "/", member: "Ping" });
    expect(hex(bytes)).toBe(
      "6c010001" + "00000000" + "01000000" + "1d000000" +
        "01016f00" + "01000000" + "2f00" + "000000000000" +
        "03017300" + "04000000" + "50696e6700" + "000000",
    );
  });

  test("reads back what it wrote", () => {
    const message: Message = {
      type: MessageType.MethodReturn,
      serial: 7,
      replySerial: 3,
      destination: ":1.42",
      signature: "a(ssssssouso)",
      body: [[
        ["sshd.service", "SSH Daemon", "loaded", "active", "running", "", "/org/freedesktop/systemd1/unit/sshd_2eservice", 0, "", "/"],
        ["minecraft.service", "", "loaded", "active", "running", "", "/org/freedesktop/systemd1/unit/minecraft_2eservice", 12, "start", "/org/freedesktop/systemd1/job/12"],
      ]],
    };
    const decoded = decode(encode(message));
    expect(decoded?.message).toMatchObject(message);
    expect(decoded?.length).toBe(encode(message).length);
  });

  test("reads every basic type, aligned wherever it falls", () => {
    const signature = "ybnqiuxtdsogas";
    const body = [7, true, -2, 3, -4, 5, -6n, 7n, 1.5, "text", "/a/path", "sa{sv}", ["x", "yz"]];
    expect(decode(encode({ type: MessageType.Signal, serial: 1, signature, body }))?.message.body).toEqual(body);
  });

  test("reads a dictionary of variants as an object, as GetAll returns", () => {
    const properties = {
      FragmentPath: new Variant("s", "/etc/systemd/system/minecraft.service"),
      DropInPaths: new Variant("as", ["/etc/systemd/system/minecraft.service.d/forward.conf"]),
      NeedDaemonReload: new Variant("b", false),
      InactiveExitTimestamp: new Variant("t", 1729000000000000n),
      Conditions: new Variant("a(sbbsi)", [["ConditionPathExists", false, false, "/x", 0]]),
    };
    const decoded = decode(encode({ type: MessageType.MethodReturn, serial: 1, signature: "a{sv}", body: [properties] }));
    expect(decoded?.message.body).toEqual([
      {
        FragmentPath: "/etc/systemd/system/minecraft.service",
        DropInPaths: ["/etc/systemd/system/minecraft.service.d/forward.conf"],
        NeedDaemonReload: false,
        InactiveExitTimestamp: 1729000000000000n,
        Conditions: [["ConditionPathExists", false, false, "/x", 0]],
      },
    ]);
  });

  test("pads an empty array to its element's alignment", () => {
    const decoded = decode(encode({ type: MessageType.Signal, serial: 1, signature: "ya(tt)u", body: [1, [], 9] }));
    expect(decoded?.message.body).toEqual([1, [], 9]);
  });

  test("waits for the rest of a message that has not all arrived", () => {
    const bytes = encode({ type: MessageType.Signal, serial: 1, member: "X", signature: "s", body: ["hello"] });
    expect(decode(bytes.subarray(0, 10))).toBeUndefined();
    expect(decode(bytes.subarray(0, bytes.length - 1))).toBeUndefined();
    expect(decode(bytes)?.length).toBe(bytes.length);
  });

  test("reads a big-endian message", () => {
    // The same Ping as above, from a big-endian peer.
    const bytes = Buffer.from(
      "42010001" + "00000000" + "00000001" + "0000001d" +
        "01016f00" + "00000001" + "2f00" + "000000000000" +
        "03017300" + "00000004" + "50696e6700" + "000000",
      "hex",
    );
    expect(decode(new Uint8Array(bytes))?.message).toMatchObject({ serial: 1, path: "/", member: "Ping" });
  });

  test("splits a signature into complete types", () => {
    expect(splitSignature("sa{sv}a(ssu)ub")).toEqual(["s", "a{sv}", "a(ssu)", "u", "b"]);
  });
});

describe("systemBusPath", () => {
  test("is the standard socket by default", () => {
    expect(systemBusPath({})).toBe("/run/dbus/system_bus_socket");
  });

  test("follows DBUS_SYSTEM_BUS_ADDRESS", () => {
    expect(systemBusPath({ DBUS_SYSTEM_BUS_ADDRESS: "unix:path=/run/other/bus" })).toBe("/run/other/bus");
    expect(systemBusPath({ DBUS_SYSTEM_BUS_ADDRESS: "tcp:host=x;unix:guid=1,path=/run/b" })).toBe("/run/b");
  });

  test("refuses an address it cannot use", () => {
    expect(() => systemBusPath({ DBUS_SYSTEM_BUS_ADDRESS: "tcp:host=x,port=1" })).toThrow(/unix:path/);
  });
});

describe("DbusConnection", () => {
  let bus: FakeBus;
  afterEach(async () => bus?.stop());

  test("authenticates as its uid and says Hello", async () => {
    bus = await FakeBus.start();
    const connection = await DbusConnection.open(bus.path, 1000);
    expect(bus.auth).toBe("AUTH EXTERNAL 31303030");
    expect(bus.calls[0]).toMatchObject({ member: "Hello", destination: "org.freedesktop.DBus" });
    connection.close();
  });

  test("matches overlapping replies to their calls", async () => {
    bus = await FakeBus.start();
    bus.answer = async (call) => {
      const [n] = call.body as [number];
      // Answered in the reverse order to the one they were asked in.
      await Bun.sleep(50 - n * 10);
      return { signature: "u", body: [n * 2] };
    };
    const connection = await DbusConnection.open(bus.path);
    const replies = await Promise.all(
      [1, 2, 3].map((n) => connection.call("x.y", "/x", "x.y.Z", "Double", "u", [n])),
    );
    expect(replies).toEqual([[2], [4], [6]]);
    connection.close();
  });

  test("rejects a call the far end answered with an error", async () => {
    bus = await FakeBus.start();
    bus.answer = () => new DbusError("org.freedesktop.DBus.Error.AccessDenied", "not you");
    const connection = await DbusConnection.open(bus.path);
    const failure = connection.call("x.y", "/x", "x.y.Z", "Secret").catch((error) => error);
    expect(await failure).toBeInstanceOf(DbusError);
    expect((await failure).message).toBe("org.freedesktop.DBus.Error.AccessDenied: not you");
    connection.close();
  });

  test("delivers signals", async () => {
    bus = await FakeBus.start();
    const connection = await DbusConnection.open(bus.path);
    const received = new Promise<Message>((resolve) => connection.onSignal(resolve));
    bus.signal({ path: "/a", interface: "x.y", member: "Changed", signature: "s", body: ["now"] });
    expect(await received).toMatchObject({ member: "Changed", body: ["now"] });
    connection.close();
  });

  test("fails calls in flight when the connection drops", async () => {
    bus = await FakeBus.start();
    bus.answer = () => new Promise(() => {});
    const connection = await DbusConnection.open(bus.path);
    const closed = new Promise<Error>((resolve) => connection.onClose(resolve));
    const call = connection.call("x.y", "/x", "x.y.Z", "Hang").catch((error: Error) => error);
    await Bun.sleep(20);
    bus.disconnect();
    expect(((await call) as Error).message).toMatch(/closed/);
    expect((await closed).message).toMatch(/closed/);
    expect(connection.closed).toBe(true);
  });

  test("says where it was going when there is no bus", async () => {
    await expect(DbusConnection.open("/nonexistent/bus")).rejects.toThrow(/\/nonexistent\/bus/);
  });
});
