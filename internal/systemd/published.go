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

	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/rules"
)

type sockets struct {
	listeners []sockdiag.Listener
	low, high int
}

// listening is the host's sockets, read once for every unit in a pass that
// asks for `published`.
//
// Sockets listed without their cgroups (sockdiag.ErrUnknownCgroups) are not a
// failure: the pass goes on without `published`, and says why, as only failing
// to list the sockets at all should stop it.
func (s *Source) listening(ctx context.Context, warnings *[]string) (*sockets, error) {
	listeners, listed := s.sockets.Listening(ctx)
	unknown := errors.Is(listed, sockdiag.ErrUnknownCgroups)
	if listed != nil && !unknown {
		return nil, fmt.Errorf("listing listening sockets: %w", listed)
	}
	low, high, err := s.ephemeral()
	if err != nil {
		return nil, err
	}
	if unknown {
		*warnings = append(*warnings, "'published' cannot tell which unit owns a socket and forwards nothing: "+listed.Error())
	}
	return &sockets{listeners: listeners, low: low, high: high}, nil
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
		// A unit with no cgroup, "", claims nothing: sockdiag.Listener.Within
		// never matches an empty group.
		groups = append(groups, group)
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
		if !listener.Within(groups...) || !reachable(listener, host) {
			continue
		}
		if listener.Protocol == sockdiag.UDP && listener.Port >= open.low && listener.Port <= open.high {
			continue
		}
		found = append(found, model.Rule{ExternalPort: listener.Port, InternalPort: listener.Port, Protocol: model.Protocol(listener.Protocol)})
	}

	return rules.Collapse(found)
}

// reachable is whether the gateway's traffic could reach this socket.
//
// Wildcards, and the host's own address. An IPv6 wildcard counts unless the
// socket is v6only, because otherwise it takes IPv4 as well, which is Linux's
// default. Loopback, other specific addresses and anything bound to an
// interface (127.0.0.53%lo) cannot be what a forward is for.
func reachable(listener sockdiag.Listener, host netip.Addr) bool {
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
