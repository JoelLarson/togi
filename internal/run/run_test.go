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
	"github.com/joellarson/togi/internal/gate/gatetest"
	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
	"github.com/joellarson/togi/internal/repoid"
)

func TestComposeReportRecordsDiffScopeWithoutLineRanges(t *testing.T) {
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

	report, err := ComposeReport("run-id", strings.Repeat("d", 40), started, started.Add(time.Second), diff, []GateReport{{
		Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error, finding.Warning}, FixPolicy: gate.ReportOnly, Status: GatePassed,
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
	if report.SchemaVersion != ReportSchemaVersion || report.Diff != want {
		t.Fatalf("report metadata = %#v, want schema 3 and %#v", report, want)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `{"schema_version":3,"run_id":"run-id","repo_id":"` + strings.Repeat("d", 40) + `","diff":{"base_ref":"origin/main","base_commit":"` + strings.Repeat("a", 40) + `","merge_base":"` + strings.Repeat("b", 40) + `","head":"` + strings.Repeat("c", 40) + `","changed_files":2,"changed_lines":3},"started_at":"2026-08-21T12:00:00Z","finished_at":"2026-08-21T12:00:01Z","verdict":"unverified","gates":[{"gate":"lint","language":"go","blocking":["error","warning"],"fix_policy":"report-only","position":0,"status":"passed","duration_ms":0}],"findings":[],"counts":{"errors":0,"warnings":0,"info":0,"occurrences":0}}`
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

func TestComposeReportRejectsInvalidDiffBeforeProjection(t *testing.T) {
	diff := fixtureDiff()
	diff.ChangedLines = 1
	if _, err := ComposeReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime, diff, []GateReport{{
		Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: gate.ReportOnly, Status: GatePassed,
	}}); err == nil {
		t.Fatal("ComposeReport accepted a diff whose changed-line count cannot be verified")
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
		{name: "too few files", mutate: func(diff *Diff) {
			diff.ChangedFiles = 2
			diff.Lines = finding.ChangedLines{"image.png": {}}
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
	validPaths := resolveTestPaths(t, filepath.Join(abs, "config"), filepath.Join(abs, "state"), filepath.Join(abs, "cache"))
	validExecutor := Executor{Enrichers: enricher.NewRegistry()}
	for _, tc := range []struct {
		name    string
		service Service
	}{
		{name: "empty paths", service: Service{Executor: validExecutor, Stdout: io.Discard}},
		{name: "empty registry", service: Service{Paths: validPaths, Executor: Executor{}, Stdout: io.Discard}},
		{name: "empty output", service: Service{Paths: validPaths, Executor: validExecutor}},
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
			if _, err := os.Stat(filepath.Join(abs, "state", "togi")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state touched: %v", err)
			}
		})
	}
}

func TestServiceResolvesExplicitAndDefaultBase(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("lint.go", 2, "golangci-lint/errcheck", "unchecked"), false)...))
	base := gitFixture(t, root, "rev-parse", "refs/remotes/origin/main")

	defaultReport, err := fixtureService(paths, new(bytes.Buffer)).Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}})
	assertVerdictError(t, err)
	if defaultReport.Diff.BaseRef != "origin/main" || defaultReport.Diff.BaseCommit != base {
		t.Fatalf("default diff = %#v, want origin/main at %s", defaultReport.Diff, base)
	}

	explicitReport, err := fixtureService(paths, new(bytes.Buffer)).Run(context.Background(), Options{Root: root, Base: base, GateNames: []string{"lint"}})
	assertVerdictError(t, err)
	if explicitReport.Diff.BaseRef != base || explicitReport.Diff.BaseCommit != base {
		t.Fatalf("explicit diff = %#v, want exact base %s", explicitReport.Diff, base)
	}
	if explicitReport.Diff.MergeBase != defaultReport.Diff.MergeBase || explicitReport.Diff.Head != defaultReport.Diff.Head {
		t.Fatalf("resolved commits differ: default %#v explicit %#v", defaultReport.Diff, explicitReport.Diff)
	}
}

