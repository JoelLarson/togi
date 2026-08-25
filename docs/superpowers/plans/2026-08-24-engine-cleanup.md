# Engine Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refactor flywheel orchestration so attempt, retry, stop, and barrier policies are readable in isolation without changing behavior or persisted data.

**Architecture:** `Execute` keeps wave ownership and delegates barrier evaluation. `executeBatch` owns retry policy while an attempt-scoped `batchAttempt` owns the linear side-effect pipeline and returns classified results to one retry decision point.

**Tech Stack:** Go 1.25, the existing standard-library test suite, and no new dependencies.

**Design:** `docs/superpowers/specs/2026-08-24-engine-cleanup-design.md`

---

## File Map

- Modify `internal/flywheel/plan.go` for the typed durable attempt status.
- Modify `internal/flywheel/plan_test.go` for status serialization coverage.
- Modify `internal/flywheel/engine.go` for state construction, attempt orchestration, barrier evaluation, naming, and invariant comments.
- Modify `internal/flywheel/engine_test.go` for focused structural contracts and unchanged behavior.
- Modify `internal/flywheel/engine.go.cleanup.md` after each completed slice.

### Task 1: Name Durable Attempt State And Engine Construction

**Files:**
- Modify: `internal/flywheel/plan.go`
- Modify: `internal/flywheel/plan_test.go`
- Modify: `internal/flywheel/engine.go`
- Modify: `internal/flywheel/engine_test.go`
- Modify: `internal/flywheel/engine.go.cleanup.md`

- [x] **Step 1: Write failing tests**

Add a plan JSON test that assigns `AttemptRunning`, `AttemptPassed`, and
`AttemptFailed` to `Attempt.Status` and expects the existing string values. Add
an engine test that calls `newEngineState(audit, plan)` and asserts
`semanticFindings` is non-nil and the plan/audit are retained.

```go
func TestAttemptStatusesKeepPersistedValues(t *testing.T) {
    got, err := json.Marshal([]Attempt{
        {Status: AttemptRunning},
        {Status: AttemptPassed},
        {Status: AttemptFailed},
    })
    if err != nil { t.Fatal(err) }
    if !strings.Contains(string(got), `"status":"running"`) ||
        !strings.Contains(string(got), `"status":"passed"`) ||
        !strings.Contains(string(got), `"status":"failed"`) {
        t.Fatalf("statuses = %s", got)
    }
}

func TestNewEngineStateInitializesSemanticFindings(t *testing.T) {
    audit := &engineAudit{}
    plan := Plan{SchemaVersion: 1}
    state := newEngineState(audit, plan)
    if state.audit != audit || state.plan.SchemaVersion != 1 || state.semanticFindings == nil {
        t.Fatalf("state = %#v", state)
    }
}
```

- [x] **Step 2: Verify RED**

Run:

```sh
go test ./internal/flywheel -run 'TestAttemptStatusesKeepPersistedValues|TestNewEngineStateInitializesSemanticFindings'
```

Expected: compile failure because the status constants and constructor do not exist.

- [x] **Step 3: Implement the named state**

In `plan.go`, define `AttemptStatus string`, constants for running/passed/failed,
and change `Attempt.Status` from `string` to `AttemptStatus`. In `engine.go`, add
`const maxAttempts = 2`, add `newEngineState`, and construct state through it.
Replace raw status strings and remove the lazy semantic map initialization.

- [x] **Step 4: Verify GREEN and commit**

```sh
gofmt -w internal/flywheel/plan.go internal/flywheel/plan_test.go internal/flywheel/engine.go internal/flywheel/engine_test.go
go test ./internal/flywheel
git add internal/flywheel
git commit -m "Name engine attempt state"
```

Expected: PASS. Mark the applicable `maxAttempts`, typed-status, and lazy-map bullets
in checklist item 7 complete.

### Task 2: Separate Attempt Execution From Retry Policy

**Files:**
- Modify: `internal/flywheel/engine.go`
- Modify: `internal/flywheel/engine_test.go`
- Modify: `internal/flywheel/engine.go.cleanup.md`

- [x] **Step 1: Write failing result and checkpoint tests**

Add table tests for an `attemptResult` with kinds `attemptSucceeded`,
`attemptSemanticFailure`, `attemptInfrastructureFailure`, and `attemptStopped`.
Assert failure text, retryability, findings, and terminal outcome are retained.
Add a canceled-context test for `batchAttempt.checkpoint` proving it marks the
active attempt failed, resets once with a recovery deadline, and returns a terminal
result.

```go
func TestAttemptResultRetainsClassification(t *testing.T) {
    want := Outcome{Kind: OutcomeRails, Failure: "rail"}
    result := attemptResult{kind: attemptStopped, outcome: want, failure: "failure", retryable: true}
    if result.kind != attemptStopped || result.outcome.Kind != OutcomeRails ||
        result.failure != "failure" || !result.retryable {
        t.Fatalf("result = %#v", result)
    }
}
```

- [x] **Step 2: Verify RED**

Run:

```sh
go test ./internal/flywheel -run 'TestAttemptResultRetainsClassification|TestBatchAttemptCheckpoint'
```

Expected: compile failure because `attemptResult`, its kinds, `batchAttempt`, and
`checkpoint` do not exist.

- [x] **Step 3: Add attempt-scoped types and the linear pipeline**

Define:

