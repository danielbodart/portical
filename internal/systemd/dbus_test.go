package systemd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/dbusx"
	"github.com/danielbodart/gokit/dbusx/dbustest"
	"github.com/godbus/dbus/v5"
)

// fakeBus is a dbustest bus, and the adapter talking to it: Portical's
// *DBus, found through SystemBus as DBUS_SYSTEM_BUS_ADDRESS would point it
// there, and closed when the test ends.
func fakeBus(t *testing.T) (*dbustest.Bus, *DBus) {
	t.Helper()
	fake := dbustest.New(t)
	found, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": fake.Address()})
	if err != nil {
		t.Fatal(err)
	}
	bus := found.(*DBus)
	t.Cleanup(func() { bus.Close() })
	return fake, bus
}

func propertiesChanged(path dbus.ObjectPath, iface string, changed map[string]dbus.Variant, invalidated ...string) *dbus.Signal {
	return &dbus.Signal{
		Path: path,
		Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
		Body: []any{iface, changed, append([]string{}, invalidated...)},
	}
}

const sshd = dbus.ObjectPath("/org/freedesktop/systemd1/unit/sshd_2eservice")

func TestToChangeReadsAUnitsActiveStateChanging(t *testing.T) {
	change, ok := toChange(propertiesChanged(sshd, dbusx.SystemdUnit, map[string]dbus.Variant{
		"ActiveState": dbus.MakeVariant("active"),
		"SubState":    dbus.MakeVariant("running"),
	}))
	assertEqual(t, ok, true)
	assertEqual(t, change, Change{Unit: "sshd.service", State: "active"})
}

func TestToChangeTakesActiveStateBeingInvalidatedAsAChangeToSomewhereUnknown(t *testing.T) {
	change, ok := toChange(propertiesChanged(sshd, dbusx.SystemdUnit, map[string]dbus.Variant{}, "ActiveState"))
	assertEqual(t, ok, true)
	assertEqual(t, change, Change{Unit: "sshd.service"})
}

func TestToChangeIgnoresAChangeThatLeavesActiveStateAlone(t *testing.T) {
	_, ok := toChange(propertiesChanged(sshd, dbusx.SystemdUnit, map[string]dbus.Variant{"SubState": dbus.MakeVariant("running")}))
	assertEqual(t, ok, false)
	_, ok = toChange(propertiesChanged(sshd, "org.freedesktop.systemd1.Service", map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(1))}))
	assertEqual(t, ok, false)
	_, ok = toChange(propertiesChanged("/org/freedesktop/systemd1", dbusx.SystemdUnit, map[string]dbus.Variant{"ActiveState": dbus.MakeVariant("active")}))
	assertEqual(t, ok, false)
}

func TestToChangeTakesAReloadFinishingButNotStarting(t *testing.T) {
	reloading := func(starting bool) *dbus.Signal {
		return &dbus.Signal{Path: dbusx.SystemdPath, Name: "org.freedesktop.systemd1.Manager.Reloading", Body: []any{starting}}
	}
	change, ok := toChange(reloading(false))
	assertEqual(t, ok, true)
	assertEqual(t, change, Change{})
	_, ok = toChange(reloading(true))
	assertEqual(t, ok, false)
	_, ok = toChange(&dbus.Signal{Path: dbusx.SystemdPath, Name: "org.freedesktop.systemd1.Manager.Reloading"})
	assertEqual(t, ok, false)
}

func TestSaysWhereItWasGoingWhenThereIsNoBus(t *testing.T) {
	bus, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": "unix:path=/nonexistent/bus"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Units(ctx); err == nil || !strings.Contains(err.Error(), "/nonexistent/bus") {
		t.Fatalf("got %v", err)
	}
}

// listed is one of ListUnitsByPatterns' a(ssssssouso), written out apart
// from the adapter's own so the two cannot agree by sharing a mistake.
type listed struct {
	Name, Description, Load, Active, Sub, Following string
	Path                                            dbus.ObjectPath
	Job                                             uint32
	JobType                                         string
	JobPath                                         dbus.ObjectPath
}

func running(name string) listed {
	return listed{name, "", "loaded", "active", "running", "", dbusx.UnitPath(name), 0, "", "/"}
}

