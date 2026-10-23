package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/portical/internal/docker"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/systemd"
	"github.com/danielbodart/portical/internal/version"
)

// world is everything outside Portical, in memory: a router, a Docker, a
// systemd, the network that discovery searches and the signals the process
// is sent. Main runs against it exactly as it would against the real ones,
// down to the bytes on stdout and stderr.
type world struct {
	t       *testing.T
	router  *fakes.Gateway
	docker  *fakeDocker
	systemd *fakeSystemd
	clock   *clock.Stopped
	signals chan os.Signal
	stdout  *output
	stderr  *output
	env     map[string]string

	mu sync.Mutex
	// What an SSDP search finds, and which hosts we asked the route to.
	discovered []string
	searches   int
	facing     []string
	mountinfo  string
	gatewayHit int
}

const ownContainer = "4f1c2a9b8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928170f6e5d4c"

func newWorld(t *testing.T) *world {
	router := fakes.NewGateway(fakes.Quirks{})
	return &world{
		t:          t,
		router:     router,
		docker:     newFakeDocker(router),
		systemd:    &fakeSystemd{},
		clock:      clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
		signals:    make(chan os.Signal, 1),
		stdout:     &output{},
		stderr:     &output{},
		env:        map[string]string{},
		discovered: []string{fakes.RootURL},
		// The host, not a container: nothing in it names a container id.
		mountinfo: "22 1 0:21 / / rw,relatime - ext4 /dev/sda1 rw\n",
	}
}

func (w *world) container(args ...string) *Container {
	files := fstest.MapFS{}
	for name, text := range w.systemd.files {
		files[name] = &fstest.MapFile{Data: []byte(text)}
	}
	return &Container{
		Args:   args,
		Env:    w.env,
		Stdout: w.stdout,
		Stderr: w.stderr,
		Clock:  w.clock,
		GatewayTransport: httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			w.mu.Lock()
			w.gatewayHit++
			w.mu.Unlock()
			return httpx.HandlerTransport(w.router).RoundTrip(r)
		}),
		DockerTransport: w.docker.transport(),
		Discover: func(context.Context) []string {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.searches++
			return w.discovered
		},
		AddressFacing: func(host string) string {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.facing = append(w.facing, host)
			return fakes.HostAddress
		},
		Bus:       func(map[string]string) (systemd.Bus, error) { return w.systemd, nil },
		Sockets:   sockdiag.ListerFunc(func(context.Context) ([]sockdiag.Listener, error) { return nil, nil }),
		Files:     files,
		Ephemeral: func() (int, int, error) { return 32768, 60999, nil },
		MountInfo: func() ([]byte, error) { return []byte(w.mountinfo), nil },
		Signals:   func() (<-chan os.Signal, func()) { return w.signals, func() {} },
		Kernel:    func() string { return "6.18.57-test" },
	}
}

// main runs a command to the end.
func (w *world) main(args ...string) int {
	return Main(context.Background(), w.container(args...))
}

// start runs a command that keeps going until it is sent a signal.
func (w *world) start(args ...string) <-chan int {
	code := make(chan int, 1)
	go func() { code <- Main(context.Background(), w.container(args...)) }()
	return code
}

func (w *world) exited(code <-chan int) int {
	w.t.Helper()
	select {
	case c := <-code:
		return c
	case <-time.After(10 * time.Second):
		w.t.Fatalf("still running after a signal\n%s", w.stderr)
		return -1
	}
}

func (w *world) says(text string) {
	w.t.Helper()
	if !strings.Contains(w.stderr.String(), text) {
		w.t.Fatalf("stderr does not say %q:\n%s", text, w.stderr)
	}
}

func (w *world) never(text string) {
	w.t.Helper()
	if strings.Contains(w.stderr.String(), text) {
		w.t.Fatalf("stderr says %q:\n%s", text, w.stderr)
	}
}

func (w *world) mapped() []string {
	var found []string
	for _, m := range w.router.Mappings() {
		found = append(found, m.Description+" -> "+m.InternalClient)
	}
	slices.Sort(found)
	return found
}

func (w *world) expectMapped(want ...string) {
	w.t.Helper()
	if got := w.mapped(); !slices.Equal(got, want) {
		w.t.Fatalf("mapped %q, want %q\n%s", got, want, w.stderr)
	}
}

