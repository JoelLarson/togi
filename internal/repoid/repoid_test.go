package repoid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
)

func TestResolveUsesRootCommit(t *testing.T) {
	repo := newCommittedRepo(t)
	want := gitTestOutput(t, repo, "rev-list", "--max-parents=0", "HEAD")
	nested := filepath.Join(repo, "nested", "directory")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(context.Background(), nested)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key() != want {
		t.Fatalf("Key = %q, want %q", got.Key(), want)
	}
	assertKeyDirectory(t, got)
	if got.Root() != repo {
		t.Fatalf("Root = %q, want %q", got.Root(), repo)
	}
}

func TestNewCanonicalizesSymlinkRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	id, err := New(strings.Repeat("a", 40), link)
	if err != nil {
		t.Fatal(err)
	}
	if id.Root() != real {
		t.Fatalf("Root = %q, want canonical %q", id.Root(), real)
	}
	if id.Key() != id.Dir() || id.IsZero() {
		t.Fatalf("ID = %#v, want usable matching key and directory", id)
	}
}

func TestIDZeroValueIsUnusable(t *testing.T) {
	var id ID
	if !id.IsZero() || id.Key() != "" || id.Root() != "" || id.Dir() != "" {
		t.Fatalf("zero ID = %#v", id)
	}
}

func TestNewRejectsNonDirectoryRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "repo-file")
	writeFile(t, file, "not a repository directory")
	if _, err := New(strings.Repeat("a", 40), file); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("New error = %v, want directory error", err)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(strings.Repeat("a", 40), ""); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("New error = %v, want root error", err)
	}
}

func TestValidKeyAcceptsOnlyFullLowercaseHex(t *testing.T) {
	for _, key := range []string{strings.Repeat("a", 40), strings.Repeat("0", 64)} {
		if !ValidKey(key) {
			t.Fatalf("ValidKey(%q) = false", key)
		}
	}
	for _, key := range []string{"", strings.Repeat("a", 39), strings.Repeat("a", 65), strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		if ValidKey(key) {
			t.Fatalf("ValidKey(%q) = true", key)
		}
	}
}

func TestResolveHashesNormalizedRemoteWithoutRoot(t *testing.T) {
	repo := newEmptyRepo(t, "remote")
	gitRun(t, repo, "remote", "add", "origin", "git@github.com:JoelLarson/togi.git")

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex("github.com/JoelLarson/togi")
	if got.Key() != want {
		t.Fatalf("Key = %q, want %q", got.Key(), want)
	}
	assertKeyDirectory(t, got)
}

func TestResolveHashesCanonicalPathWithoutCommitOrRemote(t *testing.T) {
	repo := newEmptyRepo(t, "path")
	link := filepath.Join(t.TempDir(), "repository-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(real); got.Key() != want {
		t.Fatalf("Key = %q, want %q", got.Key(), want)
	}
	assertKeyDirectory(t, got)
}

