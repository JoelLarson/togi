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
	Proof        BatchProof
}

// WorkspacePort is the flywheel's narrow ownership boundary for attempt state.
type WorkspacePort interface {
	Root() string
	SnapshotGitState(context.Context) (GitState, error)
	CheckGitState(context.Context, GitState) error
	ChangedFiles(context.Context) ([]string, error)
	PrepareBatch(context.Context, []string) (BatchProof, error)
	VerifyBatch(context.Context, BatchProof) error
	ResetAttempt(context.Context) error
	CommitBatch(context.Context, string, BatchProof) (string, error)
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

const (
	maxAttempts     = 2
	recoveryTimeout = 30 * time.Second
)

type engineState struct {
	audit            Audit
	plan             Plan
	persisted        Plan
	semanticFindings map[int][]finding.Finding
}

func newEngineState(audit Audit, plan Plan) *engineState {
	return &engineState{
		audit:            audit,
		plan:             plan,
		semanticFindings: make(map[int][]finding.Finding),
	}
}

func (state *engineState) outcomeFindings(additional []finding.Finding) []finding.Finding {
	combined := cloneFindings(additional)
	for index := range state.plan.Batches {
		combined = append(combined, cloneFindings(state.semanticFindings[index])...)
	}
	grouped, err := finding.Group(combined)
	if err != nil {
		return cloneFindings(combined)
	}
	return grouped
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
	state := newEngineState(ports.Audit, plan)
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
			result := executeBatch(ctx, request, ports, state, index)
			if result.outcome != nil {
				terminal := *result.outcome
				terminal.Findings = state.outcomeFindings(terminal.Findings)
				return terminal
			}
			if state.plan.Batches[index].Status == BatchStuck {
				waveStuck = true
			}
		}

		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			outcome.Findings = state.outcomeFindings(outcome.Findings)
			return outcome
		}
		if outcome, stopped := stopForRail(request.Rails, state.persisted); stopped {
			outcome.Findings = state.outcomeFindings(outcome.Findings)
			return outcome
		}
		barrier := ports.Barrier(ctx)
		outcomeFindings := state.outcomeFindings(barrier.Findings)
		if ctx.Err() != nil {
			if err := state.persist(); err != nil {
				return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "write barrier plan: "+err.Error())
			}
			outcome, _ := stopForContext(ctx, request.Rails, state.persisted)
			outcome.Findings = cloneFindings(outcomeFindings)
			return outcome
		}
		if railErr := request.Rails.AdmitLanding(); railErr != nil {
			if err := state.persist(); err != nil {
				return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "write barrier plan: "+err.Error())
			}
			kind := OutcomeErrored
			if errors.Is(railErr, ErrRailExhausted) {
				kind = OutcomeRails
			}
			return engineOutcome(kind, state.persisted, outcomeFindings, request.Rails, railErr.Error())
		}
		after, err := BlockingMultiset(barrier.Findings)
		if err != nil {
			if writeErr := state.persist(); writeErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "write barrier plan: "+writeErr.Error())
			}
			return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "classify barrier blockers: "+err.Error())
		}
		if err := validateBarrierResult(barrier); err != nil {
			if writeErr := state.persist(); writeErr != nil {
				return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "write barrier plan: "+writeErr.Error())
			}
			return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "invalid barrier validation: "+err.Error())
		}
		if err := state.persist(); err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "write barrier plan: "+err.Error())
		}
		if barrier.Kind == ValidationInfrastructureFailure {
			return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, barrier.Failure)
		}
		if barrier.Kind == ValidationSemanticFailure {
			return engineOutcome(OutcomeBlocked, state.persisted, outcomeFindings, request.Rails, barrier.Failure)
		}
		if waveStuck {
			return engineOutcome(OutcomeBlocked, state.persisted, outcomeFindings, request.Rails, "one or more batches are stuck")
		}
		if len(after) == 0 {
			return engineOutcome(OutcomeReady, state.persisted, nil, request.Rails, "")
		}
		if !StrictlyShrinks(after, blockers) {
			return engineOutcome(OutcomeBlocked, state.persisted, outcomeFindings, request.Rails, "blocking findings did not strictly shrink")
		}

		next, err := NewPlan(barrier.Findings)
		if err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "create next plan wave: "+err.Error())
		}
		waveStart = len(state.plan.Batches)
		state.plan.Batches = append(state.plan.Batches, next.Batches...)
		blockers = after
		if err := state.persist(); err != nil {
			return engineOutcome(OutcomeErrored, state.persisted, outcomeFindings, request.Rails, "write next plan wave: "+err.Error())
		}
	}
}

type executionResult struct {
	outcome *Outcome
}

func continuedExecution() executionResult {
	return executionResult{}
}

