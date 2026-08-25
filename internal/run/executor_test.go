//go:build linux

package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/enricher"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/runner"
)

const helperSource = `package main
import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)
func main() {
	if len(os.Args) < 2 { os.Exit(99) }
	switch os.Args[1] {
	case "emit":
		stdout, _ := strconv.Unquote(os.Args[2])
		stderr, _ := strconv.Unquote(os.Args[3])
		code, _ := strconv.Atoi(os.Args[4])
		fmt.Fprint(os.Stdout, stdout)
		fmt.Fprint(os.Stderr, stderr)
		os.Exit(code)
	case "large":
		n, _ := strconv.Atoi(os.Args[2])
		fmt.Fprint(os.Stdout, strings.Repeat("x", n))
		fmt.Fprint(os.Stderr, strings.Repeat("y", n))
	case "sleep":
		d, _ := time.ParseDuration(os.Args[2])
		time.Sleep(d)
	case "version":
		fmt.Fprint(os.Stdout, os.Args[2])
	case "record-dir":
		wd, _ := os.Getwd()
		fmt.Fprint(os.Stdout, wd)
	case "mark":
		_ = os.WriteFile(os.Args[2], []byte("executed"), 0600)
	case "active":
		path := os.Args[2]
		d, _ := time.ParseDuration(os.Args[3])
		_ = os.WriteFile(path, []byte("active"), 0600)
		time.Sleep(d)
		if len(os.Args) > 4 { _ = os.WriteFile(os.Args[4], []byte("complete"), 0600) }
		_ = os.Remove(path)
	case "panic":
		panic("helper crash")
	case "spawn":
		child := exec.Command(os.Args[0], "sleep", os.Args[2])
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil { os.Exit(97) }
	case "spawn-survivor":
		child := exec.Command(os.Args[0], "survivor", os.Args[2], os.Args[3])
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil { os.Exit(97) }
		time.Sleep(5 * time.Second)
	case "survivor":
		_ = os.WriteFile(os.Args[2], []byte("started"), 0600)
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(os.Args[3], []byte("survived"), 0600)
	default:
		os.Exit(98)
	}
}
`

var (
	helperOnce sync.Once
	helperPath string
	helperErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if helperPath != "" {
		_ = os.RemoveAll(filepath.Dir(helperPath))
	}
	os.Exit(code)
}

func helperBinary(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "togi-executor-helper-")
		if err != nil {
			helperErr = err
			return
		}
		source := filepath.Join(dir, "main.go")
		if err := os.WriteFile(source, []byte(helperSource), 0o600); err != nil {
			helperErr = err
			return
		}
		helperPath = filepath.Join(dir, "helper")
		command := exec.Command("go", "build", "-o", helperPath, source)
		if output, err := command.CombinedOutput(); err != nil {
			helperErr = fmt.Errorf("build helper: %w: %s", err, output)
		}
	})
	if helperErr != nil {
		t.Fatal(helperErr)
	}
	return helperPath
}

type rawWrite struct {
	stream string
	raw    []byte
}

type memoryRawSink struct {
	mu     sync.Mutex
	writes []rawWrite
	err    map[string]error
}

func (s *memoryRawSink) WriteRaw(stream string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, rawWrite{stream: stream, raw: append([]byte(nil), raw...)})
	return s.err[stream]
}

func (s *memoryRawSink) raw(stream string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, write := range s.writes {
		if write.stream == stream {
			return append([]byte(nil), write.raw...)
		}
	}
	return nil
}

type recordingEnricher struct {
	mu       sync.Mutex
	calls    int
	contexts []enricher.Context
	err      error
	errFor   func(enricher.Context) error
	mutate   func([]finding.Finding) []finding.Finding
}

