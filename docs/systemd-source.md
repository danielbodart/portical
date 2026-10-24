# Forwarding for systemd units

Portical was written to read its forwards off Docker container labels. Nothing
underneath that depends on Docker: the rule grammar (`internal/rules`), the
ownership rules (`internal/model`), the reconcile (`internal/reconcile`), the
gateway and discovery (`internal/upnp`) only ever see `DesiredForward`s. This
document is the design for a second place forwards can come from: systemd
units, declared with a key in the unit file, so that a service that runs
natively on the host - NixOS especially - gets the same forwards a labelled
container does.

Status: implemented - `internal/source` (the seam and `Combine`),
`internal/systemd`, and the flake in `flake.nix` and `nix/`. Where the build
proved the plan wrong, this document has been corrected to match what was
built, and the open questions at the end are answered.

## The Source seam

```go
// internal/source
type Desired struct {
	Forwards []model.DesiredForward
	Warnings []string
}

type Event struct {
	Description string // "" asks for a pass without a log line
}

type Source interface {
	Desired(ctx context.Context) (Desired, error)
	Events(ctx context.Context, notify func(Event)) error
}
```

The daemon takes a `Source` where it once took a Docker client:

```go
daemon.New(daemon.Deps{Source: source, Gateway: gateway, Options: options, Log: log, AsContainer: asContainer})
```

The contract, which both sources keep:

- **`Desired` is complete or it fails.** Removals are derived from absence,
  so a partial answer tears down the forwards that were not seen. A source that
  fails to read anything it needs fails the pass, and the daemon logs it and
  tries again on the next tick. This is the same rule `Once` already followed
  for the container list.
- **Warnings are returned, not logged.** The daemon prefixes and logs them,
  which keeps sources free of a logger and lets tests read them.
- **Events only mean "look again".** The daemon does not read anything out of
  an event except the optional line for the log. Every event triggers a pass,
  bursts collapse into one pass through the queue in `Run`, and the interval
  covers anything a stream missed. A source with no useful stream at all could
  block until cancelled and be correct, only slower.
- **`Events` may return early**, with an error or without. `Run` reopens it
  after `ReconnectBackoff`, and that loop is the same for every source.
- **`Description` is optional.** A source that has to listen to everything to
  catch what it cares about can ask for a pass without adding a log line.

### How Docker maps onto it

`docker.Source` wraps a `docker.Client` and the resolve options (`Label`,
`NetworkLabel`, `HostAddress`):

| Source    | Docker                                                         |
|-----------|----------------------------------------------------------------|
| `Desired` | `docker.Resolve(client.Containers(ctx, label), options)`       |
| `Events`  | `client.Events(ctx, label, ...)`, each as `Container <name> <action>` |

The client itself stays narrower than a source: the relay (`namespace.go`)
still needs it to run a container in another's network, which has nothing to do
with deciding what to forward. `AsContainer` stays a dependency of the daemon,
and only Docker forwards with a `container` target ever reach it.

### Several sources at once

`source.Combine(sources ...Source) (Source, error)`:

- `Desired` asks every source, so one failing fails the pass, for the reason
  above. Forwards are concatenated in the order the sources were named.
- Two sources asking for the same gateway slot (`Rule.Identity()`, protocol and
  external port) would rewrite each other every pass. The first one named
  wins, the same "first term wins" rule `rules.Parse` uses within a label, and
  the loser is reported as a warning naming both owners.
- `Events` merges the streams. If one returns, the merged stream returns and
  `Run` reopens all of them; each reopen is cheap and the interval pass covers
  the gap.
- None is refused: nothing to combine would want nothing, and take every
  forward down.

One Portical should run every source on a host. Two Porticals on the same
host, say a container for Docker and a native one for systemd, both forward to
the same host address, so each sees the other's mappings as its own, finds nothing
asking for them, and removes them. `ManagedAddresses` exists to stop exactly
that between hosts, and it cannot tell two instances on one host apart. The
README says so, and the NixOS module offers `sources = [ "systemd" "docker" ]`
rather than a second service.

## The systemd source

### Declaring a forward

```ini
[Unit]
X-Portical-Forward=25565
X-Portical-Forward=19133:19132/udp,published
```

The value is exactly the label grammar: `ext[:int][/proto]` terms and
`published`, comma separated, parsed by the same `rules.Parse`. systemd ignores
any key starting `X-`, so the declaration is harmless on a host without
Portical, and `systemd-analyze verify` does not complain.

Where it may appear:

