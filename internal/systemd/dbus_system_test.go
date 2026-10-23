package systemd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// systemBus is the host's real system bus, or a skip where there is none -
// a container, a sandbox, a machine without systemd - or where
// PORTICAL_SKIP_SYSTEM_BUS says not to use it.
func systemBus(t *testing.T) *DBus {
	t.Helper()
	if os.Getenv("PORTICAL_SKIP_SYSTEM_BUS") != "" {
		t.Skip("PORTICAL_SKIP_SYSTEM_BUS is set")
	}
	env := map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")}
	found, err := SystemBus(env)
	if err != nil {
		t.Skip(err)
	}
	bus := found.(*DBus)
	if _, err := bus.conn(); err != nil {
		t.Skip("no system bus here: ", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

// Read-only: listing units, reading cgroups and subscribing change nothing on
// the host.
func TestSystemBusListsTheHostsRunningUnits(t *testing.T) {
	bus := systemBus(t)
	units, err := bus.Units(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var services []Unit
	for _, unit := range units {
		if strings.HasSuffix(unit.Name, ".service") && unit.FragmentPath != "" {
			services = append(services, unit)
		}
	}
	if len(services) == 0 {
		t.Fatalf("no running service with a unit file among %d units", len(units))
	}

	// Not every active service has a cgroup: a oneshot that stays active after
	// exiting has no processes left to hold one. But dbus itself is running,
	// or this could not have asked, so at least one does.
	for _, service := range services {
		group, err := bus.ControlGroup(ctx, service.Name)
		if err != nil {
			t.Fatal(err)
		}
		if group != "" {
			if !strings.HasPrefix(group, "/") {
				t.Errorf("%s's cgroup is %q", service.Name, group)
			}
			return
		}
	}
	t.Errorf("none of %d running services has a cgroup", len(services))
}

func TestSystemBusSubscribesUntilCancelled(t *testing.T) {
	bus := systemBus(t)
	watching, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if err := bus.Watch(watching, func(Change) {}); err != nil {
		t.Fatal(err)
	}
}
