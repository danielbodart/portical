![Portical Logo](logo.png)

# Portical

## Overview
Portical is a script/docker container designed to manage UPnP port forwarding rules for Docker containers. 
It allows users to set up port forwarding based on container labels and supports updating these rules periodically.
It was inspired by [Traefik Proxy](https://traefik.io/traefik/) autoconfiguration of port forwarding rules.

## Requirements
- Docker environment
- UPnP-enabled network gateway (tested on Google Nest Wifi)

## Configuration
Environment Variables:
- `PORTICAL_UPNP_ROOT_URL`: The root URL for the UPnP device.
- `PORTICAL_POLL_INTERVAL`: Interval in seconds for polling and updating rules (default: 15 seconds).

## Usage
The script can be executed with various commands and options:

- **Commands**:
    - `update`: Default command. Finds containers with the specified label and sets up port forwarding (one off).
    - `poll`: Continuously updates port forwarding rules at specified intervals.

- **Options**:
    - `-r`, `--root [URL]`: Set the UPnP root URL.
    - `-d`, `--duration [SECONDS]`: Set the polling interval in seconds (default: `15` seconds).
    - `-l`, `--label [LABEL]`: Specify the Docker label to filter containers (default: `portical.upnp.forward`.
    - `-v`, `--verbose`: Enable verbose output.


### Shell Usage (Primarily for testing)

```shell
./run update
```

If autodiscovery does not work, you can specify the UPnP root URL using the `-r` or `--root` option:

```shell
./run --root "http://internal-ip:5000/rootDesc.xml" update
```

### Docker Usage

```shell
docker run --rm -d -v '/var/run/docker.sock:/var/run/docker.sock' danielbodart/portical:latest /opt/portical/run poll 
```



### Docker Compose Setup (Example)

Running the `./run` command will set the UPnP duration to the maximum allowed by the device (On Google Nest Wifi, this is 7 days, or 604800 seconds) 
so you should setup a Docker container to run the script periodically. For example, you can use the following `docker-compose.yml` file:


```yaml
version: '3.8'

volumes:
  minecraft_java:
    name: minecraft_java

services:

  portical:
    image: 'danielbodart/portical:latest'
    container_name: portical
    environment:
      - PORTICAL_UPNP_ROOT_URL="http://internal-ip:5000/rootDesc.xml"
    volumes:
      - '/var/run/docker.sock:/var/run/docker.sock'
    restart: unless-stopped
    network_mode: none

  # This is a service we are going to expose to the internet
  minecraft_java: 
    image: 'gameservermanagers/gameserver:mc'
    container_name: minecraft_java 
    volumes:
      - 'minecraft_java:/data'
    restart: unless-stopped
    ports: 
      - '25565:25565' # This is the port that will be exposed on the host (when in bridge network mode)
    labels:
    - 'portical.upnp.forward=25565:25565' # This is the port that will be exposed on your router

  # This is another service we are going to expose to the internet
  minecraft_bedrock: 
    image: 'gameservermanagers/gameserver:mcb'
    container_name: minecraft_bedrock
    volumes:
      - 'minecraft_bedrock:/data'
    restart: unless-stopped
    network_mode: custom_network # This is a custom network (could be macvlan or ipvlan), notice no ports are needed
    labels:
      - 'portical.upnp.forward=19132:19132/udp'
```


## How it Works
1. **Forwarding Setup**: For each Docker container with the specified label `portical.upnp.forward` and a rule `${external_port}:${internal_port}/${optional-protocol}`, the script sets up port forwarding using UPnP. eg:
    - `8000:8000/tcp` will forward port `8000` to the container's port `8000` using only the TCP protocol.
    - `25565:25565` will forward port `25565` to the container's port `25565` using both TCP and UDP protocol.
    - `19132:19132/udp` will forward port `19132` to the container's port `19132` using only the UDP protocol.
2. **Polling**: In polling mode, the script periodically checks and updates the forwarding rules based on the current state of the containers.
3. **Network Handling**: Supports different network drivers and configures port forwarding accordingly. Works with:
    - `bridge` network driver
    - `host` network driver
    - `macvlan` network driver
    - `ipvlan` network driver
    
## TODO

* WARNING: Currently, the script will remove and recreate ports on each poll (Fixing as soon as possible!)
* Add support for automatic port forwarding based on exposed ports of container (only for bridge network driver)
* Test more corner cases!


## Contributing
Contributions to Portical are welcome. Please submit your contributions as pull requests on GitHub.

## License
Apache License 2.0
