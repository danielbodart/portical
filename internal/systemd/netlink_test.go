package systemd

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/danielbodart/portical/internal/model"
)

// diag is an inet_diag_msg as the kernel writes one, followed by attributes.
func diag(family byte, address netip.Addr, port uint16, iface uint32, attributes ...[]byte) []byte {
	message := make([]byte, inetDiagMsgLength)
	message[0] = family
	message[1] = tcpListen
	binary.BigEndian.PutUint16(message[4:], port)
	raw := address.As16()
	if address.Is4() {
		four := address.As4()
		copy(raw[:], four[:])
		clear(raw[4:])
	}
	copy(message[8:24], raw[:])
	binary.NativeEndian.PutUint32(message[40:], iface)
	for _, attribute := range attributes {
		message = append(message, attribute...)
	}
	return message
}

func attribute(kind uint16, payload []byte) []byte {
	length := syscall.SizeofRtAttr + len(payload)
	out := make([]byte, (length+3)&^3)
	binary.NativeEndian.PutUint16(out[0:], uint16(length))
	binary.NativeEndian.PutUint16(out[2:], kind)
	copy(out[syscall.SizeofRtAttr:], payload)
	return out
}

func u64(value uint64) []byte { return binary.NativeEndian.AppendUint64(nil, value) }

func TestParseDiagReadsAnIPv4ListenerAndItsCgroup(t *testing.T) {
	listener, err := parseDiag(diag(syscall.AF_INET, netip.MustParseAddr("0.0.0.0"), 22, 0,
		attribute(inetDiagCgroupID, u64(1234))), model.TCP)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, listener, Listener{Protocol: model.TCP, Address: netip.MustParseAddr("0.0.0.0"), Port: 22, CgroupID: 1234})
}

func TestParseDiagReadsAnIPv6ListenerBoundToAnInterface(t *testing.T) {
	address := netip.MustParseAddr("fe80::c8bb:f868:b170:f12e")
	listener, err := parseDiag(diag(syscall.AF_INET6, address, 546, 2,
		attribute(inetDiagSkV6Only, []byte{1}),
		attribute(4 /* INET_DIAG_CONG, not read */, []byte("cubic\x00")),
		attribute(inetDiagCgroupID, u64(99))), model.UDP)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, listener, Listener{Protocol: model.UDP, Address: address, Port: 546, Interface: 2, V6Only: true, CgroupID: 99})
}

func TestParseDiagLeavesTheCgroupUnknownWhereTheKernelDoesNotSay(t *testing.T) {
	listener, err := parseDiag(diag(syscall.AF_INET, netip.MustParseAddr("127.0.0.1"), 631, 0), model.TCP)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, listener.CgroupID, uint64(0))
}

func TestParseDiagRefusesWhatItCannotRead(t *testing.T) {
	if _, err := parseDiag(make([]byte, 10), model.TCP); err == nil {
		t.Error("want an error for a short message")
	}
	broken := append(diag(syscall.AF_INET, netip.MustParseAddr("0.0.0.0"), 1, 0), 200, 0, 21, 0)
	if _, err := parseDiag(broken, model.TCP); err == nil {
		t.Error("want an error for an attribute longer than the message")
	}
}

func TestRequestAsksForOneFamilyProtocolAndState(t *testing.T) {
	message := request(7, query{model.UDP, syscall.AF_INET6, syscall.IPPROTO_UDP, tcpClose})
	order := binary.NativeEndian
	assertEqual(t, len(message), 72)
	assertEqual(t, order.Uint32(message[0:]), uint32(72))
	assertEqual(t, order.Uint16(message[4:]), uint16(sockDiagByFamily))
	assertEqual(t, order.Uint16(message[6:]), uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP))
	assertEqual(t, order.Uint32(message[8:]), uint32(7))
	assertEqual(t, message[16:18], []byte{syscall.AF_INET6, syscall.IPPROTO_UDP})
	assertEqual(t, order.Uint32(message[20:]), uint32(1<<7))
}

func TestCgroupPathsRefusesADirectoryThatIsNotCgroup2(t *testing.T) {
	if _, err := cgroupPaths(t.TempDir()); err == nil || !strings.Contains(err.Error(), "cgroup2") {
		t.Fatalf("got %v", err)
	}
}

