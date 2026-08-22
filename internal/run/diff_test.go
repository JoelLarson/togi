package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/finding"
)

func TestResolveDiffDetectsOriginHeadAndChangedLines(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "edit.go", "one\ntwo\nthree\nfour\n")
	base := commitDiffTestRepo(t, repo, "base")
	setDiffTestOriginHead(t, repo, "main", base)

	writeDiffTestFile(t, repo, "edit.go", "one\nTWO\nthree\nFOUR")
	writeDiffTestFile(t, repo, "new.go", "first\nsecond")
	head := commitDiffTestRepo(t, repo, "feature")

	got, err := resolveDiff(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	wantLines := finding.ChangedLines{
		"edit.go": {{Start: 2, End: 2}, {Start: 4, End: 4}},
		"new.go":  {{Start: 1, End: 2}},
	}
	if got.BaseRef != "origin/main" || got.BaseCommit != base || got.MergeBase != base || got.Head != head {
		t.Fatalf("resolveDiff() commits = %#v, want base origin/main at %s and head %s", got, base, head)
	}
	if got.ChangedFiles != 2 || got.ChangedLines != 4 || !reflect.DeepEqual(got.Lines, wantLines) {
		t.Fatalf("resolveDiff() scope = %#v, want 2 files, 4 lines, %#v", got, wantLines)
	}
	assertFullObjectID(t, got.BaseCommit)
	assertFullObjectID(t, got.MergeBase)
	assertFullObjectID(t, got.Head)
}

func TestResolveDiffExplicitBaseTakesPrecedence(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	setDiffTestOriginHead(t, repo, "main", base)
	writeDiffTestFile(t, repo, "file.go", "middle\n")
	explicit := commitDiffTestRepo(t, repo, "middle")
	gitDiffTest(t, repo, "tag", "comparison", explicit)
	writeDiffTestFile(t, repo, "file.go", "head\n")
	commitDiffTestRepo(t, repo, "head")

	got, err := resolveDiff(context.Background(), repo, "comparison")
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	if got.BaseRef != "comparison" || got.BaseCommit != explicit || got.MergeBase != explicit {
		t.Fatalf("resolveDiff() = %#v, want explicit comparison base %s", got, explicit)
	}
}

func TestResolveDiffRequiresBaseWhenOriginHeadIsMissing(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	commitDiffTestRepo(t, repo, "base")

	_, err := resolveDiff(context.Background(), repo, "")
	if err == nil || !strings.Contains(err.Error(), "--base") {
		t.Fatalf("resolveDiff() error = %v, want --base diagnostic", err)
	}
}

func TestResolveDiffRejectsInvalidBasesSafely(t *testing.T) {
	for _, base := range []string{"missing-ref", "--help", "   ", "bad\nref"} {
		t.Run(strings.ReplaceAll(base, "\n", "newline"), func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, "file.go", "base\n")
			commitDiffTestRepo(t, repo, "base")

			_, err := resolveDiff(context.Background(), repo, base)
			if err == nil || !strings.Contains(err.Error(), "base") {
				t.Fatalf("resolveDiff(%q) error = %v, want base diagnostic", base, err)
			}
			if strings.Contains(err.Error(), "usage:") || strings.Contains(err.Error(), "file.go") {
				t.Fatalf("resolveDiff(%q) exposed command output: %v", base, err)
			}
		})
	}
}

func TestResolveDiffRejectsUnrelatedHistories(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "main.go", "main\n")
	base := commitDiffTestRepo(t, repo, "main")
	gitDiffTest(t, repo, "checkout", "--orphan", "unrelated")
	gitDiffTest(t, repo, "rm", "-rf", "--", ".")
	writeDiffTestFile(t, repo, "other.go", "other\n")
	commitDiffTestRepo(t, repo, "unrelated")

	_, err := resolveDiff(context.Background(), repo, base)
	if err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("resolveDiff() error = %v, want unrelated histories diagnostic", err)
	}
}