func stoppedExecution(outcome Outcome) executionResult {
	return executionResult{outcome: &outcome}
}

type attemptResultKind uint8

const (
	attemptSucceeded attemptResultKind = iota
	attemptSemanticFailure
	attemptInfrastructureFailure
	attemptStopped
)

type attemptResult struct {
	kind            attemptResultKind
	failure         string
	findings        []finding.Finding
	retryable       bool
	stopIfCancelled bool
	outcome         Outcome
}

type batchAttempt struct {
	request      Request
	ports        Ports
	state        *engineState
	index        int
	number       int
	retryFailure string
}

func executeBatch(ctx context.Context, request Request, ports Ports, state *engineState, index int) executionResult {
	retryFailure := ""
	for number := 1; number <= maxAttempts; number++ {
		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			return stoppedExecution(outcome)
		}
		if err := request.Rails.AdmitAttempt(); err != nil {
			kind := OutcomeErrored
			if errors.Is(err, ErrRailExhausted) {
				kind = OutcomeRails
			}
			return stoppedExecution(engineOutcome(kind, state.persisted, nil, request.Rails, err.Error()))
		}
		delete(state.semanticFindings, index)
		if outcome, stopped := stopForContext(ctx, request.Rails, state.persisted); stopped {
			return stoppedExecution(outcome)
		}

		attempt := batchAttempt{request: request, ports: ports, state: state, index: index, number: number, retryFailure: retryFailure}
		result := attempt.run(ctx)
		switch result.kind {
		case attemptSucceeded:
			return continuedExecution()
		case attemptStopped:
			return stoppedExecution(result.outcome)
		case attemptSemanticFailure:
			if outcome := attempt.abortSemantic(ctx, result.failure); outcome != nil {
				return stoppedExecution(*outcome)
			}
			if result.stopIfCancelled && ctx.Err() != nil {
				return stoppedExecution(engineOutcome(OutcomeErrored, state.persisted, nil, request.Rails, result.failure))
			}
			if number == maxAttempts {
				return continuedExecution()
			}
		case attemptInfrastructureFailure:
			failure := result.failure
			if resetErr := attempt.failAndReset(ctx, failure); resetErr != nil {
				failure = resetErr.Error()
				return stoppedExecution(engineOutcome(OutcomeErrored, state.persisted, result.findings, request.Rails, failure))
			}
			if !result.retryable || number == maxAttempts {
				return stoppedExecution(engineOutcome(OutcomeErrored, state.persisted, result.findings, request.Rails, failure))
			}
		}
		retryFailure = result.failure
	}
	return continuedExecution()
}

