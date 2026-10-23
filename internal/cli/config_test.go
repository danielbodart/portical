package cli

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, env map[string]string, args ...string) Config {
	t.Helper()
	config, err := Parse(args, env)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	return config
}

func refused(t *testing.T, env map[string]string, want string, args ...string) {
	t.Helper()
	_, err := Parse(args, env)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse(%q) = %v, want an error containing %q", args, err, want)
	}
}

func TestParseDefaultsToRunningTheDaemon(t *testing.T) {
	if got := parse(t, nil).Command; got != "run" {
		t.Fatalf("command %q", got)
	}
}

func TestParseTakesEachCommand(t *testing.T) {
	for _, name := range []string{"run", "poll", "listen", "update", "list"} {
		if got := parse(t, nil, name).Command; got != name {
			t.Errorf("%s: command %q", name, got)
		}
	}
}

// v1 was a shell script invoked by its full path, and its own README told
// people to write `command: "/opt/portical/run poll"`. Those compose files are
// still running, and v2 sets an entrypoint, so that path now arrives as an
// ordinary argument. Read as a command name it would break every one of them
// on upgrade.
func TestParseKeepsV1CommandLinesWorking(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"/opt/portical/run", "listen"}, "listen"},
		{[]string{"/opt/portical/run", "poll"}, "poll"},
		{[]string{"/opt/portical/run", "update"}, "update"},
		{[]string{"/usr/local/bin/portical", "update"}, "update"},
	} {
		if got := parse(t, nil, c.args...).Command; got != c.want {
			t.Errorf("%q runs %q, want %q", c.args, got, c.want)
		}
	}
}

func TestParseRunsTheDaemonFromTheBarePathOnItsOwn(t *testing.T) {
	if got := parse(t, nil, "/opt/portical/run").Command; got != "run" {
		t.Fatalf("command %q", got)
	}
}

func TestParseStillReadsOptionsAroundTheV1Path(t *testing.T) {
	config := parse(t, nil, "/opt/portical/run", "-v", "-f", "update")
	if config.Command != "update" || !config.Daemon.Force {
		t.Fatalf("command %q, force %v", config.Command, config.Daemon.Force)
	}
}

// Only a *path* is skipped. Our own command is a bare word, and a relay
// payload could be anything, so neither must be swallowed.
func TestParseDoesNotSwallowTheRunCommandItself(t *testing.T) {
	if got := parse(t, nil, "run").Command; got != "run" {
		t.Fatalf("command %q", got)
	}
}

func TestParseDoesNotSwallowARelayPayloadThatLooksLikeThePath(t *testing.T) {
	config := parse(t, nil, "relay", "eyJ1cmwiOi/run")
	if !slices.Equal(config.Args, []string{"eyJ1cmwiOi/run"}) {
		t.Fatalf("args %q", config.Args)
	}
}

func TestParseReadsV1sEnvironmentVariables(t *testing.T) {
	config := parse(t, map[string]string{
		"PORTICAL_UPNP_ROOT_URL": "http://10.0.0.1:5000/rootDesc.xml",
		"PORTICAL_POLL_INTERVAL": "30",
	})
	if config.Root != "http://10.0.0.1:5000/rootDesc.xml" || config.Daemon.Interval != 30*time.Second {
		t.Fatalf("root %q, interval %v", config.Root, config.Daemon.Interval)
	}
}

// Compose files and unit files write "not this one" as a variable set to
// nothing.
func TestParseReadsAnEmptyVariableAsUnset(t *testing.T) {
	config := parse(t, map[string]string{
		"PORTICAL_UPNP_ROOT_URL": "", "PORTICAL_POLL_INTERVAL": "", "PORTICAL_SYSTEMD_KEY": "",
		"DOCKER_SOCKET": "", "PORTICAL_HELPER_IMAGE": "",
	})
	if config.Root != "" || config.Daemon.Interval != 15*time.Second || config.SystemdKey != "X-Portical-Forward" ||
		config.Socket != "/var/run/docker.sock" || config.HelperImage != "" {
		t.Fatalf("%+v", config)
	}
}

func TestParseAcceptsV1sFlags(t *testing.T) {
	config := parse(t, nil, "-r", "http://x/d.xml", "-d", "5", "-l", "my.label", "-f", "-v")
	if config.Root != "http://x/d.xml" || config.Daemon.Interval != 5*time.Second || config.Label != "my.label" || !config.Daemon.Force {
		t.Fatalf("%+v", config)
	}
}

