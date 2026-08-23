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

func TestComposeReportSnapshotsNestedGateState(t *testing.T) {
	item := reportFinding(t, "lint", finding.Warning, "source.go", 1)
	item.Occurrences = []finding.Occurrence{{Line: 2}}
	source := []GateReport{{
		Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Warning},
		FixPolicy: gate.ReportOnly, Position: 0, Status: GateFindings,
		Warnings: []string{"version drift"}, Findings: []finding.Finding{item},
	}}
	report, err := ComposeReport("run-id", strings.Repeat("d", 40), fixedTime, fixedTime, fixtureDiff(), source)
	if err != nil {
		t.Fatal(err)
	}

	source[0].Blocking[0] = finding.Info
	source[0].Warnings[0] = "mutated"
	source[0].Findings[0].Message = "mutated"
	source[0].Findings[0].Occurrences[0].Line = 99
	if !reflect.DeepEqual(report.Gates[0].Blocking, []finding.Severity{finding.Warning}) || !reflect.DeepEqual(report.Gates[0].Warnings, []string{"version drift"}) {
		t.Fatalf("report metadata aliases source: %#v", report.Gates[0])
	}
	if report.Gates[0].Findings[0].Message != "message" || report.Gates[0].Findings[0].Occurrences[0].Line != 2 {
		t.Fatalf("report gate finding aliases source: %#v", report.Gates[0].Findings[0])
	}

	report.Gates[0].Findings[0].Message = "gate mutation"
	report.Gates[0].Findings[0].Occurrences[0].Line = 77
	if report.Findings[0].Message != "message" || report.Findings[0].Occurrences[0].Line != 2 {
		t.Fatalf("top-level finding aliases gate finding: %#v", report.Findings[0])
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

func TestRunLedgerRawSinkBindsIdentityAndWritesStreams(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
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
		contents, err := os.ReadFile(filepath.Join(run.Dir, "raw", rawOutputName("lint", "go", stream)))
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v; want %q", stream, contents, err, want)
		}
	}
}

func TestRunLedgerRawSinkEncodesDistinctCompiledIdentitiesWithinRawDirectory(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	identities := [][2]string{
		{"gate.name", "go lang"},
		{"gate name", "go.lang"},
		{strings.Repeat("g", 200), strings.Repeat("l", 200)},
	}
	names := make(map[string]struct{}, len(identities))
	for index, identity := range identities {
		sink, err := run.RawSink(identity[0], identity[1])
		if err != nil {
			t.Fatalf("RawSink(%q, %q): %v", identity[0], identity[1], err)
		}
		if err := sink.WriteRaw("stdout", []byte{byte(index)}); err != nil {
			t.Fatal(err)
		}
		name := rawOutputName(identity[0], identity[1], "stdout")
		if filepath.Base(name) != name || name == "." || name == ".." || len(name) > 255 {
			t.Fatalf("raw name escaped or exceeded filesystem limits: %q", name)
		}
		if _, duplicate := names[name]; duplicate {
			t.Fatalf("distinct identities collided at %q", name)
		}
		names[name] = struct{}{}
		contents, err := os.ReadFile(filepath.Join(run.Dir, "raw", name))
		if err != nil || !bytes.Equal(contents, []byte{byte(index)}) {
			t.Fatalf("raw %q = %v, %v", name, contents, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(run.Dir, "raw"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(identities) {
		t.Fatalf("raw entries = %d, want %d", len(entries), len(identities))
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
