package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/run"
	"github.com/joellarson/togi/internal/wiki"
)

var validErroredReport = []byte(`{"schema_version":1,"run_id":"run-1","repo_id":"repo-1","diff":{"base_ref":"main","base_commit":"a","merge_base":"a","head":"b","changed_files":1,"changed_lines":1},"started_at":"2026-08-22T00:00:00Z","finished_at":"2026-08-22T00:00:01Z","verdict":"errored","gates":[],"findings":[],"counts":{"errors":0,"warnings":0,"info":0,"occurrences":0}}`)

func TestReportComesOnlyFromPersistedBytes(t *testing.T) {
	obs := newServiceRunObservation(nil, nil, &run.ExitError{Code: 4}, nil, "")
	if _, err := obs.Report(); err == nil {
		t.Fatal("Report succeeded without persisted report bytes")
	}
}

func TestOutcomeDoesNotInferFromReport(t *testing.T) {
	obs := newProcessRunObservation(nil, nil, 1, validErroredReport, "report.json")
	got, err := obs.Outcome()
	if err != nil || got.Code != 1 {
		t.Fatalf("Outcome() = %#v, %v, want process exit 1", got, err)
	}
}

func TestServiceOutcomeCodes(t *testing.T) {
	for _, code := range []int{1, 4, 5} {
		t.Run("exit", func(t *testing.T) {
			obs := newServiceRunObservation(nil, nil, &run.ExitError{Code: code}, nil, "")
			got, err := obs.Outcome()
			if err != nil || got.Code != code {
				t.Fatalf("Outcome() = %#v, %v, want code %d", got, err, code)
			}
		})
	}
}

func TestServiceOutcomeClassifiesWikiAndUnexpectedErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"wiki aliases", wiki.ErrConflictingAliases, 1},
		{"wrapped wiki aliases", fmt.Errorf("context: %w", wiki.ErrConflictingAliases), 1},
		{"unexpected", errors.New("broken"), 70},
		{"nil", nil, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			obs := newServiceRunObservation(nil, nil, test.err, nil, "")
			got, err := obs.Outcome()
			if err != nil || got.Code != test.want {
				t.Fatalf("Outcome() = %#v, %v, want code %d", got, err, test.want)
			}
		})
	}
}

func TestProcessOutcomeUsesCapturedExitCode(t *testing.T) {
	for _, code := range []int{0, 1, 4, 5, 70} {
		t.Run("exit", func(t *testing.T) {
			obs := newProcessRunObservation(nil, nil, code, validErroredReport, "report.json")
			got, err := obs.Outcome()
			if err != nil || got.Code != code {
				t.Fatalf("Outcome() = %#v, %v, want code %d", got, err, code)
			}
		})
	}
}

func TestReportRejectsMalformedUnknownAndTrailingJSON(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"malformed", []byte(`{`)},
		{"unknown field", append(validErroredReport[:len(validErroredReport)-1], []byte(`,"unexpected":true}`)...)},
		{"trailing document", append(append([]byte(nil), validErroredReport...), []byte(` {}`)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			obs := newProcessRunObservation(nil, nil, 0, test.body, "report.json")
			if _, err := obs.Report(); err == nil {
				t.Fatal("Report() succeeded")
			}
		})
	}
}

func TestReportPathAndRawArtifactsAreProvenanceOnly(t *testing.T) {
	obs := newProcessRunObservation([]byte("rendered report\n"), []byte("diagnostic\n"), 4, validErroredReport, "/state/run/report.json")
	obs.rawPaths = map[string]string{"lint\x00go\x00stdout": "/state/run/raw/lint-go.stdout"}

	if got, want := obs.ReportPath(), "/state/run/report.json"; got != want {
		t.Fatalf("ReportPath() = %q, want %q", got, want)
	}
	if got, ok := obs.RawPath("lint", "go", "stdout"); !ok || got != "/state/run/raw/lint-go.stdout" {
		t.Fatalf("RawPath() = %q, %t", got, ok)
	}
	if got := obs.Stdout(); strings.Contains(got, "raw/lint") || got != "rendered report\n" {
		t.Fatalf("Stdout() = %q, want rendered output only", got)
	}
}