func TestParseReadsEveryOtherOption(t *testing.T) {
	config := parse(t, map[string]string{"DOCKER_SOCKET": "/env/docker.sock", "PORTICAL_HELPER_IMAGE": "env/portical"},
		"--network-label", "my.network", "--lease", "86400", "--renew-within", "600",
		"--docker-socket", "/run/user/1000/docker.sock", "--helper-image", "me/portical:1",
		"-n", "--steal", "--manage-all", "--cleanup-on-exit", "--verbose", "update")
	d := config.Daemon
	if config.NetworkLabel != "my.network" || d.LeaseDuration != 86400*time.Second || d.RenewWithin != 600*time.Second ||
		config.Socket != "/run/user/1000/docker.sock" || config.HelperImage != "me/portical:1" ||
		!d.DryRun || !d.Steal || !d.ManageAll || !d.CleanupOnExit || d.Force {
		t.Fatalf("%+v", config)
	}
}

func TestParseDefaultsWhatItIsNotTold(t *testing.T) {
	config := parse(t, nil)
	d := config.Daemon
	if config.Label != "portical.upnp.forward" || config.NetworkLabel != "portical.upnp.network" ||
		config.Socket != "/var/run/docker.sock" || config.HelperImage != "" || config.Root != "" ||
		d.Interval != 15*time.Second || d.LeaseDuration != 0 || d.RenewWithin != 12*time.Hour {
		t.Fatalf("%+v", config)
	}
}

// -v is v1's verbose flag and is still accepted and ignored, so the version
// flag only has a long form. Asking for one is not asking to run anything.
func TestParseAsksForTheVersionWithoutNamingACommand(t *testing.T) {
	config := parse(t, nil, "--version")
	if !config.Version || config.Command != "run" {
		t.Fatalf("%+v", config)
	}
}

func TestParseDoesNotMistakeDashVForTheVersion(t *testing.T) {
	if parse(t, nil, "-v").Version {
		t.Fatal("-v read as --version")
	}
}

func TestParseAsksForHelp(t *testing.T) {
	if !parse(t, nil, "-h").Help || !parse(t, nil, "--help").Help {
		t.Fatal("no help")
	}
}

func TestParseRejectsAnUnknownOptionRatherThanIgnoringIt(t *testing.T) {
	refused(t, nil, "unknown option '--nonsense'", "--nonsense")
}

func TestParseRejectsANonNumericInterval(t *testing.T) {
	refused(t, nil, "positive number", "-d", "soon")
	refused(t, map[string]string{"PORTICAL_POLL_INTERVAL": "soon"}, "PORTICAL_POLL_INTERVAL must be a positive number", "update")
}

// JavaScript's Number() read all of these as something. They are mistakes,
// and an interval of nothing would walk the router's table back to back.
func TestParseRejectsAnythingButWholeSeconds(t *testing.T) {
	for _, value := range []string{"0", "-5", "1.5", "1e3", " 15", "+15", "0x10", "99999999999"} {
		refused(t, nil, "--duration must be a positive number of seconds", "--duration", value)
	}
	for _, value := range []string{"-1", "0.5", "week", "4294967296"} {
		refused(t, nil, "--lease must be a whole number of seconds", "--lease", value)
		refused(t, nil, "--renew-within must be a whole number of seconds", "--renew-within", value)
	}
	if parse(t, nil, "--lease", "0").Daemon.LeaseDuration != 0 || parse(t, nil, "--renew-within", "0").Daemon.RenewWithin != 0 {
		t.Fatal("0 is a lease that never expires, and never renewing early")
	}
}

func TestParseRejectsAMissingValue(t *testing.T) {
	refused(t, nil, "-r needs a value", "-r")
	refused(t, nil, "--source needs a value", "--source")
	refused(t, nil, "--label needs a value", "--label", "")
	refused(t, nil, "--helper-image needs a value", "--helper-image", "")
}

func TestParseRejectsARootThatCouldNeverBeFetched(t *testing.T) {
	refused(t, nil, "--root must be an http:// URL", "--root", "192.168.1.1:5000/rootDesc.xml")
	refused(t, map[string]string{"PORTICAL_UPNP_ROOT_URL": "rootDesc.xml"}, "--root must be an http:// URL")
	parse(t, nil, "--root", "https://gateway.lan/rootDesc.xml")
}

