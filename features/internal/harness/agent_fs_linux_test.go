//go:build linux

package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

const installAgentUmaskRoot = "TOGI_ACCEPTANCE_INSTALL_UMASK_ROOT"

func TestInstallAgentSetsExecutableModeDespiteUmask(t *testing.T) {
	if root := os.Getenv(installAgentUmaskRoot); root != "" {
		environment := &Environment{TempRoot: root, BinRoot: filepath.Join(root, "bin")}
		if err := os.Mkdir(environment.BinRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "agents", "codex", "invocations"), 0o700); err != nil {
			t.Fatal(err)
		}
		syscall.Umask(0o111)
		if _, err := environment.InstallAgent("codex", AgentBehavior{}); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestInstallAgentSetsExecutableModeDespiteUmask$")
	command.Env = append(os.Environ(), installAgentUmaskRoot+"="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("umask helper: %v: %s", err, output)
	}
	info, err := os.Stat(filepath.Join(root, "bin", "codex"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
		t.Fatalf("installed mode = %v, %v; want regular 0700", info, err)
	}
}

func TestAgentToolRejectsConcurrentFixtureMutator(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	root := filepath.Join(environment.TempRoot, "worktree")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rootFD, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(rootFD)
	if err := syscall.Flock(rootFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	path, err := environment.InstallAgent("codex", AgentBehavior{Edits: map[string]string{"feature.go": "mutated"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := invokeAgentFixture(t, context.Background(), path, root, "brief"); result.err == nil {
		t.Fatal("concurrent fake mutator succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "feature.go")); !os.IsNotExist(err) {
		t.Fatalf("feature stat after rejected mutation = %v", err)
	}
}

func TestSecureAtomicWriteRejectsDetectedParentRename(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	moved := filepath.Join(base, "moved")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "nested", "feature.go")
	if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentFD, final, err := secureParent(root, "nested/feature.go", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(parentFD)
	if err := os.Rename(filepath.Join(root, "nested"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secureAtomicWriteAt(root, "nested/feature.go", parentFD, final, []byte("replacement"), 0o600); err == nil {
		t.Fatal("atomic write accepted renamed parent")
	}
	got, err := os.ReadFile(filepath.Join(moved, "feature.go"))
	if err != nil || string(got) != "sentinel" {
		t.Fatalf("moved external target = %q, %v", got, err)
	}
}
