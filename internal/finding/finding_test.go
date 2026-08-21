package finding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFingerprintKeysOnlyNormalizedIdentity(t *testing.T) {
	base := Finding{
		Gate:        "lint",
		Language:    "go",
		RuleID:      "lint/example",
		Severity:    Warning,
		File:        "internal\\finding\\finding.go",
		Line:        12,
		EndLine:     14,
		Snippet:     "func   Example( )",
		Occurrences: []Occurrence{{Line: 28, EndLine: 30}},
		Message:     "first message",
	}

	want := expectedFingerprint(base)
	if got := Fingerprint(base); got != want {
		t.Fatalf("Fingerprint(base) = %q, want %q", got, want)
	}

	for _, change := range []struct {
		name  string
		apply func(*Finding)
	}{
		{"language", func(f *Finding) { f.Language = "rust" }},
		{"severity", func(f *Finding) { f.Severity = Error }},
		{"message", func(f *Finding) { f.Message = "different" }},
		{"occurrences", func(f *Finding) { f.Occurrences = []Occurrence{{Line: 100}} }},
		{"line", func(f *Finding) { f.Line = 99 }},
		{"end line", func(f *Finding) { f.EndLine = 101 }},
		{"snippet whitespace", func(f *Finding) { f.Snippet = "\nfunc\tExample( )  " }},
	} {
		t.Run(change.name, func(t *testing.T) {
			got := cloneFinding(base)
			change.apply(&got)
			if fingerprint := Fingerprint(got); fingerprint != want {
				t.Fatalf("Fingerprint() = %q, want unchanged %q", fingerprint, want)
			}
		})
	}

	for _, change := range []struct {
		name  string
		apply func(*Finding)
	}{
		{"gate", func(f *Finding) { f.Gate = "complexity" }},
		{"rule ID", func(f *Finding) { f.RuleID = "lint/other" }},
		{"file", func(f *Finding) { f.File = "internal/finding/other.go" }},
		{"snippet", func(f *Finding) { f.Snippet = "func Different()" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			got := cloneFinding(base)
			change.apply(&got)
			if fingerprint := Fingerprint(got); fingerprint == want {
				t.Fatal("Fingerprint() did not change")
			}
		})
	}
}

func TestFingerprintUsesLengthDelimitedFields(t *testing.T) {
	first := Finding{Gate: "ab", RuleID: "c"}
	second := Finding{Gate: "a", RuleID: "bc"}

	if got, want := Fingerprint(first), expectedFingerprint(first); got != want {
		t.Fatalf("Fingerprint(first) = %q, want %q", got, want)
	}
	if got, want := Fingerprint(second), expectedFingerprint(second); got != want {
		t.Fatalf("Fingerprint(second) = %q, want %q", got, want)
	}
	if Fingerprint(first) == Fingerprint(second) {
		t.Fatal("ambiguous tuples received the same fingerprint")
	}
}

func TestFingerprintNormalizesLiteralBackslashSeparators(t *testing.T) {
	backslash := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir\\nested\\file.go", Snippet: "x"}
	slash := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir/nested/file.go", Snippet: "x"}

	if got, want := Fingerprint(backslash), Fingerprint(slash); got != want {
		t.Fatalf("Fingerprint() = %q, want normalized-path fingerprint %q", got, want)
	}
}

func TestGroupCollapsesLocationsAndPreservesPrimaryFinding(t *testing.T) {
	findings := []Finding{
		{
			Gate:        "lint",
			Language:    "go",
			RuleID:      "lint/example",
			Severity:    Warning,
			File:        "dir\\file.go",
			Line:        30,
			EndLine:     32,
			Snippet:     "raw    later",
			Message:     "later message",
			Occurrences: []Occurrence{{Line: 20, EndLine: 22}, {Line: 10, EndLine: 12}, {Line: 20, EndLine: 22}},
		},
		{
			Gate:        "lint",
			Language:    "go",
			RuleID:      "lint/example",
			Severity:    Error,
			File:        "dir/file.go",
			Line:        5,
			EndLine:     7,
			Snippet:     "raw later",
			Message:     "earliest message",
			Occurrences: []Occurrence{{Line: 30, EndLine: 32}},
		},
	}

	got := Group(findings)
	if len(got) != 1 {
		t.Fatalf("len(Group()) = %d, want 1", len(got))
	}
	grouped := got[0]
	if grouped.File != "dir/file.go" {
		t.Fatalf("grouped file = %q, want forward-slash path", grouped.File)
	}
	if grouped.Line != 5 || grouped.EndLine != 7 {
		t.Fatalf("primary location = %d:%d, want 5:7", grouped.Line, grouped.EndLine)
	}
	if grouped.Snippet != "raw later" || grouped.Message != "earliest message" || grouped.Severity != Error {
		t.Fatalf("primary finding fields = %#v, want earliest finding's raw fields", grouped)
	}
	wantOccurrences := []Occurrence{{Line: 10, EndLine: 12}, {Line: 20, EndLine: 22}, {Line: 30, EndLine: 32}}
	if !reflect.DeepEqual(grouped.Occurrences, wantOccurrences) {
		t.Fatalf("occurrences = %#v, want %#v", grouped.Occurrences, wantOccurrences)
	}
}

