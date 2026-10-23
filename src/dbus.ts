import { connect, type Socket } from "node:net";

/**
 * Just enough D-Bus to ask systemd what is running and hear when it changes.
 *
 * Written by hand rather than taken from a library to keep Portical free of
 * runtime dependencies, which is what lets the Nix package be a plain
 * `bun build` with nothing to fetch. It is not general: no file descriptor
 * passing, no introspection, no serving of methods, and authentication only by
 * EXTERNAL, which is all a local system bus asks for.
 *
 * The wire format is in the D-Bus specification, "Message Protocol". Values
 * are aligned to their own size relative to the start of the message, which
 * is why the header and body are written separately but always from an offset
 * that is a multiple of eight.
 */

/** A value that has to carry its own signature on the wire. */
export class Variant {
  constructor(
    readonly signature: string,
    readonly value: unknown,
  ) {}
}

export const MessageType = {
  MethodCall: 1,
  MethodReturn: 2,
  Error: 3,
  Signal: 4,
} as const;

export type MessageType = (typeof MessageType)[keyof typeof MessageType];

export interface Message {
  readonly type: MessageType;
  readonly flags?: number;
  readonly serial: number;
  readonly path?: string;
  readonly interface?: string;
  readonly member?: string;
  readonly errorName?: string;
  readonly replySerial?: number;
  readonly destination?: string;
  readonly sender?: string;
  readonly signature?: string;
  readonly body?: readonly unknown[];
}

/** The header fields, by the codes the specification gives them. */
const FIELDS = [
  [1, "path", "o"],
  [2, "interface", "s"],
  [3, "member", "s"],
  [4, "errorName", "s"],
  [5, "replySerial", "u"],
  [6, "destination", "s"],
  [7, "sender", "s"],
  [8, "signature", "g"],
] as const;

function alignment(type: string): number {
  switch (type[0]) {
    case "y": case "g": case "v": return 1;
    case "n": case "q": return 2;
    case "b": case "i": case "u": case "h": case "s": case "o": case "a": return 4;
    case "x": case "t": case "d": case "(": case "{": return 8;
    default: throw new Error(`unsupported D-Bus type '${type}'`);
  }
}

/** Where the single complete type starting at `start` ends. */
function typeEnd(signature: string, start: number): number {
  const first = signature[start];
  if (first === undefined) throw new Error(`truncated D-Bus signature '${signature}'`);
  if (first === "a") return typeEnd(signature, start + 1);
  if (first === "(" || first === "{") {
    const close = first === "(" ? ")" : "}";
    let index = start + 1;
    while (signature[index] !== close) {
      if (index >= signature.length) throw new Error(`unbalanced D-Bus signature '${signature}'`);
      index = typeEnd(signature, index);
    }
    return index + 1;
  }
  return start + 1;
}

/** Split a signature into its complete types: `sa{sv}u` is `s`, `a{sv}`, `u`. */
export function splitSignature(signature: string): string[] {
  const types: string[] = [];
  for (let index = 0; index < signature.length; ) {
    const end = typeEnd(signature, index);
    types.push(signature.slice(index, end));
    index = end;
  }
  return types;
}

/** The types inside a struct or dict entry: `(su)` is `s`, `u`. */
function members(type: string): string[] {
  return splitSignature(type.slice(1, -1));
}

const encoder = new TextEncoder();
const decoder = new TextDecoder();

class Writer {
  private buffer = new Uint8Array(256);
  private view = new DataView(this.buffer.buffer);
  length = 0;

  private reserve(bytes: number): void {
    if (this.length + bytes <= this.buffer.length) return;
    let size = this.buffer.length * 2;
    while (size < this.length + bytes) size *= 2;
    const grown = new Uint8Array(size);
    grown.set(this.buffer.subarray(0, this.length));
    this.buffer = grown;
    this.view = new DataView(grown.buffer);
  }

  align(to: number): void {
    const padding = (to - (this.length % to)) % to;
    this.reserve(padding);
    this.buffer.fill(0, this.length, this.length + padding);
    this.length += padding;
  }

  u8(value: number): void {
    this.reserve(1);
    this.view.setUint8(this.length, value);
    this.length += 1;
  }

  u32(value: number): void {
    this.align(4);
    this.reserve(4);
    this.view.setUint32(this.length, value, true);
    this.length += 4;
  }

  patchU32(at: number, value: number): void {
    this.view.setUint32(at, value, true);
  }

  bytes(value: Uint8Array): void {
    this.reserve(value.length);
    this.buffer.set(value, this.length);
    this.length += value.length;
  }

