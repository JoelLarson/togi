package flywheel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/adapter"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
)

func TestNewEngineStateInitializesSemanticFindings(t *testing.T) {
	audit := &engineAudit{}
	plan := Plan{SchemaVersion: 1}
	state := newEngineState(audit, plan)
	if state.audit != audit || state.plan.SchemaVersion != 1 || state.semanticFindings == nil {
		t.Fatalf("state = %#v", state)
	}
}

func TestAttemptResultRetainsClassification(t *testing.T) {
	want := Outcome{Kind: OutcomeRails, Failure: "rail"}
	wantFinding := planFinding("a.go", 1, "lint/a", "a")
	result := attemptResult{
		kind:      attemptStopped,
		outcome:   want,
		failure:   "failure",
		findings:  []finding.Finding{wantFinding},
		retryable: true,
	}
	if result.kind != attemptStopped || result.outcome.Kind != OutcomeRails || result.failure != "failure" || !result.retryable {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(result.findings, []finding.Finding{wantFinding}) {
		t.Fatalf("findings = %#v", result.findings)
	}
}

func TestBatchAttemptCheckpointAbortsCanceledAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	workspace := &engineWorkspace{root: "/worktree"}
	request := engineRequest(t, nil, maxAttempts)
	state := newEngineState(&engineAudit{}, Plan{SchemaVersion: 1, Batches: []Batch{{
		Status:   BatchRunning,
		Attempts: []Attempt{{Number: 1, Status: AttemptRunning}},
	}}})
	attempt := batchAttempt{request: request, ports: Ports{Workspace: workspace}, state: state, index: 0, number: 1}

	result := attempt.checkpoint(ctx)

	if result.kind != attemptStopped || result.outcome.Kind != OutcomeErrored {
		t.Fatalf("result = %#v", result)
	}
	if len(workspace.resets) != 1 || !workspace.resetDeadlines[0] || state.plan.Batches[0].Attempts[0].Status != AttemptFailed {
		t.Fatalf("resets = %d, deadlines = %v, plan = %#v", len(workspace.resets), workspace.resetDeadlines, state.plan)
	}
}

func TestEvaluateBarrierPersistsOnceWhenClassificationFails(t *testing.T) {
	audit := &engineAudit{}
	state := newEngineState(audit, Plan{SchemaVersion: 1})
	request := engineRequest(t, nil, maxAttempts)
	barrier := ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{{}}}

	result := evaluateBarrier(context.Background(), request, state, barrier, map[string]int{}, false)

	if result.outcome == nil || result.outcome.Kind != OutcomeErrored || !strings.Contains(result.outcome.Failure, "classify barrier blockers") {
		t.Fatalf("result = %#v", result)
	}
	if len(audit.plans) != 1 {
		t.Fatalf("plan writes = %d, want 1", len(audit.plans))
	}
}

func TestEvaluateBarrierClassifiesResultsAndPersistsOnce(t *testing.T) {
	first := planFinding("a.go", 1, "lint/a", "a")
	second := planFinding("b.go", 2, "lint/b", "b")
	beforeBoth, err := BlockingMultiset([]finding.Finding{first, second})
	if err != nil {
		t.Fatal(err)
	}
	beforeFirst, err := BlockingMultiset([]finding.Finding{first})
	if err != nil {
		t.Fatal(err)
	}
	afterFirst, err := BlockingMultiset([]finding.Finding{first})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		barrier     ValidationResult
		before      map[string]int
		waveStuck   bool
		cancel      bool
		exhaustRail bool
		wantKind    OutcomeKind
		wantFailure string
		wantBlocker map[string]int
		wantFinding bool
	}{
		{name: "cancellation", barrier: ValidationResult{Kind: ValidationPassed}, before: beforeFirst, cancel: true, wantKind: OutcomeErrored, wantFailure: context.Canceled.Error()},
		{name: "rail exhaustion", barrier: ValidationResult{Kind: ValidationPassed}, before: beforeFirst, exhaustRail: true, wantKind: OutcomeRails, wantFailure: ErrRailExhausted.Error()},
		{name: "invalid validation", barrier: ValidationResult{Kind: ValidationPassed, Failure: "unexpected"}, before: beforeFirst, wantKind: OutcomeErrored, wantFailure: "invalid barrier validation"},
		{name: "infrastructure failure", barrier: ValidationResult{Kind: ValidationInfrastructureFailure, Failure: "gate crashed", Findings: []finding.Finding{first}}, before: beforeBoth, wantKind: OutcomeErrored, wantFailure: "gate crashed", wantFinding: true},
		{name: "semantic failure", barrier: ValidationResult{Kind: ValidationSemanticFailure, Failure: "regression", Findings: []finding.Finding{first}}, before: beforeBoth, wantKind: OutcomeBlocked, wantFailure: "regression", wantFinding: true},
		{name: "stuck wave", barrier: ValidationResult{Kind: ValidationPassed}, before: beforeFirst, waveStuck: true, wantKind: OutcomeBlocked, wantFailure: "one or more batches are stuck"},
		{name: "ready", barrier: ValidationResult{Kind: ValidationPassed}, before: beforeFirst, wantKind: OutcomeReady},
		{name: "stalemate", barrier: ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{first}}, before: beforeFirst, wantKind: OutcomeBlocked, wantFailure: "did not strictly shrink", wantFinding: true},
		{name: "shrinking continuation", barrier: ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{first}}, before: beforeBoth, wantBlocker: afterFirst},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			audit := &engineAudit{}
			state := newEngineState(audit, Plan{SchemaVersion: 1})
			request := engineRequest(t, nil, maxAttempts)
			if test.exhaustRail {
				now := time.Unix(0, 0)
				rails, err := NewRails(RailConfig{MaxIterations: maxAttempts, MaxWallClock: time.Minute}, func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				request.Rails = rails
				now = now.Add(time.Minute)
			}
			ctx := context.Background()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			result := evaluateBarrier(ctx, request, state, test.barrier, test.before, test.waveStuck)

			if len(audit.plans) != 1 {
				t.Fatalf("plan writes = %d, want 1", len(audit.plans))
			}
			if test.wantBlocker != nil {
				if result.outcome != nil || !reflect.DeepEqual(result.blockers, test.wantBlocker) {
					t.Fatalf("result = %#v, want continuation with %v", result, test.wantBlocker)
				}
				return
			}
			if result.outcome == nil || result.outcome.Kind != test.wantKind || !strings.Contains(result.outcome.Failure, test.wantFailure) {
				t.Fatalf("outcome = %#v, blockers = %v, want kind %q containing %q", result.outcome, result.blockers, test.wantKind, test.wantFailure)
			}
			if gotFinding := len(result.outcome.Findings) > 0; gotFinding != test.wantFinding {
				t.Fatalf("findings = %#v, want present %v", result.outcome.Findings, test.wantFinding)
			}
		})
	}
}

