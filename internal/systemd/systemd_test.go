package systemd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/source"
)

var ctx = context.Background()

type fixture struct {
	systemd *fakeSystemd
	files   *files
	sockets *fakeSockets
	source  *Source
}

func newFixture() *fixture {
	f := &fixture{systemd: newFakeSystemd(), files: newFiles(), sockets: &fakeSockets{}}
	f.source = NewSource(f.systemd, f.sockets, f.files, defaultEphemeral, Options{HostAddress: host})
	return f
}

func (f *fixture) desired(t *testing.T) source.Desired {
	t.Helper()
	desired, err := f.source.Desired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return desired
}

func rulesOf(desired source.Desired) []model.Rule {
	var found []model.Rule
	for _, forward := range desired.Forwards {
		found = append(found, forward.Rule)
	}
	return found
}

func TestForwardsWhatAnActiveUnitDeclaresToThisHost(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565/tcp\n")
	f.systemd.Run(unit("mc.service"))

	assertEqual(t, f.desired(t), source.Desired{Forwards: []model.DesiredForward{{
		Rule:           rule(25565, model.TCP),
		Owner:          "mc.service",
		Target:         model.Host,
		InternalClient: host,
	}}})
}

func TestWantsNothingFromAUnitThatDeclaresNothingOrHasNoFile(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/sshd.service", "[Service]\nExecStart=/bin/sshd\n")
	f.systemd.Run(unit("sshd.service"), unit("dev-sda.device", fragment("")))
	assertEqual(t, f.desired(t), source.Desired{})
}

func TestReadsDropInsAfterTheUnitFile(t *testing.T) {
	f := newFixture()
	f.files.Put("/nix/store/x-mc.service", "[Unit]\nX-Portical-Forward=25565/tcp\n")
	f.files.Put("/etc/systemd/system/mc.service.d/off.conf", "[Unit]\nX-Portical-Forward=\n")
	f.systemd.Run(unit("mc.service", fragment("/nix/store/x-mc.service"), dropIns("/etc/systemd/system/mc.service.d/off.conf")))
	assertEqual(t, f.desired(t).Forwards, nil)
}

func TestReportsABadTermAgainstTheUnit(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565/tcp,%i/udp\n")
	f.systemd.Run(unit("mc.service"))
	desired := f.desired(t)
	assertEqual(t, len(desired.Forwards), 1)
	assertEqual(t, desired.Warnings, []string{"mc.service: ignoring unrecognised rule '%i/udp'"})
}

func TestReportsAKeyInTheWrongSectionAgainstTheUnit(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Install]\nX-Portical-Forward=25565/tcp\n")
	f.systemd.Run(unit("mc.service"))
	assertEqual(t, f.desired(t).Warnings, []string{"mc.service: ignoring X-Portical-Forward in [Install] at " +
		"/etc/systemd/system/mc.service:2 - put it in [Unit] or [Service]"})
}

func TestWarnsWhenTheFilesHaveChangedSinceSystemdLoadedThem(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565/tcp\n")
	f.systemd.Run(unit("mc.service", needsReload))
	assertEqual(t, f.desired(t).Warnings, []string{"mc.service: its unit files have changed since systemd loaded them - " +
		"forwarding what is on disk now. Run 'systemctl daemon-reload'."})
}

func TestFailsTheWholePassWhenAFileCannotBeRead(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565/tcp\n")
	f.files.Refuse("/etc/systemd/system/locked.service", fs.ErrPermission)
	f.systemd.Run(unit("mc.service"), unit("locked.service"))
	_, err := f.source.Desired(ctx)
	if err == nil || !strings.Contains(err.Error(), "locked.service") {
		t.Fatalf("got %v, want an error naming locked.service", err)
	}
}

func TestFailsTheWholePassWhenSystemdCannotBeAsked(t *testing.T) {
	f := newFixture()
	f.systemd.FailWith(errors.New("D-Bus connection closed"))
	if _, err := f.source.Desired(ctx); err == nil || err.Error() != "D-Bus connection closed" {
		t.Fatalf("got %v", err)
	}
}

