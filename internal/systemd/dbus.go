package systemd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/danielbodart/gokit/dbusx"
	"github.com/danielbodart/gokit/environ"
	"github.com/godbus/dbus/v5"
)

// cgroupInterfaces is the unit types that have a ControlGroup, and the
// interface that carries it.
var cgroupInterfaces = map[string]string{
	"service": "Service",
	"socket":  "Socket",
	"mount":   "Mount",
	"swap":    "Swap",
	"slice":   "Slice",
	"scope":   "Scope",
}

// InFlight is how many calls one connection has waiting on a reply at once:
// well under dbus-daemon's default max_replies_per_connection of 128, leaving
// room for anything else sharing the connection. dbus-broker has no such
// limit, but a host easily has more active units than that, and dbus-daemon
// refuses the lot.
const InFlight = 32

// matches are the signals Watch asks the bus for: a unit's properties
// changing, and systemd reloading.
var matches = []string{
	"type='signal',sender='" + dbusx.Systemd + "',interface='" + dbusx.Properties + "',member='PropertiesChanged'," +
		"path_namespace='" + dbusx.UnitPathPrefix + "',arg0='" + dbusx.SystemdUnit + "'",
	"type='signal',sender='" + dbusx.Systemd + "',interface='" + dbusx.SystemdManager + "',member='Reloading'",
}

// SystemBus is systemd over the system bus at DBUS_SYSTEM_BUS_ADDRESS, or the
// standard socket - as dbusx.SystemBusAddress finds it, unix: addresses only.
// Nothing is connected until the first call.
func SystemBus(env environ.Env) (Bus, error) {
	address, err := dbusx.SystemBusAddress(env)
	if err != nil {
		return nil, err
	}
	return &DBus{address: address}, nil
}

// DBus is systemd over D-Bus.
//
// Queries share one connection, opened when first needed and again after it
// drops. Watch has a connection of its own, because a subscription belongs to
// a connection and ends with it - which is also how it is cancelled.
type DBus struct {
	address string

	mu     sync.Mutex
	shared *dbus.Conn
}

var _ Bus = (*DBus)(nil)

func (b *DBus) conn() (*dbus.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shared != nil && b.shared.Connected() {
		return b.shared, nil
	}
	conn, err := dbusx.Connect(b.address)
	if err != nil {
		return nil, err
	}
	b.shared = conn
	return conn, nil
}

// Close closes the shared connection, if there is one. A Watch in progress
// has its own, closed when its context is.
func (b *DBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shared == nil {
		return nil
	}
	err := b.shared.Close()
	b.shared = nil
	return err
}

// listedUnit is one of ListUnitsByPatterns' a(ssssssouso).
type listedUnit struct {
	Name, Description, LoadState, ActiveState, SubState, Following string
	Path                                                           dbus.ObjectPath
	JobID                                                          uint32
	JobType                                                        string
	JobPath                                                        dbus.ObjectPath
}

func (b *DBus) Units(ctx context.Context) ([]Unit, error) {
	conn, err := b.conn()
	if err != nil {
		return nil, err
	}
	var listed []listedUnit
	// refreshing is a running unit having its mount namespace refreshed.
	err = conn.Object(dbusx.Systemd, dbusx.SystemdPath).CallWithContext(ctx, dbusx.SystemdManager+".ListUnitsByPatterns", 0,
		[]string{"active", "reloading", "refreshing"}, []string{}).Store(&listed)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", dbusx.Describe(err))
	}

	// Overlapping, as the connection matches replies by serial, but only so
	// many at a time.
	units := make([]Unit, len(listed))
	failures := make([]error, len(listed))
	limit := make(chan struct{}, InFlight)
	var wg sync.WaitGroup
	for i, entry := range listed {
		limit <- struct{}{}
		wg.Go(func() {
			defer func() { <-limit }()
			units[i], failures[i] = properties(ctx, conn, entry)
		})
	}
	wg.Wait()
	// The first in the list, so the error does not depend on which was quicker.
	for _, err := range failures {
		if err != nil {
			return nil, err
		}
	}
	return units, nil
}

