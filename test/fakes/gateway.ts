import type { Handler } from "../../src/http.ts";
import type { Mapping, Protocol } from "../../src/model.ts";
import { encode, text } from "../../src/xml.ts";

/**
 * The ways real routers misbehave, as switches.
 *
 * Every one of these is taken from a report on the tracker or from the IGD
 * specification's list of permitted refusals. Being able to turn them on is
 * the point of the fake: issue #6 is a router truncating descriptions, and
 * before this there was no way to reproduce that without owning the router.
 */
export interface Quirks {
  /** Truncate descriptions to this many characters, as several firmwares do. */
  readonly descriptionLimit?: number;
  /** Refuse an unlimited lease and substitute this many seconds. */
  readonly leaseLimit?: number;
  /** Report the end of the mapping table with this code instead of 713. */
  readonly endOfTableCode?: number;
  /** Refuse to map to an address other than the one that asked. */
  readonly onlyMapsRequester?: string;
  /** Refuse AddPortMapping outright, as an unauthorised client would see. */
  readonly refuseWith?: number;
}

const FAULTS: Readonly<Record<number, string>> = {
  402: "Invalid Args",
  501: "Action Failed",
  606: "Action not authorized",
  713: "SpecifiedArrayIndexInvalid",
  714: "NoSuchEntryInArray",
  718: "ConflictInMappingEntry",
  725: "OnlyPermanentLeasesSupported",
};

const SERVICE = "urn:schemas-upnp-org:service:WANIPConnection:1";

/**
 * An internet gateway that exists only as a function.
 *
 * Because the rest of Portical reaches the router through a Handler, this
 * stands in for one with no server, no port and no router - the whole daemon
 * can be driven end to end against it in memory.
 */
export class FakeGateway {
  readonly mappings: Mapping[] = [];
  readonly actions: string[] = [];
  externalIp = "81.2.69.142";

  constructor(
    private readonly quirks: Quirks = {},
    readonly rootUrl = "http://192.168.1.1:5000/rootDesc.xml",
  ) {}

  readonly handler: Handler = async (request) => {
    const url = new URL(request.url);

    if (request.method === "GET" && url.pathname === "/rootDesc.xml") {
      return xml(this.description());
    }

    if (request.method === "POST" && url.pathname === "/ctl/IPConn") {
      return this.soap(await request.text());
    }

    return new Response("Not Found", { status: 404 });
  };

  /** Put a mapping in place without going through the gateway's own API. */
  given(mapping: Partial<Mapping> & Pick<Mapping, "externalPort" | "protocol">): this {
    this.mappings.push({
      internalClient: "192.168.1.5",
      internalPort: mapping.externalPort,
      description: "",
      remoteHost: "",
      leaseDuration: 0,
      ...mapping,
    });
    return this;
  }

  private description(): string {
    return `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <friendlyName>Fake Router</friendlyName>
    <deviceList><device>
      <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
      <deviceList><device>
        <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
        <serviceList>
          <service>
            <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
            <serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
            <controlURL>/ctl/IPConn</controlURL>
            <eventSubURL>/evt/IPConn</eventSubURL>
            <SCPDURL>/WANIPCn.xml</SCPDURL>
          </service>
        </serviceList>
      </device></deviceList>
    </device></deviceList>
  </device>
</root>`;
  }

  private soap(body: string): Response {
    const action = /<(?:\w+:)?(\w+) xmlns:u=/.exec(body)?.[1] ?? "";
    this.actions.push(action);

    switch (action) {
      case "GetGenericPortMappingEntry": return this.entry(Number(text(body, "NewPortMappingIndex")));
      case "AddPortMapping": return this.add(body);
      case "DeletePortMapping": return this.remove(body);
      case "GetExternalIPAddress":
        return xml(envelope("GetExternalIPAddressResponse", { NewExternalIPAddress: this.externalIp }));
      default: return fault(401, "Invalid Action");
    }
  }

  private entry(index: number): Response {
    const mapping = this.mappings[index];
    if (!mapping) {
      const code = this.quirks.endOfTableCode ?? 713;
      return fault(code, FAULTS[code] ?? "SpecifiedArrayIndexInvalid");
    }
    return xml(
      envelope("GetGenericPortMappingEntryResponse", {
        NewRemoteHost: mapping.remoteHost,
        NewExternalPort: String(mapping.externalPort),
        NewProtocol: mapping.protocol.toUpperCase(),
        NewInternalPort: String(mapping.internalPort),
        NewInternalClient: mapping.internalClient,
        NewEnabled: "1",
        NewPortMappingDescription: mapping.description,
        NewLeaseDuration: String(mapping.leaseDuration),
      }),
    );
  }

  private add(body: string): Response {
    if (this.quirks.refuseWith) {
      return fault(this.quirks.refuseWith, FAULTS[this.quirks.refuseWith] ?? "Action Failed");
    }

    const internalClient = text(body, "NewInternalClient") ?? "";
    if (this.quirks.onlyMapsRequester && internalClient !== this.quirks.onlyMapsRequester) {
      return fault(718, FAULTS[718]!);
    }

    const externalPort = Number(text(body, "NewExternalPort"));
    const protocol = (text(body, "NewProtocol") ?? "").toLowerCase() as Protocol;
    const description = text(body, "NewPortMappingDescription") ?? "";
    const requested = Number(text(body, "NewLeaseDuration")) || 0;

    const mapping: Mapping = {
      externalPort,
      protocol,
      internalPort: Number(text(body, "NewInternalPort")),
      internalClient,
      description: this.quirks.descriptionLimit
        ? description.slice(0, this.quirks.descriptionLimit)
        : description,
      remoteHost: text(body, "NewRemoteHost") ?? "",
      leaseDuration: requested === 0 && this.quirks.leaseLimit ? this.quirks.leaseLimit : requested,
    };

    // The IGD table is keyed on (remote host, external port, protocol), so
    // adding over an existing entry replaces it rather than duplicating.
    const existing = this.mappings.findIndex(
      (m) => m.externalPort === externalPort && m.protocol === protocol,
    );
    if (existing === -1) this.mappings.push(mapping);
    else this.mappings[existing] = mapping;

    return xml(envelope("AddPortMappingResponse", {}));
  }

  private remove(body: string): Response {
    const externalPort = Number(text(body, "NewExternalPort"));
    const protocol = (text(body, "NewProtocol") ?? "").toLowerCase();
    const index = this.mappings.findIndex(
      (m) => m.externalPort === externalPort && m.protocol === protocol,
    );
    if (index === -1) return fault(714, FAULTS[714]!);
    this.mappings.splice(index, 1);
    return xml(envelope("DeletePortMappingResponse", {}));
  }
}

function envelope(action: string, args: Record<string, string>): string {
  return `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:${action} xmlns:u="${SERVICE}">${Object.entries(args)
    .map(([name, value]) => `<${name}>${encode(value)}</${name}>`)
    .join("")}</u:${action}></s:Body></s:Envelope>`;
}

function fault(code: number, description: string): Response {
  return xml(
    `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>
<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
<errorCode>${code}</errorCode><errorDescription>${description}</errorDescription>
</UPnPError></detail></s:Fault></s:Body></s:Envelope>`,
    500,
  );
}

function xml(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "Content-Type": 'text/xml; charset="utf-8"' } });
}
