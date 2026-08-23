package harness

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCLIDriverRunsCompiledBinaryAndObservesPersistedArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable gate fixtures require POSIX shell")
	}
	environment, repository := serviceDriverFixture(t)
	if _, err := environment.InstallTool("lint-tool", ToolBehavior{Stdout: []byte(`{"Issues":[{"FromLinter":"lint","Text":"bad","Severity":"warning","Pos":{"Filename":"source.go","Line":1,"Column":1}}]}`), Stderr: []byte("diagnostic\\n")}); err != nil {
		t.Fatal(err)
	}
	if err := environment.WriteGate(GateDefinition{Name: "external-lint", Description: "fixture lint", Tool: "lint-tool", RuleID: "lint", Command: []string{"lint-tool"}}); err != nil {
		t.Fatal(err)
	}
	binary := buildAcceptanceCLITestBinary(t)
	driver, err := newCLIFactory(binary).NewGauntlet(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()

	observation, err := driver.Run(context.Background(), RunRequest{Root: repository.Root, Base: "base", GateNames: []string{"external-lint"}, NoColor: true})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if !strings.Contains(observation.Stdout(), "verdict: findings") {
		t.Fatalf("Stdout() = %q, want rendered report", observation.Stdout())
	}
	outcome, err := observation.Outcome()
	if err != nil || outcome.Code != 1 {
		t.Fatalf("Outcome() = %#v, %v, want process exit 1", outcome, err)
	}
	if _, err := observation.Report(); err != nil {
		t.Fatalf("Report() = %v", err)
	}
	if _, err := os.Stat(observation.ReportPath()); err != nil {
		t.Fatalf("persisted report = %v", err)
	}
	if strings.HasPrefix(observation.ReportPath(), repository.Root+string(filepath.Separator)) {
		t.Fatalf("report path %q is inside target repository", observation.ReportPath())
	}
	if _, ok := observation.RawPath("external-lint", "go", "stdout"); !ok {
		t.Fatal("stdout raw artifact missing")
	}
}

func TestCLIDriverStatusAndWikiReturnCommandObservations(t *testing.T) {
	environment, repository := serviceDriverFixture(t)
	binary := buildAcceptanceCLITestBinary(t)
	factory := newCLIFactory(binary)
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
	if err != nil || outcome.Code != 70 {
		t.Fatalf("Status outcome = %#v, %v", outcome, err)
	}
	wiki, err := factory.NewWiki(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer wiki.Close()
	show, err := wiki.Show(context.Background(), "small-composable-functions")
	if err != nil || !strings.Contains(show.Stdout(), "Small, composable functions") {
		t.Fatalf("Show() = %q, %v", show.Stdout(), err)
	}
}

func TestCLIFactoryRejectsMissingCompiledBinary(t *testing.T) {
	environment, _ := serviceDriverFixture(t)
	if _, err := newCLIFactory("").NewGauntlet(environment); err == nil {
		t.Fatal("NewGauntlet() error = nil, want missing compiled binary")
	}
}

func buildAcceptanceCLITestBinary(t *testing.T) string {
	t.Helper()
	root, err := findModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "togi")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := buildCLI(root, binary); err != nil {
		t.Fatal(err)
	}
	return binary
}
