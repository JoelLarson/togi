package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/enricher"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/normalizer"
)

// RawStore persists captured output from one gate execution.
type RawStore interface {
	WriteRaw(gate, language, stream string, raw []byte) error
}

// Request identifies one gate binding to execute against a repository.
type Request struct {
	Gate     gate.Gate
	Binding  gate.Binding
	Root     string
	RawStore RawStore
}

// Executor runs one gate through normalization, enrichment, and grouping.
type Executor struct {
	Registry normalizer.Registry
	Enricher enricher.Enricher
	Now      func() time.Time
}

// Execute runs a gate and always returns a report, including infrastructure errors.
func (e Executor) Execute(parent context.Context, req Request) (report GateReport) {
	report = GateReport{Gate: req.Gate.Manifest.Name, Language: req.Binding.Language}
	now := e.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	defer func() {
		duration := now().Sub(started)
		if duration < 0 {
			duration = 0
		}
		report.DurationMS = duration.Milliseconds()
	}()

	if err := validateExecution(parent, e, req); err != nil {
		return errored(report, err)
	}
	command, err := req.Binding.RenderCommand()
	if err != nil {
		return errored(report, fmt.Errorf("render command: %w", err))
	}
	if err := validateCommand(command); err != nil {
		return errored(report, err)
	}

	ctx, cancel := context.WithTimeout(parent, req.Gate.Manifest.Timeout)
	defer cancel()
	if len(req.Binding.Version.Command) > 0 {
		if versionErr := observeVersion(ctx, req, &report); versionErr != nil {
			return errored(report, versionErr)
		}
	}
	stdout, stderr, runErr := runCommand(ctx, req.Root, command)
	persistErr := persistRaw(req, stdout.Bytes(), stderr.Bytes())
	if persistErr != nil {
		return errored(report, errors.New("persist raw output: storage failure"))
	}
	if ctx.Err() != nil {
		return errored(report, contextExecutionError(ctx.Err()))
	}
	if stdout.Truncated() || stderr.Truncated() {
		return errored(report, errors.New("gate output exceeded the 1 MiB capture limit"))
	}

	exitCode, exited := commandExitCode(runErr)
	if !exited {
		return errored(report, fmt.Errorf("run gate command: %w", runErr))
	}
	successExit := slices.Contains(req.Binding.SuccessExitCodes, exitCode)
	findingExit := slices.Contains(req.Binding.FindingExitCodes, exitCode)
	if !successExit && !findingExit {
		return errored(report, fmt.Errorf("gate command exited with code %d", exitCode))
	}
	if findingExit && stderr.Len() != 0 {
		return errored(report, errors.New("finding exit wrote to stderr"))
	}

	normalized, err := e.Registry.Normalize(req.Binding.Normalizer, normalizer.Context{
		Gate: req.Gate.Manifest.Name, Root: req.Root, Binding: req.Binding,
	}, stdout.Bytes())
	if err != nil {
		return errored(report, errors.New("normalize gate output: invalid tool output; inspect persisted raw output"))
	}
	if findingExit && len(normalized) == 0 {
		return errored(report, errors.New("finding exit produced no valid findings"))
	}
	enriched, err := e.Enricher.Enrich(ctx, enricher.Context{Root: req.Root, Language: req.Binding.Language}, normalized)
	if err != nil {
		return errored(report, errors.New("enrich findings: enrichment failed"))
	}
	grouped, err := finding.Group(enriched)
	if err != nil {
		return errored(report, errors.New("group findings: invalid enriched findings"))
	}
	report.Findings = grouped
	if len(grouped) == 0 {
		report.Status = GatePassed
	} else {
		report.Status = GateFindings
	}
	return report
}

func observeVersion(ctx context.Context, req Request, report *GateReport) error {
	command := req.Binding.Version.Command
	if err := validateCommand(command); err != nil {
		report.Warnings = append(report.Warnings, "version command is invalid")
		return nil
	}
	stdout, stderr, runErr := runCommand(ctx, req.Root, command)
	if ctx.Err() != nil {
		return contextExecutionError(ctx.Err())
	}
	if stdout.Truncated() || stderr.Truncated() {
		report.Warnings = append(report.Warnings, "version command output exceeded the capture limit")
		return nil
	}
	exitCode, exited := commandExitCode(runErr)
	if !exited || exitCode != 0 {
		report.Warnings = append(report.Warnings, "version command failed")
		return nil
	}
	observed, matches, err := observeVersionStreams(req.Binding.Version, stdout.Bytes(), stderr.Bytes())
	if err != nil {
		report.Warnings = append(report.Warnings, "could not observe tool version")
		return nil
	}
	report.ObservedVersion = observed
	if !matches {
		report.Warnings = append(report.Warnings, "observed tool version does not satisfy the configured constraint")
	}
	return nil
}

