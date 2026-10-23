package systemd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	systemd         = "org.freedesktop.systemd1"
	managerPath     = dbus.ObjectPath("/org/freedesktop/systemd1")
	manager         = "org.freedesktop.systemd1.Manager"
	unitIface       = "org.freedesktop.systemd1.Unit"
	propertiesIface = "org.freedesktop.DBus.Properties"
	unitPaths       = "/org/freedesktop/systemd1/unit"

	// DefaultSystemBus is where the system bus is when nothing says otherwise.
	DefaultSystemBus = "unix:path=/run/dbus/system_bus_socket"
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
	"type='signal',sender='" + systemd + "',interface='" + propertiesIface + "',member='PropertiesChanged'," +
		"path_namespace='" + unitPaths + "',arg0='" + unitIface + "'",
	"type='signal',sender='" + systemd + "',interface='" + manager + "',member='Reloading'",
}

// SystemBus is systemd over the system bus at DBUS_SYSTEM_BUS_ADDRESS, the
// variable libdbus and sd-bus read, or the standard socket.
//
// Only unix: addresses are taken, which is all a system bus has ever been:
// anything else is far more likely a mistake than a bus. Nothing is connected
// until the first call.
func SystemBus(env map[string]string) (Bus, error) {
	address := env["DBUS_SYSTEM_BUS_ADDRESS"]
	if address == "" {
		address = DefaultSystemBus
	}
	var usable []string
	for _, candidate := range strings.Split(address, ";") {
		if strings.HasPrefix(candidate, "unix:") {
			usable = append(usable, candidate)
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("cannot use DBUS_SYSTEM_BUS_ADDRESS '%s': only unix: addresses are supported", address)
	}
	return &DBus{address: strings.Join(usable, ";")}, nil
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

func (b *DBus) connect(options ...dbus.ConnOption) (*dbus.Conn, error) {
	// In order, so a test can see a unit's changes in the order systemd sent
	// them; the default handler hands a busy reader's signals to goroutines.
	options = append(options, dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	conn, err := dbus.Connect(b.address, options...)
	if err != nil {
		return nil, fmt.Errorf("D-Bus at %s: %w", b.address, err)
	}
	return conn, nil
}

func (b *DBus) conn() (*dbus.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shared != nil && b.shared.Connected() {
		return b.shared, nil
	}
	conn, err := b.connect()
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
	err = conn.Object(systemd, managerPath).CallWithContext(ctx, manager+".ListUnitsByPatterns", 0,
		[]string{"active", "reloading", "refreshing"}, []string{}).Store(&listed)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", describe(err))
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
	err := conn.Object(systemd, entry.Path).CallWithContext(ctx, propertiesIface+".GetAll", 0, unitIface).Store(&all)
	if err != nil {
		return Unit{}, fmt.Errorf("%s: %w", entry.Name, describe(err))
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
	err = conn.Object(systemd, unitPath(unit)).CallWithContext(ctx, propertiesIface+".Get", 0,
		systemd+"."+kind, "ControlGroup").Store(&group)
	if err != nil {
		return "", fmt.Errorf("reading %s's cgroup: %w", unit, describe(err))
	}
	path, _ := group.Value().(string)
	return path, nil
}

func (b *DBus) Watch(ctx context.Context, notify func(Change)) error {
	conn, err := b.connect()
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

	bus := conn.BusObject()
	for _, rule := range matches {
		if err := bus.CallWithContext(ctx, "org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
			return ended(ctx, fmt.Errorf("asking for systemd's signals: %w", describe(err)))
		}
	}
	// Without this systemd sends no unit signals at all. Any user may ask,
	// and it lasts as long as this connection does.
	if err := conn.Object(systemd, managerPath).CallWithContext(ctx, manager+".Subscribe", 0).Err; err != nil {
		return ended(ctx, fmt.Errorf("subscribing to systemd: %w", describe(err)))
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

// describe adds the error's name to a D-Bus error, whose own text is only its
// message - "Unit x.service not loaded." says less without
// org.freedesktop.systemd1.NoSuchUnit.
func describe(err error) error {
	var named dbus.Error
	if errors.As(err, &named) {
		return fmt.Errorf("%s: %w", named.Name, err)
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
	switch {

	case signal.Name == propertiesIface+".PropertiesChanged" && strings.HasPrefix(string(signal.Path), unitPaths+"/"):
		iface, _ := first[string](signal.Body, 0)
		if iface != unitIface {
			return Change{}, false
		}
		changed, _ := first[map[string]dbus.Variant](signal.Body, 1)
		invalidated, _ := first[[]string](signal.Body, 2)
		state, present := changed["ActiveState"]
		if !present && !contains(invalidated, "ActiveState") {
			return Change{}, false
		}
		value, _ := state.Value().(string)
		return Change{Unit: unitName(string(signal.Path)[len(unitPaths)+1:]), State: value}, true
	}
	return Change{}, false
}

// reloadSignal is whether a signal is systemd's Reloading, and if so whether
// the reload is starting rather than done.
func reloadSignal(signal *dbus.Signal) (starting, ok bool) {
	if signal.Name != manager+".Reloading" {
		return false, false
	}
	return first[bool](signal.Body, 0)
}

func first[T any](body []any, index int) (T, bool) {
	var zero T
	if index >= len(body) {
		return zero, false
	}
	value, ok := body[index].(T)
	return value, ok
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// unitPath is a unit's object path, escaped as systemd escapes it: every
// byte that is not a letter or digit, and a leading digit, becomes `_` and
// two hex digits.
func unitPath(name string) dbus.ObjectPath {
	var escaped strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			escaped.WriteByte(c)
		} else {
			fmt.Fprintf(&escaped, "_%02x", c)
		}
	}
	return dbus.ObjectPath(unitPaths + "/" + escaped.String())
}

// unitName is the reverse of unitPath, for the last element of the path.
func unitName(escaped string) string {
	var name []byte
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '_' && i+2 < len(escaped) && hex(escaped[i+1]) && hex(escaped[i+2]) {
			name = append(name, unhex(escaped[i+1])<<4|unhex(escaped[i+2]))
			i += 2
			continue
		}
		name = append(name, escaped[i])
	}
	return string(name)
}

func hex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}
