package flywheel

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/finding"
)

func TestAttemptStatusesKeepPersistedValues(t *testing.T) {
	got, err := json.Marshal([]Attempt{
		{Status: AttemptRunning},
		{Status: AttemptPassed},
		{Status: AttemptFailed},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "passed", "failed"} {
		if !strings.Contains(string(got), `"status":"`+status+`"`) {
			t.Fatalf("statuses = %s, want %q", got, status)
		}
	}
}

func TestNewPlanGroupsByPrimaryFileInStableReportOrder(t *testing.T) {
	findings := []finding.Finding{
		planFinding("z.go", 9, "lint/z", "z"),
		planFinding("a.go", 20, "lint/b", "b"),
		planFinding("a.go", 3, "lint/a", "a"),
	}

	plan, err := NewPlan(findings)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}
	if plan.SchemaVersion != 1 {
		t.Fatalf("schema version = %d, want 1", plan.SchemaVersion)
	}
	if got, want := len(plan.Batches), 2; got != want {
		t.Fatalf("batch count = %d, want %d", got, want)
	}
	if got, want := []string{plan.Batches[0].PrimaryFile, plan.Batches[1].PrimaryFile}, []string{"a.go", "z.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("primary files = %v, want %v", got, want)
	}
	if got, want := []string{plan.Batches[0].Findings[0].RuleID, plan.Batches[0].Findings[1].RuleID}, []string{"lint/a", "lint/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a.go rules = %v, want %v", got, want)
	}
	waveDigest := expectedWaveDigest(findings)
	for _, batch := range plan.Batches {
		if batch.Status != BatchPending || batch.Attempts == nil || len(batch.Attempts) != 0 {
			t.Fatalf("initial batch state = %#v, want pending with an empty attempts array", batch)
		}
		if got, want := batch.ID, expectedBatchID(batch.PrimaryFile, waveDigest); got != want {
			t.Fatalf("batch ID = %q, want %q", got, want)
		}
	}
}

func TestNewPlanIsIndependentOfInputOrder(t *testing.T) {
	first := planFinding("b.go", 2, "lint/b", "b")
	second := planFinding("a.go", 1, "lint/a", "a")

	forward, err := NewPlan([]finding.Finding{first, second})
	if err != nil {
		t.Fatalf("NewPlan(forward) error = %v", err)
	}
	reverse, err := NewPlan([]finding.Finding{second, first})
	if err != nil {
		t.Fatalf("NewPlan(reverse) error = %v", err)
	}
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("plans differ by input order:\nforward = %#v\nreverse = %#v", forward, reverse)
	}
}

func TestNewPlanChangesEveryBatchIDWhenTheBlockerWaveShrinksElsewhere(t *testing.T) {
	unchanged := planFinding("a.go", 1, "lint/a", "a")
	removed := planFinding("b.go", 2, "lint/b", "b")
	before, err := NewPlan([]finding.Finding{unchanged, removed})
	if err != nil {
		t.Fatalf("NewPlan(before) error = %v", err)
	}
	after, err := NewPlan([]finding.Finding{unchanged})
	if err != nil {
		t.Fatalf("NewPlan(after) error = %v", err)
	}
	if before.Batches[0].ID == after.Batches[0].ID {
		t.Fatalf("unchanged-file batch ID collided across waves: %q", before.Batches[0].ID)
	}
	beforeAttemptArtifact := fmt.Sprintf("%s/attempt-%d", before.Batches[0].ID, 1)
	afterAttemptArtifact := fmt.Sprintf("%s/attempt-%d", after.Batches[0].ID, 1)
	if beforeAttemptArtifact == afterAttemptArtifact {
		t.Fatalf("attempt artifact identity collided across waves: %q", beforeAttemptArtifact)
	}
	if before.Batches[0].PrimaryFile != after.Batches[0].PrimaryFile {
		t.Fatalf("test setup changed primary file: %q != %q", before.Batches[0].PrimaryFile, after.Batches[0].PrimaryFile)
	}
}

func TestNewPlanDeepCopiesFindings(t *testing.T) {
	source := []finding.Finding{planFinding("a.go", 1, "lint/a", "a")}
	source[0].Occurrences = []finding.Occurrence{{Line: 4}}

	plan, err := NewPlan(source)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}
	source[0].Occurrences[0].Line = 99
	source[0].Message = "mutated source"
	if got := plan.Batches[0].Findings[0]; got.Occurrences[0].Line != 4 || got.Message == "mutated source" {
		t.Fatalf("plan aliases input: %#v", got)
	}

	plan.Batches[0].Findings[0].Occurrences[0].Line = 77
	if source[0].Occurrences[0].Line == 77 {
		t.Fatal("input aliases plan occurrence storage")
	}
}

