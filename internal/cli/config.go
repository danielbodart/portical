// Package cli is Portical's command line: what the arguments mean, and the
// object graph a command runs on.
package cli

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielbodart/portical/internal/daemon"
	"github.com/danielbodart/portical/internal/docker"
	"github.com/danielbodart/portical/internal/systemd"
)

// Usage is what --help prints, and what follows a command line that does not
// parse.
var Usage = `portical - UPnP port forwarding for Docker containers and systemd units

Usage: portical [options] [command]

Commands:
  run       Reconcile continuously, reacting to changes (default)
  update    Reconcile once and exit
  list      Show the gateway's current port mappings and exit

Options:
  -r, --root URL          UPnP root description URL (skips discovery, much faster)
  -d, --duration SECONDS  Seconds between reconcile passes (default ` + seconds(daemon.Defaults.Interval) + `)
  -s, --source NAME       Where forwards are declared: docker, systemd, or both
                          as docker,systemd. Repeatable (default docker)
      --systemd-key KEY   Unit file key to read (default ` + systemd.ForwardKey + `)
  -l, --label LABEL       Container label to read (default ` + docker.ForwardLabel + `)
      --network-label L   Label naming which network to forward to (default ` + docker.NetworkLabel + `)
      --lease SECONDS     Lease to request; 0 never expires (default ` + seconds(daemon.Defaults.LeaseDuration) + `)
      --renew-within SEC  Renew a mapping expiring within this (default ` + seconds(daemon.Defaults.RenewWithin) + `)
      --docker-socket P   Docker socket (default ` + DefaultDockerSocket + `)
  -n, --dry-run           Report what would change without changing it
  -f, --force             Rewrite every rule even if it already looks correct
      --steal             Take over a port another tool already forwards
      --helper-image IMG  Portical's own image, used to reach the gateway from
                          inside a macvlan container's network. Detected
                          automatically; only set this if detection fails.
      --manage-all        Manage every Portical rule, wherever it points.
                          Only safe with one Portical on the network.
      --cleanup-on-exit   Remove our mappings on shutdown
      --version           Show the version and exit
  -h, --help              Show this message

Environment:
  PORTICAL_UPNP_ROOT_URL  Same as --root
  PORTICAL_POLL_INTERVAL  Same as --duration
  PORTICAL_SOURCES        Same as --source
  PORTICAL_SYSTEMD_KEY    Same as --systemd-key
  DBUS_SYSTEM_BUS_ADDRESS The system bus, for the systemd source

The commands 'poll' and 'listen' are accepted as aliases for 'run'. In v1 they
were different things - 'listen' reacted to container starts but never renewed
a lease or noticed a container stopping, and 'poll' did the reverse. 'run' does
both, so there is nothing left to choose between.`

func seconds(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }

// DefaultDockerSocket is where Docker listens unless told otherwise.
const DefaultDockerSocket = "/var/run/docker.sock"

// Sources is every source that can be named, in the order the usage lists
// them.
var Sources = []string{"docker", "systemd"}

// Commands is every command, with the number of arguments each takes.
//
// `relay` is not in the usage: it is what Portical runs inside the throwaway
// container it uses to reach the gateway from a macvlan container's network,
// and its one argument is the request to make.
var commands = map[string]int{
	"run": 0, "poll": 0, "listen": 0, "update": 0, "list": 0, "relay": 1,
}

// Config is a command line, read.
type Config struct {
	Command string
	// What follows the command: only `relay` takes anything.
	Args []string
	// In the order named, which is also who wins a port both ask for.
	Sources    []string
	SystemdKey string
	// The gateway's root description; "" means discover it.
	Root   string
	Socket string
	// Only set when the user named one; otherwise it is detected at runtime.
	HelperImage  string
	Label        string
	NetworkLabel string
	// The daemon's options, all but HostAddress, which is only known once the
	// gateway is found.
	Daemon  daemon.Options
	Help    bool
	Version bool
}

