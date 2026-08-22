//go:build linux

package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/enricher"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/normalizer"
	"github.com/joellarson/togi/internal/repoid"
)

func TestBuildReportRecordsDiffScopeWithoutLineRanges(t *testing.T) {
	started := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	diff := Diff{
		BaseRef:      "origin/main",
		BaseCommit:   strings.Repeat("a", 40),
		MergeBase:    strings.Repeat("b", 40),
		Head:         strings.Repeat("c", 40),
		ChangedFiles: 2,
		ChangedLines: 3,
		Lines: finding.ChangedLines{
			"first.go":  {{Start: 1, End: 2}},
			"second.go": {{Start: 4, End: 4}},
		},
	}

	report, err := buildReport("run-id", "repo-id", started, started.Add(time.Second), diff, []GateReport{{
		Gate: "lint", Language: "go", Status: GatePassed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := DiffReport{
		BaseRef:      diff.BaseRef,
		BaseCommit:   diff.BaseCommit,
		MergeBase:    diff.MergeBase,
		Head:         diff.Head,
		ChangedFiles: diff.ChangedFiles,
		ChangedLines: diff.ChangedLines,
	}
	if report.SchemaVersion != 2 || report.Diff != want {
		t.Fatalf("report metadata = %#v, want schema 2 and %#v", report, want)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `{"schema_version":2,"run_id":"run-id","repo_id":"repo-id","diff":{"base_ref":"origin/main","base_commit":"` + strings.Repeat("a", 40) + `","merge_base":"` + strings.Repeat("b", 40) + `","head":"` + strings.Repeat("c", 40) + `","changed_files":2,"changed_lines":3},"started_at":"2026-08-21T12:00:00Z","finished_at":"2026-08-21T12:00:01Z","verdict":"unverified","gates":[{"gate":"lint","language":"go","status":"passed","duration_ms":0}],"findings":[],"counts":{"errors":0,"warnings":0,"info":0,"occurrences":0}}`
	if got := string(encoded); got != wantJSON {
		t.Fatalf("report JSON = %s, want %s", got, wantJSON)
	}
	var roundTrip Report
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, report) {
		t.Fatalf("round trip = %#v, want %#v", roundTrip, report)
	}
}

func TestBuildReportRejectsInvalidDiffBeforeProjection(t *testing.T) {
	diff := fixtureDiff()
	diff.ChangedLines = 1
	if _, err := buildReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime, diff, []GateReport{{
		Gate: "lint", Language: "go", Status: GatePassed,
	}}); err == nil {
		t.Fatal("buildReport accepted a diff whose changed-line count cannot be verified")
	}
}

func TestValidateDiffRejectsUnverifiableScope(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Diff)
	}{
		{name: "missing lines", mutate: func(diff *Diff) { diff.Lines = nil }},
		{name: "too many files", mutate: func(diff *Diff) {
			diff.ChangedFiles = 1
			diff.Lines = finding.ChangedLines{"first.go": {{Start: 1, End: 1}}, "second.go": {{Start: 3, End: 3}}}
			diff.ChangedLines = 2
		}},
		{name: "false changed-line count", mutate: func(diff *Diff) {
			diff.ChangedFiles = 1
			diff.Lines = finding.ChangedLines{"first.go": {{Start: 1, End: 2}}}
			diff.ChangedLines = 1
		}},
		{name: "overlapping ranges", mutate: func(diff *Diff) {
			diff.ChangedFiles = 1
			diff.Lines = finding.ChangedLines{"first.go": {{Start: 1, End: 2}, {Start: 2, End: 3}}}
			diff.ChangedLines = 4
		}},
		{name: "unsorted ranges", mutate: func(diff *Diff) {
			diff.ChangedFiles = 1
			diff.Lines = finding.ChangedLines{"first.go": {{Start: 3, End: 3}, {Start: 1, End: 1}}}
			diff.ChangedLines = 2
		}},
		{name: "adjacent ranges", mutate: func(diff *Diff) {
			diff.ChangedFiles = 1
			diff.Lines = finding.ChangedLines{"first.go": {{Start: 1, End: 1}, {Start: 2, End: 2}}}
			diff.ChangedLines = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			diff := fixtureDiff()
			test.mutate(&diff)
			if err := validateDiff(diff); err == nil {
				t.Fatal("validateDiff accepted an unverifiable diff")
			}
		})
	}
}

