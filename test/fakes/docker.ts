import type { Container, ContainerNetwork, DockerClient, DockerEvent, PublishedPort } from "../../src/docker.ts";

/**
 * Docker as an object, so a whole reconcile can be driven without a daemon.
 *
 * Containers are started and stopped by calling methods, and the event stream
 * they produce is the same one the real client produces, so the daemon under
 * test cannot tell the difference.
 */
export class FakeDocker implements DockerClient {
  running: Container[] = [];
  /** Set to make listing fail, as a Docker restart would. */
  failWith?: Error;
  /** Every relay container Portical asked for, in order. */
  readonly relayed: RelayCall[] = [];
  /** Answers relay requests. Set by tests that exercise the namespace path. */
  relay?: (call: RelayCall) => string | Promise<string>;
  /** What imageOf reports, keyed by container. */
  images: Record<string, string> = {};

  private waiting: ((event: DockerEvent) => void)[] = [];
  private queue: DockerEvent[] = [];

  async containers(label: string): Promise<Container[]> {
    if (this.failWith) throw this.failWith;
    return this.running.filter((container) => container.labels[label] !== undefined);
  }

  async *events(_label: string, signal: AbortSignal): AsyncIterable<DockerEvent> {
    while (!signal.aborted) {
      const queued = this.queue.shift();
      if (queued) { yield queued; continue; }
      const next = await new Promise<DockerEvent | undefined>((resolve) => {
        this.waiting.push(resolve);
        signal.addEventListener("abort", () => resolve(undefined), { once: true });
      });
      if (!next) return;
      yield next;
    }
  }

  async runInNetworkOf(container: string, image: string, command: string[]): Promise<string> {
    const call = { container, image, command };
    this.relayed.push(call);
    if (!this.relay) throw new Error(`no relay configured for ${container}`);
    return this.relay(call);
  }

  async imageOf(container: string): Promise<string | undefined> {
    return this.images[container];
  }

  start(container: Container): void {
    this.running.push(container);
    this.emit({ action: "start", container: container.name });
  }

  stop(name: string): void {
    this.running = this.running.filter((container) => container.name !== name);
    this.emit({ action: "die", container: name });
  }

  private emit(event: DockerEvent): void {
    const waiter = this.waiting.shift();
    if (waiter) waiter(event);
    else this.queue.push(event);
  }
}

export function container(
  name: string,
  labels: Record<string, string>,
  networks: ContainerNetwork[] = [{ name: "bridge", driver: "bridge", ipAddress: "172.17.0.2" }],
  published: PublishedPort[] = [],
): Container {
  return { id: `id-${name}`, name, labels, networks, published };
}

export function network(name: string, driver: string, ipAddress = ""): ContainerNetwork {
  return { name, driver, ipAddress };
}

/** Records relay requests so tests can assert Portical asked from the right place. */
export interface RelayCall {
  readonly container: string;
  readonly image: string;
  readonly command: string[];
}
