package flywheel

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrRailExhausted is the common sentinel for a consumed execution rail.
var ErrRailExhausted = errors.New("rail exhausted")

// RailKind identifies which execution budget prevented admission.
type RailKind string

const (
	RailIterations RailKind = "iterations"
	RailWallClock  RailKind = "wall-clock"
)

// RailExhaustedError identifies the specific rail that was exhausted.
type RailExhaustedError struct {
	Rail RailKind
}

func (err *RailExhaustedError) Error() string {
	if err == nil {
		return ErrRailExhausted.Error()
	}
	return fmt.Sprintf("%s: %s", ErrRailExhausted, err.Rail)
}

// Unwrap makes every specific exhaustion match ErrRailExhausted.
func (err *RailExhaustedError) Unwrap() error {
	return ErrRailExhausted
}

// RailConfig defines the hard limits for one fix loop.
type RailConfig struct {
	MaxIterations int
	MaxWallClock  time.Duration
}

// RailSnapshot is a read-only view of configured and consumed rails.
type RailSnapshot struct {
	MaxIterations int           `json:"max_iterations"`
	Iterations    int           `json:"iterations"`
	MaxWallClock  time.Duration `json:"max_wall_clock"`
	StartedAt     time.Time     `json:"started_at"`
	Deadline      time.Time     `json:"deadline"`
	Elapsed       time.Duration `json:"elapsed"`
}

// Rails admits budgeted operations against iteration and wall-clock limits.
type Rails struct {
	mu            sync.Mutex
	maxIterations int
	iterations    int
	maxWallClock  time.Duration
	startedAt     time.Time
	deadline      time.Time
	observedAt    time.Time
	now           func() time.Time
}

// NewRails starts rail accounting at the supplied clock's current time.
func NewRails(config RailConfig, now func() time.Time) (*Rails, error) {
	if config.MaxIterations <= 0 {
		return nil, errors.New("max iterations must be positive")
	}
	if config.MaxWallClock <= 0 {
		return nil, errors.New("max wall clock must be positive")
	}
	if now == nil {
		return nil, errors.New("clock is required")
	}
	startedAt := now()
	return &Rails{
		maxIterations: config.MaxIterations,
		maxWallClock:  config.MaxWallClock,
		startedAt:     startedAt,
		deadline:      startedAt.Add(config.MaxWallClock),
		observedAt:    startedAt,
		now:           now,
	}, nil
}

// AdmitAttempt consumes one iteration only when both rails admit the attempt.
func (rails *Rails) AdmitAttempt() error {
	if err := rails.validate(); err != nil {
		return err
	}
	rails.mu.Lock()
	defer rails.mu.Unlock()
	if !rails.observeLocked().Before(rails.deadline) {
		return &RailExhaustedError{Rail: RailWallClock}
	}
	if rails.iterations >= rails.maxIterations {
		return &RailExhaustedError{Rail: RailIterations}
	}
	rails.iterations++
	return nil
}

// AdmitLanding checks the wall-clock deadline without consuming an iteration.
func (rails *Rails) AdmitLanding() error {
	if err := rails.validate(); err != nil {
		return err
	}
	rails.mu.Lock()
	defer rails.mu.Unlock()
	if !rails.observeLocked().Before(rails.deadline) {
		return &RailExhaustedError{Rail: RailWallClock}
	}
	return nil
}

// Snapshot reports current consumption without admitting or consuming work.
func (rails *Rails) Snapshot() RailSnapshot {
	if rails == nil || rails.now == nil {
		return RailSnapshot{}
	}
	rails.mu.Lock()
	defer rails.mu.Unlock()
	elapsed := rails.observeLocked().Sub(rails.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return RailSnapshot{
		MaxIterations: rails.maxIterations,
		Iterations:    rails.iterations,
		MaxWallClock:  rails.maxWallClock,
		StartedAt:     rails.startedAt,
		Deadline:      rails.deadline,
		Elapsed:       elapsed,
	}
}

func (rails *Rails) observeLocked() time.Time {
	observed := rails.now()
	if observed.After(rails.observedAt) {
		rails.observedAt = observed
	}
	return rails.observedAt
}

func (rails *Rails) validate() error {
	if rails == nil || rails.now == nil || rails.maxIterations <= 0 || rails.maxWallClock <= 0 {
		return errors.New("rails are uninitialized")
	}
	return nil
}
