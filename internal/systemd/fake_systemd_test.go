package systemd

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing/fstest"

	"github.com/danielbodart/gokit/sockdiag"
)

// fakeSystemd is systemd as an object: units are started and stopped by
// calling methods, and each change comes out of Watch the way an ActiveState
// change would from the real thing.
//
// Changes made while no Watch is open are kept for the next one, as the real
// bus would have them waiting on its socket.
type fakeSystemd struct {
	mu      sync.Mutex
	running []Unit
	groups  map[string]string
	err     error
	queued  []Change
	stream  chan Change
	dropped chan error
	watches int
}

func newFakeSystemd() *fakeSystemd { return &fakeSystemd{groups: map[string]string{}} }

func (f *fakeSystemd) Units(ctx context.Context) ([]Unit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]Unit(nil), f.running...), nil
}

func (f *fakeSystemd) ControlGroup(ctx context.Context, unit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups[unit], nil
}

func (f *fakeSystemd) Watch(ctx context.Context, notify func(Change)) error {
	stream, dropped := make(chan Change, 64), make(chan error, 1)
	f.mu.Lock()
	for _, change := range f.queued {
		stream <- change
	}
	f.queued = nil
	f.stream, f.dropped = stream, dropped
	f.watches++
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stream == stream {
			f.stream, f.dropped = nil, nil
		}
	}()
	for {
		select {
		case change := <-stream:
			notify(change)
		case err := <-dropped:
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

// Watching is how many times Watch has been opened.
func (f *fakeSystemd) Watching() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watches
}

// Open is whether a Watch is open now.
func (f *fakeSystemd) Open() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stream != nil
}

func (f *fakeSystemd) Run(units ...Unit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = units
}

func (f *fakeSystemd) Group(unit, group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[unit] = group
}

// FailWith makes listing fail, as the bus going away would, until called
// with nil.
func (f *fakeSystemd) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSystemd) Start(unit Unit) {
	f.mu.Lock()
	f.running = append(f.running, unit)
	f.mu.Unlock()
	f.Emit(Change{Unit: unit.Name, State: "activating"}, Change{Unit: unit.Name, State: "active"})
}

func (f *fakeSystemd) Stop(name string) {
	f.mu.Lock()
	var left []Unit
	for _, unit := range f.running {
		if unit.Name != name {
			left = append(left, unit)
		}
	}
	f.running = left
	f.mu.Unlock()
	f.Emit(Change{Unit: name, State: "deactivating"}, Change{Unit: name, State: "inactive"})
}

func (f *fakeSystemd) Emit(changes ...Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, change := range changes {
		if f.stream != nil {
			f.stream <- change
		} else {
			f.queued = append(f.queued, change)
		}
	}
}

// DropStream ends the open Watch with err, as the bus dropping would.
func (f *fakeSystemd) DropStream(err error) {
	if err == nil {
		err = errors.New("D-Bus connection closed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropped != nil {
		f.dropped <- err
		f.stream, f.dropped = nil, nil
	}
}

func unit(name string, edits ...func(*Unit)) Unit {
	u := Unit{Name: name, FragmentPath: "/etc/systemd/system/" + name}
	for _, edit := range edits {
		edit(&u)
	}
	return u
}

func dropIns(paths ...string) func(*Unit) { return func(u *Unit) { u.DropInPaths = paths } }

func triggeredBy(units ...string) func(*Unit) { return func(u *Unit) { u.TriggeredBy = units } }

func fragment(path string) func(*Unit) { return func(u *Unit) { u.FragmentPath = path } }

func needsReload(u *Unit) { u.NeedDaemonReload = true }

// files is unit files by absolute path, changeable while a source reads them.
type files struct {
	mu     sync.Mutex
	byPath map[string]string
	// Paths that fail to read with this, as a file this user may not read
	// would.
	refuse map[string]error
}

func newFiles() *files { return &files{byPath: map[string]string{}, refuse: map[string]error{}} }

func (f *files) Put(path, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPath[path] = text
}

func (f *files) Refuse(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse[path] = err
}

func (f *files) Open(name string) (fs.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.refuse["/"+name]; ok {
		return nil, &fs.PathError{Op: "open", Path: "/" + name, Err: err}
	}
	snapshot := fstest.MapFS{}
	for path, text := range f.byPath {
		snapshot[path[1:]] = &fstest.MapFile{Data: []byte(text)}
	}
	return snapshot.Open(name)
}

// fakeSockets is a fixed list of listeners, counting how often it was asked.
type fakeSockets struct {
	mu        sync.Mutex
	listeners []sockdiag.Listener
	asked     int
}

func (s *fakeSockets) Set(listeners []sockdiag.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = listeners
}

func (s *fakeSockets) Listening(ctx context.Context) ([]sockdiag.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	return s.listeners, nil
}

func (s *fakeSockets) Asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func defaultEphemeral() (int, int, error) { return defaultEphemeralLow, defaultEphemeralHigh, nil }
