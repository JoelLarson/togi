package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/joellarson/togi/internal/runner"
)

// Command runs a named agent CLI with the brief on stdin and the worktree as cwd.
type Command struct {
	name       string
	executable string
}

// NewCommand returns a vendor-neutral adapter that invokes executable in the
// worktree and treats the resulting tree as the agent's output.
func NewCommand(name, executable string) *Command {
	return &Command{name: name, executable: executable}
}

func (c *Command) Name() string {
	if c == nil {
		return ""
	}
	return c.name
}

// Run invokes the configured CLI in the supplied worktree. The brief enters on
// stdin; results are the worktree diff the caller inspects after return.
func (c *Command) Run(ctx context.Context, request Request) (Result, error) {
	if c == nil {
		return Result{}, &Error{Err: errors.New("adapter is required")}
	}
	if ctx == nil {
		return Result{}, &Error{Err: errors.New("adapter context is required")}
	}
	if request.Sink == nil {
		return Result{}, &Error{Err: errors.New("adapter sink is required")}
	}
	if strings.TrimSpace(c.name) == "" {
		return Result{}, &Error{Err: errors.New("adapter name is required")}
	}
	if strings.TrimSpace(c.executable) == "" {
		return Result{}, &Error{Err: fmt.Errorf("%s executable is required", c.name)}
	}
	if strings.TrimSpace(request.Root) == "" {
		return Result{}, &Error{Err: errors.New("adapter root is required")}
	}
	process := runner.Run(ctx, request.Root, []string{c.executable}, runner.Options{
		Stdin:            strings.NewReader(request.Brief),
		StdoutLimit:      adapterStreamLimit,
		StderrLimit:      adapterStreamLimit,
		TruncationMarker: adapterTruncationMarker,
	})
	raw := process.Stdout.Bytes()
	if err := request.Sink.WriteAdapterJSONL(raw); err != nil {
		return Result{}, &Error{Err: fmt.Errorf("persist adapter JSONL: %w", err)}
	}
	if processErr := errors.Join(process.RunErr, process.CleanupErr); processErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			processErr = errors.Join(ctxErr, processErr)
		}
		return Result{}, &Error{Retryable: !missingExecutable(process.RunErr), Err: fmt.Errorf("run %s: %w", c.name, processErr)}
	}
	if process.Stdout.Truncated() || process.Stderr.Truncated() {
		return Result{}, &Error{Retryable: true, Err: fmt.Errorf("%s output exceeded its limit", c.name)}
	}
	return Result{}, nil
}

var _ Adapter = (*Command)(nil)