// systemdAnswers is systemd's side of the conversation, for sshd alone.
func systemdAnswers(call *dbus.Message) dbustest.Reply {
	switch dbustest.Header[string](call, dbus.FieldMember) {
	case "ListUnitsByPatterns":
		return dbustest.Reply{Body: []any{[]listed{running("sshd.service")}}}
	case "GetAll":
		return dbustest.Reply{Body: []any{map[string]dbus.Variant{
			"Id":                   dbus.MakeVariant("sshd.service"),
			"FragmentPath":         dbus.MakeVariant("/etc/systemd/system/sshd.service"),
			"DropInPaths":          dbus.MakeVariant([]string{"/etc/systemd/system/sshd.service.d/forward.conf"}),
			"TriggeredBy":          dbus.MakeVariant([]string{}),
			"NeedDaemonReload":     dbus.MakeVariant(false),
			"ActiveEnterTimestamp": dbus.MakeVariant(uint64(1)),
		}}}
	case "Get":
		if call.Body[0] == "org.freedesktop.systemd1.Service" {
			return dbustest.Reply{Body: []any{dbus.MakeVariant("/system.slice/sshd.service")}}
		}
		return dbustest.Reply{Err: dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{"Unknown property"})}
	}
	return dbustest.Reply{}
}

func TestDBusListsActiveUnitsWithTheirFiles(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(systemdAnswers)
	units, err := bus.Units(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, units, []Unit{{
		Name:         "sshd.service",
		FragmentPath: "/etc/systemd/system/sshd.service",
		DropInPaths:  []string{"/etc/systemd/system/sshd.service.d/forward.conf"},
		TriggeredBy:  []string{},
	}})

	list := fake.Calls("ListUnitsByPatterns")[0]
	assertEqual(t, dbustest.Header[dbus.ObjectPath](list, dbus.FieldPath), dbusx.SystemdPath)
	assertEqual(t, list.Body, []any{[]string{"active", "reloading", "refreshing"}, []string{}})
	all := fake.Calls("GetAll")[0]
	assertEqual(t, dbustest.Header[dbus.ObjectPath](all, dbus.FieldPath), sshd)
	assertEqual(t, dbustest.Header[string](all, dbus.FieldInterface), "org.freedesktop.DBus.Properties")
	assertEqual(t, all.Body, []any{"org.freedesktop.systemd1.Unit"})
}

func TestDBusKeepsTheCallsItHasWaitingUnderDbusDaemonsLimitHoweverManyUnitsThereAre(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.MaxPending(128)
	var names []string
	var all []listed
	for i := range 300 {
		names = append(names, fmt.Sprintf("unit%d.service", i))
		all = append(all, running(names[i]))
	}
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		switch dbustest.Header[string](call, dbus.FieldMember) {
		case "ListUnitsByPatterns":
			return dbustest.Reply{Body: []any{all}}
		case "GetAll":
			time.Sleep(5 * time.Millisecond)
			return dbustest.Reply{Body: []any{map[string]dbus.Variant{
				"FragmentPath": dbus.MakeVariant("/etc/systemd/system/" + string(dbustest.Header[dbus.ObjectPath](call, dbus.FieldPath))),
			}}}
		}
		return dbustest.Reply{}
	})

	units, err := bus.Units(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, unit := range units {
		got = append(got, unit.Name)
	}
	assertEqual(t, got, names)
	if fake.MostPending() > InFlight {
		t.Errorf("%d calls waiting at once", fake.MostPending())
	}
}

func TestDBusFailsTheListWhenAnyUnitCannotBeRead(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		if dbustest.Header[string](call, dbus.FieldMember) == "GetAll" {
			return dbustest.Reply{Err: dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", []any{"not you"})}
		}
		return systemdAnswers(call)
	})
	_, err := bus.Units(ctx)
	if err == nil || !strings.Contains(err.Error(), "sshd.service: org.freedesktop.DBus.Error.AccessDenied: not you") {
		t.Fatalf("got %v", err)
	}
}

func TestDBusReadsAUnitsCgroupFromItsTypesInterface(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(systemdAnswers)
	group, err := bus.ControlGroup(ctx, "sshd.service")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, group, "/system.slice/sshd.service")
	get := fake.Calls("Get")[0]
	assertEqual(t, dbustest.Header[dbus.ObjectPath](get, dbus.FieldPath), sshd)
	assertEqual(t, get.Body, []any{"org.freedesktop.systemd1.Service", "ControlGroup"})

	group, err = bus.ControlGroup(ctx, "multi-user.target")
	assertEqual(t, group, "")
	assertEqual(t, err, nil)
	assertEqual(t, len(fake.Calls("Get")), 1)

	if _, err := bus.ControlGroup(ctx, "home.mount"); err == nil || !strings.Contains(err.Error(), "UnknownProperty") {
		t.Fatalf("got %v", err)
	}
}

func TestDBusReconnectsForTheNextQueryAfterTheBusDrops(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(systemdAnswers)
	if _, err := bus.Units(ctx); err != nil {
		t.Fatal(err)
	}
	fake.Disconnect()
	await.Until(t, "the connection to notice", func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return !bus.shared.Connected()
	})
	units, err := bus.Units(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(units), 1)
}

