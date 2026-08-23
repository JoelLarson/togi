package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/joellarson/togi/internal/runner"
)

const adapterStreamLimit = 1 << 20

var adapterTruncationMarker = []byte("[output truncated]")

// Codex runs the Codex CLI through its JSONL protocol.
type Codex struct {
	executable string
}

// NewCodex returns a Codex adapter using executable.
func NewCodex(executable string) *Codex {
	return &Codex{executable: executable}
}

func (c *Codex) Name() string {
	return "codex"
}

// Run invokes Codex in the supplied worktree and persists its raw JSONL before
// interpreting process or protocol failures.
func (c *Codex) Run(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, &Error{Err: errors.New("adapter context is required")}
	}
	if request.Sink == nil {
		return Result{}, &Error{Err: errors.New("adapter sink is required")}
	}
	if strings.TrimSpace(c.executable) == "" {
		return Result{}, &Error{Err: errors.New("codex executable is required")}
	}
	if strings.TrimSpace(request.Root) == "" {
		return Result{}, &Error{Err: errors.New("adapter root is required")}
	}
	argv := []string{
		c.executable, "--ask-for-approval", "never",
		"exec", "--ephemeral", "--json",
		"--sandbox", "workspace-write",
		"--ignore-user-config",
		"--cd", request.Root, "-",
	}
	process := runner.Run(ctx, request.Root, argv, runner.Options{
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
		return Result{}, &Error{Retryable: !missingExecutable(process.RunErr), Err: fmt.Errorf("run codex: %w", processErr)}
	}
	if process.Stdout.Truncated() || process.Stderr.Truncated() {
		return Result{}, &Error{Retryable: true, Err: errors.New("codex output exceeded its limit")}
	}
	result, err := decodeCodexJSONL(raw)
	if err != nil {
		return Result{}, &Error{Retryable: true, Err: err}
	}
	return result, nil
}

func decodeCodexJSONL(raw []byte) (Result, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), adapterStreamLimit)
	completed := false
	var usage *Usage
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			return Result{}, errors.New("decode codex JSONL: line is not a JSON object")
		}
		var event struct {
			Type  string `json:"type"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return Result{}, errors.New("decode codex JSONL: malformed JSON object")
		}
		if event.Type == "turn.completed" {
			if event.Usage != nil && (event.Usage.InputTokens < 0 || event.Usage.CachedInputTokens < 0 || event.Usage.OutputTokens < 0) {
				return Result{}, errors.New("decode codex JSONL: invalid usage")
			}
			completed = true
			usage = event.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, fmt.Errorf("decode codex JSONL: %w", err)
	}
	if !completed {
		return Result{}, errors.New("codex JSONL ended without turn.completed")
	}
	return Result{Usage: usage}, nil
}

func missingExecutable(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	return errors.As(err, &execErr) || errors.Is(err, os.ErrNotExist)
}

var _ Adapter = (*Codex)(nil)
