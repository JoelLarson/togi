package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/adapter"
	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/flywheel"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/repoid"
)

const (
	DefaultMaxIterations = 20
	DefaultMaxWallClock  = 30 * time.Minute
)

// Options controls one run.
type Options struct {
	Root      string
	Base      string
	GateNames []string
	// ReportOnly stabilizes the pre-phase-3 CLI surface; see docs/implementation.md.
	ReportOnly    bool
	Agent         string
	MaxIterations int
	MaxWallClock  time.Duration
	Verbose       bool
	NoColor       bool
}

// Service ties repository identity, gate data, execution, and external state together.
type Service struct {
	Paths      config.Paths
	Loader     gate.Loader
	Executor   Executor
	Stdout     io.Writer
	VerboseOut io.Writer
	Now        func() time.Time
	Random     io.Reader
	Suite      SuiteRunner
	Adapters   map[string]adapter.Adapter

	WorkspaceFactory     func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error)
	SnapshotOriginal     func(context.Context, string, string) (flywheel.TreeSnapshot, error)
	ExecuteFlywheel      func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome
	ResolveFeatureBranch func(context.Context, string) (string, error)
	ResolveFixIdentity   func(context.Context, string) (string, flywheel.Identity, error)
	RenderReport         func(io.Writer, Report, RenderOptions) error

	// GOOS and ResolveRepo are narrow seams for boundary and orchestration tests.
	GOOS        string
	ResolveRepo func(context.Context, string) (repoid.ID, error)
}

// Run executes the selected Go gates, persists the report, renders it, and returns its typed verdict.
func (service Service) Run(ctx context.Context, opts Options) (report Report, resultErr error) {
	if err := service.checkPlatform(); err != nil {
		return Report{}, err
	}
	if err := service.validateRun(); err != nil {
		return Report{}, err
	}
	if ctx == nil {
		return Report{}, errors.New("run context is required")
	}
	if opts.ReportOnly && strings.TrimSpace(opts.Agent) != "" {
		return Report{}, errors.New("agent cannot be used in report-only mode")
	}
	if !opts.ReportOnly {
		if strings.TrimSpace(opts.Agent) == "" {
			return Report{}, errors.New("agent is required in fix mode")
		}
		if opts.MaxIterations <= 0 {
			return Report{}, errors.New("max iterations must be positive")
		}
		if opts.MaxWallClock <= 0 {
			return Report{}, errors.New("max wall clock must be positive")
		}
	}
	prepared, err := service.prepareRun(ctx, opts)
	if err != nil {
		return Report{}, err
	}
	if !opts.ReportOnly {
		return service.runFix(ctx, opts, prepared)
	}
	return service.runReportOnly(ctx, opts, prepared)
}

func (service Service) runReportOnly(ctx context.Context, opts Options, prepared preparedRun) (report Report, resultErr error) {

	now := service.Now
	if now == nil {
		now = time.Now
	}
	startedAt := now().UTC()
	ledger := Ledger{RepoID: prepared.repository.Key(), RepoState: prepared.repoState, RunsDir: prepared.runsDir, Now: func() time.Time { return startedAt }, Random: service.Random}
	active, err := ledger.Start()
	if err != nil {
		return Report{}, fmt.Errorf("start run ledger: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, active.Close())
		}
	}()
	for index := range prepared.requests {
		sink, sinkErr := active.RawSink(prepared.requests[index].Gate.Manifest.Name, prepared.requests[index].Binding.Language)
		if sinkErr != nil {
			return Report{}, fmt.Errorf("prepare raw sink: %w", sinkErr)
		}
		prepared.requests[index].RawSink = sink
	}
	if err := service.writeVerbose(opts.Verbose, prepared.requests); err != nil {
		return Report{}, err
	}
	gateReports := Collect(ctx, service.Executor, prepared.requests, min(runtime.NumCPU(), defaultMaximumWorkers))
	report, err = ComposeReport(active.runID, prepared.repository.Key(), startedAt, now().UTC(), prepared.diff, gateReports)
	if err != nil {
		return Report{}, err
	}
	if err := active.WriteReport(report); err != nil {
		return report, fmt.Errorf("write report: %w", err)
	}
	report.Ref = RunRef{ID: active.runID, Dir: active.Dir}
	closeErr := active.Close()
	closed = closeErr == nil
	if closeErr != nil {
		return report, fmt.Errorf("close run ledger: %w", closeErr)
	}
	output := service.Stdout
	if output == nil {
		output = io.Discard
	}
	if err := Render(output, report, RenderOptions{Color: writerIsTerminal(output), NoColor: opts.NoColor}); err != nil {
		return report, fmt.Errorf("render report: %w", err)
	}
	return report, &ExitError{Code: ExitCode(report.Verdict), Err: verdictError(report.Verdict)}
}

