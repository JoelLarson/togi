//go:build linux

package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if code, handled := runAgentHelperFromEnvironment(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestAgentToolAppliesCrossFileEditsAndDeletion(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	defer environment.Close()
	root := filepath.Join(environment.TempRoot, "worktree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "delete.go"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := environment.InstallAgent("codex", AgentBehavior{
		Edits:  map[string]string{"feature.go": "package fixture\n", "related.go": "package fixture\n"},
		Delete: []string{"delete.go"},
	})
	if err != nil {
		t.Fatalf("InstallAgent: %v", err)
	}

	result := invokeAgentFixture(t, context.Background(), path, root, "brief\n")
	if result.err != nil || string(result.stdout) != "{\"type\":\"turn.completed\"}\n" {
		t.Fatalf("invoke = stdout %q, err %v", result.stdout, result.err)
	}
	for name, want := range map[string]string{"feature.go": "package fixture\n", "related.go": "package fixture\n"} {
		got, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, readErr, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "delete.go")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleted file stat = %v", err)
	}
	invocations, err := environment.AgentInvocations("codex")
	if err != nil {
		t.Fatalf("AgentInvocations: %v", err)
	}
	if len(invocations) != 1 || invocations[0].Brief != "brief\n" || invocations[0].Root != root {
		t.Fatalf("invocations = %#v", invocations)
	}
}