  value(type: string, value: unknown): void {
    switch (type[0]) {
      case "y": return this.u8(Number(value));
      case "b": return this.u32(value ? 1 : 0);
      case "n": case "q": {
        this.align(2);
        this.reserve(2);
        if (type === "n") this.view.setInt16(this.length, Number(value), true);
        else this.view.setUint16(this.length, Number(value), true);
        this.length += 2;
        return;
      }
      case "i": {
        this.align(4);
        this.reserve(4);
        this.view.setInt32(this.length, Number(value), true);
        this.length += 4;
        return;
      }
      case "u": case "h": return this.u32(Number(value));
      case "x": case "t": {
        this.align(8);
        this.reserve(8);
        if (type === "x") this.view.setBigInt64(this.length, BigInt(value as bigint), true);
        else this.view.setBigUint64(this.length, BigInt(value as bigint), true);
        this.length += 8;
        return;
      }
      case "d": {
        this.align(8);
        this.reserve(8);
        this.view.setFloat64(this.length, Number(value), true);
        this.length += 8;
        return;
      }
      case "s": case "o": {
        const text = encoder.encode(String(value));
        this.u32(text.length);
        this.bytes(text);
        return this.u8(0);
      }
      case "g": {
        const text = encoder.encode(String(value));
        this.u8(text.length);
        this.bytes(text);
        return this.u8(0);
      }
      case "v": {
        if (!(value instanceof Variant)) throw new Error("a D-Bus variant needs a Variant to write");
        this.value("g", value.signature);
        return this.value(value.signature, value.value);
      }
      case "(": {
        this.align(8);
        const fields = members(type);
        const values = value as readonly unknown[];
        fields.forEach((field, index) => this.value(field, values[index]));
        return;
      }
      case "a": {
        const element = type.slice(1);
        this.u32(0);
        const lengthAt = this.length - 4;
        // The length excludes the padding to the first element, which is
        // there even when the array is empty.
        this.align(alignment(element));
        const start = this.length;
        if (element[0] === "{") {
          const [key, item] = members(element) as [string, string];
          const entries = value instanceof Map ? [...value] : Object.entries(value as object);
          for (const [k, v] of entries) {
            this.align(8);
            this.value(key, k);
            this.value(item, v);
          }
        } else {
          for (const item of value as readonly unknown[]) this.value(element, item);
        }
        this.patchU32(lengthAt, this.length - start);
        return;
      }
      default:
        throw new Error(`unsupported D-Bus type '${type}'`);
    }
  }

  done(): Uint8Array {
    return this.buffer.slice(0, this.length);
  }
}

class Reader {
  private readonly view: DataView;
  constructor(
    private readonly buffer: Uint8Array,
    public offset: number,
    private readonly little: boolean,
  ) {
    this.view = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);
  }

  align(to: number): void {
    this.offset += (to - (this.offset % to)) % to;
  }

  private check(bytes: number): void {
    if (this.offset + bytes > this.buffer.length) throw new Error("truncated D-Bus message");
  }

  u8(): number {
    this.check(1);
    return this.view.getUint8(this.offset++);
  }

  u32(): number {
    this.align(4);
    this.check(4);
    const value = this.view.getUint32(this.offset, this.little);
    this.offset += 4;
    return value;
  }

  value(type: string): unknown {
    switch (type[0]) {
      case "y": return this.u8();
      case "b": return this.u32() !== 0;
      case "n": case "q": {
        this.align(2);
        this.check(2);
        const value = type === "n"
          ? this.view.getInt16(this.offset, this.little)
          : this.view.getUint16(this.offset, this.little);
        this.offset += 2;
        return value;
      }
      case "i": {
        this.align(4);
        this.check(4);
        const value = this.view.getInt32(this.offset, this.little);
        this.offset += 4;
        return value;
      }
      case "u": case "h": return this.u32();
      case "x": case "t": {
        this.align(8);
        this.check(8);
        const value = type === "x"
          ? this.view.getBigInt64(this.offset, this.little)
          : this.view.getBigUint64(this.offset, this.little);
        this.offset += 8;
        return value;
      }
      case "d": {
        this.align(8);
        this.check(8);
        const value = this.view.getFloat64(this.offset, this.little);
        this.offset += 8;
        return value;
      }
      case "s": case "o": {
        const length = this.u32();
        this.check(length + 1);
        const text = decoder.decode(this.buffer.subarray(this.offset, this.offset + length));
        this.offset += length + 1;
        return text;
      }
      case "g": {
        const length = this.u8();
        this.check(length + 1);
        const text = decoder.decode(this.buffer.subarray(this.offset, this.offset + length));
        this.offset += length + 1;
        return text;
      }
      // Unwrapped: what is read here is only ever looked at, never written
      // back, so carrying the signature along would only get in the way.
      case "v": return this.value(this.value("g") as string);
      case "(": {
        this.align(8);
        return members(type).map((field) => this.value(field));
      }
      case "a": {
        const element = type.slice(1);
        const length = this.u32();
        this.align(alignment(element));
        const end = this.offset + length;
        this.check(length);
        if (element[0] === "{") {
          const [key, item] = members(element) as [string, string];
          const entries: Record<string, unknown> = {};
          while (this.offset < end) {
            this.align(8);
            const k = this.value(key);
            entries[String(k)] = this.value(item);
          }
          return entries;
        }
        const items: unknown[] = [];
        while (this.offset < end) items.push(this.value(element));
        return items;
      }
      default:
        throw new Error(`unsupported D-Bus type '${type}'`);
    }
  }
}