func TestNewPlanRejectsInvalidOrUngroupedFindings(t *testing.T) {
	valid := planFinding("a.go", 1, "lint/a", "a")
	duplicate := valid
	duplicate.Line = 2
	duplicate.Occurrences = nil

	for _, test := range []struct {
		name     string
		findings []finding.Finding
	}{
		{name: "invalid", findings: []finding.Finding{{}}},
		{name: "missing fingerprint", findings: []finding.Finding{func() finding.Finding { got := valid; got.Fingerprint = ""; return got }()}},
		{name: "duplicate ungrouped fingerprint", findings: []finding.Finding{valid, duplicate}},
		{name: "unsorted occurrences", findings: []finding.Finding{func() finding.Finding {
			got := valid
			got.Occurrences = []finding.Occurrence{{Line: 5}, {Line: 3}}
			return got
		}()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if plan, err := NewPlan(test.findings); err == nil || plan.Batches != nil {
				t.Fatalf("NewPlan() = (%#v, %v), want zero plan and error", plan, err)
			}
		})
	}
}

func TestBlockingMultisetPreservesOccurrenceMultiplicity(t *testing.T) {
	one := planFinding("a.go", 1, "lint/a", "a")
	three := planFinding("b.go", 1, "lint/b", "b")
	three.Occurrences = []finding.Occurrence{{Line: 2}, {Line: 3}}

	got, err := BlockingMultiset([]finding.Finding{three, one})
	if err != nil {
		t.Fatalf("BlockingMultiset() error = %v", err)
	}
	want := map[string]int{one.Fingerprint: 1, three.Fingerprint: 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BlockingMultiset() = %#v, want %#v", got, want)
	}
}

func TestBlockingMultisetRejectsInvalidOrUngroupedFindings(t *testing.T) {
	valid := planFinding("a.go", 1, "lint/a", "a")
	duplicate := valid
	duplicate.Line = 2
	stale := valid
	stale.Fingerprint = "stale"
	emptyFingerprint := valid
	emptyFingerprint.Fingerprint = ""
	noncanonical := valid
	noncanonical.Occurrences = []finding.Occurrence{{Line: 3}, {Line: 1}}
	overlap := valid
	overlap.Occurrences = []finding.Occurrence{{Line: 1}}

	for _, test := range []struct {
		name     string
		findings []finding.Finding
	}{
		{name: "invalid", findings: []finding.Finding{{}}},
		{name: "empty fingerprint", findings: []finding.Finding{emptyFingerprint}},
		{name: "stale fingerprint", findings: []finding.Finding{stale}},
		{name: "duplicate fingerprint", findings: []finding.Finding{valid, duplicate}},
		{name: "noncanonical occurrences", findings: []finding.Finding{noncanonical}},
		{name: "primary occurrence overlap", findings: []finding.Finding{overlap}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := BlockingMultiset(test.findings); err == nil || got != nil {
				t.Fatalf("BlockingMultiset() = (%#v, %v), want nil and error", got, err)
			}
		})
	}
}

func TestStrictlyShrinksRequiresSubsetAndOneStrictReduction(t *testing.T) {
	before := map[string]int{"a": 3, "b": 1}
	for _, test := range []struct {
		name  string
		after map[string]int
		want  bool
	}{
		{name: "occurrence removed", after: map[string]int{"a": 2, "b": 1}, want: true},
		{name: "fingerprint removed", after: map[string]int{"a": 3}, want: true},
		{name: "all removed", after: map[string]int{}, want: true},
		{name: "equal", after: map[string]int{"a": 3, "b": 1}},
		{name: "larger", after: map[string]int{"a": 4, "b": 1}},
		{name: "rotated", after: map[string]int{"a": 2, "c": 1}},
		{name: "nonpositive after", after: map[string]int{"a": 0, "b": 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := StrictlyShrinks(test.after, before); got != test.want {
				t.Fatalf("StrictlyShrinks(%v, %v) = %t, want %t", test.after, before, got, test.want)
			}
		})
	}
	if StrictlyShrinks(map[string]int{}, map[string]int{"a": 0}) {
		t.Fatal("StrictlyShrinks accepted a nonpositive before multiplicity")
	}
}

func planFinding(file string, line int, ruleID, snippet string) finding.Finding {
	got := finding.Finding{
		Gate: "lint", Language: "go", RuleID: ruleID, Severity: finding.Warning,
		File: file, Line: line, Snippet: snippet, Message: "fix " + snippet,
	}
	got.Fingerprint = finding.Fingerprint(got)
	return got
}

func expectedWaveDigest(findings []finding.Finding) string {
	ordered := append([]finding.Finding(nil), findings...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].File != ordered[right].File {
			return ordered[left].File < ordered[right].File
		}
		if ordered[left].Line != ordered[right].Line {
			return ordered[left].Line < ordered[right].Line
		}
		return ordered[left].Fingerprint < ordered[right].Fingerprint
	})
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/fix-wave-identity/v1\x00"))
	var encoded [8]byte
	for _, item := range ordered {
		binary.BigEndian.PutUint64(encoded[:], uint64(len(item.Fingerprint)))
		_, _ = hash.Write(encoded[:])
		_, _ = hash.Write([]byte(item.Fingerprint))
		binary.BigEndian.PutUint64(encoded[:], uint64(1+len(item.Occurrences)))
		_, _ = hash.Write(encoded[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func expectedBatchID(primaryFile, waveDigest string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/fix-batch-identity/v1\x00"))
	var length [8]byte
	for _, value := range []string{primaryFile, waveDigest} {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return "batch-" + hex.EncodeToString(hash.Sum(nil))
}
