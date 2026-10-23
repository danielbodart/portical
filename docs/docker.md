# Running Portical in Docker

Portical's image watches Docker for containers carrying a label and forwards
their ports on your internet gateway. This is the setup for a host whose
services run in Docker. For services running natively on the host, see
[Running Portical as a systemd service](systemd.md) - and see that page too if a
host has both, since one Portical should do both jobs.

The rule grammar, the commands and the options are the same however Portical
runs, and are in the [README](../README.md).

## Images

The same image is published to two registries:

- Docker Hub: `danielbodart/portical`
- GitHub Container Registry: `ghcr.io/danielbodart/portical`

Each carries `:latest` and an exact version, such as `:2.58.412`. The examples
here use Docker Hub; `ghcr.io/danielbodart/portical` can be used in its place
anywhere. Both are built for `amd64`, `arm64` and 32-bit `arm/v7`, so a Pi or an
ARM NAS pulls the right one, whichever operating system it runs.

## Part 1: Adding the `portical.upnp.forward` label

The label `portical.upnp.forward` holds the container's forwarding rules, in the
grammar described in the [README](../README.md#forwarding-rules) -
`published`, `8080:80/tcp`, `8080:80`, `8080`, `8080/udp` and so on.

For a container, `published` forwards every port the container publishes on the
host. Useful for the default `bridge` network driver, and avoids repeating
yourself.

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

### Network drivers

Port forwarding works differently depending on the network driver:

* With `bridge` (the default), traffic takes two hops: gateway to Docker host
  (Portical's rule), then host to container (your normal `-p` / `ports`).
* With `host`, `macvlan` or `ipvlan`, traffic goes straight from the gateway to the
  container, and no published ports are needed.

### Containers on several networks

If a container is attached to more than one network, Portical forwards to whichever
one reaches the LAN, preferring `macvlan` and `ipvlan` over `bridge`. To choose
explicitly, add a second label:

```yaml
labels:
  - 'portical.upnp.forward=443/tcp'
  - 'portical.upnp.network=lan'
```

## Part 2: Running Portical

### Checking it works

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

To pin a version, so a deployment can be rolled back, name it instead of
`:latest`:

```yaml
image: 'danielbodart/portical:2.58.412'
```

### Only the docker source

The image reads Docker and nothing else. Reading systemd units from inside the
container is not supported: it would need the host's system bus, the host's unit
files at their own paths, and the host's sockets and cgroups, none of which a
container has.
To forward systemd units as well as containers, run Portical natively with
`--source systemd,docker` instead - see
[Running Portical as a systemd service](systemd.md#docker-containers-too).

### The macvlan relay

Some gateways refuse a rule pointing at a macvlan container's address unless the
request comes from that address. When that happens, Portical asks again from a
throwaway container in that container's network, running Portical's own image.
Running in Docker, it works out which image that is by itself; `--helper-image`
names one explicitly, which matters on a fork or a locally built image.

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
