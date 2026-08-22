package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrUnsupportedPlatform means this platform is outside phase-one runtime support.
var ErrUnsupportedPlatform = errors.New("phase-one runtime is unsupported on this platform")

// DefaultWaitDelay bounds how long Wait lingers after cancellation before
// the process is forcibly reaped.
const DefaultWaitDelay = 100 * time.Millisecond

// Options configures one execution.
type Options struct {
	Env              []string      // nil inherits the parent environment
	WaitDelay        time.Duration // 0 means DefaultWaitDelay
	StdoutLimit      int           // capture limit in bytes, including the marker
	StderrLimit      int
	TruncationMarker []byte
}

// Result carries both captured streams and both failure channels: RunErr is
// the command's own failure (including *exec.ExitError for nonzero exits),
// CleanupErr is process-tree teardown failure. Stdout and Stderr are always
// non-nil, even when the command never started.
type Result struct {
	Stdout     *Buffer
	Stderr     *Buffer
	RunErr     error
	CleanupErr error
}

// Run executes argv in dir under ctx with process-group kill and bounded
// capture. It never panics on a bad request; validation failures surface as
// RunErr so callers have a single error channel.
func Run(ctx context.Context, dir string, argv []string, opts Options) Result {
	stdout := NewBuffer(opts.StdoutLimit, opts.TruncationMarker)
	stderr := NewBuffer(opts.StderrLimit, opts.TruncationMarker)
	result := Result{Stdout: stdout, Stderr: stderr}
	if ctx == nil {
		result.RunErr = errors.New("command context is required")
		return result
	}
	if err := ctx.Err(); err != nil {
		result.RunErr = err
		return result
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		result.RunErr = errors.New("command is required")
		return result
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = opts.Env
	waitDelay := opts.WaitDelay
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	cmd.WaitDelay = waitDelay
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		result.RunErr = fmt.Errorf("prepare process tree: %w", err)
		return result
	}
	cmd.Cancel = func() error {
		return tree.terminate(cmd.Process)
	}
	if err := cmd.Start(); err != nil {
		result.RunErr = err
		result.CleanupErr = tree.close(nil)
		return result
	}
	if err := tree.afterStart(cmd.Process); err != nil {
		terminateErr := tree.terminate(cmd.Process)
		waitErr := cmd.Wait()
		result.RunErr = errors.Join(err, terminateErr, waitErr)
		result.CleanupErr = tree.close(cmd.Process)
		return result
	}
	result.RunErr = cmd.Wait()
	result.CleanupErr = tree.close(cmd.Process)
	return result
}