func (w *world) expectExit(got, want int) {
	w.t.Helper()
	if got != want {
		w.t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", got, want, w.stdout, w.stderr)
	}
}

func nginx() map[string]string { return map[string]string{"portical.upnp.forward": "8080:80/tcp"} }

func TestMainPrintsTheBannerBeforeAnythingElse(t *testing.T) {
	w := newWorld(t)
	w.expectExit(w.main("--nonsense"), 2)
	first, _, _ := strings.Cut(w.stderr.String(), "\n")
	if !strings.HasPrefix(first, "Portical "+version.Version+" starting - Go ") ||
		!strings.Contains(first, "kernel 6.18.57-test, args: --nonsense") {
		t.Fatal(first)
	}
}

func TestMainPrintsTheVersionOnStdoutAndNothingElseThere(t *testing.T) {
	defer func(was string) { version.Version = was }(version.Version)
	version.Version = "9.9.9"
	w := newWorld(t)
	w.expectExit(w.main("--version"), 0)
	if w.stdout.String() != "9.9.9\n" {
		t.Fatalf("%q", w.stdout)
	}
	w.says("Portical 9.9.9 starting")
	if w.gatewayHit != 0 || w.searches != 0 {
		t.Fatal("looked for a gateway")
	}
}

func TestMainPrintsHelpOnStdout(t *testing.T) {
	w := newWorld(t)
	w.expectExit(w.main("--help"), 0)
	if !strings.Contains(w.stdout.String(), "Usage: portical [options] [command]") ||
		!strings.Contains(w.stdout.String(), "--network-label L   Label naming which network to forward to (default portical.upnp.network)") {
		t.Fatal(w.stdout)
	}
}

func TestMainRefusesABadCommandLineWithTheUsageAndTouchesNothing(t *testing.T) {
	for _, args := range [][]string{{"--nonsense"}, {"start"}, {"-d", "soon"}, {"--source", ""}, {"update", "now"}} {
		w := newWorld(t)
		w.expectExit(w.main(args...), 2)
		w.says("Error: ")
		w.says("Usage: portical")
		if w.stdout.String() != "" || w.gatewayHit != 0 || w.searches != 0 || len(w.docker.seen()) != 0 {
			t.Fatalf("%q went further than the command line", args)
		}
	}
}

func TestMainListsTheGatewaysMappingsOnStdout(t *testing.T) {
	w := newWorld(t)
	w.router.Given(model.Mapping{ExternalPort: 8080, Protocol: model.TCP, InternalClient: "192.168.1.5", InternalPort: 80, Description: "portical: (8080:80/tcp) nginx"})
	w.router.Given(model.Mapping{ExternalPort: 19132, Protocol: model.UDP, InternalClient: "192.168.1.9", InternalPort: 19132, Description: "bedrock"})
	w.router.SetExternalIP("203.0.113.7")

	w.expectExit(w.main("-r", fakes.RootURL, "list"), 0)

	want := "tcp  8080 -> 192.168.1.5:80  portical: (8080:80/tcp) nginx\n" +
		"udp 19132 -> 192.168.1.9:19132  bedrock\n"
	if w.stdout.String() != want {
		t.Fatalf("%q", w.stdout)
	}
	w.says("Connecting to the gateway...")
	w.says("Using gateway at " + fakes.ControlURL)
	w.says("External address is 203.0.113.7")
	w.never("Searching")
	if len(w.docker.seen()) != 0 {
		t.Fatal("list asked Docker")
	}
}

func TestMainSearchesForTheGatewayWhenNotGivenARoot(t *testing.T) {
	w := newWorld(t)
	w.expectExit(w.main("list"), 0)
	w.says("Searching for an internet gateway...")
	w.says("Using gateway at " + fakes.ControlURL)
}

func TestMainReadsTheRootFromTheEnvironment(t *testing.T) {
	w := newWorld(t)
	w.env["PORTICAL_UPNP_ROOT_URL"] = fakes.RootURL
	w.expectExit(w.main("list"), 0)
	if w.searches != 0 {
		t.Fatal("searched anyway")
	}
}