// A 32-bit kernel's cgroup id is the inode with a generation above it, so
// it is found by its low half when the whole does not match.
func TestCgroupPathFindsA32BitKernelsIdByItsInode(t *testing.T) {
	paths := map[uint64]string{1: "/", 4242: "/system.slice/sshd.service"}
	assertEqual(t, cgroupPath(paths, 4242), "/system.slice/sshd.service")
	assertEqual(t, cgroupPath(paths, 1<<32|4242), "/system.slice/sshd.service")
	assertEqual(t, cgroupPath(paths, 1<<32|7), "")
	assertEqual(t, cgroupPath(paths, 0), "")
}

// ownCgroup is this process's cgroup, as the kernel gives it for cgroup2.
func ownCgroup(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("no /proc/self/cgroup:", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	t.Skip("not on cgroup2")
	return ""
}

func find(listeners []Listener, protocol model.Protocol, address netip.AddrPort) (Listener, bool) {
	for _, listener := range listeners {
		if listener.Protocol == protocol && listener.Port == int(address.Port()) && listener.Address == address.Addr() {
			return listener, true
		}
	}
	return Listener{}, false
}

// The real kernel: sockets this test opens, found with this process's cgroup.
// On loopback, so nothing is reachable from anywhere else for the moment the
// test runs.
func TestKernelFindsThisProcesssListenersWithItsCgroup(t *testing.T) {
	cgroup := ownCgroup(t)

	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	// Go makes "tcp6" sockets v6only.
	tcp6, err := net.Listen("tcp6", "[::1]:0")
	if err == nil {
		defer tcp6.Close()
	}

	listeners, err := Kernel().Listening(ctx)
	if errors.Is(err, ErrUnknownCgroups) {
		// Nix's build sandbox, where the tests also run, mounts no /sys:
		// the sockets are there but no cgroupfs to name their cgroups in.
		// What the lister does then is the next test but one.
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}

	found, ok := find(listeners, model.TCP, tcp.Addr().(*net.TCPAddr).AddrPort())
	if !ok {
		t.Fatalf("no TCP listener on %s", tcp.Addr())
	}
	if found.CgroupID == 0 {
		t.Skip("this kernel reports no cgroup ids (before 5.7)")
	}
	assertEqual(t, found.Cgroup, cgroup)
	assertEqual(t, found.V6Only, false)
	assertEqual(t, found.Interface, 0)

	found, ok = find(listeners, model.UDP, udp.LocalAddr().(*net.UDPAddr).AddrPort())
	if !ok {
		t.Fatalf("no UDP socket on %s", udp.LocalAddr())
	}
	assertEqual(t, found.Cgroup, cgroup)

	if tcp6 != nil {
		found, ok = find(listeners, model.TCP, tcp6.Addr().(*net.TCPAddr).AddrPort())
		if !ok {
			t.Fatalf("no TCP listener on %s", tcp6.Addr())
		}
		assertEqual(t, found.V6Only, true)
		assertEqual(t, found.Cgroup, cgroup)
	}
}

// A connected socket is not listening, whatever its port.
func TestKernelLeavesOutConnectedSockets(t *testing.T) {
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.Dial("tcp4", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	connected, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	defer connected.Close()

	listeners, err := Kernel().Listening(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []net.Addr{client.LocalAddr(), connected.LocalAddr()} {
		port := netip.MustParseAddrPort(address.String())
		for _, protocol := range model.Protocols {
			if _, ok := find(listeners, protocol, port); ok {
				t.Errorf("listed %s %s", protocol, address)
			}
		}
	}
}

// A host without a cgroup2 hierarchy - cgroup v1 alone, or a sandbox with no
// cgroupfs - still has its sockets listed, just without saying whose they are,
// so that only `published` goes without rather than the whole pass.
func TestKernelWithoutCgroup2StillListsTheSockets(t *testing.T) {
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	listeners, err := kernel{cgroupRoot: t.TempDir()}.Listening(ctx)
	found, ok := find(listeners, model.TCP, tcp.Addr().(*net.TCPAddr).AddrPort())
	if !ok {
		t.Fatalf("no TCP listener on %s (error %v)", tcp.Addr(), err)
	}
	if found.CgroupID == 0 {
		t.Skip("this kernel reports no cgroup ids (before 5.7)")
	}
	if !errors.Is(err, ErrUnknownCgroups) || !strings.Contains(err.Error(), "no cgroup2 file system at") {
		t.Fatalf("got %v", err)
	}
	assertEqual(t, found.Cgroup, "")
}