// TypeScript only found out once it had searched for a gateway.
func TestParseRejectsAnUnknownCommandAtOnce(t *testing.T) {
	refused(t, nil, "'start' is not a command", "start")
}

func TestParseRejectsArgumentsNoCommandTakes(t *testing.T) {
	refused(t, nil, "'update' takes no arguments, got 'now'", "update", "now")
	refused(t, nil, "'relay' takes one argument, got 2", "relay", "a", "b")
	if got := parse(t, nil, "relay").Args; len(got) != 0 {
		t.Fatalf("args %q", got)
	}
}

func TestParseTakesOptionsAfterTheCommand(t *testing.T) {
	config := parse(t, nil, "update", "--dry-run")
	if config.Command != "update" || !config.Daemon.DryRun {
		t.Fatalf("%+v", config)
	}
}

func TestParseChoosesDockerAloneByDefaultAsItAlwaysWas(t *testing.T) {
	if got := parse(t, nil).Sources; !slices.Equal(got, []string{"docker"}) {
		t.Fatalf("%q", got)
	}
}

func TestParseTakesSourceRepeatedOrCommaSeparatedInOrder(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"--source", "systemd"}, []string{"systemd"}},
		{[]string{"-s", "systemd", "-s", "docker"}, []string{"systemd", "docker"}},
		{[]string{"--source", "docker, systemd"}, []string{"docker", "systemd"}},
	} {
		if got := parse(t, nil, c.args...).Sources; !slices.Equal(got, c.want) {
			t.Errorf("%q: %q, want %q", c.args, got, c.want)
		}
	}
}

func TestParseReadsPorticalSourcesAndTheCommandLineReplacesIt(t *testing.T) {
	if got := parse(t, map[string]string{"PORTICAL_SOURCES": "systemd,docker"}).Sources; !slices.Equal(got, []string{"systemd", "docker"}) {
		t.Errorf("%q", got)
	}
	if got := parse(t, map[string]string{"PORTICAL_SOURCES": "systemd"}, "-s", "docker").Sources; !slices.Equal(got, []string{"docker"}) {
		t.Errorf("%q", got)
	}
}

func TestParseNamesEachSourceOnce(t *testing.T) {
	if got := parse(t, nil, "-s", "systemd,systemd").Sources; !slices.Equal(got, []string{"systemd"}) {
		t.Fatalf("%q", got)
	}
}

func TestParseRefusesASourceItDoesNotKnow(t *testing.T) {
	refused(t, nil, "unknown source 'podman'", "--source", "podman")
}

func TestParseRefusesAnEmptyListWhichWouldTakeEveryForwardDown(t *testing.T) {
	refused(t, nil, "at least one of docker, systemd", "--source", ",")
	refused(t, nil, "at least one of docker, systemd", "--source", "")
	refused(t, map[string]string{"PORTICAL_SOURCES": " , "}, "at least one of docker, systemd")
	refused(t, map[string]string{"PORTICAL_SOURCES": ""}, "at least one of docker, systemd")
}

func TestParseReadsTheUnitFileKey(t *testing.T) {
	if got := parse(t, nil).SystemdKey; got != "X-Portical-Forward" {
		t.Errorf("%q", got)
	}
	if got := parse(t, map[string]string{"PORTICAL_SYSTEMD_KEY": "X-Forward"}).SystemdKey; got != "X-Forward" {
		t.Errorf("%q", got)
	}
	if got := parse(t, map[string]string{"PORTICAL_SYSTEMD_KEY": "X-Forward"}, "--systemd-key", "X-Other").SystemdKey; got != "X-Other" {
		t.Errorf("%q", got)
	}
}

// The NixOS module's ExecStart, which must keep parsing.
func TestParseReadsTheCommandLineTheNixOSModuleWrites(t *testing.T) {
	config := parse(t, nil, "/nix/store/x-portical/bin/portical", "--source", "systemd,docker", "--duration", "15",
		"--lease", "0", "--renew-within", "43200", "--root", "http://192.168.1.1:5000/rootDesc.xml",
		"--docker-socket", "/run/docker.sock", "--steal", "--manage-all", "--cleanup-on-exit", "run")
	if config.Command != "run" || !slices.Equal(config.Sources, []string{"systemd", "docker"}) || config.Socket != "/run/docker.sock" {
		t.Fatalf("%+v", config)
	}
}