```go
type attemptResultKind uint8

const (
    attemptSucceeded attemptResultKind = iota
    attemptSemanticFailure
    attemptInfrastructureFailure
    attemptStopped
)

type attemptResult struct {
    kind attemptResultKind
    failure string
    findings []finding.Finding
    retryable bool
    outcome Outcome
}

type batchAttempt struct {
    request Request
    ports Ports
    state *engineState
    index int
    number int
    retryFailure string
}
```

Move the brief-to-persist body into `batchAttempt.run(ctx) attemptResult`. Add
methods for `checkpoint`, failure/reset, semantic failure recording, and completed
commit persistence. Preserve each existing side-effect order exactly and add the
snapshot, validation/proof, and post-commit compensation comments from checklist
item 8 at those sites.

- [x] **Step 4: Centralize the retry epilogue**

Rewrite `executeBatch` as the admission loop over `1..maxAttempts`. It creates a
`batchAttempt`, calls `run`, and has one switch that performs retry, stuck, errored,
or continuation decisions. Return a named `executionResult` instead of
`(Outcome, bool)`, and update the `Execute` caller. Remove the unreachable panic,
the dead retryable assignment, and the old parameter-caravan helpers.

```go
type executionResult struct {
    outcome *Outcome
}

func continuedExecution() executionResult { return executionResult{} }
func stoppedExecution(outcome Outcome) executionResult {
    return executionResult{outcome: &outcome}
}
```

- [x] **Step 5: Verify GREEN and commit**

```sh
gofmt -w internal/flywheel/engine.go internal/flywheel/engine_test.go
go test ./internal/flywheel
go test ./features/gauntlet
git add internal/flywheel
git commit -m "Separate engine attempts from retry policy"
```

Expected: PASS with the existing event order unchanged. Mark checklist items 1, 2,
3, 5, 6, the relevant item 7 bullets, and the three item 8 comments complete.

### Task 3: Isolate Barrier Evaluation

**Files:**
- Modify: `internal/flywheel/engine.go`
- Modify: `internal/flywheel/engine_test.go`
- Modify: `internal/flywheel/engine.go.cleanup.md`

- [x] **Step 1: Write a failing barrier-helper test**

Add a direct `evaluateBarrier` test with invalid findings and an `engineAudit`.
Assert it returns `OutcomeErrored`, includes `classify barrier blockers`, and writes
the plan exactly once. Add cases for context cancellation, rail exhaustion,
invalid validation, infrastructure failure, semantic failure, stuck waves, ready,
stalemate, and a shrinking result that returns the next blocker multiset.

- [x] **Step 2: Verify RED**

Run:

```sh
go test ./internal/flywheel -run TestEvaluateBarrier
```

Expected: compile failure because `evaluateBarrier` and its named result do not exist.

- [x] **Step 3: Extract barrier classification and persistence**

Introduce the exact named result and helper signature below. Move context/rail
admission, blocker classification, result validation, single persistence, terminal
kind selection, stuck, ready, and shrink decisions into `evaluateBarrier`. Keep
next-wave creation and plan growth in `Execute`.

```go
type barrierResult struct {
    outcome *Outcome
    blockers map[string]int
}

func evaluateBarrier(
    ctx context.Context,
    request Request,
    state *engineState,
    barrier ValidationResult,
    before map[string]int,
    waveStuck bool,
) barrierResult
```

- [x] **Step 4: Remove the rail wrapper and verify GREEN**

Call `request.Rails.ExecutionContext(ctx)` directly and delete
`railExecutionContext`. Run:

```sh
gofmt -w internal/flywheel/engine.go internal/flywheel/engine_test.go
go test ./internal/flywheel
go test ./features/gauntlet
```

Expected: PASS. Mark checklist item 4 and the rail-wrapper bullet in item 7 complete.

- [x] **Step 5: Commit**

```sh
git add internal/flywheel
git commit -m "Isolate engine barrier evaluation"
```

### Task 4: Close The Checklist And Verify The Refactor

**Files:**
- Modify: `internal/flywheel/engine.go.cleanup.md`
- Inspect: `internal/flywheel/engine.go`
- Inspect: `internal/flywheel/engine_test.go`

- [x] **Step 1: Audit every cleanup item**

For each numbered heading, replace `## N.` with `## N. [x]` only when every
requested change under that heading is present. Convert item 7 bullets to `- [x]`
individually. Confirm item 8 has all three comments in production code.

- [x] **Step 2: Run formatting, static diff checks, and the full matrix**

```sh
gofmt -w internal/flywheel/engine.go internal/flywheel/engine_test.go internal/flywheel/plan.go internal/flywheel/plan_test.go
git diff --check main...HEAD
go build ./...
go test ./...
go test ./features/... -args -acceptance.driver=cli
go test ./features/... -args -acceptance.driver=all
```

Expected: every command exits 0.

- [x] **Step 3: Commit the completed checklist**

```sh
git add internal/flywheel/engine.go.cleanup.md
git commit -m "Complete engine cleanup checklist"
```

- [ ] **Step 4: Request independent review**

Spawn a fresh agent with no implementation history. Instruct it to read
`internal/flywheel/engine.go.cleanup.md` and the design, inspect `main...HEAD`, and
report correctness regressions, missed checklist items, ordering changes, and test
gaps with file/line references. Fix all Critical and Important findings, re-run the
full verification matrix, and commit any corrections before handoff.
