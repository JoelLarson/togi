package harness

import (
	"context"
	"testing"

	"github.com/joellarson/togi/internal/run"
)

func TestWorldConstructsRequestedPortAndForwardsRun(t *testing.T) {
	factory := &worldFactory{}
	world := NewWorld(factory, NeedsGauntlet)
	ctx, err := world.Before(context.Background(), nil)
	if err != nil {
		t.Fatalf("Before() = %v", err)
	}
	if ctx == nil || world.Environment() == nil {
		t.Fatal("Before() did not initialize the scenario environment")
	}
	if err := world.Run(ctx, RunRequest{Root: "."}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, err := world.LastRun().Outcome(); err != nil || got.Code != 1 {
		t.Fatalf("LastRun().Outcome() = %#v, %v", got, err)
	}
	if _, err := world.After(ctx, nil, nil); err != nil {
		t.Fatalf("After() = %v", err)
	}
	if !factory.gauntlet.closed {
		t.Fatal("After() did not close the constructed gauntlet port")
	}
}

func TestWorldRejectsUnavailablePort(t *testing.T) {
	world := NewWorld(&worldFactory{}, NeedsGauntlet)
	if _, err := world.History(); err != ErrUnsupportedCapability {
		t.Fatalf("History() = %v, want ErrUnsupportedCapability", err)
	}
}

type worldFactory struct{ gauntlet *worldGauntlet }

func (*worldFactory) Name() string           { return "world" }
func (*worldFactory) CapabilityTags() string { return "" }
func (*worldFactory) NewHistory(*Environment) (HistoryDriver, error) {
	return nil, ErrUnsupportedCapability
}
func (*worldFactory) NewWiki(*Environment) (WikiDriver, error) { return nil, ErrUnsupportedCapability }
func (f *worldFactory) NewGauntlet(*Environment) (GauntletDriver, error) {
	if f.gauntlet == nil {
		f.gauntlet = &worldGauntlet{}
	}
	return f.gauntlet, nil
}

type worldGauntlet struct{ closed bool }

func (*worldGauntlet) Run(context.Context, RunRequest) (RunObservation, error) {
	return newServiceRunObservation(nil, nil, &run.ExitError{Code: 1}, nil, ""), nil
}
func (g *worldGauntlet) Close() error { g.closed = true; return nil }
