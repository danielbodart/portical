import { Channel } from "../../src/channel.ts";
import type { Host, SystemdClient, SystemdEvent, Unit } from "../../src/systemd.ts";

/**
 * systemd as an object: units are started and stopped by calling methods, and
 * each change comes out of the event stream the way an ActiveState change
 * would from the real thing.
 */
export class FakeSystemd implements SystemdClient {
  running: Unit[] = [];
  /** ControlGroup by unit name. */
  groups: Record<string, string> = {};
  /** Set to make listing fail, as the bus going away would. */
  failWith?: Error;
  private stream?: Channel<SystemdEvent>;
  private readonly queued: SystemdEvent[] = [];

  async units(): Promise<Unit[]> {
    if (this.failWith) throw this.failWith;
    return this.running;
  }

  async controlGroup(unit: string): Promise<string | undefined> {
    return this.groups[unit];
  }

  async *events(signal: AbortSignal): AsyncIterable<SystemdEvent> {
    const stream = new Channel<SystemdEvent>();
    this.stream = stream;
    for (const event of this.queued.splice(0)) stream.push(event);
    const stop = () => stream.end();
    signal.addEventListener("abort", stop);
    try {
      yield* stream;
    } finally {
      signal.removeEventListener("abort", stop);
    }
  }

  start(unit: Unit): void {
    this.running.push(unit);
    this.emit({ unit: unit.name, state: "active", previous: "inactive" });
  }

  stop(name: string): void {
    this.running = this.running.filter((unit) => unit.name !== name);
    this.emit({ unit: name, state: "inactive", previous: "active" });
  }

  emit(event: SystemdEvent): void {
    if (this.stream) this.stream.push(event);
    else this.queued.push(event);
  }

  /** Make the current event stream throw, as the bus dropping would. */
  dropStream(error: Error = new Error("D-Bus connection closed")): void {
    this.stream?.fail(error);
    this.stream = undefined;
  }
}

export function unit(name: string, fragmentPath = `/etc/systemd/system/${name}`, more: Partial<Unit> = {}): Unit {
  return { name, fragmentPath, dropInPaths: [], triggeredBy: [], needDaemonReload: false, ...more };
}

/** A machine whose files are a map and whose sockets are a string. */
export class FakeHost implements Host {
  files: Record<string, string> = {};
  ss = "";
  ssRuns = 0;
  ephemeral: readonly [number, number] = [32768, 60999];

  async read(path: string): Promise<string> {
    const text = this.files[path];
    if (text === undefined) throw new Error(`ENOENT: no such file or directory, open '${path}'`);
    return text;
  }

  async listeners(): Promise<string> {
    this.ssRuns++;
    return this.ss;
  }

  async ephemeralPorts(): Promise<readonly [number, number]> {
    return this.ephemeral;
  }
}