func TestServiceResolvesLocalTrunkWithoutRemote(t *testing.T) {
	root, paths := localTrunkFixtureRepository(t)
	base := gitFixture(t, root, "rev-parse", "main")
	head := gitFixture(t, root, "rev-parse", "HEAD")
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "golangci-lint/errcheck", "unchecked"), false)...))

	report, err := fixtureService(paths, new(bytes.Buffer)).Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}})
	assertVerdictError(t, err)
	if report.Diff.BaseRef != "main" || report.Diff.BaseCommit != base {
		t.Fatalf("diff base = %#v, want local main at %s", report.Diff, base)
	}
	if report.Diff.MergeBase != base || report.Diff.Head != head {
		t.Fatalf("diff commits = %#v, want merge-base %s and head %s", report.Diff, base, head)
	}
}

func TestServiceRejectsInvalidDiffInputsBeforeLedgerOrGates(t *testing.T) {
	for _, test := range []struct {
		name    string
		base    string
		want    string
		prepare func(*testing.T, string)
	}{
		{name: "unstaged", want: "worktree must be clean", prepare: func(t *testing.T, root string) {
			writeDiffTestFile(t, root, "lint.go", "package fixture\nvar lint = 2\n")
		}},
		{name: "staged", want: "worktree must be clean", prepare: func(t *testing.T, root string) {
			writeDiffTestFile(t, root, "lint.go", "package fixture\nvar lint = 2\n")
			gitFixture(t, root, "add", "lint.go")
		}},
		{name: "untracked", want: "worktree must be clean", prepare: func(t *testing.T, root string) { writeDiffTestFile(t, root, "pending.go", "package fixture\n") }},
		{name: "submodule", want: "repositories containing submodules are unsupported", prepare: func(t *testing.T, root string) {
			head := gitFixture(t, root, "rev-parse", "HEAD")
			gitFixture(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/sub")
			gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-m", "track gitlink")
		}},
		{name: "missing default", want: "pass --base", prepare: func(t *testing.T, root string) {
			gitFixture(t, root, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
			gitFixture(t, root, "update-ref", "-d", "refs/remotes/origin/main")
			gitFixture(t, root, "update-ref", "-d", "refs/remotes/origin/master")
			if branch := gitFixture(t, root, "symbolic-ref", "--short", "HEAD"); branch != "feature" {
				gitFixture(t, root, "branch", "-m", "feature")
			}
			gitFixture(t, root, "update-ref", "-d", "refs/heads/main")
			gitFixture(t, root, "update-ref", "-d", "refs/heads/master")
		}},
		{name: "invalid base", base: "bad\nref", want: "base revision is invalid"},
		{name: "unresolved base", base: "refs/heads/missing", want: "selected revision is not a commit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, paths := fixtureRepository(t)
			marker := filepath.Join(t.TempDir(), "executed")
			gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(helperBinary(t), "active", marker, "1ms", marker+".done"))
			if test.prepare != nil {
				test.prepare(t, root)
			}
			_, err := fixtureService(paths, new(bytes.Buffer)).Run(context.Background(), Options{Root: root, Base: test.base, GateNames: []string{"lint"}})
			if err == nil {
				t.Fatal("Run succeeded")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Run error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("gate helper executed: %v", statErr)
			}
			id, resolveErr := repoid.Resolve(context.Background(), root)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if _, statErr := os.Stat(paths.RepoState(id)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("ledger state created: %v", statErr)
			}
		})
	}
}

