package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

var start = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func released(channel <-chan time.Time) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func TestStoppedReleasesAWaitOnlyOnceTimeReachesIt(t *testing.T) {
	clock := NewStopped(start)
	wait := clock.After(15 * time.Second)

	clock.Advance(14 * time.Second)
	if released(wait) {
		t.Fatal("released early")
	}
	clock.Advance(time.Second)
	if !released(wait) {
		t.Fatal("not released at its deadline")
	}
	if got := clock.Now(); !got.Equal(start.Add(15 * time.Second)) {
		t.Errorf("now is %v", got)
	}
}

func TestStoppedCountsOutstandingWaits(t *testing.T) {
	clock := NewStopped(start)
	clock.After(time.Second)
	clock.After(time.Hour)
	if clock.Waiting() != 2 {
		t.Fatalf("got %d", clock.Waiting())
	}
	clock.Advance(time.Second)
	if clock.Waiting() != 1 {
		t.Fatalf("got %d", clock.Waiting())
	}
}

func TestStoppedBlocksUntilAGoroutineIsWaiting(t *testing.T) {
	clock := NewStopped(start)
	done := make(chan error)
	go func() { done <- Sleep(context.Background(), clock, time.Minute) }()

	if err := clock.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSleepGivesUpWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, NewStopped(start), time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

func TestAZeroWaitIsOverAtOnce(t *testing.T) {
	if !released(NewStopped(start).After(0)) {
		t.Error("not released")
	}
}
