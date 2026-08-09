import { describe, expect, test } from "bun:test";
import { firstGatewayAmong, parseArguments } from "../src/main.ts";
import { FakeGateway } from "./fakes/gateway.ts";

describe("parseArguments", () => {
  test("defaults to running the daemon", () => {
    expect(parseArguments([], {}).command).toBe("run");
  });

  test.each(["run", "poll", "listen", "update", "list"])("takes '%s' as the command", (name) => {
    expect(parseArguments([name], {}).command).toBe(name);
  });

  /**
   * v1 was a shell script invoked by its full path, and its own README told
   * people to write `command: "/opt/portical/run poll"`. Those compose files
   * are still running, and v2 sets an entrypoint, so that path now arrives as
   * an ordinary argument. Read as a command name it would break every one of
   * them on upgrade.
   */
  describe("compatibility with v1 command lines", () => {
    test.each([
      [["/opt/portical/run", "listen"], "listen"],
      [["/opt/portical/run", "poll"], "poll"],
      [["/opt/portical/run", "update"], "update"],
      [["/usr/local/bin/portical", "update"], "update"],
    ])("%j runs %s", (argv, expected) => {
      expect(parseArguments(argv as string[], {}).command).toBe(expected);
    });

    test("the bare path on its own still runs the daemon", () => {
      expect(parseArguments(["/opt/portical/run"], {}).command).toBe("run");
    });

    test("options around it are still read", () => {
      const parsed = parseArguments(["/opt/portical/run", "-v", "-f", "update"], {});
      expect(parsed.command).toBe("update");
      expect(parsed.options.force).toBe(true);
    });

    // Only a *path* is skipped. Our own command is a bare word, and a relay
    // payload could be anything, so neither must be swallowed.
    test("does not swallow the run command itself", () => {
      expect(parseArguments(["run"], {}).command).toBe("run");
    });
  });

  test("reads v1's environment variables", () => {
    const parsed = parseArguments([], {
      PORTICAL_UPNP_ROOT_URL: "http://10.0.0.1:5000/rootDesc.xml",
      PORTICAL_POLL_INTERVAL: "30",
    });
    expect(parsed.root).toBe("http://10.0.0.1:5000/rootDesc.xml");
    expect(parsed.options.interval).toBe(30);
  });

  test("accepts v1's flags", () => {
    const parsed = parseArguments(["-r", "http://x/d.xml", "-d", "5", "-l", "my.label", "-f", "-v"], {});
    expect(parsed.root).toBe("http://x/d.xml");
    expect(parsed.options.interval).toBe(5);
    expect(parsed.options.label).toBe("my.label");
    expect(parsed.options.force).toBe(true);
  });

  // -v is v1's verbose flag and is still accepted and ignored, so the version
  // flag only has a long form. Asking for one is not asking to run anything.
  test("asks for the version without naming a command", () => {
    const parsed = parseArguments(["--version"], {});
    expect(parsed.showVersion).toBe(true);
    expect(parsed.command).toBe("run");
  });

  test("does not mistake -v for it", () => {
    expect(parseArguments(["-v"], {}).showVersion).toBe(false);
  });

  test("rejects an unknown option rather than ignoring it", () => {
    expect(() => parseArguments(["--nonsense"], {})).toThrow(/unknown option '--nonsense'/);
  });

  test("rejects a non-numeric interval", () => {
    expect(() => parseArguments(["-d", "soon"], {})).toThrow(/positive number/);
  });
});

/**
 * A device answering a search for an InternetGatewayDevice while forwarding no
 * ports is not hypothetical - one was found doing exactly that on the network
 * this was developed against, and it was the only device that replied before
 * the actual router.
 */
describe("firstGatewayAmong", () => {
  const notAGateway = async () =>
    new Response(`<root><device><serviceList><service>
      <serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
      <controlURL>/ctl/L3F</controlURL></service></serviceList></device></root>`);

  test("skips a device that forwards no ports and uses the next", async () => {
    const real = new FakeGateway();
    const handler = async (request: Request) =>
      new URL(request.url).hostname === "10.0.0.203"
        ? await notAGateway()
        : await real.handler(request);

    const gateway = await firstGatewayAmong(handler, [
      "http://10.0.0.203:80/description.xml",
      real.rootUrl,
    ]);

    expect(gateway.controlUrl).toBe("http://192.168.1.1:5000/ctl/IPConn");
  });

  test("skips one that cannot be reached at all", async () => {
    const real = new FakeGateway();
    const handler = async (request: Request) =>
      new URL(request.url).hostname === "10.0.0.203"
        ? new Response("nope", { status: 500 })
        : await real.handler(request);

    expect(
      (await firstGatewayAmong(handler, ["http://10.0.0.203/d.xml", real.rootUrl])).controlUrl,
    ).toContain("/ctl/IPConn");
  });

  test("reports every device it tried when none will do", async () => {
    const failure = firstGatewayAmong(notAGateway, [
      "http://10.0.0.203/d.xml",
      "http://10.0.0.128/d.xml",
    ]);
    await expect(failure).rejects.toThrow(/found 2 UPnP devices, but none of them forwards ports/);
    await expect(failure).rejects.toThrow(/10.0.0.128/);
    await expect(failure).rejects.toThrow(/Set --root/);
  });
});