func TestMainFailsCleanlyWhenNoGatewayAnswers(t *testing.T) {
	w := newWorld(t)
	w.discovered = nil
	w.expectExit(w.main("--source", "systemd", "--dry-run", "update"), 1)
	w.says("Error: no UPnP internet gateway found.")
	w.never("Usage")
}

func TestMainFailsWhenNoDeviceFoundForwardsPorts(t *testing.T) {
	w := newWorld(t)
	w.discovered = []string{"http://192.168.1.1:5000/missing.xml"}
	w.expectExit(w.main("list"), 1)
	w.says("Error: found 1 UPnP device, but none of them forwards ports")
}

func TestMainForwardsALabelledContainerToThisHost(t *testing.T) {
	w := newWorld(t)
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")

	w.expectExit(w.main("-r", fakes.RootURL, "update"), 0)

	w.expectMapped("portical: (8080:80/tcp) nginx -> 192.168.1.5")
	w.says("This host is 192.168.1.5 on the LAN")
	w.says("Talking to Docker at /var/run/docker.sock")
	w.says("Adding 8080:80/tcp for nginx")
	if !slices.Equal(w.facing, []string{"192.168.1.1"}) {
		t.Fatalf("asked the route to %q, want the gateway's host", w.facing)
	}
	if !slices.Contains(w.docker.seen(), "socket /var/run/docker.sock") {
		t.Fatal(w.docker.seen())
	}
}

func TestMainUsesTheDockerSocketAndLabelsItIsGiven(t *testing.T) {
	w := newWorld(t)
	w.env["DOCKER_SOCKET"] = "/from/env.sock"
	w.docker.container("nginx", map[string]string{"my.forward": "8080:80/tcp"}, "bridge", "bridge", "172.17.0.2")

	w.expectExit(w.main("--docker-socket", "/run/user/1000/docker.sock", "-l", "my.forward", "update"), 0)

	w.expectMapped("portical: (8080:80/tcp) nginx -> 192.168.1.5")
	w.says("Talking to Docker at /run/user/1000/docker.sock")
	if !slices.Contains(w.docker.seen(), "socket /run/user/1000/docker.sock") {
		t.Fatal(w.docker.seen())
	}
}

func TestMainForwardsAUnitsPortFromSystemd(t *testing.T) {
	w := newWorld(t)
	w.systemd.unit("minecraft.service", "[Unit]\nX-Portical-Forward=25565/tcp\n\n[Service]\nExecStart=/opt/minecraft/start\n")

	w.expectExit(w.main("--source", "systemd", "update"), 0)

	w.expectMapped("portical: (25565:25565/tcp) minecraft.service -> 192.168.1.5")
	w.says("Talking to systemd over unix:path=/run/dbus/system_bus_socket, reading X-Portical-Forward")
	if len(w.docker.seen()) != 0 {
		t.Fatal("asked Docker, which was not named")
	}
}

func TestMainReadsTheUnitFileKeyItIsGiven(t *testing.T) {
	w := newWorld(t)
	w.env["PORTICAL_SOURCES"] = "systemd"
	w.env["DBUS_SYSTEM_BUS_ADDRESS"] = "unix:path=/run/elsewhere"
	w.systemd.unit("minecraft.service", "[Unit]\nX-Forward=25565/tcp\nX-Portical-Forward=1234\n")

	w.expectExit(w.main("--systemd-key", "X-Forward", "update"), 0)

	w.expectMapped("portical: (25565:25565/tcp) minecraft.service -> 192.168.1.5")
	w.says("Talking to systemd over unix:path=/run/elsewhere, reading X-Forward")
}

func TestMainGivesAPortBothSourcesWantToTheFirstNamed(t *testing.T) {
	w := newWorld(t)
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")
	w.systemd.unit("caddy.service", "[Unit]\nX-Portical-Forward=8080:80/tcp\n")

	w.expectExit(w.main("-s", "systemd", "-s", "docker", "update"), 0)

	w.expectMapped("portical: (8080:80/tcp) caddy.service -> 192.168.1.5")
	w.says("Warning: nginx")
}

func TestMainFailsWhenTheSystemBusCannotBeUsed(t *testing.T) {
	w := newWorld(t)
	w.env["DBUS_SYSTEM_BUS_ADDRESS"] = "tcp:host=localhost,port=1"
	c := w.container("--source", "systemd", "update")
	c.Bus = systemd.SystemBus
	w.expectExit(Main(context.Background(), c), 1)
	w.says("only unix: addresses are supported")
}

