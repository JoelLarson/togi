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

	// Diff is the already-resolved scope recorded by this run. Resolution is wired separately.
	Diff Diff

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
	repository, repoState, requests, err := service.prepareRun(ctx, opts)
	if err != nil {
		return Report{}, err
	}

	now := service.Now
	if now == nil {
		now = time.Now
	}
	startedAt := now().UTC()
	ledger := Ledger{RepoState: repoState, Now: func() time.Time { return startedAt }, Random: service.Random}
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
	if err := service.writeVerbose(opts.Verbose, requests); err != nil {
		return Report{}, err
	}
	gateReports := Collect(ctx, service.Executor, requests, min(runtime.NumCPU(), defaultMaximumWorkers))
	report, err = buildReport(active.runID, repository.Key, startedAt, now().UTC(), service.Diff, gateReports)
	if err != nil {
		return Report{}, err
	}
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

func buildReport(runID, repoID string, startedAt, finishedAt time.Time, diff Diff, gateReports []GateReport) (Report, error) {
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
		SchemaVersion: 2,
		RunID:         runID,
		RepoID:        repoID,
		Diff: DiffReport{
			BaseRef:      diff.BaseRef,
			BaseCommit:   diff.BaseCommit,
			MergeBase:    diff.MergeBase,
			Head:         diff.Head,
			ChangedFiles: diff.ChangedFiles,
			ChangedLines: diff.ChangedLines,
		},
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Gates:      gateReports,
		Findings:   grouped,
	}
	if report.FinishedAt.Before(report.StartedAt) {
		report.FinishedAt = report.StartedAt
	}
	report.Counts = countFindings(grouped)
	report.Verdict = verdictFor(gateReports, grouped)
	return report, nil
}

func (service Service) prepareRun(ctx context.Context, opts Options) (repoid.ID, string, []Request, error) {
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
		return repoid.ID{}, "", nil, fmt.Errorf("resolve repository identity: %w", err)
	}
	if err := validateRepositoryID(repository); err != nil {
		return repoid.ID{}, "", nil, fmt.Errorf("validate repository identity: %w", err)
	}
	repoState := service.Paths.RepoState(repository.Directory)
	if err := validateExternalRepoState(repository.Root, repoState); err != nil {
		return repoid.ID{}, "", nil, err
	}
	loader := service.Loader
	if loader.OverrideDir == "" {
		loader.OverrideDir = service.Paths.GateOverrides()
	}
	loaded, err := loader.LoadAll()
	if err != nil {
		return repoid.ID{}, "", nil, fmt.Errorf("load gates: %w", err)
	}
	requests, err := selectRequests(loaded, opts.GateNames, repository.Root)
	return repository, repoState, requests, err
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
	if err := validateRepositoryID(repository); err != nil {
		return Report{}, fmt.Errorf("validate repository identity: %w", err)
	}
	repoState := service.Paths.RepoState(repository.Directory)
	if err := validateExternalRepoState(repository.Root, repoState); err != nil {
		return Report{}, err
	}
	report, err := (Ledger{RepoState: repoState}).Latest()
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
	if service.Paths.Config == "" || !filepath.IsAbs(service.Paths.Config) {
		return errors.New("configuration root must be absolute")
	}
	if service.Paths.State == "" || !filepath.IsAbs(service.Paths.State) {
		return errors.New("state root must be absolute")
	}
	if service.Loader.OverrideDir != "" && !filepath.IsAbs(service.Loader.OverrideDir) {
		return errors.New("gate override root must be absolute")
	}
	if !service.Executor.Registry.Ready() {
		return errors.New("executor normalizer registry is not initialized")
	}
	if isNilInterface(service.Executor.Enricher) {
		return errors.New("executor enricher is required")
	}
	if isNilInterface(service.Stdout) {
		return errors.New("report output is required")
	}
	return nil
}

func (service Service) validateStatus() error {
	if service.Paths.State == "" || !filepath.IsAbs(service.Paths.State) {
		return errors.New("state root must be absolute")
	}
	if isNilInterface(service.Stdout) {
		return errors.New("report output is required")
	}
	return nil
}

func validateRepositoryID(repository repoid.ID) error {
	if !validRepositoryKey(repository.Key) {
		return errors.New("repository key must be a full lowercase hexadecimal Git or SHA-256 identity")
	}
	if repository.Directory != repository.Key {
		return errors.New("repository state directory must equal the full repository key")
	}
	if !filepath.IsAbs(repository.Root) {
		return errors.New("repository root must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(repository.Root)
	if err != nil {
		return errors.New("repository root cannot be resolved")
	}
	if filepath.Clean(repository.Root) != canonical {
		return errors.New("repository root must be canonical")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return errors.New("repository root must be a directory")
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

func validRepositoryKey(key string) bool {
	if len(key) != 40 && len(key) != 64 {
		return false
	}
	for _, character := range key {
		if character >= '0' && character <= '9' {
			continue
		}
		if character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
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
	for _, candidate := range gates {
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
		requests = append(requests, Request{Gate: candidate, Binding: binding, Root: root})
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