func TestStopHelpersUseNilForContinuation(t *testing.T) {
	request := engineRequest(t, nil, maxAttempts)
	if outcome := stopForContext(context.Background(), request.Rails, Plan{}); outcome != nil {
		t.Fatalf("context outcome = %#v", outcome)
	}
	if outcome := stopForRail(request.Rails, Plan{}); outcome != nil {
		t.Fatalf("rail outcome = %#v", outcome)
	}
}

func TestEngineExecutesBatchesSeriallyAndAuditsTransitions(t *testing.T) {
	first := planFinding("a.go", 1, "lint/a", "a")
	second := planFinding("b.go", 2, "lint/b", "b")
	events := []string{}
	workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"b.go"}}, events: &events}
	audit := &engineAudit{events: &events}
	agent := &engineAdapter{events: &events}
	validations := 0
	ports := Ports{
		Adapter: agent, Workspace: workspace, Audit: audit,
		Validate: func(_ context.Context, batch Batch) ValidationResult {
			validations++
			events = append(events, "validate:"+batch.PrimaryFile)
			return ValidationResult{Kind: ValidationPassed}
		},
		Barrier: func(context.Context) ValidationResult {
			events = append(events, "barrier")
			return ValidationResult{Kind: ValidationPassed}
		},
	}

	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{second, first}, 10), ports)

	if outcome.Kind != OutcomeReady || outcome.Iterations != 2 || outcome.Failure != "" {
		t.Fatalf("Execute() outcome = %#v, want ready after two iterations", outcome)
	}
	if validations != 2 || len(outcome.Plan.Batches) != 2 {
		t.Fatalf("validations = %d, plan = %#v", validations, outcome.Plan)
	}
	for i, batch := range outcome.Plan.Batches {
		if batch.Status != BatchDone || len(batch.Attempts) != 1 || batch.Attempts[0].Status != "passed" || batch.Attempts[0].Commit == "" {
			t.Fatalf("batch %d = %#v, want one passed committed attempt", i, batch)
		}
	}
	if got, want := agent.roots, []string{"/worktree", "/worktree"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter roots = %v, want %v", got, want)
	}
	if !strings.Contains(agent.briefs[0], `"primary_file":"a.go"`) || !strings.Contains(agent.briefs[1], `"primary_file":"b.go"`) {
		t.Fatalf("brief order/content = %#v", agent.briefs)
	}
	if got, want := events, []string{
		"plan", "plan", "brief", "sink", "adapter", "changed", "validate:a.go", "commit:a.go", "plan",
		"plan", "brief", "sink", "adapter", "changed", "validate:b.go", "commit:b.go", "plan", "barrier", "plan",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	assertPlanArtifacts(t, audit.plans)
}

func TestEngineRetriesSemanticFailureWithFreshBriefThenMarksStuckAndContinues(t *testing.T) {
	first := planFinding("a.go", 1, "lint/a", "a")
	second := planFinding("b.go", 2, "lint/b", "b")
	workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"a.go"}, {"b.go"}}}
	audit := &engineAudit{}
	validations := 0
	ports := Ports{
		Adapter: &engineAdapter{}, Workspace: workspace, Audit: audit,
		Validate: func(_ context.Context, batch Batch) ValidationResult {
			validations++
			if batch.PrimaryFile == "a.go" {
				return ValidationResult{Kind: ValidationSemanticFailure, Failure: "assigned finding remains"}
			}
			return ValidationResult{Kind: ValidationPassed}
		},
		Barrier: func(context.Context) ValidationResult {
			return ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{first}}
		},
	}

	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{first, second}, 10), ports)

	if outcome.Kind != OutcomeBlocked || outcome.Iterations != 3 || len(workspace.resets) != 2 {
		t.Fatalf("outcome = %#v, resets = %d", outcome, len(workspace.resets))
	}
	if outcome.Plan.Batches[0].Status != BatchStuck || outcome.Plan.Batches[1].Status != BatchDone {
		t.Fatalf("batch statuses = %q, %q", outcome.Plan.Batches[0].Status, outcome.Plan.Batches[1].Status)
	}
	if got := audit.briefs[1]; !strings.Contains(string(got), `"retry_failure":"assigned finding remains"`) {
		t.Fatalf("retry brief omits deterministic failure: %s", got)
	}
	if got := []int{audit.briefAttempts[0], audit.briefAttempts[1], audit.briefAttempts[2]}; !reflect.DeepEqual(got, []int{1, 2, 1}) {
		t.Fatalf("artifact attempt numbers = %v", got)
	}
}

func TestEngineRetainsSemanticValidationFindingsInTerminalOutcome(t *testing.T) {
	assigned := planFinding("a.go", 1, "lint/a", "a")
	integrity := planFinding("a.go", 2, "integrity/outside-batch", "outside")
	integrity.Gate = "integrity"
	integrity.Fingerprint = finding.Fingerprint(integrity)
	integrity.Occurrences = []finding.Occurrence{{Line: 3}}
	validationFindings := []finding.Finding{integrity}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{assigned}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"a.go"}}}, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult {
			return ValidationResult{Kind: ValidationSemanticFailure, Failure: "integrity validation found regressions", Findings: validationFindings}
		},
		Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})
	validationFindings[0].Message = "mutated"
	validationFindings[0].Occurrences[0].Line = 99
	if outcome.Kind != OutcomeBlocked || len(outcome.Findings) != 1 || outcome.Findings[0].Gate != "integrity" || outcome.Findings[0].Message == "mutated" || outcome.Findings[0].Occurrences[0].Line != 3 {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestEngineDropsPriorSemanticFindingsWhenRetryErrors(t *testing.T) {
	assigned := planFinding("a.go", 1, "lint/a", "a")
	integrity := planFinding("a.go", 2, "integrity/outside-batch", "outside")
	integrity.Gate = "integrity"
	integrity.Fingerprint = finding.Fingerprint(integrity)
	validations := 0
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{assigned}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"a.go"}}}, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult {
			validations++
			if validations == 1 {
				return ValidationResult{Kind: ValidationSemanticFailure, Failure: "integrity regression", Findings: []finding.Finding{integrity}}
			}
			return ValidationResult{Kind: ValidationInfrastructureFailure, Failure: "gate crashed"}
		},
		Barrier: func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || len(outcome.Findings) != 0 {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestEngineRetriesInfrastructureFailureThenErrors(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	workspace := &engineWorkspace{root: "/worktree"}
	agent := &engineAdapter{errs: []error{
		&adapter.Error{Retryable: true, Err: errors.New("provider unavailable")},
		&adapter.Error{Retryable: true, Err: errors.New("provider unavailable again")},
	}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 10), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier:  func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})

	if outcome.Kind != OutcomeErrored || outcome.Iterations != 2 || len(workspace.resets) != 2 {
		t.Fatalf("outcome = %#v, resets = %d", outcome, len(workspace.resets))
	}
	if len(agent.briefs) != 2 {
		t.Fatalf("adapter calls = %d, want 2", len(agent.briefs))
	}
}