func TestMainFailsAnUpdateDockerCannotAnswerAndRemovesNothing(t *testing.T) {
	w := newWorld(t)
	w.router.Given(model.Mapping{ExternalPort: 8080, Protocol: model.TCP, InternalClient: fakes.HostAddress, InternalPort: 80, Description: "portical: (8080:80/tcp) nginx"})
	c := w.container("update")
	c.DockerTransport = func(string) http.RoundTripper {
		return httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial unix /var/run/docker.sock: connect: permission denied")
		})
	}
	w.expectExit(Main(context.Background(), c), 1)
	w.says("permission denied")
	w.expectMapped("portical: (8080:80/tcp) nginx -> 192.168.1.5")
}

func TestMainDryRunSaysWhatItWouldDoAndDoesNothing(t *testing.T) {
	w := newWorld(t)
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")

	w.expectExit(w.main("--dry-run", "update"), 0)

	w.says("Dry run - nothing will be changed")
	w.says("Adding 8080:80/tcp for nginx")
	w.expectMapped()
}

func TestMainRunsUntilSIGTERMAndLeavesItsMappingsInPlace(t *testing.T) {
	w := newWorld(t)
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")

	code := w.start("run")
	fakes.Await(t, "the forward", func() bool { return len(w.router.Mappings()) == 1 })
	fakes.Await(t, "the event stream", func() bool { return w.docker.open() == 1 })
	w.signals <- syscall.SIGTERM

	w.expectExit(w.exited(code), 0)
	w.says("\nReceived SIGTERM, shutting down...")
	w.never("Removing Portical's mappings")
	w.expectMapped("portical: (8080:80/tcp) nginx -> 192.168.1.5")
	fakes.Await(t, "the event stream closed", func() bool { return w.docker.open() == 0 })
}

func TestMainTakesV1sPollAndListenAsRun(t *testing.T) {
	for _, args := range [][]string{{"/opt/portical/run", "poll"}, {"/opt/portical/run", "listen"}, {}} {
		w := newWorld(t)
		w.systemd.unit("minecraft.service", "[Unit]\nX-Portical-Forward=25565/tcp\n")
		w.env["PORTICAL_SOURCES"] = "systemd"

		code := w.start(args...)
		fakes.Await(t, "the bus watched", func() bool { return w.systemd.watching() == 1 })
		w.signals <- syscall.SIGINT

		w.expectExit(w.exited(code), 0)
		w.says("Received SIGINT, shutting down...")
		w.expectMapped("portical: (25565:25565/tcp) minecraft.service -> 192.168.1.5")
		if w.systemd.watching() != 0 {
			t.Fatal("still watching the bus")
		}
	}
}

func TestMainRepeatsThePassEveryInterval(t *testing.T) {
	w := newWorld(t)
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")

	code := w.start("-d", "30", "run")
	fakes.Await(t, "the forward", func() bool { return len(w.router.Mappings()) == 1 })
	w.router.ClearActions()
	if err := w.clock.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(29 * time.Second)
	if w.router.Received("GetGenericPortMappingEntry") != 0 {
		t.Fatal("passed before the interval")
	}
	w.clock.Advance(time.Second)
	fakes.Await(t, "a second pass", func() bool { return w.router.Received("GetGenericPortMappingEntry") > 0 })

	w.signals <- syscall.SIGTERM
	w.expectExit(w.exited(code), 0)
}

func TestMainRemovesItsMappingsOnExitWhenAsked(t *testing.T) {
	w := newWorld(t)
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")
	w.router.Given(model.Mapping{ExternalPort: 22, Protocol: model.TCP, InternalClient: fakes.HostAddress, InternalPort: 22, Description: "someone else's"})

	code := w.start("--cleanup-on-exit", "run")
	fakes.Await(t, "the forward", func() bool { return len(w.router.Mappings()) == 2 })
	w.signals <- syscall.SIGTERM

	w.expectExit(w.exited(code), 0)
	w.says("Removing Portical's mappings before exit...")
	w.expectMapped("someone else's -> 192.168.1.5")
}

