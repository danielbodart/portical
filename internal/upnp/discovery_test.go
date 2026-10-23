package upnp

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A gateway answering on loopback in place of the multicast group, so the
// search and the reading of its answers run for real without a router.
func TestDiscoverCollectsEachLocationOnce(t *testing.T) {
	responder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	searches := make(chan string, 2)
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, from, err := responder.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			searches <- string(buffer[:n])
			// Both targets answered with the same device, as a real gateway
			// that is both versions of IGD would.
			responder.WriteToUDP([]byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\n"+
				"Location: http://192.168.1.1:5000/rootDesc.xml\r\nST: x\r\n\r\n"), from)
		}
	}()

	found := discover(context.Background(), responder.LocalAddr().(*net.UDPAddr), 300*time.Millisecond)

	if want := []string{"http://192.168.1.1:5000/rootDesc.xml"}; !reflect.DeepEqual(found, want) {
		t.Errorf("got %q", found)
	}
	search := <-searches
	for _, want := range []string{"M-SEARCH * HTTP/1.1\r\n", `MAN: "ssdp:discover"`, "MX: 1\r\n", "ST: urn:schemas-upnp-org:device:InternetGatewayDevice:"} {
		if !strings.Contains(search, want) {
			t.Errorf("search %q lacks %q", search, want)
		}
	}
}

func TestDiscoverFindsNothingWhereNothingAnswers(t *testing.T) {
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if found := discover(context.Background(), silent.LocalAddr().(*net.UDPAddr), 50*time.Millisecond); len(found) != 0 {
		t.Errorf("got %q", found)
	}
}

func TestAddressFacingIsTheSourceAddressTheKernelWouldUse(t *testing.T) {
	if got := AddressFacing("127.0.0.1", AddressFacingTimeout); got != "127.0.0.1" {
		t.Errorf("got %q", got)
	}
}

// An IPv6 address, which no IPv4 route reaches, rather than a name that
// would need a resolver.
func TestAddressFacingSaysNothingForAnAddressItCannotRoute(t *testing.T) {
	if got := AddressFacing("::1", AddressFacingTimeout); got != "" {
		t.Errorf("got %q", got)
	}
}
