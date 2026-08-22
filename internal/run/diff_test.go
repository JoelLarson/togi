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
	"time"

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

func TestResolveDiffIgnoresHostileGitEnvironmentWithoutRefreshingIndex(t *testing.T) {
	target := newDiffTestRepo(t)
	writeDiffTestFile(t, target, "target.go", "base\n")
	base := commitDiffTestRepo(t, target, "target base")
	writeDiffTestFile(t, target, "target.go", "feature\n")
	wantHead := commitDiffTestRepo(t, target, "target feature")

	foreign := newDiffTestRepo(t)
	writeDiffTestFile(t, foreign, "foreign.go", "foreign\n")
	commitDiffTestRepo(t, foreign, "foreign")

	tracked := filepath.Join(target, "target.go")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(tracked, future, future); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(target, ".git", "index")
	beforeIndex, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	beforeFile, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(foreign, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(foreign, ".git", "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(foreign, ".git", "objects"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.interHunkContext")
	t.Setenv("GIT_CONFIG_VALUE_0", "999")

	got, err := resolveDiff(context.Background(), target, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	if got.Head != wantHead || got.BaseCommit != base {
		t.Fatalf("resolveDiff() commits = %#v, want target HEAD %s and base %s", got, wantHead, base)
	}
	afterIndex, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	afterFile, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterIndex, beforeIndex) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("read-only diff resolution refreshed the target index")
	}
	if !reflect.DeepEqual(afterFile, beforeFile) {
		t.Fatal("read-only diff resolution changed the target worktree")
	}
}

func TestResolveDiffIgnoresInjectedAttributesConfig(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	head := commitDiffTestRepo(t, repo, "feature")

	attributes := filepath.Join(t.TempDir(), "attributes")
	if err := os.WriteFile(attributes, []byte("*.go binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.attributesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", attributes)

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 1, End: 1}}}
	if got.BaseCommit != base || got.Head != head || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want base %s, head %s, and text line scope %#v", got, base, head, want)
	}
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

func TestResolveDiffUsesMergeBaseAcrossDivergedHistory(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "one\ntwo\nthree\n")
	common := commitDiffTestRepo(t, repo, "common")
	gitDiffTest(t, repo, "checkout", "-b", "comparison")
	writeDiffTestFile(t, repo, "file.go", "BASE\ntwo\nthree\n")
	baseTip := commitDiffTestRepo(t, repo, "base-only change")
	gitDiffTest(t, repo, "checkout", "-b", "feature", common)
	writeDiffTestFile(t, repo, "file.go", "one\ntwo\nFEATURE\n")
	head := commitDiffTestRepo(t, repo, "feature-only change")

	got, err := resolveDiff(context.Background(), repo, "comparison")
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 3, End: 3}}}
	if got.BaseCommit != baseTip || got.MergeBase != common || got.Head != head || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want base tip %s, merge base %s, head %s, lines %#v", got, baseTip, common, head, want)
	}
}

func TestResolveDiffSupportsSHA256ObjectIDs(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "--object-format=sha256")
	cmd.Dir = repo
	cmd.Env = diffTestGitEnvironment()
	if output, err := cmd.CombinedOutput(); err != nil {
		message := string(output)
		if strings.Contains(message, "unknown option") || strings.Contains(message, "unknown value") || strings.Contains(message, "unsupported") || strings.Contains(message, "invalid object format") {
			t.Skipf("Git SHA-256 repositories unsupported: %s", output)
		}
		t.Fatalf("initialize SHA-256 repository: %v: %s", err, output)
	}
	gitDiffTest(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	gitDiffTest(t, repo, "config", "user.name", "Togi Tests")
	gitDiffTest(t, repo, "config", "user.email", "togi@example.invalid")
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	head := commitDiffTestRepo(t, repo, "feature")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	for name, objectID := range map[string]string{"base": got.BaseCommit, "merge base": got.MergeBase, "head": got.Head} {
		if len(objectID) != 64 {
			t.Fatalf("%s object ID = %q, want 64 hexadecimal characters", name, objectID)
		}
		assertFullObjectID(t, objectID)
	}
	if got.BaseCommit != base || got.MergeBase != base || got.Head != head {
		t.Fatalf("resolveDiff() commits = %#v, want SHA-256 base %s and head %s", got, base, head)
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

func TestResolveDiffRejectsAnnotatedTagToNonCommit(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	commitDiffTestRepo(t, repo, "base")
	blob := gitDiffTest(t, repo, "hash-object", "file.go")
	gitDiffTest(t, repo, "tag", "-a", "blob-tag", "-m", "not a commit", blob)

	_, err := resolveDiff(context.Background(), repo, "blob-tag")
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("resolveDiff() error = %v, want non-commit base rejection", err)
	}
}

func TestResolveDiffRejectsUnbornHead(t *testing.T) {
	repo := newDiffTestRepo(t)
	_, err := resolveDiff(context.Background(), repo, "main")
	if err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("resolveDiff() error = %v, want invalid HEAD diagnostic", err)
	}
}

