package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

type platformFeature struct {
	world          *harness.World
	beforeTree     []string
	marker         string
	previousMarker string
	markerWasSet   bool
}

func newPlatformFeature(factory harness.DriverFactory) *platformFeature {
	return &platformFeature{world: harness.NewWorld(factory, harness.NeedsGauntlet)}
}

func (f *platformFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.after)
	sc.Step(`^a committed Go repository with a clear gate$`, f.clearGate)
	sc.Step(`^a committed Go repository with a gate that records whether it starts$`, f.startupProbeGate)
	sc.Step(`^the runtime platform is (.*)$`, f.runtimePlatform)
	sc.Step(`^I run the gauntlet on the real host$`, f.runOnRealHost)
	sc.Step(`^I run the gauntlet$`, f.run)
	sc.Step(`^a completed unverified report is persisted$`, f.completedUnverified)
	sc.Step(`^the platform is rejected before repository, gate, or ledger access$`, f.rejectedBeforeStartup)
}

func (f *platformFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.beforeTree = nil
	f.marker = ""
	f.previousMarker, f.markerWasSet = os.LookupEnv("TOGI_ACCEPTANCE_GATE_MARKER")
	return f.world.Before(ctx, scenario)
}

func (f *platformFeature) after(ctx context.Context, scenario *godog.Scenario, scenarioErr error) (context.Context, error) {
	if f.markerWasSet {
		_ = os.Setenv("TOGI_ACCEPTANCE_GATE_MARKER", f.previousMarker)
	} else {
		_ = os.Unsetenv("TOGI_ACCEPTANCE_GATE_MARKER")
	}
	return f.world.After(ctx, scenario, scenarioErr)
}

func (f *platformFeature) repository() (*harness.Repository, error) {
	repository, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repository"))
	if err != nil {
		return nil, err
	}
	if err := repository.Write("go.mod", "module fixture\n\ngo 1.25\n"); err != nil {
		return nil, err
	}
	if err := repository.Write("fixture.go", "package fixture\n\nfunc Fixture() {}\n"); err != nil {
		return nil, err
	}
	if _, err := repository.Commit("base"); err != nil {
		return nil, err
	}
	if err := repository.Branch("base"); err != nil {
		return nil, err
	}
	f.beforeTree, err = repository.Tree()
	if err != nil {
		return nil, err
	}
	if err := f.world.UseRepository(repository); err != nil {
		return nil, err
	}
	return repository, nil
}

func (f *platformFeature) clearGate() error {
	if _, err := f.repository(); err != nil {
		return err
	}
	if _, err := f.world.Environment().InstallTool("clear-gate", harness.ToolBehavior{Stdout: []byte(`{"Issues":[]}`)}); err != nil {
		return err
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{
		Name: "clear", Description: "clear", Tool: "clear-gate", Normalizer: "golangci-json",
		Command: []string{"clear-gate"}, Scope: "repo", Location: "point",
		SeverityMap: map[string]string{"default": "warning"},
	})
}

func (f *platformFeature) startupProbeGate() error {
	if _, err := f.repository(); err != nil {
		return err
	}
	f.marker = filepath.Join(f.world.Environment().TempRoot, "gate-started")
	if err := os.Setenv("TOGI_ACCEPTANCE_GATE_MARKER", f.marker); err != nil {
		return err
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{
		Name: "startup-probe", Description: "startup probe", Tool: "platform-test-binary", Normalizer: "golangci-json",
		Command: []string{os.Args[0], "-test.run=^TestGateStartupProbe$"}, Scope: "repo", Location: "point",
		SeverityMap: map[string]string{"default": "warning"},
	})
}

func (f *platformFeature) runtimePlatform(platform string) error {
	if platform != "darwin" && platform != "windows" && platform != "freebsd" {
		return fmt.Errorf("unsupported simulated platform %q", platform)
	}
	f.world.Environment().GOOS = platform
	return nil
}

func (f *platformFeature) runOnRealHost(ctx context.Context) error { return f.run(ctx) }

func (f *platformFeature) run(ctx context.Context) error {
	return f.world.Run(ctx, harness.RunRequest{
		Root: f.world.Repository().Root, Base: "base", GateNames: []string{"clear"}, NoColor: true,
	})
}

func (f *platformFeature) completedUnverified() error {
	report, err := f.world.LastRun().Report()
	if err != nil {
		return err
	}
	if report.Verdict != "unverified" || f.world.LastRun().ReportPath() == "" {
		return fmt.Errorf("report=%#v path=%q", report, f.world.LastRun().ReportPath())
	}
	outcome, err := f.world.LastRun().Outcome()
	if err != nil {
		return err
	}
	if outcome.Code != 5 {
		return fmt.Errorf("outcome=%#v, want exit 5", outcome)
	}
	return nil
}

func (f *platformFeature) rejectedBeforeStartup() error {
	observation := f.world.LastRun()
	outcome, err := observation.Outcome()
	if err != nil {
		return err
	}
	if outcome.Code != 70 {
		return fmt.Errorf("outcome=%#v, want exit 70", outcome)
	}
	diagnostic := outcome.Message + "\n" + observation.Stderr()
	if !strings.Contains(diagnostic, "unsupported on this platform") {
		return fmt.Errorf("missing unsupported-platform diagnostic: %q", diagnostic)
	}
	if observation.ReportPath() != "" {
		return fmt.Errorf("rejected run persisted report %q", observation.ReportPath())
	}
	if got := f.world.Environment().RepoResolutions(); got != 0 {
		return fmt.Errorf("repository resolutions=%d, want 0", got)
	}
	if _, err := os.Stat(f.marker); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("gate startup probe ran: %v", err)
	}
	entries, err := os.ReadDir(f.world.Environment().StateRoot)
	if err != nil {
		return fmt.Errorf("read state root: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("ledger state exists: %v", entries)
	}
	afterTree, err := f.world.Repository().Tree()
	if err != nil {
		return err
	}
	if !sameStrings(afterTree, f.beforeTree) {
		return fmt.Errorf("target tree changed: before=%v after=%v", f.beforeTree, afterTree)
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TestGateStartupProbe is a gate command only when its marker is configured.
func TestGateStartupProbe(t *testing.T) {
	marker := os.Getenv("TOGI_ACCEPTANCE_GATE_MARKER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
