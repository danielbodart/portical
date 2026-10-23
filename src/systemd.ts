import { readFile, stat } from "node:fs/promises";
import { Channel } from "./channel.ts";
import { DbusConnection, MessageType, SYSTEM_BUS, type Message } from "./dbus.ts";
import type { Protocol, Rule } from "./model.ts";
import type { DesiredForward } from "./reconcile.ts";
import { parseLabel } from "./rules.ts";
import type { Desired, Source, SourceEvent } from "./source.ts";

/** The unit file key a forward is declared with, the counterpart of the label. */
export const FORWARD_KEY = "X-Portical-Forward";

/** An active unit, and where its configuration came from. */
export interface Unit {
  readonly name: string;
  /** The unit file itself. Empty for units with none, such as devices. */
  readonly fragmentPath: string;
  /** Drop-ins, in the order systemd applied them. */
  readonly dropInPaths: readonly string[];
  /** The units that start this one, `.socket` units for socket activation. */
  readonly triggeredBy: readonly string[];
  /** The files on disk have changed since systemd last loaded them. */
  readonly needDaemonReload: boolean;
}

/**
 * Something about the units changed.
 *
 * `unit` and `state` are set when a unit's ActiveState changed, and both are
 * absent when systemd finished reloading its configuration.
 */
export interface SystemdEvent {
  readonly unit?: string;
  readonly state?: string;
  /**
   * The state this unit was last seen in on this stream. Absent the first
   * time, when all that is known is where it is now, not that it moved.
   */
  readonly previous?: string;
}

/**
 * The systemd operations Portical needs, behind an interface for the same
 * reason DockerClient is: so a fake can stand in for the whole of systemd.
 */
export interface SystemdClient {
  /** Every unit that is active, reloading or refreshing - running, in other words. */
  units(): Promise<Unit[]>;
  /** The unit's cgroup, such as `/system.slice/sshd.service`, if it has one. */
  controlGroup(unit: string): Promise<string | undefined>;
  events(signal: AbortSignal): AsyncIterable<SystemdEvent>;
}

/** What the systemd source needs from the machine, besides systemd itself. */
export interface Host {
  /** A unit file's text. */
  read(path: string): Promise<string>;
  /** The listening sockets, as `ss -Hlntu -e` prints them. */
  listeners(): Promise<string>;
  /** The kernel's ephemeral port range, inclusive. */
  ephemeralPorts(): Promise<readonly [number, number]>;
}

export interface SystemdOptions {
  readonly key?: string;
  /** This host's address on the LAN, where every unit's forward points. */
  readonly hostAddress?: string;
}

/**
 * Active systemd units as a Source: the forwards their unit files declare.
 *
 * ```ini
 * [Unit]
 * X-Portical-Forward=25565,19133:19132/udp
 * ```
 *
 * systemd ignores any key starting `X-`, so the declaration does nothing on a
 * host without Portical. It cannot be read back over D-Bus - systemd keeps
 * unknown keys to itself - so the files systemd names for each unit are read
 * directly.
 */
export class SystemdSource implements Source {
  /**
   * Every unit that has declared forwards this run, so their events are
   * logged and everyone else's are not. Every unit's events still ask for a
   * pass, because any unit can start declaring one.
   *
   * Never shrinks. A unit that stops drops out of the very pass its first
   * event triggers, so by the time its stream reaches "inactive" it would no
   * longer be declaring anything, and the line that says why its forward
   * went would be the one left out.
   */
  private readonly declaring = new Set<string>();

  constructor(
    private readonly systemd: SystemdClient,
    private readonly options: SystemdOptions = {},
    private readonly host: Host = machine(),
  ) {}