/** One message as it goes on the wire. Always little-endian. */
export function encode(message: Message): Uint8Array {
  const signature = message.signature ?? "";
  const body = new Writer();
  splitSignature(signature).forEach((type, index) => body.value(type, message.body?.[index]));

  const header = new Writer();
  header.u8("l".charCodeAt(0));
  header.u8(message.type);
  header.u8(message.flags ?? 0);
  header.u8(1);
  header.u32(body.length);
  header.u32(message.serial);
  const fields: [number, Variant][] = [];
  for (const [code, name, type] of FIELDS) {
    const value = name === "signature" ? (signature === "" ? undefined : signature) : message[name];
    if (value !== undefined) fields.push([code, new Variant(type, value)]);
  }
  header.value("a(yv)", fields);
  header.align(8);
  header.bytes(body.done());
  return header.done();
}

/**
 * The first whole message in `buffer`, and how many bytes it took.
 *
 * Undefined while the message is still arriving, so a stream reader can keep
 * appending until there is enough.
 */
export function decode(buffer: Uint8Array): { message: Message; length: number } | undefined {
  if (buffer.length < 16) return undefined;
  const little = buffer[0] === "l".charCodeAt(0);
  if (!little && buffer[0] !== "B".charCodeAt(0)) throw new Error("not a D-Bus message");

  const view = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);
  const bodyLength = view.getUint32(4, little);
  const fieldsLength = view.getUint32(12, little);
  const headerLength = 16 + fieldsLength + ((8 - (fieldsLength % 8)) % 8);
  const length = headerLength + bodyLength;
  if (buffer.length < length) return undefined;

  const reader = new Reader(buffer.subarray(0, length), 12, little);
  const fields = reader.value("a(yv)") as [number, unknown][];
  const message: Record<string, unknown> = {
    type: buffer[1],
    flags: buffer[2],
    serial: view.getUint32(8, little),
  };
  for (const [code, value] of fields) {
    const known = FIELDS.find(([c]) => c === code);
    if (known) message[known[1]] = value;
  }

  reader.offset = headerLength;
  const signature = (message.signature as string | undefined) ?? "";
  message.body = splitSignature(signature).map((type) => reader.value(type));
  return { message: message as unknown as Message, length };
}

/** An error the far end replied with, as opposed to the connection failing. */
export class DbusError extends Error {
  constructor(
    readonly errorName: string,
    message: string,
  ) {
    super(`${errorName}: ${message}`);
    this.name = "DbusError";
  }
}

export const SYSTEM_BUS = "/run/dbus/system_bus_socket";

/**
 * The system bus socket, from the same variable libdbus and sd-bus read.
 *
 * Only `unix:path=` is understood, which is all a system bus has ever been.
 */
export function systemBusPath(env: Record<string, string | undefined> = {}): string {
  const address = env.DBUS_SYSTEM_BUS_ADDRESS;
  if (!address) return SYSTEM_BUS;
  for (const candidate of address.split(";")) {
    const path = /^unix:(?:.*,)?path=([^,]+)/.exec(candidate)?.[1];
    if (path) return decodeURIComponent(path);
  }
  throw new Error(`cannot use DBUS_SYSTEM_BUS_ADDRESS '${address}': only unix:path= is supported`);
}

interface Pending {
  resolve(body: readonly unknown[]): void;
  reject(error: Error): void;
}

/**
 * A connection to a bus, authenticated and introduced.
 *
 * Method calls may overlap: each waits on its own reply serial, so a pass can
 * ask about every unit at once rather than one round trip at a time. Signals
 * go to whoever is listening, and are dropped when nobody is.
 */
export class DbusConnection {
  private serial = 0;
  private readonly pending = new Map<number, Pending>();
  private buffer: Uint8Array = new Uint8Array(0);
  private failure?: Error;
  private readonly listeners = new Set<(message: Message) => void>();
  private readonly closers = new Set<(error: Error) => void>();