func TestResolveDiffRejectsInvalidHeadWithoutExposingContents(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	commitDiffTestRepo(t, repo, "base")
	const invalid = "invalid-head-secret"
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte(invalid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resolveDiff(context.Background(), repo, "main")
	if err == nil || strings.Contains(err.Error(), invalid) {
		t.Fatalf("resolveDiff() error = %v, want redacted invalid-HEAD rejection", err)
	}
}

func TestDiffGitOutputBoundsAndRedactsOutput(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	commit := commitDiffTestRepo(t, repo, "base")

	if _, err := diffGitOutput(context.Background(), repo, 32, "rev-parse", "HEAD"); err == nil || !strings.Contains(err.Error(), "exceeded") || strings.Contains(err.Error(), commit) {
		t.Fatalf("bounded Git output error = %v, want redacted size diagnostic", err)
	}
	const secret = "missing-secret-ref"
	if _, err := diffGitOutput(context.Background(), repo, 1024, "show", secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("failed Git command error = %v, want redacted stderr and arguments", err)
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

func TestResolveDiffAnchorsFirstLineDeletionAtLineOne(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "delete\nkeep two\nkeep three\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "keep two\nkeep three\n")
	commitDiffTestRepo(t, repo, "delete first line")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 1, End: 1}}}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() lines = %#v, want %#v", got.Lines, want)
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
		"new name.go":  {},
		"tab\tname.go": {{Start: 1, End: 1}},
	}
	if got.ChangedFiles != 2 || got.ChangedLines != 1 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want %#v", got, want)
	}
}

func TestResolveDiffRenameWithEditKeepsOnlyEditedDestinationLine(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "old.go", "one\ntwo\nthree\nfour\nfive\n")
	base := commitDiffTestRepo(t, repo, "base")
	if err := os.Rename(filepath.Join(repo, "old.go"), filepath.Join(repo, "new.go")); err != nil {
		t.Fatal(err)
	}
	writeDiffTestFile(t, repo, "new.go", "one\ntwo\nTHREE\nfour\nfive\n")
	commitDiffTestRepo(t, repo, "rename with edit")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"new.go": {{Start: 3, End: 3}}}
	if got.ChangedFiles != 1 || got.ChangedLines != 1 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want only destination line 3", got)
	}
}

func TestResolveDiffUsesLiteralPathspecs(t *testing.T) {
	repo := newDiffTestRepo(t)
	const magicName = ":(glob)*.go"
	writeDiffTestFile(t, repo, magicName, "one\ntwo\nthree\n")
	writeDiffTestFile(t, repo, "victim.go", "one\ntwo\nthree\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, magicName, "one\ntwo\nTHREE\n")
	writeDiffTestFile(t, repo, "victim.go", "ONE\ntwo\nthree\n")
	commitDiffTestRepo(t, repo, "feature")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{
		magicName:   {{Start: 3, End: 3}},
		"victim.go": {{Start: 1, End: 1}},
	}
	if got.ChangedLines != 2 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want literal-path scopes %#v", got, want)
	}
}

func TestResolveDiffOverridesInterHunkContext(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "one\ntwo\nthree\nfour\nfive\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "one\nTWO\nthree\nFOUR\nfive\n")
	commitDiffTestRepo(t, repo, "feature")
	gitDiffTest(t, repo, "config", "diff.interHunkContext", "99")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 2, End: 2}, {Start: 4, End: 4}}}
	if got.ChangedLines != 2 || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() = %#v, want separate line sites %#v", got, want)
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
			gitDiffTest(t, repo, "config", "submodule.nested.ignore", "all")
			writeDiffTestFile(t, filepath.Join(repo, "nested"), "nested.go", "dirty secret\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, "tracked.go", "clean\n")
			commitDiffTestRepo(t, repo, "base")
			test.dirty(t, repo)

			_, err := resolveDiff(context.Background(), repo, "main")
			want := "clean"
			if test.name == "submodule" {
				want = "submodules are unsupported"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("resolveDiff() error = %v, want %q diagnostic", err, want)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), ".go") {
				t.Fatalf("resolveDiff() exposed raw status: %v", err)
			}
		})
	}
}

