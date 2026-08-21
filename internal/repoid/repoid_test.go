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
	if want := sha256Hex(strings.TrimSuffix(origin, ".git")); got.Key != want {
		t.Fatalf("Key = %q, want normalized origin hash %q", got.Key, want)
	}
	if got.Key == shallowRoot {
		t.Fatalf("Key = shallow boundary %q", got.Key)
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
	forms := []struct {
		name   string
		remote string
	}{
		{name: "ssh", remote: "git@GitHub.com:JoelLarson/togi.git"},
		{name: "https", remote: "https://user:password@github.com/JoelLarson/togi.git/"},
		{name: "scp without username", remote: "github.com:JoelLarson/togi.git"},
	}

	want := sha256Hex("github.com/JoelLarson/togi")
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			repo := newEmptyRepo(t, form.name)
			gitRun(t, repo, "remote", "add", "origin", form.remote)

			got, err := Resolve(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if got.Key != want {
				t.Fatalf("Key = %q, want %q", got.Key, want)
			}
		})
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
