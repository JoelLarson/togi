package flywheel

import (
	"reflect"
	"testing"

	"github.com/joellarson/togi/internal/finding"
)

func TestIntegrityFindingIsCanonicalAndDeterministic(t *testing.T) {
	want := finding.Finding{
		Gate:        "integrity",
		Language:    "go",
		RuleID:      "togi/new-suppression",
		Severity:    finding.Error,
		File:        "pkg/check.go",
		Line:        7,
		Snippet:     "func check(): //nolint:errcheck",
		Occurrences: []finding.Occurrence{},
		Message:     "new suppression",
		Fingerprint: "04d91d57bcf841de368a8a8898f42f1485955dc931b42618fa3487030974e5d9",
	}

	first := integrityFinding(
		"togi/new-suppression",
		"pkg/check.go",
		7,
		"func check(): //nolint:errcheck",
		"new suppression",
	)
	second := integrityFinding(
		"togi/new-suppression",
		"pkg/check.go",
		7,
		"func check(): //nolint:errcheck",
		"new suppression",
	)

	if !reflect.DeepEqual(first, want) {
		t.Fatalf("integrityFinding() = %#v, want %#v", first, want)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("second integrityFinding() = %#v, want %#v", second, first)
	}
	if err := finding.Validate(first); err != nil {
		t.Fatalf("integrityFinding() is invalid: %v", err)
	}
}
