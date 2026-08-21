//go:build windows

package finding

import "testing"

func TestFingerprintUsesWindowsNativePathSemantics(t *testing.T) {
	backslash := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir\\file.go", Snippet: "x"}
	slash := Finding{Gate: "lint", RuleID: "lint/rule", File: "dir/file.go", Snippet: "x"}
	if got, want := Fingerprint(backslash), Fingerprint(slash); got != want {
		t.Fatalf("Fingerprint() = %q, want native-separator fingerprint %q", got, want)
	}
}
