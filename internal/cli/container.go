package cli

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/environ"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx"
	"github.com/danielbodart/gokit/sockdiag"
	sd "github.com/danielbodart/gokit/systemd"
	"github.com/danielbodart/portical/internal/systemd"
	"github.com/danielbodart/portical/internal/upnp"
	"github.com/danielbodart/portical/internal/version"
)

// Container is the one object graph. Production wires it once; each test
// wires a fresh one for an isolated graph. Leaves left nil get production
// defaults when Wire runs, so a test sets them to doubles *before* wiring.
// Everything below the leaves is derived by Wire, never set.
type Container struct {
	// The command line, without the program name, and the environment.
	Args []string
	Env  environ.Env
	// stdout carries only a command's output - list's rows, the version, the
	// help, relay's framed result - so anything reading it is not corrupted
	// by commentary, which all goes to stderr.
	Stdout, Stderr io.Writer
	// Running commentary and errors alike, both on Stderr by default, for the
	// reason above. Every breadcrumb Main leaves and everything the daemon
	// says comes through here, from many goroutines, a line at a time.
	Log logx.Logger
	// Tells systemd how the service is doing, as sd_notify(3) does. Nothing
	// outside systemd - under Docker, from a shell - where there is no
	// NOTIFY_SOCKET.
	Notify func(env environ.Env, states ...string) error
	// Time, for the interval and the reconnect backoff.
	Clock clock.Clock
	// How the gateway is reached, for SOAP, its description and `relay`.
	// Portical names itself on it whatever it is - see Wire.
	GatewayTransport http.RoundTripper
	// How the Docker Engine API is reached at a socket path, named as the
	// gateway's is.
	DockerTransport func(socket string) http.RoundTripper
	// An SSDP search: the root description URL of every device that
	// answered.
	Discover func(ctx context.Context) []string
	// This host's address on the network that reaches host, or "".
	AddressFacing func(host string) string
	// The system bus, from the environment.
	Bus func(env environ.Env) (systemd.Bus, error)
	// Listening sockets and their cgroups, for `published` on a unit.
	Sockets sockdiag.Lister
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
	// The Docker Engine API at a socket path.
	Docker func(socket string) http.RoundTripper
}

// product is how Portical names itself to the gateway and to Docker, rather
// than as Go's generic "Go-http-client/1.1", so a router's or a daemon's log
// says which program is asking, and which version.
func product() string { return "portical/" + version.Version }

// Wire fills in the leaves left unset and derives the rest from them.
func (c *Container) Wire() {
	if c.Args == nil {
		c.Args = os.Args[1:]
	}
	if c.Env == nil {
		c.Env = environ.Current()
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.Log == nil {
		c.Log = logx.Writers(c.Stderr, c.Stderr)
	}
	if c.Notify == nil {
		c.Notify = sd.Notify
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
		c.Sockets = sockdiag.Kernel()
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

	// Named here rather than in the defaults, so a transport a test hands in
	// is named too, and the test sees what the gateway would.
	//
	// Not Retrying: a gateway answers every UPnP fault as a 500 - a conflict,
	// and the end of the mapping table that every listing reads up to - so
	// it would retry what was never going to change, slowing each pass, and
	// the next pass already tries again whatever did fail.
	identifying := httpx.Identifying(product())
	c.Gateway = &http.Client{Transport: httpx.Decorate(c.GatewayTransport, identifying), Timeout: upnp.RequestTimeout}
	c.Docker = func(socket string) http.RoundTripper {
		return httpx.Decorate(c.DockerTransport(socket), identifying)
	}
}

// interrupts is SIGINT and SIGTERM, caught rather than fatal.
func interrupts() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	return signals, func() { signal.Stop(signals) }
}
