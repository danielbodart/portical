import { createSocket } from "node:dgram";

const SSDP_ADDRESS = "239.255.255.250";
const SSDP_PORT = 1900;

/** Device types that can forward a port, best first. */
const TARGETS = [
  "urn:schemas-upnp-org:device:InternetGatewayDevice:2",
  "urn:schemas-upnp-org:device:InternetGatewayDevice:1",
];

/**
 * Find internet gateways on the network by SSDP.
 *
 * This is the one thing Portical does that is not HTTP - SSDP is a multicast
 * UDP search - so it stays behind its own small interface rather than being
 * folded into the Handler.
 *
 * Discovery is slow and, on a host with several interfaces, unreliable, which
 * is why the README recommends setting the root URL explicitly. It is the
 * fallback, not the happy path.
 */
export async function discover(timeout = 3000): Promise<string[]> {
  const found = new Set<string>();

  await Promise.all(TARGETS.map((target) => search(target, timeout, found)));

  return [...found];
}

function search(target: string, timeout: number, found: Set<string>): Promise<void> {
  return new Promise((resolve) => {
    const socket = createSocket({ type: "udp4", reuseAddr: true });
    const finish = () => {
      // Closing an already-closed socket throws, and both the timer and an
      // error can get here first.
      try { socket.close(); } catch { /* already closed */ }
      resolve();
    };

    const timer = setTimeout(finish, timeout);
    timer.unref?.();

    socket.on("error", finish);

    socket.on("message", (message) => {
      const location = /^location:\s*(\S+)/im.exec(message.toString())?.[1];
      if (location) found.add(location);
    });

    socket.on("listening", () => {
      const search =
        `M-SEARCH * HTTP/1.1\r\n` +
        `HOST: ${SSDP_ADDRESS}:${SSDP_PORT}\r\n` +
        `MAN: "ssdp:discover"\r\n` +
        // Seconds the responder may wait before replying, so that a busy
        // network does not answer all at once. Kept below our own timeout.
        `MX: ${Math.max(1, Math.floor(timeout / 1000) - 1)}\r\n` +
        `ST: ${target}\r\n\r\n`;
      socket.send(search, SSDP_PORT, SSDP_ADDRESS, (error) => {
        if (error) { clearTimeout(timer); finish(); }
      });
    });

    socket.bind();
  });
}

/**
 * Work out which of this host's addresses the gateway would see.
 *
 * Needed because a bridge or host networked container is forwarded to *the
 * Docker host*, so the mapping has to name the host's address on the LAN. v1
 * never had to work this out: it let upnpc infer the address from the source
 * of the connection.
 *
 * Connecting a UDP socket sends nothing - it only asks the kernel to pick the
 * route, and with it the source address it would use to reach that host.
 */
export function addressFacing(host: string): Promise<string | undefined> {
  return new Promise((resolve) => {
    const socket = createSocket("udp4");
    socket.once("error", () => { socket.close(); resolve(undefined); });
    socket.connect(SSDP_PORT, host, () => {
      const address = socket.address().address;
      socket.close();
      resolve(address === "0.0.0.0" ? undefined : address);
    });
  });
}
