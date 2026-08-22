// Package gitcmdtest is the one git harness for togi's test fixtures: every
// command runs under the hermetic policy plus fixture safety overrides
// (signing off, hooks disconnected), so no test inherits the developer's
// git configuration.
package gitcmdtest

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/gitcmd"
)

// Git runs one git command hermetically in dir and returns trimmed stdout,
// failing the test on any error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	stdout, stderr, err := run(dir, args)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(stdout)
}

// GitErr runs one git command hermetically in dir for want-failure cases,
// returning trimmed stdout (or stderr when the command failed) and the error.
func GitErr(dir string, args ...string) (string, error) {
	stdout, stderr, err := run(dir, args)
	if err != nil {
		return strings.TrimSpace(stderr), err
	}
	return strings.TrimSpace(stdout), nil
}

func run(dir string, args []string) (string, string, error) {
	safety := []string{"-c", "commit.gpgSign=false", "-c", "core.hooksPath=" + os.DevNull}
	argv := append(safety, gitcmd.Args(gitcmd.Hermetic, args...)...)
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	cmd.Env = gitcmd.Env(gitcmd.Hermetic)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
