package flywheel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRailsObserveExecutionContextRecordsDeadlineWithStaticClock(t *testing.T) {
	started := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	rails, err := NewRails(RailConfig{MaxIterations: 2, MaxWallClock: time.Millisecond}, func() time.Time { return started })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}

	ctx, cancel, err := rails.ExecutionContext(context.Background())
	if err != nil {
		t.Fatalf("ExecutionContext() error = %v", err)
	}
	defer cancel()
	<-ctx.Done()
	rails.ObserveExecutionContext(ctx)

	if got := rails.Snapshot().Elapsed; got != time.Millisecond {
		t.Fatalf("elapsed = %s, want %s", got, time.Millisecond)
	}
}

func TestRailsAdmitsAttemptsThroughExactIterationLimit(t *testing.T) {
	now := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	rails, err := NewRails(RailConfig{MaxIterations: 2, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}
	if err := rails.AdmitAttempt(); err != nil {
		t.Fatalf("first AdmitAttempt() error = %v", err)
	}
	if err := rails.AdmitAttempt(); err != nil {
		t.Fatalf("second AdmitAttempt() error = %v", err)
	}
	if got := rails.Snapshot().Iterations; got != 2 {
		t.Fatalf("iterations = %d, want 2", got)
	}

	err = rails.AdmitAttempt()
	assertRailExhausted(t, err, RailIterations)
	if got := rails.Snapshot().Iterations; got != 2 {
		t.Fatalf("rejected attempt changed iterations to %d", got)
	}
	if err := rails.AdmitLanding(); err != nil {
		t.Fatalf("AdmitLanding() after iteration limit error = %v", err)
	}
}

func TestRailsRejectsAdmissionAtExactWallClockDeadline(t *testing.T) {
	started := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	now := started
	rails, err := NewRails(RailConfig{MaxIterations: 2, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}

	now = started.Add(time.Minute - time.Nanosecond)
	if err := rails.AdmitAttempt(); err != nil {
		t.Fatalf("AdmitAttempt() before deadline error = %v", err)
	}
	now = started.Add(time.Minute)
	assertRailExhausted(t, rails.AdmitAttempt(), RailWallClock)
	assertRailExhausted(t, rails.AdmitLanding(), RailWallClock)
	if got := rails.Snapshot().Iterations; got != 1 {
		t.Fatalf("wall-clock rejection changed iterations to %d", got)
	}
}

func TestRailsNeverReopensAfterClockMovesBackward(t *testing.T) {
	started := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	now := started
	rails, err := NewRails(RailConfig{MaxIterations: 3, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}
	now = started.Add(time.Minute)
	assertRailExhausted(t, rails.AdmitAttempt(), RailWallClock)
	now = started.Add(10 * time.Second)
	assertRailExhausted(t, rails.AdmitAttempt(), RailWallClock)
	assertRailExhausted(t, rails.AdmitLanding(), RailWallClock)
	if got := rails.Snapshot().Elapsed; got != time.Minute {
		t.Fatalf("elapsed reopened after backward clock = %s, want %s", got, time.Minute)
	}
}

func TestRailSnapshotMakesObservedDeadlineSticky(t *testing.T) {
	started := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	now := started
	rails, err := NewRails(RailConfig{MaxIterations: 3, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}
	now = started.Add(time.Minute)
	if got := rails.Snapshot().Elapsed; got != time.Minute {
		t.Fatalf("snapshot elapsed at deadline = %s, want %s", got, time.Minute)
	}
	now = started.Add(10 * time.Second)
	assertRailExhausted(t, rails.AdmitAttempt(), RailWallClock)
	assertRailExhausted(t, rails.AdmitLanding(), RailWallClock)
	if got := rails.Snapshot(); got.Elapsed != time.Minute || got.Iterations != 0 {
		t.Fatalf("snapshot after backward clock = %#v, want sticky elapsed with no iterations", got)
	}
}

func TestRailSnapshotPersistsMaximumObservedTimeBeyondDeadline(t *testing.T) {
	started := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	now := started
	rails, err := NewRails(RailConfig{MaxIterations: 3, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}
	now = started.Add(90 * time.Second)
	if got := rails.Snapshot().Elapsed; got != 90*time.Second {
		t.Fatalf("snapshot elapsed beyond deadline = %s, want %s", got, 90*time.Second)
	}
	now = started.Add(20 * time.Second)
	if got := rails.Snapshot().Elapsed; got != 90*time.Second {
		t.Fatalf("snapshot elapsed after backward clock = %s, want %s", got, 90*time.Second)
	}
}

func TestRailsConcurrentAttemptsAdmitExactlyMaximum(t *testing.T) {
	now := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	const maximum = 7
	rails, err := NewRails(RailConfig{MaxIterations: maximum, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}
	var admitted atomic.Int64
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 100)
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := rails.AdmitAttempt(); err == nil {
				admitted.Add(1)
			} else {
				errorsSeen <- err
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		assertRailExhausted(t, err, RailIterations)
	}
	if got := admitted.Load(); got != maximum {
		t.Fatalf("admitted attempts = %d, want %d", got, maximum)
	}
	if got := rails.Snapshot().Iterations; got != maximum {
		t.Fatalf("snapshot iterations = %d, want %d", got, maximum)
	}
}

func TestRailSnapshotReportsLimitsAndDoesNotAdmit(t *testing.T) {
	started := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	now := started.Add(15 * time.Second)
	rails, err := NewRails(RailConfig{MaxIterations: 3, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRails() error = %v", err)
	}
	// NewRails records the clock at construction, so move it after creation.
	now = started.Add(30 * time.Second)
	first := rails.Snapshot()
	second := rails.Snapshot()
	if first != second {
		t.Fatalf("successive snapshots differ: %#v != %#v", first, second)
	}
	if first.MaxIterations != 3 || first.Iterations != 0 || first.MaxWallClock != time.Minute {
		t.Fatalf("snapshot limits/usage = %#v", first)
	}
	if first.StartedAt != started.Add(15*time.Second) || first.Deadline != started.Add(75*time.Second) || first.Elapsed != 15*time.Second {
		t.Fatalf("snapshot timing = %#v", first)
	}
}

func TestNewRailsAndNilReceiverFailSafely(t *testing.T) {
	validClock := func() time.Time { return time.Unix(0, 0) }
	for _, test := range []struct {
		name   string
		config RailConfig
		clock  func() time.Time
	}{
		{name: "zero iterations", config: RailConfig{MaxWallClock: time.Second}, clock: validClock},
		{name: "negative iterations", config: RailConfig{MaxIterations: -1, MaxWallClock: time.Second}, clock: validClock},
		{name: "zero wall clock", config: RailConfig{MaxIterations: 1}, clock: validClock},
		{name: "negative wall clock", config: RailConfig{MaxIterations: 1, MaxWallClock: -time.Second}, clock: validClock},
		{name: "nil clock", config: RailConfig{MaxIterations: 1, MaxWallClock: time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if rails, err := NewRails(test.config, test.clock); err == nil || rails != nil {
				t.Fatalf("NewRails() = (%#v, %v), want nil and error", rails, err)
			}
		})
	}

	var rails *Rails
	if err := rails.AdmitAttempt(); err == nil {
		t.Fatal("nil Rails.AdmitAttempt() returned nil")
	}
	if err := rails.AdmitLanding(); err == nil {
		t.Fatal("nil Rails.AdmitLanding() returned nil")
	}
	if got := rails.Snapshot(); got != (RailSnapshot{}) {
		t.Fatalf("nil Rails.Snapshot() = %#v, want zero snapshot", got)
	}
}

func assertRailExhausted(t *testing.T, err error, want RailKind) {
	t.Helper()
	if !errors.Is(err, ErrRailExhausted) {
		t.Fatalf("error = %v, want errors.Is(ErrRailExhausted)", err)
	}
	var exhausted *RailExhaustedError
	if !errors.As(err, &exhausted) || exhausted.Rail != want {
		t.Fatalf("error = %#v, want RailExhaustedError for %q", err, want)
	}
}
