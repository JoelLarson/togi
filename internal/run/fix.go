package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/joellarson/togi/internal/adapter"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/flywheel"
	"github.com/joellarson/togi/internal/gitcmd"
	"github.com/joellarson/togi/internal/waiver"
)

// SuiteRunner is the behavioral-suite boundary used by fix orchestration.
type SuiteRunner interface {
	Run(context.Context, string, []string, bool) (SuiteResult, error)
}

// FixWorkspace is the complete owned-worktree lifecycle used by fix mode.
type FixWorkspace interface {
	flywheel.WorkspacePort
	SnapshotValidated(context.Context) (flywheel.ValidatedTree, error)
	SquashValidated(context.Context, flywheel.ValidatedTree) (string, error)
	Land(context.Context, string) (flywheel.LandingStatus, error)
	Cleanup(context.Context, flywheel.CleanupDisposition) error
}

type ledgerAudit struct{ run *RunLedger }

func (audit ledgerAudit) WritePlan(raw []byte) error { return audit.run.WritePlan(raw) }
func (audit ledgerAudit) WriteBrief(batchID string, attempt int, raw []byte) error {
	return audit.run.WriteBrief(batchID, attempt, raw)
}
func (audit ledgerAudit) AdapterSink(batchID string, attempt int) (adapter.Sink, error) {
	if audit.run == nil {
		return nil, ErrUninitialized
	}
	return ledgerAdapterSink{run: audit.run, batchID: batchID, attempt: attempt}, nil
}

type ledgerAdapterSink struct {
	run     *RunLedger
	batchID string
	attempt int
}

func (sink ledgerAdapterSink) WriteAdapterJSONL(raw []byte) error {
	return sink.run.WriteAdapterJSONL(sink.batchID, sink.attempt, raw)
}

type usageAdapter struct {
	adapter.Adapter
	mu    sync.Mutex
	usage adapter.Usage
	seen  bool
}

func (wrapped *usageAdapter) Run(ctx context.Context, request adapter.Request) (adapter.Result, error) {
	result, err := wrapped.Adapter.Run(ctx, request)
	if result.Usage != nil {
		wrapped.mu.Lock()
		wrapped.usage.InputTokens += result.Usage.InputTokens
		wrapped.usage.CachedInputTokens += result.Usage.CachedInputTokens
		wrapped.usage.OutputTokens += result.Usage.OutputTokens
		wrapped.seen = true
		wrapped.mu.Unlock()
	}
	return result, err
}

func (wrapped *usageAdapter) Usage() *adapter.Usage {
	wrapped.mu.Lock()
	defer wrapped.mu.Unlock()
	if !wrapped.seen {
		return nil
	}
	usage := wrapped.usage
	return &usage
}

type fixExecution struct {
	baseline  SuiteResult
	final     *SuiteResult
	gates     []GateReport
	outcome   flywheel.Outcome
	integrity []finding.Finding
	landing   LandingReport
	branch    string
	usage     *adapter.Usage
	verdict   Verdict
	failure   error
	cleanup   func() error
	waivers   []waiver.Record
}

func loadWaiverRecords(stateDir string) ([]waiver.Record, error) {
	records, err := (waiver.Store{Dir: stateDir}).Load()
	if err != nil {
		return nil, fmt.Errorf("load waivers: %w", err)
	}
	return cloneWaiverRecords(records), nil
}

func loadWaivedFingerprints(stateDir string) (map[string]struct{}, error) {
	records, err := loadWaiverRecords(stateDir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(records))
	for _, record := range records {
		result[record.Fingerprint] = struct{}{}
	}
	return result, nil
}

