// Package systemd is active systemd units as a source: the forwards their
// unit files declare.
//
//	[Unit]
//	X-Portical-Forward=25565,19133:19132/udp
//
// systemd ignores any key starting `X-`, so the declaration does nothing on a
// host without Portical. It cannot be read back over D-Bus - systemd keeps
// unknown keys to itself - so the files systemd names for each unit are read
// directly.
package systemd

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/rules"
	"github.com/danielbodart/portical/internal/source"
)

// ForwardKey is the unit file key a forward is declared with, the
// counterpart of the container label.
const ForwardKey = "X-Portical-Forward"

// Unit is a running unit, and where its configuration came from.
type Unit struct {
	Name string
	// The unit file itself. Empty for units with none, such as devices.
	FragmentPath string
	// Drop-ins, in the order systemd applied them.
	DropInPaths []string
	// The units that start this one: `.socket` units, for socket activation.
	TriggeredBy []string
	// The files on disk have changed since systemd last loaded them.
	NeedDaemonReload bool
}

// Change is something about the units that changed.
//
// Unit is set when a unit's ActiveState changed, and State is where it is now
// ("" when systemd only said the old value is stale). Both are empty when
// systemd finished reloading its configuration.
//
// Changes say where a unit is, not where it was: systemd repeats ActiveState
// with every change to a unit's properties, whether or not it moved, and
// telling a move from a repeat is the source's business, so the bus stays a
// translation of the signals and nothing more.
type Change struct {
	Unit, State string
}

// Bus is what the source needs from systemd, narrow enough that a fake can
// stand in for the whole of it.
type Bus interface {
	// Units is every unit that is active, reloading or refreshing - running,
	// in other words.
	Units(ctx context.Context) ([]Unit, error)
	// ControlGroup is the unit's cgroup, such as /system.slice/sshd.service,
	// or "" for a unit that has none.
	ControlGroup(ctx context.Context, unit string) (string, error)
	// Watch calls notify for each change until ctx is done, and then returns
	// nil, with the same contract as source.Source's Events: it may return
	// early when the bus drops, and calls notify from one goroutine.
	Watch(ctx context.Context, notify func(Change)) error
}

// Options are the source's settings.
type Options struct {
	// The unit file key; ForwardKey when empty.
	Key string
	// This host's address on the LAN, where every unit's forward points.
	HostAddress string
}

// Source is systemd's units as a source.Source.
type Source struct {
	bus       Bus
	sockets   sockdiag.Lister
	files     *unitFiles
	ephemeral func() (low, high int, err error)
	options   Options

	mu sync.Mutex
	// Every unit that has declared forwards this run, so their events are
	// logged and everyone else's are not. Every unit's events still ask for a
	// pass, because any unit can start declaring one.
	//
	// Never shrinks. A unit that stops drops out of the very pass its first
	// event triggers, so by the time its stream reaches "inactive" it would no
	// longer be declaring anything, and the line that says why its forward
	// went would be the one left out.
	declaring map[string]bool
}

// NewSource reads units from bus and their files from files, which is rooted
// at / as the paths systemd gives are absolute. sockets and ephemeral are only
// asked when a unit declares `published`. A nil files, sockets or ephemeral
// is the real machine's.
func NewSource(bus Bus, sockets sockdiag.Lister, files fs.FS, ephemeral func() (low, high int, err error), options Options) *Source {
	if files == nil {
		files = os.DirFS("/")
	}
	if sockets == nil {
		sockets = sockdiag.Kernel()
	}
	if ephemeral == nil {
		ephemeral = EphemeralPorts
	}
	if options.Key == "" {
		options.Key = ForwardKey
	}
	return &Source{
		bus:       bus,
		sockets:   sockets,
		files:     newUnitFiles(files),
		ephemeral: ephemeral,
		options:   options,
		declaring: map[string]bool{},
	}
}

var _ source.Source = (*Source)(nil)

type wanted struct {
	unit      Unit
	rules     []model.Rule
	published bool
}

// Desired is every forward the running units declare.
func (s *Source) Desired(ctx context.Context) (source.Desired, error) {
	var desired source.Desired

	units, err := s.bus.Units(ctx)
	if err != nil {
		return source.Desired{}, err
	}

	var wants []wanted
	anyPublished := false
	for _, unit := range units {
		files, err := s.read(unit)
		if err != nil {
			return source.Desired{}, err
		}
		value, warnings := declaration(unit.Name, files, s.options.Key)
		for _, warning := range warnings {
			desired.Warnings = append(desired.Warnings, unit.Name+": "+warning)
		}
		if value == "" {
			continue
		}

		s.declare(unit.Name)
		terms := rules.Parse(value)
		for _, problem := range terms.Errors {
			desired.Warnings = append(desired.Warnings, unit.Name+": "+problem)
		}
		if unit.NeedDaemonReload {
			desired.Warnings = append(desired.Warnings, unit.Name+": its unit files have changed since systemd loaded them - "+
				"forwarding what is on disk now. Run 'systemctl daemon-reload'.")
		}
		wants = append(wants, wanted{unit: unit, rules: terms.Rules, published: terms.Published})
		anyPublished = anyPublished || terms.Published
	}

	// The sockets are read once a pass, and only when someone asks for them:
	// most hosts never use `published`.
	var open *sockets
	if anyPublished {
		open, err = s.listening(ctx, &desired.Warnings)
		if err != nil {
			return source.Desired{}, err
		}
	}

	for _, want := range wants {
		found := want.rules
		if want.published {
			listened, err := s.published(ctx, want.unit, open)
			if err != nil {
				return source.Desired{}, err
			}
			// Explicit rules first, so they win any collision with a
			// listening port, as they do for a container.
			found = rules.Collapse(append(append([]model.Rule(nil), want.rules...), listened...))
		}
		for _, rule := range found {
			desired.Forwards = append(desired.Forwards, model.DesiredForward{
				Rule:           rule,
				Owner:          want.unit.Name,
				Target:         model.Host,
				InternalClient: s.options.HostAddress,
			})
		}
	}

	return desired, nil
}

// settled is where a unit comes to rest, rather than passes through.
var settled = map[string]bool{"active": true, "inactive": true, "failed": true}

// Events asks for a pass on every change to any unit, and puts a line in the
// log only where a unit that declares forwards settles, having been seen
// somewhere else first on this stream.
func (s *Source) Events(ctx context.Context, notify func(source.Event)) error {
	// Per stream: a new stream starts knowing nothing, so its first word on a
	// unit is where it is, not that it moved.
	states := map[string]string{}
	return s.bus.Watch(ctx, func(change Change) {
		if change.Unit == "" {
			notify(source.Event{})
			return
		}
		previous, seen := states[change.Unit]
		if seen && previous == change.State {
			return
		}
		states[change.Unit] = change.State
		if previous != "" && settled[change.State] && s.declared(change.Unit) {
			notify(source.Event{Description: fmt.Sprintf("Unit %s %s", change.Unit, change.State)})
			return
		}
		notify(source.Event{})
	})
}

func (s *Source) declare(unit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declaring[unit] = true
}

func (s *Source) declared(unit string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.declaring[unit]
}

// read is the unit file and its drop-ins, in the order systemd applies them.
func (s *Source) read(unit Unit) ([]unitFile, error) {
	paths := append([]string{unit.FragmentPath}, unit.DropInPaths...)
	var files []unitFile
	for _, path := range paths {
		if path == "" {
			continue
		}
		text, err := s.files.read(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", unit.Name, err)
		}
		files = append(files, unitFile{path: path, text: text})
	}
	return files, nil
}