func TestResolveHashesOriginRemoteForShallowClone(t *testing.T) {
	source := newCommittedRepoNamed(t, "source.git")
	writeFile(t, filepath.Join(source, "second.txt"), "second\n")
	gitRun(t, source, "add", "second.txt")
	gitRun(t, source, "commit", "-m", "second commit")

	clone := filepath.Join(t.TempDir(), "shallow")
	origin := "file://" + source
	gitRun(t, t.TempDir(), "clone", "--depth", "1", origin, clone)
	if got, want := gitTestOutput(t, clone, "rev-parse", "--is-shallow-repository"), "true"; got != want {
		t.Fatalf("shallow state = %q, want %q", got, want)
	}
	shallowRoot := gitTestOutput(t, clone, "rev-list", "--max-parents=0", "HEAD")

	got, err := Resolve(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(strings.TrimSuffix(origin, ".git")); got.Key() != want {
		t.Fatalf("Key = %q, want normalized origin hash %q", got.Key(), want)
	}
	if got.Key() == shallowRoot {
		t.Fatalf("Key = shallow boundary %q", got.Key())
	}
	assertKeyDirectory(t, got)
}

func TestResolvePropagatesCorruptObjectError(t *testing.T) {
	repo := newCommittedRepo(t)
	gitRun(t, repo, "remote", "add", "origin", "https://github.com/JoelLarson/togi.git")
	root := gitTestOutput(t, repo, "rev-list", "--max-parents=0", "HEAD")
	object := filepath.Join(repo, ".git", "objects", root[:2], root[2:])
	if err := os.Chmod(object, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve(context.Background(), repo)
	if err == nil {
		t.Fatal("Resolve succeeded for a corrupt committed repository")
	}
}

func TestResolvePropagatesCanceledContext(t *testing.T) {
	repo := newCommittedRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Resolve(ctx, repo)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want context cancellation", err)
	}
}

func TestResolveIgnoresRepoSelectingGitEnvironment(t *testing.T) {
	repoA := newCommittedRepo(t)
	writeFile(t, filepath.Join(repoA, "second.txt"), "second\n")
	gitRun(t, repoA, "add", "second.txt")
	gitRun(t, repoA, "commit", "-m", "second commit")
	repoB := newEmptyRepo(t, "repository-b")
	writeFile(t, filepath.Join(repoB, "initial.txt"), "repository B\n")
	gitRun(t, repoB, "add", "initial.txt")
	gitRun(t, repoB, "commit", "-m", "initial commit")
	want := gitTestOutput(t, repoB, "rev-list", "--max-parents=0", "HEAD")

	t.Setenv("GIT_DIR", filepath.Join(repoA, ".git"))
	got, err := Resolve(context.Background(), repoB)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key() != want {
		t.Fatalf("Key = %q, want repository B root %q", got.Key(), want)
	}
}

func TestResolveIgnoresGitConfigInjection(t *testing.T) {
	repo := newEmptyRepo(t, "config-injection")
	gitRun(t, repo, "remote", "add", "origin", "https://github.com/JoelLarson/togi.git")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/injected/repository.git")

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex("github.com/JoelLarson/togi"); got.Key() != want {
		t.Fatalf("Key = %q, want local origin key %q", got.Key(), want)
	}
}

func TestResolveUsesOrdinaryGlobalURLRewrite(t *testing.T) {
	repo := newEmptyRepo(t, "global-url-rewrite")
	gitRun(t, repo, "remote", "add", "origin", "gh:JoelLarson/togi.git")
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".gitconfig"), "[url \"https://github.com/\"]\n\tinsteadOf = gh:\n")
	t.Setenv("HOME", home)

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex("github.com/JoelLarson/togi"); got.Key() != want {
		t.Fatalf("Key = %q, want rewritten origin key %q", got.Key(), want)
	}
}