// legacyEntrypoint is a path to v1's shell script, or to the binary itself,
// used as the first argument.
var legacyEntrypoint = regexp.MustCompile(`/(run|portical)$`)

// Parse reads a command line and the environment it runs in.
//
// Everything that can be checked without the network is checked here, so a
// mistake in a unit file or a compose file fails at once with the usage,
// rather than after a gateway search, or on the first pass an hour later.
func Parse(args []string, env map[string]string) (Config, error) {
	config := Config{
		Socket:       DefaultDockerSocket,
		SystemdKey:   systemd.ForwardKey,
		Label:        docker.ForwardLabel,
		NetworkLabel: docker.NetworkLabel,
		Daemon:       daemon.Defaults,
	}

	// A variable set to nothing reads as unset, which is how compose files
	// and unit files write "not this one" - except PORTICAL_SOURCES, below.
	if value := env["PORTICAL_UPNP_ROOT_URL"]; value != "" {
		config.Root = value
	}
	if value := env["DOCKER_SOCKET"]; value != "" {
		config.Socket = value
	}
	if value := env["PORTICAL_HELPER_IMAGE"]; value != "" {
		config.HelperImage = value
	}
	if value := env["PORTICAL_SYSTEMD_KEY"]; value != "" {
		config.SystemdKey = value
	}
	if value := env["PORTICAL_POLL_INTERVAL"]; value != "" {
		interval, err := interval(value, "PORTICAL_POLL_INTERVAL")
		if err != nil {
			return Config{}, err
		}
		config.Daemon.Interval = interval
	}
	// Set to nothing is still set: it is a list of no sources, which would
	// want no forwards and take down every one Portical owns.
	var sources []string
	if value, set := env["PORTICAL_SOURCES"]; set {
		named, err := sourceNames(value)
		if err != nil {
			return Config{}, err
		}
		sources = named
	}

	named := false
	commanded := false
	for i := 0; i < len(args); i++ {
		argument := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", argument)
			}
			i++
			// An empty value is never what was meant - it is a variable that
			// expanded to nothing - and every option here has a default that
			// would quietly have been better.
			if args[i] == "" {
				return "", fmt.Errorf("%s needs a value, got ''", argument)
			}
			return args[i], nil
		}

		var err error
		switch argument {
		case "-r", "--root":
			config.Root, err = value()
		case "-d", "--duration":
			var text string
			if text, err = value(); err == nil {
				config.Daemon.Interval, err = interval(text, argument)
			}
		case "-s", "--source":
			// Not value(): an empty list gets its own explanation.
			if i+1 >= len(args) {
				return Config{}, fmt.Errorf("%s needs a value", argument)
			}
			i++
			more, err := sourceNames(args[i])
			if err != nil {
				return Config{}, err
			}
			// The first flag replaces the environment's list rather than
			// adding to it, so a command line always says the whole of what
			// it means.
			if named {
				sources = append(sources, more...)
			} else {
				sources = more
			}
			named = true
		case "--systemd-key":
			config.SystemdKey, err = value()
		case "-l", "--label":
			config.Label, err = value()
		case "--network-label":
			config.NetworkLabel, err = value()
		case "--lease":
			var text string
			if text, err = value(); err == nil {
				config.Daemon.LeaseDuration, err = duration(text, argument)
			}
		case "--renew-within":
			var text string
			if text, err = value(); err == nil {
				config.Daemon.RenewWithin, err = duration(text, argument)
			}
		case "--docker-socket":
			config.Socket, err = value()
		case "--helper-image":
			config.HelperImage, err = value()
		case "-n", "--dry-run":
			config.Daemon.DryRun = true
		case "-f", "--force":
			config.Daemon.Force = true
		case "--steal":
			config.Daemon.Steal = true
		case "--manage-all":
			config.Daemon.ManageAll = true
		case "--cleanup-on-exit":
			config.Daemon.CleanupOnExit = true
		// Accepted and ignored: v1 used it to show upnpc's output, and there
		// is no longer a subprocess whose output could be hidden. It is also
		// why the version flag has no short form - -v was spoken for.
		case "-v", "--verbose":
		case "--version":
			config.Version = true
		case "-h", "--help":
			config.Help = true
		default:
			if strings.HasPrefix(argument, "-") {
				return Config{}, fmt.Errorf("unknown option '%s'", argument)
			}
			switch {
			// v1 was a shell script invoked by its full path, and its own
			// README told people to write `command: "/opt/portical/run
			// poll"`. Those compose files are still out there and must keep
			// working, so a leading argument that is plainly a path to the old
			// script is skipped rather than mistaken for a command name. The
			// slash is what distinguishes it from our own `run` command, and
			// only one standing where the command would is skipped: after the
			// command, it is that command's argument, and a relay payload can
			// end in anything.
			case !commanded && legacyEntrypoint.MatchString(argument):
			// The first bare word names the command; anything after it
			// belongs to that command, which is how `relay` receives its
			// payload.
			case !commanded:
				config.Command = argument
				commanded = true
			default:
				config.Args = append(config.Args, argument)
			}
		}
		if err != nil {
			return Config{}, err
		}
	}

	if !commanded {
		config.Command = "run"
	}
	if config.Help || config.Version {
		return config, nil
	}
	wanted, known := commands[config.Command]
	if !known {
		return Config{}, fmt.Errorf("'%s' is not a command", config.Command)
	}
	// relay's missing payload has its own message, which says what relay is.
	switch {
	case len(config.Args) > wanted && wanted == 0:
		return Config{}, fmt.Errorf("'%s' takes no arguments, got '%s'", config.Command, strings.Join(config.Args, " "))
	case len(config.Args) > wanted:
		return Config{}, fmt.Errorf("'%s' takes one argument, got %d", config.Command, len(config.Args))
	}
	if config.Root != "" {
		if err := checkRoot(config.Root); err != nil {
			return Config{}, err
		}
	}

	if sources == nil {
		sources = []string{"docker"}
	}
	for _, name := range sources {
		if !slices.Contains(config.Sources, name) {
			config.Sources = append(config.Sources, name)
		}
	}
	return config, nil
}