func TestMainRemovesNothingOnExitInADryRun(t *testing.T) {
	w := newWorld(t)
	w.router.Given(model.Mapping{ExternalPort: 8080, Protocol: model.TCP, InternalClient: fakes.HostAddress, InternalPort: 80, Description: "portical: (8080:80/tcp) nginx"})
	w.docker.container("nginx", nginx(), "bridge", "bridge", "172.17.0.2")

	code := w.start("--cleanup-on-exit", "--dry-run", "run")
	fakes.Await(t, "the event stream", func() bool { return w.docker.open() == 1 })
	w.signals <- syscall.SIGTERM

	w.expectExit(w.exited(code), 0)
	w.never("Removing Portical's mappings")
	w.expectMapped("portical: (8080:80/tcp) nginx -> 192.168.1.5")
}

// A macvlan container's rule names its own address, so it is asked for from
// inside that container's network by Portical's own image - the one this
// process is running as, recognised from its mounts.
func TestMainReachesTheGatewayAsAMacvlanContainerWithItsOwnImage(t *testing.T) {
	w := newWorld(t)
	w.mountinfo = "531 512 259:2 /var/lib/docker/containers/" + ownContainer + "/hostname /etc/hostname rw - ext4 /dev/nvme0n1p2 rw\n"
	w.docker.images[ownContainer] = "ghcr.io/danielbodart/portical:2.58.412"
	w.docker.container("web", nginx(), "lan", "macvlan", "192.168.1.50")

	w.expectExit(w.main("update"), 0)

	w.expectMapped("portical: (8080:80/tcp) web -> 192.168.1.50")
	if images := w.docker.helperImages(); len(images) == 0 || images[0] != "ghcr.io/danielbodart/portical:2.58.412" {
		t.Fatalf("helpers ran %q", images)
	}
}

func TestMainRunsTheHelperImageItIsGivenWithoutLookingForItsOwn(t *testing.T) {
	w := newWorld(t)
	w.mountinfo = "531 512 259:2 /var/lib/docker/containers/" + ownContainer + "/hostname /etc/hostname rw - ext4 /dev/nvme0n1p2 rw\n"
	w.docker.images[ownContainer] = "ghcr.io/danielbodart/portical:2.58.412"
	w.docker.container("web", nginx(), "lan", "macvlan", "192.168.1.50")

	w.expectExit(w.main("--helper-image", "me/portical:fork", "update"), 0)

	if images := w.docker.helperImages(); len(images) == 0 || images[0] != "me/portical:fork" {
		t.Fatalf("helpers ran %q", images)
	}
	if slices.Contains(w.docker.seen(), "GET /containers/"+ownContainer+"/json") {
		t.Fatal("inspected itself anyway")
	}
}

func TestMainFallsBackToThePublishedImageOutsideAContainer(t *testing.T) {
	w := newWorld(t)
	w.docker.container("web", nginx(), "lan", "macvlan", "192.168.1.50")

	w.expectExit(w.main("update"), 0)

	if images := w.docker.helperImages(); len(images) == 0 || images[0] != docker.DefaultHelperImage {
		t.Fatalf("helpers ran %q", images)
	}
}

// relay is what the helper container runs: one request to the gateway, from
// where it stands, framed on stdout.
func TestMainRelaysARequestAndPrintsOnlyTheFramedResult(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, fakes.RootURL, nil)
	relayed, err := docker.ToRelayedRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.discovered = nil

	w.expectExit(w.main("relay", docker.EncodeRelay(relayed)), 0)

	response, err := docker.ParseRelayOutput(w.stdout.String())
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != http.StatusOK || !strings.Contains(response.Body, "WANIPConnection") {
		t.Fatalf("%+v", response)
	}
	w.never("Connecting to the gateway")
	if w.searches != 0 || len(w.docker.seen()) != 0 {
		t.Fatal("relay went looking for a gateway or Docker")
	}
}

func TestMainRefusesARelayWithNoPayload(t *testing.T) {
	w := newWorld(t)
	w.expectExit(w.main("relay"), 2)
	w.says("Error: relay needs a payload. It is used internally by Portical.")
	if w.stdout.String() != "" {
		t.Fatal(w.stdout)
	}
}
