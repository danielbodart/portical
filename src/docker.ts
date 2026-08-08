import { ok, type Handler } from "./http.ts";
import type { Protocol } from "./model.ts";

/** A network a container is attached to, joined with that network's driver. */
export interface ContainerNetwork {
  readonly name: string;
  readonly driver: string;
  readonly ipAddress: string;
}

/** A port the container publishes on the Docker host. */
export interface PublishedPort {
  readonly hostPort: number;
  readonly containerPort: number;
  readonly protocol: Protocol;
}

export interface Container {
  readonly id: string;
  readonly name: string;
  readonly labels: Readonly<Record<string, string>>;
  readonly networks: readonly ContainerNetwork[];
  readonly published: readonly PublishedPort[];
}

export interface DockerEvent {
  readonly action: string;
  readonly container: string;
}

/**
 * The Docker operations Portical needs.
 *
 * An interface rather than a concrete client so the daemon can be driven by a
 * fake in tests. Every decision Portical makes is a function of what this
 * returns, so being able to hand it a container list without running Docker is
 * most of what makes the thing testable.
 */
export interface DockerClient {
  containers(label: string): Promise<Container[]>;
  events(label: string, signal: AbortSignal): AsyncIterable<DockerEvent>;
}

/** Engine API version. 1.41 ships with Docker 20.10, old enough to be safe. */
const API = "v1.41";

/**
 * Talks to the Docker Engine API.
 *
 * v1 shelled out to the `docker` CLI, which meant the image had to carry
 * Docker itself, and a container list cost one process per container plus one
 * more per network. This is two HTTP requests per pass and no CLI at all.
 *
 * It takes a Handler rather than a socket path, so the tests hand it an
 * in-memory Docker and it cannot tell the difference.
 */
export class HttpDockerClient implements DockerClient {
  constructor(private readonly handler: Handler) {}

  private async get(path: string, signal?: AbortSignal): Promise<Response> {
    try {
      return await ok(await this.handler(new Request(`http://docker${path}`, { signal })));
    } catch (cause) {
      throw new Error(`Docker API ${path} failed: ${(cause as Error).message}`, { cause });
    }
  }

  async containers(label: string): Promise<Container[]> {
    // Networks are fetched whole and joined in memory rather than inspected
    // per container: a host has a handful of networks and this keeps a pass at
    // a constant two requests however many containers are labelled.
    const [containers, networks] = await Promise.all([
      this.get(`/${API}/containers/json?filters=${filters({ label: [label] })}`)
        .then((response) => response.json() as Promise<readonly RawContainer[]>),
      this.get(`/${API}/networks`).then(
        (response) => response.json() as Promise<readonly RawNetwork[]>,
      ),
    ]);

    return containers.map((container) => toContainer(container, driversByName(networks)));
  }

  async *events(label: string, signal: AbortSignal): AsyncIterable<DockerEvent> {
    // `destroy` is not listened for: a container that is destroyed has already
    // died, and reacting twice only means reconciling twice.
    const query = filters({
      type: ["container"],
      event: ["start", "die", "stop", "kill", "pause", "unpause"],
      label: [label],
    });
    const response = await this.get(`/${API}/events?filters=${query}`, signal);
    if (!response.body) return;

    for await (const line of lines(response.body)) {
      const raw = JSON.parse(line) as RawEvent;
      yield {
        action: raw.Action ?? "",
        container: raw.Actor?.Attributes?.name ?? raw.id ?? "",
      };
    }
  }
}

/** Docker wants its filters as a URL-encoded JSON map of name to values. */
function filters(spec: Record<string, string[]>): string {
  return encodeURIComponent(JSON.stringify(spec));
}

/** Split a chunked NDJSON body into lines, holding partial lines back. */
async function* lines(body: ReadableStream<Uint8Array>): AsyncIterable<string> {
  const decoder = new TextDecoder();
  let buffer = "";

  for await (const chunk of body) {
    buffer += decoder.decode(chunk, { stream: true });
    let newline: number;
    while ((newline = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, newline).trim();
      buffer = buffer.slice(newline + 1);
      if (line !== "") yield line;
    }
  }
}

interface RawContainer {
  Id: string;
  Names?: string[];
  Labels?: Record<string, string>;
  Ports?: { PrivatePort: number; PublicPort?: number; Type: string }[];
  NetworkSettings?: { Networks?: Record<string, { IPAddress?: string }> };
}

interface RawNetwork {
  Name: string;
  Driver: string;
}

interface RawEvent {
  Action?: string;
  id?: string;
  Actor?: { Attributes?: { name?: string } };
}

function driversByName(networks: readonly RawNetwork[]): Map<string, string> {
  return new Map(networks.map((network) => [network.Name, network.Driver]));
}

/**
 * Turn one Engine API container into our own shape.
 *
 * Exported for tests, which drive it from captured API responses - the mapping
 * is where the interesting mistakes live, not in the HTTP.
 */
export function toContainer(
  raw: RawContainer,
  drivers: Map<string, string>,
): Container {
  return {
    id: raw.Id,
    // Docker reports names with a leading slash, and a container may have
    // several. The first is the one people recognise, and it is what ends up
    // in the rule description on the router.
    name: (raw.Names?.[0] ?? raw.Id).replace(/^\//, ""),
    labels: raw.Labels ?? {},
    networks: Object.entries(raw.NetworkSettings?.Networks ?? {}).map(([name, network]) => ({
      name,
      // A network attached by id, or removed between the two calls, has no
      // entry. Reported as unknown so it is skipped as an unsupported driver
      // rather than silently treated as bridge.
      driver: drivers.get(name) ?? "unknown",
      ipAddress: network.IPAddress ?? "",
    })),
    published: publishedPorts(raw.Ports ?? []),
  };
}

/**
 * The ports this container actually publishes on the host.
 *
 * Two things are filtered out. Ports with no PublicPort are merely exposed,
 * not published, so nothing on the host listens for them. And Docker reports a
 * single `-p 8888:80` twice, once for 0.0.0.0 and once for ::, which would
 * otherwise produce a duplicate rule for every published port.
 */
function publishedPorts(
  ports: readonly { PrivatePort: number; PublicPort?: number; Type: string }[],
): PublishedPort[] {
  const found = new Map<string, PublishedPort>();

  for (const port of ports) {
    if (port.PublicPort === undefined) continue;
    if (port.Type !== "tcp" && port.Type !== "udp") continue;

    const key = `${port.Type}/${port.PublicPort}`;
    if (!found.has(key)) {
      found.set(key, {
        hostPort: port.PublicPort,
        containerPort: port.PrivatePort,
        protocol: port.Type,
      });
    }
  }

  return [...found.values()];
}
