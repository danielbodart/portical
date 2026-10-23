import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { decode, DbusError, encode, MessageType, type Message } from "../../src/dbus.ts";

export type Reply = { signature?: string; body?: unknown[] } | DbusError;

/**
 * A message bus on a Unix socket in a temporary directory.
 *
 * Real enough that the client cannot tell: the SASL handshake, framed
 * messages both ways, replies matched by serial. Method calls are answered by
 * `answer`, and every call is recorded, so a test can say both what systemd
 * replied and what it was asked.
 */
export class FakeBus {
  readonly path: string;
  readonly calls: Message[] = [];
  /** The line received after the NUL byte, such as `AUTH EXTERNAL 31303030`. */
  auth?: string;
  answer: (call: Message) => Reply | Promise<Reply> = () => ({});
  /**
   * Set to answer calls concurrently and refuse any that would leave more
   * than this many waiting, as dbus-daemon's max_replies_per_connection does.
   */
  maxPending?: number;
  /** The most calls that were waiting on an answer at once. */
  mostPending = 0;
  private pending = 0;
  private readonly server: Server;
  private readonly directory: string;
  private readonly clients = new Set<Socket>();
  private serial = 1000;

  private constructor() {
    this.directory = mkdtempSync(join(tmpdir(), "portical-bus-"));
    this.path = join(this.directory, "bus");
    this.server = createServer((socket) => this.accept(socket));
  }

  static async start(): Promise<FakeBus> {
    const bus = new FakeBus();
    await new Promise<void>((resolve) => bus.server.listen(bus.path, resolve));
    return bus;
  }

  /** Send a signal to every connected client. */
  signal(message: Omit<Message, "type" | "serial">): void {
    const bytes = encode({ ...message, type: MessageType.Signal, serial: ++this.serial });
    for (const client of this.clients) client.write(bytes);
  }

  /** Drop every client, as a bus restart would. */
  disconnect(): void {
    for (const client of this.clients) client.destroy();
    this.clients.clear();
  }

  get connected(): number {
    return this.clients.size;
  }

  async stop(): Promise<void> {
    this.disconnect();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
    rmSync(this.directory, { recursive: true, force: true });
  }

  private accept(socket: Socket): void {
    let authenticated = false;
    let answered = false;
    let text = "";
    let buffer: Uint8Array = new Uint8Array(0);

    socket.on("error", () => {});
    socket.on("close", () => this.clients.delete(socket));
    socket.on("data", async (chunk: Buffer) => {
      if (!authenticated) {
        text += chunk.toString("latin1");
        if (!text.startsWith("\0")) return;
        const auth = text.indexOf("\r\n");
        if (auth !== -1 && !answered) {
          answered = true;
          this.auth = text.slice(1, auth);
          socket.write("OK 0123456789abcdef0123456789abcdef\r\n");
        }
        const begin = text.indexOf("BEGIN\r\n");
        if (begin === -1) return;
        authenticated = true;
        this.clients.add(socket);
        chunk = Buffer.from(text.slice(begin + "BEGIN\r\n".length), "latin1");
      }

      const joined = new Uint8Array(buffer.length + chunk.length);
      joined.set(buffer);
      joined.set(chunk, buffer.length);
      buffer = joined;

      let decoded: ReturnType<typeof decode>;
      while ((decoded = decode(buffer))) {
        buffer = buffer.slice(decoded.length);
        if (this.maxPending === undefined) await this.respond(socket, decoded.message);
        else void this.respond(socket, decoded.message);
      }
    });
  }

  private async respond(socket: Socket, call: Message): Promise<void> {
    this.calls.push(call);
    let reply: Reply;
    if (this.maxPending !== undefined && this.pending >= this.maxPending) {
      reply = new DbusError(
        "org.freedesktop.DBus.Error.LimitsExceeded",
        "The maximum number of pending replies per connection has been reached",
      );
    } else {
      this.pending++;
      this.mostPending = Math.max(this.mostPending, this.pending);
      try {
        reply = call.member === "Hello" ? { signature: "s", body: [":1.42"] } : await this.answer(call);
      } finally {
        this.pending--;
      }
    }
    const common = { serial: ++this.serial, replySerial: call.serial, destination: ":1.42" };
    socket.write(
      encode(
        reply instanceof DbusError
          ? {
              ...common,
              type: MessageType.Error,
              errorName: reply.errorName,
              signature: "s",
              body: [reply.message.replace(`${reply.errorName}: `, "")],
            }
          : { ...common, type: MessageType.MethodReturn, signature: reply.signature, body: reply.body },
      ),
    );
  }
}