func (e *recordingEnricher) Enrich(_ context.Context, ctx enricher.Context, in []finding.Finding) ([]finding.Finding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.contexts = append(e.contexts, ctx)
	if e.errFor != nil {
		if err := e.errFor(ctx); err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.mutate != nil {
		return e.mutate(in), nil
	}
	return in, nil
}

func executeFixture(t *testing.T, root string, binding gate.Binding, manifest gate.Manifest) (GateReport, *memoryRawSink, *recordingEnricher) {
	t.Helper()
	if manifest.Name == "" {
		manifest.Name = "test"
	}
	if manifest.Timeout == 0 {
		manifest.Timeout = 5 * time.Second
	}
	compiledGate, compiledBinding := compileGateFixture(t, manifest, binding)
	store := &memoryRawSink{}
	enrich := &recordingEnricher{}
	report := (Executor{Enrichers: enricher.Registry{"go": enrich}}).Execute(context.Background(), compileRequest(t, Request{
		Gate: compiledGate, Binding: compiledBinding, Root: root, RawSink: store,
	}))
	return report, store, enrich
}

func compileGateFixture(t *testing.T, manifest gate.Manifest, binding gate.Binding) (gate.Gate, gate.Binding) {
	t.Helper()
	if manifest.Description == "" {
		manifest.Description = "test gate"
	}
	if manifest.CostClass == "" {
		manifest.CostClass = gate.Fast
	}
	if manifest.FixPolicy == "" {
		manifest.FixPolicy = gate.ReportOnly
	}
	if manifest.Scope == "" {
		manifest.Scope = gate.Repo
	}
	if manifest.Location == "" {
		manifest.Location = gate.PointLocation
	}
	if manifest.Blocking == nil {
		manifest.Blocking = []finding.Severity{finding.Error, finding.Warning}
	}
	compiled, err := gate.Compile(manifest, map[string]gate.Binding{binding.Language: binding})
	if err != nil {
		t.Fatalf("compile gate fixture: %v", err)
	}
	return compiled, compiled.Bindings[binding.Language]
}

func compileRequest(t *testing.T, request Request) Request {
	t.Helper()
	request.Gate, request.Binding = compileGateFixture(t, request.Gate.Manifest, request.Binding)
	return request
}

func emitCommand(t *testing.T, stdout, stderr string, code int) []string {
	t.Helper()
	return []string{helperBinary(t), "emit", strconvQuote(stdout), strconvQuote(stderr), fmt.Sprint(code)}
}

func strconvQuote(value string) string { return fmt.Sprintf("%q", value) }

func writeSource(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteNormalizesEnrichesAndGroupsGolangCI(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nfunc duplicate() {}\n")
	raw := `{"Issues":[{"FromLinter":"dupl","Text":"duplicate","Severity":"warning","Pos":{"Filename":"source.go","Line":2,"Column":1}},{"FromLinter":"dupl","Text":"duplicate again","Severity":"error","Pos":{"Filename":"source.go","Line":2,"Column":1}}]}`
	binding := gate.Binding{
		Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 1),
		SuccessExitCodes: []int{0}, FindingExitCodes: []int{1}, Normalizer: "golangci-json",
		SeverityMap: map[string]finding.Severity{"warning": finding.Warning, "error": finding.Error},
	}

	report, store, enrich := executeFixture(t, root, binding, gate.Manifest{Name: "lint"})

	if report.Status != GateFindings {
		t.Fatalf("status = %q, want %q; error=%q", report.Status, GateFindings, report.Error)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %#v, want one grouped finding", report.Findings)
	}
	if report.Findings[0].Severity != finding.Error {
		t.Fatalf("severity = %q, want error", report.Findings[0].Severity)
	}
	if enrich.calls != 1 || !reflect.DeepEqual(enrich.contexts, []enricher.Context{{Root: root, Location: gate.PointLocation}}) {
		t.Fatalf("enricher calls = %d, contexts = %#v", enrich.calls, enrich.contexts)
	}
	if !slices.Equal(store.raw("stdout"), []byte(raw)) || len(store.raw("stderr")) != 0 {
		t.Fatalf("persisted stdout/stderr = %q / %q", store.raw("stdout"), store.raw("stderr"))
	}
}

func TestExecuteNormalizesRegexAndAcceptsCleanSuccess(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nfunc complex() {}\n")
	binding := gate.Binding{
		Language: "go", Tool: "fixture", Command: emitCommand(t, "17 pkg complex source.go:2:1\n", "", 0),
		SuccessExitCodes: []int{0}, FindingExitCodes: []int{1},
		Normalizer: `regex:^(?P<value>\d+) \S+ (?P<symbol>\S+) (?P<file>[^:]+):(?P<line>\d+):\d+$`,
		RuleID:     "gocyclo/complexity", Message: "complexity {{.value}} in {{.symbol}}",
		SeverityMap: map[string]finding.Severity{"default": finding.Warning},
	}
	report, _, _ := executeFixture(t, root, binding, gate.Manifest{Name: "complexity"})
	if report.Status != GateFindings || len(report.Findings) != 1 {
		t.Fatalf("report = %#v, want findings", report)
	}
	if report.Findings[0].Message != "complexity 17 in complex" {
		t.Fatalf("message = %q", report.Findings[0].Message)
	}

	binding.Command = emitCommand(t, "", "", 0)
	report, _, enrich := executeFixture(t, root, binding, gate.Manifest{Name: "complexity"})
	if report.Status != GatePassed || len(report.Findings) != 0 || enrich.calls != 1 {
		t.Fatalf("clean report = %#v, enrich calls=%d", report, enrich.calls)
	}
}

func TestExecuteGroupsBeforeEnrichThenFiltersDiffScope(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar same = 1\nvar same = 1\nvar same = 1\n")
	raw := `{"Issues":[{"FromLinter":"check","Text":"same","Severity":"warning","Pos":{"Filename":"source.go","Line":2}},{"FromLinter":"check","Text":"same","Severity":"error","Pos":{"Filename":"source.go","Line":3}},{"FromLinter":"check","Text":"same","Severity":"info","Pos":{"Filename":"source.go","Line":4}}]}`
	binding := gate.Binding{
		Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0),
		SuccessExitCodes: []int{0}, Normalizer: "golangci-json",
		SeverityMap: map[string]finding.Severity{"warning": finding.Warning, "error": finding.Error, "info": finding.Info},
	}
	var enrichInput []finding.Finding
	enrich := &recordingEnricher{mutate: func(in []finding.Finding) []finding.Finding {
		enrichInput = append([]finding.Finding(nil), in...)
		in[0].EndLine = 2
		in[0].Occurrences = []finding.Occurrence{{Line: 3, EndLine: 3}, {Line: 4, EndLine: 6}}
		return in
	}}
	report := (Executor{Enrichers: enricher.Registry{"go": enrich}}).Execute(context.Background(), compileRequest(t, Request{
		Gate:    gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second, Scope: gate.Diff, Location: gate.EntityLocation}},
		Binding: binding, Root: root, RawSink: &memoryRawSink{},
		ChangedLines: finding.ChangedLines{"source.go": {{Start: 6, End: 6}}},
	}))

	if len(enrichInput) != 1 {
		t.Fatalf("enricher received %d findings, want the identical hits grouped into one", len(enrichInput))
	}
	if enrichInput[0].Severity != finding.Error || enrichInput[0].Line != 2 ||
		!reflect.DeepEqual(enrichInput[0].Occurrences, []finding.Occurrence{{Line: 3}, {Line: 4}}) {
		t.Fatalf("grouped enricher input = %#v", enrichInput[0])
	}
	if report.Status != GateFindings || len(report.Findings) != 1 {
		t.Fatalf("report = %#v", report)
	}
	got := report.Findings[0]
	if got.Severity != finding.Error || got.Line != 4 || got.EndLine != 6 || len(got.Occurrences) != 0 {
		t.Fatalf("scoped finding = %#v", got)
	}
	if !reflect.DeepEqual(enrich.contexts, []enricher.Context{{Root: root, Location: gate.EntityLocation}}) {
		t.Fatalf("enricher contexts = %#v", enrich.contexts)
	}
}