func TestValidateDiffAcceptsBinaryAndZeroLineFiles(t *testing.T) {
	for _, diff := range []Diff{
		fixtureDiff(),
		func() Diff {
			diff := fixtureDiff()
			diff.ChangedFiles = 1
			diff.Lines = finding.ChangedLines{"image.png": {}}
			return diff
		}(),
	} {
		if err := validateDiff(diff); err != nil {
			t.Fatalf("validateDiff(%#v): %v", diff, err)
		}
	}
}

func TestRenderUsesCompilerStyleAndOccurrences(t *testing.T) {
	report := Report{
		Verdict: VerdictFindings,
		Findings: []finding.Finding{
			{File: "z.go", Line: 8, Severity: finding.Error, Message: "later", RuleID: "tool/later"},
			{File: "a.go", Line: 42, Severity: finding.Warning, Message: "complexity 18", RuleID: "gocyclo/complexity", Occurrences: []finding.Occurrence{{Line: 91}, {Line: 104}}},
		},
		Counts: Counts{Errors: 1, Warnings: 3, Occurrences: 4},
		Gates:  []GateReport{{Gate: "lint", Status: GateErrored, Error: "binary missing"}, {Gate: "complexity", Status: GateFindings, DurationMS: 12}},
	}
	var out bytes.Buffer
	if err := Render(&out, report, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	wantFinding := "a.go:42: warning: complexity 18 (gocyclo/complexity)\n    +2 more at lines 91, 104\n"
	if !strings.Contains(got, wantFinding) {
		t.Fatalf("output missing grouped finding:\n%s", got)
	}
	if strings.Index(got, "a.go:42") > strings.Index(got, "z.go:8") {
		t.Fatalf("findings not sorted:\n%s", got)
	}
	for _, want := range []string{"4 findings (1 error, 3 warnings, 0 info)", "lint: errored: binary missing", "complexity: findings (12ms)", "verdict: findings"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("unexpected ANSI in colorless output: %q", got)
	}
}

func TestServiceRejectsUnsupportedPlatformBeforeResolvingRepository(t *testing.T) {
	called := false
	service := Service{GOOS: "darwin", ResolveRepo: func(context.Context, string) (repoid.ID, error) {
		called = true
		return repoid.ID{}, nil
	}}
	if _, err := service.Run(context.Background(), Options{}); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Run error = %v, want ErrUnsupportedPlatform", err)
	}
	if _, err := service.Status(context.Background(), ".", true); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Status error = %v, want ErrUnsupportedPlatform", err)
	}
	if called {
		t.Fatal("repository resolver called on unsupported platform")
	}
}

func TestServiceRejectsUnsafeWiringBeforeRepositoryResolution(t *testing.T) {
	abs := t.TempDir()
	validPaths := config.Paths{Config: filepath.Join(abs, "config"), State: filepath.Join(abs, "state")}
	validExecutor := Executor{Registry: normalizer.NewRegistry(), Enricher: enricher.Noop{}}
	for _, tc := range []struct {
		name    string
		service Service
	}{
		{name: "empty paths", service: Service{Executor: validExecutor, Stdout: io.Discard, Diff: fixtureDiff()}},
		{name: "relative config", service: Service{Paths: config.Paths{Config: "config", State: validPaths.State}, Executor: validExecutor, Stdout: io.Discard, Diff: fixtureDiff()}},
		{name: "relative state", service: Service{Paths: config.Paths{Config: validPaths.Config, State: "state"}, Executor: validExecutor, Stdout: io.Discard, Diff: fixtureDiff()}},
		{name: "empty registry", service: Service{Paths: validPaths, Executor: Executor{Enricher: enricher.Noop{}}, Stdout: io.Discard, Diff: fixtureDiff()}},
		{name: "empty enricher", service: Service{Paths: validPaths, Executor: Executor{Registry: normalizer.NewRegistry()}, Stdout: io.Discard, Diff: fixtureDiff()}},
		{name: "empty output", service: Service{Paths: validPaths, Executor: validExecutor, Diff: fixtureDiff()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			service := tc.service
			service.ResolveRepo = func(context.Context, string) (repoid.ID, error) { called = true; return repoid.ID{}, nil }
			if _, err := service.Run(context.Background(), Options{}); err == nil {
				t.Fatal("Run succeeded")
			}
			if called {
				t.Fatal("repository resolver called with unsafe service wiring")
			}
			if _, err := os.Stat(validPaths.State); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state touched: %v", err)
			}
		})
	}
}

