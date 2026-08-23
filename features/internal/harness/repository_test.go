package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepositoryBuildsHistoriesAndWorkingStates(t *testing.T) {
	repository, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}

	if branch, err := repository.Git("branch", "--show-current"); err != nil || branch != "main" {
		t.Fatalf("initial branch = %q, %v; want main, nil", branch, err)
	}

	if err := repository.Write("sp ace.txt", "first\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := repository.WriteBytes("binary.bin", []byte{0, 1, 2, 0xff}); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	first, err := repository.Commit("initial fixture")
	if err != nil || len(first) != 40 {
		t.Fatalf("Commit = %q, %v; want full object ID", first, err)
	}

	if err := repository.Branch("feature"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if err := repository.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := repository.Write("renamed.txt", "feature\n"); err != nil {
		t.Fatalf("Write feature: %v", err)
	}
	if _, err := repository.Commit("feature commit"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}
	if err := repository.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := repository.Write("main.txt", "main\n"); err != nil {
		t.Fatalf("Write main: %v", err)
	}
	if _, err := repository.Commit("main commit"); err != nil {
		t.Fatalf("Commit main: %v", err)
	}
	if merged, err := repository.Git("merge-base", "main", "feature"); err != nil || merged != first {

		t.Fatalf("merge-base = %q, %v; want %q, nil", merged, err, first)
	}

	if err := repository.Write("main.txt", "dirty tracked\n"); err != nil {
		t.Fatalf("write dirty tracked: %v", err)
	}
	if err := repository.Write("untracked.txt", "dirty untracked\n"); err != nil {
		t.Fatalf("write dirty untracked: %v", err)
	}
	if status, err := repository.Git("status", "--porcelain"); err != nil || !strings.Contains(status, " M main.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("status = %q, %v; want tracked and untracked changes", status, err)
	}
	if err := repository.Remove("untracked.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := repository.Write("delete-me.txt", "delete\n"); err != nil {
		t.Fatalf("Write delete candidate: %v", err)
	}
	if _, err := repository.Commit("add delete candidate"); err != nil {
		t.Fatalf("Commit delete candidate: %v", err)
	}
	if err := repository.Remove("delete-me.txt"); err != nil {
		t.Fatalf("Remove tracked: %v", err)
	}
	if _, err := repository.Commit("delete candidate"); err != nil {
		t.Fatalf("Commit deletion: %v", err)
	}

	tree, err := repository.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	wantTree := []string{"binary.bin", "main.txt", "sp ace.txt"}
	if !reflect.DeepEqual(tree, wantTree) {
		t.Fatalf("Tree = %#v, want %#v", tree, wantTree)
	}
}

func TestRepositoryCreatesRemoteRefsWorktreesAndSubmodules(t *testing.T) {
	source, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepository source: %v", err)
	}
	if err := source.Write("source.txt", "source\n"); err != nil {
		t.Fatalf("source Write: %v", err)
	}
	if _, err := source.Commit("source"); err != nil {
		t.Fatalf("source Commit: %v", err)
	}

	repository, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	if err := repository.Write("base.txt", "base\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := repository.Commit("base"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	localCommit, err := repository.Git("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve local HEAD: %v", err)
	}
	if err := repository.SetOriginHEAD("main", localCommit); err != nil {
		t.Fatalf("SetOriginHEAD: %v", err)
	}
	if reference, err := repository.Git("rev-parse", "refs/remotes/origin/main"); err != nil || reference != localCommit {
		t.Fatalf("origin/main = %q, %v; want %q, nil", reference, err, localCommit)
	}

	worktreePath := filepath.Join(t.TempDir(), "linked")
	linked, err := repository.LinkedWorktree(worktreePath, "linked")
	if err != nil {
		t.Fatalf("LinkedWorktree: %v", err)
	}
	if err := linked.Write("linked.txt", "linked\n"); err != nil {
		t.Fatalf("linked Write: %v", err)
	}
	if _, err := linked.Commit("linked"); err != nil {
		t.Fatalf("linked Commit: %v", err)
	}
	if _, err := repository.Git("rev-parse", "linked"); err != nil {
		t.Fatalf("linked branch absent: %v", err)
	}

	if err := repository.AddSubmodule("vendor/source", source.Root); err != nil {
		t.Fatalf("AddSubmodule: %v", err)
	}
	if _, err := repository.Commit("add submodule"); err != nil {
		t.Fatalf("Commit submodule: %v", err)
	}
	if entry, err := repository.Git("ls-tree", "HEAD", "vendor/source"); err != nil || !strings.HasPrefix(entry, "160000 commit ") {
		t.Fatalf("submodule tree entry = %q, %v; want gitlink, nil", entry, err)
	}
}

func TestRepositoryRejectsPathsOutsideRoot(t *testing.T) {
	repository, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}

	for _, path := range []string{"", "/absolute", "../outside", "nested/../../outside"} {
		if err := repository.Write(path, "nope"); err == nil {
			t.Errorf("Write(%q) succeeded", path)
		}
		if err := repository.WriteBytes(path, []byte("nope")); err == nil {
			t.Errorf("WriteBytes(%q) succeeded", path)
		}
		if err := repository.Remove(path); err == nil {
			t.Errorf("Remove(%q) succeeded", path)
		}
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(repository.Root), "outside")); !os.IsNotExist(err) {
		t.Fatalf("outside path state = %v, want nonexistent", err)
	}
}

func TestRepositorySupportsUnrelatedHistories(t *testing.T) {
	repository, err := NewRepository(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	if err := repository.Write("first.txt", "first\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := repository.Commit("first"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := repository.Git("checkout", "--orphan", "unrelated"); err != nil {
		t.Fatalf("Checkout orphan: %v", err)
	}
	if err := repository.Remove("first.txt"); err != nil {
		t.Fatalf("Remove inherited index file: %v", err)
	}
	if err := repository.Write("other.txt", "other\n"); err != nil {
		t.Fatalf("Write orphan: %v", err)
	}
	if _, err := repository.Commit("unrelated"); err != nil {
		t.Fatalf("Commit orphan: %v", err)
	}
	if _, err := repository.Git("merge-base", "main", "unrelated"); err == nil {
		t.Fatal("merge-base for unrelated histories succeeded")
	}
}