func TestGroupComputesMissingFingerprintAndUsesSuppliedFingerprint(t *testing.T) {
	missing := Finding{Gate: "lint", RuleID: "lint/missing", File: "file.go", Line: 1, Snippet: "missing"}
	grouped := Group([]Finding{missing})
	if len(grouped) != 1 {
		t.Fatalf("len(Group()) = %d, want 1", len(grouped))
	}
	if got, want := grouped[0].Fingerprint, Fingerprint(missing); got != want || got == "" {
		t.Fatalf("computed fingerprint = %q, want nonempty %q", got, want)
	}

	const supplied = "supplied-fingerprint"
	grouped = Group([]Finding{
		{Gate: "lint", RuleID: "lint/first", File: "file.go", Line: 1, Snippet: "first", Fingerprint: supplied},
		{Gate: "lint", RuleID: "lint/second", File: "file.go", Line: 2, Snippet: "second", Fingerprint: supplied},
	})
	if len(grouped) != 1 {
		t.Fatalf("len(Group()) with a supplied fingerprint = %d, want 1", len(grouped))
	}
	if got := grouped[0].Fingerprint; got != supplied {
		t.Fatalf("supplied fingerprint = %q, want %q", got, supplied)
	}
}

func TestGroupUsesEndLineToBreakEqualLineTies(t *testing.T) {
	grouped := Group([]Finding{
		{Gate: "lint", RuleID: "lint/rule", File: "file.go", Line: 11, EndLine: 19, Snippet: "same", Message: "longer"},
		{Gate: "lint", RuleID: "lint/rule", File: "file.go", Line: 11, EndLine: 13, Snippet: "same", Message: "shorter"},
	})
	if len(grouped) != 1 {
		t.Fatalf("len(Group()) = %d, want 1", len(grouped))
	}
	if got := grouped[0]; got.Line != 11 || got.EndLine != 13 || got.Message != "shorter" {
		t.Fatalf("primary = %#v, want shorter range and its raw fields", got)
	}
	if got, want := grouped[0].Occurrences, []Occurrence{{Line: 11, EndLine: 19}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("occurrences = %#v, want %#v", got, want)
	}
}

func TestGroupSortsByLocationThenIdentity(t *testing.T) {
	findings := []Finding{
		{Gate: "z", RuleID: "z/rule", File: "b.go", Line: 1, Snippet: "z"},
		{Gate: "a", RuleID: "a/two", File: "a.go", Line: 2, Snippet: "a"},
		{Gate: "b", RuleID: "b/rule", File: "a.go", Line: 2, Snippet: "b"},
		{Gate: "a", RuleID: "a/one", File: "a.go", Line: 2, Snippet: "one"},
		{Gate: "a", RuleID: "a/rule", File: "a.go", Line: 1, Snippet: "a"},
	}

	got := Group(findings)
	if len(got) != len(findings) {
		t.Fatalf("len(Group()) = %d, want %d", len(got), len(findings))
	}
	for index, want := range []Finding{
		findings[4], findings[3], findings[1], findings[2], findings[0],
	} {
		if got[index].File != want.File || got[index].Line != want.Line || got[index].Gate != want.Gate || got[index].RuleID != want.RuleID {
			t.Fatalf("Group()[%d] = (%q, %d, %q, %q), want (%q, %d, %q, %q)", index, got[index].File, got[index].Line, got[index].Gate, got[index].RuleID, want.File, want.Line, want.Gate, want.RuleID)
		}
	}
}