func TestEngineCreatesAnotherWaveOnlyForStrictlySmallerBarrier(t *testing.T) {
	first := planFinding("a.go", 1, "lint/a", "a")
	second := planFinding("b.go", 2, "lint/b", "b")
	workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"b.go"}, {"a.go"}}}
	barriers := []ValidationResult{
		{Kind: ValidationPassed, Findings: []finding.Finding{first}},
		{Kind: ValidationPassed},
	}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{first, second}, 10), Ports{
		Adapter: &engineAdapter{}, Workspace: workspace, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier: func(context.Context) ValidationResult {
			got := barriers[0]
			barriers = barriers[1:]
			return got
		},
	})

	if outcome.Kind != OutcomeReady || outcome.Iterations != 3 || len(outcome.Plan.Batches) != 3 {
		t.Fatalf("outcome = %#v, want ready with accumulated second wave", outcome)
	}
	if outcome.Plan.Batches[0].ID == outcome.Plan.Batches[2].ID || outcome.Plan.Batches[2].Attempts[0].Number != 1 {
		t.Fatalf("wave IDs/attempts collided: %#v", outcome.Plan.Batches)
	}
}

func TestEngineBlocksOnStalemateAndRejectsInvalidBarrierFindings(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	for _, test := range []struct {
		name    string
		barrier ValidationResult
		want    OutcomeKind
	}{
		{name: "equal", barrier: ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{item}}, want: OutcomeBlocked},
		{name: "invalid", barrier: ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{{}}}, want: OutcomeErrored},
		{name: "invalid semantic", barrier: ValidationResult{Kind: ValidationSemanticFailure, Findings: []finding.Finding{{}}}, want: OutcomeErrored},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 10), Ports{
				Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}, Audit: &engineAudit{},
				Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
				Barrier:  func(context.Context) ValidationResult { return test.barrier },
			})
			if outcome.Kind != test.want {
				t.Fatalf("outcome = %#v, want %q", outcome, test.want)
			}
		})
	}
}

func TestEngineStopsAtRailsBeforeAllocatingAttemptArtifacts(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	audit := &engineAudit{}
	agent := &engineAdapter{}
	// Force the first requested attempt to be rejected.
	req := engineRequest(t, []finding.Finding{item}, 1)
	if err := req.Rails.AdmitAttempt(); err != nil {
		t.Fatal(err)
	}
	audit = &engineAudit{}
	agent = &engineAdapter{}
	outcome := Execute(context.Background(), req, Ports{Adapter: agent, Workspace: &engineWorkspace{root: "/worktree"}, Audit: audit,
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
	if outcome.Kind != OutcomeRails || outcome.Iterations != 1 || len(agent.briefs) != 0 || len(audit.briefs) != 0 {
		t.Fatalf("outcome = %#v, adapter calls = %d, briefs = %d", outcome, len(agent.briefs), len(audit.briefs))
	}
}

func TestEngineTreatsAuditFailureAsInfrastructureAndResetsPossibleEdits(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	workspace := &engineWorkspace{root: "/worktree"}
	audit := &engineAudit{briefErr: errors.New("ledger closed")}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: workspace, Audit: audit,
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier:  func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})
	if outcome.Kind != OutcomeErrored || len(workspace.resets) != 1 {
		t.Fatalf("outcome = %#v, resets = %d", outcome, len(workspace.resets))
	}
	if !workspace.resetDeadlines[0] {
		t.Fatal("attempt reset did not receive recovery deadline")
	}
}

func TestEngineRejectsInvalidInputsWithoutConsumingRails(t *testing.T) {
	req := engineRequest(t, []finding.Finding{planFinding("a.go", 1, "lint/a", "a")}, 1)
	req.MergeBase = "not-an-object-id"
	outcome := Execute(context.Background(), req, Ports{})
	if outcome.Kind != OutcomeErrored || outcome.Iterations != 0 || outcome.Failure != "merge base must be a 40- or 64-character hexadecimal object ID" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestEngineReturnsLastPersistedPlanWhenTransitionAuditFails(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	tests := []struct {
		name       string
		failWrites map[int]error
		adapter    *engineAdapter
		changed    [][]string
		validate   func(context.Context, Batch) ValidationResult
		barriers   []ValidationResult
		wantStatus BatchStatus
		wantTries  int
	}{
		{name: "initial write", failWrites: map[int]error{1: errors.New("initial write")}, adapter: &engineAdapter{}},
		{name: "running and failed transition writes", failWrites: map[int]error{2: errors.New("running write"), 3: errors.New("failed write")}, adapter: &engineAdapter{}, wantStatus: BatchPending},
		{name: "attempt failure write", failWrites: map[int]error{3: errors.New("failed write")}, adapter: &engineAdapter{errs: []error{errors.New("plain failure")}}, wantStatus: BatchRunning, wantTries: 1},
		{name: "done write", failWrites: map[int]error{3: errors.New("done write")}, adapter: &engineAdapter{}, changed: [][]string{{"a.go"}}, wantStatus: BatchRunning, wantTries: 1},
		{name: "stuck write", failWrites: map[int]error{6: errors.New("stuck write")}, adapter: &engineAdapter{}, changed: [][]string{{"a.go"}, {"a.go"}}, validate: func(context.Context, Batch) ValidationResult {
			return ValidationResult{Kind: ValidationSemanticFailure, Failure: "still broken"}
		}, wantStatus: BatchRunning, wantTries: 2},
		{name: "ready barrier write", failWrites: map[int]error{4: errors.New("barrier write")}, adapter: &engineAdapter{}, changed: [][]string{{"a.go"}}, barriers: []ValidationResult{{Kind: ValidationPassed}}, wantStatus: BatchDone, wantTries: 1},
		{name: "stalemate barrier write", failWrites: map[int]error{4: errors.New("barrier write")}, adapter: &engineAdapter{}, changed: [][]string{{"a.go"}}, barriers: []ValidationResult{{Kind: ValidationPassed, Findings: []finding.Finding{item}}}, wantStatus: BatchDone, wantTries: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			validate := test.validate
			if validate == nil {
				validate = func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }
			}
			barriers := append([]ValidationResult(nil), test.barriers...)
			if len(barriers) == 0 {
				barriers = []ValidationResult{{Kind: ValidationPassed}}
			}
			audit := &engineAudit{planErrors: test.failWrites}
			outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 5), Ports{
				Adapter: test.adapter, Workspace: &engineWorkspace{root: "/worktree", changed: test.changed}, Audit: audit, Validate: validate,
				Barrier: func(context.Context) ValidationResult { got := barriers[0]; barriers = barriers[1:]; return got },
			})
			if outcome.Kind != OutcomeErrored || !strings.Contains(outcome.Failure, "write") {
				t.Fatalf("outcome = %#v, want audit error", outcome)
			}
			if test.name == "initial write" {
				if outcome.Plan.Batches != nil {
					t.Fatalf("initial write failure claimed plan %#v", outcome.Plan)
				}
				return
			}
			if len(outcome.Plan.Batches) != 1 || outcome.Plan.Batches[0].Status != test.wantStatus || len(outcome.Plan.Batches[0].Attempts) != test.wantTries {
				t.Fatalf("returned plan = %#v, want last persisted status %q with %d attempts", outcome.Plan, test.wantStatus, test.wantTries)
			}
		})
	}
}