func TestAgentToolPreservesPayloadWhenCleaningEditPath(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	root := filepath.Join(environment.TempRoot, "worktree")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := environment.InstallAgent("codex", AgentBehavior{Edits: map[string]string{"nested/./feature.go": "payload"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := invokeAgentFixture(t, context.Background(), path, root, "brief"); result.err != nil {
		t.Fatal(result.err)
	}
	got, err := os.ReadFile(filepath.Join(root, "nested", "feature.go"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("cleaned edit = %q, %v", got, err)
	}
}

func TestAgentToolReplacesHardlinkedEditWithoutMutatingOtherLink(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	root := filepath.Join(environment.TempRoot, "worktree")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(environment.TempRoot, "external.go")
	if err := os.WriteFile(external, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(root, "feature.go")); err != nil {
		t.Fatal(err)
	}
	path, err := environment.InstallAgent("codex", AgentBehavior{Edits: map[string]string{"feature.go": "replacement"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := invokeAgentFixture(t, context.Background(), path, root, "brief"); result.err != nil {
		t.Fatal(result.err)
	}
	gotExternal, err := os.ReadFile(external)
	if err != nil || string(gotExternal) != "sentinel" {
		t.Fatalf("external hardlink = %q, %v", gotExternal, err)
	}
	gotFeature, err := os.ReadFile(filepath.Join(root, "feature.go"))
	if err != nil || string(gotFeature) != "replacement" {
		t.Fatalf("feature edit = %q, %v", gotFeature, err)
	}
}

func TestAgentToolSupportsNoOpMalformedTimeoutAndGitMutation(t *testing.T) {
	for _, test := range []struct {
		name       string
		behavior   AgentBehavior
		timeout    time.Duration
		wantOutput string
		wantErr    bool
		wantHead   string
	}{
		{name: "no-op", wantOutput: "{\"type\":\"turn.completed\"}\n"},
		{name: "malformed", behavior: AgentBehavior{MalformedJSONL: true}, wantOutput: "{malformed\n"},
		{name: "timeout", behavior: AgentBehavior{Sleep: time.Second}, timeout: 30 * time.Millisecond, wantErr: true},
		{name: "git mutation", behavior: AgentBehavior{GitArgs: []string{"commit", "--allow-empty", "-m", "agent mutation"}}, wantOutput: "{\"type\":\"turn.completed\"}\n", wantHead: "agent mutation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment, err := NewEnvironment()
			if err != nil {
				t.Fatalf("NewEnvironment: %v", err)
			}
			defer environment.Close()
			root := filepath.Join(environment.TempRoot, "worktree")
			repository, err := NewRepository(root)
			if err != nil {
				t.Fatalf("NewRepository: %v", err)
			}
			if err := repository.Write("go.mod", "module fixture\n\ngo 1.25\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.Commit("base"); err != nil {
				t.Fatal(err)
			}
			path, err := environment.InstallAgent("codex", test.behavior)
			if err != nil {
				t.Fatalf("InstallAgent: %v", err)
			}
			ctx := context.Background()
			if test.timeout != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.timeout)
				defer cancel()
			}
			result := invokeAgentFixture(t, ctx, path, root, "brief")
			if (result.err != nil) != test.wantErr || string(result.stdout) != test.wantOutput {
				t.Fatalf("invoke = stdout %q, err %v", result.stdout, result.err)
			}
			if test.wantHead != "" {
				got, err := repository.Git("log", "-1", "--format=%s")
				if err != nil || got != test.wantHead {
					t.Fatalf("HEAD subject = %q, %v", got, err)
				}
			}
		})
	}
}

func TestAgentToolRejectsUnexpectedProductionArguments(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	path, err := environment.InstallAgent("codex", AgentBehavior{})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(path, "unexpected")
	command.Dir = environment.TempRoot
	if err := command.Run(); err == nil {
		t.Fatal("unexpected argv succeeded")
	}
}

func TestAgentToolDoesNotFollowEditSymlinks(t *testing.T) {
	for _, test := range []struct {
		name     string
		editPath string
		prepare  func(*testing.T, string, string)
	}{
		{
			name:     "parent",
			editPath: "linked/target.go",
			prepare: func(t *testing.T, root, outside string) {
				t.Helper()
				if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "final",
			editPath: "target.go",
			prepare: func(t *testing.T, root, outside string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(outside, "target.go"), filepath.Join(root, "target.go")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment, err := NewEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			defer environment.Close()
			root := filepath.Join(environment.TempRoot, "worktree")
			outside := t.TempDir()
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "target.go"), []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, root, outside)
			path, err := environment.InstallAgent("codex", AgentBehavior{Edits: map[string]string{test.editPath: "escaped"}})
			if err != nil {
				t.Fatal(err)
			}
			if result := invokeAgentFixture(t, context.Background(), path, root, "brief"); result.err == nil {
				t.Fatal("symlink edit succeeded")
			}
			got, err := os.ReadFile(filepath.Join(outside, "target.go"))
			if err != nil || string(got) != "sentinel" {
				t.Fatalf("outside target = %q, %v", got, err)
			}
		})
	}
}

func TestAgentToolDoesNotFollowDeleteSymlinks(t *testing.T) {
	for _, final := range []bool{false, true} {
		name := "parent"
		if final {
			name = "final"
		}
		t.Run(name, func(t *testing.T) {
			environment, err := NewEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			defer environment.Close()
			root := filepath.Join(environment.TempRoot, "worktree")
			outside := t.TempDir()
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "target.go"), []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			deletePath := "linked/target.go"
			if final {
				deletePath = "target.go"
				if err := os.Symlink(filepath.Join(outside, "target.go"), filepath.Join(root, deletePath)); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
				t.Fatal(err)
			}
			path, err := environment.InstallAgent("codex", AgentBehavior{Delete: []string{deletePath}})
			if err != nil {
				t.Fatal(err)
			}
			_ = invokeAgentFixture(t, context.Background(), path, root, "brief")
			got, err := os.ReadFile(filepath.Join(outside, "target.go"))
			if err != nil || string(got) != "sentinel" {
				t.Fatalf("outside target = %q, %v", got, err)
			}
		})
	}
}

func TestInstallAgentAtomicallyReplacesFinalSymlink(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	outside := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(environment.BinRoot, "codex")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.InstallAgent("codex", AgentBehavior{}); err != nil {
		t.Fatalf("InstallAgent: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("installed executable = %v, %v", info, err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "sentinel" {
		t.Fatalf("outside executable = %q, %v", got, err)
	}
}

func TestInstallAgentRejectsSymlinkedBinRoot(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	outside := t.TempDir()
	if err := os.Remove(environment.BinRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, environment.BinRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.InstallAgent("codex", AgentBehavior{}); err == nil {
		t.Fatal("InstallAgent accepted symlinked BinRoot")
	}
	if _, err := os.Lstat(filepath.Join(outside, "codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside executable stat = %v", err)
	}
}

func TestRestrictedPathKeepsNamedToolsAndHidesAmbientAgent(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	if err := environment.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := environment.RestrictPath("go", "git"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "git"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Errorf("retained tool %q: %v", name, err)
		}
	}
	if _, err := exec.LookPath("codex"); err == nil {
		t.Fatal("ambient codex remained visible")
	}
}

type agentFixtureResult struct {
	stdout []byte
	err    error
}

func invokeAgentFixture(t *testing.T, ctx context.Context, executable, root, brief string) agentFixtureResult {
	t.Helper()
	args := []string{"--ask-for-approval", "never", "exec", "--ephemeral", "--json", "--sandbox", "workspace-write", "--ignore-user-config", "--cd", root, "-"}
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = root
	command.Stdin = strings.NewReader(brief)
	stdout, err := command.Output()
	return agentFixtureResult{stdout: stdout, err: err}
}
