package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDriverConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable gate fixtures require POSIX shell")
	}
	root, err := findModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	binaryDirectory := t.TempDir()
	binary := filepath.Join(binaryDirectory, "togi")
	if err := buildCLI(root, binary); err != nil {
		t.Fatal(err)
	}

	service := exerciseDriver(t, newServiceFactory())
	cli := exerciseDriver(t, newCLIFactory(binary))
	if service.runOutcome != cli.runOutcome {
		t.Fatalf("run outcome = %d and %d, want equal", service.runOutcome, cli.runOutcome)
	}
	if service.statusOutcome != cli.statusOutcome {
		t.Fatalf("status outcome = %d and %d, want equal", service.statusOutcome, cli.statusOutcome)
	}
	if !reflect.DeepEqual(service.report, cli.report) {
		t.Fatalf("normalized reports differ:\nservice: %#v\ncli: %#v", service.report, cli.report)
	}
	if service.status != cli.status {
		t.Fatalf("normalized status differs:\nservice: %q\ncli: %q", service.status, cli.status)
	}
	if service.show != cli.show || service.lint != cli.lint || service.eject != cli.eject {
		t.Fatalf("wiki output differs:\nservice: %#v\ncli: %#v", service, cli)
	}
}

type driverExercise struct {
	report        Report
	runOutcome    int
	status        string
	statusOutcome int
	show          string
	lint          string
	eject         string
}

func exerciseDriver(t *testing.T, factory DriverFactory) driverExercise {
	t.Helper()
	environment, repository := serviceDriverFixture(t)
	if _, err := environment.InstallTool("lint-tool", ToolBehavior{Stdout: []byte(`{"Issues":[{"FromLinter":"lint","Text":"bad","Severity":"warning","Pos":{"Filename":"source.go","Line":1,"Column":1}}]}`)}); err != nil {
		t.Fatal(err)
	}
	if err := environment.WriteGate(GateDefinition{Name: "external-lint", Description: "fixture lint", Tool: "lint-tool", RuleID: "lint", Command: []string{"lint-tool"}}); err != nil {
		t.Fatal(err)
	}
	gauntlet, err := factory.NewGauntlet(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer gauntlet.Close()
	run, err := gauntlet.Run(context.Background(), RunRequest{Root: repository.Root, Base: "base", GateNames: []string{"external-lint"}, NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run.ReportPath()); err != nil {
		t.Fatalf("report artifact: %v", err)
	}
	if isWithin(repository.Root, run.ReportPath()) {
		t.Fatalf("report artifact %q is inside target repository", run.ReportPath())
	}
	for _, stream := range []string{"stdout", "stderr"} {
		path, ok := run.RawPath("external-lint", "go", stream)
		if !ok {
			t.Fatalf("raw %s artifact missing", stream)
		}
		if isWithin(repository.Root, path) {
			t.Fatalf("raw artifact %q is inside target repository", path)
		}
	}
	report, err := run.Report()
	if err != nil {
		t.Fatal(err)
	}
	runOutcome, err := run.Outcome()
	if err != nil {
		t.Fatal(err)
	}
	history, err := factory.NewHistory(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	status, err := history.Status(context.Background(), StatusRequest{Root: repository.Root, NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	statusOutcome, err := status.Outcome()
	if err != nil {
		t.Fatal(err)
	}
	wiki, err := factory.NewWiki(environment)
	if err != nil {
		t.Fatal(err)
	}
	defer wiki.Close()
	show, err := wiki.Show(context.Background(), "small-composable-functions")
	if err != nil {
		t.Fatal(err)
	}
	lint, err := wiki.Lint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eject, err := wiki.Eject(context.Background(), "small-composable-functions")
	if err != nil {
		t.Fatal(err)
	}
	return driverExercise{
		report: normalizeReport(report), runOutcome: runOutcome.Code,
		status: normalizeDuration(status.Stdout()), statusOutcome: statusOutcome.Code,
		show: show.Stdout(), lint: lint.Stdout(), eject: strings.ReplaceAll(eject.Stdout(), environment.ConfigRoot, "<config>"),
	}
}

func normalizeReport(report Report) Report {
	report.RunID = ""
	report.StartedAt = time.Time{}
	report.FinishedAt = time.Time{}
	for index := range report.Gates {
		report.Gates[index].DurationMS = 0
	}
	return report
}

var durationOutput = regexp.MustCompile(`\([0-9]+ms\)`)

func normalizeDuration(value string) string { return durationOutput.ReplaceAllString(value, "(0ms)") }

func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
