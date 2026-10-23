package source_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/source"
)

var ctx = context.Background()

func forward(owner string, port int, protocol model.Protocol) model.DesiredForward {
	return model.DesiredForward{
		Rule:           model.Rule{ExternalPort: port, InternalPort: port, Protocol: protocol},
		Owner:          owner,
		Target:         model.Host,
		InternalClient: "192.168.1.5",
	}
}

func answering(forwards []model.DesiredForward, warnings ...string) *fakes.Source {
	fake := fakes.NewSource()
	fake.Want(source.Desired{Forwards: forwards, Warnings: warnings})
	return fake
}

func combine(t *testing.T, sources ...source.Source) source.Source {
	t.Helper()
	combined, err := source.Combine(sources...)
	if err != nil {
		t.Fatal(err)
	}
	return combined
}

func desired(t *testing.T, s source.Source) source.Desired {
	t.Helper()
	got, err := s.Desired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCombineIsTheSourceItselfWhenThereIsOnlyOne(t *testing.T) {
	only := fakes.NewSource()
	if combine(t, only) != source.Source(only) {
		t.Error("wrapped a single source")
	}
}

func TestCombineRefusesNoSourcesAtAllRatherThanWantingNothing(t *testing.T) {
	if _, err := source.Combine(); err == nil || err.Error() != "combine needs at least one source" {
		t.Errorf("got %v", err)
	}
}

func TestCombineWantsWhatEverySourceWantsInTheOrderTheyWereNamed(t *testing.T) {
	docker := answering([]model.DesiredForward{forward("nginx", 80, model.TCP)}, "nginx: something")
	systemd := answering([]model.DesiredForward{forward("mc.service", 25565, model.TCP)})
	want := source.Desired{
		Forwards: []model.DesiredForward{forward("nginx", 80, model.TCP), forward("mc.service", 25565, model.TCP)},
		Warnings: []string{"nginx: something"},
	}
	if got := desired(t, combine(t, docker, systemd)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestCombineFailsWhenAnySourceFailsSoNothingIsRemovedOnAPartialAnswer(t *testing.T) {
	docker := answering([]model.DesiredForward{forward("nginx", 80, model.TCP)})
	systemd := fakes.NewSource()
	systemd.FailWith(errors.New("D-Bus connection closed"))
	if _, err := combine(t, docker, systemd).Desired(ctx); err == nil || err.Error() != "D-Bus connection closed" {
		t.Errorf("got %v", err)
	}
}

func TestCombineGivesAPortTwoSourcesAskForToTheFirstAndSaysSo(t *testing.T) {
	docker := answering([]model.DesiredForward{forward("mc", 25565, model.TCP)})
	systemd := answering([]model.DesiredForward{forward("mc.service", 25565, model.TCP), forward("mc.service", 25565, model.UDP)})
	want := source.Desired{
		Forwards: []model.DesiredForward{forward("mc", 25565, model.TCP), forward("mc.service", 25565, model.UDP)},
		Warnings: []string{"mc.service and mc both ask for tcp/25565, forwarding it for mc"},
	}
	if got := desired(t, combine(t, docker, systemd)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestCombineLeavesAClashWithinOneSourceToTheReconcileAsBefore(t *testing.T) {
	docker := answering([]model.DesiredForward{forward("a", 80, model.TCP), forward("b", 80, model.TCP)})
	systemd := answering(nil)
	if got := desired(t, combine(t, docker, systemd)).Forwards; len(got) != 2 {
		t.Errorf("got %+v", got)
	}
}

// events reads the combined stream on its own goroutine, collecting what it
// sees, and cancelling once it has seen stopAfter events (never, when 0).
type events struct {
	mu   sync.Mutex
	seen []source.Event
	done chan error
}

func (e *events) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.seen)
}

func read(s source.Source, stopAfter int) *events {
	e := &events{done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		e.done <- s.Events(ctx, func(event source.Event) {
			e.mu.Lock()
			e.seen = append(e.seen, event)
			n := len(e.seen)
			e.mu.Unlock()
			if n == stopAfter {
				cancel()
			}
		})
	}()
	return e
}

func TestCombinePassesOnEventsFromEverySource(t *testing.T) {
	docker, systemd := fakes.NewSource(), fakes.NewSource()
	reading := read(combine(t, docker, systemd), 2)

	docker.Emit(source.Event{Description: "Container nginx start"})
	fakes.Await(t, "the first event", func() bool { return reading.count() == 1 })
	systemd.Emit(source.Event{})

	if err := <-reading.done; err != nil {
		t.Fatal(err)
	}
	if want := []source.Event{{Description: "Container nginx start"}, {}}; !reflect.DeepEqual(reading.seen, want) {
		t.Errorf("got %+v", reading.seen)
	}
	if docker.Open() != 0 || systemd.Open() != 0 {
		t.Error("a stream was left open")
	}
}

func TestCombineReturnsWhenAnyStreamFailsAndStopsTheOthers(t *testing.T) {
	docker, systemd := fakes.NewSource(), fakes.NewSource()
	reading := read(combine(t, docker, systemd), 0)
	fakes.Await(t, "both streams to open", func() bool { return docker.Open() == 1 && systemd.Open() == 1 })

	systemd.Drop(errors.New("D-Bus connection closed"))

	if err := <-reading.done; err == nil || err.Error() != "D-Bus connection closed" {
		t.Errorf("got %v", err)
	}
	if docker.Open() != 0 {
		t.Error("the other stream was left open")
	}
}

func TestCombineEndsWhenAnyStreamEndsSoTheDaemonReopensThemAll(t *testing.T) {
	docker, systemd := fakes.NewSource(), fakes.NewSource()
	reading := read(combine(t, docker, systemd), 0)
	fakes.Await(t, "both streams to open", func() bool { return docker.Open() == 1 && systemd.Open() == 1 })

	docker.Drop(nil)

	if err := <-reading.done; err != nil {
		t.Errorf("got %v", err)
	}
	if systemd.Open() != 0 {
		t.Error("the other stream was left open")
	}
}