func TestEngineDoesNotClaimUnpersistedNextWave(t *testing.T) {
	first := planFinding("a.go", 1, "lint/a", "a")
	second := planFinding("b.go", 2, "lint/b", "b")
	barriers := []ValidationResult{{Kind: ValidationPassed, Findings: []finding.Finding{first}}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{first, second}, 5), Ports{
		Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"b.go"}}}, Audit: &engineAudit{planErrors: map[int]error{7: errors.New("next wave write")}},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier:  func(context.Context) ValidationResult { got := barriers[0]; barriers = barriers[1:]; return got },
	})
	if outcome.Kind != OutcomeErrored || len(outcome.Plan.Batches) != 2 {
		t.Fatalf("outcome = %#v, want only persisted first wave", outcome)
	}
}

func TestEnginePassesValidatorADeepBatchCopy(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	item.Occurrences = []finding.Occurrence{{Line: 2}}
	plan, err := NewPlan([]finding.Finding{item})
	if err != nil {
		t.Fatal(err)
	}
	originalID := plan.Batches[0].ID
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}, Audit: &engineAudit{},
		Validate: func(_ context.Context, batch Batch) ValidationResult {
			batch.ID = "hostile"
			batch.Findings[0].Message = "mutated"
			batch.Findings[0].Occurrences[0].Line = 999
			batch.Attempts[0].ChangedFiles = append(batch.Attempts[0].ChangedFiles, "hostile")
			return ValidationResult{Kind: ValidationPassed}
		},
		Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})
	got := outcome.Plan.Batches[0]
	if outcome.Kind != OutcomeReady || got.ID != originalID || got.Findings[0].Message == "mutated" || got.Findings[0].Occurrences[0].Line != 2 {
		t.Fatalf("validator mutated live plan: %#v", got)
	}
}

func TestEngineBoundsFailureReasonsWithoutBreakingUTF8OrRetry(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	huge := strings.Repeat("界", maxBriefDiagnosticFieldBytes)
	audit := &engineAudit{}
	validations := 0
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}, {"a.go"}}}, Audit: audit,
		Validate: func(context.Context, Batch) ValidationResult {
			validations++
			return ValidationResult{Kind: ValidationSemanticFailure, Failure: huge}
		},
		Barrier: func(context.Context) ValidationResult {
			return ValidationResult{Kind: ValidationPassed, Findings: []finding.Finding{item}}
		},
	})
	if outcome.Kind != OutcomeBlocked || validations != 2 {
		t.Fatalf("outcome = %#v, validations = %d", outcome, validations)
	}
	for _, attempt := range outcome.Plan.Batches[0].Attempts {
		if len(attempt.Failure) > maxBriefDiagnosticFieldBytes || !utf8.ValidString(attempt.Failure) || !strings.HasSuffix(attempt.Failure, failureTruncationMarker) {
			t.Fatalf("unbounded/invalid failure: bytes=%d valid=%v suffix=%v", len(attempt.Failure), utf8.ValidString(attempt.Failure), strings.HasSuffix(attempt.Failure, failureTruncationMarker))
		}
	}
	if !strings.Contains(string(audit.briefs[1]), "...[truncated]") {
		t.Fatalf("retry brief lacks bounded marker")
	}
}

func TestEngineBoundsTerminalInfrastructureFailure(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	huge := errors.New(strings.Repeat("界", maxBriefDiagnosticFieldBytes))
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 1), Ports{
		Adapter: &engineAdapter{errs: []error{huge}}, Workspace: &engineWorkspace{root: "/worktree"}, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})
	if outcome.Kind != OutcomeErrored || len(outcome.Failure) > maxBriefDiagnosticFieldBytes || !utf8.ValidString(outcome.Failure) || !strings.HasSuffix(outcome.Failure, failureTruncationMarker) {
		t.Fatalf("outcome failure = bytes %d, valid %v, suffix %v", len(outcome.Failure), utf8.ValidString(outcome.Failure), strings.HasSuffix(outcome.Failure, failureTruncationMarker))
	}
}

func TestEngineBoundsWorkspaceFailureBeforeRetryBrief(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	huge := errors.New(strings.Repeat("界", maxBriefDiagnosticFieldBytes))
	audit := &engineAudit{}
	workspace := &engineWorkspace{root: "/worktree", changedErrors: []error{huge, huge}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{Adapter: &engineAdapter{}, Workspace: workspace, Audit: audit, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
	if outcome.Kind != OutcomeErrored || len(audit.briefs) != 2 || !strings.Contains(string(audit.briefs[1]), "...[truncated]") {
		t.Fatalf("outcome = %#v, briefs=%d", outcome, len(audit.briefs))
	}
}

func TestEngineRejectsInconsistentAttemptValidationResults(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	tests := []ValidationResult{
		{Kind: ValidationPassed, Failure: "contradiction"},
		{Kind: ValidationPassed, Findings: []finding.Finding{item}},
		{Kind: ValidationSemanticFailure},
		{Kind: ValidationInfrastructureFailure},
		{Kind: "unknown", Failure: "unknown"},
	}
	for _, result := range tests {
		result := result
		t.Run(string(result.Kind)+result.Failure, func(t *testing.T) {
			workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}
			outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
				Adapter: &engineAdapter{}, Workspace: workspace, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return result },
				Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
			})
			if outcome.Kind != OutcomeErrored || len(workspace.resets) != 1 {
				t.Fatalf("result %#v produced outcome %#v and %d resets", result, outcome, len(workspace.resets))
			}
		})
	}
}