func TestServiceRejectsInvalidDiffBeforeRepositoryResolutionOrSideEffects(t *testing.T) {
	root, paths := fixtureRepository(t)
	marker := filepath.Join(t.TempDir(), "executed")
	writeFixtureGateCommand(t, paths.GateOverrides(), "lint", []string{helperBinary(t), "active", marker, "1ms", marker + ".done"})
	for _, test := range []struct {
		name string
		diff Diff
	}{
		{name: "zero diff", diff: Diff{}},
		{name: "missing lines", diff: func() Diff { diff := fixtureDiff(); diff.Lines = nil; return diff }()},
		{name: "false counts", diff: func() Diff { diff := fixtureDiff(); diff.ChangedLines = 1; return diff }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := false
			service := fixtureService(paths, new(bytes.Buffer))
			service.Diff = test.diff
			service.ResolveRepo = func(context.Context, string) (repoid.ID, error) {
				resolved = true
				return repoid.ID{}, nil
			}
			if _, err := service.Run(context.Background(), Options{Root: root}); err == nil {
				t.Fatal("Run accepted invalid diff")
			}
			if resolved {
				t.Fatal("Run resolved the repository before rejecting its diff")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("gate command ran: %v", err)
			}
			if _, err := os.Stat(paths.State); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state or raw output was created: %v", err)
			}
		})
	}
}

func TestServiceRejectsUnsafeInjectedRepositoryIdentityBeforeStateUse(t *testing.T) {
	root := t.TempDir()
	paths := config.Paths{Config: filepath.Join(t.TempDir(), "config"), State: filepath.Join(t.TempDir(), "state")}
	for _, id := range []repoid.ID{
		{Key: "key", Directory: "repo-key", Root: "relative"},
		{Key: "", Directory: "repo-key", Root: root},
		{Key: "key", Directory: "../escape", Root: root},
		{Key: "key", Directory: " ", Root: root},
		{Key: "key", Directory: "bad:name", Root: root},
	} {
		service := Service{Paths: paths, Executor: Executor{Registry: normalizer.NewRegistry(), Enricher: enricher.Noop{}}, Stdout: io.Discard, Diff: fixtureDiff(), ResolveRepo: func(context.Context, string) (repoid.ID, error) { return id, nil }}
		if _, err := service.Run(context.Background(), Options{}); err == nil {
			t.Fatalf("Run accepted identity %#v", id)
		}
		if _, err := os.Stat(paths.State); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state touched: %v", err)
		}
	}
}

func TestValidateRepositoryIDRequiresFullHexKeyDirectory(t *testing.T) {
	root := t.TempDir()
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)

	for _, id := range []repoid.ID{
		{Key: sha1, Directory: sha1[:12], Root: root},
		{Key: sha1, Directory: "repository-" + sha1, Root: root},
		{Key: "not-hex", Directory: "not-hex", Root: root},
		{Key: strings.ToUpper(sha1), Directory: strings.ToUpper(sha1), Root: root},
	} {
		if err := validateRepositoryID(id); err == nil {
			t.Fatalf("validateRepositoryID(%#v) succeeded", id)
		}
	}

	for _, key := range []string{sha1, sha256} {
		if err := validateRepositoryID(repoid.ID{Key: key, Directory: key, Root: root}); err != nil {
			t.Fatalf("validateRepositoryID(%d-character key): %v", len(key), err)
		}
	}
}

func TestServiceRejectsRepositoryStateInsideTargetWithoutSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state func(*testing.T, string) string
	}{
		{name: "state root is repository", state: func(_ *testing.T, root string) string { return root }},
		{name: "state root is descendant", state: func(_ *testing.T, root string) string { return filepath.Join(root, ".state") }},
		{name: "symlinked state root enters repository", state: func(t *testing.T, root string) string {
			link := filepath.Join(t.TempDir(), "state-link")
			if err := os.Symlink(root, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return link
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := fixtureRepository(t)
			beforeTree := targetTree(t, root)
			beforeInfo, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			paths := config.Paths{Config: filepath.Join(t.TempDir(), "config"), State: tc.state(t, root)}
			service := Service{Paths: paths, Executor: Executor{Registry: normalizer.NewRegistry(), Enricher: enricher.Noop{}}, Stdout: io.Discard, Diff: fixtureDiff()}
			if _, err := service.Run(context.Background(), Options{Root: root}); err == nil || !strings.Contains(err.Error(), "target repository") {
				t.Fatalf("Run error = %v", err)
			}
			if _, err := service.Status(context.Background(), root, true); err == nil || !strings.Contains(err.Error(), "target repository") {
				t.Fatalf("Status error = %v", err)
			}
			afterInfo, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			if beforeInfo.Mode().Perm() != afterInfo.Mode().Perm() {
				t.Fatalf("root mode changed from %04o to %04o", beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())
			}
			if afterTree := targetTree(t, root); !reflect.DeepEqual(beforeTree, afterTree) {
				t.Fatalf("target tree changed:\n%v\n%v", beforeTree, afterTree)
			}
		})
	}
}