func (attempt batchAttempt) run(ctx context.Context) attemptResult {
	batch := &attempt.state.plan.Batches[attempt.index]
	batch.Status = BatchRunning
	batch.Attempts = append(batch.Attempts, Attempt{Number: attempt.number, Status: AttemptRunning})
	if err := attempt.state.persist(); err != nil {
		return attempt.infrastructureFailure("write running plan: "+err.Error(), false, nil)
	}
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}

	brief, err := BuildBrief(BriefInput{MergeBase: attempt.request.MergeBase, OriginalHead: attempt.request.OriginalHead, Batch: *batch, RetryFailure: attempt.retryFailure})
	if err != nil {
		return attempt.infrastructureFailure("build brief: "+err.Error(), false, nil)
	}
	if err := attempt.ports.Audit.WriteBrief(batch.ID, attempt.number, []byte(brief)); err != nil {
		return attempt.infrastructureFailure("write brief: "+err.Error(), false, nil)
	}
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}

	sink, err := attempt.ports.Audit.AdapterSink(batch.ID, attempt.number)
	if err != nil {
		return attempt.infrastructureFailure("allocate adapter sink: "+err.Error(), false, nil)
	}
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}

	gitState, err := attempt.ports.Workspace.SnapshotGitState(ctx)
	// A stop takes precedence because SnapshotGitState may have crossed a rail
	// while touching the workspace, even when it also returned an error.
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}
	if err != nil {
		return attempt.infrastructureFailure("snapshot Git state: "+err.Error(), false, nil)
	}

	_, adapterRunErr := attempt.ports.Adapter.Run(ctx, adapter.Request{Root: attempt.ports.Workspace.Root(), Brief: brief, Sink: sink})
	recoveryCtx, cancelRecovery := recoveryContext(ctx)
	integrityErr := attempt.ports.Workspace.CheckGitState(recoveryCtx, gitState)
	cancelRecovery()
	if integrityErr != nil {
		failure := boundedFailure("check Git state: " + integrityErr.Error())
		if !errors.Is(integrityErr, ErrGitStateRestored) {
			return attempt.infrastructureFailure(failure, false, nil)
		}
		return attemptResult{kind: attemptSemanticFailure, failure: failure, stopIfCancelled: true}
	}
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}
	if adapterRunErr != nil {
		var classified *adapter.Error
		retryable := errors.As(adapterRunErr, &classified) && classified.Retryable
		return attempt.infrastructureFailure("adapter: "+adapterRunErr.Error(), retryable, nil)
	}

	changed, err := attempt.ports.Workspace.ChangedFiles(ctx)
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}
	if err != nil {
		return attempt.infrastructureFailure("inspect changed files: "+err.Error(), true, nil)
	}
	if len(changed) == 0 {
		return attemptResult{kind: attemptSemanticFailure, failure: "agent produced no worktree changes"}
	}

	proof, err := attempt.ports.Workspace.PrepareBatch(ctx, changed)
	if stopped := attempt.checkpointContext(ctx); stopped.kind == attemptStopped {
		return stopped
	}
	if err != nil {
		return attempt.infrastructureFailure("prepare batch proof: "+err.Error(), true, nil)
	}

	validationBatch := cloneBatch(*batch)
	validationBatch.proof = cloneBatchProof(proof)
	validation := attempt.ports.Validate(ctx, validationBatch)
	// Validation is followed by proof verification so validator-side workspace
	// mutation cannot be committed using evidence prepared before validation.
	recoveryCtx, cancelRecovery = recoveryContext(ctx)
	proofErr := attempt.ports.Workspace.VerifyBatch(recoveryCtx, proof)
	cancelRecovery()
	if proofErr != nil {
		validation = ValidationResult{Kind: ValidationInfrastructureFailure, Failure: boundedFailure("verify batch proof: " + proofErr.Error()), ChangedFiles: append([]string(nil), changed...)}
	}
	if proofErr != nil && ctx.Err() != nil {
		return attempt.infrastructureFailure(validation.Failure, false, nil)
	}
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}
	if proofErr == nil && validation.Kind == ValidationPassed {
		validation.Proof = cloneBatchProof(proof)
	} else {
		validation.Proof = BatchProof{}
	}
	if _, err := BlockingMultiset(validation.Findings); err != nil {
		return attempt.infrastructureFailure("classify validation findings: "+err.Error(), false, nil)
	}
	if err := validateAttemptResult(validation); err != nil {
		return attempt.infrastructureFailure("invalid batch validation: "+err.Error(), false, nil)
	}
	switch validation.Kind {
	case ValidationSemanticFailure:
		attempt.state.semanticFindings[attempt.index] = cloneFindings(validation.Findings)
		return attemptResult{kind: attemptSemanticFailure, failure: boundedFailure(validation.Failure)}
	case ValidationInfrastructureFailure:
		return attempt.infrastructureFailure(validation.Failure, true, validation.Findings)
	case ValidationPassed:
		delete(attempt.state.semanticFindings, attempt.index)
	}
	if stopped := attempt.checkpoint(ctx); stopped.kind == attemptStopped {
		return stopped
	}

	commit, err := attempt.ports.Workspace.CommitBatch(ctx, batch.PrimaryFile, validation.Proof)
	if err != nil {
		if stopped := attempt.checkpointContext(ctx); stopped.kind == attemptStopped {
			return stopped
		}
		return attempt.infrastructureFailure("commit batch: "+err.Error(), true, nil)
	}
	batch.Attempts[len(batch.Attempts)-1] = Attempt{Number: attempt.number, Status: AttemptPassed, ChangedFiles: append([]string(nil), changed...), Commit: commit}
	batch.Status = BatchDone
	if err := attempt.state.persist(); err != nil {
		// A published plan already claims the commit. Otherwise compensate for
		// the failed durable transition by rolling the workspace commit back.
		if !planWasPublished(err) {
			recoveryCtx, cancelRecovery := recoveryContext(ctx)
			rollbackErr := attempt.ports.Workspace.RollbackBatch(recoveryCtx, commit)
			cancelRecovery()
			if rollbackErr != nil {
				return attempt.stopped(OutcomeErrored, "write completed plan: "+err.Error()+"; rollback committed batch: "+rollbackErr.Error())
			}
		}
		return attempt.stopped(OutcomeErrored, "write completed plan: "+err.Error())
	}
	if outcome, stopped := stopForContext(ctx, attempt.request.Rails, attempt.state.persisted); stopped {
		return attemptResult{kind: attemptStopped, outcome: outcome}
	}
	if outcome, stopped := stopForRail(attempt.request.Rails, attempt.state.persisted); stopped {
		return attemptResult{kind: attemptStopped, outcome: outcome}
	}
	return attemptResult{kind: attemptSucceeded}
}