func TestEngineRejectsInconsistentBarrierValidationResults(t *testing.T) {
	for _, result := range []ValidationResult{
		{Kind: ValidationPassed, Failure: "contradiction"},
		{Kind: ValidationSemanticFailure},
		{Kind: ValidationInfrastructureFailure},
		{Kind: "unknown", Failure: "unknown"},
	} {
		result := result
		t.Run(string(result.Kind)+result.Failure, func(t *testing.T) {
			outcome := Execute(context.Background(), engineRequest(t, nil, 1), Ports{Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree"}, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return result }})
			if outcome.Kind != OutcomeErrored {
				t.Fatalf("result %#v produced %#v", result, outcome)
			}
		})
	}
}

func TestEnginePlainAdapterErrorDoesNotRetry(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	agent := &engineAdapter{errs: []error{errors.New("plain failure")}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: agent, Workspace: &engineWorkspace{root: "/worktree"}, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})
	if outcome.Kind != OutcomeErrored || len(agent.briefs) != 1 || outcome.Iterations != 1 {
		t.Fatalf("outcome = %#v, calls = %d", outcome, len(agent.briefs))
	}
}

func TestEngineRejectsNilAndCanceledContextsBeforeSideEffects(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	for _, test := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "nil"},
		{name: "canceled", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			audit := &engineAudit{}
			agent := &engineAdapter{}
			outcome := Execute(test.ctx, engineRequest(t, []finding.Finding{item}, 1), Ports{Adapter: agent, Workspace: &engineWorkspace{root: "/worktree"}, Audit: audit, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
			if outcome.Kind != OutcomeErrored || outcome.Iterations != 0 || len(audit.plans) != 0 || len(agent.briefs) != 0 {
				t.Fatalf("outcome = %#v, plans=%d calls=%d", outcome, len(audit.plans), len(agent.briefs))
			}
		})
	}
}

func TestEngineNilContextDoesNotReadRails(t *testing.T) {
	reads := 0
	rails, err := NewRails(RailConfig{MaxIterations: 1, MaxWallClock: time.Second}, func() time.Time { reads++; return time.Unix(0, 0) })
	if err != nil {
		t.Fatal(err)
	}
	reads = 0
	req := Request{MergeBase: strings.Repeat("a", 40), OriginalHead: strings.Repeat("b", 40), Rails: rails}
	outcome := Execute(nil, req, Ports{})
	if outcome.Kind != OutcomeErrored || reads != 0 {
		t.Fatalf("outcome = %#v, rail clock reads = %d", outcome, reads)
	}
}

func TestEngineCancellationAfterAdapterEditsResetsBeforeReturning(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	ctx, cancel := context.WithCancel(context.Background())
	workspace := &engineWorkspace{root: "/worktree"}
	agent := &engineAdapter{run: func(context.Context, adapter.Request) error { cancel(); return nil }}
	outcome := Execute(ctx, engineRequest(t, []finding.Finding{item}, 2), Ports{Adapter: agent, Workspace: workspace, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
	if outcome.Kind != OutcomeErrored || len(workspace.resets) != 1 || len(outcome.Plan.Batches[0].Attempts) != 1 || outcome.Plan.Batches[0].Attempts[0].Status != "failed" {
		t.Fatalf("outcome = %#v, resets = %d", outcome, len(workspace.resets))
	}
}

func TestEnginePersistsSuccessfulCommitBeforeHonoringCancellation(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	ctx, cancel := context.WithCancel(context.Background())
	workspace := &engineWorkspace{
		root: "/worktree", changed: [][]string{{"a.go", "related.go"}},
		commitRun: func(string) (string, error) { cancel(); return strings.Repeat("c", 40), nil },
	}
	audit := &engineAudit{}
	barrierCalls := 0
	outcome := Execute(ctx, engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: workspace, Audit: audit,
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier: func(context.Context) ValidationResult {
			barrierCalls++
			return ValidationResult{Kind: ValidationPassed}
		},
	})
	if outcome.Kind != OutcomeErrored || outcome.Failure != context.Canceled.Error() {
		t.Fatalf("outcome = %#v, want canceled error", outcome)
	}
	if barrierCalls != 0 || len(workspace.resets) != 0 {
		t.Fatalf("barrier calls = %d, resets = %d", barrierCalls, len(workspace.resets))
	}
	if len(outcome.Plan.Batches) != 1 {
		t.Fatalf("plan = %#v", outcome.Plan)
	}
	batch := outcome.Plan.Batches[0]
	if batch.Status != BatchDone || len(batch.Attempts) != 1 {
		t.Fatalf("batch = %#v, want persisted done attempt", batch)
	}
	attempt := batch.Attempts[0]
	if attempt.Status != "passed" || attempt.Commit != strings.Repeat("c", 40) || !reflect.DeepEqual(attempt.ChangedFiles, []string{"a.go", "related.go"}) {
		t.Fatalf("attempt = %#v", attempt)
	}
	var persisted Plan
	if err := json.Unmarshal(audit.plans[len(audit.plans)-1], &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Batches[0].Status != BatchDone || persisted.Batches[0].Attempts[0].Commit != strings.Repeat("c", 40) {
		t.Fatalf("last audit plan = %#v", persisted)
	}
}

func TestEngineProtectsGitStateAroundEveryAdapterInvocation(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	workspace := &engineWorkspace{
		root: "/worktree", changed: [][]string{{"a.go"}},
		checkErrors: []error{&GitStateCheckError{Restored: true, Err: errors.New("unauthorized Git state mutation")}, nil},
	}
	agent := &engineAdapter{errs: []error{errors.New("adapter result must lose precedence"), nil}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 3), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier:  func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
	})
	if outcome.Kind != OutcomeReady || len(agent.briefs) != 2 || workspace.snapshotCalls != 2 || workspace.checkCalls != 2 || len(workspace.resets) != 1 {
		t.Fatalf("outcome=%#v calls(adapter=%d snapshot=%d check=%d reset=%d)", outcome, len(agent.briefs), workspace.snapshotCalls, workspace.checkCalls, len(workspace.resets))
	}
	if !workspace.checkDeadlines[0] || !workspace.checkDeadlines[1] || !workspace.resetDeadlines[0] {
		t.Fatal("Git check/reset did not receive recovery deadlines")
	}
	if got := outcome.Plan.Batches[0].Attempts[0].Failure; !strings.Contains(got, "unauthorized Git state mutation") || strings.Contains(got, "adapter result") {
		t.Fatalf("integrity failure precedence = %q", got)
	}
}

