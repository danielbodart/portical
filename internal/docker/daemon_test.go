package docker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/portical/internal/clock"
	"github.com/danielbodart/portical/internal/daemon"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/upnp"
)

// portical is the whole daemon over the Docker source, wired to a Docker and a
// router that exist only in memory, with time stopped.
//
// The daemon's own behaviour is tested in its package against a fake source;
// these are the cases that need real resolution from the Engine API's
// answers, and the real event stream.
type portical struct {
	docker  *fakeDocker
	router  *fakes.Gateway
	log     *fakes.Log
	clock   *clock.Stopped
	options daemon.Options
	daemon  *daemon.Daemon
}

func newPortical(t *testing.T) *portical {
	t.Helper()
	p := &portical{
		docker: newFakeDocker(),
		router: fakes.NewGateway(fakes.Quirks{}),
		log:    &fakes.Log{},
		clock:  clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
	}
	gateway, err := upnp.At(ctx, p.router.Client(), fakes.RootURL)
	if err != nil {
		t.Fatal(err)
	}
	p.options = daemon.Defaults
	p.options.HostAddress = fakes.HostAddress
	p.daemon = daemon.New(daemon.Deps{
		Source:  NewSource(p.docker.client(), Options{HostAddress: fakes.HostAddress}),
		Gateway: gateway,
		Options: p.options,
		Log:     p.log.Line,
		Clock:   p.clock,
	})
	return p
}

// run starts the daemon's loop and waits for its event stream to open,
// stopping it again when the test ends.
func (p *portical) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.daemon.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		// Docker sees the stream close a moment after Run has returned, as it
		// would over a socket.
		fakes.Await(t, "every event stream to close", func() bool { return p.docker.open() == 0 })
	})
	fakes.Await(t, "the event stream to open", func() bool { return p.docker.open() == 1 })
}

func (p *portical) ports() []int {
	var ports []int
	for _, mapping := range p.router.Mappings() {
		ports = append(ports, mapping.ExternalPort)
	}
	slices.Sort(ports)
	return ports
}

func nginx() Container { return container("nginx", forward("9999:80/tcp")) }

// Issue #1: the reverse-proxy setup that v1 could not resolve at all.
func onSeveralNetworks(labels ...string) Container {
	all := forward("80/tcp,443/tcp")
	for i := 0; i+1 < len(labels); i += 2 {
		all[labels[i]] = labels[i+1]
	}
	return container("traefik", all,
		network("networka", "bridge", "172.18.0.2"),
		network("networkb", "bridge", "172.19.0.2"),
		network("lan", "macvlan", containerIP),
	)
}

func TestAContainerOnSeveralNetworksIsForwardedToItsAddressOnTheLAN(t *testing.T) {
	p := newPortical(t)
	p.docker.given(onSeveralNetworks())

	p.daemon.Once(ctx)

	got := p.router.Mappings()
	if len(got) != 2 || got[0].InternalClient != containerIP || got[1].InternalClient != containerIP {
		t.Errorf("got %+v", got)
	}
}

func TestAContainerOnSeveralNetworksCanBeToldWhichNetworkToUse(t *testing.T) {
	p := newPortical(t)
	p.docker.given(onSeveralNetworks(NetworkLabel, "networkb"))

	p.daemon.Once(ctx)

	got := p.router.Mappings()
	if len(got) != 2 || got[0].InternalClient != fakes.HostAddress || got[1].InternalClient != fakes.HostAddress {
		t.Errorf("got %+v", got)
	}
}

// Removals are inferred from absence, so reconciling against a container
// list we failed to fetch would read as "everything stopped".
func TestADockerOutageNeverTearsDownLiveMappings(t *testing.T) {
	p := newPortical(t)
	p.docker.given(nginx())
	p.daemon.Once(ctx)

	p.docker.fail("Cannot connect to the Docker daemon")
	_, err := p.daemon.Once(ctx)

	if err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("got %v", err)
	}
	if got := p.router.Mappings(); len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestRunReconcilesOnAContainerEventWithoutWaitingForTheInterval(t *testing.T) {
	p := newPortical(t)
	p.run(t)

	p.docker.startContainer(nginx())
	fakes.Await(t, "nginx's forward", func() bool { return len(p.router.Mappings()) == 1 })

	p.docker.stop("nginx")
	fakes.Await(t, "nginx's forward to go", func() bool { return len(p.router.Mappings()) == 0 })

	if !p.log.Contains("Container nginx start") || !p.log.Contains("Container nginx die") {
		t.Errorf("logged %s", p.log.Text())
	}
}

func TestRunKeepsGoingAfterAPassFails(t *testing.T) {
	p := newPortical(t)
	p.run(t)

	p.docker.fail("Cannot connect to the Docker daemon")
	p.docker.startContainer(nginx())
	fakes.Await(t, "the failure to be reported", func() bool { return p.log.Contains("Cannot connect to the Docker daemon") })

	p.docker.fail("")
	p.docker.startContainer(container("plex", forward("32400/tcp")))

	// Both containers, including the one whose start event was lost to the
	// outage - every pass reconciles from scratch rather than replaying
	// events, so nothing has to be remembered across the failure.
	fakes.Await(t, "both forwards", func() bool { return slices.Equal(p.ports(), []int{9999, 32400}) })
}

func TestRunReconnectsWhenTheEventStreamDropsInsteadOfExiting(t *testing.T) {
	p := newPortical(t)
	p.run(t)

	// The stream drops the way the Docker socket's idle timeout or a Docker
	// restart drops it, without the daemon being asked to stop.
	p.docker.dropStream()
	// The interval's wait, and now the reconnect backoff's beside it.
	if err := p.clock.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Happens while the daemon is between streams; the reopened stream must
	// see it. Had Run exited on the drop, no mapping would ever appear: the
	// interval has not moved.
	p.docker.startContainer(nginx())
	p.clock.Advance(p.options.ReconnectBackoff)

	fakes.Await(t, "nginx's forward", func() bool { return len(p.router.Mappings()) == 1 })
	if opened := p.docker.everOpened(); opened != 2 {
		t.Errorf("opened %d streams", opened)
	}
}

// Each reconnect closes the stream it replaces, however many times Docker
// drops it - the TypeScript version leaked a listener per reconnect (#10).
func TestRunLeavesNothingOpenBehindAcrossReconnects(t *testing.T) {
	p := newPortical(t)
	p.run(t)

	for round := 1; round <= 20; round++ {
		p.docker.dropStream()
		if err := p.clock.BlockUntil(ctx, 2); err != nil {
			t.Fatal(err)
		}
		p.clock.Advance(p.options.ReconnectBackoff)
		fakes.Await(t, "the stream to reopen", func() bool { return p.docker.everOpened() == round+1 && p.docker.open() == 1 })
	}
}
