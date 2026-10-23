<p align="center"><img src="logo.png" alt="Logo" width="600"></p>

# Portical

## Overview
Portical manages UPnP port forwarding rules for Docker containers and systemd units.
Set one label on a container, or one key in a unit file, and its ports are forwarded
on your internet gateway.
It was inspired by [Traefik Proxy](https://traefik.io/traefik/) autoconfiguration of HTTP port forwarding rules.

## Requirements
- Some Docker containers or systemd services you want to expose to the internet
- UPnP-enabled internet gateway (tested on Google Nest Wifi running OpenWRT)
- Linux on `amd64`, `arm64` or 32-bit `armv7`. The published images and binaries
  cover all three, so a Pi - on 64-bit or 32-bit Raspberry Pi OS - or an ARM NAS
  gets the right one. Each is a single static binary: no runtime, no libc, and
  nothing else to install.
- For `published` on a systemd unit, Linux 5.7 or later, whose socket diagnostics
  say which cgroup a socket belongs to. Nothing else needs a recent kernel.

## Usage
There are 2 parts to Portical:

1. Say which ports to forward, where the service is defined: a `portical.upnp.forward`
   label on a Docker container, or an `X-Portical-Forward` key in a systemd unit.
2. Run Portical to set up the port forwarding rules and keep them that way.

How to do both depends on where your services run:

- **[Run it in Docker](https://github.com/danielbodart/portical/blob/master/docs/docker.md)**
  for services in Docker containers. The image is on Docker Hub as
  `danielbodart/portical` and on GitHub as `ghcr.io/danielbodart/portical`.
- **[Run it as a systemd service](https://github.com/danielbodart/portical/blob/master/docs/systemd.md)**
  for services running natively on the host, from a release binary on any systemd
  distribution or from the flake's NixOS module. A native Portical can read Docker
  containers too, and is the one to use on a host that has both.

The rest of this page applies however Portical runs.

### Forwarding rules

The label and the unit key take the same rules, in the format
`${external_port}:${internal_port}/${optional-protocol}`.

- `published` forwards every port the service already exposes: for a container, the
  ports it publishes on the host; for a unit, the ports it is listening on. Avoids
  repeating yourself.
- `9999:8000/tcp` forwards port `9999` on the internet gateway to port `8000` using only TCP.
- `25565:25565` forwards port `25565` using both TCP and UDP.
- `19132/udp` forwards UDP port `19132`.
- `19132/udp,8080/tcp` forwards two ports.
- `published,9999:80/tcp` combines them. Terms compose freely, and an explicit rule wins if it collides with a published one.

A rule that cannot be parsed is reported and skipped, rather than half-understood.

### Commands

- `run` (default): reconcile continuously. Reacts to containers and units starting and stopping, and re-checks on an interval to renew leases and correct drift.
- `update`: reconcile once and exit.
- `list`: show the gateway's current port mappings and exit. The quickest way to check Portical can talk to your router.

`poll` and `listen` are accepted as aliases for `run`. In v1 they were different
things, and neither was complete on its own - see [What changed in v2](#what-changed-in-v2).

### Options

| Option | Description |
| --- | --- |
| `-r`, `--root URL` | UPnP root description URL. Skips discovery, and is much faster. |
| `-d`, `--duration SECONDS` | Seconds between reconcile passes (default `15`). |
| `-s`, `--source NAME` | Where forwards are declared: `docker`, `systemd`, or both as `docker,systemd`. Repeatable (default `docker`). The first named wins a port both ask for. |
| `--systemd-key KEY` | Unit file key to read (default `X-Portical-Forward`). |
| `-l`, `--label LABEL` | Container label to read (default `portical.upnp.forward`). |
| `--network-label LABEL` | Label naming which network to forward to (default `portical.upnp.network`). |
| `--lease SECONDS` | Lease to request; `0` never expires (default `0`). |
| `--renew-within SECONDS` | Renew a mapping expiring within this (default `43200`). |
| `--docker-socket PATH` | Docker socket (default `/var/run/docker.sock`). |
| `-n`, `--dry-run` | Report what would change without changing it. |
| `-f`, `--force` | Rewrite every rule even if it already looks correct. |
| `--steal` | Take over an external port another tool already forwards. |
| `--manage-all` | Manage every Portical rule regardless of where it points. Only safe with one Portical on the network. |
| `--helper-image IMG` | Portical's own image, used to reach the gateway from inside a macvlan container. Detected automatically. |
| `--cleanup-on-exit` | Remove Portical's mappings on shutdown. |
| `--version` | Show the version and exit. |

Environment variables: `PORTICAL_UPNP_ROOT_URL` (same as `--root`),
`PORTICAL_POLL_INTERVAL` (same as `--duration`), `PORTICAL_SOURCES` (same as
`--source`) and `PORTICAL_SYSTEMD_KEY` (same as `--systemd-key`). The systemd
source finds the system bus at `DBUS_SYSTEM_BUS_ADDRESS`, as any D-Bus client does.

If you leave out `--root`, Portical searches for a gateway over SSDP. Discovery is
slow, needs host networking and is often blocked by a host firewall, so setting the
root URL is worth the one-off effort. Start with `list`, which only reads, then
`--dry-run update` to see what Portical would do without doing it - each page above
has the commands for its setup.

## How it Works

Portical compares two things and makes the second look like the first:

- **Wanted**: containers carrying the label, active units carrying the key, and the
  rules they ask for.
- **Actual**: the mappings currently on the gateway.

Anything wanted but missing is added, anything of Portical's that nothing wants any
more is removed, and anything already correct is *left alone*. Mappings belonging to
other tools are never touched unless you pass `--steal`.

The Docker event stream, systemd's unit signals and the interval do nothing but
ask for another comparison, so changes are picked up immediately and expiring
leases are still noticed.

### Leases, and why renewal does not interrupt anything

Portical asks for a mapping that never expires, but many gateways refuse and
substitute a lease of their own — OpenWrt hands out a week, for instance. Portical
reads the remaining time on every pass and renews a mapping before it runs out.

A renewal rewrites the mapping in place and **never deletes it first**. That
distinction matters more than it looks. A gateway's redirect governs *new*
connections only; traffic on an established connection is carried by the router's
connection tracking, so a rule that vanishes for a moment is invisible to everyone
already connected — and a closed door to anyone trying to join in that window. A
delete-then-add renewal on a game server would therefore look like the server
briefly disappearing, with nothing in any log to explain it.

The one case that does delete first is a rule that has to *move* — a different
internal port or address — because several firmwares refuse to overwrite a mapping
whose target changed. `--force` rewrites in place rather than recreating.

If a mapping does expire (because Portical was not running), the same asymmetry
applies: existing connections carry on until they go idle, while new ones are
refused. That is worth knowing, because it makes an expired forward look like a
problem with the service rather than with the forward.

## What changed in v2

v2 is a rewrite from Bash to Go: one static binary, with no runtime and nothing to
install alongside it. (Its first releases were TypeScript running on Bun; the Go
port that replaced them reads the same configuration and behaves the same.)

**Upgrading needs no changes.** Every v1 flag (`-r`, `-d`, `-l`, `-v`, `-f`), both
environment variables, and all three commands (`update`, `poll`, `listen`) work as
before. `-v` is accepted and ignored, there being no subprocess left whose output
could be hidden. Label syntax is unchanged, and so is the text Portical writes into
rule descriptions - so rules already on your router are recognised and managed
rather than duplicated alongside them.

v1 was a shell script run by its full path, and its README suggested
`command: "/opt/portical/run poll"`. That still works: the path is ignored if it is
passed as an argument, and it also still exists inside the image. New setups do not
need it - the image runs Portical by default.

- **Rules that already exist are no longer rewritten.** v1 decided whether a rule
  existed by looking for its description in `upnpc -l` output. Routers truncate and
  rewrite descriptions, so on many of them every rule looked missing and was deleted
  and re-added on every pass - dropping live connections each interval, and failing
  with `code 714` when there was nothing to delete. ([#6](https://github.com/danielbodart/portical/issues/6))
- **Rules are removed when their container stops.** ([#2](https://github.com/danielbodart/portical/issues/2))
- **Containers on several networks work.** v1 ran their network names together into
  one nonsense string and skipped every rule with `Unsupported network driver: `.
  ([#1](https://github.com/danielbodart/portical/issues/1))
- **One rule failing no longer stops the rest.** v1 exited the process on any
  failure, so a single rule the router refused took down every forward on the host.
- **`listen` and `poll` are one command.** `listen` reacted to containers starting
  but never renewed a lease or noticed one stopping; `poll` did the reverse. `run`
  does both.
- **No dependencies.** v1 shelled out to the `docker` CLI and `upnpc`, and for
  macvlan networks it ran *itself* in another container's network namespace.
  Portical now speaks the Docker Engine API and UPnP SOAP directly, so the image is
  a single binary, and Portical no longer needs to be able to launch containers.
- **ARM images and binaries**, 64-bit and 32-bit, which matters for the Pi and NAS
  boxes this tends to run on.
- **Discovery tries every reply**, not just the first, so a device that claims to
  be a gateway but forwards no ports no longer hides a working router.
- **`--dry-run`, `list`, `--steal`, `--manage-all` and `--cleanup-on-exit`** are new.

Thanks to [@weedy](https://github.com/weedy) for the lease-expiry and
listing-caching ideas in [#8](https://github.com/danielbodart/portical/pull/8), both
of which are in v2, and to [@jhenkens](https://github.com/jhenkens), whose
[Python fork](https://github.com/jhenkens/portical) is worth a look.

## Development

Portical is Go, and the standard library apart from one module:
[godbus](https://github.com/godbus/dbus) for the systemd source's system bus.

```shell
mise install       # installs the pinned Go
./run test         # go test, every package
./run check        # gofmt and go vet
./run build        # a static binary for this machine, into dist/
./run release      # every architecture's binary, the unit and checksums, into dist/release/
./run image        # the image, for every architecture, locally
nix build          # the Nix package, running the tests on the way
nix flake check    # also the module check and the NixOS VM test (needs KVM)
```

`./run` is a POSIX shell script and the only place any of this is defined, so CI
runs the same commands you do.

### Versions

Published images carry both `:latest` and an exact version, so a deployment can
be pinned and rolled back:

```yaml
image: 'danielbodart/portical:2.58.412'
```

Every build that publishes an image also publishes a
[GitHub release](https://github.com/danielbodart/portical/releases) of the same
version, tagged `v2.58.412`, with the standalone binaries for `x64`, `arm64` and
`armv7`, the systemd unit, and their `SHA256SUMS`.

The number is derived from the repository rather than stored in it. Only the
major is committed - in `MAJOR`, because that one is a decision: it says this is
the rewrite and the shell script was v1, and it moves when compatibility breaks.
The minor is the commit count, so it only goes up and names exactly one commit;
the patch is the CI run number, which separates two builds of the same commit.
Locally the patch becomes a timestamp, so a developer build sorts after CI's and
is obviously not one.

Nothing needs bumping to release, and there is no committed number that can
disagree with what was published. It reaches the binary at link time
(`-ldflags -X .../internal/version.Version=...`), so nothing is generated into
the tree. `portical --version` reports it, every run logs it as its first line,
and it is on the image as `org.opencontainers.image.version`. `go run .` with no
build step says `development`, which is the truth about that build.

Everything that talks HTTP - the Docker Engine API over its socket and the
gateway's SOAP - goes through an `http.RoundTripper`, so the tests replace both
with `http.Handler`s reached through an adapter rather than a listener. There is
no server, no port and no router involved: the fake gateway is a handler, and it
has switches for the ways real routers actually misbehave (truncating
descriptions, downgrading leases, ending their mapping table with the wrong code,
refusing to overwrite a mapping that moved). The fake Docker streams its
`/events` through the same adapter as a real socket would. The bugs above have
tests written against those.

## Shutdown

Portical stops cleanly on `SIGTERM` and `SIGINT`: it stops reconciling, finishes
the pass it is in, and exits. Existing forwards are **left in place**, because a
Portical that is restarting should not take down the services it is about to
forward again. Pass `--cleanup-on-exit` to remove them instead.

`SIGKILL` cannot be caught by any process, so nothing runs on it. Nothing needs
to: Portical keeps no state of its own and works out what to do by comparing
containers and units against the gateway on the next start.

## Limits worth knowing

**Run one Portical per host.** Two on the same host - say a container for Docker
and a native one for systemd - both forward to the host's address, so each takes the
other's mappings for its own, finds nothing asking for them, and removes them. Give
one Portical every source the host needs instead: `--source docker,systemd`, which
means running it natively - see
[Docker containers too](https://github.com/danielbodart/portical/blob/master/docs/systemd.md#docker-containers-too).

Each setup has limits of its own:
[macvlan containers](https://github.com/danielbodart/portical/blob/master/docs/docker.md#limits-worth-knowing)
and [`published` on a unit](https://github.com/danielbodart/portical/blob/master/docs/systemd.md#published-for-a-unit).

**Discovery is a fallback, not the happy path.** Anything can answer an SSDP
search, including devices that claim to be an internet gateway and forward no
ports - Portical tries every reply rather than the first because of one such
device. Some routers do not answer at all. Setting `--root` or
`PORTICAL_UPNP_ROOT_URL` skips discovery and is faster and more reliable.

## Contributing
Contributions to Portical are welcome. Please submit your contributions as pull requests on GitHub.

## License
Apache License 2.0