func sourceNames(value string) ([]string, error) {
	var names []string
	for name := range strings.SplitSeq(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(Sources, name) {
			return nil, fmt.Errorf("unknown source '%s', expected one of %s", name, strings.Join(Sources, ", "))
		}
		names = append(names, name)
	}
	// No sources would want no forwards, and the first pass would take down
	// every one Portical owns on the gateway.
	if len(names) == 0 {
		return nil, fmt.Errorf("a source list needs at least one of %s, got '%s'", strings.Join(Sources, ", "), value)
	}
	return names, nil
}

// interval is a time between passes: whole seconds, and at least one, since
// an interval of nothing would walk the router's table back to back.
func interval(value, name string) (time.Duration, error) {
	d, err := duration(value, name)
	if err != nil || d == 0 {
		return 0, fmt.Errorf("%s must be a positive number of seconds, got '%s'", name, value)
	}
	return d, nil
}

// duration is whole seconds, from 0 up to what UPnP's lease, an unsigned
// 32-bit count of seconds, can hold.
//
// Only digits: no sign, no fraction, no exponent and no spaces, so "1e3",
// "0.5" or " 15" are refused rather than read as whatever a looser parser made
// of them.
func duration(value, name string) (time.Duration, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number of seconds, got '%s'", name, value)
	}
	return time.Duration(parsed) * time.Second, nil
}

// checkRoot refuses a root URL that could never be fetched, so a typo is
// caught at start-up rather than read as a gateway that does not answer.
func checkRoot(root string) error {
	parsed, err := url.Parse(root)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("--root must be an http:// URL, such as http://192.168.1.1:5000/rootDesc.xml, got '%s'", root)
	}
	return nil
}