func TestExternalRepositoryStateAllowsNonexistentSuffixWithoutCreatingIt(t *testing.T) {
	repository := t.TempDir()
	state := filepath.Join(t.TempDir(), "missing", "nested", "repo-id")
	if err := validateExternalRepoState(repository, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(state))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation created state path: %v", err)
	}
}

func TestReportOnlyRunIsStableAndKeepsErroredGateSeparate(t *testing.T) {
	root, paths := fixtureRepository(t)
	baselineTree := targetTree(t, root)
	writeFixtureGate(t, paths.GateOverrides(), "lint", fixtureJSON("lint.go", 2, "golangci-lint/errcheck", "unchecked"), false)
	writeFixtureGate(t, paths.GateOverrides(), "complexity", fixtureJSON("complex.go", 2, "fixture/complexity", "too complex"), false)

	first, firstOutput, firstDir, firstTree := runFixtureService(t, root, paths)
	for _, name := range []string{"lint", "complexity"} {
		gateReport := gateByName(t, first, name)
		if gateReport.Status != GateFindings || len(gateReport.Findings) == 0 {
			t.Fatalf("healthy gate %s = %#v", name, gateReport)
		}
	}
	second, _, secondDir, secondTree := runFixtureService(t, root, paths)
	if first.RunID == second.RunID {
		t.Fatalf("run IDs equal: %q", first.RunID)
	}
	if got, want := findingKeys(first.Findings), findingKeys(second.Findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("finding sets differ:\n%v\n%v", got, want)
	}
	if !strings.Contains(firstOutput, "verdict: findings") {
		t.Fatalf("output: %s", firstOutput)
	}
	if !reflect.DeepEqual(firstTree, secondTree) {
		t.Fatalf("target tree changed:\n%v\n%v", firstTree, secondTree)
	}
	if !reflect.DeepEqual(baselineTree, firstTree) {
		t.Fatalf("first run changed target tree:\n%v\n%v", baselineTree, firstTree)
	}
	assertPersistedFixtureRuns(t, firstDir, secondDir)

	writeFixtureGate(t, paths.GateOverrides(), "lint", "", true)
	broken, _, _, _ := runFixtureService(t, root, paths)
	if broken.Verdict != VerdictErrored {
		t.Fatalf("verdict = %s", broken.Verdict)
	}
	if gateByName(t, broken, "lint").Status != GateErrored {
		t.Fatal("lint did not error")
	}
	complexity := gateByName(t, broken, "complexity")
	if complexity.Status != GateFindings || len(complexity.Findings) == 0 {
		t.Fatal("healthy complexity findings lost")
	}
}

func assertPersistedFixtureRuns(t *testing.T, directories ...string) {
	t.Helper()
	for _, dir := range directories {
		data, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil {
			t.Fatal(err)
		}
		var decoded Report
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if decoded.RunID == "" {
			t.Fatal("persisted report has no run ID")
		}
		for _, name := range []string{"lint.go.stdout", "lint.go.stderr", "complexity.go.stdout", "complexity.go.stderr"} {
			if _, err := os.Stat(filepath.Join(dir, "raw", name)); err != nil {
				t.Fatalf("raw %s: %v", name, err)
			}
		}
	}
}

