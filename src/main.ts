import { DEFAULTS, Portical, type Options } from "./daemon.ts";
import { DEFAULT_HELPER_IMAGE, ownImage, viaContainer } from "./namespace.ts";
import { runRelay } from "./relay.ts";
import { addressFacing, discover } from "./discovery.ts";
import { HttpDockerClient } from "./docker.ts";
import { http, overUnixSocket, withTimeout } from "./http.ts";
import { UpnpGateway } from "./upnp.ts";

const USAGE = `portical - UPnP port forwarding for Docker containers, driven by a label

Usage: portical [options] [command]

Commands:
  run       Reconcile continuously, reacting to container changes (default)
  update    Reconcile once and exit
  list      Show the gateway's current port mappings and exit

Options:
  -r, --root URL          UPnP root description URL (skips discovery, much faster)
  -d, --duration SECONDS  Seconds between reconcile passes (default ${DEFAULTS.interval})
  -l, --label LABEL       Container label to read (default ${DEFAULTS.label})
      --network-label L   Label naming which network to forward to (default ${DEFAULTS.networkLabel})
      --lease SECONDS     Lease to request; 0 never expires (default ${DEFAULTS.leaseDuration})
      --renew-within SEC  Renew a mapping expiring within this (default ${DEFAULTS.renewWithin})
      --docker-socket P   Docker socket (default /var/run/docker.sock)
  -n, --dry-run           Report what would change without changing it
  -f, --force             Rewrite every rule even if it already looks correct
      --steal             Take over a port another tool already forwards
      --helper-image IMG  Portical's own image, used to reach the gateway from
                          inside a macvlan container's network. Detected
                          automatically; only set this if detection fails.
      --manage-all        Manage every Portical rule, wherever it points.
                          Only safe with one Portical on the network.
      --cleanup-on-exit   Remove our mappings on shutdown
  -h, --help              Show this message

Environment:
  PORTICAL_UPNP_ROOT_URL  Same as --root
  PORTICAL_POLL_INTERVAL  Same as --duration

The commands 'poll' and 'listen' are accepted as aliases for 'run'. In v1 they
were different things - 'listen' reacted to container starts but never renewed
a lease or noticed a container stopping, and 'poll' did the reverse. 'run' does
both, so there is nothing left to choose between.
`;

interface Parsed {
  readonly command: string;
  readonly args: string[];
  readonly options: Options;
  readonly root?: string;
  readonly socket: string;
  /** Only set when the user named one; otherwise it is detected at runtime. */
  readonly explicitHelperImage?: string;
  readonly help: boolean;
}

export function parseArguments(argv: readonly string[], env: Record<string, string | undefined> = {}): Parsed {
  let command = "run";
  const args: string[] = [];
  let root = env.PORTICAL_UPNP_ROOT_URL;
  let socket = env.DOCKER_SOCKET ?? "/var/run/docker.sock";
  let explicitHelperImage = env.PORTICAL_HELPER_IMAGE;
  let help = false;
  const options: Record<string, unknown> = {
    ...DEFAULTS,
    interval: env.PORTICAL_POLL_INTERVAL ? number(env.PORTICAL_POLL_INTERVAL, "PORTICAL_POLL_INTERVAL") : DEFAULTS.interval,
  };

  const rest = [...argv];
  while (rest.length > 0) {
    const argument = rest.shift()!;
    const value = () => {
      const next = rest.shift();
      if (next === undefined) throw new Error(`${argument} needs a value`);
      return next;
    };

    switch (argument) {
      case "-r": case "--root": root = value(); break;
      case "-d": case "--duration": options.interval = number(value(), argument); break;
      case "-l": case "--label": options.label = value(); break;
      case "--network-label": options.networkLabel = value(); break;
      case "--lease": options.leaseDuration = number(value(), argument); break;
      case "--renew-within": options.renewWithin = number(value(), argument); break;
      case "--docker-socket": socket = value(); break;
      case "--helper-image": explicitHelperImage = value(); break;
      case "-n": case "--dry-run": options.dryRun = true; break;
      case "-f": case "--force": options.force = true; break;
      case "--steal": options.steal = true; break;
      case "--manage-all": options.manageAll = true; break;
      case "--cleanup-on-exit": options.cleanupOnExit = true; break;
      // Accepted and ignored: v1 used it to show upnpc's output, and there is
      // no longer a subprocess whose output could be hidden.
      case "-v": case "--verbose": break;
      case "-h": case "--help": help = true; break;
      default:
        if (argument.startsWith("-")) throw new Error(`unknown option '${argument}'`);
        // The first bare word names the command; anything after it belongs to
        // that command, which is how `relay` receives its payload.
        if (args.length === 0 && command === "run") command = argument;
        else args.push(argument);
    }
  }

  return {
    command,
    args,
    options: options as unknown as Options,
    root,
    socket,
    explicitHelperImage,
    help,
  };
}

