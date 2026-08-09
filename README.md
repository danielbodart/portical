![Portical Logo](https://raw.githubusercontent.com/danielbodart/portical/master/logo.png)

# Portical

## Overview
Portical manages UPnP port forwarding rules for Docker containers.
Set one label on a container and its ports are forwarded on your internet gateway.
It was inspired by [Traefik Proxy](https://traefik.io/traefik/) autoconfiguration of HTTP port forwarding rules.

## Requirements
- Some Docker containers you want to expose to the internet
- UPnP-enabled internet gateway (tested on Google Nest Wifi)
- 64-bit Linux, `amd64` or `arm64`. The published image is both, so a Pi 4/5 or
  an ARM NAS pulls the right one. 32-bit ARM (`armv7`, a Pi on 32-bit Raspberry
  Pi OS) is not supported: Bun has no 32-bit target to compile to.

## Usage
There are 2 parts to Portical:

1. Add the `portical.upnp.forward` label and rules (`published`, `8080:80/tcp`, `8080:80` or `8080`, `8080/udp` etc) to your Docker containers to expose them to the internet.
2. Run Portical to set up the port forwarding rules and keep them that way.

### Part 1: Adding the `portical.upnp.forward` label

The label `portical.upnp.forward` specifies port forwarding rules in the format
`${external_port}:${internal_port}/${optional-protocol}`.

#### Examples
- `published` forwards every port the container publishes on the host. Useful for the default `bridge` network driver, and avoids repeating yourself.
- `9999:8000/tcp` forwards port `9999` on the internet gateway to port `8000` using only TCP.
- `25565:25565` forwards port `25565` using both TCP and UDP.
- `19132/udp` forwards UDP port `19132`.
- `19132/udp,8080/tcp` forwards two ports.
- `published,9999:80/tcp` combines them. Terms compose freely, and an explicit rule wins if it collides with a published one.

A rule that cannot be parsed is reported and skipped, rather than half-understood.

**Docker**:

```shell
docker run -d --label portical.upnp.forward=9999:8888 -p 8888:80 nginx:latest
```

**Docker Compose**:

```yaml
services:
  nginx:
    image: 'nginx:latest'
    ports:
      - '8888:80'
    labels:
      - 'portical.upnp.forward=9999:8888'
```

#### Containers on several networks

If a container is attached to more than one network, Portical forwards to whichever
one reaches the LAN, preferring `macvlan` and `ipvlan` over `bridge`. To choose
explicitly, add a second label:

```yaml
labels:
  - 'portical.upnp.forward=443/tcp'
  - 'portical.upnp.network=lan'
```

### Part 2: Running Portical

#### Commands

- `run` (default): reconcile continuously. Reacts to containers starting and stopping, and re-checks on an interval to renew leases and correct drift.
- `update`: reconcile once and exit.
- `list`: show the gateway's current port mappings and exit. The quickest way to check Portical can talk to your router.

`poll` and `listen` are accepted as aliases for `run`. In v1 they were different
things, and neither was complete on its own - see [What changed in v2](#what-changed-in-v2).

#### Options

| Option | Description |
| --- | --- |
| `-r`, `--root URL` | UPnP root description URL. Skips discovery, and is much faster. |
| `-d`, `--duration SECONDS` | Seconds between reconcile passes (default `15`). |
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

Environment variables: `PORTICAL_UPNP_ROOT_URL` (same as `--root`) and
`PORTICAL_POLL_INTERVAL` (same as `--duration`).

#### Checking it works

Start with `list`, which only reads:

```shell
docker run --rm --network host danielbodart/portical:latest \
  -r "http://internal-gateway-ip:5000/rootDesc.xml" list
```

Then see what Portical would do, without doing it:

```shell
docker run --rm --network host -v '/var/run/docker.sock:/var/run/docker.sock' \
  danielbodart/portical:latest -r "http://internal-gateway-ip:5000/rootDesc.xml" --dry-run update
```

If you leave out `--root`, Portical searches for a gateway over SSDP. Discovery is
slow and needs host networking, so setting the root URL is worth the one-off effort.

### Docker Compose Setup (Recommended)

```yaml
services:

  portical:
    image: 'danielbodart/portical:latest'
    environment:
      - PORTICAL_UPNP_ROOT_URL=http://internal-gateway-ip:5000/rootDesc.xml # Optional
    volumes:
      - '/var/run/docker.sock:/var/run/docker.sock' # Required
    restart: unless-stopped
    network_mode: host

  # A service we are going to expose to the internet
  minecraft_java:
    image: 'gameservermanagers/gameserver:mc'
    restart: unless-stopped
    ports:
      - '25565:25565'
    labels:
      - 'portical.upnp.forward=published'

  # Another, on its own address on the LAN, so no published ports are needed
  nginx:
    image: 'nginx:latest'
    restart: unless-stopped
    networks:
      - lan
    labels:
      - 'portical.upnp.forward=8000:80/tcp'
```

`depends_on: portical` is no longer needed. Portical reconciles from the current
state of Docker on every pass, so a container that starts first, or while Portical
is down, is picked up regardless.

## How it Works

Portical compares two things and makes the second look like the first:

- **Wanted**: containers carrying the label, and the rules those labels ask for.
- **Actual**: the mappings currently on the gateway.

Anything wanted but missing is added, anything of Portical's that nothing wants any
more is removed, and anything already correct is *left alone*. Mappings belonging to
other tools are never touched unless you pass `--steal`.

Both the Docker event stream and the interval do nothing but ask for another
comparison, so container changes are picked up immediately and expiring leases are
still noticed.

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

Port forwarding works differently depending on the network driver:

* With `bridge` (the default), traffic takes two hops: gateway to Docker host
  (Portical's rule), then host to container (your normal `-p` / `ports`).
* With `host`, `macvlan` or `ipvlan`, traffic goes straight from the gateway to the
  container, and no published ports are needed.

## What changed in v2

v2 is a rewrite from Bash to TypeScript running on [Bun](https://bun.sh).

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
- **arm64 images**, which matters for the Pi and NAS boxes this tends to run on.
- **Discovery tries every reply**, not just the first, so a device that claims to
  be a gateway but forwards no ports no longer hides a working router.
- **`--dry-run`, `list`, `--steal`, `--manage-all` and `--cleanup-on-exit`** are new.

Thanks to [@weedy](https://github.com/weedy) for the lease-expiry and
listing-caching ideas in [#8](https://github.com/danielbodart/portical/pull/8), both
of which are in v2, and to [@jhenkens](https://github.com/jhenkens), whose
[Python fork](https://github.com/jhenkens/portical) is worth a look.

## Development

```shell
mise install     # installs the pinned Bun
bun install
bun run.ts test
bun run.ts check   # typecheck
bun run.ts build   # compiles a standalone binary into dist/
bun run.ts image   # builds both architectures locally
```

`run.ts` is the only place any of this is defined, so CI runs the same commands
you do.

### Versions

Published images carry both `:latest` and an exact version, so a deployment can
be pinned and rolled back:

```yaml
image: 'danielbodart/portical:2.58.412'
```

The number is derived from the repository rather than stored in it. Only the
major is committed - in `package.json`, because that one is a decision: it says
this is the rewrite and the shell script was v1, and it moves when
compatibility breaks. The minor is the commit count, so it only goes up and
names exactly one commit; the patch is the CI run number, which separates two
builds of the same commit. Locally the patch becomes a timestamp, so a
developer build sorts after CI's and is obviously not one.

Nothing needs bumping to release, and there is no committed number that can
disagree with what was published. `portical --version` reports it, every run
logs it as its first line, and it is on the image as
`org.opencontainers.image.version`. Running from source with no build step says
`development`, which is the truth about that build.

Everything that talks HTTP - the Docker Engine API and the gateway - goes through a
single `(Request) => Promise<Response>` function type, so the tests replace both
with in-memory implementations. There is no server, no port and no router involved:
the fake gateway is a function, and it has switches for the ways real routers
actually misbehave (truncating descriptions, downgrading leases, ending their
mapping table with the wrong code). The bugs above have tests written against those.

## Shutdown

Portical stops cleanly on `SIGTERM` and `SIGINT`: it stops reconciling, finishes
the pass it is in, and exits. Existing forwards are **left in place**, because a
Portical that is restarting should not take down the services it is about to
forward again. Pass `--cleanup-on-exit` to remove them instead.

`SIGKILL` cannot be caught by any process, so nothing runs on it. Nothing needs
to: Portical keeps no state of its own and works out what to do by comparing
containers against the gateway on the next start.

## Limits worth knowing

**A macvlan or ipvlan container's rule is not removed if the container went away
while Portical was not running.** Portical only removes rules pointing somewhere
it could have sent traffic, so that two Porticals on one network cannot delete
each other's rules. A macvlan container takes its address with it when it stops,
so a Portical that was not running at the time cannot tell that rule from
another host's - it leaves it alone and says so. Such a rule is reclaimed if the
container returns to the same address, and expires by itself on any gateway that
sets a lease. Bridge and host networking are unaffected, and so is the ordinary
case of a container stopping while Portical is running.

Removing it would mean starting a container that claims that address, and an
address that is free now may not be free later. Portical will not do that.

**Discovery is a fallback, not the happy path.** Anything can answer an SSDP
search, including devices that claim to be an internet gateway and forward no
ports - Portical tries every reply rather than the first because of one such
device. Some routers do not answer at all. Setting `--root` or
`PORTICAL_UPNP_ROOT_URL` skips discovery and is faster and more reliable.

## Contributing
Contributions to Portical are welcome. Please submit your contributions as pull requests on GitHub.

## License
Apache License 2.0