func observeVersionStreams(version gate.Version, stdout, stderr []byte) (string, bool, error) {
	var lastErr error
	for _, output := range [][]byte{stdout, stderr} {
		if len(output) == 0 {
			continue
		}
		observed, matches, err := version.Observe(string(output))
		if err == nil {
			return observed, matches, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("version output is empty")
	}
	return "", false, lastErr
}

func validateExecution(parent context.Context, e Executor, req Request) error {
	switch {
	case parent == nil:
		return errors.New("execution context is required")
	case strings.TrimSpace(req.Gate.Manifest.Name) == "":
		return errors.New("gate name is required")
	case !safeRawComponent(req.Gate.Manifest.Name):
		return errors.New("gate name is unsafe for raw output storage")
	case strings.TrimSpace(req.Binding.Language) == "":
		return errors.New("binding language is required")
	case !safeRawComponent(req.Binding.Language):
		return errors.New("binding language is unsafe for raw output storage")
	case strings.TrimSpace(req.Binding.Tool) == "":
		return errors.New("binding tool is required")
	case req.Gate.Manifest.Timeout <= 0:
		return errors.New("gate timeout must be positive")
	case strings.TrimSpace(req.Root) == "":
		return errors.New("repository root is required")
	case isNilInterface(req.RawStore):
		return errors.New("raw store is required")
	case isNilInterface(e.Enricher):
		return errors.New("enricher is required")
	case strings.TrimSpace(req.Binding.Normalizer) == "":
		return errors.New("normalizer is required")
	}
	info, err := os.Stat(req.Root)
	if err != nil {
		return errors.New("repository root cannot be opened")
	}
	if !info.IsDir() {
		return errors.New("repository root is not a directory")
	}
	return nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

func validateCommand(command []string) error {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return errors.New("gate command is required")
	}
	for index, argument := range command {
		if argument == "" {
			return fmt.Errorf("gate command argument %d is empty", index)
		}
	}
	return nil
}

func runCommand(ctx context.Context, root string, command []string) (*boundedBuffer, *boundedBuffer, error) {
	stdout := newBoundedBuffer(rawOutputLimit, rawTruncationMarker)
	stderr := newBoundedBuffer(rawOutputLimit, rawTruncationMarker)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = root
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	tree, err := prepareProcessTree(cmd)
	if err != nil {
		return stdout, stderr, fmt.Errorf("prepare process tree: %w", err)
	}
	cmd.Cancel = func() error {
		return tree.terminate(cmd.Process)
	}
	if err := cmd.Start(); err != nil {
		return stdout, stderr, errors.Join(err, tree.close(nil))
	}
	if err := tree.afterStart(cmd.Process); err != nil {
		terminateErr := tree.terminate(cmd.Process)
		waitErr := cmd.Wait()
		return stdout, stderr, errors.Join(err, terminateErr, waitErr, tree.close(cmd.Process))
	}
	waitErr := cmd.Wait()
	return stdout, stderr, errors.Join(waitErr, tree.close(cmd.Process))
}

func persistRaw(req Request, stdout, stderr []byte) error {
	stdoutErr := req.RawStore.WriteRaw(req.Gate.Manifest.Name, req.Binding.Language, "stdout", stdout)
	stderrErr := req.RawStore.WriteRaw(req.Gate.Manifest.Name, req.Binding.Language, "stderr", stderr)
	if stdoutErr != nil || stderrErr != nil {
		return errors.Join(
			wrapIfError("persist stdout", stdoutErr),
			wrapIfError("persist stderr", stderrErr),
		)
	}
	return nil
}

func wrapIfError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func commandExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

func contextExecutionError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("gate deadline exceeded")
	}
	return errors.New("gate execution canceled")
}

func errored(report GateReport, err error) GateReport {
	report.Status = GateErrored
	report.Findings = nil
	if err == nil {
		report.Error = "gate execution errored"
	} else {
		report.Error = err.Error()
	}
	return report
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	marker    []byte
	truncated bool
}

func newBoundedBuffer(limit int, marker []byte) *boundedBuffer {
	return &boundedBuffer{limit: limit, marker: append([]byte(nil), marker...)}
}

func (b *boundedBuffer) Write(input []byte) (int, error) {
	inputLength := len(input)
	if b.truncated {
		return inputLength, nil
	}
	remaining := b.limit - b.buffer.Len()
	if len(input) <= remaining {
		_, _ = b.buffer.Write(input)
		return inputLength, nil
	}
	dataLimit := b.limit - len(b.marker)
	if b.buffer.Len() > dataLimit {
		b.buffer.Truncate(dataLimit)
	} else if b.buffer.Len() < dataLimit {
		needed := dataLimit - b.buffer.Len()
		_, _ = b.buffer.Write(input[:min(needed, len(input))])
	}
	_, _ = b.buffer.Write(b.marker)
	b.truncated = true
	return inputLength, nil
}

func (b *boundedBuffer) Bytes() []byte   { return b.buffer.Bytes() }
func (b *boundedBuffer) Len() int        { return b.buffer.Len() }
func (b *boundedBuffer) Truncated() bool { return b.truncated }
