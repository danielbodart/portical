package systemd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/rules"
)

// Listener is a socket waiting for traffic: a TCP socket in LISTEN, or a
// bound, unconnected UDP socket.
type Listener struct {
	Protocol model.Protocol
	Address  netip.Addr
	Port     int
	// The interface the socket is bound to, by index - SO_BINDTODEVICE, or a
	// link-local address's scope - or 0 for none.
	Interface int
	// An IPv6 socket that does not also take IPv4.
	V6Only bool
	// The cgroup of whoever created the socket, by id, or 0 when the kernel
	// does not say (before Linux 5.7).
	CgroupID uint64
	// The same cgroup by path, such as /system.slice/sshd.service, or "" when
	// it is not known.
	Cgroup string
}

// SocketLister lists the host's listening sockets.
//
// When the sockets were listed but which cgroup each came from cannot be told,
// it returns them with Cgroup "" together with an error wrapping
// ErrUnknownCgroups: the pass goes on without `published` rather than failing,
// as only failing to list the sockets at all should.
type SocketLister interface {
	Listening(ctx context.Context) ([]Listener, error)
}

// ErrUnknownCgroups is the sockets having been listed without being able to
// say which unit owns each.
var ErrUnknownCgroups = errors.New("'published' cannot tell which unit owns a socket")

// SocketListerFunc is a function as a SocketLister.
type SocketListerFunc func(ctx context.Context) ([]Listener, error)

func (f SocketListerFunc) Listening(ctx context.Context) ([]Listener, error) { return f(ctx) }

type sockets struct {
	listeners []Listener
	low, high int
}

// listening is the host's sockets, read once for every unit in a pass that
// asks for `published`.
func (s *Source) listening(ctx context.Context, warnings *[]string) (*sockets, error) {
	listeners, listed := s.sockets.Listening(ctx)
	unknown := errors.Is(listed, ErrUnknownCgroups)
	if listed != nil && !unknown {
		return nil, fmt.Errorf("listing listening sockets: %w", listed)
	}
	low, high, err := s.ephemeral()
	if err != nil {
		return nil, err
	}
	if unknown {
		*warnings = append(*warnings, listed.Error()+" and forwards nothing")
	} else if len(listeners) > 0 && !anyCgroup(listeners) {
		*warnings = append(*warnings, "the kernel reports no cgroup for any socket, so 'published' cannot tell "+
			"which unit listens where and forwards nothing. It needs Linux 5.7 or later.")
	}
	return &sockets{listeners: listeners, low: low, high: high}, nil
}

func anyCgroup(listeners []Listener) bool {
	for _, listener := range listeners {
		if listener.CgroupID != 0 {
			return true
		}
	}
	return false
}

// published is the rules `published` stands for: what the unit is listening
// on, counting the sockets of a .socket unit that activates it.
func (s *Source) published(ctx context.Context, unit Unit, open *sockets) ([]model.Rule, error) {
	var groups []string
	for _, name := range append([]string{unit.Name}, unit.TriggeredBy...) {
		group, err := s.bus.ControlGroup(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if group != "" {
			groups = append(groups, group)
		}
	}
	return listening(open, groups, s.options.HostAddress), nil
}

// listening is the rules for the sockets in any of these cgroups, or beneath
// one.
//
// Beneath, because a service may put its workers in sub-cgroups of its own.
// UDP sockets on ephemeral ports are left out: every unconnected UDP socket is
// listed, and a service's client sockets - a resolver, a game server
// registering with a master list - sit there and must not be forwarded.
func listening(open *sockets, groups []string, hostAddress string) []model.Rule {
	host, _ := netip.ParseAddr(hostAddress)
	var found []model.Rule

	for _, listener := range open.listeners {
		if !within(listener.Cgroup, groups) || !reachable(listener, host) {
			continue
		}
		if listener.Protocol == model.UDP && listener.Port >= open.low && listener.Port <= open.high {
			continue
		}
		found = append(found, model.Rule{ExternalPort: listener.Port, InternalPort: listener.Port, Protocol: listener.Protocol})
	}

	return rules.Collapse(found)
}

func within(cgroup string, groups []string) bool {
	if cgroup == "" {
		return false
	}
	for _, group := range groups {
		if cgroup == group || strings.HasPrefix(cgroup, strings.TrimSuffix(group, "/")+"/") {
			return true
		}
	}
	return false
}

// reachable is whether the gateway's traffic could reach this socket.
//
// Wildcards, and the host's own address. An IPv6 wildcard counts unless the
// socket is v6only, because otherwise it takes IPv4 as well, which is Linux's
// default. Loopback, other specific addresses and anything bound to an
// interface (127.0.0.53%lo) cannot be what a forward is for.
func reachable(listener Listener, host netip.Addr) bool {
	address := listener.Address
	switch {
	case listener.Interface != 0 || address.Zone() != "":
		return false
	case address.Is4() && address.IsUnspecified():
		return true
	case address.IsUnspecified():
		return !listener.V6Only
	}
	return host.IsValid() && address.Unmap() == host.Unmap()
}

// The kernel's own default, for a host that will not say.
const (
	defaultEphemeralLow  = 32768
	defaultEphemeralHigh = 60999
)

// EphemeralPorts is the kernel's range for ports it picks itself, inclusive,
// from /proc/sys/net/ipv4/ip_local_port_range, or the kernel's default where
// that cannot be read.
func EphemeralPorts() (low, high int, err error) {
	return ephemeralPorts(os.DirFS("/proc/sys/net/ipv4"))
}

func ephemeralPorts(proc fs.FS) (low, high int, err error) {
	data, err := fs.ReadFile(proc, "ip_local_port_range")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return defaultEphemeralLow, defaultEphemeralHigh, nil
	}
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 2 {
		low, lowErr := strconv.Atoi(fields[0])
		high, highErr := strconv.Atoi(fields[1])
		if lowErr == nil && highErr == nil && low >= 1 && low <= high && high <= rules.MaxPort {
			return low, high, nil
		}
	}
	return 0, 0, fmt.Errorf("cannot read ip_local_port_range '%s'", strings.TrimSpace(string(data)))
}
