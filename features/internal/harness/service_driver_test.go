package harness

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServiceDriverRunObservesPersistedArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable gate fixtures require POSIX shell")
	}
	environment, repository := serviceDriverFixture(t)
	if _, err := environment.InstallTool("lint-tool", ToolBehavior{Stdout: []byte(`{"Issues":[{"FromLinter":"lint","Text":"bad","Severity":"warning","Pos":{"Filename":"source.go","Line":1,"Column":1}}]}`), Stderr: []byte("diagnostic\\n")}); err != nil {
		t.Fatalf("InstallTool() = %v", err)
	}
	if err := environment.WriteGate(GateDefinition{Name: "external-lint", Description: "fixture lint", Tool: "lint-tool", RuleID: "lint", Command: []string{"lint-tool"}}); err != nil {
		t.Fatalf("WriteGate() = %v", err)
	}

	factory := newServiceFactory()
	driver, err := factory.NewGauntlet(environment)
	if err != nil {
		t.Fatalf("NewGauntlet() = %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	observation, err := driver.Run(context.Background(), RunRequest{ReportOnly: true, Root: repository.Root, Base: "base", GateNames: []string{"external-lint"}, NoColor: true})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if !strings.Contains(observation.Stdout(), "verdict: findings") {
		t.Fatalf("Stdout() = %q, want rendered report", observation.Stdout())
	}
	if observation.Stderr() != "" {
		t.Fatalf("Stderr() = %q, want empty verbose output", observation.Stderr())
	}
	outcome, err := observation.Outcome()
	if err != nil || outcome.Code != 1 {
		t.Fatalf("Outcome() = %#v, %v, want service exit 1", outcome, err)
	}
	report, err := observation.Report()
	if err != nil {
		t.Fatalf("Report() = %v", err)
	}
	if report.RunID == "" || report.Findings[0].Gate != "external-lint" {
		t.Fatalf("Report() = %#v", report)
	}
	if _, err := os.Stat(observation.ReportPath()); err != nil {
		t.Fatalf("persisted report = %v", err)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		path, ok := observation.RawPath("external-lint", "go", stream)
		if !ok {
			t.Fatalf("RawPath(%q) missing", stream)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("raw %s = %v", stream, err)
		}
	}
	if got := environment.RepoResolutions(); got != 1 {
		t.Fatalf("RepoResolutions() = %d, want 1", got)
	}
}

