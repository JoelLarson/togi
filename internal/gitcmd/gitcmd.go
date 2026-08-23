package gitcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") || pinnedEnvName(iso, name) {
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

func pinnedEnvName(iso Isolation, name string) bool {
	if strings.EqualFold(name, "GIT_NO_REPLACE_OBJECTS") {
		return true
	}
	if iso.HonourGlobalConfig {
		return false
	}
	for _, owned := range []string{
		"LC_ALL", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_ATTR_NOSYSTEM", "GIT_NO_LAZY_FETCH", "GIT_OPTIONAL_LOCKS",
	} {
		if strings.EqualFold(name, owned) {
			return true
		}
	}
	return false
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
	return OutputEnv(ctx, dir, iso, limit, nil, args...)
}

// OutputEnv runs git with additional explicit environment values. Ambient
// Git variables are removed before the validated values are applied.
func OutputEnv(ctx context.Context, dir string, iso Isolation, limit int, extra map[string]string, args ...string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("git context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	environment, err := explicitEnv(iso, extra)
	if err != nil {
		return nil, err
	}
	return run(ctx, dir, iso, limit, environment, args...)
}

// OutputWithIndex runs Git against an explicitly selected temporary index.
// The index path is supplied separately so arbitrary callers cannot inject
// isolation-owned Git environment variables through OutputEnv.
func OutputWithIndex(ctx context.Context, dir string, iso Isolation, limit int, indexPath string, args ...string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("git context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if indexPath == "" || !filepath.IsAbs(indexPath) || strings.IndexByte(indexPath, 0) >= 0 {
		return nil, errors.New("Git index path must be absolute and contain no NUL")
	}
	environment, err := explicitEnv(iso, nil)
	if err != nil {
		return nil, err
	}
	environment = append(environment, "GIT_INDEX_FILE="+indexPath)
	return run(ctx, dir, iso, limit, environment, args...)
}

// OutputWithConfig runs Git with one exact command-scope configuration entry.
// Key and value are separate environment entries so subsection data containing
// '=' cannot be misparsed as command-line key/value syntax.
func OutputWithConfig(ctx context.Context, dir string, iso Isolation, limit int, key, value string, args ...string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("git context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" || strings.IndexByte(key, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
		return nil, errors.New("Git configuration key and value must contain no NUL")
	}
	environment, err := explicitEnv(iso, nil)
	if err != nil {
		return nil, err
	}
	environment = append(environment,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0="+key,
		"GIT_CONFIG_VALUE_0="+value,
	)
	return run(ctx, dir, iso, limit, environment, args...)
}

func run(ctx context.Context, dir string, iso Isolation, limit int, environment []string, args ...string) ([]byte, error) {
	argv := append([]string{"git"}, Args(iso, args...)...)
	result := runner.Run(ctx, dir, argv, runner.Options{
		Env:              environment,
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

func explicitEnv(iso Isolation, extra map[string]string) ([]string, error) {
	keys := make([]string, 0, len(extra))
	seen := make(map[string]string, len(extra))
	for name := range extra {
		folded := strings.ToUpper(name)
		if previous, duplicate := seen[folded]; duplicate {
			return nil, fmt.Errorf("environment variables %q and %q collide under case folding", previous, name)
		}
		seen[folded] = name
		if allowedIdentityEnv(folded) && name != folded {
			return nil, fmt.Errorf("Git identity environment variable %q must use canonical uppercase spelling", name)
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)

	environment := Env(iso)
	for _, name := range keys {
		value := extra[name]
		if !validEnvName(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment variable %q contains NUL", name)
		}
		if isolationOwns(iso, name) {
			return nil, fmt.Errorf("environment variable %q is owned by Git isolation", name)
		}
		environment = append(environment, name+"="+value)
	}
	return environment, nil
}

func allowedIdentityEnv(name string) bool {
	switch name {
	case "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE":
		return true
	default:
		return false
	}
}

func validEnvName(name string) bool {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return false
	}
	for index, character := range []byte(name) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func isolationOwns(iso Isolation, name string) bool {
	name = strings.ToUpper(name)
	if strings.HasPrefix(name, "GIT_") {
		switch name {
		case "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE":
			return false
		default:
			return true
		}
	}
	if iso.HonourGlobalConfig {
		return false
	}
	switch name {
	case "LC_ALL", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_ATTR_NOSYSTEM", "GIT_NO_LAZY_FETCH", "GIT_OPTIONAL_LOCKS":
		return true
	default:
		return false
	}
}
