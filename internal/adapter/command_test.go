package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandDeliversBriefOnStdinInWorktreeCwd(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "agent.sh")
	contents := "#!/bin/sh\nset -eu\ncat > received-brief\npwd > received-cwd\nprintf '%s\\n' 'edited' > feature.go\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	result, err := NewCommand("kimi", script).Run(context.Background(), Request{Root: root, Brief: "batch brief\n", Sink: sink})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Usage != nil {
		t.Fatalf("Usage = %#v, want nil", result.Usage)
	}
	if got := string(readHelperFile(t, root, "received-brief")); got != "batch brief\n" {
		t.Fatalf("stdin = %q", got)
	}
	cwd, err := os.ReadFile(filepath.Join(root, "received-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Clean(string(bytesTrimNewline(cwd))); got != root {
		t.Fatalf("cwd = %q, want %q", got, root)
	}
	if got := string(readHelperFile(t, root, "feature.go")); got != "edited\n" {
		t.Fatalf("worktree edit = %q", got)
	}
	if len(sink.writes) != 1 {
		t.Fatalf("sink writes = %d, want 1", len(sink.writes))
	}
}

func TestCommandNameIsTheConfiguredVendor(t *testing.T) {
	if got := NewCommand("claude", "claude").Name(); got != "claude" {
		t.Fatalf("Name() = %q, want claude", got)
	}
}

func bytesTrimNewline(value []byte) []byte {
	if n := len(value); n > 0 && value[n-1] == '\n' {
		return value[:n-1]
	}
	return value
}
