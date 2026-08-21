package repoid

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	if got.Key != want {
		t.Fatalf("Key = %q, want repository B root %q", got.Key, want)
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
	if want := sha256Hex("github.com/JoelLarson/togi"); got.Key != want {
		t.Fatalf("Key = %q, want local origin key %q", got.Key, want)
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
	if want := sha256Hex("github.com/JoelLarson/togi"); got.Key != want {
		t.Fatalf("Key = %q, want rewritten origin key %q", got.Key, want)
	}
}

func TestGitEnvironmentFiltersMixedCaseGitVariables(t *testing.T) {
	t.Setenv("git_dir", "/tmp/other-repository")
	t.Setenv("Git_Config_Count", "1")

	for _, entry := range gitEnvironment() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "GIT_DIR") || strings.EqualFold(name, "GIT_CONFIG_COUNT") {
			t.Fatalf("Git environment includes %q", entry)
		}
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
	if got.Key != want {
		t.Fatalf("Key = %q, want repository root %q", got.Key, want)
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
	if got.Root != repo {
		t.Fatalf("Root = %q, want %q", got.Root, repo)
	}
	if got.Key != want {
		t.Fatalf("Key = %q, want %q", got.Key, want)
	}
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
			if want := sha256Hex(form.normalized); got.Key != want {
				t.Fatalf("Key = %q, want %q", got.Key, want)
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

func TestSanitizeBoundsDirectoryBasename(t *testing.T) {
	name := sanitize(strings.Repeat("a", 300))
	if got, max := len(name)+1+12, 255; got > max {
		t.Fatalf("directory component length = %d, want at most %d", got, max)
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
	command := append([]string{"-c", "commit.gpgSign=false", "-c", "core.hooksPath=" + os.DevNull, "-C", repo}, args...)
	cmd := exec.Command("git", command...)
	cmd.Env = fixtureGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

func fixtureGitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_NO_REPLACE_OBJECTS=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
