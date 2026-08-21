package main

import (
	"bytes"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(streams{out: &stdout, err: &stderr})
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version command: %v", err)
	}
	if got, want := stdout.String(), "togi dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