func TestServiceScopesCommittedFindingsAndProducesStableMetadata(t *testing.T) {
	root, paths, base, head := scopedFixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "entity", gatetest.Command(fixtureCommand(t, fixtureJSON("scope.go", 3, "fixture/entity", "entity finding"), false)...), gatetest.Scope(gate.Diff), gatetest.Location(gate.EntityLocation))
	gatetest.Write(t, paths.GateOverrides(), "point", gatetest.Command(fixtureCommand(t, fixtureJSON("scope.go", 3, "fixture/point", "point finding"), false)...), gatetest.Scope(gate.Diff), gatetest.Location(gate.PointLocation))
	gatetest.Write(t, paths.GateOverrides(), "repository", gatetest.Command(fixtureCommand(t, fixtureJSON("scope.go", 3, "fixture/repository", "repository finding"), false)...), gatetest.Scope(gate.Repo), gatetest.Location(gate.PointLocation))
	baseline := targetTree(t, root)

	runOnce := func() Report {
		service := fixtureService(paths, new(bytes.Buffer))
		service.Executor.Enrichers = enricher.NewRegistry()
		report, err := service.Run(context.Background(), Options{
			Root: root, GateNames: []string{"entity", "point", "repository"}, ReportOnly: true, NoColor: true,
		})
		assertVerdictError(t, err)
		return report
	}
	first := runOnce()
	second := runOnce()

	wantDiff := DiffReport{BaseRef: "origin/main", BaseCommit: base, MergeBase: base, Head: head, ChangedFiles: 1, ChangedLines: 1}
	if first.Diff != wantDiff || second.Diff != wantDiff {
		t.Fatalf("diff metadata:\nfirst  %#v\nsecond %#v\nwant   %#v", first.Diff, second.Diff, wantDiff)
	}
	if got, want := findingKeys(first.Findings), findingKeys(second.Findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("fingerprints changed across runs:\n%v\n%v", got, want)
	}
	if len(first.Findings) != 2 {
		t.Fatalf("findings = %#v, want entity and repository findings", first.Findings)
	}
	if gateByName(t, first, "entity").Status != GateFindings || gateByName(t, first, "point").Status != GatePassed || gateByName(t, first, "repository").Status != GateFindings {
		t.Fatalf("gate reports = %#v", first.Gates)
	}
	for _, item := range first.Findings {
		switch item.Gate {
		case "entity":
			if item.Line != 3 || item.EndLine != 5 {
				t.Fatalf("entity location = %d-%d, want 3-5", item.Line, item.EndLine)
			}
		case "repository":
			if item.Line != 3 || item.EndLine != 0 {
				t.Fatalf("repository location = %d-%d, want point at 3", item.Line, item.EndLine)
			}
		default:
			t.Fatalf("unexpected surviving finding %#v", item)
		}
	}
	if after := targetTree(t, root); !reflect.DeepEqual(after, baseline) {
		t.Fatalf("target repository changed:\nbefore %v\nafter  %v", baseline, after)
	}
}

func TestServiceRejectsZeroRepositoryIdentityBeforeStateUse(t *testing.T) {
	storage := t.TempDir()
	paths := resolveTestPaths(t, filepath.Join(storage, "config"), filepath.Join(storage, "state"), filepath.Join(storage, "cache"))
	service := Service{Paths: paths, Loader: gate.Loader{OverrideDir: paths.GateOverrides()}, Executor: Executor{Enrichers: enricher.NewRegistry()}, Stdout: io.Discard, ResolveRepo: func(context.Context, string) (repoid.ID, error) { return repoid.ID{}, nil }}
	if _, err := service.Run(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("Run error = %v, want identity error", err)
	}
	if _, err := os.Stat(filepath.Join(storage, "state", "togi")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state touched: %v", err)
	}
}

func TestServiceAcceptsCanonicalizedSymlinkRepositoryRoot(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("lint.go", 2, "golangci-lint/errcheck", "unchecked"), false)...))
	link := filepath.Join(t.TempDir(), "repository-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := repoid.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := repoid.New(resolved.Key(), link)
	if err != nil {
		t.Fatal(err)
	}
	service := fixtureService(paths, new(bytes.Buffer))
	service.ResolveRepo = func(context.Context, string) (repoid.ID, error) { return id, nil }
	report, err := service.Run(context.Background(), Options{Root: link, GateNames: []string{"lint"}})
	assertVerdictError(t, err)
	if report.RepoID != id.Key() || id.Root() != root {
		t.Fatalf("report repo = %q identity = %#v", report.RepoID, id)
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
			storage := t.TempDir()
			paths := resolveTestPaths(t, filepath.Join(storage, "config"), tc.state(t, root), filepath.Join(storage, "cache"))
			service := Service{Paths: paths, Executor: Executor{Enrichers: enricher.NewRegistry()}, Stdout: io.Discard}
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
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("lint.go", 2, "golangci-lint/errcheck", "unchecked"), false)...))
	gatetest.Write(t, paths.GateOverrides(), "complexity", gatetest.Command(fixtureCommand(t, fixtureJSON("complex.go", 2, "fixture/complexity", "too complex"), false)...))

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

	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, "", true)...))
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
		for _, name := range []string{
			RawOutputName("lint", "go", "stdout"), RawOutputName("lint", "go", "stderr"),
			RawOutputName("complexity", "go", "stdout"), RawOutputName("complexity", "go", "stderr"),
		} {
			if _, err := os.Stat(filepath.Join(dir, "raw", name)); err != nil {
				t.Fatalf("raw %s: %v", name, err)
			}
		}
	}
}

