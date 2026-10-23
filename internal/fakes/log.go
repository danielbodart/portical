package fakes

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Log keeps every line it is given, for the tests that assert on what was
// said. Pass its Line method wherever a func(string) log is wanted.
type Log struct {
	mu    sync.Mutex
	lines []string
}

// Line records one line.
func (l *Log) Line(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

// Lines is every line recorded since the last Clear.
func (l *Log) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// Text is every line, joined with newlines.
func (l *Log) Text() string { return strings.Join(l.Lines(), "\n") }

// Contains is whether any line contains text.
func (l *Log) Contains(text string) bool { return strings.Contains(l.Text(), text) }

// Clear forgets everything recorded so far.
func (l *Log) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = nil
}

// Await polls until done holds, failing the test if it has not after a few
// seconds. For what a test cannot be told directly - that a goroutine it does
// not own has got round to something - and never for passing time: time is a
// clock.Stopped's job.
func Await(t testing.TB, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
