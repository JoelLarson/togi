package gitcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joellarson/togi/internal/runner"
)

// Isolation declares how much of the operator's git configuration an
// invocation may see. Repo-id resolution honours global config because URL
// rewrites are part of a repo's identity; everything else runs hermetic.
type Isolation struct {
	HonourGlobalConfig bool
}

var (
	// HonourGlobal respects the operator's global git configuration while
	// still stripping ambient GIT_* variables and replace objects.
	HonourGlobal = Isolation{HonourGlobalConfig: true}
	// Hermetic ignores user, system, and global configuration entirely and
	// pins a deterministic locale.
	Hermetic = Isolation{}
)

// ErrOutputLimit means a stream exceeded its capture limit; it is always
// wrapped in a *CommandError.
var ErrOutputLimit = errors.New("output exceeded its limit")

const stderrCaptureLimit = 1 << 20

var truncationMarker = []byte("[output truncated]")

// Env builds the process environment for the policy: ambient GIT_* variables
// are always stripped (case-insensitively) and replace objects disabled;
// Hermetic additionally disconnects system, global, and attribute
// configuration, lazy fetching, and optional locks.
func Env(iso Isolation) []string {
	environment := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "GIT_NO_REPLACE_OBJECTS=1")
	if iso.HonourGlobalConfig {
		return environment
	}
	return append(environment,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

// Args prepends the policy's -c overrides so callers cannot forget them:
// Hermetic disables filesystem monitors and user attribute files.
func Args(iso Isolation, args ...string) []string {
	if iso.HonourGlobalConfig {
		return args
	}
	return append([]string{"-c", "core.fsmonitor=false", "-c", "core.attributesFile=" + os.DevNull}, args...)
}

// CommandError reports a failed git invocation. Stderr carries the trimmed
// diagnostic; stdout is deliberately excluded so repository paths and
// object contents never leak into error text. Unwrap exposes the underlying
// cause, including *exec.ExitError for nonzero exits.
type CommandError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *CommandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

// Output runs git in dir under the policy and returns its stdout. Context
// cancellation is returned as the context's own error; every other failure —
// nonzero exit, teardown failure, output beyond limit — is a *CommandError.
func Output(ctx context.Context, dir string, iso Isolation, limit int, args ...string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("git context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	argv := append([]string{"git"}, Args(iso, args...)...)
	result := runner.Run(ctx, dir, argv, runner.Options{
		Env:              Env(iso),
		StdoutLimit:      limit,
		StderrLimit:      stderrCaptureLimit,
		TruncationMarker: truncationMarker,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, result.CleanupErr)
	}
	stderrText := strings.TrimSpace(string(result.Stderr.Bytes()))
	if result.RunErr != nil || result.CleanupErr != nil {
		return nil, &CommandError{Args: args, Err: errors.Join(result.RunErr, result.CleanupErr), Stderr: stderrText}
	}
	if result.Stdout.Truncated() || result.Stderr.Truncated() {
		return nil, &CommandError{Args: args, Err: ErrOutputLimit, Stderr: stderrText}
	}
	return append([]byte(nil), result.Stdout.Bytes()...), nil
}
