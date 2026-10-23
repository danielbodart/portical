package fakes

import (
	"context"
	"sync"

	"github.com/danielbodart/portical/internal/source"
)

// Source answers what it is told to, and emits when told to.
//
// It stands for whatever source a test is not about, so the daemon and
// Combine can be driven without Docker or systemd. Events emitted while no
// stream is open are kept for the next one, as a real source's would be
// waiting on its socket.
type Source struct {
	mu       sync.Mutex
	desired  source.Desired
	err      error
	calls    int
	queue    []source.Event
	wake     chan struct{} // closed, and replaced, on every Emit
	open     int
	opened   int
	end      chan error // the latest open stream's
	blocking func(ctx context.Context) error
}

// NewSource is a source that wants nothing.
func NewSource() *Source { return &Source{wake: make(chan struct{})} }

// Want replaces everything the source wants.
func (s *Source) Want(desired source.Desired) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired = desired
}

// FailWith makes Desired fail with err, until it is called with nil.
func (s *Source) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Blocking runs block at the start of every Desired, which then fails with
// whatever it returns. A test holds a pass in flight with it.
func (s *Source) Blocking(block func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocking = block
}

// Calls is how many times Desired has been asked.
func (s *Source) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *Source) Desired(ctx context.Context) (source.Desired, error) {
	s.mu.Lock()
	s.calls++
	block := s.blocking
	s.mu.Unlock()
	if block != nil {
		if err := block(ctx); err != nil {
			return source.Desired{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return source.Desired{}, s.err
	}
	return source.Desired{
		Forwards: append(s.desired.Forwards[:0:0], s.desired.Forwards...),
		Warnings: append(s.desired.Warnings[:0:0], s.desired.Warnings...),
	}, nil
}

// Emit sends an event to the open stream, or keeps it for the next.
func (s *Source) Emit(event source.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, event)
	close(s.wake)
	s.wake = make(chan struct{})
}

// Drop makes the open stream return err, as a socket's idle timeout or the
// bus going away would. With a nil err the stream ends cleanly. It reports
// whether there was a stream open to drop.
func (s *Source) Drop(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end == nil {
		return false
	}
	s.end <- err
	s.end = nil
	return true
}

// Open is how many streams are open now.
func (s *Source) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// Opened is how many streams have ever been opened.
func (s *Source) Opened() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened
}

func (s *Source) Events(ctx context.Context, notify func(source.Event)) error {
	end := make(chan error, 1)
	s.mu.Lock()
	s.open++
	s.opened++
	s.end = end
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.open--
		if s.end == end {
			s.end = nil
		}
	}()

	for {
		// A drop comes first, so an event emitted after it waits for the
		// next stream rather than racing the drop onto this one. Checked
		// under the lock Drop sends under, so that a queue holding such an
		// event is never taken without the drop being seen.
		s.mu.Lock()
		select {
		case err := <-end:
			s.mu.Unlock()
			return err
		default:
		}
		queued, wake := s.queue, s.wake
		s.queue = nil
		s.mu.Unlock()
		for _, event := range queued {
			notify(event)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-end:
			return err
		case <-wake:
		}
	}
}