  async desired(): Promise<Desired> {
    const key = this.options.key ?? FORWARD_KEY;
    const forwards: DesiredForward[] = [];
    const warnings: string[] = [];

    const units = await this.systemd.units();
    const declarations = await Promise.all(
      units.map(async (unit) => ({ unit, declared: declaration(unit, await this.files(unit), key) })),
    );

    const wanted: { unit: Unit; rules: readonly Rule[]; published: boolean }[] = [];
    for (const { unit, declared } of declarations) {
      for (const warning of declared.warnings) warnings.push(`${unit.name}: ${warning}`);
      if (declared.value === "") continue;

      this.declaring.add(unit.name);
      const terms = parseLabel(declared.value);
      for (const error of terms.errors) warnings.push(`${unit.name}: ${error}`);
      if (unit.needDaemonReload) {
        warnings.push(
          `${unit.name}: its unit files have changed since systemd loaded them - ` +
            `forwarding what is on disk now. Run 'systemctl daemon-reload'.`,
        );
      }
      wanted.push({ unit, rules: terms.rules, published: terms.published });
    }

    const sockets = wanted.some(({ published }) => published) ? await this.sockets(warnings) : undefined;

    for (const { unit, rules, published } of wanted) {
      const listening = published && sockets ? await this.published(unit, sockets) : [];
      // Explicit rules first, so they win any collision with a listening
      // port, as they do for a container.
      for (const rule of collapse([...rules, ...listening])) {
        forwards.push({
          rule,
          container: unit.name,
          target: { kind: "host" },
          internalClient: this.options.hostAddress,
        });
      }
    }

    return { forwards, warnings };
  }

  async *events(signal: AbortSignal): AsyncIterable<SourceEvent> {
    for await (const event of this.systemd.events(signal)) {
      // Only where a unit settles, not each step on the way there, and only
      // once it is known to have moved there.
      const moved = event.previous !== undefined && event.state !== undefined && SETTLED.has(event.state);
      yield event.unit && moved && this.declaring.has(event.unit)
        ? { description: `Unit ${event.unit} ${event.state}` }
        : {};
    }
  }

  /** The unit file and its drop-ins, in the order systemd applies them. */
  private files(unit: Unit): Promise<UnitFile[]> {
    const paths = [unit.fragmentPath, ...unit.dropInPaths].filter((path) => path !== "");
    return Promise.all(paths.map(async (path) => ({ path, text: await this.host.read(path) })));
  }

  private async sockets(warnings: string[]): Promise<Sockets> {
    const [text, ephemeral] = await Promise.all([this.host.listeners(), this.host.ephemeralPorts()]);
    const listeners = parseListeners(text);
    if (listeners.length > 0 && listeners.every((listener) => listener.cgroup === undefined)) {
      warnings.push(
        "ss reports no cgroup for any socket, so 'published' cannot tell which unit " +
          "listens where and forwards nothing. It needs Linux 5.7 and iproute2 5.9 or later.",
      );
    }
    return { listeners, ephemeral };
  }

  /** The rules `published` stands for: what the unit is listening on. */
  private async published(unit: Unit, sockets: Sockets): Promise<Rule[]> {
    const groups = (
      await Promise.all([unit.name, ...unit.triggeredBy].map((name) => this.systemd.controlGroup(name)))
    ).filter((group): group is string => group !== undefined && group !== "");

    return listening(sockets, groups, this.options.hostAddress);
  }
}

const SETTLED = new Set(["active", "inactive", "failed"]);

interface UnitFile {
  readonly path: string;
  readonly text: string;
}

interface Sockets {
  readonly listeners: readonly Listener[];
  readonly ephemeral: readonly [number, number];
}

/** The section a unit type keeps its own settings in, by unit suffix. */
const TYPE_SECTIONS: Readonly<Record<string, string>> = {
  service: "Service",
  socket: "Socket",
  mount: "Mount",
  automount: "Automount",
  swap: "Swap",
  path: "Path",
  timer: "Timer",
  slice: "Slice",
  scope: "Scope",
};

function typeSection(unit: string): string | undefined {
  return TYPE_SECTIONS[unit.slice(unit.lastIndexOf(".") + 1)];
}