func TestExecuteRepoScopeBypassesNilChangedLines(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	raw := `{"Issues":[{"FromLinter":"check","Text":"message","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"warning": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{
		Gate:    gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second, Scope: gate.Repo}},
		Binding: binding, Root: root, RawSink: &memoryRawSink{},
	}))
	if report.Status != GateFindings || len(report.Findings) != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestExecuteDiffScopeAcceptsEmptyChangedLines(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	raw := `{"Issues":[{"FromLinter":"check","Text":"message","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"warning": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{
		Gate:    gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second, Scope: gate.Diff}},
		Binding: binding, Root: root, RawSink: &memoryRawSink{}, ChangedLines: finding.ChangedLines{},
	}))
	if report.Status != GatePassed || len(report.Findings) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestExecuteRejectsNilChangedLinesForDiffScopeBeforeCommand(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "mark", marker}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	store := &memoryRawSink{}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{
		Gate:    gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second, Scope: gate.Diff}},
		Binding: binding, Root: root, RawSink: store,
	}))
	if report.Status != GateErrored || report.Error != "diff-scoped gate requires changed lines" {
		t.Fatalf("report = %#v", report)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran: %v", err)
	}
	if len(store.writes) != 0 {
		t.Fatalf("raw writes = %#v", store.writes)
	}
}

func TestExecuteRejectsInvalidChangedLinesForDiffScopeBeforeVersionOrGate(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name    string
		changed finding.ChangedLines
	}{
		{name: "invalid range", changed: finding.ChangedLines{"source.go": {{Start: 0, End: 1}}}},
		{name: "unsafe path", changed: finding.ChangedLines{"../scope-secret": {{Start: 1, End: 1}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			versionMarker := filepath.Join(root, "version-"+strings.ReplaceAll(test.name, " ", "-"))
			store := &memoryRawSink{}
			runnerCalled := false
			binding := gate.Binding{
				Language: "go", Tool: "fixture", Command: []string{"fixture"}, SuccessExitCodes: []int{0},
				Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning},
				Version: gate.Version{Command: []string{helperBinary(t), "mark", versionMarker}, Pattern: `(.*)`, Constraint: ">=1.0.0"},
			}
			report := (Executor{
				Enrichers: enricher.Registry{"go": enricher.Noop{}},
				runCommand: func(context.Context, string, []string) runner.Result {
					runnerCalled = true
					return runner.Result{Stdout: runner.NewBuffer(rawOutputLimit, rawTruncationMarker), Stderr: runner.NewBuffer(rawOutputLimit, rawTruncationMarker)}
				},
			}).Execute(context.Background(), compileRequest(t, Request{
				Gate:         gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second, Scope: gate.Diff}},
				Binding:      binding,
				Root:         root,
				RawSink:      store,
				ChangedLines: test.changed,
			}))
			if report.Status != GateErrored || report.Error != "filter findings by scope: invalid changed-line scope" {
				t.Fatalf("report = %#v", report)
			}
			if _, err := os.Stat(versionMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("version command ran: %v", err)
			}
			if runnerCalled {
				t.Fatal("gate runner was called")
			}
			if len(store.writes) != 0 {
				t.Fatalf("raw writes = %#v", store.writes)
			}
		})
	}
}

func TestExecuteRedactsInvalidDiffScope(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	const secret = "scope-secret"
	raw := `{"Issues":[{"FromLinter":"check","Text":"scope-secret","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"warning": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{
		Gate:    gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second, Scope: gate.Diff}},
		Binding: binding, Root: root, RawSink: &memoryRawSink{},
		ChangedLines: finding.ChangedLines{"../" + secret: {{Start: 1, End: 1}}},
	}))
	if report.Status != GateErrored || report.Error != "filter findings by scope: invalid changed-line scope" || len(report.Findings) != 0 {
		t.Fatalf("report = %#v", report)
	}
	if strings.Contains(report.Error, secret) {
		t.Fatalf("scope error leaked changed-line data: %q", report.Error)
	}
}

func TestCollectKeepsHealthySiblingWhenScopeOrEnrichmentFails(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	raw := `{"Issues":[{"FromLinter":"check","Text":"message","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"warning": finding.Warning}}

	for _, test := range []struct {
		name     string
		executor Executor
		broken   Request
	}{
		{
			name:     "scope",
			executor: Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}},
			broken:   Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "broken", Timeout: time.Second, Scope: gate.Diff}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}, ChangedLines: finding.ChangedLines{"../invalid": {{Start: 1, End: 1}}}},
		},
		{
			name: "enrichment",
			executor: Executor{Enrichers: enricher.Registry{"go": &recordingEnricher{errFor: func(ctx enricher.Context) error {
				if ctx.Location == gate.EntityLocation {
					return errors.New("enrichment-secret")
				}
				return nil
			}}}},
			broken: Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "broken", Timeout: time.Second, Scope: gate.Repo, Location: gate.EntityLocation}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			healthy := compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "healthy", Timeout: time.Second, Scope: gate.Repo, Location: gate.PointLocation}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}})
			reports := Collect(context.Background(), test.executor, []Request{compileRequest(t, test.broken), healthy}, 2)
			if len(reports) != 2 || reports[0].Status != GateErrored || reports[1].Status != GateFindings || len(reports[1].Findings) != 1 {
				t.Fatalf("reports = %#v", reports)
			}
			if strings.Contains(reports[0].Error, "secret") {
				t.Fatalf("broken report leaked error details: %#v", reports[0])
			}
		})
	}
}

func TestExecutorZeroValuesReturnErroredWithoutPanic(t *testing.T) {
	var report GateReport
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Execute panicked: %v", recovered)
			}
		}()
		report = (Executor{}).Execute(context.Background(), Request{})
	}()
	if report.Status != GateErrored || report.Error == "" {
		t.Fatalf("report = %#v", report)
	}
}