func (service Service) writeVerbose(enabled bool, requests []Request) error {
	if !enabled {
		return nil
	}
	destination := service.VerboseOut
	if destination == nil {
		destination = service.Stdout
	}
	for _, request := range requests {
		if destination != nil {
			if _, err := fmt.Fprintf(destination, "running %s: %q\n", request.Gate.Manifest.Name, request.Binding.Command); err != nil {
				return fmt.Errorf("write verbose output: %w", err)
			}
		}
	}
	return nil
}

func ComposeReport(runID, repoID string, startedAt, finishedAt time.Time, diff Diff, gateReports []GateReport) (Report, error) {
	if err := validateDiff(diff); err != nil {
		return Report{}, fmt.Errorf("validate report diff: %w", err)
	}
	gateReports = cloneGateReports(gateReports)
	findings := make([]finding.Finding, 0)
	for _, gateReport := range gateReports {
		findings = append(findings, gateReport.Findings...)
	}
	grouped, err := finding.Group(findings)
	if err != nil {
		return Report{}, fmt.Errorf("group collected findings: %w", err)
	}
	slices.SortFunc(gateReports, compareGateReports)
	report := Report{
		SchemaVersion: ReportSchemaVersion,
		RunID:         runID,
		RepoID:        repoID,
		Diff:          diffReport(diff),
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		Gates:         gateReports,
		Findings:      grouped,
	}
	if report.FinishedAt.Before(report.StartedAt) {
		report.FinishedAt = report.StartedAt
	}
	report.Counts = countFindings(grouped)
	report.Verdict = verdictFor(gateReports)
	if err := validateReport(report, runID); err != nil {
		return Report{}, fmt.Errorf("validate composed report: %w", err)
	}
	return report, nil
}

func validateDiff(diff Diff) error {
	if err := validateDiffReport(diffReport(diff)); err != nil {
		return err
	}
	if diff.Lines == nil {
		return errors.New("diff changed lines are required")
	}
	if len(diff.Lines) != diff.ChangedFiles {
		return errors.New("diff changed-line files do not match changed file count")
	}
	if err := finding.ValidateChangedLines(diff.Lines); err != nil {
		return fmt.Errorf("diff changed lines are invalid: %w", err)
	}
	lineCount := 0
	maximumInt := int(^uint(0) >> 1)
	for path, ranges := range diff.Lines {
		for index, lineRange := range ranges {
			if index > 0 {
				previous := ranges[index-1]
				if lineRange.Start <= previous.End || (previous.End < maximumInt && lineRange.Start == previous.End+1) {
					return fmt.Errorf("diff changed ranges for %q are overlapping, adjacent, or out of order", path)
				}
			}
			cardinality := lineRange.End - lineRange.Start + 1
			if lineCount > maximumInt-cardinality {
				return errors.New("diff changed-line count overflows int")
			}
			lineCount += cardinality
		}
	}
	if lineCount != diff.ChangedLines {
		return fmt.Errorf("diff changed-line count %d does not match ranges %d", diff.ChangedLines, lineCount)
	}
	return nil
}

func diffReport(diff Diff) DiffReport {
	return DiffReport{
		BaseRef:      diff.BaseRef,
		BaseCommit:   diff.BaseCommit,
		MergeBase:    diff.MergeBase,
		Head:         diff.Head,
		ChangedFiles: diff.ChangedFiles,
		ChangedLines: diff.ChangedLines,
	}
}

type preparedRun struct {
	repository repoid.ID
	repoState  string
	runsDir    string
	diff       Diff
	requests   []Request
}