func (service Service) runFix(ctx context.Context, opts Options, prepared preparedRun) (report Report, resultErr error) {
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

	rails, err := flywheel.NewRails(flywheel.RailConfig{MaxIterations: opts.MaxIterations, MaxWallClock: opts.MaxWallClock}, now)
	if err != nil {
		return Report{}, fmt.Errorf("construct fix rails: %w", err)
	}
	waivedFingerprints, err := loadWaivedFingerprints(prepared.repoState)
	if err != nil {
		return Report{}, err
	}
	suite := service.Suite
	if suite == nil {
		suite = NewGoSuite("go")
	}
	execution := fixExecution{
		landing: LandingReport{Status: string(flywheel.LandingBlocked)},
		verdict: VerdictErrored,
		waivers: prepared.waivers,
	}
	var baselineErr error
	execution.baseline, baselineErr = suite.Run(ctx, prepared.repository.Root(), nil, true)
	if baselineErr != nil && execution.baseline.Status == "" {
		execution.baseline.Status = SuiteErrored
		execution.baseline.Diagnostic = "behavioral suite infrastructure failed"
	}
	execution.gates, err = service.collectGates(ctx, active, prepared.requests, prepared.repository.Root(), prepared.diff)
	if err != nil {
		execution.failure = err
	}
	if branch, branchErr := service.resolveFeatureBranch(ctx, prepared.repository.Root()); branchErr == nil {
		execution.branch = branch
	} else if execution.failure == nil {
		execution.failure = branchErr
	}

	blockers := blockingFindings(execution.gates)
	switch {
	case execution.failure != nil:
		execution.verdict = VerdictErrored
	case gatesErrored(execution.gates):
		execution.verdict = VerdictErrored
	case execution.baseline.Status == SuiteMissing || execution.baseline.Status == SuiteFailed:
		execution.verdict = VerdictUnverified
	case execution.baseline.Status != SuitePassed || baselineErr != nil:
		execution.verdict = VerdictErrored
	case len(blockers) == 0:
		execution.verdict = VerdictUnsealed
		execution.landing.Status = string(flywheel.LandingNotNeeded)
	default:
		execution = service.executeFixLoop(ctx, opts, prepared, active, suite, rails, execution, blockers, waivedFingerprints)
	}
	cleanupDone := execution.cleanup == nil
	defer func() {
		if !cleanupDone {
			resultErr = errors.Join(resultErr, execution.cleanup())
		}
	}()

	report, err = composeFixReport(active.runID, prepared.repository.Key(), startedAt, now().UTC(), prepared.diff, execution, rails, opts.Agent)
	if err != nil {
		return Report{}, err
	}
	render := service.RenderReport
	if render == nil {
		render = Render
	}
	output := service.Stdout
	if output == nil {
		output = io.Discard
	}
	renderOptions := RenderOptions{Color: writerIsTerminal(output), NoColor: opts.NoColor}
	var rendered bytes.Buffer
	if err := render(&rendered, report, renderOptions); err != nil {
		return Report{}, fmt.Errorf("stage report rendering: %w", err)
	}
	if err := active.WriteReportAudit(report); err != nil {
		return report, fmt.Errorf("write pre-cleanup report audit: %w", err)
	}
	if execution.cleanup != nil {
		cleanupErr := execution.cleanup()
		cleanupDone = true
		if cleanupErr != nil {
			execution.verdict = VerdictErrored
			execution.failure = fmt.Errorf("clean up fix worktree: %w", cleanupErr)
			execution.landing.Error = publicFixFailure(execution.verdict, execution.landing.Status)
			report, err = composeFixReport(active.runID, prepared.repository.Key(), startedAt, now().UTC(), prepared.diff, execution, rails, opts.Agent)
			if err != nil {
				return Report{}, err
			}
			rendered.Reset()
			if err := render(&rendered, report, renderOptions); err != nil {
				return Report{}, fmt.Errorf("restage report rendering: %w", err)
			}
		}
	}
	if err := active.WriteReport(report); err != nil {
		return report, fmt.Errorf("write report: %w", err)
	}
	report.Ref = RunRef{ID: active.runID, Dir: active.Dir}
	if _, err := output.Write(rendered.Bytes()); err != nil {
		return report, fmt.Errorf("render report: %w", err)
	}
	closeErr := active.Close()
	closed = closeErr == nil
	if closeErr != nil {
		return report, fmt.Errorf("close run ledger: %w", closeErr)
	}
	return report, &ExitError{Code: ExitCode(report.Verdict), Err: verdictError(report.Verdict)}
}

