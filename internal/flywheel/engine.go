package flywheel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/adapter"
	"github.com/joellarson/togi/internal/finding"
)

// Audit persists private flywheel artifacts outside the target repository.
type Audit interface {
	WritePlan([]byte) error
	WriteBrief(batchID string, attempt int, raw []byte) error
	AdapterSink(batchID string, attempt int) (adapter.Sink, error)
}

// ValidationKind classifies validation evidence without coupling the flywheel
// to a particular gate or integrity implementation.
type ValidationKind string

const (
	ValidationPassed                ValidationKind = "passed"
	ValidationSemanticFailure       ValidationKind = "semantic-failure"
	ValidationInfrastructureFailure ValidationKind = "infrastructure-failure"
)

// ValidationResult is the bounded result of batch or barrier validation.
type ValidationResult struct {
	Kind         ValidationKind
	Failure      string
	Findings     []finding.Finding
	ChangedFiles []string
}

// WorkspacePort is the flywheel's narrow ownership boundary for attempt state.
type WorkspacePort interface {
	Root() string
	SnapshotGitState(context.Context) (GitState, error)
	CheckGitState(context.Context, GitState) error
	ChangedFiles(context.Context) ([]string, error)
	ResetAttempt(context.Context) error
	CommitBatch(context.Context, string) (string, error)
	RollbackBatch(context.Context, string) error
}

// Ports supplies the side effects used by deterministic orchestration.
type Ports struct {
	Adapter   adapter.Adapter
	Workspace WorkspacePort
	Audit     Audit
	Validate  func(context.Context, Batch) ValidationResult
	Barrier   func(context.Context) ValidationResult
}

// Request contains immutable run facts and its shared execution rails.
type Request struct {
	MergeBase       string
	OriginalHead    string
	InitialFindings []finding.Finding
	Rails           *Rails
}

// OutcomeKind is the terminal classification of flywheel execution.
type OutcomeKind string

const (
	OutcomeReady   OutcomeKind = "ready"
	OutcomeBlocked OutcomeKind = "blocked"
	OutcomeRails   OutcomeKind = "rails"
	OutcomeErrored OutcomeKind = "errored"
)

// Outcome is the complete deterministic engine result.
type Outcome struct {
	Kind       OutcomeKind
	Plan       Plan
	Findings   []finding.Finding
	Iterations int
	Failure    string
}

const failureTruncationMarker = "\n...[truncated]"

const recoveryTimeout = 30 * time.Second

type engineState struct {
	audit     Audit
	plan      Plan
	persisted Plan
}

func (state *engineState) persist() error {
	if err := persistPlan(state.audit, state.plan); err != nil {
		if planWasPublished(err) {
			state.persisted = clonePlan(state.plan)
		}
		return err
	}
	state.persisted = clonePlan(state.plan)
	return nil
}