func TestServiceDriverDoesNotReuseOlderReportOnRejectedRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable gate fixtures require POSIX shell")
	}
	environment, repository := serviceDriverFixture(t)
	if _, err := environment.InstallTool("clean-tool", ToolBehavior{}); err != nil {
		t.Fatal(err)
	}
	if err := environment.WriteGate(GateDefinition{Name: "clean", Description: "clean", Tool: "clean-tool", Command: []string{"clean-tool"}}); err != nil {
		t.Fatal(err)
	}
	driver, err := newServiceFactory().NewGauntlet(environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	first, err := driver.Run(context.Background(), RunRequest{ReportOnly: true, Root: repository.Root, Base: "base", GateNames: []string{"clean"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Report(); err != nil {
		t.Fatalf("first Report() = %v", err)
	}
	rejected, err := driver.Run(context.Background(), RunRequest{ReportOnly: true, Root: repository.Root, Base: "base", GateNames: []string{"missing"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rejected.Report(); err == nil {
		t.Fatal("rejected Run() inherited an older report")
	}
	outcome, err := rejected.Outcome()
	if err != nil || outcome.Code != 70 {
		t.Fatalf("rejected Outcome() = %#v, %v, want service error", outcome, err)
	}
}

func TestServiceDriverCloseIsIdempotentAndDoesNotCloseEnvironment(t *testing.T) {
	environment, _ := serviceDriverFixture(t)
	driver, err := newServiceFactory().NewGauntlet(environment)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Close(); err != nil {
		t.Fatalf("first Close() = %v", err)
	}
	if err := driver.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if _, err := os.Stat(environment.TempRoot); err != nil {
		t.Fatalf("driver Close removed environment: %v", err)
	}
}

func TestServiceDriverGeneratesIncreasingRunIDs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable gate fixtures require POSIX shell")
	}
	environment, repository := serviceDriverFixture(t)
	if _, err := environment.InstallTool("clean-tool", ToolBehavior{}); err != nil {
		t.Fatal(err)
	}
	if err := environment.WriteGate(GateDefinition{Name: "clean", Description: "clean", Tool: "clean-tool", Command: []string{"clean-tool"}}); err != nil {
		t.Fatal(err)
	}
	driver, err := newServiceFactory().NewGauntlet(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()

	first, err := driver.Run(context.Background(), RunRequest{ReportOnly: true, Root: repository.Root, Base: "base", GateNames: []string{"clean"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.Run(context.Background(), RunRequest{ReportOnly: true, Root: repository.Root, Base: "base", GateNames: []string{"clean"}})
	if err != nil {
		t.Fatal(err)
	}
	firstReport, err := first.Report()
	if err != nil {
		t.Fatal(err)
	}
	secondReport, err := second.Report()
	if err != nil {
		t.Fatal(err)
	}
	if firstReport.RunID >= secondReport.RunID || !firstReport.StartedAt.Before(secondReport.StartedAt) {
		t.Fatalf("run IDs/times are not increasing: %q at %s, %q at %s", firstReport.RunID, firstReport.StartedAt, secondReport.RunID, secondReport.StartedAt)
	}
}

func TestServiceDriverPassesEnvironmentPlatformToService(t *testing.T) {
	environment, repository := serviceDriverFixture(t)
	environment.GOOS = "darwin"
	driver, err := newServiceFactory().NewGauntlet(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()

	observation, err := driver.Run(context.Background(), RunRequest{ReportOnly: true, Root: repository.Root, Base: "base"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := observation.Outcome()
	if err != nil || outcome.Code != 70 || !strings.Contains(outcome.Message, "unsupported on this platform: darwin") {
		t.Fatalf("Outcome() = %#v, %v", outcome, err)
	}
	if got := environment.RepoResolutions(); got != 0 {
		t.Fatalf("RepoResolutions() = %d, want 0 after platform rejection", got)
	}
}

func serviceDriverFixture(t *testing.T) (*Environment, *Repository) {
	t.Helper()
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	repository, err := NewRepository(filepath.Join(environment.TempRoot, "repository"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Write("source.go", "package fixture\n\nfunc Example() {}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("base"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Branch("base"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Write("source.go", "package fixture\n\nfunc Example() { println(\"feature\") }\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("feature"); err != nil {
		t.Fatal(err)
	}
	if err := environment.Activate(); err != nil {
		t.Fatal(err)
	}
	return environment, repository
}

func TestServiceDriverHistoryAndWiki(t *testing.T) {
	environment, repository := serviceDriverFixture(t)
	factory := newServiceFactory()
	history, err := factory.NewHistory(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	status, err := history.Status(context.Background(), StatusRequest{Root: repository.Root, NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := status.Outcome()
	if err != nil || outcome.Code != 70 || !strings.Contains(outcome.Message, "no complete runs") {
		t.Fatalf("Status outcome = %#v, %v", outcome, err)
	}
	wikiDriver, err := factory.NewWiki(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer wikiDriver.Close()
	show, err := wikiDriver.Show(context.Background(), "small-composable-functions")
	if err != nil || !strings.Contains(show.Stdout(), "Small, composable functions") {
		t.Fatalf("Show() = %q, %v", show.Stdout(), err)
	}
	eject, err := wikiDriver.Eject(context.Background(), "small-composable-functions")
	if err != nil || !strings.Contains(eject.Stdout(), filepath.Join(environment.ConfigRoot, "wiki")) {
		t.Fatalf("Eject() = %q, %v", eject.Stdout(), err)
	}
}