func TestEngineProtectsRealWorkspaceGitState(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "mutated"}[mutate], func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), map[bool]string{false: "engine-clean", true: "engine-mutated"}[mutate])
			item := planFinding("feature.txt", 1, "lint/a", "original")
			calls := 0
			agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
				calls++
				if err := os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("fixed\n"), 0o600); err != nil {
					return err
				}
				if mutate && calls == 1 {
					gitcmdtest.Git(t, request.Root, "add", "--", "feature.txt")
				}
				return nil
			}}
			outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 3), Ports{
				Adapter: agent, Workspace: workspace, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
			})
			wantCalls := 1
			if mutate {
				wantCalls = 2
			}
			if outcome.Kind != OutcomeReady || calls != wantCalls {
				t.Fatalf("outcome=%#v calls=%d", outcome, calls)
			}
		})
	}
}

func TestEngineRejectsMutationAfterValidatorResultBeforeCommit(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-proof-drift")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		return os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("fixed\n"), 0o600)
	}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult {
			writeWorkspaceFile(t, workspace.Path(), "late.txt", "late\n")
			return ValidationResult{Kind: ValidationPassed}
		},
		Barrier: func(context.Context) ValidationResult {
			t.Fatal("barrier called after proof drift")
			return ValidationResult{}
		},
	})
	if outcome.Kind != OutcomeErrored || !strings.Contains(outcome.Failure, "verify batch proof") {
		t.Fatalf("outcome = %#v, want proof infrastructure failure", outcome)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-engine-proof-drift"); got != head {
		t.Fatalf("run ref = %q, want %q", got, head)
	}
}

func TestEngineRejectsIgnoredEmptyDirectoryAfterValidationAndResetsIt(t *testing.T) {
	repo, head := workspaceRepositoryWithIgnoredGenerated(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-ignored-proof-drift")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		return os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("fixed\n"), 0o600)
	}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult {
			if err := os.MkdirAll(filepath.Join(workspace.Path(), "generated", "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			return ValidationResult{Kind: ValidationPassed}
		},
		Barrier: func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || !strings.Contains(outcome.Failure, "verify batch proof") {
		t.Fatalf("outcome = %#v, want ignored-entry proof failure", outcome)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-engine-ignored-proof-drift"); got != head {
		t.Fatalf("run ref = %q, want %q", got, head)
	}
	if _, err := os.Lstat(filepath.Join(workspace.Path(), "generated")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored output survived reset: %v", err)
	}
}

func TestEngineProofFailureDominatesValidatorCancellation(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-proof-cancel")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	ctx, cancel := context.WithCancelCause(context.Background())
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		return os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("fixed\n"), 0o600)
	}}
	outcome := Execute(ctx, engineRequest(t, []finding.Finding{item}, 1), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
		Validate: func(context.Context, Batch) ValidationResult {
			writeWorkspaceFile(t, workspace.Path(), "late.txt", "late\n")
			cancel(assertError("validator canceled"))
			return ValidationResult{Kind: ValidationPassed}
		},
		Barrier: func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || !strings.Contains(outcome.Failure, "verify batch proof") || strings.Contains(outcome.Failure, "validator canceled") {
		t.Fatalf("outcome = %#v, want proof failure precedence", outcome)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-engine-proof-cancel"); got != head {
		t.Fatalf("run ref = %q, want %q", got, head)
	}
}

func TestEngineIntegrityFailureDominatesCancellationAndCleansRealWorkspace(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-mutated-canceled")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	ctx, cancel := context.WithCancel(context.Background())
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		if err := os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("mutated\n"), 0o600); err != nil {
			return err
		}
		gitcmdtest.Git(t, request.Root, "add", "--", "feature.txt")
		cancel()
		return context.Canceled
	}}
	outcome := Execute(ctx, engineRequest(t, []finding.Finding{item}, 1), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || !strings.Contains(outcome.Failure, "check Git state") || strings.Contains(outcome.Failure, "reset attempt") {
		t.Fatalf("outcome=%#v", outcome)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD=%q want %q", got, head)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "status", "--porcelain=v2", "--untracked-files=all"); got != "" {
		t.Fatalf("status=%q", got)
	}
}

func TestEngineDoesNotRetryPreservedGitMutationAsNewBaseline(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-preserved-tag")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	calls := 0
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		calls++
		gitcmdtest.Git(t, request.Root, "tag", "agent-preserved-tag")
		return nil
	}}
	barrierCalls := 0
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult {
			barrierCalls++
			return ValidationResult{Kind: ValidationPassed}
		},
	})
	if outcome.Kind != OutcomeErrored || calls != 1 || barrierCalls != 0 || !strings.Contains(outcome.Failure, "Git state") {
		t.Fatalf("outcome=%#v calls=%d barrier=%d", outcome, calls, barrierCalls)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/tags/agent-preserved-tag"); got != head {
		t.Fatalf("preserved tag=%q want %q", got, head)
	}
}

func TestEngineWallClockIsHardDuringDirtyAttempt(t *testing.T) {
	for _, crossing := range []string{"adapter", "validation"} {
		t.Run(crossing, func(t *testing.T) {
			item := planFinding("a.go", 1, "lint/a", "a")
			now := time.Unix(0, 0)
			rails, err := NewRails(RailConfig{MaxIterations: 3, MaxWallClock: time.Minute}, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}
			agent := &engineAdapter{run: func(context.Context, adapter.Request) error {
				if crossing == "adapter" {
					now = now.Add(time.Minute)
				}
				return nil
			}}
			validations := 0
			outcome := Execute(context.Background(), Request{MergeBase: strings.Repeat("a", 40), OriginalHead: strings.Repeat("b", 40), InitialFindings: []finding.Finding{item}, Rails: rails}, Ports{
				Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
				Validate: func(context.Context, Batch) ValidationResult {
					validations++
					if crossing == "validation" {
						now = now.Add(time.Minute)
					}
					return ValidationResult{Kind: ValidationPassed}
				},
				Barrier: func(context.Context) ValidationResult {
					t.Fatal("barrier called after rail exhaustion")
					return ValidationResult{}
				},
			})
			if outcome.Kind != OutcomeRails || len(workspace.resets) != 1 || workspace.commits != 0 {
				t.Fatalf("outcome=%#v resets=%d commits=%d validations=%d", outcome, len(workspace.resets), workspace.commits, validations)
			}
		})
	}
}

func TestEngineCompletionAuditFailureRollsBackRealWorkspaceCommit(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-audit-rollback")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		return os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("fixed\n"), 0o600)
	}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{planErrors: map[int]error{3: errors.New("completion audit failed")}},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier:  func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || outcome.Plan.Batches[0].Status != BatchRunning {
		t.Fatalf("outcome=%#v", outcome)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD=%q want %q", got, head)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "status", "--porcelain=v2", "--untracked-files=all"); got != "" {
		t.Fatalf("status=%q", got)
	}
}

