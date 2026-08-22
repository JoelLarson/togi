package run

import (
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
	"github.com/joellarson/togi/internal/runner"
)

// RawSink persists captured output for one already-identified gate execution.
type RawSink interface {
	WriteRaw(stream string, raw []byte) error
}

// Request identifies one gate binding to execute against a repository.
type Request struct {
	Gate         gate.Gate
	Binding      gate.Binding
	Position     int
	Root         string
	RawSink      RawSink
	ChangedLines finding.ChangedLines
}

// Executor runs one gate through normalization, enrichment, and grouping.
type Executor struct {
	Enrichers enricher.Registry
	Now       func() time.Time

	runCommand commandRunner
}

type commandRunner func(context.Context, string, []string) runner.Result

// gateCommand is the production runner for gate and version commands: raw
// output limits match what the ledger will persist.
func gateCommand(ctx context.Context, root string, command []string) runner.Result {
	return runner.Run(ctx, root, command, runner.Options{
		StdoutLimit:      rawOutputLimit,
		StderrLimit:      rawOutputLimit,
		TruncationMarker: rawTruncationMarker,
	})
}

// Execute runs a gate and always returns a report, including infrastructure errors.
func (e Executor) Execute(parent context.Context, req Request) (report GateReport) {
	report = NewGateReport(req.Gate, req.Binding, req.Position)
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

	if err := validateExecution(parent, req); err != nil {
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
	run := e.runCommand
	if run == nil {
		run = gateCommand
	}
	if len(req.Binding.Version.Command) > 0 {
		if versionErr := observeVersion(ctx, run, req, &report); versionErr != nil {
			return errored(report, versionErr)
		}
	}
	result := run(ctx, req.Root, command)
	return e.finishExecution(ctx, req, report, result)
}

func (e Executor) finishExecution(ctx context.Context, req Request, report GateReport, result runner.Result) GateReport {
	stdout, stderr := result.Stdout, result.Stderr
	persistErr := persistRaw(req, stdout.Bytes(), stderr.Bytes())
	if persistErr != nil {
		return errored(report, errors.New("persist raw output: storage failure"))
	}
	if err := validateCommandResult(ctx, req.Binding, result); err != nil {
		return errored(report, err)
	}
	normalized, err := req.Binding.Normalize(normalizer.Context{
		Gate: req.Gate.Manifest.Name, Root: req.Root,
	}, stdout.Bytes())
	if err != nil {
		return errored(report, errors.New("normalize gate output: invalid tool output; inspect persisted raw output"))
	}
	_, findingExit := commandExitClassification(req.Binding, result.RunErr)
	if findingExit && len(normalized) == 0 {
		return errored(report, errors.New("finding exit produced no valid findings"))
	}
	grouped, err := finding.Group(normalized)
	if err != nil {
		return errored(report, errors.New("group findings: invalid normalized findings"))
	}
	languageEnricher, ok := e.Enrichers.For(req.Binding.Language)
	if !ok {
		return errored(report, fmt.Errorf("no enricher for language %q", req.Binding.Language))
	}
	enriched, err := languageEnricher.Enrich(ctx, enricher.Context{
		Root:     req.Root,
		Location: req.Gate.Manifest.Location,
	}, grouped)
	if err != nil {
		return errored(report, errors.New("enrich findings: enrichment failed"))
	}
	if req.Gate.Manifest.Scope == gate.Diff {
		enriched, err = finding.FilterTouched(enriched, req.ChangedLines)
		if err != nil {
			return errored(report, errors.New("filter findings by scope: invalid changed-line scope"))
		}
	}
	// FilterTouched re-anchors a group's primary line to its first surviving
	// occurrence, which can break canonical order; the terminal Group restores
	// it and is identity-safe because fingerprints are line-independent.
	grouped, err = finding.Group(enriched)
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

func validateCommandResult(ctx context.Context, binding gate.Binding, result runner.Result) error {
	stdout, stderr := result.Stdout, result.Stderr
	if ctx.Err() != nil {
		return contextExecutionError(ctx.Err())
	}
	if result.CleanupErr != nil {
		return fmt.Errorf("clean up gate process tree: %w", result.CleanupErr)
	}
	if stdout.Truncated() || stderr.Truncated() {
		return errors.New("gate output exceeded the 1 MiB capture limit")
	}
	exitCode, exited := commandExitCode(result.RunErr)
	if !exited {
		return fmt.Errorf("run gate command: %w", result.RunErr)
	}
	successExit := slices.Contains(binding.SuccessExitCodes, exitCode)
	findingExit := slices.Contains(binding.FindingExitCodes, exitCode)
	if !successExit && !findingExit {
		return fmt.Errorf("gate command exited with code %d", exitCode)
	}
	if findingExit && stderr.Len() != 0 {
		return errors.New("finding exit wrote to stderr")
	}
	return nil
}

func commandExitClassification(binding gate.Binding, runErr error) (bool, bool) {
	exitCode, _ := commandExitCode(runErr)
	return slices.Contains(binding.SuccessExitCodes, exitCode), slices.Contains(binding.FindingExitCodes, exitCode)
}

func observeVersion(ctx context.Context, run commandRunner, req Request, report *GateReport) error {
	command := req.Binding.Version.Command
	if err := validateCommand(command); err != nil {
		report.Warnings = append(report.Warnings, "version command is invalid")
		return nil
	}
	result := run(ctx, req.Root, command)
	stdout, stderr := result.Stdout, result.Stderr
	if ctx.Err() != nil {
		return contextExecutionError(ctx.Err())
	}
	if result.CleanupErr != nil {
		return fmt.Errorf("clean up version process tree: %w", result.CleanupErr)
	}
	if stdout.Truncated() || stderr.Truncated() {
		report.Warnings = append(report.Warnings, "version command output exceeded the capture limit")
		return nil
	}
	exitCode, exited := commandExitCode(result.RunErr)
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

// validateExecution checks only genuinely runtime facts: gate validity is
// proven once by gate.Compile, so re-checking manifest and binding fields
// here would be validating a state the loader cannot produce.
func validateExecution(parent context.Context, req Request) error {
	switch {
	case parent == nil:
		return errors.New("execution context is required")
	case !req.Gate.Valid() || !req.Binding.Valid():
		return errors.New("gate was not compiled; load gates through gate.Compile")
	case !req.Gate.Owns(req.Binding):
		return errors.New("binding does not belong to gate")
	case req.Position < 0:
		return errors.New("gauntlet position cannot be negative")
	case strings.TrimSpace(req.Root) == "":
		return errors.New("repository root is required")
	case isNilInterface(req.RawSink):
		return errors.New("raw sink is required")
	}
	if req.Gate.Manifest.Scope == gate.Diff {
		if req.ChangedLines == nil {
			return errors.New("diff-scoped gate requires changed lines")
		}
		if err := finding.ValidateChangedLines(req.ChangedLines); err != nil {
			return errors.New("filter findings by scope: invalid changed-line scope")
		}
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

func persistRaw(req Request, stdout, stderr []byte) error {
	stdoutErr := req.RawSink.WriteRaw("stdout", stdout)
	stderrErr := req.RawSink.WriteRaw("stderr", stderr)
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