// Execute runs fix batches serially until the barrier is clean or execution
// reaches a domain terminal state.
func Execute(ctx context.Context, request Request, ports Ports) Outcome {
	if ctx == nil {
		return engineOutcome(OutcomeErrored, Plan{}, nil, nil, "context is required")
	}
	if failure := validateEngineInput(request, ports); failure != "" {
		return engineOutcome(OutcomeErrored, Plan{}, nil, request.Rails, failure)
	}
	if outcome, stopped := stopForContext(ctx, request.Rails, Plan{}); stopped {
		return outcome
	}
	if outcome, stopped := stopForRail(request.Rails, Plan{}); stopped {
		return outcome
	}
	executionCtx, cancelExecution, err := railExecutionContext(ctx, request.Rails)
	if err != nil {
		return engineOutcome(OutcomeRails, Plan{}, nil, request.Rails, err.Error())
	}
	defer cancelExecution()
	ctx = executionCtx

	plan, err := NewPlan(request.InitialFindings)
	if err != nil {
		return engineOutcome(OutcomeErrored, Plan{}, nil, request.Rails, "create initial plan: "+err.Error())
	}
	blockers, err := BlockingMultiset(request.InitialFindings)
	if err != nil {
		return engineOutcome(OutcomeErrored, Plan{}, nil, request.Rails, "classify initial blockers: "+err.Error())
	}
	state := &engineState{audit: ports.Audit, plan: plan}
	if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
		return outcome
	}
	if outcome, stopped := stopForRail(request.Rails, state.persisted); stopped {
		return outcome
	}
	if err := state.persist(); err != nil {
		return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, "write plan: "+err.Error())
	}

	waveStart := 0
	for {
		waveEnd := len(state.plan.Batches)
		waveStuck := false
		for index := waveStart; index < waveEnd; index++ {
			terminal, stopped := executeBatch(ctx, request, ports, state, index)
			if stopped {
				return terminal
			}
			if state.plan.Batches[index].Status == BatchStuck {
				waveStuck = true
			}
		}

		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			return outcome
		}
		if outcome, stopped := stopForRail(request.Rails, state.persisted); stopped {
			return outcome
		}
		barrier := ports.Barrier(ctx)
		if ctx.Err() != nil {
			if err := state.persist(); err != nil {
				return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "write barrier plan: "+err.Error())
			}
			outcome, _ := stopForContext(ctx, request.Rails, state.persisted)
			outcome.Findings = cloneFindings(barrier.Findings)
			return outcome
		}
		if railErr := request.Rails.AdmitLanding(); railErr != nil {
			if err := state.persist(); err != nil {
				return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "write barrier plan: "+err.Error())
			}
			kind := OutcomeErrored
			if errors.Is(railErr, ErrRailExhausted) {
				kind = OutcomeRails
			}
			return engineOutcome(kind, state.persisted, barrier.Findings, request.Rails, railErr.Error())
		}
		after, err := BlockingMultiset(barrier.Findings)
		if err != nil {
			if writeErr := state.persist(); writeErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "write barrier plan: "+writeErr.Error())
			}
			return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "classify barrier blockers: "+err.Error())
		}
		if err := validateBarrierResult(barrier); err != nil {
			if writeErr := state.persist(); writeErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "write barrier plan: "+writeErr.Error())
			}
			return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "invalid barrier validation: "+err.Error())
		}
		if err := state.persist(); err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "write barrier plan: "+err.Error())
		}
		if barrier.Kind == ValidationInfrastructureFailure {
			return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, barrier.Failure)
		}
		if barrier.Kind == ValidationSemanticFailure {
			return engineOutcome(OutcomeBlocked, state.persisted, barrier.Findings, request.Rails, barrier.Failure)
		}
		if waveStuck {
			return engineOutcome(OutcomeBlocked, state.persisted, barrier.Findings, request.Rails, "one or more batches are stuck")
		}
		if len(after) == 0 {
			return engineOutcome(OutcomeReady, state.persisted, nil, request.Rails, "")
		}
		if !StrictlyShrinks(after, blockers) {
			return engineOutcome(OutcomeBlocked, state.persisted, barrier.Findings, request.Rails, "blocking findings did not strictly shrink")
		}

		next, err := NewPlan(barrier.Findings)
		if err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "create next plan wave: "+err.Error())
		}
		waveStart = len(state.plan.Batches)
		state.plan.Batches = append(state.plan.Batches, next.Batches...)
		blockers = after
		if err := state.persist(); err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, barrier.Findings, request.Rails, "write next plan wave: "+err.Error())
		}
	}
}

