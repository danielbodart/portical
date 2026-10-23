import { describe, expect, test } from "bun:test";
import type { DesiredForward } from "../src/reconcile.ts";
import { combine, type Desired, type Source, type SourceEvent } from "../src/source.ts";
import { Channel } from "../src/channel.ts";

function forward(owner: string, externalPort: number, protocol: "tcp" | "udp" = "tcp"): DesiredForward {
  return {
    rule: { externalPort, internalPort: externalPort, protocol },
    container: owner,
    target: { kind: "host" },
    internalClient: "192.168.1.5",
  };
}

/** A source that answers what it is told to, and emits when told to. */
class StubSource implements Source {
  stream = new Channel<SourceEvent>();
  aborted = false;
  constructor(public answer: Desired | Error) {}

  async desired(): Promise<Desired> {
    if (this.answer instanceof Error) throw this.answer;
    return this.answer;
  }

  async *events(signal: AbortSignal): AsyncIterable<SourceEvent> {
    signal.addEventListener("abort", () => {
      this.aborted = true;
      this.stream.end();
    });
    yield* this.stream;
  }
}

describe("combine", () => {
  test("is the source itself when there is only one", () => {
    const only = new StubSource({ forwards: [], warnings: [] });
    expect(combine([only])).toBe(only);
  });

  test("wants what every source wants, in the order they were named", async () => {
    const docker = new StubSource({ forwards: [forward("nginx", 80)], warnings: ["nginx: something"] });
    const systemd = new StubSource({ forwards: [forward("mc.service", 25565)], warnings: [] });
    expect(await combine([docker, systemd]).desired()).toEqual({
      forwards: [forward("nginx", 80), forward("mc.service", 25565)],
      warnings: ["nginx: something"],
    });
  });

  test("fails when any source fails, so nothing is removed on a partial answer", async () => {
    const docker = new StubSource({ forwards: [forward("nginx", 80)], warnings: [] });
    const systemd = new StubSource(new Error("D-Bus connection closed"));
    await expect(combine([docker, systemd]).desired()).rejects.toThrow("D-Bus connection closed");
  });

  test("gives a port two sources ask for to the first, and says so", async () => {
    const docker = new StubSource({ forwards: [forward("mc", 25565)], warnings: [] });
    const systemd = new StubSource({
      forwards: [forward("mc.service", 25565), forward("mc.service", 25565, "udp")],
      warnings: [],
    });
    expect(await combine([docker, systemd]).desired()).toEqual({
      forwards: [forward("mc", 25565), forward("mc.service", 25565, "udp")],
      warnings: ["mc.service and mc both ask for tcp/25565, forwarding it for mc"],
    });
  });

  test("leaves a clash within one source to the reconcile, as before", async () => {
    const docker = new StubSource({ forwards: [forward("a", 80), forward("b", 80)], warnings: [] });
    const systemd = new StubSource({ forwards: [], warnings: [] });
    expect((await combine([docker, systemd]).desired()).forwards).toHaveLength(2);
  });

  test("passes on events from every source", async () => {
    const docker = new StubSource({ forwards: [], warnings: [] });
    const systemd = new StubSource({ forwards: [], warnings: [] });
    const controller = new AbortController();
    const seen: SourceEvent[] = [];
    const reading = (async () => {
      for await (const event of combine([docker, systemd]).events(controller.signal)) {
        seen.push(event);
        if (seen.length === 2) controller.abort();
      }
    })();
    docker.stream.push({ description: "Container nginx start" });
    await Bun.sleep(1);
    systemd.stream.push({});
    await reading;
    expect(seen).toEqual([{ description: "Container nginx start" }, {}]);
    expect(docker.aborted && systemd.aborted).toBe(true);
  });

  test("throws when any stream throws, and stops the others", async () => {
    const docker = new StubSource({ forwards: [], warnings: [] });
    const systemd = new StubSource({ forwards: [], warnings: [] });
    const reading = (async () => {
      for await (const _ of combine([docker, systemd]).events(new AbortController().signal)) { /* none */ }
    })();
    systemd.stream.fail(new Error("D-Bus connection closed"));
    await expect(reading).rejects.toThrow("D-Bus connection closed");
    expect(docker.aborted).toBe(true);
  });

  test("ends when any stream ends, so the daemon reopens them all", async () => {
    const docker = new StubSource({ forwards: [], warnings: [] });
    const systemd = new StubSource({ forwards: [], warnings: [] });
    const reading = (async () => {
      for await (const _ of combine([docker, systemd]).events(new AbortController().signal)) { /* none */ }
    })();
    docker.stream.end();
    await reading;
    expect(systemd.aborted).toBe(true);
  });
});
