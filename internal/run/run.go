package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/repoid"
)

// Options controls one phase-one report-only run.
type Options struct {
	Root       string
	GateNames  []string
	ReportOnly bool
	Verbose    bool
	NoColor    bool
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

	// GOOS and ResolveRepo are narrow seams for boundary and orchestration tests.
	GOOS        string
	ResolveRepo func(context.Context, string) (repoid.ID, error)
}

// Run executes the selected Go gates, persists the report, renders it, and returns its typed verdict.
func (service Service) Run(ctx context.Context, opts Options) (report Report, resultErr error) {
	if err := service.checkPlatform(); err != nil {
		return Report{}, err
	}
	if ctx == nil {
		return Report{}, errors.New("run context is required")
	}
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
		return Report{}, fmt.Errorf("resolve repository identity: %w", err)
	}
	loader := service.Loader
	if loader.OverrideDir == "" {
		loader.OverrideDir = service.Paths.GateOverrides()
	}
	loaded, err := loader.LoadAll()
	if err != nil {
		return Report{}, fmt.Errorf("load gates: %w", err)
	}
	requests, err := selectRequests(loaded, opts.GateNames, repository.Root)
	if err != nil {
		return Report{}, err
	}

	now := service.Now
	if now == nil {
		now = time.Now
	}
	startedAt := now().UTC()
	ledger := Ledger{RepoState: service.Paths.RepoState(repository.Directory), Now: func() time.Time { return startedAt }, Random: service.Random}
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
	for index := range requests {
		requests[index].RawStore = active
	}
	if opts.Verbose {
		destination := service.VerboseOut
		if destination == nil {
			destination = service.Stdout
		}
		for _, request := range requests {
			if destination != nil {
				if _, writeErr := fmt.Fprintf(destination, "running %s: %q\n", request.Gate.Manifest.Name, request.Binding.Command); writeErr != nil {
					return Report{}, fmt.Errorf("write verbose output: %w", writeErr)
				}
			}
		}
	}
	gateReports := Collect(ctx, service.Executor, requests, min(runtime.NumCPU(), defaultMaximumWorkers))
	findings := make([]finding.Finding, 0)
	for _, gateReport := range gateReports {
		findings = append(findings, gateReport.Findings...)
	}
	grouped, err := finding.Group(findings)
	if err != nil {
		return Report{}, fmt.Errorf("group collected findings: %w", err)
	}
	slices.SortFunc(gateReports, compareGateReports)
	report = Report{
		SchemaVersion: 1,
		RunID:         active.runID,
		RepoID:        repository.Key,
		StartedAt:     startedAt,
		FinishedAt:    now().UTC(),
		Gates:         gateReports,
		Findings:      grouped,
	}
	if report.FinishedAt.Before(report.StartedAt) {
		report.FinishedAt = report.StartedAt
	}
	report.Counts = countFindings(grouped)
	report.Verdict = verdictFor(gateReports, grouped)
	if err := active.WriteReport(report); err != nil {
		return report, fmt.Errorf("write report: %w", err)
	}
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

// Status renders the newest complete report without loading or executing gates.
func (service Service) Status(ctx context.Context, root string, noColor bool) (Report, error) {
	if err := service.checkPlatform(); err != nil {
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
	report, err := (Ledger{RepoState: service.Paths.RepoState(repository.Directory)}).Latest()
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
	known := make(map[string]struct{}, len(gates))
	requests := make([]Request, 0, len(gates))
	for _, candidate := range gates {
		name := candidate.Manifest.Name
		known[name] = struct{}{}
		if len(selected) > 0 {
			if _, ok := selected[name]; !ok {
				continue
			}
		}
		binding, ok := candidate.Bindings["go"]
		if !ok {
			continue
		}
		requests = append(requests, Request{Gate: candidate, Binding: binding, Root: root})
	}
	for _, name := range requestedUnique {
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("unknown gate %q", name)
		}
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

func verdictFor(gates []GateReport, items []finding.Finding) Verdict {
	for _, gateReport := range gates {
		if gateReport.Status == GateErrored {
			return VerdictErrored
		}
	}
	if len(items) > 0 {
		return VerdictFindings
	}
	return VerdictUnverified
}

func compareGateReports(left, right GateReport) int {
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
