// Package source is where forwards come from, as the daemon sees it.
//
// Docker containers are one source and systemd units another; the daemon and
// the reconcile underneath it do not care which.
package source

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/danielbodart/portical/internal/model"
)

// Desired is the forwards a source wants, and what it had to say about
// getting them.
//
// Warnings travel with the forwards rather than being logged by the source,
// so the daemon decides how they are reported and the tests can read them.
// Each is a whole sentence about one thing, conventionally led by the owner it
// concerns - "nginx: ignoring unrecognised rule '8O8O'" - and the daemon logs
// it as "Warning: " followed by the text.
type Desired struct {
	Forwards []model.DesiredForward
	Warnings []string
}

// Event is something that happened which may have changed what a source
// wants.
//
// It carries nothing the daemon acts on. Every event means the same thing -
// reconcile again - and the next pass asks Desired afresh, so the only use
// for what is in here is the line it puts in the log.
//
// The line is optional, so a source can ask for a pass without saying so. A
// source that has to listen to everything to catch the few things it cares
// about - systemd announces every unit on the host - would otherwise fill the
// log with units that have nothing to do with port forwarding.
type Event struct {
	Description string
}

// Source answers two questions: what should exist right now, and when that
// answer may have changed. The interval pass covers anything the second
// misses, so a source whose events are lossy, or absent, is still correct -
// only slower.
type Source interface {
	// Desired is every forward this source wants, or an error.
	//
	// Must never return a partial list. Removals are derived from absence, so
	// a source that skipped what it failed to read would tear down the live
	// forwards it could not see - fail the whole pass instead.
	Desired(ctx context.Context) (Desired, error)

	// Events calls notify for each change that may affect Desired, until ctx
	// is done, and then returns nil.
	//
	// It may also return early, with an error or without one, when its stream
	// drops: the daemon reopens it after a short backoff. notify does not
	// block for long and is safe to call from any goroutine, but a source
	// calls it from one goroutine at a time.
	Events(ctx context.Context, notify func(Event)) error
}

// Combine is several sources as one.
//
// Portical should run every source on a host from one instance. Two instances
// on the same host forward to the same address, so each takes the other's
// mappings for its own, finds nothing asking for them, and removes them.
//
// Desired fails if any source fails, for the reason Desired must never be
// partial. Two sources asking for the same slot on the gateway would rewrite
// each other on every pass, so the first source named keeps it and the other
// is warned about - the same first-wins rule as within a label.
//
// One source is returned as it is. None is refused: nothing to combine would
// want nothing, and take every forward down.
func Combine(sources ...Source) (Source, error) {
	switch len(sources) {
	case 0:
		return nil, errors.New("combine needs at least one source")
	case 1:
		return sources[0], nil
	}
	return combined(sources), nil
}

type combined []Source

func (sources combined) Desired(ctx context.Context) (Desired, error) {
	answers := make([]Desired, len(sources))
	failures := make([]error, len(sources))
	var wg sync.WaitGroup
	for i, source := range sources {
		wg.Go(func() { answers[i], failures[i] = source.Desired(ctx) })
	}
	wg.Wait()
	// The first named to fail, so the error does not depend on which was
	// quicker to.
	for _, err := range failures {
		if err != nil {
			return Desired{}, err
		}
	}

	var desired Desired
	claimed := map[string]model.DesiredForward{}
	for _, answer := range answers {
		desired.Warnings = append(desired.Warnings, answer.Warnings...)
		// Claims are recorded per source, after it has been read, so that a
		// source's own forwards never count as a clash with each other - two
		// containers on one port is the reconcile's business, as it was
		// before there were several sources.
		var mine []model.DesiredForward
		for _, forward := range answer.Forwards {
			slot := forward.Rule.Identity()
			if owner, ok := claimed[slot]; ok {
				desired.Warnings = append(desired.Warnings, fmt.Sprintf(
					"%s and %s both ask for %s, forwarding it for %s", forward.Owner, owner.Owner, slot, owner.Owner))
				continue
			}
			desired.Forwards = append(desired.Forwards, forward)
			mine = append(mine, forward)
		}
		for _, forward := range mine {
			if _, ok := claimed[forward.Rule.Identity()]; !ok {
				claimed[forward.Rule.Identity()] = forward
			}
		}
	}
	return desired, nil
}

// Events is every source's events as one, until ctx is done.
//
// Returns when any of them returns, with what it returned, after stopping the
// rest and waiting for them: the daemon then reopens the whole set, which is
// simpler than reopening one and costs nothing the interval does not cover.
func (sources combined) Events(ctx context.Context, notify func(Event)) error {
	inner, stop := context.WithCancel(ctx)
	defer stop()

	// One at a time, as the contract promises, whichever source it came from.
	var mu sync.Mutex
	serial := func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		notify(event)
	}

	ended := make(chan error, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Go(func() { ended <- source.Events(inner, serial) })
	}
	first := <-ended
	stop()
	wg.Wait()
	return first
}