func TestExecuteClassifiesExitCodesWithoutInventingFindings(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	valid := `{"Issues":[{"FromLinter":"check","Text":"message","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	tests := []struct {
		name, stdout, stderr string
		code                 int
		want                 GateStatus
	}{
		{name: "success may be clean", code: 0, want: GatePassed},
		{name: "success may contain findings", stdout: valid, code: 0, want: GateFindings},
		{name: "finding exit requires finding", stdout: valid, code: 1, want: GateFindings},
		{name: "finding exit empty", code: 1, want: GateErrored},
		{name: "finding exit malformed", stdout: "not json", code: 1, want: GateErrored},
		{name: "finding exit stderr", stdout: valid, stderr: "warning", code: 1, want: GateErrored},
		{name: "other exit", stdout: valid, code: 2, want: GateErrored},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, test.stdout, test.stderr, test.code), SuccessExitCodes: []int{0}, FindingExitCodes: []int{1}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning, "warning": finding.Warning}}
			report, _, enrich := executeFixture(t, root, binding, gate.Manifest{Name: "lint"})
			if report.Status != test.want {
				t.Fatalf("status = %q, want %q; error=%q", report.Status, test.want, report.Error)
			}
			if test.want == GateErrored && len(report.Findings) != 0 {
				t.Fatalf("errored report has findings: %#v", report.Findings)
			}
			if test.name == "other exit" && enrich.calls != 0 {
				t.Fatalf("unexpected exit reached enricher")
			}
		})
	}
}

func TestExecuteInfrastructureFailuresAreErrored(t *testing.T) {
	root := t.TempDir()
	base := gate.Binding{Language: "go", Tool: "fixture", SuccessExitCodes: []int{0}, FindingExitCodes: []int{1}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	tests := []struct {
		name string
		bind func(gate.Binding) gate.Binding
	}{
		{name: "missing executable", bind: func(b gate.Binding) gate.Binding { b.Command = []string{filepath.Join(root, "missing")}; return b }},
		{name: "crash", bind: func(b gate.Binding) gate.Binding { b.Command = []string{helperBinary(t), "panic"}; return b }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryRawSink{}
			enrich := &recordingEnricher{}
			report := (Executor{Enrichers: enricher.Registry{"go": enrich}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second}}, Binding: test.bind(base), Root: root, RawSink: store}))
			if report.Status != GateErrored || len(report.Findings) != 0 || report.Error == "" {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestExecuteDeadlineCoversProcessAndPersistsBothStreams(t *testing.T) {
	root := t.TempDir()
	store := &memoryRawSink{}
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "sleep", "5s"}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	started := time.Now()
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "slow", Timeout: 40 * time.Millisecond}}, Binding: binding, Root: root, RawSink: store}))
	if report.Status != GateErrored || !strings.Contains(report.Error, "deadline") {
		t.Fatalf("report = %#v", report)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("deadline did not stop child promptly")
	}
	if got := streams(store.writes); !slices.Equal(got, []string{"stdout", "stderr"}) {
		t.Fatalf("persisted streams = %v", got)
	}
}

func TestExecuteDeadlineBoundsInheritedPipeShutdown(t *testing.T) {
	root := t.TempDir()
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "spawn", "5s"}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	started := time.Now()
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "tree", Timeout: 40 * time.Millisecond}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}}))
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("inherited pipes held execution for %v", elapsed)
	}
	if report.Status != GateErrored || !strings.Contains(report.Error, "deadline") {
		t.Fatalf("report = %#v", report)
	}
}

func TestExecuteDeadlineTerminatesDescendants(t *testing.T) {
	root := t.TempDir()
	started := filepath.Join(root, "descendant-started")
	survived := filepath.Join(root, "descendant-survived")
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "spawn-survivor", started, survived}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "tree", Timeout: 150 * time.Millisecond}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}}))
	if report.Status != GateErrored || !strings.Contains(report.Error, "deadline") {
		t.Fatalf("report = %#v", report)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("descendant did not start: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(survived); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived Execute return: %v", err)
	}
}

func TestExecutePersistsBeforeClassificationAndStopsOnPersistenceFailure(t *testing.T) {
	root := t.TempDir()
	secret := "raw-secret-that-must-not-leak"
	store := &memoryRawSink{err: map[string]error{"stdout": errors.New("disk full"), "stderr": errors.New("read only")}}
	enrich := &recordingEnricher{}
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, secret, "diagnostic", 2), SuccessExitCodes: []int{0}, FindingExitCodes: []int{1}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enrich}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if report.Status != GateErrored || enrich.calls != 0 {
		t.Fatalf("report=%#v enrich calls=%d", report, enrich.calls)
	}
	if got := streams(store.writes); !slices.Equal(got, []string{"stdout", "stderr"}) {
		t.Fatalf("writes = %v", got)
	}
	if strings.Contains(report.Error, secret) || strings.Contains(report.Error, "diagnostic") {
		t.Fatalf("error leaked raw output: %q", report.Error)
	}
}

func TestExecuteCleanupFailureOverridesValidFindingExit(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nfunc complex() {}\n")
	store := &memoryRawSink{}
	binding := gate.Binding{
		Language: "go", Tool: "fixture", Command: emitCommand(t, "17 pkg complex source.go:2:1\n", "", 1), SuccessExitCodes: []int{0}, FindingExitCodes: []int{1},
		Normalizer: `regex:^(?P<value>\d+) \S+ (?P<symbol>\S+) (?P<file>[^:]+):(?P<line>\d+):\d+$`, RuleID: "gocyclo/complexity", Message: "complexity {{.value}} in {{.symbol}}", SeverityMap: map[string]finding.Severity{"default": finding.Warning},
	}
	executor := Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}
	executor.runCommand = func(ctx context.Context, root string, command []string) runner.Result {
		result := gateCommand(ctx, root, command)
		result.CleanupErr = errors.New("injected process-tree cleanup failure")
		return result
	}
	report := executor.Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "complexity", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if report.Status != GateErrored || len(report.Findings) != 0 || !strings.Contains(report.Error, "clean up") {
		t.Fatalf("report = %#v", report)
	}
	if got := string(store.raw("stdout")); got != "17 pkg complex source.go:2:1\n" {
		t.Fatalf("persisted stdout = %q", got)
	}
}

func TestExecuteCapsAndMarksBothStreamsWithoutDeadlock(t *testing.T) {
	root := t.TempDir()
	store := &memoryRawSink{}
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "large", fmt.Sprint(rawOutputLimit + 512*1024)}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "large", Timeout: 5 * time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if report.Status != GateErrored || !strings.Contains(report.Error, "capture limit") {
		t.Fatalf("report = %#v", report)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		raw := store.raw(stream)
		if len(raw) != rawOutputLimit || !slices.Equal(raw[len(raw)-len(rawTruncationMarker):], rawTruncationMarker) {
			t.Fatalf("%s len/marker = %d/%q", stream, len(raw), raw[max(0, len(raw)-len(rawTruncationMarker)):])
		}
	}
}

func TestExecuteDoesNotTruncateOutputAtExactLimit(t *testing.T) {
	root := t.TempDir()
	store := &memoryRawSink{}
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "large", fmt.Sprint(rawOutputLimit)}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "exact", Timeout: 5 * time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if strings.Contains(report.Error, "capture limit") {
		t.Fatalf("exact-limit output was marked truncated: %#v", report)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		raw := store.raw(stream)
		if len(raw) != rawOutputLimit || slices.Equal(raw[len(raw)-len(rawTruncationMarker):], rawTruncationMarker) {
			t.Fatalf("%s was truncated at exact limit", stream)
		}
	}
}

func TestExecuteUsesRepositoryDirectoryAndNoShell(t *testing.T) {
	root := t.TempDir()
	store := &memoryRawSink{}
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: []string{helperBinary(t), "record-dir"}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "cwd", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if report.Status != GateErrored {
		t.Fatalf("report = %#v", report)
	}
	if got := string(store.raw("stdout")); got != root {
		t.Fatalf("working directory = %q, want %q", got, root)
	}

	marker := filepath.Join(root, "shell-expanded")
	binding.Command = emitCommand(t, "$(touch "+marker+")", "", 0)
	report = (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "literal", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell expression executed: %v", err)
	}
}

func TestExecuteEnricherAndGroupingErrorsAreRedacted(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	raw := `{"Issues":[{"FromLinter":"check","Text":"message","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	base := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"warning": finding.Warning}}
	for _, test := range []struct {
		name   string
		enrich *recordingEnricher
		want   string
	}{
		{name: "enricher", enrich: &recordingEnricher{err: fmt.Errorf("cannot enrich %s", raw)}, want: "enrich"},
		{name: "group", enrich: &recordingEnricher{mutate: func(in []finding.Finding) []finding.Finding { in[0].RuleID = "invalid"; return in }}, want: "group"},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := (Executor{Enrichers: enricher.Registry{"go": test.enrich}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second}}, Binding: base, Root: root, RawSink: &memoryRawSink{}}))
			if report.Status != GateErrored || !strings.Contains(report.Error, test.want) {
				t.Fatalf("report = %#v", report)
			}
			if strings.Contains(report.Error, raw) {
				t.Fatalf("error leaked raw output")
			}
		})
	}

	binding := base
	binding.Command = emitCommand(t, "secret-normalizer", "", 0)
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}}))
	if report.Status != GateErrored || strings.Contains(report.Error, "secret-normalizer") {
		t.Fatalf("normalizer error leaked raw output: %#v", report)
	}
}