func TestServiceGateFilteringAndStatusDoesNotExecute(t *testing.T) {
	root, paths := fixtureRepository(t)
	marker := filepath.Join(t.TempDir(), "executed")
	writeFixtureGateCommand(t, paths.GateOverrides(), "lint", []string{helperBinary(t), "active", marker, "1ms", marker + ".done"})
	writeFixtureGate(t, paths.GateOverrides(), "complexity", fixtureJSON("complex.go", 2, "fixture/complexity", "too complex"), false)
	service := fixtureService(paths, new(bytes.Buffer))
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"complexity", "complexity"}, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("Run error = %v", err)
	}
	if len(report.Gates) != 1 || report.Gates[0].Gate != "complexity" {
		t.Fatalf("gates = %#v", report.Gates)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var roundTrip Report
	if unmarshalErr := json.Unmarshal(encoded, &roundTrip); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if validateErr := validateReport(roundTrip, roundTrip.RunID); validateErr != nil {
		t.Fatalf("round-trip report validation: %v", validateErr)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("filtered gate executed: %v", err)
	}

	statusOut := new(bytes.Buffer)
	statusID, err := repoid.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	statusService := Service{
		Paths:       config.Paths{State: paths.State},
		Stdout:      statusOut,
		ResolveRepo: func(context.Context, string) (repoid.ID, error) { return statusID, nil },
	}
	if _, err := statusService.Status(context.Background(), root, true); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status executed gate: %v", err)
	}
	if !strings.Contains(statusOut.String(), "complexity") {
		t.Fatalf("status output: %s", statusOut)
	}

	if _, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"missing"}}); err == nil || strings.Contains(err.Error(), "exited with") {
		t.Fatalf("unknown gate error = %v", err)
	}
}

func TestSelectRequestsRejectsDuplicateManifestsAndMissingGoBinding(t *testing.T) {
	duplicate := gate.Gate{Manifest: gate.Manifest{Name: "lint"}, Bindings: map[string]gate.Binding{"go": {Language: "go"}}}
	if _, err := selectRequests([]gate.Gate{duplicate, duplicate}, nil, "/repo"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate manifest error = %v", err)
	}
	rustOnly := gate.Gate{Manifest: gate.Manifest{Name: "rust"}, Bindings: map[string]gate.Binding{"rust": {Language: "rust"}}}
	if _, err := selectRequests([]gate.Gate{duplicate, rustOnly}, []string{"lint", "rust"}, "/repo"); err == nil || !strings.Contains(err.Error(), "Go binding") {
		t.Fatalf("missing Go binding error = %v", err)
	}
}

