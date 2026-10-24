package systemd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/logx/logtest"
	"github.com/danielbodart/portical/internal/daemon"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/upnp"
)

// portical is the daemon driven by systemd rather than Docker: the same
// reconcile, the same UPnP client, with a unit's lifecycle in place of a
// container's, and time stopped.
type portical struct {
	*fixture
	router *fakes.Gateway
	log    *logtest.Logger
	clock  *clock.Stopped
	daemon *daemon.Daemon
}

func newPortical(t *testing.T) *portical {
	t.Helper()
	p := &portical{
		fixture: newFixture(),
		router:  fakes.NewGateway(fakes.Quirks{}),
		log:     &logtest.Logger{},
		clock:   clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
	}
	gateway, err := upnp.At(ctx, p.router.Client(), fakes.RootURL)
	if err != nil {
		t.Fatal(err)
	}
	options := daemon.Defaults
	options.HostAddress = host
	p.daemon = daemon.New(daemon.Deps{Source: p.source, Gateway: gateway, Options: options, Log: p.log, Clock: p.clock})
	p.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565/tcp\n")
	return p
}

func (p *portical) once(t *testing.T) {
	t.Helper()
	if _, err := p.daemon.Once(ctx); err != nil {
		t.Fatal(err)
	}
}

func (p *portical) ports() []int {
	var found []int
	for _, mapping := range p.router.Mappings() {
		found = append(found, mapping.ExternalPort)
	}
	return found
}

func TestDaemonForwardsAUnitsPortToThisHostNamedAfterTheUnit(t *testing.T) {
	p := newPortical(t)
	p.systemd.Run(unit("mc.service"))
	p.once(t)
	assertEqual(t, p.router.Mappings(), []model.Mapping{{
		ExternalPort:   25565,
		Protocol:       model.TCP,
		InternalPort:   25565,
		InternalClient: host,
		Description:    "portical: (25565:25565/tcp) mc.service",
		RemoteHost:     "",
		LeaseDuration:  0,
	}})
}

func TestDaemonRemovesItWhenTheUnitStops(t *testing.T) {
	p := newPortical(t)
	p.systemd.Run(unit("mc.service"))
	p.once(t)
	p.systemd.Run()
	p.once(t)
	assertEqual(t, p.ports(), nil)
}

func TestDaemonRemovesItWhenADropInResetsTheKey(t *testing.T) {
	p := newPortical(t)
	p.systemd.Run(unit("mc.service"))
	p.once(t)
	p.files.Put("/etc/systemd/system/mc.service.d/off.conf", "[Unit]\nX-Portical-Forward=\n")
	p.systemd.Run(unit("mc.service", dropIns("/etc/systemd/system/mc.service.d/off.conf")))
	p.once(t)
	assertEqual(t, p.ports(), nil)
}

func TestDaemonRemovesNothingWhenSystemdCannotBeAsked(t *testing.T) {
	p := newPortical(t)
	p.systemd.Run(unit("mc.service"))
	p.once(t)
	p.systemd.FailWith(errors.New("D-Bus connection closed"))
	if _, err := p.daemon.Once(ctx); err == nil {
		t.Fatal("want the pass to fail")
	}
	assertEqual(t, p.ports(), []int{25565})
}

func TestDaemonReactsToAUnitStartingWithoutWaitingForTheIntervalAndSurvivesTheBusDropping(t *testing.T) {
	p := newPortical(t)
	running, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.daemon.Run(running)
	}()
	defer func() {
		stop()
		<-done
	}()
	await.Until(t, "the stream", func() bool { return p.systemd.Open() })
	assertEqual(t, p.ports(), nil)

	p.systemd.DropStream(nil)
	// The interval's wait and the backoff's.
	if err := p.clock.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	p.clock.Advance(daemon.Defaults.ReconnectBackoff)
	await.Until(t, "the stream to reopen", func() bool { return p.systemd.Watching() == 2 && p.systemd.Open() })

	p.systemd.Start(unit("mc.service"))
	await.Until(t, "the forward", func() bool { return reflect.DeepEqual(p.ports(), []int{25565}) })
	// Not declared on the pass before it started, so not worth a line yet.
	if p.log.Contains("Unit mc.service active") {
		t.Error("logged a unit that declared nothing yet")
	}

	p.systemd.Stop("mc.service")
	await.Until(t, "the forward to go", func() bool { return len(p.ports()) == 0 })
	await.Until(t, "the line", func() bool { return slices.Contains(p.log.Lines(), "Unit mc.service inactive") })
}