func TestExecuteStageErrorsNeverExposePartialRawValues(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nvar value = 1\n")
	const secret = "capture-secret"
	raw := `{"Issues":[{"FromLinter":"check","Text":"capture-secret","Severity":"warning","Pos":{"Filename":"source.go","Line":2}}]}`
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, raw, "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"warning": finding.Warning}}
	tests := []struct {
		name, stage string
		enricher    *recordingEnricher
	}{
		{name: "enricher", stage: "enrich", enricher: &recordingEnricher{err: fmt.Errorf("cannot enrich captured value %s", secret)}},
		{name: "group", stage: "group", enricher: &recordingEnricher{mutate: func(in []finding.Finding) []finding.Finding { in[0].Severity = finding.Severity(secret); return in }}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := (Executor{Enrichers: enricher.Registry{"go": test.enricher}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}}))
			if report.Status != GateErrored || !strings.Contains(report.Error, test.stage) {
				t.Fatalf("report = %#v", report)
			}
			if strings.Contains(report.Error, secret) {
				t.Fatalf("stage error leaked partial raw value: %q", report.Error)
			}
		})
	}
}

func TestExecuteVersionChecksAreAdvisoryAndShareDeadline(t *testing.T) {
	root := t.TempDir()
	base := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	tests := []struct {
		name                      string
		version                   gate.Version
		wantObserved, wantWarning string
	}{
		{name: "observed match", version: gate.Version{Command: []string{helperBinary(t), "version", "tool v1.2.3"}, Pattern: `v(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0 <2.0.0"}, wantObserved: "1.2.3"},
		{name: "constraint mismatch", version: gate.Version{Command: []string{helperBinary(t), "version", "tool v2.0.0"}, Pattern: `v(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0 <2.0.0"}, wantObserved: "2.0.0", wantWarning: "constraint"},
		{name: "missing match", version: gate.Version{Command: []string{helperBinary(t), "version", "opaque-secret"}, Pattern: `v(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0"}, wantWarning: "observe"},
		{name: "captured invalid version is not recorded", version: gate.Version{Command: []string{helperBinary(t), "version", "opaque-secret"}, Pattern: `(\S+)`, Constraint: ">=1.0.0"}, wantWarning: "observe"},
		{name: "split streams cannot synthesize version", version: gate.Version{Command: emitCommand(t, "tool v1.", "2.3", 0), Pattern: `v(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0"}, wantWarning: "observe"},
		{name: "stdout version takes precedence", version: gate.Version{Command: emitCommand(t, "tool v1.2.3", "tool v9.9.9", 0), Pattern: `v(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0"}, wantObserved: "1.2.3"},
		{name: "version command failure", version: gate.Version{Command: []string{filepath.Join(root, "missing-version")}, Pattern: `(\S+)`, Constraint: ">=1.0.0"}, wantWarning: "command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := base
			binding.Version = test.version
			report, _, _ := executeFixture(t, root, binding, gate.Manifest{Name: "lint"})
			if report.Status != GatePassed || report.ObservedVersion != test.wantObserved {
				t.Fatalf("report = %#v", report)
			}
			if test.wantWarning != "" && (len(report.Warnings) == 0 || !strings.Contains(strings.ToLower(report.Warnings[0]), test.wantWarning)) {
				t.Fatalf("warnings = %v", report.Warnings)
			}
			if leaked := report.ObservedVersion + report.Error + strings.Join(report.Warnings, "\n"); strings.Contains(leaked, "opaque-secret") {
				t.Fatalf("report leaked raw version output: %#v", report)
			}
		})
	}

	binding := base
	binding.Version = gate.Version{Command: []string{helperBinary(t), "sleep", "5s"}, Pattern: `(\S+)`, Constraint: ">=1.0.0"}
	report, _, _ := executeFixture(t, root, binding, gate.Manifest{Name: "lint", Timeout: 40 * time.Millisecond})
	if report.Status != GateErrored || !strings.Contains(report.Error, "deadline") {
		t.Fatalf("version deadline report = %#v", report)
	}

	store := &memoryRawSink{}
	binding = base
	binding.Version = gate.Version{Command: []string{helperBinary(t), "large", fmt.Sprint(rawOutputLimit + 1)}, Pattern: `(\S+)`, Constraint: ">=1.0.0"}
	report = (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "lint", Timeout: 5 * time.Second}}, Binding: binding, Root: root, RawSink: store}))
	if report.Status != GatePassed || len(report.Warnings) == 0 || !strings.Contains(report.Warnings[0], "capture limit") {
		t.Fatalf("large version report = %#v", report)
	}
	if len(store.raw("stdout")) != 0 || len(store.raw("stderr")) != 0 {
		t.Fatalf("version output was persisted as main raw output")
	}
}

func TestExecuteDurationUsesInjectedClockAndNeverGoesNegative(t *testing.T) {
	root := t.TempDir()
	times := []time.Time{time.Unix(10, 0), time.Unix(9, 0)}
	var index atomic.Int32
	now := func() time.Time { return times[min(int(index.Add(1))-1, len(times)-1)] }
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	report := (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}, Now: now}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "clock", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}}))
	if report.DurationMS != 0 {
		t.Fatalf("duration = %d, want 0", report.DurationMS)
	}

	times = []time.Time{time.Unix(10, 0), time.Unix(11, 500_000_000)}
	index.Store(0)
	report = (Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}, Now: now}).Execute(context.Background(), compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "clock", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}}))
	if report.DurationMS != 1500 {
		t.Fatalf("duration = %d, want 1500", report.DurationMS)
	}
}

func TestExecuteRejectsInvalidRuntimeInputs(t *testing.T) {
	root := t.TempDir()
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	valid := compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "gate", Timeout: time.Second}}, Binding: binding, Root: root, RawSink: &memoryRawSink{}})
	var nilSink *memoryRawSink
	var nilEnricher *recordingEnricher
	rootFile := filepath.Join(t.TempDir(), "root-file")
	writeSource(t, filepath.Dir(rootFile), filepath.Base(rootFile), "not a directory")
	with := func(change func(*Request)) Request {
		request := valid
		change(&request)
		return request
	}
	tests := []struct {
		name     string
		ctx      context.Context
		executor Executor
		request  Request
	}{
		{name: "nil context", executor: Executor{Enrichers: enricher.NewRegistry()}, request: valid},
		{name: "uncompiled witness", ctx: context.Background(), executor: Executor{Enrichers: enricher.NewRegistry()}, request: Request{}},
		{name: "empty root", ctx: context.Background(), executor: Executor{Enrichers: enricher.NewRegistry()}, request: with(func(request *Request) { request.Root = "" })},
		{name: "negative position", ctx: context.Background(), executor: Executor{Enrichers: enricher.NewRegistry()}, request: with(func(request *Request) { request.Position = -1 })},
		{name: "missing root", ctx: context.Background(), executor: Executor{Enrichers: enricher.NewRegistry()}, request: with(func(request *Request) { request.Root = filepath.Join(root, "missing") })},
		{name: "root is file", ctx: context.Background(), executor: Executor{Enrichers: enricher.NewRegistry()}, request: with(func(request *Request) { request.Root = rootFile })},
		{name: "typed nil store", ctx: context.Background(), executor: Executor{Enrichers: enricher.NewRegistry()}, request: with(func(request *Request) { request.RawSink = nilSink })},
		{name: "typed nil enricher", ctx: context.Background(), executor: Executor{Enrichers: enricher.Registry{"go": nilEnricher}}, request: valid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("panic: %v", recovered)
				}
			}()
			report := test.executor.Execute(test.ctx, test.request)
			if report.Status != GateErrored {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestValidationRequestsKeepsFastAndAssignedOwnersInGauntletOrder(t *testing.T) {
	requests := []Request{
		validationRequest(t, "slow-unassigned", gate.Slow, 1),
		validationRequest(t, "glacial-assigned", gate.Glacial, 4),
		validationRequest(t, "instant", gate.Instant, 0),
		validationRequest(t, "slow-assigned", gate.Slow, 3),
		validationRequest(t, "fast", gate.Fast, 2),
	}
	assigned := []finding.Finding{
		executorFinding(t, "glacial-assigned", "g.go", "tool/g"),
		executorFinding(t, "slow-assigned", "s.go", "tool/s"),
	}
	assigned[0].Occurrences = nil
	assigned[1].Occurrences = nil
	got := validationRequests(requests, assigned)
	var names []string
	for _, request := range got {
		names = append(names, request.Gate.Manifest.Name)
	}
	if want := []string{"instant", "fast", "slow-assigned", "glacial-assigned"}; !slices.Equal(names, want) {
		t.Fatalf("validationRequests() names = %v, want %v", names, want)
	}
}

func TestScheduledValidationRequestsHonorsCostClasses(t *testing.T) {
	requests := []Request{
		validationRequest(t, "instant", gate.Instant, 0), validationRequest(t, "fast", gate.Fast, 1),
		validationRequest(t, "slow", gate.Slow, 2), validationRequest(t, "glacial", gate.Glacial, 3),
	}
	for _, test := range []struct {
		name     string
		attempt  int
		assigned []finding.Finding
		want     []string
	}{
		{name: "first", attempt: 1, want: []string{"instant", "fast"}},
		{name: "third", attempt: 3, want: []string{"instant", "fast", "slow"}},
		{name: "slow owner", attempt: 1, assigned: []finding.Finding{executorFinding(t, "slow", "s.go", "tool/s")}, want: []string{"instant", "fast", "slow"}},
		{name: "glacial owner", attempt: 3, assigned: []finding.Finding{executorFinding(t, "glacial", "g.go", "tool/g")}, want: []string{"instant", "fast", "slow"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := scheduledValidationRequests(requests, test.assigned, test.attempt)
			names := make([]string, len(got))
			for index, request := range got {
				names[index] = request.Gate.Manifest.Name
			}
			if !slices.Equal(names, test.want) {
				t.Fatalf("got %v, want %v", names, test.want)
			}
		})
	}
}

func TestValidationRequestsDoesNotAliasInputsAndFailsClosedOnMalformedFindings(t *testing.T) {
	requests := []Request{
		validationRequest(t, "fast", gate.Fast, 0),
		validationRequest(t, "slow", gate.Slow, 1),
	}
	requests[0].ChangedLines = finding.ChangedLines{"a.go": {{Start: 1, End: 2}}}
	malformed := executorFinding(t, "slow", "a.go", "tool/a")
	malformed.Fingerprint = "wrong"
	got := validationRequests(requests, []finding.Finding{malformed})
	if len(got) != 2 {
		t.Fatalf("validationRequests() = %d requests, want all gates on malformed evidence", len(got))
	}
	got[0].ChangedLines["a.go"][0].Start = 99
	got[0].Gate.Manifest.Blocking[0] = finding.Info
	got[0].Binding.Command[0] = "mutated"
	if requests[0].ChangedLines["a.go"][0].Start == 99 {
		t.Fatal("validationRequests returned aliased request state")
	}
	if requests[0].Gate.Manifest.Blocking[0] == finding.Info || requests[0].Binding.Command[0] == "mutated" {
		t.Fatal("validationRequests returned aliased compiled gate state")
	}
}

func TestValidationRequestsFailsClosedWhenAssignedOwnerIsUnavailable(t *testing.T) {
	requests := []Request{
		validationRequest(t, "fast", gate.Fast, 0),
		validationRequest(t, "slow", gate.Slow, 1),
	}
	assigned := []finding.Finding{executorFinding(t, "missing", "a.go", "tool/a")}
	if got := validationRequests(requests, assigned); len(got) != len(requests) {
		t.Fatalf("validationRequests() returned %d requests, want fail-closed %d", len(got), len(requests))
	}
}

func TestValidationRequestsFailsClosedWithoutAliasingMalformedRequests(t *testing.T) {
	valid := validationRequest(t, "fast", gate.Fast, 0)
	foreign := validationRequest(t, "slow", gate.Slow, 1)
	foreign.Binding = valid.Binding
	mutated := validationRequest(t, "mutated", gate.Slow, 2)
	mutated.Gate.Manifest.Blocking[0] = finding.Info
	got := validationRequests([]Request{foreign, mutated, valid}, nil)
	if len(got) != 3 {
		t.Fatalf("validationRequests() = %d requests, want every malformed request retained as errored evidence", len(got))
	}
	if !got[0].Gate.Valid() || !got[0].Gate.Owns(got[0].Binding) {
		t.Fatalf("valid request was not independently cloned: %#v", got[0])
	}
	for index, request := range got[1:] {
		if request.Gate.Valid() || request.Binding.Valid() {
			t.Fatalf("malformed output request %d was silently repaired: %#v", index, request)
		}
	}
}

func validationRequest(t *testing.T, name string, cost gate.CostClass, position int) Request {
	t.Helper()
	compiled := validationRequestGate(t, name, cost)
	return Request{Gate: compiled, Binding: compiled.Bindings["go"], Position: position}
}

func validationRequestGate(t *testing.T, name string, cost gate.CostClass) gate.Gate {
	t.Helper()
	compiled, err := gate.Compile(gate.Manifest{
		Name: name, Description: name, CostClass: cost, FixPolicy: gate.LLMFix,
		Scope: gate.Diff, Location: gate.PointLocation, Blocking: []finding.Severity{finding.Error},
		Timeout: time.Second,
	}, map[string]gate.Binding{"go": {
		Language: "go", Tool: "tool", Command: []string{"tool"}, SuccessExitCodes: []int{0},
		FindingExitCodes: []int{1}, Normalizer: "golangci-json",
		SeverityMap: map[string]finding.Severity{"default": finding.Error},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func executorFinding(t *testing.T, gateName, file, rule string) finding.Finding {
	t.Helper()
	grouped, err := finding.Group([]finding.Finding{{
		Gate: gateName, Language: "go", RuleID: rule, Severity: finding.Error,
		File: file, Line: 1, Snippet: rule, Message: rule,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return grouped[0]
}

func TestExecuteRejectsBindingOwnedByAnotherGate(t *testing.T) {
	root := t.TempDir()
	binding := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	firstGate, _ := compileGateFixture(t, gate.Manifest{Name: "first", Timeout: time.Second}, binding)
	_, secondBinding := compileGateFixture(t, gate.Manifest{Name: "second", Timeout: time.Second}, binding)
	store := &memoryRawSink{}
	if !firstGate.Valid() || !secondBinding.Valid() {
		t.Fatal("test requires independently valid gate and binding")
	}

	report := (Executor{Enrichers: enricher.NewRegistry()}).Execute(context.Background(), Request{
		Gate: firstGate, Binding: secondBinding, Root: root, RawSink: store,
	})
	if report.Status != GateErrored || !strings.Contains(report.Error, "does not belong") {
		t.Fatalf("report = %#v, want binding ownership error", report)
	}
	if len(store.writes) != 0 {
		t.Fatalf("mismatched binding executed: writes=%#v", store.writes)
	}
}

func streams(writes []rawWrite) []string {
	result := make([]string, len(writes))
	for index, write := range writes {
		result[index] = write.stream
	}
	return result
}

func TestCollectReturnsReportsInRequestOrderAndLimitsConcurrency(t *testing.T) {
	root := t.TempDir()
	markerDir := t.TempDir()
	completionDir := t.TempDir()
	store := &memoryRawSink{}
	registry := enricher.Registry{}
	requests := make([]Request, 4)
	durations := []time.Duration{320 * time.Millisecond, 40 * time.Millisecond, 180 * time.Millisecond, 80 * time.Millisecond}
	for index := range requests {
		binding := gate.Binding{Language: fmt.Sprintf("go%d", index), Tool: "fixture", Command: []string{helperBinary(t), "active", filepath.Join(markerDir, fmt.Sprint(index)), durations[index].String(), filepath.Join(completionDir, fmt.Sprint(index))}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
		requests[index] = compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: fmt.Sprintf("gate%d", index), Timeout: 2 * time.Second}}, Binding: binding, Position: index, Root: root, RawSink: store})
		registry[binding.Language] = enricher.Noop{}
	}
	type result struct {
		reports []GateReport
		elapsed time.Duration
	}
	done := make(chan result, 1)
	started := time.Now()
	go func() {
		done <- result{reports: Collect(context.Background(), Executor{Enrichers: registry}, requests, 2), elapsed: time.Since(started)}
	}()
	peak := 0
	firstCompletion := ""
	for {
		select {
		case got := <-done:
			if peak > 2 || peak < 2 {
				t.Fatalf("peak active = %d, want 2", peak)
			}
			if got.elapsed < 280*time.Millisecond {
				t.Fatalf("elapsed = %v, worker limit was not enforced", got.elapsed)
			}
			if firstCompletion != "1" {
				t.Fatalf("first completion = %q, want request 1 before request 0", firstCompletion)
			}
			for index, report := range got.reports {
				if report.Gate != fmt.Sprintf("gate%d", index) {
					t.Fatalf("report %d gate = %q", index, report.Gate)
				}
				if report.Position != index {
					t.Fatalf("report %d position = %d", index, report.Position)
				}
			}
			return
		default:
			completed, err := os.ReadDir(completionDir)
			if err != nil {
				t.Fatal(err)
			}
			if firstCompletion == "" && len(completed) > 0 {
				firstCompletion = completed[0].Name()
			}
			entries, err := os.ReadDir(markerDir)
			if err != nil {
				t.Fatal(err)
			}
			peak = max(peak, len(entries))
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func TestCollectPreservesFastHealthyFindingsWhenEarlierGateTimesOut(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "source.go", "package source\nfunc complex() {}\n")
	store := &memoryRawSink{}
	slow := gate.Binding{Language: "slow", Tool: "fixture", Command: []string{helperBinary(t), "sleep", "5s"}, SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	fast := gate.Binding{
		Language: "go", Tool: "fixture", Command: emitCommand(t, "17 pkg complex source.go:2:1\n", "", 1), SuccessExitCodes: []int{0}, FindingExitCodes: []int{1},
		Normalizer: `regex:^(?P<value>\d+) \S+ (?P<symbol>\S+) (?P<file>[^:]+):(?P<line>\d+):\d+$`, RuleID: "gocyclo/complexity", Message: "complexity {{.value}} in {{.symbol}}", SeverityMap: map[string]finding.Severity{"default": finding.Warning},
	}
	requests := []Request{
		compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "slow", Timeout: 150 * time.Millisecond}}, Binding: slow, Root: root, RawSink: store}),
		compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "fast", Timeout: time.Second}}, Binding: fast, Root: root, RawSink: store}),
	}
	reports := Collect(context.Background(), Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}, "slow": enricher.Noop{}}}, requests, 2)
	if len(reports) != 2 || reports[0].Gate != "slow" || reports[1].Gate != "fast" {
		t.Fatalf("reports not in request order: %#v", reports)
	}
	if reports[0].Status != GateErrored || !strings.Contains(reports[0].Error, "deadline") {
		t.Fatalf("slow report = %#v", reports[0])
	}
	if reports[1].Status != GateFindings || len(reports[1].Findings) != 1 || reports[1].Findings[0].RuleID != "gocyclo/complexity" {
		t.Fatalf("fast report = %#v", reports[1])
	}
	if reports[1].DurationMS >= reports[0].DurationMS {
		t.Fatalf("fast duration %dms did not finish before slow duration %dms", reports[1].DurationMS, reports[0].DurationMS)
	}
}

func TestCollectKeepsHealthySiblingAndDrainsAfterCancellation(t *testing.T) {
	root := t.TempDir()
	store := &memoryRawSink{}
	clean := gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}}
	missing := clean
	missing.Command = []string{filepath.Join(root, "missing")}
	requests := []Request{
		compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "broken", Timeout: time.Second}}, Binding: missing, Root: root, RawSink: store}),
		compileRequest(t, Request{Gate: gate.Gate{Manifest: gate.Manifest{Name: "healthy", Timeout: time.Second}}, Binding: clean, Root: root, RawSink: store}),
	}
	reports := Collect(context.Background(), Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}, requests, 0)
	if len(reports) != 2 || reports[0].Status != GateErrored || reports[1].Status != GatePassed {
		t.Fatalf("reports = %#v", reports)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reports = Collect(ctx, Executor{Enrichers: enricher.Registry{"go": enricher.Noop{}}}, append(requests, requests...), 2)
	if len(reports) != 4 {
		t.Fatalf("canceled collect returned %d reports", len(reports))
	}
	for _, report := range reports {
		if report.Status != GateErrored {
			t.Fatalf("canceled report = %#v", report)
		}
	}
}