func TestForwardsNothingForAUnitWhoseFileIsGone(t *testing.T) {
	f := newFixture()
	f.source = NewSource(f.systemd, f.sockets, os.DirFS("/"), defaultEphemeral, Options{})
	f.systemd.Run(unit("gone.service", fragment(filepath.Join(t.TempDir(), "gone.service")), needsReload))
	assertEqual(t, f.desired(t), source.Desired{})
}

func TestReadsARenamedKey(t *testing.T) {
	f := newFixture()
	f.source = NewSource(f.systemd, f.sockets, f.files, defaultEphemeral, Options{Key: "X-Forward", HostAddress: host})
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Forward=80/tcp\nX-Portical-Forward=81/tcp\n")
	f.systemd.Run(unit("mc.service"))
	assertEqual(t, rulesOf(f.desired(t)), []model.Rule{rule(80, model.TCP)})
}

func TestPublishedForwardsWhatTheUnitListensOnWithExplicitTermsWinning(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=published,25565:25566/tcp\n")
	f.sockets.Set(ss(t, "tcp LISTEN 0 128 0.0.0.0:25565 0.0.0.0:* cgroup:/system.slice/mc.service <->\n"+
		"udp UNCONN 0 0 0.0.0.0:19132 0.0.0.0:* cgroup:/system.slice/mc.service <->\n"+
		"tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* cgroup:/system.slice/sshd.service <->\n"))
	f.systemd.Run(unit("mc.service"))
	f.systemd.Group("mc.service", "/system.slice/mc.service")

	assertEqual(t, rulesOf(f.desired(t)), []model.Rule{
		{ExternalPort: 25565, InternalPort: 25566, Protocol: model.TCP},
		rule(19132, model.UDP),
	})
}

func TestPublishedIncludesTheSocketsOfTheSocketUnitThatActivatesIt(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/dns.service", "[Unit]\nX-Portical-Forward=published\n")
	f.sockets.Set(ss(t, "udp UNCONN 0 0 0.0.0.0:53 0.0.0.0:* cgroup:/system.slice/dns.socket <->\n"))
	f.systemd.Run(unit("dns.service", triggeredBy("dns.socket")))
	f.systemd.Group("dns.service", "/system.slice/dns.service")
	f.systemd.Group("dns.socket", "/system.slice/dns.socket")

	assertEqual(t, rulesOf(f.desired(t)), []model.Rule{rule(53, model.UDP)})
}

func TestPublishedDoesNotListSocketsUnlessSomeUnitAsksForIt(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565\n")
	f.systemd.Run(unit("mc.service"))
	f.desired(t)
	assertEqual(t, f.sockets.Asked(), 0)
}

func TestPublishedListsSocketsOnceAPassHoweverManyUnitsAsk(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/a.service", "[Unit]\nX-Portical-Forward=published\n")
	f.files.Put("/etc/systemd/system/b.service", "[Unit]\nX-Portical-Forward=published\n")
	f.systemd.Run(unit("a.service"), unit("b.service"))
	f.desired(t)
	assertEqual(t, f.sockets.Asked(), 1)
}

func TestPublishedWarnsWhenTheKernelCannotSayWhichCgroupASocketBelongsTo(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=published\n")
	f.sockets.Set(ss(t, "tcp LISTEN 0 128 0.0.0.0:25565 0.0.0.0:* ino:1 sk:2 <->\n"))
	f.systemd.Run(unit("mc.service"))
	f.systemd.Group("mc.service", "/system.slice/mc.service")

	desired := f.desired(t)
	assertEqual(t, desired.Forwards, nil)
	if len(desired.Warnings) != 1 || !strings.Contains(desired.Warnings[0], "the kernel reports no cgroup") {
		t.Fatalf("got %q", desired.Warnings)
	}
}