  private constructor(private readonly socket: Socket) {
    socket.on("data", (chunk: Buffer) => this.receive(chunk));
    socket.on("error", (error) => this.fail(error));
    socket.on("close", () => this.fail(new Error("D-Bus connection closed")));
  }

  /** Connect, authenticate as our own uid, and say Hello. */
  static async open(path: string, uid = process.getuid?.() ?? 0): Promise<DbusConnection> {
    const socket = connect(path);
    try {
      await authenticate(socket, uid);
    } catch (error) {
      socket.destroy();
      throw new Error(`D-Bus at ${path}: ${(error as Error).message}`, { cause: error });
    }
    const connection = new DbusConnection(socket);
    await connection.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello");
    return connection;
  }

  call(
    destination: string,
    path: string,
    iface: string,
    member: string,
    signature = "",
    body: readonly unknown[] = [],
  ): Promise<readonly unknown[]> {
    if (this.failure) return Promise.reject(this.failure);
    const serial = ++this.serial;
    return new Promise((resolve, reject) => {
      this.pending.set(serial, { resolve, reject });
      this.socket.write(
        encode({
          type: MessageType.MethodCall,
          serial,
          destination,
          path,
          interface: iface,
          member,
          signature,
          body,
        }),
      );
    });
  }

  /** Every signal from now on. Returns the function that stops listening. */
  onSignal(listener: (message: Message) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /** Called once if the connection fails or closes. Returns a remover. */
  onClose(listener: (error: Error) => void): () => void {
    if (this.failure) {
      listener(this.failure);
      return () => {};
    }
    this.closers.add(listener);
    return () => this.closers.delete(listener);
  }

  get closed(): boolean {
    return this.failure !== undefined;
  }

  close(): void {
    this.socket.destroy();
    this.fail(new Error("D-Bus connection closed"));
  }

  private receive(chunk: Uint8Array): void {
    const joined = new Uint8Array(this.buffer.length + chunk.length);
    joined.set(this.buffer);
    joined.set(chunk, this.buffer.length);
    this.buffer = joined;

    try {
      let decoded: ReturnType<typeof decode>;
      while ((decoded = decode(this.buffer))) {
        this.buffer = this.buffer.slice(decoded.length);
        this.dispatch(decoded.message);
      }
    } catch (error) {
      // A message we cannot frame leaves no way to find the next one.
      this.socket.destroy();
      this.fail(error as Error);
    }
  }

  private dispatch(message: Message): void {
    if (message.type === MessageType.Signal) {
      for (const listener of this.listeners) listener(message);
      return;
    }
    if (message.replySerial === undefined) return;
    const waiting = this.pending.get(message.replySerial);
    if (!waiting) return;
    this.pending.delete(message.replySerial);
    if (message.type === MessageType.Error) {
      waiting.reject(new DbusError(message.errorName ?? "unknown", String(message.body?.[0] ?? "")));
    } else {
      waiting.resolve(message.body ?? []);
    }
  }

  private fail(error: Error): void {
    if (this.failure) return;
    this.failure = error;
    for (const waiting of this.pending.values()) waiting.reject(error);
    this.pending.clear();
    for (const closer of this.closers) closer(error);
    this.closers.clear();
  }
}

/**
 * The SASL exchange that opens every D-Bus connection.
 *
 * EXTERNAL means "believe what the kernel says about who I am": the bus reads
 * our uid off the socket, and we only have to claim the same one, written as
 * the hex of its decimal digits.
 */
function authenticate(socket: Socket, uid: number): Promise<void> {
  const hex = Buffer.from(String(uid)).toString("hex");
  return new Promise((resolve, reject) => {
    let received = "";
    const cleanup = () => {
      socket.off("data", onData);
      socket.off("error", onError);
      socket.off("close", onClose);
    };
    const onError = (error: Error) => { cleanup(); reject(error); };
    const onClose = () => { cleanup(); reject(new Error("closed during authentication")); };
    const onData = (chunk: Buffer) => {
      received += chunk.toString("latin1");
      const end = received.indexOf("\r\n");
      if (end === -1) return;
      cleanup();
      const line = received.slice(0, end);
      if (!line.startsWith("OK ")) {
        reject(new Error(`authentication refused: ${line}`));
        return;
      }
      socket.write("BEGIN\r\n");
      resolve();
    };
    socket.on("data", onData);
    socket.once("error", onError);
    socket.once("close", onClose);
    socket.write(`\0AUTH EXTERNAL ${hex}\r\n`);
  });
}