func TestResolveDiffRejectsNestedSubmoduleAtTopLevel(t *testing.T) {
	nestedSource := newDiffTestRepo(t)
	writeDiffTestFile(t, nestedSource, "deep.go", "clean\n")
	commitDiffTestRepo(t, nestedSource, "nested base")
	subSource := newDiffTestRepo(t)
	gitDiffTest(t, subSource, "-c", "protocol.file.allow=always", "submodule", "add", nestedSource, "deep")
	commitDiffTestRepo(t, subSource, "add nested submodule")
	parent := newDiffTestRepo(t)
	gitDiffTest(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", subSource, "nested")
	commitDiffTestRepo(t, parent, "add submodule")

	_, err := resolveDiff(context.Background(), parent, "main")
	if err == nil || !strings.Contains(err.Error(), "submodules are unsupported") {
		t.Fatalf("resolveDiff() error = %v, want unsupported-submodule diagnostic", err)
	}
}

func TestResolveDiffRejectsUninitializedSubmodule(t *testing.T) {
	source := newDiffTestRepo(t)
	writeDiffTestFile(t, source, "nested.go", "clean\n")
	commitDiffTestRepo(t, source, "submodule base")
	parent := newDiffTestRepo(t)
	gitDiffTest(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "nested")
	commitDiffTestRepo(t, parent, "add submodule")
	gitDiffTest(t, parent, "submodule", "deinit", "-f", "--", "nested")

	_, err := resolveDiff(context.Background(), parent, "main")
	if err == nil || !strings.Contains(err.Error(), "submodules are unsupported") {
		t.Fatalf("resolveDiff() error = %v, want unsupported-submodule diagnostic", err)
	}
}

func TestResolveDiffRejectsCleanInitializedSubmodule(t *testing.T) {
	source := newDiffTestRepo(t)
	writeDiffTestFile(t, source, "nested.go", "clean\n")
	commitDiffTestRepo(t, source, "submodule base")
	parent := newDiffTestRepo(t)
	gitDiffTest(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "nested")
	commitDiffTestRepo(t, parent, "add submodule")

	_, err := resolveDiff(context.Background(), parent, "main")
	if err == nil || !strings.Contains(err.Error(), "submodules are unsupported") {
		t.Fatalf("resolveDiff() error = %v, want unsupported-submodule diagnostic", err)
	}
}

func TestResolveDiffRejectsHiddenIndexChanges(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, "hidden.go", "clean\n")
			commitDiffTestRepo(t, repo, "base")
			gitDiffTest(t, repo, "update-index", flag, "--", "hidden.go")
			writeDiffTestFile(t, repo, "hidden.go", "hidden change\n")

			_, err := resolveDiff(context.Background(), repo, "main")
			if err == nil || !strings.Contains(err.Error(), "index") || strings.Contains(err.Error(), "hidden.go") {
				t.Fatalf("resolveDiff() error = %v, want redacted index-flag rejection", err)
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

func TestChangedPathCountRequiresCanonicalRenameScores(t *testing.T) {
	for _, status := range []string{"R000", "R001", "R100", "C050"} {
		if got, err := changedPathCount(status); err != nil || got != 2 {
			t.Errorf("changedPathCount(%q) = %d, %v, want 2, nil", status, got, err)
		}
	}
	for _, status := range []string{"R", "R0", "R00", "R0000", "R101", "R-01", "Cabc"} {
		if _, err := changedPathCount(status); err == nil {
			t.Errorf("changedPathCount(%q) error = nil, want malformed status", status)
		}
	}
}

func TestDeletionAnchorUsesCapturedHeadBlob(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "one\ntwo\nthree")
	writeDiffTestFile(t, repo, "empty.go", "")
	head := commitDiffTestRepo(t, repo, "captured")
	writeDiffTestFile(t, repo, "file.go", "live worktree\n")
	cache := make(map[string]int)

	got, err := deletionAnchor(context.Background(), repo, head, "file.go", 9, cache)
	if err != nil || got != 3 {
		t.Fatalf("deletionAnchor() = %d, %v, want 3", got, err)
	}
	if got, err := deletionAnchor(context.Background(), repo, head, "file.go", 0, cache); err != nil || got != 1 {
		t.Fatalf("deletionAnchor(first line) = %d, %v, want 1", got, err)
	}
	if got, err := deletionAnchor(context.Background(), repo, head, "empty.go", 1, cache); err != nil || got != 0 {
		t.Fatalf("deletionAnchor(empty blob) = %d, %v, want 0", got, err)
	}
	if len(cache) != 2 {
		t.Fatalf("cached blob counts = %#v, want one entry per destination path", cache)
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
	gitDiffTest(t, repo, "init")
	gitDiffTest(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
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
	cmd.Env = diffTestGitEnvironment()
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
	cmd.Env = diffTestGitEnvironment()
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git %v succeeded, want failure: %s", args, output)
	}
}

func diffTestGitEnvironment() []string {
	return append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
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
