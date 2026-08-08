import { beforeEach, describe, expect, test } from "bun:test";
import { UpnpError, UpnpGateway } from "../src/upnp.ts";
import { FakeGateway, type Quirks } from "./fakes/gateway.ts";

async function connect(quirks: Quirks = {}): Promise<[UpnpGateway, FakeGateway]> {
  const fake = new FakeGateway(quirks);
  return [await UpnpGateway.at(fake.handler, fake.rootUrl), fake];
}

describe("UpnpGateway.at", () => {
  test("finds the port forwarding service in a device description", async () => {
    const [gateway] = await connect();
    expect(gateway.controlUrl).toBe("http://192.168.1.1:5000/ctl/IPConn");
    expect(gateway.serviceType).toBe("urn:schemas-upnp-org:service:WANIPConnection:1");
  });

  // Routers that omit URLBase expect control URLs resolved against wherever
  // the description was fetched from, which is not always the root.
  test("resolves a relative control URL against the description's own URL", async () => {
    const fake = new FakeGateway({}, "http://192.168.1.1:5000/rootDesc.xml");
    const gateway = await UpnpGateway.at(fake.handler, fake.rootUrl);
    expect(gateway.controlUrl).toBe("http://192.168.1.1:5000/ctl/IPConn");
  });

  test("says so plainly when the device forwards no ports", async () => {
    const handler = async () =>
      new Response(`<root><device><serviceList><service>
        <serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
        <controlURL>/ctl/L3F</controlURL></service></serviceList></device></root>`);
    expect(UpnpGateway.at(handler, "http://192.168.1.1:5000/rootDesc.xml")).rejects.toThrow(
      /offers no port forwarding service/,
    );
  });

  test("reports an unreachable gateway rather than hanging", async () => {
    const handler = async () => new Response("nope", { status: 404, statusText: "Not Found" });
    expect(UpnpGateway.at(handler, "http://192.168.1.1:5000/rootDesc.xml")).rejects.toThrow(/404/);
  });
});

describe("mappings", () => {
  test("reads an empty table", async () => {
    const [gateway] = await connect();
    expect(await gateway.mappings()).toEqual([]);
  });

  test("reads every row", async () => {
    const [gateway, fake] = await connect();
    fake.given({ externalPort: 9999, protocol: "tcp", description: "portical: one" });
    fake.given({ externalPort: 19132, protocol: "udp", description: "portical: two" });
    expect(await gateway.mappings()).toHaveLength(2);
  });

  test("reads the fields of a row", async () => {
    const [gateway, fake] = await connect();
    fake.given({
      externalPort: 9999,
      protocol: "tcp",
      internalPort: 8080,
      internalClient: "192.168.1.10",
      description: "portical: (9999:8080/tcp) nginx",
      leaseDuration: 86400,
    });
    expect((await gateway.mappings())[0]).toEqual({
      externalPort: 9999,
      protocol: "tcp",
      internalPort: 8080,
      internalClient: "192.168.1.10",
      description: "portical: (9999:8080/tcp) nginx",
      remoteHost: "",
      leaseDuration: 86400,
    });
  });

  // The specification says 713 ends the table, but firmwares in the wild
  // answer 402 or 501 instead. Treating those as failures would leave Portical
  // unable to read its own rules on those routers.
  test.each([713, 402, 501])("treats code %i as the end of the table", async (code) => {
    const [gateway, fake] = await connect({ endOfTableCode: code });
    fake.given({ externalPort: 9999, protocol: "tcp" });
    expect(await gateway.mappings()).toHaveLength(1);
  });

  test("decodes entities in a description", async () => {
    const [gateway, fake] = await connect();
    fake.given({ externalPort: 80, protocol: "tcp", description: `Bob & Alice's "rule"` });
    expect((await gateway.mappings())[0]?.description).toBe(`Bob & Alice's "rule"`);
  });
});

describe("add", () => {
  let gateway: UpnpGateway;
  let fake: FakeGateway;
  beforeEach(async () => { [gateway, fake] = await connect(); });

  test("creates a mapping", async () => {
    await gateway.add({
      externalPort: 9999,
      protocol: "tcp",
      internalPort: 80,
      internalClient: "192.168.1.40",
      description: "portical: (9999:80/tcp) nginx",
      leaseDuration: 0,
    });
    expect(fake.mappings).toEqual([
      {
        externalPort: 9999,
        protocol: "tcp",
        internalPort: 80,
        internalClient: "192.168.1.40",
        description: "portical: (9999:80/tcp) nginx",
        remoteHost: "",
        leaseDuration: 0,
      },
    ]);
  });

  test("replaces rather than duplicates an existing external port", async () => {
    const mapping = {
      externalPort: 9999,
      protocol: "tcp",
      internalPort: 80,
      internalClient: "192.168.1.40",
      description: "portical: one",
      leaseDuration: 0,
    } as const;
    await gateway.add(mapping);
    await gateway.add({ ...mapping, internalPort: 8080 });
    expect(fake.mappings).toHaveLength(1);
    expect(fake.mappings[0]?.internalPort).toBe(8080);
  });

  // Container names may contain characters that would close the element early.
  test("escapes a description that contains markup", async () => {
    await gateway.add({
      externalPort: 80,
      protocol: "tcp",
      internalPort: 80,
      internalClient: "192.168.1.40",
      description: `portical: <a & b> "c"`,
      leaseDuration: 0,
    });
    expect(fake.mappings[0]?.description).toBe(`portical: <a & b> "c"`);
  });

  test("reports the gateway's own reason for a refusal", async () => {
    const [refusing] = await connect({ refuseWith: 606 });
    const failure = refusing.add({
      externalPort: 80,
      protocol: "tcp",
      internalPort: 80,
      internalClient: "192.168.1.40",
      description: "portical: nginx",
      leaseDuration: 0,
    });
    expect(failure).rejects.toThrow(
      "AddPortMapping failed: 606 Action not authorized - UPnP is enabled but not authorised for this client",
    );
    expect(failure).rejects.toBeInstanceOf(UpnpError);
  });
});

describe("remove", () => {
  test("deletes a mapping", async () => {
    const [gateway, fake] = await connect();
    fake.given({ externalPort: 9999, protocol: "tcp" });
    await gateway.remove(9999, "tcp");
    expect(fake.mappings).toEqual([]);
  });

  test("leaves the other protocol on the same port alone", async () => {
    const [gateway, fake] = await connect();
    fake.given({ externalPort: 9999, protocol: "tcp" });
    fake.given({ externalPort: 9999, protocol: "udp" });
    await gateway.remove(9999, "tcp");
    expect(fake.mappings).toHaveLength(1);
    expect(fake.mappings[0]?.protocol).toBe("udp");
  });

  // Issue #6's error. v1 treated 714 as fatal and exited, taking the daemon
  // down over a mapping that was already in the state we wanted.
  test("treats deleting a mapping that is not there as success", async () => {
    const [gateway] = await connect();
    expect(await gateway.remove(9999, "tcp")).toBeUndefined();
  });
});

describe("externalAddress", () => {
  test("reads the WAN address", async () => {
    const [gateway] = await connect();
    expect(await gateway.externalAddress()).toBe("81.2.69.142");
  });

  // Reported by routers that have not got a WAN address yet, which is worse
  // than saying nothing at all.
  test("treats 0.0.0.0 as not knowing", async () => {
    const [gateway, fake] = await connect();
    fake.externalIp = "0.0.0.0";
    expect(await gateway.externalAddress()).toBeUndefined();
  });
});