func (service Service) executeFixLoop(ctx context.Context, opts Options, prepared preparedRun, active *RunLedger, suite SuiteRunner, rails *flywheel.Rails, execution fixExecution, blockers []finding.Finding, waivedFingerprints map[string]struct{}) (result fixExecution) {
	selected, ok := service.Adapters[opts.Agent]
	if !ok || isNilInterface(selected) {
		execution.failure = fmt.Errorf("agent adapter %q is unavailable", opts.Agent)
		return execution
	}
	branch, identity, err := service.resolveFixIdentity(ctx, prepared.repository.Root())
	if err != nil {
		execution.failure = err
		return execution
	}
	execution.branch = branch
	snapshotOriginal := service.SnapshotOriginal
	if snapshotOriginal == nil {
		snapshotOriginal = flywheel.SnapshotOriginal
	}
	original, err := snapshotOriginal(ctx, prepared.repository.Root(), prepared.diff.Head)
	if err != nil {
		execution.failure = fmt.Errorf("snapshot original tree: %w", err)
		return execution
	}
	createWorkspace := service.WorkspaceFactory
	if createWorkspace == nil {
		createWorkspace = func(ctx context.Context, spec flywheel.WorkspaceSpec) (FixWorkspace, error) {
			return flywheel.CreateWorkspace(ctx, spec)
		}
	}
	workspace, err := createWorkspace(ctx, flywheel.WorkspaceSpec{
		RepositoryRoot: prepared.repository.Root(), Path: service.Paths.WorktreeDir(prepared.repository, active.runID),
		RunID: active.runID, OriginalHead: prepared.diff.Head, FeatureBranch: branch, Identity: identity,
	})
	if err != nil {
		execution.failure = fmt.Errorf("create fix worktree: %w", err)
		return execution
	}
	if isNilInterface(workspace) {
		execution.failure = errors.New("create fix worktree: workspace is unavailable")
		return execution
	}
	cleanupDisposition := flywheel.CleanupPreserveValidated
	execution.cleanup = func() error { return workspace.Cleanup(ctx, cleanupDisposition) }

	tracked := &usageAdapter{Adapter: selected}
	validator := flywheel.AttemptValidator{Original: original, Baseline: blockers, WaivedFingerprints: waivedFingerprints}
	var barrierReports []GateReport
	validator.RunGates = func(validationCtx context.Context, root string, batch flywheel.Batch) flywheel.GateValidation {
		reports, runErr := service.collectValidationGates(validationCtx, active, prepared, workspace.Root(), root, true, validationRequests(prepared.requests, batch.Findings))
		result := flywheel.GateValidation{Blocking: blockingFindings(reports)}
		if runErr != nil {
			result.Errored = []string{"ledger"}
		}
		result.Errored = append(result.Errored, erroredGateNames(reports)...)
		return result
	}
	validator.RunPackages = func(validationCtx context.Context, root string, packages []string, full bool) flywheel.SuiteValidation {
		result, runErr := suite.Run(validationCtx, root, packages, full)
		if runErr != nil || result.Status == SuiteErrored {
			return flywheel.SuiteValidation{InfrastructureError: "behavioral suite infrastructure failed"}
		}
		return flywheel.SuiteValidation{Passed: result.Status == SuitePassed}
	}
	ports := flywheel.Ports{
		Adapter: tracked, Workspace: workspace, Audit: ledgerAudit{run: active},
		Validate: func(validateCtx context.Context, batch flywheel.Batch) flywheel.ValidationResult {
			changed, err := workspace.ChangedFiles(validateCtx)
			if err != nil {
				return flywheel.ValidationResult{Kind: flywheel.ValidationInfrastructureFailure, Failure: "inspect changed files"}
			}
			return validator.Validate(validateCtx, workspace.Root(), changed, batch)
		},
		Barrier: func(barrierCtx context.Context) flywheel.ValidationResult {
			snapshot, snapshotErr := workspace.SnapshotValidated(barrierCtx)
			if snapshotErr != nil || isNilInterface(snapshot) {
				return flywheel.ValidationResult{Kind: flywheel.ValidationInfrastructureFailure, Failure: "create immutable all-gate barrier snapshot"}
			}
			reports, runErr := service.collectValidationGates(barrierCtx, active, prepared, workspace.Root(), snapshot.Root(), false, prepared.requests)
			verifyErr := snapshot.Verify(barrierCtx)
			closeErr := snapshot.Close()
			barrierReports = reports
			if runErr != nil || verifyErr != nil || closeErr != nil || gatesErrored(reports) {
				return flywheel.ValidationResult{Kind: flywheel.ValidationInfrastructureFailure, Failure: "all-gate barrier infrastructure failed", Findings: blockingFindings(reports)}
			}
			return flywheel.ValidationResult{Kind: flywheel.ValidationPassed, Findings: blockingFindings(reports)}
		},
	}
	execute := service.ExecuteFlywheel
	if execute == nil {
		execute = flywheel.Execute
	}
	execution.outcome = execute(ctx, flywheel.Request{MergeBase: prepared.diff.MergeBase, OriginalHead: prepared.diff.Head, InitialFindings: blockers, Rails: rails}, ports)
	execution.usage = tracked.Usage()
	if len(barrierReports) != 0 {
		execution.gates = barrierReports
	}
	for _, item := range execution.outcome.Findings {
		if item.Gate == "integrity" {
			execution.integrity = append(execution.integrity, item)
		}
	}
	execution.verdict = verdictForFlywheel(execution.outcome.Kind)
	if execution.outcome.Kind != flywheel.OutcomeReady {
		execution.failure = errors.New(execution.outcome.Failure)
		if hasValidatedBatch(execution.outcome.Plan.Batches) {
			execution.landing.PreservedBranch = "togi/run-" + active.runID
		}
		return execution
	}

	finalCtx, cancelFinal, railCtxErr := rails.ExecutionContext(ctx)
	if railCtxErr != nil {
		execution.verdict = VerdictErrored
		execution.failure = railCtxErr
		if errors.Is(railCtxErr, flywheel.ErrRailExhausted) {
			execution.verdict = VerdictRails
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	finalSnapshot, snapshotErr := workspace.SnapshotValidated(finalCtx)
	snapshotCause := context.Cause(finalCtx)
	rails.ObserveExecutionContext(finalCtx)
	if snapshotCause != nil {
		cancelFinal()
		closeErr := error(nil)
		if !isNilInterface(finalSnapshot) {
			closeErr = finalSnapshot.Close()
		}
		execution.verdict = VerdictErrored
		execution.failure = fmt.Errorf("immutable final-suite snapshot canceled: %w", snapshotCause)
		if errors.Is(snapshotCause, flywheel.ErrRailExhausted) {
			execution.verdict = VerdictRails
			execution.failure = snapshotCause
		}
		if closeErr != nil {
			execution.verdict = VerdictErrored
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if snapshotErr != nil || isNilInterface(finalSnapshot) {
		cancelFinal()
		if !isNilInterface(finalSnapshot) {
			_ = finalSnapshot.Close()
		}
		execution.verdict = VerdictErrored
		execution.failure = errors.New("create immutable final-suite snapshot")
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	final, finalErr := suite.Run(finalCtx, finalSnapshot.Root(), nil, true)
	verifyErr := finalSnapshot.Verify(finalCtx)
	finalCause := context.Cause(finalCtx)
	rails.ObserveExecutionContext(finalCtx)
	cancelFinal()
	execution.final = &final
	if errors.Is(finalCause, flywheel.ErrRailExhausted) {
		closeErr := finalSnapshot.Close()
		execution.verdict = VerdictRails
		execution.failure = finalCause
		if closeErr != nil {
			execution.verdict = VerdictErrored
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if finalCause != nil {
		closeErr := finalSnapshot.Close()
		execution.verdict = VerdictErrored
		execution.failure = fmt.Errorf("final behavioral suite canceled: %w", finalCause)
		if closeErr != nil {
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if finalErr != nil || verifyErr != nil || final.Status == SuiteErrored || final.Status == SuiteMissing {
		closeErr := finalSnapshot.Close()
		execution.verdict = VerdictErrored
		execution.failure = errors.New("final behavioral suite infrastructure failed")
		if closeErr != nil {
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if final.Status != SuitePassed {
		closeErr := finalSnapshot.Close()
		execution.verdict = VerdictBlocked
		execution.failure = errors.New("final behavioral suite failed")
		if closeErr != nil {
			execution.verdict = VerdictErrored
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if err := rails.AdmitLanding(); err != nil {
		closeErr := finalSnapshot.Close()
		execution.verdict = VerdictRails
		execution.failure = err
		if closeErr != nil {
			execution.verdict = VerdictErrored
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if cause := context.Cause(ctx); cause != nil {
		closeErr := finalSnapshot.Close()
		execution.verdict = VerdictErrored
		execution.failure = fmt.Errorf("landing admission canceled: %w", cause)
		if closeErr != nil {
			execution.failure = errors.New("close immutable final-suite snapshot")
		}
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	landingCtx, cancelLanding := flywheel.NewLandingTransactionContext(ctx)
	defer cancelLanding()
	squash, err := workspace.SquashValidated(landingCtx, finalSnapshot)
	closeErr := finalSnapshot.Close()
	if err != nil {
		execution.verdict = VerdictErrored
		execution.failure = fmt.Errorf("create landing commit: %w", err)
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	if closeErr != nil {
		execution.verdict = VerdictErrored
		execution.failure = errors.New("close immutable final-suite snapshot")
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	execution.landing.Commit = squash
	status, err := workspace.Land(landingCtx, squash)
	execution.landing.Status = string(status)
	if status == flywheel.LandingComplete {
		cleanupDisposition = flywheel.CleanupLanded
		if err != nil {
			execution.verdict = VerdictErrored
			execution.failure = fmt.Errorf("finish completed landing: %w", err)
			execution.landing.Error = execution.failure.Error()
			return execution
		}
		execution.verdict = VerdictUnsealed
		execution.failure = nil
		return execution
	}
	if err != nil || status == flywheel.LandingBlocked {
		execution.verdict = VerdictBlocked
		if err == nil {
			err = errors.New("landing was refused")
		}
		execution.failure = fmt.Errorf("guard landing: %w", err)
		execution.landing.Error = execution.failure.Error()
		execution.landing.PreservedBranch = "togi/run-" + active.runID
		return execution
	}
	execution.verdict = VerdictErrored
	execution.failure = fmt.Errorf("landing returned unexpected status %q", status)
	return execution
}

func (service Service) collectGates(ctx context.Context, active *RunLedger, requests []Request, root string, diff Diff) ([]GateReport, error) {
	prepared := make([]Request, len(requests))
	for index, request := range requests {
		prepared[index] = cloneExecutionRequest(request)
		prepared[index].Root = root
		if request.Gate.Manifest.Scope == "diff" {
			prepared[index].ChangedLines = diff.Lines
		}
		sink, err := active.RawSink(request.Gate.Manifest.Name, request.Binding.Language)
		if err != nil {
			return nil, fmt.Errorf("prepare raw sink: %w", err)
		}
		prepared[index].RawSink = sink
	}
	return Collect(ctx, service.Executor, prepared, min(runtime.NumCPU(), defaultMaximumWorkers)), nil
}

func (service Service) collectValidationGates(ctx context.Context, active *RunLedger, prepared preparedRun, gitRoot, executionRoot string, staged bool, requests []Request) ([]GateReport, error) {
	var diff Diff
	var err error
	if staged {
		diff, err = resolveStagedDiff(ctx, gitRoot, prepared.diff.MergeBase)
	} else {
		diff, err = resolveDiff(ctx, gitRoot, prepared.diff.MergeBase)
	}
	if err != nil {
		return nil, fmt.Errorf("recompute validation diff: %w", err)
	}
	return service.collectGates(ctx, active, requests, executionRoot, diff)
}

func resolveStagedDiff(ctx context.Context, root, mergeBase string) (Diff, error) {
	if ctx == nil {
		return Diff{}, errors.New("staged diff context is required")
	}
	canonicalRoot, err := canonicalDiffRoot(root)
	if err != nil {
		return Diff{}, err
	}
	base, err := resolveDiffCommit(ctx, canonicalRoot, mergeBase)
	if err != nil || base != mergeBase {
		return Diff{}, errors.New("resolve staged diff merge base")
	}
	treeRaw, err := diffGitOutput(ctx, canonicalRoot, gitReferenceOutputLimit, "write-tree")
	if err != nil {
		return Diff{}, errors.New("resolve staged tree")
	}
	tree, err := parseObjectID(treeRaw)
	if err != nil {
		return Diff{}, errors.New("parse staged tree")
	}
	pathArgs := []string{"diff", "--name-status", "--diff-filter=ACMRTUXB"}
	pathArgs = append(pathArgs, deterministicDiffOptions()...)
	pathArgs = append(pathArgs, "-z", mergeBase, tree, "--")
	pathOutput, err := diffGitOutput(ctx, canonicalRoot, gitPathsOutputLimit, pathArgs...)
	if err != nil {
		return Diff{}, errors.New("list staged changed paths")
	}
	paths, err := parseChangedPaths(canonicalRoot, pathOutput)
	if err != nil {
		return Diff{}, fmt.Errorf("parse staged changed paths: %w", err)
	}
	lines := make(finding.ChangedLines, len(paths))
	blobLineCounts := make(map[string]int)
	changedLines := 0
	for _, changed := range paths {
		args := []string{"diff", "--unified=0", "--no-color"}
		args = append(args, deterministicDiffOptions()...)
		args = append(args, mergeBase, tree, "--")
		if changed.previous != "" {
			args = append(args, literalPathspec(changed.previous))
		}
		args = append(args, literalPathspec(changed.current))
		patch, err := diffGitOutput(ctx, canonicalRoot, gitDiffOutputLimit, args...)
		if err != nil {
			return Diff{}, errors.New("read staged changed-line ranges")
		}
		ranges, err := parseDiffHunks(ctx, canonicalRoot, tree, changed.current, patch, blobLineCounts)
		if err != nil {
			return Diff{}, fmt.Errorf("parse staged changed-line ranges: %w", err)
		}
		lines[changed.current] = ranges
		for _, lineRange := range ranges {
			changedLines += lineRange.End - lineRange.Start + 1
		}
	}
	return Diff{BaseRef: mergeBase, BaseCommit: mergeBase, MergeBase: mergeBase, Head: tree, ChangedFiles: len(paths), ChangedLines: changedLines, Lines: lines}, nil
}

func (service Service) resolveFixIdentity(ctx context.Context, root string) (string, flywheel.Identity, error) {
	if service.ResolveFixIdentity != nil {
		return service.ResolveFixIdentity(ctx, root)
	}
	branchRaw, err := gitcmd.Output(ctx, root, gitcmd.Hermetic, 64<<10, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", flywheel.Identity{}, errors.New("resolve feature branch")
	}
	nameRaw, err := gitcmd.Output(ctx, root, gitcmd.HonourGlobal, 64<<10, "config", "--get", "user.name")
	if err != nil {
		return "", flywheel.Identity{}, errors.New("resolve Git user.name")
	}
	emailRaw, err := gitcmd.Output(ctx, root, gitcmd.HonourGlobal, 64<<10, "config", "--get", "user.email")
	if err != nil {
		return "", flywheel.Identity{}, errors.New("resolve Git user.email")
	}
	return strings.TrimSpace(string(branchRaw)), flywheel.Identity{Name: strings.TrimSpace(string(nameRaw)), Email: strings.TrimSpace(string(emailRaw))}, nil
}

func (service Service) resolveFeatureBranch(ctx context.Context, root string) (string, error) {
	if service.ResolveFeatureBranch != nil {
		return service.ResolveFeatureBranch(ctx, root)
	}
	branchRaw, err := gitcmd.Output(ctx, root, gitcmd.Hermetic, 64<<10, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", errors.New("resolve feature branch")
	}
	branch := strings.TrimSpace(string(branchRaw))
	if branch == "" {
		return "", errors.New("resolve feature branch")
	}
	return branch, nil
}

func composeFixReport(runID, repoID string, startedAt, finishedAt time.Time, diff Diff, execution fixExecution, rails *flywheel.Rails, agentName string) (Report, error) {
	base, err := ComposeReport(runID, repoID, startedAt, finishedAt, diff, execution.gates, execution.waivers)
	if err != nil {
		return Report{}, err
	}
	snapshot := rails.Snapshot()
	base.Verdict = execution.verdict
	base.Fix = &FixReport{
		OriginalHead: diff.Head, FeatureBranch: execution.branch,
		Agent: AgentReport{Name: agentName, Usage: cloneAdapterUsage(execution.usage)}, Baseline: cloneSuiteResult(execution.baseline), Final: cloneSuiteResultPointer(execution.final),
		Rails:   RailsReport{MaxIterations: snapshot.MaxIterations, Iterations: snapshot.Iterations, MaxWallClockMS: reportMilliseconds(snapshot.MaxWallClock), ElapsedMS: reportMilliseconds(snapshot.Elapsed)},
		Batches: cloneFixBatches(execution.outcome.Plan.Batches), Integrity: cloneReportFindings(execution.integrity), Landing: execution.landing,
	}
	if base.Fix.Batches == nil {
		base.Fix.Batches = []flywheel.Batch{}
	}
	if base.Fix.Integrity == nil {
		base.Fix.Integrity = []finding.Finding{}
	}
	if execution.failure != nil {
		base.Fix.Landing.Error = publicFixFailure(execution.verdict, execution.landing.Status)
	}
	if err := validateReport(base, runID); err != nil {
		return Report{}, fmt.Errorf("validate composed fix report: %w", err)
	}
	return base, nil
}

func reportMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	milliseconds := duration / time.Millisecond
	if duration%time.Millisecond != 0 {
		milliseconds++
	}
	return int64(milliseconds)
}

func cloneAdapterUsage(usage *adapter.Usage) *adapter.Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

func cloneSuiteResult(result SuiteResult) SuiteResult {
	result.Command = slices.Clone(result.Command)
	result.Packages = slices.Clone(result.Packages)
	return result
}

func cloneSuiteResultPointer(result *SuiteResult) *SuiteResult {
	if result == nil {
		return nil
	}
	cloned := cloneSuiteResult(*result)
	return &cloned
}

func cloneFixBatches(batches []flywheel.Batch) []flywheel.Batch {
	if batches == nil {
		return nil
	}
	cloned := make([]flywheel.Batch, len(batches))
	for index, batch := range batches {
		batch.Findings = cloneReportFindings(batch.Findings)
		batch.Attempts = slices.Clone(batch.Attempts)
		for attempt := range batch.Attempts {
			batch.Attempts[attempt].ChangedFiles = slices.Clone(batch.Attempts[attempt].ChangedFiles)
		}
		cloned[index] = batch
	}
	return cloned
}

func publicFixFailure(verdict Verdict, landingStatus string) string {
	if landingStatus == string(flywheel.LandingComplete) {
		return "landing completed but post-landing cleanup failed"
	}
	switch verdict {
	case VerdictUnverified:
		return "behavioral baseline is not verified"
	case VerdictBlocked:
		return "fix loop blocked"
	case VerdictRails:
		return "execution rail exhausted"
	default:
		return "fix loop infrastructure failed"
	}
}

func blockingFindings(reports []GateReport) []finding.Finding {
	var result []finding.Finding
	for _, report := range reports {
		for _, item := range report.Findings {
			if slices.Contains(report.Blocking, item.Severity) {
				result = append(result, item)
			}
		}
	}
	grouped, err := finding.Group(result)
	if err != nil {
		return nil
	}
	return grouped
}

func gatesErrored(reports []GateReport) bool { return len(erroredGateNames(reports)) != 0 }
func erroredGateNames(reports []GateReport) []string {
	var names []string
	for _, report := range reports {
		if report.Status == GateErrored {
			names = append(names, report.Gate)
		}
	}
	return names
}

func verdictForFlywheel(kind flywheel.OutcomeKind) Verdict {
	switch kind {
	case flywheel.OutcomeReady:
		return VerdictUnsealed
	case flywheel.OutcomeBlocked:
		return VerdictBlocked
	case flywheel.OutcomeRails:
		return VerdictRails
	default:
		return VerdictErrored
	}
}

func hasValidatedBatch(batches []flywheel.Batch) bool {
	for _, batch := range batches {
		if batch.Status == flywheel.BatchDone {
			return true
		}
	}
	return false
}