/**
 * The key's value once every file has been applied, as one label value.
 *
 * Read as systemd reads a list setting such as `Environment=`: each
 * assignment adds to what came before, across the unit file and every
 * drop-in, and an empty assignment throws away everything before it. So a
 * drop-in can add a forward to a packaged unit, or turn its forwards off with
 * a bare `X-Portical-Forward=`.
 *
 * Empty when nothing is declared, or everything declared was reset.
 */
export function declaration(
  unit: Pick<Unit, "name">,
  files: readonly UnitFile[],
  key: string = FORWARD_KEY,
): { value: string; warnings: string[] } {
  const own = typeSection(unit.name);
  const warnings: string[] = [];
  let terms: string[] = [];

  for (const file of files) {
    for (const assignment of assignments(file.text, key)) {
      if (assignment.section !== "Unit" && assignment.section !== own) {
        warnings.push(
          `ignoring ${key} in [${assignment.section ?? ""}] at ${file.path}:${assignment.line} - ` +
            `put it in [Unit]${own ? ` or [${own}]` : ""}`,
        );
        continue;
      }
      if (assignment.value === "") terms = [];
      else terms.push(assignment.value);
    }
  }

  return { value: terms.join(","), warnings };
}

interface Assignment {
  readonly section?: string;
  readonly line: number;
  readonly value: string;
}

/**
 * Every assignment of `key` in a unit file, top to bottom.
 *
 * As much of systemd's syntax as this needs: sections, `#` and `;`
 * comments, and a trailing backslash continuing the line - joined with a
 * space, skipping comment lines inside the continuation, as systemd does.
 * Specifiers such as `%i` are not expanded; one in a value is reported by the
 * rule parser as an unrecognised rule.
 */
export function assignments(text: string, key: string): Assignment[] {
  const found: Assignment[] = [];
  const lines = text.split(/\r?\n/);
  let section: string | undefined;

  for (let index = 0; index < lines.length; index++) {
    const start = index + 1;
    let line = lines[index]!.trim();
    if (line === "" || line.startsWith("#") || line.startsWith(";")) continue;

    while (line.endsWith("\\") && index + 1 < lines.length) {
      line = line.slice(0, -1);
      const next = lines[++index]!.trim();
      if (next.startsWith("#") || next.startsWith(";")) {
        // A comment inside a continuation is dropped, and the line before it
        // still continues.
        line += "\\";
        continue;
      }
      line += ` ${next}`;
    }
    if (line.endsWith("\\")) line = line.slice(0, -1);

    if (line.startsWith("[") && line.endsWith("]")) {
      section = line.slice(1, -1).trim();
      continue;
    }

    const equals = line.indexOf("=");
    if (equals === -1) continue;
    if (line.slice(0, equals).trim() !== key) continue;
    found.push({ section, line: start, value: line.slice(equals + 1).trim() });
  }

  return found;
}

/** A listening socket, from one line of `ss -Hlntu -e`. */
export interface Listener {
  readonly protocol: Protocol;
  /** Without brackets, and with any `%interface` scope kept on the end. */
  readonly address: string;
  readonly port: number;
  /** The cgroup of whoever created the socket. */
  readonly cgroup?: string;
  /** An IPv6 socket that does not also take IPv4. */
  readonly v6only: boolean;
}

/** Read `ss -Hlntu -e`. Lines that are not sockets are skipped. */
export function parseListeners(text: string): Listener[] {
  const listeners: Listener[] = [];

  for (const line of text.split("\n")) {
    const [netid, , , , local, , ...extras] = line.trim().split(/\s+/);
    if ((netid !== "tcp" && netid !== "udp") || local === undefined) continue;

    const colon = local.lastIndexOf(":");
    const port = Number(local.slice(colon + 1));
    if (colon === -1 || !Number.isInteger(port)) continue;

    const cgroup = extras.find((extra) => extra.startsWith("cgroup:"))?.slice("cgroup:".length);
    listeners.push({
      protocol: netid,
      address: local.slice(0, colon).replace(/^\[(.*)\]/, "$1"),
      port,
      cgroup,
      v6only: extras.includes("v6only:1"),
    });
  }

  return listeners;
}

