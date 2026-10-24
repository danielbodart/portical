# Running Portical as a systemd service

Portical can run natively on the host as a systemd service, rather than in a
container. Run this way, it can forward ports for:

- **systemd units**, which declare their forwards with one key in the unit file, and
- **Docker containers**, through their label, exactly as the image does.

Either, or both at once. A host with services of both kinds should run one native
Portical reading both - see [Docker containers too](#docker-containers-too).

There are two ways to install it: the [NixOS module](#nixos), which is the whole
setup in a few lines of configuration, or the [release binary and unit
file](#installing-from-a-release), which works on any other systemd distribution.
The rule grammar, the commands and the options are the same
however Portical runs, and are in the [README](../README.md). For a host whose
services are all in Docker, [running Portical in Docker](docker.md) is simpler.

## NixOS

The repository is a flake with a package and a NixOS module. Importing the
module, enabling it and listing each service's forwards is all there is to it:

```nix
# flake.nix
inputs.portical.url = "github:danielbodart/portical";

# configuration.nix, with inputs.portical.nixosModules.default imported
services.portical = {
  enable = true;
  root = "http://192.168.1.1:5000/rootDesc.xml";
  # sources = [ "systemd" "docker" ];  # systemd alone by default
};

systemd.services.minecraft.portical.forward = [ "25565/tcp" "19133:19132/udp" ];
```

`portical.forward` is added to every `systemd.services.<name>` and is written into
the unit as `X-Portical-Forward` lines, checked against the grammar when the
configuration is evaluated. It works whether or not `services.portical` is enabled
in the same place. Portical runs as a dynamic user with no capabilities. With
`"docker"` among the sources, the module adds the `docker` group and orders
Portical after Docker itself, and refuses to build if there is no `docker` group.

Set `root`: SSDP discovery through the NixOS firewall usually fails, because the
gateway's reply comes from its own address to an ephemeral port that the firewall
does not connect to the multicast search. `openFirewall = true` opens the internal
port of every `portical.forward` term (not `published`, which is only known at run
time). Other options: `interval`, `leaseDuration`, `renewWithin`, `steal`,
`manageAll`, `cleanupOnExit`, `dockerSocket` and `extraArgs`.

The rest of this page is for other distributions, and for what the module writes:
`portical.forward` becomes the unit key described next.

## Declaring forwards on a unit

A service running natively on the host declares its forwards with
`X-Portical-Forward` in its unit file, in exactly the label's
[grammar](../README.md#forwarding-rules):

```ini
[Unit]
Description=Minecraft server
X-Portical-Forward=25565/tcp
X-Portical-Forward=19133:19132/udp

[Service]
ExecStart=/opt/minecraft/start
```

Only active units are forwarded, so a unit's forwards are removed when it stops,
the same way a container's are. Every forward points at the host's own address on
the LAN.

systemd ignores keys starting `X-`, so the line is harmless on a host without
Portical. It can go in `[Unit]` or the unit type's own section (`[Service]`,
`[Socket]`, ...), in the unit file or in a drop-in, which is how to add it to a
unit you did not write:

```shell
sudo systemctl edit minecraft.service
```

```ini
[Unit]
X-Portical-Forward=25565/tcp
```

It reads like any systemd list setting: each line adds to the ones before it,
across the unit file and its drop-ins, and an empty `X-Portical-Forward=` throws
away everything before it. So a drop-in with only the empty line turns a unit's
forwards off, and one with the empty line followed by new values replaces them.
Portical reads the files on disk, so after editing one by hand, run
`systemctl daemon-reload` as usual - Portical warns while a unit's files are newer
than what systemd has loaded.

The key's name can be changed with `--systemd-key` (or `PORTICAL_SYSTEMD_KEY`).
Templates' `%i` is not expanded.

### `published` for a unit

`published` means the ports the unit is listening on, found by matching each
listening socket's cgroup, which Portical asks the kernel for directly, to the
unit's - including sockets of a `.socket` unit that activates it. Only binds on all
addresses or on the host's LAN address count. It is a convenience, and explicit
ports are the recommended form:

- **UDP is a guess.** Every bound UDP socket looks like a listener, including ones a
  service opened to talk out. Those use ephemeral ports, so UDP ports in
  `ip_local_port_range` are skipped; a UDP server on such a port has to be named
  explicitly.
- **A port opened after the unit started** is picked up by the next interval pass
  rather than straight away, since systemd announces nothing when a running service
  starts listening.
- It needs Linux 5.7 or later, whose socket diagnostics carry the socket's cgroup.
  Nothing has to be installed for it.

## Installing from a release

Every build of `master` is published as a
[GitHub release](https://github.com/danielbodart/portical/releases), with a
standalone binary for each architecture and a unit file to run it:

| Asset | |
| --- | --- |
| `portical-linux-x64` | The binary, for 64-bit Intel and AMD. |
| `portical-linux-arm64` | The binary, for 64-bit ARM - a Pi 3/4/5 on 64-bit Raspberry Pi OS, or an ARM NAS. |
| `portical-linux-armv7` | The binary, for 32-bit ARMv7 - a Pi 2 onwards on 32-bit Raspberry Pi OS, or an armhf NAS. |
| `portical.service` | The unit file. |
| `SHA256SUMS` | Checksums of the above. |

The binaries are static, so they run on any distribution, glibc or musl, and
have no dependencies at all: Portical is a single file.

### Install

```shell
cd "$(mktemp -d)"
arch=$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/; s/armv7l/armv7/')
base=https://github.com/danielbodart/portical/releases/latest/download
curl -fsSLO "$base/portical-linux-$arch" -O "$base/portical.service" -O "$base/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS

sudo install -m 755 "portical-linux-$arch" /usr/local/bin/portical
sudo install -m 644 portical.service /etc/systemd/system/portical.service
sudo systemctl daemon-reload
```

Check Portical can talk to your router, before it changes anything:

```shell
portical -r "http://internal-gateway-ip:5000/rootDesc.xml" list
portical -r "http://internal-gateway-ip:5000/rootDesc.xml" --source systemd --dry-run update
```

### Configure

Settings go in a drop-in, so they survive replacing the unit file on an upgrade:

```shell
sudo systemctl edit portical
```

```ini
[Service]
Environment=PORTICAL_UPNP_ROOT_URL=http://internal-gateway-ip:5000/rootDesc.xml
```

Then start it, and have it start on boot:

```shell
sudo systemctl enable --now portical
journalctl -u portical -f
```

The unit is `Type=notify`: Portical tells systemd it is ready only once its first
pass is done and it is watching for changes, so `systemctl start` and `restart`
return with the forwards already in place, and a unit ordered `After=portical.service`
starts after them too. A pass that fails - the router briefly unreachable, say -
still counts, since the next one tries again. After each pass it leaves a one-line
status, which `systemctl status portical` shows beside the unit:

```
     Status: "3 forwards, 1 in conflict"
```

The unit reads systemd units only (`PORTICAL_SOURCES=systemd`). It runs as a
dynamic user with no capabilities, with the same sandboxing as the NixOS module:
nothing it does needs privilege. The system bus lets any user list units and
subscribe to their changes, unit files are readable by anyone, and the kernel
answers any user's question about listening sockets. It needs a systemd with
`DynamicUser`, which is any current distribution.

`PORTICAL_UPNP_ROOT_URL`, `PORTICAL_POLL_INTERVAL`, `PORTICAL_SOURCES` and
`PORTICAL_SYSTEMD_KEY` can all be set with `Environment=` this way. Any other
option is passed on the command line, by replacing `ExecStart` in the same drop-in -
the empty line clears the unit's own, as with any list setting:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/portical --lease 86400 --steal run
```

If you leave out the root URL, Portical searches for a gateway over SSDP. That is
slow, and a host firewall often drops the gateway's reply, so setting it is worth
the one-off effort.

### Docker containers too

A native Portical reads Docker containers' labels exactly as the image does. On a
host with both systemd services and Docker containers to forward, this is the way
to run it: **one Portical with both sources**, rather than the image for Docker
alongside a native one for systemd. Two Porticals on one host both forward to the
host's address, so each removes the other's mappings - see
[Limits worth knowing](../README.md#limits-worth-knowing).

Docker's socket belongs to the `docker` group, so the drop-in adds that group as
well as the source, and orders Portical after Docker:

```ini
[Unit]
After=docker.service

[Service]
Environment=PORTICAL_SOURCES=systemd,docker
SupplementaryGroups=docker
```

The shipped unit leaves the group out because a host with no `docker` group would
fail to start it at all, with `216/GROUP`. The socket is `/var/run/docker.sock`
unless `--docker-socket` says otherwise.

Everything in [Running Portical in Docker](docker.md) about labels, networks and
macvlan applies unchanged. One difference: to reach the gateway for a macvlan
container, Portical starts a container from its own image, which a native
Portical cannot detect - it uses `danielbodart/portical:latest`, and
`--helper-image` names another.

### Upgrade

Replace the binary and restart:

```shell
arch=$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/; s/armv7l/armv7/')
curl -fsSL -o portical "https://github.com/danielbodart/portical/releases/latest/download/portical-linux-$arch"
sudo install -m 755 portical /usr/local/bin/portical
sudo systemctl restart portical
```

Releases are versioned the same way as the images, so a particular one can be
installed from its own release page instead of `latest`.

### Uninstall

```shell
sudo systemctl disable --now portical
sudo rm -rf /etc/systemd/system/portical.service /etc/systemd/system/portical.service.d \
  /usr/local/bin/portical
sudo systemctl daemon-reload
```

Its mappings are left on the gateway, as on any shutdown. If they should go too,
add `--cleanup-on-exit` to its `ExecStart` before stopping it, or remove them on
the router.