type enginePlanWriteError struct {
	published bool
	err       error
}

func (err *enginePlanWriteError) Error() string       { return err.err.Error() }
func (err *enginePlanWriteError) Unwrap() error       { return err.err }
func (err *enginePlanWriteError) PlanPublished() bool { return err.published }

func TestEnginePublishedCompletionErrorPreservesDonePlanAndCommit(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: &engineAdapter{}, Workspace: workspace, Audit: &engineAudit{planErrors: map[int]error{3: &enginePlanWriteError{published: true, err: errors.New("sync failed")}}},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || outcome.Plan.Batches[0].Status != BatchDone || len(workspace.rollbackCalls) != 0 {
		t.Fatalf("outcome=%#v rollbacks=%v", outcome, workspace.rollbackCalls)
	}
}

func TestEngineUnpublishedCompletionRollbackGetsRecoveryDeadline(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{Adapter: &engineAdapter{}, Workspace: workspace, Audit: &engineAudit{planErrors: map[int]error{3: errors.New("publish failed")}}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
	if outcome.Kind != OutcomeErrored || len(workspace.rollbackDeadlines) != 1 || !workspace.rollbackDeadlines[0] {
		t.Fatalf("outcome=%#v rollback deadlines=%v", outcome, workspace.rollbackDeadlines)
	}
}

func TestEnginePublishedCompletionErrorPreservesRealWorkspaceCommit(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "engine-published-plan")
	item := planFinding("feature.txt", 1, "lint/a", "original")
	agent := &engineAdapter{run: func(_ context.Context, request adapter.Request) error {
		return os.WriteFile(filepath.Join(request.Root, "feature.txt"), []byte("fixed\n"), 0o600)
	}}
	outcome := Execute(context.Background(), engineRequest(t, []finding.Finding{item}, 2), Ports{
		Adapter: agent, Workspace: workspace, Audit: &engineAudit{planErrors: map[int]error{3: &enginePlanWriteError{published: true, err: errors.New("sync failed")}}},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { t.Fatal("barrier called"); return ValidationResult{} },
	})
	if outcome.Kind != OutcomeErrored || outcome.Plan.Batches[0].Status != BatchDone {
		t.Fatalf("outcome=%#v", outcome)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD"); got == head || got != outcome.Plan.Batches[0].Attempts[0].Commit {
		t.Fatalf("HEAD=%q original=%q plan=%#v", got, head, outcome.Plan)
	}
}

func TestEngineRailDeadlineCancelsActiveCallbacks(t *testing.T) {
	for _, operation := range []string{"snapshot", "adapter", "validation", "barrier"} {
		t.Run(operation, func(t *testing.T) {
			findings := []finding.Finding{planFinding("a.go", 1, "lint/a", "a")}
			if operation == "barrier" {
				findings = nil
			}
			rails, err := NewRails(RailConfig{MaxIterations: 2, MaxWallClock: 20 * time.Millisecond}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			workspace := &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}, snapshotRun: func(ctx context.Context) (GitState, error) {
				if operation == "snapshot" {
					<-ctx.Done()
					return GitState{}, ctx.Err()
				}
				return GitState{}, nil
			}}
			agent := &engineAdapter{run: func(ctx context.Context, _ adapter.Request) error {
				if operation == "adapter" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
			outcome := Execute(context.Background(), Request{MergeBase: strings.Repeat("a", 40), OriginalHead: strings.Repeat("b", 40), InitialFindings: findings, Rails: rails}, Ports{
				Adapter: agent, Workspace: workspace, Audit: &engineAudit{},
				Validate: func(ctx context.Context, _ Batch) ValidationResult {
					if operation == "validation" {
						<-ctx.Done()
					}
					return ValidationResult{Kind: ValidationPassed}
				},
				Barrier: func(ctx context.Context) ValidationResult {
					if operation == "barrier" {
						<-ctx.Done()
					}
					return ValidationResult{Kind: ValidationPassed}
				},
			})
			if outcome.Kind != OutcomeRails {
				t.Fatalf("outcome=%#v", outcome)
			}
			if operation == "snapshot" && len(workspace.resets) != 1 {
				t.Fatalf("snapshot cancellation resets=%d, want 1", len(workspace.resets))
			}
		})
	}
}

func TestRecoveryContextHasIndependentBoundedDeadline(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	started := time.Now()
	ctx, cancel := recoveryContextWithTimeout(parent, 15*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		t.Fatalf("recovery context deadline/initial error = %v, %v", ok, ctx.Err())
	}
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("recovery context = %v after %s", ctx.Err(), time.Since(started))
	}
}

