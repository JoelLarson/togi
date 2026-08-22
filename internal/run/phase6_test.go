//go:build linux

package run

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

func TestNewGateReportSeedsExecutionIdentity(t *testing.T) {
	compiled, binding := compileGateFixture(t, gate.Manifest{
		Name: "lint", FixPolicy: gate.AutofixThenLLM,
		Blocking: []finding.Severity{finding.Error}, Timeout: time.Second,
	}, gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning}})

	report := NewGateReport(compiled, binding, 4)
	if report.Gate != "lint" || report.Language != "go" || report.FixPolicy != gate.AutofixThenLLM || report.Position != 4 || !reflect.DeepEqual(report.Blocking, []finding.Severity{finding.Error}) {
		t.Fatalf("gate report identity = %#v", report)
	}
	compiled.Manifest.Blocking[0] = finding.Info
	if report.Blocking[0] != finding.Error {
		t.Fatal("gate report retained mutable manifest blocking slice")
	}
}

func TestNewGateReportPreservesExplicitAdvisoryBlockingSet(t *testing.T) {
	compiled, binding := compileGateFixture(t, gate.Manifest{
		Name: "advisory", Blocking: []finding.Severity{}, Timeout: time.Second,
	}, gate.Binding{Language: "go", Tool: "fixture", Command: emitCommand(t, "", "", 0), SuccessExitCodes: []int{0}, Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Info}})
	report := NewGateReport(compiled, binding, 0)
	if report.Blocking == nil || len(report.Blocking) != 0 {
		t.Fatalf("blocking = %#v, want a present empty set", report.Blocking)
	}
}

func TestComposeReportUsesPerGateBlockingAndPositionOrder(t *testing.T) {
	warning := reportFinding(t, "advisory", finding.Warning, "advisory.go", 2)
	info := reportFinding(t, "blocking-info", finding.Info, "blocking.go", 3)
	gates := []GateReport{
		{Gate: "advisory", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: gate.ReportOnly, Position: 1, Status: GateFindings, Findings: []finding.Finding{warning}},
		{Gate: "blocking-info", Language: "go", Blocking: []finding.Severity{finding.Info}, FixPolicy: gate.LLMFix, Position: 0, Status: GateFindings, Findings: []finding.Finding{info}},
	}
	report, err := ComposeReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime.Add(time.Second), fixtureDiff(), gates)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != ReportSchemaVersion || report.Verdict != VerdictFindings {
		t.Fatalf("report schema/verdict = %d/%q", report.SchemaVersion, report.Verdict)
	}
	if report.Gates[0].Gate != "blocking-info" || report.Gates[1].Gate != "advisory" {
		t.Fatalf("gate order = %#v", report.Gates)
	}

	gates[1].Blocking = []finding.Severity{finding.Error}
	report, err = ComposeReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime.Add(time.Second), fixtureDiff(), gates)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictUnverified || report.Counts.Warnings != 1 || report.Counts.Info != 1 {
		t.Fatalf("non-blocking report = %#v", report)
	}
	var rendered bytes.Buffer
	if err := Render(&rendered, report, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), "advisory.go:2: warning") || !strings.Contains(rendered.String(), "blocking.go:3: info") {
		t.Fatalf("non-blocking findings not rendered:\n%s", rendered.String())
	}
}

func TestComposeReportErroredPrecedesBlockingFindings(t *testing.T) {
	item := reportFinding(t, "lint", finding.Error, "source.go", 1)
	report, err := ComposeReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime, fixtureDiff(), []GateReport{
		{Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: gate.ReportOnly, Position: 0, Status: GateFindings, Findings: []finding.Finding{item}},
		{Gate: "broken", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: gate.ReportOnly, Position: 1, Status: GateErrored, Error: "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictErrored {
		t.Fatalf("verdict = %q, want errored", report.Verdict)
	}
}

func TestComposeReportRejectsInvalidExecutionMetadata(t *testing.T) {
	base := GateReport{Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: gate.ReportOnly, Position: 0, Status: GatePassed}
	for _, mutate := range []func(*GateReport){
		func(report *GateReport) { report.Blocking = nil },
		func(report *GateReport) { report.Blocking[0] = "fatal" },
		func(report *GateReport) { report.Blocking = append(report.Blocking, finding.Error) },
		func(report *GateReport) { report.FixPolicy = "manual" },
		func(report *GateReport) { report.Position = -1 },
	} {
		candidate := base
		candidate.Blocking = append([]finding.Severity(nil), base.Blocking...)
		mutate(&candidate)
		if _, err := ComposeReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime, fixtureDiff(), []GateReport{candidate}); err == nil {
			t.Fatalf("ComposeReport accepted %#v", candidate)
		}
	}
}

func TestReportRefIsRuntimeOnly(t *testing.T) {
	report := completeReportFixture("20260821T120000.000000000Z-0000")
	report.Ref = RunRef{ID: report.RunID, Dir: "/external/run"}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("/external/run")) || bytes.Contains(encoded, []byte(`"ref"`)) {
		t.Fatalf("runtime ref persisted: %s", encoded)
	}
}

func TestComposeReportLedgerRoundTripPreservesCanonicalArtifact(t *testing.T) {
	repoState := t.TempDir()
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	report, err := ComposeReport(run.runID, ledgerTestRepoID, fixedTime, fixedTime.Add(time.Second), fixtureDiff(), []GateReport{{
		Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error, finding.Warning},
		FixPolicy: gate.ReportOnly, Position: 0, Status: GatePassed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.WriteReport(report); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	latest, err := (testLedger(repoState)).Latest()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(latest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("round trip artifact = %s, want %s", gotJSON, wantJSON)
	}
	if latest.Ref.ID != report.RunID || latest.Ref.Dir != filepath.Join(repoState, "runs", report.RunID) {
		t.Fatalf("latest ref = %#v", latest.Ref)
	}
}

func TestRunLedgerRawSinkValidatesIdentityUpFrontAndWritesStreams(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if _, err := run.RawSink("../lint", "go"); err == nil {
		t.Fatal("RawSink accepted unsafe gate identity")
	}
	sink, err := run.RawSink("lint", "go")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteRaw("stdout", []byte("out")); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteRaw("stderr", []byte("err")); err != nil {
		t.Fatal(err)
	}
	for stream, want := range map[string]string{"stdout": "out", "stderr": "err"} {
		got, err := filepath.Abs(filepath.Join(run.Dir, "raw", "lint.go."+stream))
		if err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(got)
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v; want %q", stream, contents, err, want)
		}
	}
}

func reportFinding(t *testing.T, gateName string, severity finding.Severity, file string, line int) finding.Finding {
	t.Helper()
	item := finding.Finding{Gate: gateName, Language: "go", RuleID: gateName + "/rule", Severity: severity, File: file, Line: line, Snippet: "var value = 1", Message: "message"}
	item.Fingerprint = finding.Fingerprint(item)
	if err := finding.Validate(item); err != nil {
		t.Fatal(err)
	}
	return item
}
