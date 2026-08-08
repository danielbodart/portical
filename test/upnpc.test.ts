import { describe, expect, test } from "bun:test";
import { parseListing } from "../src/upnpc.ts";

// Captured from miniupnpc 2.2.7, the version the image ships.
const LISTING = `upnpc : miniupnpc library test client, version 2.2.7.
 (c) 2005-2024 Thomas Bernard.
More information at https://miniupnp.tuxfamily.org/ or http://miniupnp.free.fr/

List of UPNP devices found on the network :
 desc: http://192.168.1.1:5000/rootDesc.xml
 st: urn:schemas-upnp-org:device:InternetGatewayDevice:1

Found valid IGD : http://192.168.1.1:5000/ctl/IPConn
Local LAN ip address : 192.168.1.5
Connection Type : IP_Routed
Status : Connected, uptime=612345s, LastConnectionError : ERROR_NONE
  Time started : Fri Jan 10 09:15:02 2025
MaxBitRateDown : 1000000000 bps (1000.0 Mbps)   MaxBitRateUp 1000000000 bps (1000.0 Mbps)
ExternalIPAddress = 81.2.69.142
 i protocol exPort->inAddr:inPort description remoteHost leaseTime
 0 TCP  9999->192.168.1.10:8080  'portical: (9999:8080/tcp) nginx' '' 0
 1 UDP 19132->192.168.1.11:19132 'portical: (19132:19132/udp) minecraft' '' 0
 2 TCP 32400->192.168.1.12:32400 'plex' '' 86400
`;

describe("parseListing", () => {
  test("reads every mapping in the table", () => {
    expect(parseListing(LISTING).mappings).toHaveLength(3);
  });

  test("reads the fields of a mapping", () => {
    expect(parseListing(LISTING).mappings[0]).toEqual({
      protocol: "tcp",
      externalPort: 9999,
      internalClient: "192.168.1.10",
      internalPort: 8080,
      description: "portical: (9999:8080/tcp) nginx",
      remoteHost: "",
      leaseDuration: 0,
    });
  });

  test("reads a finite lease", () => {
    expect(parseListing(LISTING).mappings[2]?.leaseDuration).toBe(86400);
  });

  test("picks up the local and external addresses", () => {
    const listing = parseListing(LISTING);
    expect(listing.localAddress).toBe("192.168.1.5");
    expect(listing.externalAddress).toBe("81.2.69.142");
  });

  test("reads an empty table without complaint", () => {
    const empty = parseListing(`Found valid IGD : http://192.168.1.1:5000/ctl/IPConn
Local LAN ip address : 192.168.1.5
ExternalIPAddress = 81.2.69.142
 i protocol exPort->inAddr:inPort description remoteHost leaseTime
`);
    expect(empty.mappings).toEqual([]);
    expect(empty.localAddress).toBe("192.168.1.5");
  });

  test("survives output with no recognisable content at all", () => {
    expect(parseListing("No IGD UPnP Device found on the network !\n")).toEqual({
      mappings: [],
      localAddress: undefined,
      externalAddress: undefined,
    });
  });

  describe("descriptions", () => {
    // Other tools share the router and put whatever they like in here, so a
    // quote in someone else's description must not shift our field boundaries.
    test("containing quotes still parse", () => {
      const mapping = parseListing(
        ` 0 TCP  8080->192.168.1.10:8080  'someone's 'odd' rule' '' 0`,
      ).mappings[0];
      expect(mapping?.description).toBe("someone's 'odd' rule");
      expect(mapping?.leaseDuration).toBe(0);
    });

    test("that are empty still parse", () => {
      expect(
        parseListing(` 0 TCP  8080->192.168.1.10:8080  '' '' 0`).mappings[0]?.description,
      ).toBe("");
    });
  });

  test("reads a non-empty remote host", () => {
    expect(
      parseListing(` 0 TCP  8080->192.168.1.10:8080  'x' '203.0.113.9' 0`).mappings[0]?.remoteHost,
    ).toBe("203.0.113.9");
  });

  // Routers are free to rewrite or truncate descriptions, and several in issue
  // #6 do. Identity has to survive that, so the parser must still return a
  // usable mapping when the description no longer resembles what we wrote.
  test("still yields identity when the router truncated our description", () => {
    const mapping = parseListing(
      ` 0 UDP  6881->192.168.1.10:6881  'portical: (6881:6881/ud' '' 0`,
    ).mappings[0];
    expect(mapping?.externalPort).toBe(6881);
    expect(mapping?.protocol).toBe("udp");
  });
});