/**
 * Whether the gateway's traffic could reach this socket.
 *
 * Wildcards, and the host's own address. An IPv6 wildcard counts unless the
 * socket is v6only, because otherwise it takes IPv4 as well, which is
 * Linux's default. Loopback, other specific addresses and anything scoped to
 * an interface (`127.0.0.53%lo`) cannot be what a forward is for.
 */
function reachable(listener: Listener, hostAddress: string | undefined): boolean {
  const { address } = listener;
  if (address.includes("%")) return false;
  if (address === "0.0.0.0" || address === "*") return true;
  if (address === "::") return !listener.v6only;
  return hostAddress !== undefined && (address === hostAddress || address === `::ffff:${hostAddress}`);
}

/**
 * The rules for the sockets in any of these cgroups, or beneath one.
 *
 * Beneath, because a service may put its workers in sub-cgroups of its own.
 * UDP sockets on ephemeral ports are left out: `-l` lists every unconnected
 * UDP socket, and a service's client sockets - a resolver, a game server
 * registering with a master list - sit there and must not be forwarded.
 */
export function listening(
  sockets: { readonly listeners: readonly Listener[]; readonly ephemeral: readonly [number, number] },
  groups: readonly string[],
  hostAddress: string | undefined,
): Rule[] {
  const [low, high] = sockets.ephemeral;
  const rules: Rule[] = [];

  for (const listener of sockets.listeners) {
    const { cgroup } = listener;
    if (cgroup === undefined) continue;
    if (!groups.some((group) => cgroup === group || cgroup.startsWith(`${group}/`))) continue;
    if (!reachable(listener, hostAddress)) continue;
    if (listener.protocol === "udp" && listener.port >= low && listener.port <= high) continue;
    rules.push({ externalPort: listener.port, internalPort: listener.port, protocol: listener.protocol });
  }

  return collapse(rules);
}

/** Drop later rules that would fight an earlier one for the same gateway slot. */
function collapse(rules: readonly Rule[]): Rule[] {
  const byIdentity = new Map<string, Rule>();
  for (const rule of rules) {
    const key = `${rule.protocol}/${rule.externalPort}`;
    if (!byIdentity.has(key)) byIdentity.set(key, rule);
  }
  return [...byIdentity.values()];
}

/**
 * The real machine.
 *
 * Unit files are cached by inode, size and modification time. A pass reads
 * every active unit's files, and almost none of them change between passes,
 * so this keeps a pass to a stat per file.
 */
export function machine(): Host {
  const cache = new Map<string, { stamp: string; text: string }>();

  return {
    async read(path) {
      let info;
      try {
        info = await stat(path);
      } catch (error) {
        // systemd still names a file deleted since it was loaded, until the
        // next daemon-reload. What is on disk now is nothing, so it declares
        // nothing - failing instead would fail every pass, for every source,
        // until someone reloads. Any other error still fails the pass.
        if (missing(error)) {
          cache.delete(path);
          return "";
        }
        throw error;
      }
      const stamp = `${info.ino}/${info.size}/${info.mtimeMs}`;
      const cached = cache.get(path);
      if (cached?.stamp === stamp) return cached.text;
      let text: string;
      try {
        text = await readFile(path, "utf8");
      } catch (error) {
        // Deleted between the stat and the read.
        if (missing(error)) return "";
        throw error;
      }
      cache.set(path, { stamp, text });
      return text;
    },

    async listeners() {
      const ss = Bun.spawn(["ss", "-Hlntu", "-e"], { stdout: "pipe", stderr: "pipe" });
      const [out, err, code] = await Promise.all([
        new Response(ss.stdout).text(),
        new Response(ss.stderr).text(),
        ss.exited,
      ]);
      if (code !== 0) throw new Error(`ss exited with status ${code}: ${err.trim()}`);
      return out;
    },

    async ephemeralPorts() {
      try {
        const [low, high] = (await readFile("/proc/sys/net/ipv4/ip_local_port_range", "utf8"))
          .trim()
          .split(/\s+/)
          .map(Number);
        if (low && high) return [low, high];
      } catch {
        // Fall through to the kernel's default.
      }
      return [32768, 60999];
    },
  };
}