func TestGroupSortsEqualLocationAndIdentityByFingerprint(t *testing.T) {
	findings := []Finding{
		{Gate: "lint", RuleID: "lint/rule", File: "file.go", Line: 4, Snippet: "first"},
		{Gate: "lint", RuleID: "lint/rule", File: "file.go", Line: 4, Snippet: "second"},
	}

	grouped := Group(findings)
	if len(grouped) != 2 {
		t.Fatalf("len(Group()) = %d, want 2", len(grouped))
	}
	if got, want := grouped[0].Fingerprint, Fingerprint(findings[0]); got != want && got != Fingerprint(findings[1]) {
		t.Fatalf("first fingerprint = %q, want a valid input fingerprint", got)
	}
	if got, want := grouped[1].Fingerprint, Fingerprint(findings[0]); got != want && got != Fingerprint(findings[1]) {
		t.Fatalf("second fingerprint = %q, want a valid input fingerprint", got)
	}
	if grouped[0].Fingerprint >= grouped[1].Fingerprint {
		t.Fatalf("fingerprints = (%q, %q), want lexical order", grouped[0].Fingerprint, grouped[1].Fingerprint)
	}
}

func TestGroupDoesNotMutateInputAndIsIdempotent(t *testing.T) {
	findings := []Finding{
		{Gate: "lint", RuleID: "lint/rule", File: "a\\file.go", Line: 10, Snippet: "same", Occurrences: []Occurrence{{Line: 20}}},
		{Gate: "lint", RuleID: "lint/rule", File: "a/file.go", Line: 5, Snippet: "same", Occurrences: []Occurrence{{Line: 10}}},
	}
	wantInput := cloneFindings(findings)

	first := Group(findings)
	if !reflect.DeepEqual(findings, wantInput) {
		t.Fatalf("Group() mutated input: got %#v, want %#v", findings, wantInput)
	}
	second := Group(first)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("Group(Group(findings)) = %#v, want %#v", second, first)
	}

	first[0].Occurrences[0].Line = 999
	if findings[0].Occurrences[0].Line == 999 {
		t.Fatal("Group() reused caller occurrence storage")
	}
}

func TestFindingJSONSchemaAndSeverityRoundTrip(t *testing.T) {
	finding := Finding{
		Gate:        "lint",
		Language:    "go",
		RuleID:      "lint/rule",
		Severity:    Warning,
		File:        "file.go",
		Line:        4,
		Snippet:     "x",
		Message:     "example",
		Fingerprint: "abc123",
	}

	encoded, err := json.Marshal(finding)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	const wantJSON = `{"gate":"lint","language":"go","rule_id":"lint/rule","severity":"warning","file":"file.go","line":4,"snippet":"x","message":"example","fingerprint":"abc123"}`
	if got := string(encoded); got != wantJSON {
		t.Fatalf("JSON = %s, want %s", got, wantJSON)
	}

	for _, severity := range []Severity{Error, Warning, Info} {
		t.Run(string(severity), func(t *testing.T) {
			encoded, err := json.Marshal(Finding{Severity: severity})
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			var decoded Finding
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if decoded.Severity != severity {
				t.Fatalf("decoded severity = %q, want %q", decoded.Severity, severity)
			}
		})
	}

	withLocations := finding
	withLocations.EndLine = 8
	withLocations.Occurrences = []Occurrence{{Line: 10}, {Line: 12, EndLine: 14}}
	encoded, err = json.Marshal(withLocations)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	const wantLocationsJSON = `{"gate":"lint","language":"go","rule_id":"lint/rule","severity":"warning","file":"file.go","line":4,"end_line":8,"snippet":"x","occurrences":[{"line":10},{"line":12,"end_line":14}],"message":"example","fingerprint":"abc123"}`
	if got := string(encoded); got != wantLocationsJSON {
		t.Fatalf("JSON = %s, want %s", got, wantLocationsJSON)
	}
}

func expectedFingerprint(f Finding) string {
	hash := sha256.New()
	for _, field := range []string{f.Gate, f.RuleID, strings.ReplaceAll(f.File, "\\", "/"), strings.Join(strings.Fields(f.Snippet), " ")} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneFinding(f Finding) Finding {
	f.Occurrences = append([]Occurrence(nil), f.Occurrences...)
	return f
}

func cloneFindings(findings []Finding) []Finding {
	cloned := make([]Finding, len(findings))
	for index, finding := range findings {
		cloned[index] = cloneFinding(finding)
	}
	return cloned
}