func TestEngineBarrierAuditFailureDominatesWallClockExhaustion(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	now := time.Unix(0, 0)
	rails, err := NewRails(RailConfig{MaxIterations: 2, MaxWallClock: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	outcome := Execute(context.Background(), Request{MergeBase: strings.Repeat("a", 40), OriginalHead: strings.Repeat("b", 40), InitialFindings: []finding.Finding{item}, Rails: rails}, Ports{
		Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree", changed: [][]string{{"a.go"}}}, Audit: &engineAudit{planErrors: map[int]error{4: errors.New("barrier audit failed")}},
		Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} },
		Barrier: func(context.Context) ValidationResult {
			now = now.Add(time.Minute)
			return ValidationResult{Kind: ValidationPassed}
		},
	})
	if outcome.Kind != OutcomeErrored || !strings.Contains(outcome.Failure, "barrier audit failed") {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestEngineCanceledContextReportsIndependentlyExhaustedWallClockRail(t *testing.T) {
	item := planFinding("a.go", 1, "lint/a", "a")
	now := time.Unix(0, 0)
	rails, err := NewRails(RailConfig{MaxIterations: 1, MaxWallClock: time.Second}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := Request{MergeBase: strings.Repeat("a", 40), OriginalHead: strings.Repeat("b", 40), InitialFindings: []finding.Finding{item}, Rails: rails}
	audit := &engineAudit{}
	outcome := Execute(ctx, req, Ports{Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree"}, Audit: audit, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
	if outcome.Kind != OutcomeRails || outcome.Iterations != 0 || len(audit.plans) != 0 {
		t.Fatalf("outcome = %#v, plans=%d", outcome, len(audit.plans))
	}
}

func TestEngineCanceledContextDoesNotInferRailOutcomeFromIterationSnapshot(t *testing.T) {
	req := engineRequest(t, nil, 1)
	if err := req.Rails.AdmitAttempt(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := Execute(ctx, req, Ports{Adapter: &engineAdapter{}, Workspace: &engineWorkspace{root: "/worktree"}, Audit: &engineAudit{}, Validate: func(context.Context, Batch) ValidationResult { return ValidationResult{Kind: ValidationPassed} }, Barrier: func(context.Context) ValidationResult { return ValidationResult{Kind: ValidationPassed} }})
	if outcome.Kind != OutcomeErrored || outcome.Iterations != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func engineRequest(t *testing.T, findings []finding.Finding, iterations int) Request {
	t.Helper()
	rails, err := NewRails(RailConfig{MaxIterations: iterations, MaxWallClock: time.Hour}, func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return Request{MergeBase: strings.Repeat("a", 40), OriginalHead: strings.Repeat("b", 40), InitialFindings: findings, Rails: rails}
}

type engineAdapter struct {
	errs          []error
	briefs, roots []string
	events        *[]string
	run           func(context.Context, adapter.Request) error
}

func (a *engineAdapter) Name() string { return "fake" }
func (a *engineAdapter) Run(ctx context.Context, req adapter.Request) (adapter.Result, error) {
	a.briefs = append(a.briefs, req.Brief)
	a.roots = append(a.roots, req.Root)
	if a.events != nil {
		*a.events = append(*a.events, "adapter")
	}
	if a.run != nil {
		return adapter.Result{}, a.run(ctx, req)
	}
	if len(a.errs) == 0 {
		return adapter.Result{}, nil
	}
	err := a.errs[0]
	a.errs = a.errs[1:]
	return adapter.Result{}, err
}

type engineSink struct{}

func (engineSink) WriteAdapterJSONL([]byte) error { return nil }

type engineAudit struct {
	plans, briefs              [][]byte
	briefAttempts              []int
	planErr, briefErr, sinkErr error
	planErrors                 map[int]error
	planWrites                 int
	events                     *[]string
}

func (a *engineAudit) WritePlan(raw []byte) error {
	a.planWrites++
	a.plans = append(a.plans, append([]byte(nil), raw...))
	if a.events != nil {
		*a.events = append(*a.events, "plan")
	}
	if err := a.planErrors[a.planWrites]; err != nil {
		return err
	}
	return a.planErr
}
func (a *engineAudit) WriteBrief(_ string, attempt int, raw []byte) error {
	a.briefs = append(a.briefs, append([]byte(nil), raw...))
	a.briefAttempts = append(a.briefAttempts, attempt)
	if a.events != nil {
		*a.events = append(*a.events, "brief")
	}
	return a.briefErr
}
func (a *engineAudit) AdapterSink(_ string, _ int) (adapter.Sink, error) {
	if a.events != nil {
		*a.events = append(*a.events, "sink")
	}
	return engineSink{}, a.sinkErr
}

type engineWorkspace struct {
	root              string
	changed           [][]string
	changedErrors     []error
	resets            []struct{}
	commits           int
	events            *[]string
	commitRun         func(string) (string, error)
	snapshotCalls     int
	checkCalls        int
	checkErrors       []error
	rollbackCalls     []string
	snapshotRun       func(context.Context) (GitState, error)
	checkDeadlines    []bool
	resetDeadlines    []bool
	rollbackDeadlines []bool
	prepared          []BatchProof
	verifyErrors      []error
}

func (w *engineWorkspace) Root() string { return w.root }
func (w *engineWorkspace) SnapshotGitState(ctx context.Context) (GitState, error) {
	w.snapshotCalls++
	if w.snapshotRun != nil {
		return w.snapshotRun(ctx)
	}
	return GitState{}, nil
}
func (w *engineWorkspace) CheckGitState(ctx context.Context, _ GitState) error {
	w.checkCalls++
	_, deadline := ctx.Deadline()
	w.checkDeadlines = append(w.checkDeadlines, deadline)
	if len(w.checkErrors) == 0 {
		return nil
	}
	err := w.checkErrors[0]
	w.checkErrors = w.checkErrors[1:]
	return err
}
func (w *engineWorkspace) ChangedFiles(context.Context) ([]string, error) {
	if w.events != nil {
		*w.events = append(*w.events, "changed")
	}
	if len(w.changedErrors) > 0 {
		err := w.changedErrors[0]
		w.changedErrors = w.changedErrors[1:]
		return nil, err
	}
	if len(w.changed) == 0 {
		return nil, nil
	}
	got := w.changed[0]
	w.changed = w.changed[1:]
	return append([]string(nil), got...), nil
}
func (w *engineWorkspace) PrepareBatch(_ context.Context, changed []string) (BatchProof, error) {
	proof := BatchProof{
		owner: w, tree: strings.Repeat("f", 40), changed: append([]string(nil), changed...), verify: w.VerifyBatch,
		validation: &validationSnapshot{private: &privateTempDir{path: "/private/validation"}},
	}
	w.prepared = append(w.prepared, proof)
	return proof, nil
}
func (w *engineWorkspace) VerifyBatch(_ context.Context, _ BatchProof) error {
	if len(w.verifyErrors) == 0 {
		return nil
	}
	err := w.verifyErrors[0]
	w.verifyErrors = w.verifyErrors[1:]
	return err
}
func (w *engineWorkspace) ResetAttempt(ctx context.Context) error {
	_, deadline := ctx.Deadline()
	w.resetDeadlines = append(w.resetDeadlines, deadline)
	w.resets = append(w.resets, struct{}{})
	return nil
}
func (w *engineWorkspace) CommitBatch(_ context.Context, primary string, proof BatchProof) (string, error) {
	if proof.owner != w {
		return "", errors.New("foreign batch proof")
	}
	if w.commitRun != nil {
		return w.commitRun(primary)
	}
	w.commits++
	if w.events != nil {
		*w.events = append(*w.events, "commit:"+primary)
	}
	return strings.Repeat(string(rune('0'+w.commits)), 40), nil
}
func (w *engineWorkspace) RollbackBatch(ctx context.Context, commit string) error {
	_, deadline := ctx.Deadline()
	w.rollbackDeadlines = append(w.rollbackDeadlines, deadline)
	w.rollbackCalls = append(w.rollbackCalls, commit)
	return nil
}

func assertPlanArtifacts(t *testing.T, plans [][]byte) {
	t.Helper()
	if len(plans) == 0 {
		t.Fatal("no plan artifacts")
	}
	for i, raw := range plans {
		if len(raw) == 0 || raw[len(raw)-1] != '\n' || !strings.Contains(string(raw), "\n  \"schema_version\"") {
			t.Fatalf("plan artifact %d is not indented JSON with trailing newline: %q", i, raw)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("plan artifact %d: %v", i, err)
		}
		for _, batch := range decoded["batches"].([]any) {
			if _, exists := batch.(map[string]any)["identity"]; exists {
				t.Fatalf("artifact exposes proof: %s", raw)
			}
		}
	}
}
