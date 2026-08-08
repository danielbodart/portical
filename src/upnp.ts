import { ok, type Handler } from "./http.ts";
import type { Mapping, Protocol } from "./model.ts";
import { elements, encode, text } from "./xml.ts";

/** A mapping we are asking the gateway to create. */
export interface NewMapping {
  readonly externalPort: number;
  readonly protocol: Protocol;
  readonly internalPort: number;
  readonly internalClient: string;
  readonly description: string;
  /** 0 asks for a mapping that never expires. */
  readonly leaseDuration: number;
}

/**
 * The gateway operations Portical needs.
 *
 * v1 got these by running `upnpc`, which meant the image carried miniupnpc,
 * failures arrived as exit codes with the reason on stdout, and testing any of
 * it needed a real router. Behind this interface it is an HTTP conversation
 * like any other.
 */
export interface Gateway {
  mappings(): Promise<Mapping[]>;
  add(mapping: NewMapping): Promise<void>;
  remove(externalPort: number, protocol: Protocol): Promise<void>;
  externalAddress(): Promise<string | undefined>;
}

/**
 * A UPnP error, carrying what the gateway actually said.
 *
 * The shell version originally reported only an exit status, which left
 * "something went wrong" as the entire diagnosis - the reason issue #6 was so
 * hard for people to read. The gateway's own code and description are far more
 * useful, so they are kept whole.
 */
export class UpnpError extends Error {
  constructor(
    readonly code: number,
    readonly description: string,
    action: string,
  ) {
    super(`${action} failed: ${code} ${description}${EXPLANATIONS[code] ? ` - ${EXPLANATIONS[code]}` : ""}`);
    this.name = "UpnpError";
  }
}

/** Plain-language notes for the codes people actually hit. */
const EXPLANATIONS: Readonly<Record<number, string>> = {
  402: "the gateway rejected the arguments",
  501: "the gateway refused the action",
  606: "UPnP is enabled but not authorised for this client",
  713: "no mapping at that index",
  714: "no such mapping",
  715: "the gateway requires a specific source address",
  716: "the gateway requires a specific external port",
  718: "another mapping already claims that external port",
  724: "the gateway requires the internal and external ports to match",
  725: "the gateway only supports permanent leases",
  726: "the gateway only supports a wildcard remote host",
  727: "the gateway only supports a wildcard external port",
  729: "conflicts with a mapping made by another mechanism",
};

/** No mapping at that index - the gateway's way of saying the table ended. */
const NO_SUCH_INDEX = 713;
/** No such mapping - deleting something that was already gone. */
export const NO_SUCH_MAPPING = 714;

/**
 * A stop for a runaway listing.
 *
 * The table is read by asking for index 0, 1, 2... until the gateway says
 * there is nothing there. A router that answers every index instead of
 * reporting the end would otherwise spin forever.
 */
const MAX_MAPPINGS = 4096;

/** Service types that can forward a port, best first. */
const SERVICES = [
  "urn:schemas-upnp-org:service:WANIPConnection:2",
  "urn:schemas-upnp-org:service:WANIPConnection:1",
  "urn:schemas-upnp-org:service:WANPPPConnection:1",
];

export class UpnpGateway implements Gateway {
  constructor(
    private readonly handler: Handler,
    readonly controlUrl: string,
    readonly serviceType: string,
  ) {}

  /**
   * Read a gateway's device description and find the service that forwards
   * ports.
   *
   * This is what `-r`/`PORTICAL_UPNP_ROOT_URL` points at, and what SSDP
   * discovery returns.
   */
  static async at(handler: Handler, rootDescriptionUrl: string): Promise<UpnpGateway> {
    const description = await ok(await handler(new Request(rootDescriptionUrl))).then((r) => r.text());

    // URLBase is optional and routers that omit it expect control URLs to be
    // resolved against wherever the description was fetched from.
    const base = text(description, "URLBase")?.trim() || rootDescriptionUrl;

    for (const wanted of SERVICES) {
      for (const service of elements(description, "service")) {
        if (text(service, "serviceType")?.trim() !== wanted) continue;
        const control = text(service, "controlURL")?.trim();
        if (control === undefined || control === "") continue;
        return new UpnpGateway(handler, new URL(control, base).toString(), wanted);
      }
    }

    throw new Error(
      `${rootDescriptionUrl} is a UPnP device but offers no port forwarding service ` +
        `(looked for ${SERVICES.join(", ")})`,
    );
  }

