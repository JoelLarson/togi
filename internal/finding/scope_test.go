package finding

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestFilterTouchedKeepsFindingWhenPrimaryOverlaps(t *testing.T) {
	finding := scopedFinding("src/file.go", 10, 20)
	got, err := FilterTouched([]Finding{finding}, ChangedLines{"src/file.go": {{Start: 15, End: 15}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if len(got) != 1 || got[0].Line != 10 || got[0].EndLine != 20 {
		t.Fatalf("FilterTouched() = %#v, want primary range 10-20", got)
	}
}

func TestFilterTouchedDropsPointOutsideChangedLine(t *testing.T) {
	finding := scopedFinding("src/file.go", 10, 0)
	got, err := FilterTouched([]Finding{finding}, ChangedLines{"src/file.go": {{Start: 11, End: 11}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("FilterTouched() = %#v, want no findings", got)
	}
}

func TestFilterTouchedFiltersOccurrencesIndividually(t *testing.T) {
	finding := scopedFinding("src/file.go", 10, 0)
	finding.Occurrences = []Occurrence{{Line: 30}, {Line: 40, EndLine: 50}}
	got, err := FilterTouched([]Finding{finding}, ChangedLines{"src/file.go": {{Start: 30, End: 30}, {Start: 45, End: 45}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if len(got) != 1 || got[0].Line != 30 || !reflect.DeepEqual(got[0].Occurrences, []Occurrence{{Line: 40, EndLine: 50}}) {
		t.Fatalf("FilterTouched() = %#v, want primary 30 and occurrence 40-50", got)
	}
}

func TestFilterTouchedPromotesEarliestSurvivingOccurrence(t *testing.T) {
	finding := scopedFinding("src/file.go", 10, 20)
	finding.Occurrences = []Occurrence{{Line: 30}, {Line: 40, EndLine: 50}}
	wantFingerprint := Fingerprint(finding)
	got, err := FilterTouched([]Finding{finding}, ChangedLines{"src/file.go": {{Start: 45, End: 45}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if len(got) != 1 || got[0].Line != 40 || got[0].EndLine != 50 || len(got[0].Occurrences) != 0 {
		t.Fatalf("FilterTouched() = %#v, want promoted 40-50", got)
	}
	if got[0].Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want unchanged %q", got[0].Fingerprint, wantFingerprint)
	}
}

func TestFilterTouchedSortsMultipleSurvivors(t *testing.T) {
	finding := scopedFinding("src/file.go", 10, 20)
	finding.Occurrences = []Occurrence{{Line: 50}, {Line: 30}, {Line: 40, EndLine: 45}}
	got, err := FilterTouched([]Finding{finding}, ChangedLines{"src/file.go": {{Start: 30, End: 50}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	want := []Occurrence{{Line: 30}, {Line: 40, EndLine: 45}, {Line: 50}}
	if len(got) != 1 || got[0].Line != 30 || !reflect.DeepEqual(got[0].Occurrences, want[1:]) {
		t.Fatalf("FilterTouched() = %#v, want primary 30 and occurrences %#v", got, want[1:])
	}
}

func TestFilterTouchedRequiresSameFile(t *testing.T) {
	got, err := FilterTouched([]Finding{scopedFinding("src/file.go", 10, 0)}, ChangedLines{"other.go": {{Start: 10, End: 10}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("FilterTouched() = %#v, want no findings", got)
	}
}

func TestFilterTouchedNormalizesFileSeparators(t *testing.T) {
	finding := scopedFinding(filepath.FromSlash("src/file.go"), 10, 0)
	got, err := FilterTouched([]Finding{finding}, ChangedLines{filepath.FromSlash("src/file.go"): {{Start: 10, End: 10}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if len(got) != 1 || got[0].File != "src/file.go" {
		t.Fatalf("FilterTouched() = %#v, want normalized matching finding", got)
	}
}

func TestFilterTouchedRejectsInvalidChangedRanges(t *testing.T) {
	for name, changed := range map[string]ChangedLines{
		"zero start":       {"file.go": {{Start: 0, End: 1}}},
		"end before start": {"file.go": {{Start: 3, End: 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FilterTouched(nil, changed); err == nil {
				t.Fatal("FilterTouched() error = nil, want validation error")
			}
		})
	}
}

func TestFilterTouchedRejectsChangedPathTraversal(t *testing.T) {
	for _, path := range []string{"../file.go", "dir/../../file.go", `..\file.go`} {
		t.Run(path, func(t *testing.T) {
			if _, err := FilterTouched(nil, ChangedLines{path: {{Start: 1, End: 1}}}); err == nil {
				t.Fatal("FilterTouched() error = nil, want path validation error")
			}
		})
	}
}

func TestFilterTouchedDoesNotMutateInputOrOccurrenceStorage(t *testing.T) {
	finding := scopedFinding("src/file.go", 10, 0)
	finding.Occurrences = []Occurrence{{Line: 30}, {Line: 20}}
	input := []Finding{finding}
	wantInput := cloneFindings(input)
	got, err := FilterTouched(input, ChangedLines{"src/file.go": {{Start: 20, End: 30}}})
	if err != nil {
		t.Fatalf("FilterTouched() error = %v", err)
	}
	if !reflect.DeepEqual(input, wantInput) {
		t.Fatalf("FilterTouched() mutated input: got %#v, want %#v", input, wantInput)
	}
	got[0].Occurrences[0].Line = 999
	if input[0].Occurrences[0].Line == 999 {
		t.Fatal("FilterTouched() reused caller occurrence storage")
	}
}

func TestFilterTouchedNilAndEmptyInputsAreDeterministic(t *testing.T) {
	for _, changed := range []ChangedLines{nil, ChangedLines{}} {
		got, err := FilterTouched(nil, changed)
		if err != nil {
			t.Fatalf("FilterTouched() error = %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("FilterTouched() = %#v, want non-nil empty result", got)
		}
	}
	got, err := FilterTouched([]Finding{}, ChangedLines{"file.go": {{Start: 1, End: 1}}})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("FilterTouched(empty) = %#v, %v; want non-nil empty result", got, err)
	}
}

func scopedFinding(file string, line, endLine int) Finding {
	finding := Finding{
		Gate: "lint", Language: "go", RuleID: "lint/rule", Severity: Warning,
		File: file, Line: line, EndLine: endLine, Snippet: "snippet", Message: "message",
	}
	finding.Fingerprint = Fingerprint(finding)
	return finding
}