func (service Service) prepareRun(ctx context.Context, opts Options) (preparedRun, error) {
	root := opts.Root
	if root == "" {
		root = "."
	}
	resolve := service.ResolveRepo
	if resolve == nil {
		resolve = repoid.Resolve
	}
	repository, err := resolve(ctx, root)
	if err != nil {
		return preparedRun{}, fmt.Errorf("resolve repository identity: %w", err)
	}
	if repository.IsZero() {
		return preparedRun{}, errors.New("repository identity is required")
	}
	repoState := service.Paths.RepoState(repository)
	runsDir := service.Paths.RunsDir(repository)
	if err := validateExternalRepoState(repository.Root(), repoState); err != nil {
		return preparedRun{}, err
	}
	diff, err := resolveDiff(ctx, repository.Root(), opts.Base)
	if err != nil {
		return preparedRun{}, fmt.Errorf("resolve diff scope: %w", err)
	}
	if err := validateDiff(diff); err != nil {
		return preparedRun{}, fmt.Errorf("validate diff scope: %w", err)
	}
	loaded, err := service.Loader.LoadAll()
	if err != nil {
		return preparedRun{}, fmt.Errorf("load gates: %w", err)
	}
	requests, err := selectRequests(loaded, opts.GateNames, repository.Root())
	if err != nil {
		return preparedRun{}, err
	}
	for index := range requests {
		if _, ok := service.Executor.Enrichers.For(requests[index].Binding.Language); !ok {
			return preparedRun{}, fmt.Errorf("gate %q: no enricher for language %q", requests[index].Gate.Manifest.Name, requests[index].Binding.Language)
		}
		if requests[index].Gate.Manifest.Scope == gate.Diff {
			requests[index].ChangedLines = diff.Lines
		}
	}
	return preparedRun{repository: repository, repoState: repoState, runsDir: runsDir, diff: diff, requests: requests}, nil
}

// Status renders the newest complete report without loading or executing gates.
func (service Service) Status(ctx context.Context, root string, noColor bool) (Report, error) {
	if err := service.checkPlatform(); err != nil {
		return Report{}, err
	}
	if err := service.validateStatus(); err != nil {
		return Report{}, err
	}
	if ctx == nil {
		return Report{}, errors.New("status context is required")
	}
	if root == "" {
		root = "."
	}
	resolve := service.ResolveRepo
	if resolve == nil {
		resolve = repoid.Resolve
	}
	repository, err := resolve(ctx, root)
	if err != nil {
		return Report{}, fmt.Errorf("resolve repository identity: %w", err)
	}
	if repository.IsZero() {
		return Report{}, errors.New("repository identity is required")
	}
	repoState := service.Paths.RepoState(repository)
	if err := validateExternalRepoState(repository.Root(), repoState); err != nil {
		return Report{}, err
	}
	report, err := (Ledger{RepoID: repository.Key(), RepoState: repoState, RunsDir: service.Paths.RunsDir(repository)}).Latest()
	if err != nil {
		return Report{}, fmt.Errorf("read latest report: %w", err)
	}
	output := service.Stdout
	if output == nil {
		output = io.Discard
	}
	if err := Render(output, report, RenderOptions{Color: writerIsTerminal(output), NoColor: noColor}); err != nil {
		return Report{}, fmt.Errorf("render report: %w", err)
	}
	return report, nil
}

func (service Service) validateRun() error {
	if service.Paths.IsZero() {
		return errors.New("storage paths are required")
	}
	if service.Loader.OverrideDir != "" && !filepath.IsAbs(service.Loader.OverrideDir) {
		return errors.New("gate override root must be absolute")
	}
	if len(service.Executor.Enrichers.Languages()) == 0 {
		return errors.New("executor enricher registry is not initialized")
	}
	if isNilInterface(service.Stdout) {
		return errors.New("report output is required")
	}
	return nil
}

func (service Service) validateStatus() error {
	if service.Paths.IsZero() {
		return errors.New("storage paths are required")
	}
	if isNilInterface(service.Stdout) {
		return errors.New("report output is required")
	}
	return nil
}