function number(value: string, name: string): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed < 0) throw new Error(`${name} must be a positive number, got '${value}'`);
  return parsed;
}

/** Find the gateway, by root URL if we were given one and by SSDP if not. */
async function connect(root: string | undefined) {
  const gatewayHttp = withTimeout(http, 10_000);

  if (root) return UpnpGateway.at(gatewayHttp, root);

  console.log("Searching for an internet gateway...");
  const found = await discover();
  if (found.length === 0) {
    throw new Error(
      "no UPnP internet gateway found. Check UPnP is enabled on your router, and " +
        "note that discovery needs the container on the host network. Setting --root " +
        "or PORTICAL_UPNP_ROOT_URL skips discovery entirely and is much faster.",
    );
  }

  // Several answers usually means one router replying to both searches, but it
  // can mean two gateways, in which case saying which one we took matters.
  if (found.length > 1) console.log(`Found ${found.length} gateways, using ${found[0]}`);
  return UpnpGateway.at(gatewayHttp, found[0]!);
}

export async function main(argv: readonly string[]): Promise<number> {
  let parsed: Parsed;
  try {
    parsed = parseArguments(argv, Bun.env);
  } catch (error) {
    console.error(`Error: ${(error as Error).message}\n`);
    console.error(USAGE);
    return 2;
  }

  if (parsed.help) {
    console.log(USAGE);
    return 0;
  }

  // Handled before anything else: relay runs inside a throwaway container that
  // has no Docker socket and no business discovering a gateway. It performs one
  // HTTP request from that container's network and prints the result.
  if (parsed.command === "relay") {
    const payload = parsed.args[0];
    if (payload === undefined) {
      console.error("Error: relay needs a payload. It is used internally by Portical.");
      return 2;
    }
    return runRelay(payload);
  }

  const gateway = await connect(parsed.root);
  console.log(`Using gateway at ${gateway.controlUrl}`);

  const external = await gateway.externalAddress().catch(() => undefined);
  if (external) console.log(`External address is ${external}`);

  if (parsed.command === "list") {
    for (const mapping of await gateway.mappings()) {
      console.log(
        `${mapping.protocol.padEnd(3)} ${String(mapping.externalPort).padStart(5)} -> ` +
          `${mapping.internalClient}:${mapping.internalPort}  ${mapping.description}`,
      );
    }
    return 0;
  }

  // Which of this host's addresses the gateway can see. Bridge and host
  // networked containers are forwarded to the host, so their mappings have to
  // name it, and v1 never had to work it out because upnpc inferred it.
  const hostAddress = await addressFacing(new URL(gateway.controlUrl).hostname);
  if (hostAddress) console.log(`This host is ${hostAddress} on the LAN`);

  const docker = new HttpDockerClient(overUnixSocket(parsed.socket));
  const options = { ...parsed.options, hostAddress };

  // The same gateway, reached from inside a given container's network. Used
  // only when a rule pointing at a container's own address is refused because
  // of where it was asked from - see namespace.ts.
  //
  // The relay runs Portical's own image, so an explicit --helper-image is only
  // needed when we cannot work out what we are running as.
  const helperImage = parsed.explicitHelperImage ?? (await ownImage(docker)) ?? DEFAULT_HELPER_IMAGE;
  const asContainer = (container: string) =>
    new UpnpGateway(
      viaContainer(docker, container, helperImage),
      gateway.controlUrl,
      gateway.serviceType,
    );

  const portical = new Portical(docker, gateway, options, console.log, asContainer);

  if (options.dryRun) console.log("Dry run - nothing will be changed");

  switch (parsed.command) {
    case "update":
      await portical.once();
      return 0;

    case "run": case "poll": case "listen": {
      const controller = new AbortController();
      for (const signal of ["SIGINT", "SIGTERM"] as const) {
        process.on(signal, () => {
          console.log(`\nReceived ${signal}, shutting down...`);
          controller.abort();
        });
      }

      await portical.run(controller.signal);
      if (options.cleanupOnExit && !options.dryRun) await portical.cleanup();
      return 0;
    }

    default:
      console.error(`Error: '${parsed.command}' is not a command\n`);
      console.error(USAGE);
      return 2;
  }
}

if (import.meta.main) {
  main(Bun.argv.slice(2))
    .then((code) => process.exit(code))
    .catch((error: Error) => {
      console.error(`Error: ${error.message}`);
      process.exit(1);
    });
}