func TestObservationCopiesInputBytesAndRawPaths(t *testing.T) {
	stdout := []byte("stdout")
	stderr := []byte("stderr")
	report := append([]byte(nil), validErroredReport...)
	rawPaths := map[string]string{"gate\x00go\x00stderr": "raw"}
	obs := newRunObservation(stdout, stderr, processExit{code: 0}, report, "report.json", rawPaths)

	stdout[0] = 'X'
	stderr[0] = 'X'
	report[0] = 'X'
	if got := obs.Stdout(); got != "stdout" {
		t.Fatalf("Stdout() = %q", got)
	}
	if got := obs.Stderr(); got != "stderr" {
		t.Fatalf("Stderr() = %q", got)
	}
	if _, err := obs.Report(); err != nil {
		t.Fatalf("Report() = %v", err)
	}

	rawPaths["gate\x00go\x00stderr"] = "changed"
	if got, _ := obs.RawPath("gate", "go", "stderr"); got != "raw" {
		t.Fatalf("RawPath() = %q", got)
	}
}

func TestEnvironmentActivatesIsolatedRootsAndRestoresProcess(t *testing.T) {
	previousHome, homeWasSet := os.LookupEnv("HOME")
	previousScenario, scenarioWasSet := os.LookupEnv("TOGI_ACCEPTANCE_SCENARIO")
	if err := os.Setenv("TOGI_ACCEPTANCE_SCENARIO", "outside"); err != nil {
		t.Fatal(err)
	}

	environment, err := NewEnvironment()
	if err != nil {
		t.Fatalf("NewEnvironment() = %v", err)
	}
	if err := environment.Setenv("TOGI_ACCEPTANCE_SCENARIO", "inside"); err != nil {
		t.Fatalf("Setenv() = %v", err)
	}
	if err := environment.Activate(); err != nil {
		t.Fatalf("Activate() = %v", err)
	}
	if got := os.Getenv("HOME"); got != environment.Home {
		t.Fatalf("HOME = %q, want %q", got, environment.Home)
	}
	if got := os.Getenv("XDG_CONFIG_HOME"); got != filepath.Dir(environment.ConfigRoot) {
		t.Fatalf("XDG_CONFIG_HOME = %q", got)
	}
	if got := os.Getenv("TOGI_ACCEPTANCE_SCENARIO"); got != "inside" {
		t.Fatalf("scenario variable = %q", got)
	}
	if err := environment.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if got, ok := os.LookupEnv("HOME"); got != previousHome || ok != homeWasSet {
		t.Fatalf("HOME after Close() = %q, %t", got, ok)
	}
	if got, ok := os.LookupEnv("TOGI_ACCEPTANCE_SCENARIO"); got != "outside" || !ok {
		t.Fatalf("scenario after Close() = %q, %t", got, ok)
	}
	if _, err := os.Stat(environment.TempRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scenario root still exists: %v", err)
	}
	if scenarioWasSet {
		_ = os.Setenv("TOGI_ACCEPTANCE_SCENARIO", previousScenario)
	} else {
		_ = os.Unsetenv("TOGI_ACCEPTANCE_SCENARIO")
	}
}

func TestEnvironmentEnvironUsesMatchingXDGRoots(t *testing.T) {
	moduleCache := goEnvironment(t, "GOMODCACHE")
	buildCache := goEnvironment(t, "GOCACHE")
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatalf("NewEnvironment() = %v", err)
	}
	t.Cleanup(func() { _ = environment.Close() })

	values := make(map[string]string)
	for _, entry := range environment.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if got, want := values["XDG_CONFIG_HOME"], filepath.Dir(environment.ConfigRoot); got != want {
		t.Fatalf("XDG_CONFIG_HOME = %q, want %q", got, want)
	}
	if got, want := values["XDG_STATE_HOME"], filepath.Dir(environment.StateRoot); got != want {
		t.Fatalf("XDG_STATE_HOME = %q, want %q", got, want)
	}
	if got, want := values["XDG_CACHE_HOME"], filepath.Dir(environment.CacheRoot); got != want {
		t.Fatalf("XDG_CACHE_HOME = %q, want %q", got, want)
	}
	if got := values["PATH"]; !strings.HasPrefix(got, environment.BinRoot+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q", got)
	}
	if got := values["GOMODCACHE"]; got != moduleCache {
		t.Fatalf("GOMODCACHE = %q, want %q", got, moduleCache)
	}
	if got := values["GOCACHE"]; got != buildCache {
		t.Fatalf("GOCACHE = %q, want %q", got, buildCache)
	}
}

func goEnvironment(t *testing.T, key string) string {
	t.Helper()
	output, err := exec.Command("go", "env", key).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", key, err)
	}
	return strings.TrimSpace(string(output))
}