// watch runs Watch on its own goroutine, its changes on a channel.
func watch(t *testing.T, fake *dbustest.Bus, bus *DBus) (<-chan Change, <-chan error, context.CancelFunc) {
	t.Helper()
	watching, stop := context.WithCancel(ctx)
	changes, done := make(chan Change, 16), make(chan error, 1)
	go func() { done <- bus.Watch(watching, func(change Change) { changes <- change }) }()
	t.Cleanup(stop)
	await.Until(t, "the subscription", func() bool { return len(fake.Calls("Subscribe")) == 1 })
	return changes, done, stop
}

func next(t *testing.T, changes <-chan Change) Change {
	t.Helper()
	select {
	case change := <-changes:
		return change
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a change")
		return Change{}
	}
}

func TestDBusSubscribesAndTurnsSignalsIntoChangesUntilCancelled(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(systemdAnswers)
	changes, done, stop := watch(t, fake, bus)

	var rules []string
	for _, call := range fake.Calls("AddMatch") {
		rules = append(rules, call.Body[0].(string))
	}
	assertEqual(t, len(rules), 2)
	if !strings.Contains(rules[0], "member='PropertiesChanged'") || !strings.Contains(rules[0], "arg0='org.freedesktop.systemd1.Unit'") {
		t.Errorf("first rule %q", rules[0])
	}
	if !strings.Contains(rules[1], "member='Reloading'") {
		t.Errorf("second rule %q", rules[1])
	}
	subscribe := fake.Calls("Subscribe")[0]
	assertEqual(t, dbustest.Header[string](subscribe, dbus.FieldDestination), "org.freedesktop.systemd1")
	assertEqual(t, dbustest.Header[dbus.ObjectPath](subscribe, dbus.FieldPath), dbusx.SystemdPath)

	for _, state := range []string{"deactivating", "deactivating", "inactive"} {
		fake.Signal(sshd, "org.freedesktop.DBus.Properties", "PropertiesChanged",
			"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant(state)}, []string{})
	}
	fake.Signal(sshd, "org.freedesktop.DBus.Properties", "PropertiesChanged",
		"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"SubState": dbus.MakeVariant("dead")}, []string{})
	fake.Signal(dbusx.SystemdPath, "org.freedesktop.systemd1.Manager", "Reloading", true)
	fake.Signal(dbusx.SystemdPath, "org.freedesktop.systemd1.Manager", "Reloading", false)

	// Repeats and all: telling a move from a repeat is the source's job.
	assertEqual(t, next(t, changes), Change{Unit: "sshd.service", State: "deactivating"})
	assertEqual(t, next(t, changes), Change{Unit: "sshd.service", State: "deactivating"})
	assertEqual(t, next(t, changes), Change{Unit: "sshd.service", State: "inactive"})
	assertEqual(t, next(t, changes), Change{})

	stop()
	assertEqual(t, <-done, nil)
	// Its own connection, closed with it; queries never opened the shared one.
	await.Until(t, "the connection to close", func() bool { return fake.Connected() == 0 })
}

// A pass started mid-reload would find units not yet reloaded still marked as
// needing it, and warn about a reload already under way.
func TestDBusHoldsUnitChangesUntilAReloadIsDone(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(systemdAnswers)
	changes, done, stop := watch(t, fake, bus)

	fake.Signal(dbusx.SystemdPath, "org.freedesktop.systemd1.Manager", "Reloading", true)
	fake.Signal(sshd, "org.freedesktop.DBus.Properties", "PropertiesChanged",
		"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant("active")}, []string{})
	fake.Signal(dbusx.SystemdPath, "org.freedesktop.systemd1.Manager", "Reloading", false)
	fake.Signal(sshd, "org.freedesktop.DBus.Properties", "PropertiesChanged",
		"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant("inactive")}, []string{})

	assertEqual(t, next(t, changes), Change{})
	assertEqual(t, next(t, changes), Change{Unit: "sshd.service", State: "inactive"})

	stop()
	assertEqual(t, <-done, nil)
}

func TestDBusEndsTheWatchWithAnErrorWhenTheBusDrops(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(systemdAnswers)
	_, done, _ := watch(t, fake, bus)
	fake.Disconnect()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still watching")
	}
}

func TestDBusEndsTheWatchWhenSubscribingIsRefused(t *testing.T) {
	fake, bus := fakeBus(t)
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		if dbustest.Header[string](call, dbus.FieldMember) == "Subscribe" {
			return dbustest.Reply{Err: dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", []any{"no"})}
		}
		return dbustest.Reply{}
	})
	err := bus.Watch(ctx, func(Change) {})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("got %v", err)
	}
	if !slices.ContainsFunc(fake.Calls("AddMatch"), func(*dbus.Message) bool { return true }) {
		t.Error("asked for no signals")
	}
}