function missing(error: unknown): boolean {
  const code = (error as NodeJS.ErrnoException).code;
  return code === "ENOENT" || code === "ENOTDIR";
}

const SYSTEMD = "org.freedesktop.systemd1";
const MANAGER_PATH = "/org/freedesktop/systemd1";
const MANAGER = "org.freedesktop.systemd1.Manager";
const UNIT = "org.freedesktop.systemd1.Unit";
const PROPERTIES = "org.freedesktop.DBus.Properties";
const UNIT_PATHS = "/org/freedesktop/systemd1/unit";

/** The unit types that have a ControlGroup, and the interface that carries it. */
const CGROUP_INTERFACES: Readonly<Record<string, string>> = {
  service: "Service",
  socket: "Socket",
  mount: "Mount",
  swap: "Swap",
  slice: "Slice",
  scope: "Scope",
};

/**
 * How many calls one connection has waiting on a reply at once: well under
 * dbus-daemon's default max_replies_per_connection of 128, leaving room for
 * anything else sharing the connection. dbus-broker has no such limit.
 */
export const IN_FLIGHT = 32;

/**
 * Every item through `map`, in order, with at most `limit` running at once.
 * Fails as soon as one does, like Promise.all.
 */
export async function limited<T, R>(
  items: readonly T[],
  limit: number,
  map: (item: T) => Promise<R>,
): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const index = next++;
      results[index] = await map(items[index]!);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return results;
}

/**
 * systemd over the system bus.
 *
 * Queries share one connection, opened when first needed and again after it
 * drops. The event stream has a connection of its own, because a
 * subscription belongs to a connection and ends with it - which is also how
 * it is cancelled.
 */
export class DbusSystemdClient implements SystemdClient {
  private connection?: Promise<DbusConnection>;

  constructor(private readonly path: string = SYSTEM_BUS) {}

  private bus(): Promise<DbusConnection> {
    if (!this.connection) {
      const opening = DbusConnection.open(this.path);
      this.connection = opening;
      opening.then(
        (connection) => connection.onClose(() => {
          if (this.connection === opening) this.connection = undefined;
        }),
        () => {
          if (this.connection === opening) this.connection = undefined;
        },
      );
    }
    return this.connection;
  }

  async units(): Promise<Unit[]> {
    const bus = await this.bus();
    const [listed] = (await bus.call(SYSTEMD, MANAGER_PATH, MANAGER, "ListUnitsByPatterns", "asas", [
      ["active", "reloading", "refreshing"],
      [],
    ])) as [[string, string, string, string, string, string, string][]];

    // Overlapping, as the connection matches replies by serial, but only so
    // many at a time: dbus-daemon refuses a connection more than 128 pending
    // replies by default, and a host easily has more active units than that.
    return limited(
      listed,
      IN_FLIGHT,
      async ([name, , , , , , path]) => {
        const [properties] = (await bus.call(SYSTEMD, path, PROPERTIES, "GetAll", "s", [UNIT])) as [
          Record<string, unknown>,
        ];
        return {
          name,
          fragmentPath: String(properties.FragmentPath ?? ""),
          dropInPaths: (properties.DropInPaths as string[] | undefined) ?? [],
          triggeredBy: (properties.TriggeredBy as string[] | undefined) ?? [],
          needDaemonReload: properties.NeedDaemonReload === true,
        };
      },
    );
  }

  async controlGroup(unit: string): Promise<string | undefined> {
    const type = CGROUP_INTERFACES[unit.slice(unit.lastIndexOf(".") + 1)];
    if (!type) return undefined;
    const bus = await this.bus();
    const [group] = await bus.call(SYSTEMD, unitPath(unit), PROPERTIES, "Get", "ss", [
      `${SYSTEMD}.${type}`,
      "ControlGroup",
    ]);
    return typeof group === "string" && group !== "" ? group : undefined;
  }