func executeBatch(ctx context.Context, request Request, ports Ports, state *engineState, index int) (Outcome, bool) {
	retryFailure := ""
	for attempt := 1; attempt <= 2; attempt++ {
		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			return outcome, true
		}
		if err := request.Rails.AdmitAttempt(); err != nil {
			kind := OutcomeErrored
			if errors.Is(err, ErrRailExhausted) {
				kind = OutcomeRails
			}
			return engineOutcome(kind, state.persisted, nil, request.Rails, err.Error()), true
		}
		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			return outcome, true
		}

		batch := &state.plan.Batches[index]
		batch.Status = BatchRunning
		batch.Attempts = append(batch.Attempts, Attempt{Number: attempt, Status: "running"})
		if err := state.persist(); err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "write running plan: "+err.Error())
		}
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		brief, err := BuildBrief(BriefInput{MergeBase: request.MergeBase, OriginalHead: request.OriginalHead, Batch: *batch, RetryFailure: retryFailure})
		if err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "build brief: "+err.Error())
		}
		if err := ports.Audit.WriteBrief(batch.ID, attempt, []byte(brief)); err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "write brief: "+err.Error())
		}
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		sink, err := ports.Audit.AdapterSink(batch.ID, attempt)
		if err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "allocate adapter sink: "+err.Error())
		}
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		gitState, err := ports.Workspace.SnapshotGitState(ctx)
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "snapshot Git state: "+err.Error())
		}
		_, adapterRunErr := ports.Adapter.Run(ctx, adapter.Request{Root: ports.Workspace.Root(), Brief: brief, Sink: sink})
		recoveryCtx, cancelRecovery := recoveryContext(ctx)
		integrityErr := ports.Workspace.CheckGitState(recoveryCtx, gitState)
		cancelRecovery()
		if integrityErr != nil {
			failure := boundedFailure("check Git state: " + integrityErr.Error())
			if !errors.Is(integrityErr, ErrGitStateRestored) {
				return failedInfrastructure(ctx, request, ports, state, index, attempt, failure)
			}
			if terminal, stopped := semanticFailure(ctx, request, ports, state, index, attempt, failure); stopped {
				return terminal, true
			}
			if ctx.Err() != nil {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, failure), true
			}
			if attempt == 2 {
				return Outcome{}, false
			}
			retryFailure = failure
			continue
		}
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if adapterRunErr != nil {
			retryable := false
			var classified *adapter.Error
			retryable = errors.As(adapterRunErr, &classified) && classified.Retryable
			failure := boundedFailure("adapter: " + adapterRunErr.Error())
			if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, resetErr.Error()), true
			}
			if !retryable || attempt == 2 {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, failure), true
			}
			retryFailure = failure
			continue
		}

		changed, err := ports.Workspace.ChangedFiles(ctx)
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if err != nil {
			failure := boundedFailure("inspect changed files: " + err.Error())
			if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, resetErr.Error()), true
			}
			if attempt == 2 {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, failure), true
			}
			retryFailure = failure
			continue
		}
		if len(changed) == 0 {
			failure := "agent produced no worktree changes"
			if terminal, stopped := semanticFailure(ctx, request, ports, state, index, attempt, failure); stopped {
				return terminal, true
			}
			if attempt == 2 {
				return Outcome{}, false
			}
			retryFailure = failure
			continue
		}

		validation := ports.Validate(ctx, cloneBatch(*batch))
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if _, err := BlockingMultiset(validation.Findings); err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "classify validation findings: "+err.Error())
		}
		if err := validateAttemptResult(validation); err != nil {
			return failedInfrastructure(ctx, request, ports, state, index, attempt, "invalid batch validation: "+err.Error())
		}
		switch validation.Kind {
		case ValidationSemanticFailure:
			failure := boundedFailure(validation.Failure)
			if terminal, stopped := semanticFailure(ctx, request, ports, state, index, attempt, failure); stopped {
				return terminal, true
			}
			if attempt == 2 {
				return Outcome{}, false
			}
			retryFailure = failure
			continue
		case ValidationInfrastructureFailure:
			failure := boundedFailure(validation.Failure)
			if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, validation.Findings, request.Rails, resetErr.Error()), true
			}
			if attempt == 2 {
				return engineOutcome(OutcomeErrored, state.persisted, validation.Findings, request.Rails, failure), true
			}
			retryFailure = failure
			continue
		case ValidationPassed:
		}
		if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}
		if outcome, stopped := stopAttemptForRail(ctx, request, ports, state, index, attempt); stopped {
			return outcome, true
		}

		commit, err := ports.Workspace.CommitBatch(ctx, batch.PrimaryFile)
		if err != nil {
			if outcome, stopped := stopAttemptForContext(ctx, request, ports, state, index, attempt); stopped {
				return outcome, true
			}
			failure := boundedFailure("commit batch: " + err.Error())
			if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, resetErr.Error()), true
			}
			if attempt == 2 {
				return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, failure), true
			}
			retryFailure = failure
			continue
		}
		batch.Attempts[len(batch.Attempts)-1] = Attempt{Number: attempt, Status: "passed", ChangedFiles: append([]string(nil), changed...), Commit: commit}
		batch.Status = BatchDone
		if err := state.persist(); err != nil {
			if !planWasPublished(err) {
				recoveryCtx, cancelRecovery := recoveryContext(ctx)
				rollbackErr := ports.Workspace.RollbackBatch(recoveryCtx, commit)
				cancelRecovery()
				if rollbackErr != nil {
					return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, "write completed plan: "+err.Error()+"; rollback committed batch: "+rollbackErr.Error()), true
				}
			}
			return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, "write completed plan: "+err.Error()), true
		}
		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			return outcome, true
		}
		if outcome, stopped := stopForRail(request.Rails, state.persisted); stopped {
			return outcome, true
		}
		return Outcome{}, false
	}
	panic("unreachable")
}

