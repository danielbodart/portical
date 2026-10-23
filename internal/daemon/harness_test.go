package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/rules"
	"github.com/danielbodart/portical/internal/source"
	"github.com/danielbodart/portical/internal/upnp"
)

const host = fakes.HostAddress

var ctx = context.Background()

// harness is the whole daemon, wired to a source and a router that exist only
// in memory, with time stopped.
//
// No process, no socket, no port. The tests below are the real reconcile
// loop and the real UPnP SOAP client, with only the far ends replaced. The
// source stands in for Docker: what it wants is worked out from a label the
// way a bridge or macvlan container's would be, so the Docker source's own
// choices - which network, what `published` means - are its tests' business,
// not these.
type harness struct {
	source *fakes.Source
	router *fakes.Gateway
	log    *fakes.Log
	clock  *clock.Stopped
	daemon *Daemon
}

func newHarness(t *testing.T, quirks fakes.Quirks, edit ...func(*Options)) *harness {
	t.Helper()
	h := &harness{
		source: fakes.NewSource(),
		router: fakes.NewGateway(quirks),
		log:    &fakes.Log{},
		clock:  clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
	}
	gateway, err := upnp.At(ctx, h.router.Client(), fakes.RootURL)
	if err != nil {
		t.Fatal(err)
	}
	options := Defaults
	options.HostAddress = host
	for _, e := range edit {
		e(&options)
	}
	h.daemon = New(Deps{Source: h.source, Gateway: gateway, Options: options, Log: h.log.Line, Clock: h.clock})
	return h
}

// container is something that wants ports forwarded, as its label says.
type container struct {
	name, label string
	// The container's own address on a macvlan network, or "" for one on a
	// bridge, which is forwarded to the host.
	macvlan string
}

func nginx() container { return container{name: "nginx", label: "9999:80/tcp"} }

func labelled(name, label string) container { return container{name: name, label: label} }

func (c container) forwards() []model.DesiredForward {
	target, address := model.Host, host
	if c.macvlan != "" {
		target, address = model.Container("lan"), c.macvlan
	}
	var forwards []model.DesiredForward
	for _, rule := range rules.Parse(c.label).Rules {
		forwards = append(forwards, model.DesiredForward{Rule: rule, Owner: c.name, Target: target, InternalClient: address})
	}
	return forwards
}

// running makes these the containers the source reports.
func (h *harness) running(containers ...container) {
	var desired source.Desired
	for _, c := range containers {
		desired.Forwards = append(desired.Forwards, c.forwards()...)
	}
	h.source.Want(desired)
}

// once runs one pass, failing the test if the pass fails.
func (h *harness) once(t *testing.T) {
	t.Helper()
	if _, err := h.daemon.Once(ctx); err != nil {
		t.Fatal(err)
	}
}

// launch starts the daemon, and returns what cancels it and what is closed
// once Run has returned.
func (h *harness) launch() (cancel func(), done <-chan struct{}) {
	ctx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.daemon.Run(ctx)
	}()
	return cancel, finished
}

// run starts the daemon, and returns what stops it and waits for it to stop.
// It is stopped at the end of the test if not before.
func (h *harness) run(t *testing.T) (stop func()) {
	t.Helper()
	cancel, done := h.launch()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after being cancelled")
		}
	}
	t.Cleanup(stop)
	return stop
}

func (h *harness) ports() []int {
	var ports []int
	for _, mapping := range h.router.Mappings() {
		ports = append(ports, mapping.ExternalPort)
	}
	slices.Sort(ports)
	return ports
}

func (h *harness) received(action string) bool { return h.router.Received(action) > 0 }