  async mappings(): Promise<Mapping[]> {
    const found: Mapping[] = [];

    for (let index = 0; index < MAX_MAPPINGS; index++) {
      let entry: string;
      try {
        entry = await this.call("GetGenericPortMappingEntry", {
          NewPortMappingIndex: String(index),
        });
      } catch (error) {
        // The end of the table. Routers disagree about how to say so: the spec
        // says 713, but plenty answer 402 or 501 once the index runs past the
        // end, and treating those as failures would make Portical unable to
        // read its own rules on those routers.
        if (error instanceof UpnpError && [NO_SUCH_INDEX, 402, 501, 713].includes(error.code)) break;
        throw error;
      }

      const mapping = toMapping(entry);
      if (mapping) found.push(mapping);
    }

    return found;
  }

  async add(mapping: NewMapping): Promise<void> {
    // Argument order follows the IGD specification exactly. It should not
    // matter, but a number of firmwares parse positionally and reject anything
    // else with a bare 402.
    await this.call("AddPortMapping", {
      NewRemoteHost: "",
      NewExternalPort: String(mapping.externalPort),
      NewProtocol: mapping.protocol.toUpperCase(),
      NewInternalPort: String(mapping.internalPort),
      NewInternalClient: mapping.internalClient,
      NewEnabled: "1",
      NewPortMappingDescription: mapping.description,
      NewLeaseDuration: String(mapping.leaseDuration),
    });
  }

  async remove(externalPort: number, protocol: Protocol): Promise<void> {
    try {
      await this.call("DeletePortMapping", {
        NewRemoteHost: "",
        NewExternalPort: String(externalPort),
        NewProtocol: protocol.toUpperCase(),
      });
    } catch (error) {
      // Already gone is the outcome we wanted. This is the error at the heart
      // of issue #6: v1 treated it as fatal and took the whole daemon down
      // with it, when it means nothing more than "there was nothing to do".
      if (error instanceof UpnpError && error.code === NO_SUCH_MAPPING) return;
      throw error;
    }
  }

  async externalAddress(): Promise<string | undefined> {
    const response = await this.call("GetExternalIPAddress", {});
    const address = text(response, "NewExternalIPAddress")?.trim();
    // Routers that have not got a WAN address yet report 0.0.0.0 rather than
    // failing, which is worse than saying nothing.
    return address === undefined || address === "" || address === "0.0.0.0" ? undefined : address;
  }

  private async call(action: string, args: Record<string, string>): Promise<string> {
    const body =
      `<?xml version="1.0"?>\n` +
      `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
      `s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
      `<s:Body><u:${action} xmlns:u="${this.serviceType}">` +
      Object.entries(args)
        .map(([name, value]) => `<${name}>${encode(value)}</${name}>`)
        .join("") +
      `</u:${action}></s:Body></s:Envelope>`;

    const response = await this.handler(
      new Request(this.controlUrl, {
        method: "POST",
        headers: {
          "Content-Type": 'text/xml; charset="utf-8"',
          SOAPAction: `"${this.serviceType}#${action}"`,
        },
        body,
      }),
    );

    const payload = await response.text();

    // A SOAP fault arrives as HTTP 500 with the real reason in the body, so
    // the body is read before the status is judged.
    if (!response.ok) {
      const code = Number(text(payload, "errorCode"));
      if (Number.isFinite(code) && code > 0) {
        throw new UpnpError(code, text(payload, "errorDescription")?.trim() ?? "", action);
      }
      throw new Error(`${action} failed: HTTP ${response.status} ${payload.trim()}`.trim());
    }

    return payload;
  }
}

/**
 * Read one row of the gateway's mapping table.
 *
 * Rows that are disabled, or that name a protocol we do not manage, are
 * dropped rather than half-understood - a row we cannot read correctly is one
 * we might otherwise delete.
 */
function toMapping(entry: string): Mapping | undefined {
  const protocol = text(entry, "NewProtocol")?.trim().toLowerCase();
  if (protocol !== "tcp" && protocol !== "udp") return undefined;

  const externalPort = Number(text(entry, "NewExternalPort"));
  const internalPort = Number(text(entry, "NewInternalPort"));
  if (!Number.isInteger(externalPort) || !Number.isInteger(internalPort)) return undefined;

  return {
    externalPort,
    protocol,
    internalPort,
    internalClient: text(entry, "NewInternalClient")?.trim() ?? "",
    description: text(entry, "NewPortMappingDescription") ?? "",
    remoteHost: text(entry, "NewRemoteHost")?.trim() ?? "",
    leaseDuration: Number(text(entry, "NewLeaseDuration")) || 0,
  };
}