func semanticFailure(ctx context.Context, request Request, ports Ports, state *engineState, index, attempt int, failure string) (Outcome, bool) {
	if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
		return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, resetErr.Error()), true
	}
	if attempt == 2 {
		state.plan.Batches[index].Status = BatchStuck
		if err := state.persist(); err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, "write stuck plan: "+err.Error()), true
		}
	}
	return Outcome{}, false
}

func failedInfrastructure(ctx context.Context, request Request, ports Ports, state *engineState, index, attempt int, failure string) (Outcome, bool) {
	if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
		failure = resetErr.Error()
	}
	return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, failure), true
}

func failAndReset(ctx context.Context, ports Ports, state *engineState, index, attempt int, failure string) error {
	failure = boundedFailure(failure)
	batch := &state.plan.Batches[index]
	batch.Attempts[len(batch.Attempts)-1] = Attempt{Number: attempt, Status: "failed", Failure: failure}
	if err := state.persist(); err != nil {
		recoveryCtx, cancelRecovery := recoveryContext(ctx)
		resetErr := ports.Workspace.ResetAttempt(recoveryCtx)
		cancelRecovery()
		if resetErr != nil {
			return fmt.Errorf("write failed plan: %v; reset attempt: %w", err, resetErr)
		}
		return fmt.Errorf("write failed plan: %w", err)
	}
	recoveryCtx, cancelRecovery := recoveryContext(ctx)
	resetErr := ports.Workspace.ResetAttempt(recoveryCtx)
	cancelRecovery()
	if resetErr != nil {
		return fmt.Errorf("reset attempt: %w", resetErr)
	}
	return nil
}

func persistPlan(audit Audit, plan Plan) error {
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return audit.WritePlan(raw)
}

func planWasPublished(err error) bool {
	var published interface{ PlanPublished() bool }
	return errors.As(err, &published) && published.PlanPublished()
}

func validateAttemptResult(result ValidationResult) error {
	switch result.Kind {
	case ValidationPassed:
		if result.Failure != "" {
			return errors.New("passed result includes a failure")
		}
		if len(result.Findings) != 0 {
			return errors.New("passed result includes blocking findings")
		}
	case ValidationSemanticFailure:
		if result.Failure == "" {
			return errors.New("semantic failure requires a reason")
		}
	case ValidationInfrastructureFailure:
		if result.Failure == "" {
			return errors.New("infrastructure failure requires a reason")
		}
	default:
		return fmt.Errorf("unknown validation kind %q", result.Kind)
	}
	return nil
}

func validateBarrierResult(result ValidationResult) error {
	switch result.Kind {
	case ValidationPassed:
		if result.Failure != "" {
			return errors.New("passed result includes a failure")
		}
	case ValidationSemanticFailure:
		if result.Failure == "" {
			return errors.New("semantic failure requires a reason")
		}
	case ValidationInfrastructureFailure:
		if result.Failure == "" {
			return errors.New("infrastructure failure requires a reason")
		}
	default:
		return fmt.Errorf("unknown validation kind %q", result.Kind)
	}
	return nil
}

func boundedFailure(failure string) string {
	failure = strings.ToValidUTF8(failure, "\uFFFD")
	if len(failure) <= maxBriefDiagnosticFieldBytes {
		return failure
	}
	limit := maxBriefDiagnosticFieldBytes - len(failureTruncationMarker)
	for limit > 0 && !utf8.RuneStart(failure[limit]) {
		limit--
	}
	return failure[:limit] + failureTruncationMarker
}

