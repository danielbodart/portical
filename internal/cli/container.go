package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/danielbodart/portical/internal/clock"
	"github.com/danielbodart/portical/internal/httpx"
	"github.com/danielbodart/portical/internal/systemd"
	"github.com/danielbodart/portical/internal/upnp"
)

// Container is the one object graph. Production wires it once; each test
// wires a fresh one for an isolated graph. Leaves left nil get production
// defaults when Wire runs, so a test sets them to doubles *before* wiring.
// Everything below the leaves is derived by Wire, never set.
type Container struct {
	// The command line, without the program name, and the environment.
	Args []string
	Env  map[string]string
	// stdout carries only a command's output - list's rows, the version, the
	// help, relay's framed result - so anything reading it is not corrupted
	// by commentary, which all goes to stderr.
	Stdout, Stderr io.Writer
	// Time, for the interval and the reconnect backoff.
	Clock clock.Clock
	// How the gateway is reached, for SOAP, its description and `relay`.
	GatewayTransport http.RoundTripper
	// How the Docker Engine API is reached at a socket path.
	DockerTransport func(socket string) http.RoundTripper
	// An SSDP search: the root description URL of every device that
	// answered.
	Discover func(ctx context.Context) []string
	// This host's address on the network that reaches host, or "".
	AddressFacing func(host string) string
	// The system bus, from the environment.
	Bus func(env map[string]string) (systemd.Bus, error)
	// Listening sockets and their cgroups, for `published` on a unit.
	Sockets systemd.SocketLister
	// Unit files, rooted at /.
	Files fs.FS
	// The kernel's ephemeral port range, which `published` skips for UDP.
	Ephemeral func() (low, high int, err error)
	// /proc/self/mountinfo, from which our own container is recognised.
	MountInfo func() ([]byte, error)
	// SIGINT and SIGTERM as they arrive, and how to stop listening for them.
	Signals func() (<-chan os.Signal, func())
	// The running kernel's release, for the banner.
	Kernel func() string

	// The gateway's HTTP client, bounded by upnp.RequestTimeout.
	Gateway *http.Client

	speaking sync.Mutex
}

// Wire fills in the leaves left unset and derives the rest from them.
func (c *Container) Wire() {
	if c.Args == nil {
		c.Args = os.Args[1:]
	}
	if c.Env == nil {
		c.Env = environ()
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.Clock == nil {
		c.Clock = clock.System{}
	}
	if c.GatewayTransport == nil {
		c.GatewayTransport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if c.DockerTransport == nil {
		c.DockerTransport = httpx.UnixSocket
	}
	if c.Discover == nil {
		c.Discover = func(ctx context.Context) []string { return upnp.Discover(ctx, upnp.DiscoveryTimeout) }
	}
	if c.AddressFacing == nil {
		c.AddressFacing = func(host string) string { return upnp.AddressFacing(host, upnp.AddressFacingTimeout) }
	}
	if c.Bus == nil {
		c.Bus = systemd.SystemBus
	}
	if c.Sockets == nil {
		c.Sockets = systemd.Kernel()
	}
	if c.Files == nil {
		c.Files = os.DirFS("/")
	}
	if c.Ephemeral == nil {
		c.Ephemeral = systemd.EphemeralPorts
	}
	if c.MountInfo == nil {
		c.MountInfo = func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }
	}
	if c.Signals == nil {
		c.Signals = interrupts
	}
	if c.Kernel == nil {
		c.Kernel = release
	}

	c.Gateway = &http.Client{Transport: c.GatewayTransport, Timeout: upnp.RequestTimeout}
}

// Note is a line of running commentary, on stderr.
//
// Everything the daemon says about what it is doing, and every breadcrumb
// Main leaves, comes through here - from the daemon's goroutines and the
// signal handler alike, so one line is written at a time.
func (c *Container) Note(line string) {
	c.speaking.Lock()
	defer c.speaking.Unlock()
	fmt.Fprintln(c.Stderr, line)
}

func environ() map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

// interrupts is SIGINT and SIGTERM, caught rather than fatal.
func interrupts() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	return signals, func() { signal.Stop(signals) }
}
