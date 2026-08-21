package repoid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
	if got.Key != want {
		t.Fatalf("Key = %q, want %q", got.Key, want)
	}
	if got.Root != repo {
		t.Fatalf("Root = %q, want %q", got.Root, repo)
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
	if got.Key != want {
		t.Fatalf("Key = %q, want %q", got.Key, want)
	}
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
	if want := sha256Hex(real); got.Key != want {
		t.Fatalf("Key = %q, want %q", got.Key, want)
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
	if got.Key != want {
		t.Fatalf("Key = %q, want %q", got.Key, want)
	}
}

func TestResolveDirectoryUsesSanitizedBasenameAndShortKey(t *testing.T) {
	repo := newCommittedRepoNamed(t, "togi repo!")

	got, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := "togi-repo-" + got.Key[:12]
	if got.Directory != want {
		t.Fatalf("Directory = %q, want %q", got.Directory, want)
	}
}

func TestResolveNormalizesEquivalentRemoteForms(t *testing.T) {
	sshRepo := newEmptyRepo(t, "ssh")
	gitRun(t, sshRepo, "remote", "add", "origin", "git@GitHub.com:JoelLarson/togi.git")

	httpsRepo := newEmptyRepo(t, "https")
	gitRun(t, httpsRepo, "remote", "add", "origin", "https://user:password@github.com/JoelLarson/togi.git/")

	sshID, err := Resolve(context.Background(), sshRepo)
	if err != nil {
		t.Fatal(err)
	}
	httpsID, err := Resolve(context.Background(), httpsRepo)
	if err != nil {
		t.Fatal(err)
	}
	if sshID.Key != httpsID.Key {
		t.Fatalf("equivalent remote keys differ: SSH %q, HTTPS %q", sshID.Key, httpsID.Key)
	}
	if want := sha256Hex("github.com/JoelLarson/togi"); sshID.Key != want {
		t.Fatalf("Key = %q, want %q", sshID.Key, want)
	}
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
	if output, err := gitTestOutputErr(repo, args...); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitTestOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	output, err := gitTestOutputErr(repo, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(output)
}

func gitTestOutputErr(repo string, args ...string) (string, error) {
	command := append([]string{"-C", repo}, args...)
	output, err := exec.Command("git", command...).CombinedOutput()
	return string(output), err
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
