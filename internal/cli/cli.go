package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"syscall"

	"github.com/danielbodart/gokit/dbusx"
	sd "github.com/danielbodart/gokit/systemd"
	"github.com/danielbodart/portical/internal/daemon"
	"github.com/danielbodart/portical/internal/docker"
	"github.com/danielbodart/portical/internal/source"
	"github.com/danielbodart/portical/internal/systemd"
	"github.com/danielbodart/portical/internal/upnp"
	"github.com/danielbodart/portical/internal/version"
)

// Main runs the command line c was given, and is the process's exit code: 2
// for a command line that does not parse, 1 for a failure, 0 otherwise.
func Main(ctx context.Context, c *Container) int {
	c.Wire()

	// Before anything else, and to stderr so it lands whatever a command
	// later writes to stdout. See Banner: this is the breadcrumb that tells a
	// start-up crash apart from Portical's code never having run at all.
	c.Log.Log(Banner(c.Args, c.Kernel()))

	config, err := Parse(c.Args, c.Env)
	if err != nil {
		c.Log.Error(fmt.Sprintf("Error: %s\n\n%s", err, Usage))
		return 2
	}
	if config.Version {
		fmt.Fprintln(c.Stdout, version.Version)
		return 0
	}
	if config.Help {
		fmt.Fprintln(c.Stdout, Usage)
		return 0
	}

	// Handled before anything else: relay runs inside a throwaway container
	// that has no Docker socket and no business discovering a gateway. It
	// performs one HTTP request from that container's network and prints the
	// result.
	if config.Command == "relay" {
		if len(config.Args) == 0 {
			c.Log.Error("Error: relay needs a payload. It is used internally by Portical.")
			return 2
		}
		return docker.RunRelay(config.Args[0], c.Stdout, c.Gateway.Transport)
	}

	if err := run(ctx, c, config); err != nil {
		c.Log.Error("Error: " + err.Error())
		return 1
	}
	return 0
}

func run(ctx context.Context, c *Container, config Config) error {
	c.Log.Log("Connecting to the gateway...")
	gateway, err := connect(ctx, c, config.Root)
	if err != nil {
		return err
	}
	c.Log.Log("Using gateway at " + gateway.ControlURL)

	// Only commentary, so a gateway that will not say is no reason to stop.
	if external, err := gateway.ExternalAddress(ctx); err == nil && external != "" {
		c.Log.Log("External address is " + external)
	}

	if config.Command == "list" {
		mappings, err := gateway.Mappings(ctx)
		if err != nil {
			return err
		}
		for _, m := range mappings {
			fmt.Fprintf(c.Stdout, "%-3s %5d -> %s:%d  %s\n", m.Protocol, m.ExternalPort, m.InternalClient, m.InternalPort, m.Description)
		}
		return nil
	}

	// Which of this host's addresses the gateway can see. Bridge and host
	// networked containers, and every systemd unit, are forwarded to the
	// host, so their mappings have to name it, and v1 never had to work it
	// out because upnpc inferred it.
	options := config.Daemon
	if control, err := url.Parse(gateway.ControlURL); err == nil {
		options.HostAddress = c.AddressFacing(control.Hostname())
	}
	if options.HostAddress != "" {
		c.Log.Log(fmt.Sprintf("This host is %s on the LAN", options.HostAddress))
	}

	var sources []source.Source
	var asContainer func(owner string) upnp.Gateway
	for _, name := range config.Sources {
		switch name {
		case "docker":
			c.Log.Log("Talking to Docker at " + config.Socket)
			client := docker.NewClient(c.Docker(config.Socket))
			sources = append(sources, docker.NewSource(client, docker.Options{
				Label: config.Label, NetworkLabel: config.NetworkLabel, HostAddress: options.HostAddress,
			}))
			// The same gateway, reached from inside a given container's
			// network. Used only for a rule pointing at a container's own
			// address - see docker.ViaContainer.
			asContainer = docker.AsContainer(client, gateway, helperImage(ctx, c, client, config))

		case "systemd":
			bus, err := c.Bus(c.Env)
			if err != nil {
				return err
			}
			if closer, ok := bus.(io.Closer); ok {
				defer closer.Close()
			}
			address := cmp.Or(c.Env["DBUS_SYSTEM_BUS_ADDRESS"], dbusx.DefaultSystemBus)
			c.Log.Log(fmt.Sprintf("Talking to systemd over %s, reading %s", address, config.SystemdKey))
			sources = append(sources, systemd.NewSource(bus, c.Sockets, c.Files, c.Ephemeral, systemd.Options{
				Key: config.SystemdKey, HostAddress: options.HostAddress,
			}))
		}
	}
	combined, err := source.Combine(sources...)
	if err != nil {
		return err
	}

	portical := daemon.New(daemon.Deps{
		Source:      combined,
		Gateway:     gateway,
		Options:     options,
		Log:         c.Log,
		Notify:      func(states ...string) error { return c.Notify(c.Env, states...) },
		Clock:       c.Clock,
		AsContainer: asContainer,
	})

	if options.DryRun {
		c.Log.Log("Dry run - nothing will be changed")
	}

	if config.Command == "update" {
		_, err := portical.Once(ctx)
		return err
	}

	// run, poll and listen: Parse has refused anything else.
	ctx, stop := untilInterrupted(ctx, c)
	defer stop()
	portical.Run(ctx)
	if options.CleanupOnExit && !options.DryRun {
		// Not ctx, which is what just told us to stop.
		return portical.Cleanup(context.WithoutCancel(ctx))
	}
	return nil
}

// connect finds the gateway, by root URL if we were given one and by SSDP if
// not.
func connect(ctx context.Context, c *Container, root string) (*upnp.Client, error) {
	if root != "" {
		return upnp.At(ctx, c.Gateway, root)
	}
	c.Log.Log("Searching for an internet gateway...")
	found := c.Discover(ctx)
	if len(found) == 0 {
		return nil, errors.New("no UPnP internet gateway found. Check UPnP is enabled on your router, and " +
			"note that discovery needs the container on the host network. Setting --root " +
			"or PORTICAL_UPNP_ROOT_URL skips discovery entirely and is much faster.")
	}
	return upnp.FirstAmong(ctx, c.Gateway, found)
}

// helperImage is the image the relay runs: Portical's own, so an explicit
// --helper-image is only needed when we cannot work out what we are running
// as. "" leaves it to docker.AsContainer's default.
func helperImage(ctx context.Context, c *Container, client *docker.Client, config Config) string {
	if config.HelperImage != "" {
		return config.HelperImage
	}
	mountinfo, err := c.MountInfo()
	if err != nil {
		return ""
	}
	return client.OwnImage(ctx, mountinfo)
}

// untilInterrupted is ctx, cancelled by the first SIGINT or SIGTERM.
//
// Run finishes the pass in flight before it returns, so stopping never leaves
// a rule removed and not yet put back. Only the first signal is caught: once
// it has been, the handler is released, so a second Ctrl-C at a shutdown that
// is taking too long ends the process the ordinary way.
func untilInterrupted(ctx context.Context, c *Container) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	signals, release := c.Signals()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case received := <-signals:
			release()
			c.Log.Log(fmt.Sprintf("\nReceived %s, shutting down...", signalName(received)))
			// Before the pass in flight and any cleanup, which systemd shows
			// as deactivating rather than as a service still running.
			if err := c.Notify(c.Env, sd.Stopping); err != nil {
				c.Log.Error("Error: " + err.Error())
			}
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		<-done
		release()
	}
}

func signalName(received os.Signal) string {
	switch received {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return received.String()
}