func TestResolveDiffHonorsCancellation(t *testing.T) {
	repo := newDiffTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := resolveDiff(ctx, repo, "main")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveDiff() error = %v, want context.Canceled", err)
	}
}

func TestResolveDiffAnchorsPureDeletionsInCurrentFiles(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "middle.go", "one\ntwo\nthree\n")
	writeDiffTestFile(t, repo, "tail.go", "one\ntwo\nthree")
	writeDiffTestFile(t, repo, "empty.go", "only\n")
	writeDiffTestFile(t, repo, "deleted.go", "gone\n")
	base := commitDiffTestRepo(t, repo, "base")

	writeDiffTestFile(t, repo, "middle.go", "one\nthree\n")
	writeDiffTestFile(t, repo, "tail.go", "one\n")
	writeDiffTestFile(t, repo, "empty.go", "")
	removeDiffTestFile(t, repo, "deleted.go")
	commitDiffTestRepo(t, repo, "deletions")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{
		"empty.go":  {},
		"middle.go": {{Start: 1, End: 1}},
		"tail.go":   {{Start: 1, End: 1}},
	}
	if got.ChangedFiles != 3 || got.ChangedLines != 2 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want %#v", got, want)
	}
	if _, exists := got.Lines["deleted.go"]; exists {
		t.Fatal("wholly deleted path survived the required diff filter")
	}
}

func TestResolveDiffUsesRenameDestinationAndNULSafePaths(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "old name.go", "same\n")
	writeDiffTestFile(t, repo, "tab\tname.go", "old\n")
	base := commitDiffTestRepo(t, repo, "base")

	if err := os.Rename(filepath.Join(repo, "old name.go"), filepath.Join(repo, "new name.go")); err != nil {
		t.Fatal(err)
	}
	writeDiffTestFile(t, repo, "tab\tname.go", "new\n")
	commitDiffTestRepo(t, repo, "rename and edit")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{
		"new name.go":  {{Start: 1, End: 1}},
		"tab\tname.go": {{Start: 1, End: 1}},
	}
	if got.ChangedFiles != 2 || got.ChangedLines != 2 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want %#v", got, want)
	}
}

func TestResolveDiffCountsBinaryFileWithoutLineSites(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "image.bin", "\x00old")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "image.bin", "\x00new")
	commitDiffTestRepo(t, repo, "binary")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"image.bin": {}}
	if got.ChangedFiles != 1 || got.ChangedLines != 0 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want one binary path and no lines", got)
	}
}

func TestResolveDiffRejectsDirtyWorktreeWithoutEchoingStatus(t *testing.T) {
	for _, test := range []struct {
		name  string
		dirty func(*testing.T, string)
	}{
		{name: "staged", dirty: func(t *testing.T, repo string) {
			writeDiffTestFile(t, repo, "staged-secret.go", "staged\n")
			gitDiffTest(t, repo, "add", "--", "staged-secret.go")
		}},
		{name: "unstaged", dirty: func(t *testing.T, repo string) {
			writeDiffTestFile(t, repo, "tracked.go", "changed secret\n")
		}},
		{name: "untracked", dirty: func(t *testing.T, repo string) {
			writeDiffTestFile(t, repo, "untracked-secret.go", "secret\n")
		}},
		{name: "conflicted", dirty: func(t *testing.T, repo string) {
			gitDiffTest(t, repo, "branch", "side")
			writeDiffTestFile(t, repo, "tracked.go", "main secret\n")
			commitDiffTestRepo(t, repo, "main change")
			gitDiffTest(t, repo, "checkout", "side")
			writeDiffTestFile(t, repo, "tracked.go", "side secret\n")
			commitDiffTestRepo(t, repo, "side change")
			gitDiffTest(t, repo, "checkout", "main")
			gitDiffTestWantError(t, repo, "merge", "side")
		}},
		{name: "submodule", dirty: func(t *testing.T, repo string) {
			submodule := newDiffTestRepo(t)
			writeDiffTestFile(t, submodule, "nested.go", "clean\n")
			commitDiffTestRepo(t, submodule, "submodule base")
			gitDiffTest(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "nested")
			commitDiffTestRepo(t, repo, "add submodule")
			writeDiffTestFile(t, filepath.Join(repo, "nested"), "nested.go", "dirty secret\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, "tracked.go", "clean\n")
			commitDiffTestRepo(t, repo, "base")
			test.dirty(t, repo)

			_, err := resolveDiff(context.Background(), repo, "main")
			if err == nil || !strings.Contains(err.Error(), "clean") {
				t.Fatalf("resolveDiff() error = %v, want clean-worktree diagnostic", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), ".go") {
				t.Fatalf("resolveDiff() exposed raw status: %v", err)
			}
		})
	}
}

func TestResolveDiffValidatesInputs(t *testing.T) {
	if _, err := resolveDiff(nil, t.TempDir(), "main"); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("resolveDiff(nil) error = %v", err)
	}
	if _, err := resolveDiff(context.Background(), "", "main"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("resolveDiff(empty root) error = %v", err)
	}
}