- In `[Unit]`, which is what the NixOS module writes, or in the unit type's own
  section (`[Service]`, `[Socket]`, ...), so a drop-in can put it next to what
  it describes. Any other section is ignored with a warning naming the file and
  line.
- In the unit file or any drop-in.

For a non-Nix host, a drop-in is the way to add it without touching a
packaged unit:

```sh
systemctl edit minecraft.service
```

```ini
[Unit]
X-Portical-Forward=25565/tcp
```

The key is `X-Portical-Forward` by default and can be renamed with
`--systemd-key`, the counterpart of `--label`.

### Which units

Only active units. A stopped unit wants nothing, so its forwards fall out of
the next reconcile the same way a stopped container's do - nothing has to watch
for stops.

Discovery is over the system bus, `org.freedesktop.systemd1`:

1. `Manager.ListUnitsByPatterns(states = ["active", "reloading",
   "refreshing"], patterns = [])` - one call for every candidate, with each
   unit's object path. `refreshing` is a running unit having its mount
   namespace refreshed, so it is as wanted as an active one.
2. For each, `Properties.GetAll("org.freedesktop.systemd1.Unit")`, for
   `FragmentPath`, `DropInPaths`, `TriggeredBy` and `NeedDaemonReload`.
3. Only for a unit whose declaration includes `published`, its
   `ControlGroup` from the type-specific interface (`...Service`,
   `...Socket`, `...Scope`, chosen by the unit's suffix), and the
   `ControlGroup` of each unit in `TriggeredBy`.

A host has a few hundred active units, so a pass is a few hundred local D-Bus
round trips and a few hundred small file reads. The round trips overlap, but
at most 32 at a time: dbus-daemon (not dbus-broker) refuses a connection more
than 128 pending replies by default. That is well inside the
15 second interval, and the parse is cached by path, inode, size and mtime so
an unchanged file is read once per run. If that ever proves too slow, the
GetAll results can be cached until the next `Reloading` signal.

Any failure in any of those - the bus, a property, a file listed in
`FragmentPath` or `DropInPaths` that cannot be read - fails the pass, for the
completeness reason above. The one exception is a file that no longer exists:
systemd keeps naming a deleted unit file or drop-in until the next
daemon-reload, and what is on disk now is nothing, so it is read as empty.

User units (`systemd --user`) are out of scope.

### Reading the key

Files are parsed the way systemd parses them, as far as this key needs:

- `[Section]` headers; blank lines; comments are lines whose first
  non-blank character is `#` or `;`.
- A line ending in `\` continues on the next line, joined with a space. A
  comment line inside a continuation is skipped, as systemd does.
- `Key=Value`, whitespace around both trimmed. Keys are case sensitive.
- No specifier expansion. A term containing `%` falls out of `rules.Parse` as an
  unrecognised rule and is reported like any other bad term. (`%i` for
  template instances is an open question below.)

Files are applied in systemd's order: the fragment, then `DropInPaths` in the
order systemd reports them, which is already the order it applied them in -
sorted by file name across the drop-in directories, with a file of the same
name in a higher priority directory replacing the lower one. Within a file,
top to bottom.

Assignments behave like a systemd list setting such as `ExecStartPre=` or
`Environment=`:

- **Each assignment appends** its terms, so a drop-in adds forwards to the
  ones in the unit file.
- **An empty assignment resets** everything before it, in that file and in
  every file applied earlier. A drop-in containing `X-Portical-Forward=` and
  nothing else turns off a packaged unit's forwards; `X-Portical-Forward=`
  followed by new values replaces them.

The terms gathered from every assignment are joined and parsed once with
`rules.Parse`, so duplicate slots collapse first-term-wins exactly as within one
label, and errors are reported as `<unit>: <error>`.

The files are what is on disk now, not what systemd loaded at the last
reload, because systemd does not expose unknown keys over D-Bus. Where the unit
says `NeedDaemonReload`, the pass warns that the forwards come from files
systemd has not loaded yet.

### What a unit's forward points at

Always this host: target `model.Host`, `InternalClient` the address
`upnp.AddressFacing` works out from the gateway's control URL, as for a bridge or
host-networked container. There is no relay and no macvlan case. The unit name
in full (`minecraft.service`) is what the description carries, in the existing
format: `portical: (25565:25565/tcp) minecraft.service`.

### `published`

For a container, `published` means "the ports Docker publishes on the host".
For a unit it means **the ports the unit is listening on**, found from the
kernel's socket table with each socket's cgroup.

The kernel is asked directly, over `NETLINK_SOCK_DIAG` - the interface `ss`
itself uses, so nothing has to be installed, run or parsed. Four `inet_diag`
dumps, TCP in `LISTEN` and UDP bound but unconnected, each for IPv4 and IPv6,
asking for the socket's cgroup id and its `v6only` flag. They are made once per
pass, and only when some active unit asks for `published`. All of it is the
standard library's `syscall` package, with the request and reply layouts
written out in the `sockdiag` package of
[gokit](https://github.com/danielbodart/gokit), the standard-library-only
module Portical shares with its sibling daemons.

The cgroup id is the inode of the cgroup's directory in the cgroup2 file
system, so the ids are put back to paths by walking `/sys/fs/cgroup` (or its
`unified/` on a host still mounting the v1 hierarchies), as `ss` does. On a
32-bit kernel the id carries a generation above the inode, in its high 32
bits, so there a socket is matched by the id's low half. Each
socket's cgroup - the cgroup of whoever created it - is then matched against
the unit:

- A socket whose cgroup is the unit's `ControlGroup`, or beneath it
  (`<ControlGroup>/...`, for services that put workers in sub-cgroups).
- A socket whose cgroup is the `ControlGroup` of a unit in the unit's
  `TriggeredBy`. Sockets for socket activation are made by PID 1 on behalf of
  the `.socket` unit, and are charged to that unit's cgroup - on the
  development host, `systemd-resolved`'s own sockets are in
  `/system.slice/systemd-resolved.service`, but a socket-activated one is in
  `/system.slice/frisket-dns.socket`. A `.socket` unit can carry the key
  itself, too, and matches by its own cgroup.

Only binds that the gateway's traffic could reach count:

- `0.0.0.0`, and `::` unless the socket is `v6only` (with Linux's default
  `bindv6only=0` an IPv6 wildcard takes IPv4 too), or the host address itself.
- Not loopback, link-local, other specific addresses, or sockets bound to an
  interface (`127.0.0.53%lo`).

Each surviving socket becomes the rule `port:port/protocol`, as Docker's
published ports do, and explicit terms win any collision with them.

Caveats, which the README carries:

- **UDP is guesswork.** Every bound, unconnected UDP socket is listed, which
  includes client sockets a service opened to talk out - a resolver, a game
  server's query to a master list. Those sit on ephemeral ports, so UDP
  sockets inside `/proc/sys/net/ipv4/ip_local_port_range` are left out. A UDP
  server that does bind in that range has to be named explicitly.
- **A late listener is caught by the interval, not an event.** systemd says
  nothing when a running service opens a port.
- **It needs the socket's cgroup in `sock_diag`** - Linux 5.7 or later. Where
  the kernel reports no cgroup for any socket, or there is no cgroup2 file
  system to look the ids up in, `published` resolves to nothing for every unit
  and the pass warns once; failing to list the sockets at all fails the pass.

Explicit terms are the recommended form; `published` is a convenience.

### Events

On the same bus connection:

1. `AddMatch` for `Reloading` on `org.freedesktop.systemd1.Manager`, and
   `PropertiesChanged` with `arg0='org.freedesktop.systemd1.Unit'` under
   `path_namespace='/org/freedesktop/systemd1/unit'`.
2. `Manager.Subscribe()`, without which systemd sends no unit signals.
   Allowed for any user by systemd's bus policy; checked unprivileged on the
   development host and as the module's DynamicUser in the VM test.

`UnitNew`, `UnitRemoved` and `JobRemoved` turned out to add nothing: whatever
they announce that matters also moves a unit's `ActiveState`.
`Reloading(false)` is an event (a daemon-reload may have changed the files);
`Reloading(true)` is not, since the old configuration is live until it ends.

systemd sends `ActiveState` with every change to *any* of a unit's
properties, whether or not the state moved - seen on the development host, a
unit starting produces several signals all saying `active`. The bus passes
them all on as `Change{Unit, State}`, a plain translation of the signal, and
the source keeps the last state it saw per unit on this stream, acting only
when the state differs from it. A new stream starts knowing nothing, so its
first word on a unit is where the unit is, not that it moved. Any unit can
start carrying the key, so every such event asks for a pass, but only a unit
that has declared forwards at some point this run, reaching `active`,
`inactive` or `failed` from a state it was seen in, gets a log line
(`Unit minecraft.service inactive`); the rest come through without a
description. The set of declaring units never shrinks:
a stopping unit drops out of the pass its first event triggers, so a set
rebuilt each pass would leave out exactly the line explaining why its forward
went.

As for Docker, the stream is opened after the first pass, so a unit that
changes between the two is caught by the interval rather than an event.

The bus dropping ends the stream with an error, and `Run` reconnects as it
does for Docker. If the bus cannot be reached at all the interval still
reconciles - but `Desired` needs the bus too, so in practice the passes
report the error until it is back.

`busctl monitor` is not an option: it needs `BecomeMonitor`, which an
unprivileged user is refused ("Access denied", checked). `busctl call` would
work for `Desired` but costs a process per call.


### The D-Bus client

[godbus](https://github.com/godbus/dbus), Portical's one third-party module
(gokit, its other, is the author's own and only the standard library), with
what every systemd client over godbus needs - finding the system bus, a
connection whose signals arrive in order, naming a D-Bus error, unit object
paths, subscribing to systemd - taken from gokit's separate `dbusx` module,
shared with the sibling daemons. All behind an interface no wider than the
source needs:

```go
// internal/systemd
type Unit struct {
	Name             string
	FragmentPath     string
	DropInPaths      []string
	TriggeredBy      []string
	NeedDaemonReload bool
}

type Change struct {
	Unit, State string // both "" for a finished Reloading
}

type Bus interface {
	Units(ctx context.Context) ([]Unit, error)
	ControlGroup(ctx context.Context, unit string) (string, error)
	Watch(ctx context.Context, notify func(Change)) error
}
```

`DBus` implements it over the system bus at `DBUS_SYSTEM_BUS_ADDRESS`, or
`/run/dbus/system_bus_socket`; only `unix:` addresses are taken
(`dbusx.SystemBusAddress`). It is thin:
the calls above and the matches for the two signals, with godbus doing the
authentication and the wire format. The `GetAll` calls overlap on the one
connection, at most `InFlight` (32) at a time.

`systemd.NewSource` takes a `Bus`, a `sockdiag.Lister` (`sockdiag.Kernel()` for
the real one), an `fs.FS` rooted at `/` for the unit files (cached by path,
inode, size and mtime) and a function for the ephemeral port range, so every
part of it can be driven by a fake.

## Command line

| Flag                 | Env                    | Meaning                                        |
|----------------------|------------------------|------------------------------------------------|
| `-s`, `--source NAME` | `PORTICAL_SOURCES`    | `docker` or `systemd`. Repeatable, or comma separated (`docker,systemd`). Default `docker`, so nothing changes for anyone already running Portical. The first flag replaces the variable's list rather than adding to it. |
| `--systemd-key KEY`  | `PORTICAL_SYSTEMD_KEY` | The unit file key to read (default `X-Portical-Forward`). |

`--label`, `--network-label`, `--docker-socket` and `--helper-image` only mean
anything with the Docker source. Without it, Portical does not connect to
Docker at all, and does not look for its own image.

The usage line is "UPnP port forwarding for Docker containers and systemd
units", and "Watching for container changes" became "Watching for changes".

## Nix

### Flake

`flake.nix` with `nixpkgs` (unstable, for `buildGo127Module`: go.mod says
`go 1.27`, and the default `buildGoModule` there is still Go 1.26) as its only
input, for `x86_64-linux` and `aarch64-linux`. The pieces are in `nix/`:
`package.nix`, `module.nix`, `module-check.nix` and `vm-test.nix`.

- `packages.<system>.default` - the same static `go build` as `./run build`,
  with the version from `MAJOR`, the flake's commit count and its
  `lastModifiedDate` linked in with `-X`. godbus, gokit and gokit/dbusx are
  vendored by the build's fixed-output `vendorHash`. Nothing is wrapped and nothing goes
  on `PATH`: `published` asks the kernel itself. `go test` runs as the
  package's check phase.
- `checks.<system>` - the package (and so `go test`); the module evaluation
  check; the VM test.
- `nixosModules.default`.

### Module

```nix
services.portical = {
  enable = true;
  package = ...;                       # default: this flake's package
  root = "http://192.168.1.1:5000/rootDesc.xml";  # null = SSDP discovery
  sources = [ "systemd" ];             # listOf (enum [ "systemd" "docker" ])
  interval = 15;
  leaseDuration = 0;
  renewWithin = 43200;
  steal = false;
  manageAll = false;
  cleanupOnExit = false;
  dockerSocket = "/run/docker.sock";   # only with "docker" in sources
  openFirewall = false;
  extraArgs = [ ];
};

systemd.services.minecraft.portical.forward = [ "25565" "19133:19132/udp" ];
```

The second part extends the `systemd.services` submodule, as nixpkgs'
confinement module does: declaring `options.systemd.services` again as
`attrsOf (submodule { ... })` merges a new option into every service.

```nix
options.systemd.services = lib.mkOption {
  type = lib.types.attrsOf (lib.types.submodule ({ config, ... }: {
    options.portical.forward = lib.mkOption {
      type = lib.types.listOf (lib.types.strMatching
        "published|[0-9]{1,5}(:[0-9]{1,5})?(/(tcp|udp))?");
      default = [ ];
    };
    config = lib.mkIf (config.portical.forward != [ ]) {
      unitConfig."X-Portical-Forward" = config.portical.forward;
    };
  }));
};
```

A list in `unitConfig` renders as one `X-Portical-Forward=` line per element,
which the append rule reads as all of them. The type check catches a typo at
evaluation rather than as a runtime warning. The option is declared whether or
not `services.portical.enable` is set, so a configuration can declare forwards
in one place and enable Portical in another.

The service:

- `wantedBy = [ "multi-user.target" ]`, after and wanting
  `network-online.target`; `Restart = "on-failure"`.
- `DynamicUser = true`, no capabilities (`CapabilityBoundingSet = ""`),
  `NoNewPrivileges`.
- `RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" "AF_NETLINK" ]`,
  for the gateway and SSDP, the system bus, and `published`'s `NETLINK_SOCK_DIAG`.
- `ProtectSystem = "strict"`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
  `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`,
  `ProtectControlGroups` (read-only is enough: `published` only reads
  `/sys/fs/cgroup` to name a cgroup), `ProtectClock`, `ProtectHostname`,
  `ProtectProc = "invisible"`, `RestrictNamespaces`, `RestrictRealtime`,
  `RestrictSUIDSGID`, `LockPersonality`, `SystemCallArchitectures = "native"`,
  `SystemCallFilter = [ "@system-service" "~@privileged" ]`, `UMask = "0077"`,
  and `MemoryDenyWriteExecute`, since a Go binary has no JIT. The VM test runs
  under all of it, `published` included.
- Not `~@resources`, which forbids calls a runtime may reasonably make for its
  own threads, for no gain here. Not `PrivateUsers`: it would leave the
  `docker` group unmapped.
- Not `PrivateNetwork`: the whole point is the LAN.
- The system bus needs nothing extra: systemd's bus policy already lets any
  user call `ListUnitsByPatterns`, `Get`/`GetAll` and `Subscribe`. The unit
  files live in `/etc`, `/run` and `/nix/store`, all readable under
  `ProtectSystem = "strict"`.
- With `"docker"` in `sources`, `SupplementaryGroups = [ "docker" ]` for the
  socket. The relay for macvlan containers runs Portical's own image, which a
  native Portical cannot detect, so `--helper-image` has to be given in
  `extraArgs` for that case.
- `openFirewall` opens each declared forward's internal port and protocol in
  `networking.firewall`, worked out from every service's `portical.forward` at
  evaluation time. `published` cannot be known then, so it opens nothing.

SSDP discovery behind the NixOS firewall is unreliable: the gateway answers an
M-SEARCH from its own address to an ephemeral port, which conntrack does not
match to a multicast request, so the reply is dropped. The option's
description will recommend setting `root`.

### VM test

`runNixOSTest` with three nodes, laid out as nixpkgs' own `upnp` test:

- `router`: two interfaces with NAT, `services.miniupnpd` with
  `appendConfig = "http_port=5000"` so the root URL is known. nftables, since
  miniupnpd's iptables backend cannot initialise in the VM.
- `host`: `services.portical` with that root, `interval = 3600` so every change
  after the first pass has to arrive as an event, `openFirewall`, a Python
  HTTP server on 8080 with `portical.forward = [ "8080/tcp" ]`, and one on 9090
  with `portical.forward = [ "published" ]`.
- `client`: outside the router, to `curl` both forwards through it.

The script checks the rendered `X-Portical-Forward` line, waits for both
mappings in `upnpc -l`, curls both from outside, stops a service and waits for
its mapping and its `Unit explicit.service inactive` line, adds a drop-in with
an empty `X-Portical-Forward=` to the other and `daemon-reload`s, waits for
that mapping to go, and checks the service ran as a non-root user with no
`Error:` or `Warning:` in its journal.

There is also `checks.module`: a NixOS evaluation with `portical.forward` set,
asserting the rendered unit text, the service's `ExecStart` and what
`openFirewall` opened, for a machine without KVM.

## Test plan

All in memory, in the style of the rest of the tests: fakes behind the same
interfaces the real things implement, with captured real output as fixtures.
The few tests that touch the real host - the kernel's sockets, the system bus -
skip where there is none, as in Nix's build sandbox.

- **Unit file parser** - sections, both comment characters, continuations
  (with a comment inside one), key in `[Unit]`, in `[Service]` and in an
  ignored section (with its warning), several assignments appending, an empty
  assignment resetting within a file and across files.
- **Drop-in ordering** - fragment then drop-ins in the given order; a drop-in
  that resets; one that adds; one that resets and replaces.
- **Which sockets count** - the development host's real sockets, captured once
  as `ss -Hlntu -e` output in `internal/systemd/testdata` and read into
  `sockdiag.Listener`s by the test: wildcard IPv4 and IPv6, `v6only`, loopback
  and `%lo` dropped, the host address kept, cgroup equal, beneath, and via a
  `TriggeredBy` socket unit, ephemeral UDP dropped, no cgroup reported at all
  (warns, resolves nothing), a unit with no cgroup claiming nothing.
- **The netlink lister** is tested where it lives, in gokit's `sockdiag`: the
  reply parser against built messages, and the real kernel - a TCP and a UDP
  listener the test opens, found with its own cgroup; connected sockets left
  out; no cgroup2 file system still listing the sockets, without their cgroups.
- **`systemd.Source`** - with a fake systemd (units as plain values), an
  in-memory `fs.FS` and a fake lister: an active unit's forwards, an inactive
  unit wanting nothing, `published` alongside explicit terms with explicit
  winning, `NeedDaemonReload` warning, an unreadable fragment failing the
  whole pass.
- **The daemon, end to end** - the daemon with a `systemd.Source` over the
  fakes and the fake gateway: a unit gets its forward pointing at the host
  address, stopping it removes it, a reset drop-in removes it, a unit event
  reconciles without waiting for the interval, a dropped event stream
  reconnects (the same cases `Run` has for Docker).
- **`Combine`** - forwards from both; one source failing fails the pass and
  removes nothing; a slot claimed by both goes to the first source with a
  warning; events from either trigger a pass.
- **`DBus`** - against gokit's `dbustest` bus: the calls a pass makes and
  how many wait at once, signals translated into `Change`s, unit changes held
  until a reload is done, a dropped bus; and, against the real system bus
  where there is one, listing the host's running units and subscribing until
  cancelled. The system bus address, unit path escaping and D-Bus error names
  are tested where they live, in `dbusx`.
- **Arguments** - `--source` repeated and comma separated,
  `PORTICAL_SOURCES`, an unknown source rejected, `--systemd-key`.
- **Nix** - `nix flake check`: build, `go test`, module evaluation, VM test.

## README

A "systemd units" section alongside the label documentation: the key and its
grammar (pointing at the label's), a unit file example, the `systemctl edit`
drop-in example and the reset rule, `published` and its UDP caveat, the
`--source` flag; a "NixOS" section with the flake input, `services.portical`
and `systemd.services.<name>.portical.forward`; and a line in "Limits worth
knowing" about one Portical per host.

## Decisions on the open questions

1. **godbus for D-Bus**, behind the narrow `Bus` interface. Portical's first,
   TypeScript, implementation had a hand-written client of about 600 lines to
   stay free of dependencies; in Go one well-kept module is worth more than
   that much wire format to maintain. Checked against the real system bus
   unprivileged and in the VM.
2. **Netlink rather than `ss`** for `published`: the same kernel interface,
   asked directly, so the package needs nothing on `PATH` and nothing is
   parsed from another program's output.
3. **Go comes from nixos-unstable**, as `buildGo127Module`. The lock is pinned
   to the same unstable revision as the development host's system.
4. **`%i` is not expanded.** A term containing it is reported as unrecognised.
   Still small to add later.
5. **`DesiredForward.Owner`** is the owner's name for logs and the
   description, a container's or a unit's. The daemon's wording does not
   assume containers: "Watching for changes", and removals say "nothing asks
   for it any more". "for <name>" reads fine with a unit name.
6. **The CLI defaults to `docker`**, so existing installs are untouched; the
   module defaults to `[ "systemd" ]`. No detection.
7. **`openFirewall` is in**, default off.
8. **The description carries the full unit name** (`portical: (25565:25565/tcp)
   minecraft.service`), unambiguous against a container called `minecraft`.