func TestServicePersistsPassingRunBeforeReturningUnverified(t *testing.T) {
	root, paths := fixtureRepository(t)
	for _, name := range []string{"lint", "complexity"} {
		writeFixtureGateCommand(t, paths.GateOverrides(), name, []string{helperBinary(t), "emit", fmt.Sprintf("%q", `{"Issues":[]}`), fmt.Sprintf("%q", ""), "0"})
	}
	output := new(bytes.Buffer)
	service := fixtureService(paths, output)
	report, err := service.Run(context.Background(), Options{Root: root, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("Run error = %v, want exit 5", err)
	}
	if report.Verdict != VerdictUnverified || report.Counts.Occurrences != 0 {
		t.Fatalf("report = %#v", report)
	}
	id, err := repoid.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := (Ledger{RepoState: paths.RepoState(id.Directory)}).Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if persisted.RunID != report.RunID {
		t.Fatalf("persisted run = %q, want %q", persisted.RunID, report.RunID)
	}
}

func fixtureRepository(t *testing.T) (string, config.Paths) {
	t.Helper()
	root := t.TempDir()
	for name, contents := range map[string]string{"lint.go": "package fixture\nvar lint = 1\n", "complex.go": "package fixture\nfunc complex() {}\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	base := t.TempDir()
	return root, config.Paths{Config: filepath.Join(base, "config"), State: filepath.Join(base, "state"), Cache: filepath.Join(base, "cache")}
}

func fixtureService(paths config.Paths, output *bytes.Buffer) Service {
	return Service{
		Paths:    paths,
		Loader:   gate.Loader{OverrideDir: paths.GateOverrides()},
		Executor: Executor{Registry: normalizer.NewRegistry(), Enricher: enricher.Noop{}},
		Stdout:   output,
		Diff:     fixtureDiff(),
	}
}

func fixtureDiff() Diff {
	return Diff{
		BaseRef:    "origin/main",
		BaseCommit: strings.Repeat("a", 40),
		MergeBase:  strings.Repeat("b", 40),
		Head:       strings.Repeat("c", 40),
		Lines:      finding.ChangedLines{},
	}
}

func runFixtureService(t *testing.T, root string, paths config.Paths) (Report, string, string, []string) {
	t.Helper()
	out := new(bytes.Buffer)
	service := fixtureService(paths, out)
	report, err := service.Run(context.Background(), Options{Root: root, ReportOnly: true, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || (exitErr.Code != 1 && exitErr.Code != 4) {
		t.Fatalf("Run error = %v", err)
	}
	if exitErr.Code != ExitCode(report.Verdict) {
		t.Fatalf("exit code = %d, want %d for %s", exitErr.Code, ExitCode(report.Verdict), report.Verdict)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var roundTrip Report
	if unmarshalErr := json.Unmarshal(encoded, &roundTrip); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if validateErr := validateReport(roundTrip, roundTrip.RunID); validateErr != nil {
		t.Fatalf("round-trip report validation: %v", validateErr)
	}
	id, err := repoid.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(paths.RepoState(id.Directory), "runs", report.RunID)
	return report, out.String(), dir, targetTree(t, root)
}

func fixtureJSON(file string, line int, rule, message string) string {
	return fmt.Sprintf(`{"Issues":[{"FromLinter":"%s","Text":"%s","Severity":"warning","Pos":{"Filename":"%s","Line":%d}}]}`, strings.Split(rule, "/")[1], message, file, line)
}

func writeFixtureGate(t *testing.T, override, name, output string, broken bool) {
	t.Helper()
	command := []string{helperBinary(t), "emit", fmt.Sprintf("%q", output), fmt.Sprintf("%q", ""), "1"}
	if broken {
		command = []string{helperBinary(t), "emit", fmt.Sprintf("%q", ""), fmt.Sprintf("%q", "failed"), "2"}
	}
	writeFixtureGateCommand(t, override, name, command)
}

func writeFixtureGateCommand(t *testing.T, override, name string, command []string) {
	t.Helper()
	dir := filepath.Join(override, name, "go")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name = %q\ndescription = %q\ncost_class = \"fast\"\nfix_policy = \"report-only\"\nscope = \"repo\"\nblocking = [\"error\", \"warning\"]\ntimeout = \"5s\"\n", name, name)
	if err := os.WriteFile(filepath.Join(override, name, "gate.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	binding := fmt.Sprintf("language = \"go\"\ntool = \"fixture\"\ncommand = %s\nsuccess_exit_codes = [0]\nfinding_exit_codes = [1]\nnormalizer = \"golangci-json\"\n[severity_map]\nwarning = \"warning\"\ndefault = \"warning\"\n", encoded)
	if err := os.WriteFile(filepath.Join(dir, "binding.toml"), []byte(binding), 0o600); err != nil {
		t.Fatal(err)
	}
}

func findingKeys(items []finding.Finding) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fmt.Sprintf("%s:%d:%s:%v", item.Fingerprint, item.Line, item.Severity, item.Occurrences))
	}
	sort.Strings(keys)
	return keys
}

func gateByName(t *testing.T, report Report, name string) GateReport {
	t.Helper()
	for _, item := range report.Gates {
		if item.Gate == name {
			return item
		}
	}
	t.Fatalf("gate %q missing", name)
	return GateReport{}
}

func targetTree(t *testing.T, root string) []string {
	t.Helper()
	var result []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			result = append(result, fmt.Sprintf("%s:%x", rel, sha256.Sum256(contents)))
		} else {
			result = append(result, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func TestRenderColorCanBeDisabled(t *testing.T) {
	report := Report{Verdict: VerdictUnverified, Gates: []GateReport{{Gate: "lint", Status: GateErrored, Error: "\x1b[31mboom\nnext"}}}
	for _, tc := range []struct {
		name string
		opts RenderOptions
		ansi bool
	}{
		{name: "enabled", opts: RenderOptions{Color: true}, ansi: true},
		{name: "disabled", opts: RenderOptions{Color: true, NoColor: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := Render(&out, report, tc.opts); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(out.String(), "\x1b["); got != tc.ansi {
				t.Fatalf("ANSI present = %v, want %v; output %q", got, tc.ansi, out.String())
			}
			if strings.Contains(out.String(), "boom\nnext") {
				t.Fatalf("report data injected a line break: %q", out.String())
			}
		})
	}
}

func TestExitCodeAndError(t *testing.T) {
	for _, tc := range []struct {
		verdict Verdict
		want    int
	}{
		{VerdictUnverified, 5}, {VerdictFindings, 1}, {VerdictErrored, 4},
	} {
		if got := ExitCode(tc.verdict); got != tc.want {
			t.Fatalf("%s = %d, want %d", tc.verdict, got, tc.want)
		}
	}
	cause := errors.New("findings remain")
	err := &ExitError{Code: 1, Err: cause}
	if !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatalf("exit error does not wrap cause: %v", err)
	}
}