func TestResolveIgnoresGitShallowOverride(t *testing.T) {
	repo := newCommittedRepo(t)
	want := gitTestOutput(t, repo, "rev-list", "--max-parents=0", "HEAD")
	shallowFile := filepath.Join(t.TempDir(), "shallow")
	if err := os.WriteFile(shallowFile, []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SHALLOW_FILE", shallowFile)

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key() != want {
		t.Fatalf("Key = %q, want repository root %q", got.Key(), want)
	}
}

func TestResolveIgnoresGitTraceDiagnostics(t *testing.T) {
	repo := newCommittedRepo(t)
	want := gitTestOutput(t, repo, "rev-list", "--max-parents=0", "HEAD")
	t.Setenv("GIT_TRACE", "1")

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root() != repo {
		t.Fatalf("Root = %q, want %q", got.Root(), repo)
	}
	if got.Key() != want {
		t.Fatalf("Key = %q, want %q", got.Key(), want)
	}
	assertKeyDirectory(t, got)
}

func TestGitOutputSeparatesStdoutAndStderr(t *testing.T) {
	repo := newCommittedRepo(t)
	_, err := gitOutput(context.Background(), repo, "rev-parse", "--show-toplevel", "--verify", "missing-ref")
	if err == nil {
		t.Fatal("gitOutput succeeded for a missing ref")
	}
	if strings.Contains(err.Error(), repo) {
		t.Fatalf("error includes stdout repository path: %q", err)
	}
}

func TestResolveHashesMultipleRootCommits(t *testing.T) {
	repo := newCommittedRepo(t)
	primary := gitTestOutput(t, repo, "branch", "--show-current")
	gitRun(t, repo, "checkout", "--orphan", "other")
	gitRun(t, repo, "rm", "-rf", ".")
	writeFile(t, filepath.Join(repo, "other.txt"), "other\n")
	gitRun(t, repo, "add", "other.txt")
	gitRun(t, repo, "commit", "-m", "other root")
	gitRun(t, repo, "checkout", primary)
	gitRun(t, repo, "merge", "--allow-unrelated-histories", "other", "-m", "merge roots")

	roots := strings.Fields(gitTestOutput(t, repo, "rev-list", "--max-parents=0", "HEAD"))
	sort.Strings(roots)
	want := sha256Hex(strings.Join(roots, "\n"))

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key() != want {
		t.Fatalf("Key = %q, want %q", got.Key(), want)
	}
}

func TestResolveSharesStateDirectoryAcrossLinkedWorktrees(t *testing.T) {
	repo := newCommittedRepoNamed(t, "main checkout")
	worktree := filepath.Join(t.TempDir(), "linked-worktree")
	gitRun(t, repo, "worktree", "add", "--detach", worktree, "HEAD")

	mainID, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	worktreeID, err := Resolve(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}

	if worktreeID.Key() != mainID.Key() {
		t.Fatalf("worktree Key = %q, want main Key %q", worktreeID.Key(), mainID.Key())
	}
	if worktreeID.Dir() != mainID.Dir() {
		t.Fatalf("worktree Directory = %q, want main Directory %q", worktreeID.Dir(), mainID.Dir())
	}
}

func TestResolveDirectoryUsesFullStableKey(t *testing.T) {
	repo := newCommittedRepoNamed(t, "togi repo!")

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dir() != got.Key() {
		t.Fatalf("Directory = %q, want full Key %q", got.Dir(), got.Key())
	}
	if _, err := hex.DecodeString(got.Dir()); err != nil {
		t.Fatalf("Directory = %q, want a hexadecimal component: %v", got.Dir(), err)
	}
	if filepath.Base(got.Dir()) != got.Dir() {
		t.Fatalf("Directory = %q, want one safe path component", got.Dir())
	}
}

func TestResolveNormalizesEquivalentRemoteForms(t *testing.T) {
	forms := []struct {
		name       string
		remote     string
		normalized string
	}{
		{name: "ssh", remote: "git@GitHub.com:JoelLarson/togi.git", normalized: "github.com/JoelLarson/togi"},
		{name: "https", remote: "https://user:password@github.com/JoelLarson/togi.git/", normalized: "github.com/JoelLarson/togi"},
		{name: "scp without username", remote: "github.com:JoelLarson/togi.git", normalized: "github.com/JoelLarson/togi"},
		{name: "ssh default port", remote: "ssh://github.com:22/JoelLarson/togi.git", normalized: "github.com/JoelLarson/togi"},
		{name: "http default port", remote: "http://github.com:80/JoelLarson/togi.git", normalized: "github.com/JoelLarson/togi"},
		{name: "https default port", remote: "https://github.com:443/JoelLarson/togi.git", normalized: "github.com/JoelLarson/togi"},
		{name: "ssh custom port 2222", remote: "ssh://github.com:2222/JoelLarson/togi.git", normalized: "github.com:2222/JoelLarson/togi"},
		{name: "ssh custom port 2223", remote: "ssh://github.com:2223/JoelLarson/togi.git", normalized: "github.com:2223/JoelLarson/togi"},
	}

	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			repo := newEmptyRepo(t, form.name)
			gitRun(t, repo, "remote", "add", "origin", form.remote)

			got, err := Resolve(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if want := sha256Hex(form.normalized); got.Key() != want {
				t.Fatalf("Key = %q, want %q", got.Key(), want)
			}
		})
	}
}

func TestGitFixturesIgnoreInheritedCommitSigning(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "commit.gpgSign")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")

	_ = newCommittedRepo(t)
}

func newCommittedRepo(t *testing.T) string {
	t.Helper()
	repo := newEmptyRepo(t, "committed")
	writeFile(t, filepath.Join(repo, "initial.txt"), "initial\n")
	gitRun(t, repo, "add", "initial.txt")
	gitRun(t, repo, "commit", "-m", "initial commit")
	return repo
}

func newCommittedRepoNamed(t *testing.T, name string) string {
	t.Helper()
	repo := newEmptyRepo(t, name)
	writeFile(t, filepath.Join(repo, "initial.txt"), "initial\n")
	gitRun(t, repo, "add", "initial.txt")
	gitRun(t, repo, "commit", "-m", "initial commit")
	return repo
}

func newEmptyRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init")
	gitRun(t, repo, "config", "user.name", "Togi Test")
	gitRun(t, repo, "config", "user.email", "togi@example.test")
	return repo
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	gitcmdtest.Git(t, repo, args...)
}

func gitTestOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	return gitcmdtest.Git(t, repo, args...)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func assertKeyDirectory(t *testing.T, id ID) {
	t.Helper()
	if id.Dir() != id.Key() {
		t.Fatalf("Directory = %q, want full Key %q", id.Dir(), id.Key())
	}
	if _, err := hex.DecodeString(id.Dir()); err != nil {
		t.Fatalf("Directory = %q, want a hexadecimal component: %v", id.Dir(), err)
	}
}