func TestMergeLineRangesSortsAndCombinesAdjacentAndOverlapping(t *testing.T) {
	got := mergeLineRanges([]finding.LineRange{
		{Start: 8, End: 10},
		{Start: 1, End: 2},
		{Start: 6, End: 8},
		{Start: 3, End: 5},
		{Start: 12, End: 12},
	})
	want := []finding.LineRange{{Start: 1, End: 10}, {Start: 12, End: 12}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeLineRanges() = %#v, want %#v", got, want)
	}
}

func TestDeletionAnchorUsesFinalCurrentLineAndRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	writeDiffTestFile(t, root, "file.go", "one\ntwo\nthree")
	got, err := deletionAnchor(root, "file.go", 9)
	if err != nil || got != 3 {
		t.Fatalf("deletionAnchor() = %d, %v, want 3", got, err)
	}
	if got, err := deletionAnchor(root, "deleted.go", 1); err != nil || got != 0 {
		t.Fatalf("deletionAnchor(deleted file) = %d, %v, want no anchor", got, err)
	}

	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := deletionAnchor(root, "escape.go", 1); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("deletionAnchor(symlink escape) error = %v", err)
	}
}

func TestValidateDiffPathRejectsPlatformIndependentEscapes(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escape.go", "dir/../../escape.go", "/absolute.go", "C:/absolute.go"} {
		if err := validateDiffPath(root, path); err == nil {
			t.Errorf("validateDiffPath(%q) error = nil, want unsafe-path error", path)
		}
	}
}

func newDiffTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitDiffTest(t, repo, "init", "-b", "main")
	gitDiffTest(t, repo, "config", "user.name", "Togi Tests")
	gitDiffTest(t, repo, "config", "user.email", "togi@example.invalid")
	return repo
}

func setDiffTestOriginHead(t *testing.T, repo, branch, commit string) {
	t.Helper()
	gitDiffTest(t, repo, "update-ref", "refs/remotes/origin/"+branch, commit)
	gitDiffTest(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

func writeDiffTestFile(t *testing.T, repo, name, contents string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func removeDiffTestFile(t *testing.T, repo, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(repo, name)); err != nil {
		t.Fatal(err)
	}
}

func commitDiffTestRepo(t *testing.T, repo, message string) string {
	t.Helper()
	gitDiffTest(t, repo, "add", "-A", "--", ".")
	gitDiffTest(t, repo, "commit", "-m", message)
	return gitDiffTest(t, repo, "rev-parse", "HEAD")
}

func gitDiffTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitDiffTestWantError(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git %v succeeded, want failure: %s", args, output)
	}
}

func assertFullObjectID(t *testing.T, value string) {
	t.Helper()
	if len(value) != 40 && len(value) != 64 {
		t.Fatalf("object ID %q has length %d", value, len(value))
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			t.Fatalf("object ID %q is not lowercase hexadecimal", value)
		}
	}
}
