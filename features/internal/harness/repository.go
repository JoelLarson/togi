package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joellarson/togi/internal/gitcmd"
)

// Repository is a hermetic Git repository fixture rooted at Root.
type Repository struct {
	Root string
}

// NewRepository initializes an empty main-branch fixture repository at root.
func NewRepository(root string) (*Repository, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create repository root: %w", err)
	}
	repository := &Repository{Root: absRoot}
	if _, err := repository.Git("init", "--initial-branch=main"); err != nil {
		return nil, fmt.Errorf("initialize repository: %w", err)
	}
	if _, err := repository.Git("config", "user.name", "Togi Acceptance"); err != nil {
		return nil, fmt.Errorf("configure repository author name: %w", err)
	}
	if _, err := repository.Git("config", "user.email", "acceptance@togi.invalid"); err != nil {
		return nil, fmt.Errorf("configure repository author email: %w", err)
	}
	return repository, nil
}

// Write writes body to a slash-relative fixture path.
func (r *Repository) Write(name, body string) error {
	return r.WriteBytes(name, []byte(body))
}

// WriteBytes writes body to a slash-relative fixture path.
func (r *Repository) WriteBytes(name string, body []byte) error {
	clean, err := fixturePath(name)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()
	if parent := path.Dir(clean); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create parent directory for %q: %w", name, err)
		}
	}
	if err := root.WriteFile(clean, body, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", name, err)
	}
	return nil
}

// Remove removes a slash-relative fixture path.
func (r *Repository) Remove(name string) error {
	clean, err := fixturePath(name)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()
	if err := root.Remove(clean); err != nil {
		return fmt.Errorf("remove %q: %w", name, err)
	}
	return nil
}

// Commit stages all fixture changes and returns the full resulting HEAD ID.
func (r *Repository) Commit(message string) (string, error) {
	if _, err := r.Git("add", "-A"); err != nil {
		return "", fmt.Errorf("stage fixture changes: %w", err)
	}
	if _, err := r.Git("commit", "-m", message); err != nil {
		return "", fmt.Errorf("commit fixture changes: %w", err)
	}
	head, err := r.Git("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve committed HEAD: %w", err)
	}
	return head, nil
}

// Branch creates name at the current HEAD.
func (r *Repository) Branch(name string) error {
	if _, err := r.Git("branch", name); err != nil {
		return fmt.Errorf("create branch %q: %w", name, err)
	}
	return nil
}

// Checkout checks out name.
func (r *Repository) Checkout(name string) error {
	if _, err := r.Git("checkout", name); err != nil {
		return fmt.Errorf("checkout %q: %w", name, err)
	}
	return nil
}

// SetOriginHEAD creates a remote-tracking branch and symbolic origin HEAD.
func (r *Repository) SetOriginHEAD(branch, commit string) error {
	ref := "refs/remotes/origin/" + branch
	if _, err := r.Git("update-ref", ref, commit); err != nil {
		return fmt.Errorf("set %s: %w", ref, err)
	}
	if _, err := r.Git("symbolic-ref", "refs/remotes/origin/HEAD", ref); err != nil {
		return fmt.Errorf("set origin HEAD: %w", err)
	}
	return nil
}

// LinkedWorktree creates a linked worktree with branch checked out there.
func (r *Repository) LinkedWorktree(worktreePath, branch string) (*Repository, error) {
	absPath, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolve linked worktree path: %w", err)
	}
	if _, err := r.Git("worktree", "add", "-b", branch, absPath); err != nil {
		return nil, fmt.Errorf("create linked worktree: %w", err)
	}
	return &Repository{Root: absPath}, nil
}

// AddSubmodule adds a local fixture repository at a slash-relative path.
func (r *Repository) AddSubmodule(name, source string) error {
	clean, err := fixturePath(name)
	if err != nil {
		return err
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve submodule source: %w", err)
	}
	if source != absSource {
		return fmt.Errorf("submodule source must be an absolute local path: %q", source)
	}
	if output, err := runGit(absSource, "rev-parse", "--is-inside-work-tree"); err != nil || output != "true" {
		return fmt.Errorf("submodule source is not a local repository: %q: %w", source, err)
	}
	if _, err := r.Git("-c", "protocol.file.allow=always", "submodule", "add", absSource, clean); err != nil {
		return fmt.Errorf("add submodule %q: %w", name, err)
	}
	return nil
}

// Git executes an arbitrary Git command using the project's hermetic policy.
func (r *Repository) Git(args ...string) (string, error) {
	return runGit(r.Root, args...)
}

// Tree lists the committed tree paths in sorted order.
func (r *Repository) Tree() ([]string, error) {
	output, err := r.Git("ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("list committed tree: %w", err)
	}
	if output == "" {
		return nil, nil
	}
	paths := strings.Split(output, "\n")
	sort.Strings(paths)
	return paths, nil
}

func fixturePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return "", fmt.Errorf("fixture path must be slash-relative: %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("fixture path escapes repository root: %q", name)
	}
	return clean, nil
}

// fixtureCommitTime pins every fixture commit's author and committer date.
// Without it a fixture's commit IDs — and therefore its repository identity —
// depend on the wall clock, which makes any comparison between two separately
// built fixtures a coin toss on how long the first one took.
const fixtureCommitTime = "2026-08-22T00:00:00+0000"

func runGit(dir string, args ...string) (string, error) {
	safety := []string{"-c", "commit.gpgSign=false", "-c", "core.hooksPath=" + os.DevNull}
	argv := append(safety, gitcmd.Args(gitcmd.Hermetic, args...)...)
	command := exec.Command("git", argv...)
	command.Dir = dir
	command.Env = append(gitcmd.Env(gitcmd.Hermetic),
		"GIT_AUTHOR_DATE="+fixtureCommitTime,
		"GIT_COMMITTER_DATE="+fixtureCommitTime,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return trimGitOutput(stdout.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return trimGitOutput(stdout.String()), nil
}

func trimGitOutput(output string) string {
	return strings.TrimSuffix(strings.TrimSuffix(output, "\n"), "\r")
}
