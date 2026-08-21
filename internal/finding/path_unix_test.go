//go:build !windows

package finding

import "testing"

func TestFingerprintUsesNonWindowsNativePathSemantics(t *testing.T) {
	backslash := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir\\file.go", Snippet: "x"}
	slash := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir/file.go", Snippet: "x"}
	if Fingerprint(backslash) == Fingerprint(slash) {
		t.Fatal("Fingerprint() treated a literal backslash filename as a path separator")
	}

	cleaned := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir/./file.go", Snippet: "x"}
	if got, want := Fingerprint(cleaned), Fingerprint(slash); got != want {
		t.Fatalf("Fingerprint() = %q, want cleaned-path fingerprint %q", got, want)
	}
}