func TestServiceGateFilteringAndStatusDoesNotExecute(t *testing.T) {
	root, paths := fixtureRepository(t)
	marker := filepath.Join(t.TempDir(), "executed")
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(helperBinary(t), "active", marker, "1ms", marker+".done"))
	gatetest.Write(t, paths.GateOverrides(), "complexity", gatetest.Command(fixtureCommand(t, fixtureJSON("complex.go", 2, "fixture/complexity", "too complex"), false)...))
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
		Paths:       paths,
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

func TestPrepareRunRejectsSelectedBindingWithoutEnricher(t *testing.T) {
	root, paths := fixtureRepository(t)
	marker := filepath.Join(t.TempDir(), "executed")
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(helperBinary(t), "mark", marker))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Executor.Enrichers = enricher.Registry{"rust": enricher.Noop{}}

	_, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, NoColor: true})
	if err == nil || !strings.Contains(err.Error(), `no enricher for language "go"`) {
		t.Fatalf("Run error = %v, want missing enricher error", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("gate executed before enricher validation: %v", statErr)
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

func TestSelectRequestsRetainsConfiguredGauntletPositions(t *testing.T) {
	goGate := func(name string) gate.Gate {
		return gate.Gate{Manifest: gate.Manifest{Name: name}, Bindings: map[string]gate.Binding{"go": {Language: "go"}}}
	}
	loaded := []gate.Gate{
		goGate("first"),
		{Manifest: gate.Manifest{Name: "rust-only"}, Bindings: map[string]gate.Binding{"rust": {Language: "rust"}}},
		goGate("third"),
	}
	full, err := selectRequests(loaded, nil, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 2 || full[0].Position != 0 || full[1].Position != 2 {
		t.Fatalf("full positions = %#v", full)
	}
	filtered, err := selectRequests(loaded, []string{"third"}, "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Position != 2 {
		t.Fatalf("filtered positions = %#v", filtered)
	}
}

func TestServiceRunsGateWithFormerlyUnsafeRawIdentityAlongsideHealthyGate(t *testing.T) {
	root, paths := fixtureRepository(t)
	unsafeName := "lint.with space"
	gatetest.Write(t, paths.GateOverrides(), unsafeName, gatetest.Command(fixtureCommand(t, fixtureJSON("lint.go", 2, "fixture/unsafe", "unsafe identity ran"), false)...))
	gatetest.Write(t, paths.GateOverrides(), "complexity", gatetest.Command(fixtureCommand(t, fixtureJSON("complex.go", 2, "fixture/complexity", "healthy ran"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{unsafeName, "complexity"}, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("Run error = %v, want findings exit", err)
	}
	for _, name := range []string{unsafeName, "complexity"} {
		gateReport := gateByName(t, report, name)
		if gateReport.Status != GateFindings || len(gateReport.Findings) != 1 {
			t.Fatalf("gate %q = %#v", name, gateReport)
		}
		for _, stream := range []string{"stdout", "stderr"} {
			if _, err := os.Stat(filepath.Join(report.Ref.Dir, "raw", RawOutputName(name, "go", stream))); err != nil {
				t.Fatalf("gate %q raw %s: %v", name, stream, err)
			}
		}
	}
}

func TestServicePersistsPassingRunBeforeReturningUnverified(t *testing.T) {
	root, paths := fixtureRepository(t)
	for _, name := range []string{"lint", "complexity"} {
		gatetest.Write(t, paths.GateOverrides(), name, gatetest.Command(helperBinary(t), "emit", fmt.Sprintf("%q", `{"Issues":[]}`), fmt.Sprintf("%q", ""), "0"))
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
	if report.Ref.ID != report.RunID || report.Ref.Dir == "" || filepath.Base(report.Ref.Dir) != report.RunID {
		t.Fatalf("run ref = %#v, report run ID = %q", report.Ref, report.RunID)
	}
	id, err := repoid.Resolve(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := (Ledger{RepoID: id.Key(), RepoState: paths.RepoState(id), RunsDir: paths.RunsDir(id)}).Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if persisted.RunID != report.RunID {
		t.Fatalf("persisted run = %q, want %q", persisted.RunID, report.RunID)
	}
	if persisted.Ref.ID != persisted.RunID || persisted.Ref.Dir != report.Ref.Dir {
		t.Fatalf("persisted ref = %#v, want %#v", persisted.Ref, report.Ref)
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
		gitFixture(t, root, args...)
	}
	baseCommit := gitFixture(t, root, "rev-parse", "HEAD")
	gitFixture(t, root, "update-ref", "refs/remotes/origin/main", baseCommit)
	gitFixture(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	writeDiffTestFile(t, root, "feature.go", "package fixture\nvar feature = true\n")
	gitFixture(t, root, "add", "feature.go")
	gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "feature")
	base := t.TempDir()
	return root, resolveTestPaths(t, filepath.Join(base, "config"), filepath.Join(base, "state"), filepath.Join(base, "cache"))
}

func localTrunkFixtureRepository(t *testing.T) (string, config.Paths) {
	t.Helper()
	root := t.TempDir()
	gitFixture(t, root, "init", "-q")
	gitFixture(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
	writeDiffTestFile(t, root, "base.go", "package fixture\n")
	gitFixture(t, root, "add", "base.go")
	gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "base")
	gitFixture(t, root, "checkout", "-q", "-b", "feature")
	writeDiffTestFile(t, root, "feature.go", "package fixture\n")
	gitFixture(t, root, "add", "feature.go")
	gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "feature one")
	writeDiffTestFile(t, root, "second.go", "package fixture\n")
	gitFixture(t, root, "add", "second.go")
	gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "feature two")
	storage := t.TempDir()
	return root, resolveTestPaths(t, filepath.Join(storage, "config"), filepath.Join(storage, "state"), filepath.Join(storage, "cache"))
}

func scopedFixtureRepository(t *testing.T) (root string, paths config.Paths, base, head string) {
	t.Helper()
	root = t.TempDir()
	gitFixture(t, root, "init", "-q")
	gitFixture(t, root, "symbolic-ref", "HEAD", "refs/heads/feature")
	writeDiffTestFile(t, root, "scope.go", "package fixture\n\nfunc complexity() int {\n\treturn 1\n}\n")
	gitFixture(t, root, "add", "scope.go")
	gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "base")
	base = gitFixture(t, root, "rev-parse", "HEAD")
	gitFixture(t, root, "update-ref", "refs/remotes/origin/main", base)
	gitFixture(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	writeDiffTestFile(t, root, "scope.go", "package fixture\n\nfunc complexity() int {\n\treturn 2\n}\n")
	gitFixture(t, root, "add", "scope.go")
	gitFixture(t, root, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "-qm", "feature")
	head = gitFixture(t, root, "rev-parse", "HEAD")
	storage := t.TempDir()
	paths = resolveTestPaths(t, filepath.Join(storage, "config"), filepath.Join(storage, "state"), filepath.Join(storage, "cache"))
	return root, paths, base, head
}

func fixtureService(paths config.Paths, output *bytes.Buffer) Service {
	return Service{
		Paths:    paths,
		Loader:   gate.Loader{OverrideDir: paths.GateOverrides()},
		Executor: Executor{Enrichers: enricher.NewRegistry()},
		Stdout:   output,
	}
}

func gitFixture(t *testing.T, root string, args ...string) string {
	t.Helper()
	return gitcmdtest.Git(t, root, args...)
}

func assertVerdictError(t *testing.T, err error) {
	t.Helper()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run error = %v, want verdict ExitError", err)
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
	return report, out.String(), report.Ref.Dir, targetTree(t, root)
}

func resolveTestPaths(t *testing.T, configHome, stateHome, cacheHome string) config.Paths {
	t.Helper()
	values := map[string]string{
		"XDG_CONFIG_HOME": configHome,
		"XDG_STATE_HOME":  stateHome,
		"XDG_CACHE_HOME":  cacheHome,
	}
	paths, err := config.Resolve(config.Environment{Getenv: func(key string) string { return values[key] }})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func fixtureJSON(file string, line int, rule, message string) string {
	return fmt.Sprintf(`{"Issues":[{"FromLinter":"%s","Text":"%s","Severity":"warning","Pos":{"Filename":"%s","Line":%d}}]}`, strings.Split(rule, "/")[1], message, file, line)
}

func fixtureCommand(t *testing.T, output string, broken bool) []string {
	t.Helper()
	command := []string{helperBinary(t), "emit", fmt.Sprintf("%q", output), fmt.Sprintf("%q", ""), "1"}
	if broken {
		command = []string{helperBinary(t), "emit", fmt.Sprintf("%q", ""), fmt.Sprintf("%q", "failed"), "2"}
	}
	return command
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