func (attempt batchAttempt) checkpoint(ctx context.Context) attemptResult {
	if stopped := attempt.checkpointContext(ctx); stopped.kind == attemptStopped {
		return stopped
	}
	err := attempt.request.Rails.AdmitLanding()
	if err == nil {
		return attemptResult{kind: attemptSucceeded}
	}
	failure := boundedFailure(err.Error())
	if resetErr := attempt.failAndReset(ctx, failure); resetErr != nil {
		return attempt.stopped(OutcomeErrored, resetErr.Error())
	}
	kind := OutcomeErrored
	if errors.Is(err, ErrRailExhausted) {
		kind = OutcomeRails
	}
	return attempt.stopped(kind, failure)
}

func (attempt batchAttempt) checkpointContext(ctx context.Context) attemptResult {
	if ctx.Err() == nil {
		return attemptResult{kind: attemptSucceeded}
	}
	if resetErr := attempt.failAndReset(ctx, ctx.Err().Error()); resetErr != nil {
		return attempt.stopped(OutcomeErrored, resetErr.Error())
	}
	outcome, _ := stopForContext(ctx, attempt.request.Rails, attempt.state.persisted)
	return attemptResult{kind: attemptStopped, outcome: outcome}
}

func (attempt batchAttempt) abortSemantic(ctx context.Context, failure string) *Outcome {
	if resetErr := attempt.failAndReset(ctx, failure); resetErr != nil {
		outcome := engineOutcome(OutcomeErrored, attempt.state.persisted, nil, attempt.request.Rails, resetErr.Error())
		return &outcome
	}
	if attempt.number == maxAttempts {
		attempt.state.plan.Batches[attempt.index].Status = BatchStuck
		if err := attempt.state.persist(); err != nil {
			outcome := engineOutcome(OutcomeErrored, attempt.state.persisted, nil, attempt.request.Rails, "write stuck plan: "+err.Error())
			return &outcome
		}
	}
	return nil
}

func (attempt batchAttempt) infrastructureFailure(failure string, retryable bool, findings []finding.Finding) attemptResult {
	return attemptResult{
		kind:      attemptInfrastructureFailure,
		failure:   boundedFailure(failure),
		findings:  cloneFindings(findings),
		retryable: retryable,
	}
}

func (attempt batchAttempt) stopped(kind OutcomeKind, failure string) attemptResult {
	return attemptResult{kind: attemptStopped, outcome: engineOutcome(kind, attempt.state.persisted, nil, attempt.request.Rails, failure)}
}

func (attempt batchAttempt) failAndReset(ctx context.Context, failure string) error {
	failure = boundedFailure(failure)
	batch := &attempt.state.plan.Batches[attempt.index]
	batch.Attempts[len(batch.Attempts)-1] = Attempt{Number: attempt.number, Status: AttemptFailed, Failure: failure}
	if err := attempt.state.persist(); err != nil {
		recoveryCtx, cancelRecovery := recoveryContext(ctx)
		resetErr := attempt.ports.Workspace.ResetAttempt(recoveryCtx)
		cancelRecovery()
		if resetErr != nil {
			return fmt.Errorf("write failed plan: %v; reset attempt: %w", err, resetErr)
		}
		return fmt.Errorf("write failed plan: %w", err)
	}
	recoveryCtx, cancelRecovery := recoveryContext(ctx)
	resetErr := attempt.ports.Workspace.ResetAttempt(recoveryCtx)
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

// Audit implementations may report that a failed write published the plan,
// allowing commit compensation to preserve the now-durable state.
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
		if !result.Proof.present() {
			return errors.New("passed result lacks a prepared batch proof")
		}
	case ValidationSemanticFailure:
		if result.Failure == "" {
			return errors.New("semantic failure requires a reason")
		}
		if result.Proof.present() {
			return errors.New("semantic failure includes a batch proof")
		}
	case ValidationInfrastructureFailure:
		if result.Failure == "" {
			return errors.New("infrastructure failure requires a reason")
		}
		if result.Proof.present() {
			return errors.New("infrastructure failure includes a batch proof")
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
			rails.ObserveExecutionContext(ctx)
			kind = OutcomeRails
			failure = cause.Error()
		} else if railExhaustedNow(rails) {
			kind = OutcomeRails
		}
		return engineOutcome(kind, persisted, nil, rails, failure), true
	}
	return Outcome{}, false
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

func railExhaustedNow(rails *Rails) bool {
	if rails == nil {
		return false
	}
	return errors.Is(rails.AdmitLanding(), ErrRailExhausted)
}

func railExecutionContext(parent context.Context, rails *Rails) (context.Context, context.CancelFunc, error) {
	return rails.ExecutionContext(parent)
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