// Not knowing whose sockets they are is not failing to list them: `published`
// comes to nothing and says why, and every other forward still goes ahead.
func TestPublishedWithoutCgroup2WarnsAndKeepsTheExplicitForwards(t *testing.T) {
	f := newFixture()
	f.source = NewSource(f.systemd, SocketListerFunc(func(context.Context) ([]Listener, error) {
		listeners := ss(t, "tcp LISTEN 0 128 0.0.0.0:25565 0.0.0.0:* ino:1 sk:2 <->\n")
		listeners[0].CgroupID = 1
		return listeners, fmt.Errorf("no cgroup2 file system at /sys/fs/cgroup, so %w", ErrUnknownCgroups)
	}), f.files, defaultEphemeral, Options{HostAddress: host})
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=published\n")
	f.files.Put("/etc/systemd/system/web.service", "[Unit]\nX-Portical-Forward=8080/tcp\n")
	f.systemd.Run(unit("mc.service"), unit("web.service"))
	f.systemd.Group("mc.service", "/system.slice/mc.service")

	desired := f.desired(t)
	assertEqual(t, rulesOf(desired), []model.Rule{rule(8080, model.TCP)})
	assertEqual(t, desired.Warnings, []string{"no cgroup2 file system at /sys/fs/cgroup, so 'published' cannot tell which unit owns a socket and forwards nothing"})
}

func TestPublishedFailsThePassWhenTheSocketsCannotBeListed(t *testing.T) {
	f := newFixture()
	f.source = NewSource(f.systemd, SocketListerFunc(func(context.Context) ([]Listener, error) {
		return nil, errors.New("netlink refused")
	}), f.files, defaultEphemeral, Options{})
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=published\n")
	f.systemd.Run(unit("mc.service"))
	if _, err := f.source.Desired(ctx); err == nil || !strings.Contains(err.Error(), "netlink refused") {
		t.Fatalf("got %v", err)
	}
}

// events emits changes and reads the source's events until n have come out.
func events(t *testing.T, f *fixture, n int, changes ...Change) []source.Event {
	t.Helper()
	f.systemd.Emit(changes...)
	stream, stop := context.WithCancel(ctx)
	var seen []source.Event
	done := make(chan error)
	go func() {
		done <- f.source.Events(stream, func(event source.Event) {
			seen = append(seen, event)
			if len(seen) == n {
				stop()
			}
		})
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stop()
	return seen
}

func TestEventsLogWhereAUnitThatDeclaresForwardsSettlesAndNothingElse(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565\n")
	f.systemd.Run(unit("mc.service"))
	f.desired(t)

	assertEqual(t, events(t, f, 5,
		Change{Unit: "mc.service", State: "deactivating"},
		Change{Unit: "mc.service", State: "inactive"},
		Change{Unit: "cron.service", State: "activating"},
		Change{Unit: "cron.service", State: "active"},
		Change{},
	), []source.Event{{}, {Description: "Unit mc.service inactive"}, {}, {}, {}})
}

func TestEventsSayNothingOfAUnitsFirstStateOnAStreamWhichIsWhereItIsNotThatItMoved(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565\n")
	f.systemd.Run(unit("mc.service"))
	f.desired(t)

	assertEqual(t, events(t, f, 1, Change{Unit: "mc.service", State: "active"}), []source.Event{{}})
}

func TestEventsPassOverAStateRepeatedWithAnotherPropertyChange(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565\n")
	f.systemd.Run(unit("mc.service"))
	f.desired(t)

	assertEqual(t, events(t, f, 2,
		Change{Unit: "mc.service", State: "deactivating"},
		Change{Unit: "mc.service", State: "deactivating"},
		Change{Unit: "mc.service", State: "failed"},
	), []source.Event{{}, {Description: "Unit mc.service failed"}})
}

func TestEventsKeepLoggingAUnitThatHasStoppedDeclaring(t *testing.T) {
	f := newFixture()
	f.files.Put("/etc/systemd/system/mc.service", "[Unit]\nX-Portical-Forward=25565\n")
	f.systemd.Run(unit("mc.service"))
	f.desired(t)
	f.systemd.Run()
	f.desired(t)

	assertEqual(t, events(t, f, 2,
		Change{Unit: "mc.service", State: "active"},
		Change{Unit: "mc.service", State: "inactive"},
	), []source.Event{{}, {Description: "Unit mc.service inactive"}})
}