func properties(ctx context.Context, conn *dbus.Conn, entry listedUnit) (Unit, error) {
	var all map[string]dbus.Variant
	err := conn.Object(dbusx.Systemd, entry.Path).CallWithContext(ctx, dbusx.Properties+".GetAll", 0, dbusx.SystemdUnit).Store(&all)
	if err != nil {
		return Unit{}, fmt.Errorf("%s: %w", entry.Name, dbusx.Describe(err))
	}
	unit := Unit{Name: entry.Name}
	// Missing or of another type reads as nothing, as an older systemd may
	// not have them all.
	unit.FragmentPath, _ = all["FragmentPath"].Value().(string)
	unit.DropInPaths, _ = all["DropInPaths"].Value().([]string)
	unit.TriggeredBy, _ = all["TriggeredBy"].Value().([]string)
	unit.NeedDaemonReload, _ = all["NeedDaemonReload"].Value().(bool)
	return unit, nil
}

func (b *DBus) ControlGroup(ctx context.Context, unit string) (string, error) {
	kind, ok := cgroupInterfaces[suffix(unit)]
	if !ok {
		return "", nil
	}
	conn, err := b.conn()
	if err != nil {
		return "", err
	}
	var group dbus.Variant
	err = conn.Object(dbusx.Systemd, dbusx.UnitPath(unit)).CallWithContext(ctx, dbusx.Properties+".Get", 0,
		dbusx.Systemd+"."+kind, "ControlGroup").Store(&group)
	if err != nil {
		return "", fmt.Errorf("reading %s's cgroup: %w", unit, dbusx.Describe(err))
	}
	path, _ := group.Value().(string)
	return path, nil
}

func (b *DBus) Watch(ctx context.Context, notify func(Change)) error {
	conn, err := dbusx.Connect(b.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Closing the connection is what stops it: the signal channel closes, and
	// any call waiting on the bus fails.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	signals := make(chan *dbus.Signal, 64)
	conn.Signal(signals)

	// The match rules, then Manager.Subscribe: both last as long as this
	// connection does.
	if err := dbusx.Subscribe(ctx, conn, matches...); err != nil {
		return ended(ctx, err)
	}

	// Mid-reload, systemd says things about units as it reloads them, and a
	// pass they started would read units it has yet to reach, still marked as
	// needing the reload. The reload finishing starts a pass that reads them
	// all afresh, so nothing is lost by waiting for it.
	reloading := false
	for signal := range signals {
		if starting, ok := reloadSignal(signal); ok {
			reloading = starting
		} else if reloading {
			continue
		}
		if change, ok := toChange(signal); ok {
			notify(change)
		}
	}
	return ended(ctx, errors.New("D-Bus connection closed"))
}

// ended is what Watch returns once its connection has gone: nothing if that
// was because ctx is done, otherwise why.
func ended(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// toChange is the change a signal stands for, if it is one worth a pass: a
// unit's ActiveState, or a reload finishing. Everything else a unit says
// about itself - and it says a lot - cannot change what it forwards.
func toChange(signal *dbus.Signal) (Change, bool) {
	if starting, ok := reloadSignal(signal); ok {
		// Sent true as the reload starts and false when it is done. Only the
		// second is worth acting on: before it, the old configuration is live.
		return Change{}, !starting
	}
	if signal.Name != dbusx.Properties+".PropertiesChanged" {
		return Change{}, false
	}
	unit, ok := dbusx.UnitName(signal.Path)
	if !ok {
		return Change{}, false
	}
	if iface, _ := dbusx.Arg[string](signal.Body, 0); iface != dbusx.SystemdUnit {
		return Change{}, false
	}
	changed, _ := dbusx.Arg[map[string]dbus.Variant](signal.Body, 1)
	invalidated, _ := dbusx.Arg[[]string](signal.Body, 2)
	state, present := changed["ActiveState"]
	if !present && !slices.Contains(invalidated, "ActiveState") {
		return Change{}, false
	}
	value, _ := state.Value().(string)
	return Change{Unit: unit, State: value}, true
}

// reloadSignal is whether a signal is systemd's Reloading, and if so whether
// the reload is starting rather than done.
func reloadSignal(signal *dbus.Signal) (starting, ok bool) {
	if signal.Name != dbusx.SystemdManager+".Reloading" {
		return false, false
	}
	return dbusx.Arg[bool](signal.Body, 0)
}