  async *events(signal: AbortSignal): AsyncIterable<SystemdEvent> {
    const bus = await DbusConnection.open(this.path);
    const events = new Channel<SystemdEvent>();
    const stop = () => {
      events.end();
      bus.close();
    };
    signal.addEventListener("abort", stop);
    bus.onClose((error) => events.fail(error));
    // systemd sends ActiveState with every change to a unit's properties,
    // whether or not it moved, so only a state that differs from the last one
    // seen for that unit is passed on.
    const states = new Map<string, string | undefined>();
    bus.onSignal((message) => {
      const event = toEvent(message);
      if (!event) return;
      if (event.unit === undefined) return events.push(event);
      const seen = states.has(event.unit);
      const previous = states.get(event.unit);
      if (seen && previous === event.state) return;
      states.set(event.unit, event.state);
      events.push(seen ? { ...event, previous } : event);
    });

    try {
      if (signal.aborted) return;
      for (const rule of MATCHES) {
        await bus.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "AddMatch", "s", [
          rule,
        ]);
      }
      // Without this systemd sends no unit signals at all. Any user may ask,
      // and it lasts as long as this connection does.
      await bus.call(SYSTEMD, MANAGER_PATH, MANAGER, "Subscribe");
      yield* events;
    } finally {
      signal.removeEventListener("abort", stop);
      bus.close();
    }
  }
}

const MATCHES = [
  `type='signal',sender='${SYSTEMD}',interface='${PROPERTIES}',member='PropertiesChanged',` +
    `path_namespace='${UNIT_PATHS}',arg0='${UNIT}'`,
  `type='signal',sender='${SYSTEMD}',interface='${MANAGER}',member='Reloading'`,
];

/**
 * The signals worth a pass: a unit's ActiveState, and a reload finishing. Everything else a unit says about itself - and it says a lot -
 * cannot change what it forwards.
 */
export function toEvent(message: Message): SystemdEvent | undefined {
  if (message.type !== MessageType.Signal) return undefined;

  if (message.member === "Reloading" && message.interface === MANAGER) {
    // Sent true as the reload starts and false when it is done. Only the
    // second is worth acting on: before it, the old configuration is live.
    return message.body?.[0] === false ? {} : undefined;
  }

  if (message.member === "PropertiesChanged" && message.path?.startsWith(`${UNIT_PATHS}/`)) {
    const [iface, changed, invalidated] = (message.body ?? []) as [
      string,
      Record<string, unknown> | undefined,
      string[] | undefined,
    ];
    if (iface !== UNIT) return undefined;
    const state = changed?.ActiveState;
    if (state === undefined && !invalidated?.includes("ActiveState")) return undefined;
    return {
      unit: unitName(message.path.slice(UNIT_PATHS.length + 1)),
      state: typeof state === "string" ? state : undefined,
    };
  }

  return undefined;
}

/**
 * A unit's object path, escaped as systemd escapes it: every byte that is
 * not a letter or digit, and a leading digit, becomes `_` and two hex digits.
 */
export function unitPath(name: string): string {
  let escaped = "";
  for (const [index, byte] of [...new TextEncoder().encode(name)].entries()) {
    const char = String.fromCharCode(byte);
    escaped += /[A-Za-z]/.test(char) || (index > 0 && /[0-9]/.test(char))
      ? char
      : `_${byte.toString(16).padStart(2, "0")}`;
  }
  return `${UNIT_PATHS}/${escaped}`;
}

/** The reverse of unitPath, for the last element of the path. */
export function unitName(escaped: string): string {
  const bytes: number[] = [];
  for (let index = 0; index < escaped.length; index++) {
    if (escaped[index] === "_" && /^[0-9a-f]{2}$/.test(escaped.slice(index + 1, index + 3))) {
      bytes.push(parseInt(escaped.slice(index + 1, index + 3), 16));
      index += 2;
    } else {
      bytes.push(escaped.charCodeAt(index));
    }
  }
  return new TextDecoder().decode(new Uint8Array(bytes));
}
