package upnp

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// SSDP's multicast group, which every UPnP device listens on.
var ssdp = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

// Targets is the device types that can forward a port, best first.
var Targets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
}

// DiscoveryTimeout is how long Discover listens for answers by default.
const DiscoveryTimeout = 3 * time.Second

// Discover finds internet gateways on the network by SSDP, and returns where
// each one's device description is, in the order they answered.
//
// This is the one thing Portical does that is not HTTP - SSDP is a multicast
// UDP search - so it stays a function of its own rather than going through a
// transport.
//
// Discovery is slow and, on a host with several interfaces, unreliable, which
// is why the README recommends setting the root URL explicitly. It is the
// fallback, not the happy path, and a search that fails is reported as having
// found nothing: the caller already says what to do about that.
func Discover(ctx context.Context, timeout time.Duration) []string {
	return discover(ctx, ssdp, timeout)
}

func discover(ctx context.Context, group *net.UDPAddr, timeout time.Duration) []string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var mu sync.Mutex
	var found []string
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, target := range Targets {
		wg.Go(func() {
			search(ctx, group, target, timeout, func(location string) {
				mu.Lock()
				defer mu.Unlock()
				if !seen[location] {
					seen[location] = true
					found = append(found, location)
				}
			})
		})
	}
	wg.Wait()
	return found
}

var locationHeader = regexp.MustCompile(`(?im)^location:[ \t]*(\S+)`)

func search(ctx context.Context, group *net.UDPAddr, target string, timeout time.Duration, found func(string)) {
	socket, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return
	}
	// Closed when the search is over, which is also what ends the read
	// below: a socket that hears nothing would otherwise wait forever.
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	defer socket.Close()

	// Seconds the responder may wait before replying, so that a busy network
	// does not answer all at once. Kept below our own timeout.
	mx := max(1, int(timeout/time.Second)-1)
	message := fmt.Sprintf("M-SEARCH * HTTP/1.1\r\n"+
		"HOST: %s\r\n"+
		"MAN: \"ssdp:discover\"\r\n"+
		"MX: %s\r\n"+
		"ST: %s\r\n\r\n", group, strconv.Itoa(mx), target)
	if _, err := socket.WriteToUDP([]byte(message), group); err != nil {
		return
	}

	buffer := make([]byte, 8192)
	for {
		n, _, err := socket.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if match := locationHeader.FindSubmatch(buffer[:n]); match != nil {
			found(string(match[1]))
		}
	}
}

// AddressFacingTimeout bounds AddressFacing by default.
const AddressFacingTimeout = time.Second

// AddressFacing is which of this host's addresses the gateway would see, or
// "" when that cannot be worked out.
//
// Needed because a bridge or host networked container, and every systemd
// unit, is forwarded to *this host*, so the mapping has to name the host's
// address on the LAN. v1 never had to work this out: it let upnpc infer the
// address from the source of the connection.
//
// Connecting a UDP socket sends nothing - it only asks the kernel to pick the
// route, and with it the source address it would use to reach that host. It
// answers at once or not at all, so it is bounded, rather than left to hold a
// socket open for the life of the process on a kernel that does neither.
func AddressFacing(host string, timeout time.Duration) string {
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.Dial("udp4", net.JoinHostPort(host, strconv.Itoa(ssdp.Port)))
	if err != nil {
		return ""
	}
	defer connection.Close()
	local, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP.IsUnspecified() {
		return ""
	}
	return local.IP.String()
}