func stopForContext(ctx context.Context, rails *Rails, persisted Plan) (Outcome, bool) {
	if ctx == nil {
		return engineOutcome(OutcomeErrored, persisted, nil, rails, "context is required"), true
	}
	if err := ctx.Err(); err != nil {
		kind := OutcomeErrored
		failure := err.Error()
		if cause := context.Cause(ctx); errors.Is(cause, ErrRailExhausted) {
			kind = OutcomeRails
			failure = cause.Error()
		} else if railExhaustedNow(rails) {
			kind = OutcomeRails
		}
		return engineOutcome(kind, persisted, nil, rails, failure), true
	}
	return Outcome{}, false
}

func stopAttemptForContext(ctx context.Context, request Request, ports Ports, state *engineState, index, attempt int) (Outcome, bool) {
	if ctx.Err() == nil {
		return Outcome{}, false
	}
	failure := ctx.Err().Error()
	if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
		return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, resetErr.Error()), true
	}
	return stopForContext(ctx, request.Rails, state.persisted)
}

func stopForRail(rails *Rails, persisted Plan) (Outcome, bool) {
	err := rails.AdmitLanding()
	if err == nil {
		return Outcome{}, false
	}
	kind := OutcomeErrored
	if errors.Is(err, ErrRailExhausted) {
		kind = OutcomeRails
	}
	return engineOutcome(kind, persisted, nil, rails, err.Error()), true
}

func stopAttemptForRail(ctx context.Context, request Request, ports Ports, state *engineState, index, attempt int) (Outcome, bool) {
	err := request.Rails.AdmitLanding()
	if err == nil {
		return Outcome{}, false
	}
	failure := boundedFailure(err.Error())
	if resetErr := failAndReset(ctx, ports, state, index, attempt, failure); resetErr != nil {
		return engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, resetErr.Error()), true
	}
	kind := OutcomeErrored
	if errors.Is(err, ErrRailExhausted) {
		kind = OutcomeRails
	}
	return engineOutcome(kind, state.persisted, nil, request.Rails, failure), true
}

func railExhaustedNow(rails *Rails) bool {
	if rails == nil {
		return false
	}
	return errors.Is(rails.AdmitLanding(), ErrRailExhausted)
}

func railExecutionContext(parent context.Context, rails *Rails) (context.Context, context.CancelFunc, error) {
	snapshot := rails.Snapshot()
	remaining := snapshot.MaxWallClock - snapshot.Elapsed
	if remaining <= 0 {
		return nil, nil, &RailExhaustedError{Rail: RailWallClock}
	}
	ctx, cancel := context.WithTimeoutCause(parent, remaining, &RailExhaustedError{Rail: RailWallClock})
	return ctx, cancel, nil
}

func recoveryContext(parent context.Context) (context.Context, context.CancelFunc) {
	return recoveryContextWithTimeout(parent, recoveryTimeout)
}

func recoveryContextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func validateEngineInput(request Request, ports Ports) string {
	switch {
	case !validRevision(request.MergeBase):
		return "merge base must be a 40- or 64-character hexadecimal object ID"
	case !validRevision(request.OriginalHead):
		return "original HEAD must be a 40- or 64-character hexadecimal object ID"
	case request.Rails == nil:
		return "rails are required"
	case ports.Adapter == nil:
		return "adapter is required"
	case ports.Workspace == nil:
		return "workspace is required"
	case ports.Audit == nil:
		return "audit is required"
	case ports.Validate == nil:
		return "batch validator is required"
	case ports.Barrier == nil:
		return "barrier validator is required"
	default:
		return ""
	}
}

func engineOutcome(kind OutcomeKind, plan Plan, findings []finding.Finding, rails *Rails, failure string) Outcome {
	iterations := 0
	if rails != nil {
		iterations = rails.Snapshot().Iterations
	}
	return Outcome{Kind: kind, Plan: clonePlan(plan), Findings: cloneFindings(findings), Iterations: iterations, Failure: boundedFailure(failure)}
}
