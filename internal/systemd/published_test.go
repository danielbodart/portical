package systemd

import (
	"hash/fnv"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/portical/internal/model"
)

const host = "192.168.1.5"

// ss reads `ss -Hlntu -e` - the form Portical read before it asked the kernel
// itself, and the fixture was captured in - into what the kernel lister
// returns, so the development host's real sockets can still be the test data.
// The cgroup id is made up, the path is ss's.
func ss(t *testing.T, text string) []sockdiag.Listener {
	t.Helper()
	var listeners []sockdiag.Listener
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || (fields[0] != "tcp" && fields[0] != "udp") {
			continue
		}
		local := fields[4]
		colon := strings.LastIndex(local, ":")
		port, err := strconv.Atoi(local[colon+1:])
		if err != nil {
			t.Fatalf("bad port in %q", line)
		}
		address, scope, _ := strings.Cut(local[:colon], "%")
		address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
		if address == "*" {
			address = "::"
		}
		listener := sockdiag.Listener{Protocol: sockdiag.Protocol(fields[0]), Address: netip.MustParseAddr(address), Port: port}
		if scope != "" {
			listener.Interface = 1
		}
		for _, extra := range fields[5:] {
			if cgroup, ok := strings.CutPrefix(extra, "cgroup:"); ok {
				id := fnv.New64()
				id.Write([]byte(cgroup))
				listener.Cgroup, listener.CgroupID = cgroup, id.Sum64()
			}
			if extra == "v6only:1" {
				listener.V6Only = true
			}
		}
		listeners = append(listeners, listener)
	}
	return listeners
}

func developmentHost(t *testing.T) []sockdiag.Listener {
	t.Helper()
	text, err := os.ReadFile("testdata/ss-host.txt")
	if err != nil {
		t.Fatal(err)
	}
	return ss(t, string(text))
}

func open(listeners []sockdiag.Listener) *sockets {
	return &sockets{listeners: listeners, low: defaultEphemeralLow, high: defaultEphemeralHigh}
}

func rule(port int, protocol model.Protocol) model.Rule {
	return model.Rule{ExternalPort: port, InternalPort: port, Protocol: protocol}
}

func ports(rules []model.Rule) []int {
	var found []int
	for _, rule := range rules {
		found = append(found, rule.ExternalPort)
	}
	return found
}

func TestListeningFindsSshdsPortOnTheDevelopmentHostOnceForBothAddressFamilies(t *testing.T) {
	assertEqual(t, listening(open(developmentHost(t)), []string{"/system.slice/sshd.service"}, "10.0.0.219"),
		[]model.Rule{rule(22, model.TCP)})
}

func TestListeningLeavesOutLoopbackAndInterfaceScopedBinds(t *testing.T) {
	// resolved listens on 5355 everywhere, and on 53 only on loopback.
	assertEqual(t, listening(open(developmentHost(t)), []string{"/system.slice/systemd-resolved.service"}, "10.0.0.219"),
		[]model.Rule{rule(5355, model.UDP), rule(5355, model.TCP)})
}

func TestListeningTakesASocketBoundToTheHostsOwnAddress(t *testing.T) {
	listeners := ss(t, "tcp LISTEN 0 128 192.168.1.5:8080 0.0.0.0:* cgroup:/system.slice/web.service <->\n"+
		"tcp LISTEN 0 128 192.168.7.1:8081 0.0.0.0:* cgroup:/system.slice/web.service <->\n"+
		"tcp LISTEN 0 128 [::ffff:192.168.1.5]:8082 [::]:* cgroup:/system.slice/web.service <->\n")
	assertEqual(t, ports(listening(open(listeners), []string{"/system.slice/web.service"}, host)), []int{8080, 8082})
}

func TestListeningTakesADualStackIPv6WildcardButNotAV6OnlyOne(t *testing.T) {
	listeners := ss(t, "tcp LISTEN 0 128 *:25565 *:* cgroup:/system.slice/mc.service <->\n"+
		"tcp LISTEN 0 128 [::]:25575 [::]:* cgroup:/system.slice/mc.service <->\n"+
		"tcp LISTEN 0 128 [::]:25576 [::]:* cgroup:/system.slice/mc.service v6only:1 <->\n")
	assertEqual(t, ports(listening(open(listeners), []string{"/system.slice/mc.service"}, host)), []int{25565, 25575})
}

func TestListeningIncludesSocketsInTheUnitsSubCgroupsButNotASiblingsWithALongerName(t *testing.T) {
	listeners := ss(t, "tcp LISTEN 0 128 0.0.0.0:1 0.0.0.0:* cgroup:/system.slice/mc.service/worker <->\n"+
		"tcp LISTEN 0 128 0.0.0.0:2 0.0.0.0:* cgroup:/system.slice/mc.service2 <->\n")
	assertEqual(t, ports(listening(open(listeners), []string{"/system.slice/mc.service"}, host)), []int{1})
}

func TestListeningLeavesOutUDPOnEphemeralPortsWhichAreClientSockets(t *testing.T) {
	listeners := ss(t, "udp UNCONN 0 0 0.0.0.0:19132 0.0.0.0:* cgroup:/system.slice/mc.service <->\n"+
		"udp UNCONN 0 0 0.0.0.0:41234 0.0.0.0:* cgroup:/system.slice/mc.service <->\n"+
		"tcp LISTEN 0 128 0.0.0.0:41235 0.0.0.0:* cgroup:/system.slice/mc.service <->\n")
	assertEqual(t, listening(open(listeners), []string{"/system.slice/mc.service"}, host),
		[]model.Rule{rule(19132, model.UDP), rule(41235, model.TCP)})
}

func TestListeningMatchesNothingWithoutACgroup(t *testing.T) {
	listeners := ss(t, "tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* ino:1 sk:2 <->\n")
	assertEqual(t, listening(open(listeners), []string{"/"}, host), []model.Rule{})
}

func TestEphemeralPortsReadsTheKernelsRange(t *testing.T) {
	low, high, err := ephemeralPorts(fstest.MapFS{"ip_local_port_range": {Data: []byte("1024\t65000\n")}})
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, [2]int{low, high}, [2]int{1024, 65000})
}

func TestEphemeralPortsFallsBackToTheKernelsDefaultWhenThereIsNoRange(t *testing.T) {
	low, high, err := ephemeralPorts(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, [2]int{low, high}, [2]int{32768, 60999})
}

func TestEphemeralPortsRefusesARangeItCannotRead(t *testing.T) {
	for _, text := range []string{"", "32768", "32768 x", "0x8000 60999", "60999 32768", "1 70000"} {
		if _, _, err := ephemeralPorts(fstest.MapFS{"ip_local_port_range": {Data: []byte(text)}}); err == nil {
			t.Errorf("%q: want an error", text)
		}
	}
}

func TestEphemeralPortsOnThisHost(t *testing.T) {
	low, high, err := EphemeralPorts()
	if err != nil {
		t.Fatal(err)
	}
	if low < 1 || high < low {
		t.Fatalf("got %d-%d", low, high)
	}
}