func validateExternalRepoState(repositoryRoot, repoState string) error {
	prospective, err := resolveProspectiveDirectory(repoState)
	if err != nil {
		return fmt.Errorf("validate repository state destination: %w", err)
	}
	relative, err := filepath.Rel(repositoryRoot, prospective)
	if err != nil {
		return fmt.Errorf("compare repository state destination: %w", err)
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)) {
		return errors.New("repository state destination must be outside the target repository")
	}
	return nil
}

func resolveProspectiveDirectory(destination string) (string, error) {
	destination = filepath.Clean(destination)
	current := destination
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			resolvedInfo, err := os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !resolvedInfo.IsDir() {
				return "", fmt.Errorf("existing state ancestor %q is not a directory", current)
			}
			remainder, err := filepath.Rel(current, destination)
			if err != nil {
				return "", err
			}
			if remainder == "." {
				return filepath.Clean(resolved), nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("repository state destination has no existing directory ancestor")
		}
		current = parent
	}
}

func (service Service) checkPlatform() error {
	platform := service.GOOS
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "linux" {
		return fmt.Errorf("%w: %s", ErrUnsupportedPlatform, platform)
	}
	return nil
}

func selectRequests(gates []gate.Gate, requested []string, root string) ([]Request, error) {
	selected := make(map[string]struct{})
	requestedUnique := make([]string, 0, len(requested))
	for _, name := range requested {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("gate name cannot be empty")
		}
		if _, exists := selected[name]; !exists {
			requestedUnique = append(requestedUnique, name)
			selected[name] = struct{}{}
		}
	}
	known := make(map[string]gate.Gate, len(gates))
	for _, candidate := range gates {
		name := candidate.Manifest.Name
		if _, exists := known[name]; exists {
			return nil, fmt.Errorf("duplicate loaded gate manifest %q", name)
		}
		known[name] = candidate
	}
	for _, name := range requestedUnique {
		candidate, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("unknown gate %q", name)
		}
		if _, ok := candidate.Bindings["go"]; !ok {
			return nil, fmt.Errorf("requested gate %q has no Go binding", name)
		}
	}
	requests := make([]Request, 0, len(gates))
	for position, candidate := range gates {
		name := candidate.Manifest.Name
		if len(selected) > 0 {
			if _, ok := selected[name]; !ok {
				continue
			}
		}
		binding, ok := candidate.Bindings["go"]
		if !ok {
			continue
		}
		requests = append(requests, Request{Gate: candidate, Binding: binding, Position: position, Root: root})
	}
	if len(requests) == 0 {
		return nil, errors.New("no Go gates selected")
	}
	return requests, nil
}

func countFindings(items []finding.Finding) Counts {
	var counts Counts
	for _, item := range items {
		occurrences := 1 + len(item.Occurrences)
		counts.Occurrences += occurrences
		switch item.Severity {
		case finding.Error:
			counts.Errors += occurrences
		case finding.Warning:
			counts.Warnings += occurrences
		case finding.Info:
			counts.Info += occurrences
		}
	}
	return counts
}

func verdictFor(gates []GateReport) Verdict {
	for _, gateReport := range gates {
		if gateReport.Status == GateErrored {
			return VerdictErrored
		}
	}
	for _, gateReport := range gates {
		for _, item := range gateReport.Findings {
			if slices.Contains(gateReport.Blocking, item.Severity) {
				return VerdictFindings
			}
		}
	}
	return VerdictUnverified
}

func compareGateReports(left, right GateReport) int {
	if left.Position != right.Position {
		return left.Position - right.Position
	}
	if left.Gate != right.Gate {
		return strings.Compare(left.Gate, right.Gate)
	}
	return strings.Compare(left.Language, right.Language)
}

func verdictError(verdict Verdict) error {
	switch verdict {
	case VerdictFindings:
		return errors.New("findings remain")
	case VerdictErrored:
		return errors.New("one or more gates errored")
	case VerdictUnverified:
		return errors.New("run is unverified")
	case VerdictBlocked:
		return errors.New("fix loop is blocked")
	case VerdictRails:
		return errors.New("fix loop exhausted its rails")
	case VerdictUnsealed:
		return errors.New("run is unsealed")
	default:
		return errors.New("unknown run verdict")
	}
}

func writerIsTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
