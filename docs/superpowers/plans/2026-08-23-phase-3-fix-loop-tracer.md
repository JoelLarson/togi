# Phase 3 Fix-Loop Tracer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first safe Codex-driven fix loop, from a required green baseline through isolated validated batches and a guarded one-commit landing.

**Architecture:** `run.Service` remains the application coordinator and report owner. `adapter` owns the headless Codex process contract; `flywheel` owns deterministic batching, the cache worktree, integrity, validation, retries, rails, and landing through narrow ports. Report-only execution stays on its existing path.

**Tech Stack:** Go 1.25, Cobra, stdlib AST packages, Git plumbing through `internal/gitcmd`, subprocesses through `internal/runner`, and Godog acceptance specifications. Add no production dependency.

**Design:** `docs/superpowers/specs/2026-08-23-phase-3-fix-loop-tracer-design.md`

---

## File Map

Create focused files rather than expanding the coordinator:

- `internal/adapter/adapter.go`, `codex.go`: provider-neutral contract and Codex JSONL implementation.
- `internal/flywheel/plan.go`, `brief.go`, `rails.go`: deterministic inputs and limits.
- `internal/flywheel/worktree.go`, `landing.go`: isolated Git lifecycle and guarded landing.
- `internal/flywheel/integrity.go`, `suppression.go`, `test_integrity.go`: anti-gaming checks.
- `internal/flywheel/validate.go`, `engine.go`: local validation and serial state machine.
- `internal/run/suite.go`, `fix.go`: behavioral-suite execution and application orchestration.
- `features/internal/harness/agent_tool.go`: fake Codex executable.
- `features/gauntlet/fixing_a_feature_diff.feature` and adjacent `_test.go`: user contract.

Modify `runner` for stdin, `gitcmd` for explicit commit identity, `config.Paths` for cache worktrees, and `run`/CLI/report/ledger files for Phase 3. Every new production file gets a same-package test file.

## Working Rules

- Complete one task and commit before starting the next.
- Write a focused failing test, observe the expected failure, add the minimum implementation, then run the owning package and `go test ./...`.
- Automated tests use fake Codex and gate executables only.
- Existing report-only service tests must pass `ReportOnly: true`; do not preserve old behavior through a zero-value fallback.
- Never persist state inside the target repository.

---

### Task 1: Publish Fix-Mode CLI And Verdict Contracts

**Files:**
- Modify: `internal/run/run.go`
- Modify: `internal/run/report.go`
- Modify: `internal/run/exit.go`
- Modify: `internal/run/exit_test.go`
- Modify: `internal/run/run_test.go`
- Modify: `cmd/togi/command.go`
- Modify: `cmd/togi/command_test.go`

- [ ] **Step 1: Write failing CLI and exit tests**

Add a recording fake-service test for:

```go
want := runpkg.Options{
    Root: ".", Agent: "codex", MaxIterations: 7,
    MaxWallClock: 12 * time.Minute,
}
cmd.SetArgs([]string{
    "run", "--agent", "codex",
    "--max-iterations", "7", "--max-wall-clock", "12m",
})
```

Add cases proving `run` without an agent, report-only plus agent/explicit rails, unsupported agents, and nonpositive rails fail before `service.Run`. Extend exit tests for codes 2, 3, and 6.

- [ ] **Step 2: Run the tests and observe failure**

Run:

```sh
go test ./cmd/togi ./internal/run -run 'TestRunCommand.*Fix|TestResolveExit|TestExitCode'
```

Expected: FAIL because the flags, fields, verdicts, and exit 6 are absent.

- [ ] **Step 3: Add the contracts**

Use these exact declarations:

```go
const (
    DefaultMaxIterations = 20
    DefaultMaxWallClock = 30 * time.Minute
)

type Options struct {
    Root string
    Base string
    GateNames []string
    ReportOnly bool
    Agent string
    MaxIterations int
    MaxWallClock time.Duration
    Verbose bool
    NoColor bool
}

const (
    VerdictUnverified Verdict = "unverified"
    VerdictFindings Verdict = "findings"
    VerdictBlocked Verdict = "blocked"
    VerdictRails Verdict = "rails"
    VerdictErrored Verdict = "errored"
    VerdictUnsealed Verdict = "unsealed"
)
```

Map verdicts to exits `1..6` and allow `ResolveExit` through 6. Use Cobra's `Flags().Changed` to distinguish explicit report-only rail flags from shipped defaults. Update existing report-only tests explicitly.

- [ ] **Step 4: Verify and commit**

```sh
go test ./cmd/togi ./internal/run
go test ./...
git add cmd/togi internal/run
git commit -m "Publish fix-loop CLI contracts"
```

Expected: PASS.

---

### Task 2: Add Stdin-Aware Execution And The Codex Adapter

**Files:**
- Modify: `internal/runner/runner.go`
- Modify: `internal/runner/runner_test.go`
- Create: `internal/adapter/adapter.go`
- Create: `internal/adapter/codex.go`
- Create: `internal/adapter/codex_test.go`
- Modify: `internal/adapter/doc.go`

- [ ] **Step 1: Test and implement runner stdin**

Add `Stdin io.Reader` to `runner.Options`, set `cmd.Stdin = opts.Stdin`, and prove a helper process receives `"batch brief\n"`. Nil must preserve existing callers.

Run `go test ./internal/runner -run TestRunSuppliesStdin`; observe FAIL first, then PASS.

- [ ] **Step 2: Write failing adapter contract tests**

Define:

```go
type Sink interface { WriteAdapterJSONL([]byte) error }

type Request struct {
    Root string
    Brief string
    Sink Sink
}
type Usage struct {
    InputTokens int64 `json:"input_tokens"`
    CachedInputTokens int64 `json:"cached_input_tokens"`
    OutputTokens int64 `json:"output_tokens"`
}
type Result struct { Usage *Usage }
type Adapter interface {
    Name() string
    Run(context.Context, Request) (Result, error)
}
type Error struct {
    Retryable bool
    Err error
}
```

Test exact argv/cwd/stdin, JSONL persistence, `turn.completed` usage, absent completion, malformed/truncated output, process failure, and missing executable classification.

- [ ] **Step 3: Run tests and observe failure**

Run: `go test ./internal/adapter`

Expected: FAIL because the contract is absent.

- [ ] **Step 4: Implement Codex**

Invoke exactly:

```go
[]string{
    executable, "exec", "--ephemeral", "--json",
    "--sandbox", "workspace-write",
    "--ask-for-approval", "never",
    "--ignore-user-config",
    "--cd", request.Root, "-",
}
```

Use the brief as stdin and 1 MiB bounded streams. Persist stdout even on failure. Decode one JSON object per line, require `turn.completed`, and expose only optional usage. Missing executable is non-retryable; process, timeout, malformed stream, and incomplete completion are retryable.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/runner ./internal/adapter
go test ./...
git add internal/runner internal/adapter
git commit -m "Add the Codex process adapter"
```

---

### Task 3: Add Cache Paths And Private Ledger Artifacts

**Files:**
- Modify: `internal/config/paths.go`
- Modify: `internal/config/paths_test.go`
- Modify: `internal/run/ledger.go`
- Modify: `internal/run/ledger_test.go`

- [ ] **Step 1: Test cache worktree paths**

Require:

```go
paths.WorktreesDir(id) ==
    filepath.Join(cacheHome, "togi", id.Key(), "worktrees")
paths.WorktreeDir(id, "run-id") ==
    filepath.Join(cacheHome, "togi", id.Key(), "worktrees", "run-id")
```

Reuse `validPathComponent`; test traversal, absolute, Windows-style, zero path, and zero ID cases.

- [ ] **Step 2: Implement path methods and verify**

Methods compute paths only and create nothing. Run `go test ./internal/config`.

- [ ] **Step 3: Write failing artifact tests**

Add:

```go
func (r *RunLedger) WritePlan(raw []byte) error
func (r *RunLedger) WriteBrief(batchID string, attempt int, raw []byte) error
func (r *RunLedger) WriteAdapterJSONL(batchID string, attempt int, raw []byte) error
```

Prove atomic replacement of `plan.json`, immutable brief/log files, 1 MiB adapter-log bounds, `0600` files, unsafe IDs rejected, retained-root confinement, and `ErrClosed`.

- [ ] **Step 4: Implement artifact storage**

Create private `briefs/` and `adapter/` roots during `Ledger.Start`. Derive attempt filenames from SHA-256 of `(batchID, attempt)`. Reuse the ledger's temporary-file/sync/rename pattern for plans.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/config ./internal/run -run 'Worktree|Plan|Brief|Adapter'
go test ./...
git add internal/config internal/run
git commit -m "Persist private fix-loop artifacts"
```

---

### Task 4: Discover And Execute The Go Behavioral Suite

**Files:**
- Create: `internal/run/suite.go`
- Create: `internal/run/suite_test.go`

- [ ] **Step 1: Write discovery and result tests**

Use:

```go
type SuiteStatus string
const (
    SuitePassed SuiteStatus = "passed"
    SuiteFailed SuiteStatus = "failed"
    SuiteMissing SuiteStatus = "missing"
    SuiteErrored SuiteStatus = "errored"
)
type SuiteResult struct {
    Command []string `json:"command"`
    Packages []string `json:"packages,omitempty"`
    Status SuiteStatus `json:"status"`
    DurationMS int64 `json:"duration_ms"`
    Diagnostic string `json:"diagnostic,omitempty"`
}
```

Prove `TestXxx`, `ExampleXxx`, and `FuzzXxx` count; benchmarks alone, malformed signatures, empty test files, `vendor`, and `testdata` do not.

- [ ] **Step 2: Run tests and observe failure**

Run: `go test ./internal/run -run 'Suite|DiscoverGo'`

Expected: FAIL.

- [ ] **Step 3: Implement AST discovery and execution**

Parse `*_test.go` through stdlib AST. Full verification runs `go test ./...`; local verification receives sorted unique `./relative/package` arguments. Classify no target as missing, nonzero test exit as failed, start/teardown failures as errored, and rail context cancellation through a sentinel the caller can map to rails. Bound combined diagnostics.

- [ ] **Step 4: Add execution matrix tests**

Fake pass, test failure, missing `go`, cancellation, truncation, stable ordering, and root-package behavior.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/run -run 'Suite|DiscoverGo'
go test ./...
git add internal/run/suite.go internal/run/suite_test.go
git commit -m "Add Go behavioral suite verification"
```

---

### Task 5: Build Plans, Briefs, Progress, And Rails

**Files:**
- Create: `internal/flywheel/plan.go`, `plan_test.go`
- Create: `internal/flywheel/brief.go`, `brief_test.go`
- Create: `internal/flywheel/rails.go`, `rails_test.go`
- Modify: `internal/flywheel/doc.go`

- [ ] **Step 1: Test deterministic plans**

Define:

```go
type BatchStatus string
const (
    BatchPending BatchStatus = "pending"
    BatchRunning BatchStatus = "running"
    BatchDone BatchStatus = "done"
    BatchStuck BatchStatus = "stuck"
)
type Attempt struct {
    Number int `json:"number"`
    Status string `json:"status"`
    Failure string `json:"failure,omitempty"`
    ChangedFiles []string `json:"changed_files,omitempty"`
    Commit string `json:"commit,omitempty"`
}
type Batch struct {
    ID string `json:"id"`
    PrimaryFile string `json:"primary_file"`
    Findings []finding.Finding `json:"findings"`
    Status BatchStatus `json:"status"`
    Attempts []Attempt `json:"attempts"`
}
type Plan struct {
    SchemaVersion int `json:"schema_version"`
    Batches []Batch `json:"batches"`
}
```

Test primary-file grouping, stable ordering, digest IDs, deep copies, and blocker multisets keyed by `(fingerprint, 1+len(Occurrences))`. Equal/larger/rotated sets do not shrink.

- [ ] **Step 2: Implement plan and progress functions**

Export `NewPlan`, `BlockingMultiset`, and `StrictlyShrinks`. Reject invalid/ungrouped findings and never mutate inputs.

- [ ] **Step 3: Golden-test and implement briefs**

Use:

```go
type BriefInput struct {
    MergeBase string
    OriginalHead string
    Batch Batch
    RetryFailure string
}
func BuildBrief(BriefInput) (string, error)
```

The golden text states full diff scope, cross-file permission, integrity, Togi Git ownership, findings/occurrences, and deterministic retry failure. It contains no raw gate output.

- [ ] **Step 4: Test and implement rails**

Use:

```go
type RailConfig struct {
    MaxIterations int
    MaxWallClock time.Duration
}
func NewRails(RailConfig, func() time.Time) (*Rails, error)
func (r *Rails) AdmitAttempt() error
func (r *Rails) AdmitLanding() error
func (r *Rails) Snapshot() RailSnapshot
```

`AdmitAttempt` increments only when admitted. `ErrRailExhausted` identifies iteration or wall-clock. Test exact boundaries with a fake clock.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/flywheel -run 'Plan|Brief|Rail|Progress'
go test ./...
git add internal/flywheel
git commit -m "Plan deterministic fix batches"
```

---

### Task 6: Own The Cache Worktree And Batch Commits

**Files:**
- Modify: `internal/gitcmd/gitcmd.go`, `gitcmd_test.go`
- Create: `internal/flywheel/worktree.go`, `worktree_test.go`

- [ ] **Step 1: Test and implement explicit Git environment**

Add:

```go
func OutputEnv(
    ctx context.Context, dir string, iso Isolation, limit int,
    extra map[string]string, args ...string,
) ([]byte, error)
```

Test explicit author/committer values, input immutability, ambient `GIT_*` stripping, and NUL/invalid-name rejection. Keep `Output` as a wrapper.

- [ ] **Step 2: Write worktree lifecycle tests**

Define:

```go
type Identity struct { Name string; Email string }
type WorkspaceSpec struct {
    RepositoryRoot string
    Path string
    RunID string
    OriginalHead string
    FeatureBranch string
    Identity Identity
}
```

Test `togi/run-<id>` at original HEAD, external path enforcement, collisions, clean state, and no feature-worktree change.

- [ ] **Step 3: Implement creation**

Use `git worktree add -b togi/run-<id> <path> <original-head>`. Validate branch, HEAD, common repository, and clean status.

- [ ] **Step 4: Test and implement batch operations**

`ChangedFiles` returns sorted relative paths including renames. `ResetAttempt` removes tracked/untracked edits back to the latest green commit. `CommitBatch` stages the whole observed tree, uses `togi batch: <primary-file>`, supplies identity, disables hooks, and advances the green commit only after success.

- [ ] **Step 5: Protect Git control state**

Snapshot workspace HEAD, index tree, run ref, worktree registrations, repository config digest, and refs. Reject staging, agent commits, new refs/tags/worktrees, config edits, or moved refs. Restore only through exact compare-and-swap; never rewind a possibly concurrent operator ref.

- [ ] **Step 6: Verify and commit**

```sh
go test ./internal/gitcmd ./internal/flywheel -run 'OutputEnv|Workspace|Batch|GitState'
go test ./...
git add internal/gitcmd internal/flywheel
git commit -m "Isolate fix batches in a cache worktree"
```

---

### Task 7: Implement The Serial Flywheel State Machine

**Files:**
- Create: `internal/flywheel/engine.go`, `engine_test.go`
- Modify: `internal/flywheel/plan.go`

- [ ] **Step 1: Write engine tests with fakes**

Use narrow ports:

```go
type Audit interface {
    WritePlan([]byte) error
    WriteBrief(batchID string, attempt int, raw []byte) error
    AdapterSink(batchID string, attempt int) (adapter.Sink, error)
}
type ValidationKind string
const (
    ValidationPassed ValidationKind = "passed"
    ValidationSemanticFailure ValidationKind = "semantic-failure"
    ValidationInfrastructureFailure ValidationKind = "infrastructure-failure"
)
type ValidationResult struct {
    Kind ValidationKind
    Failure string
    Findings []finding.Finding
    ChangedFiles []string
}
type WorkspacePort interface {
    Root() string
    ChangedFiles(context.Context) ([]string, error)
    ResetAttempt(context.Context) error
    CommitBatch(context.Context, string) (string, error)
}
type Ports struct {
    Adapter adapter.Adapter
    Workspace WorkspacePort
    Audit Audit
    Validate func(context.Context, Batch) ValidationResult
    Barrier func(context.Context) ValidationResult
}
```

Test serial calls, fresh attempts, plan writes, nonempty diff, commit after validation, one semantic retry, retry note, second semantic failure becoming stuck while later batches continue, repeated infrastructure failure becoming errored, and rail exhaustion before invocation.

- [ ] **Step 2: Run tests and observe failure**

Run: `go test ./internal/flywheel -run Engine`

Expected: FAIL.

- [ ] **Step 3: Implement attempts and outcomes**

Use:

```go
type Request struct {
    MergeBase string
    OriginalHead string
    InitialFindings []finding.Finding
    Rails *Rails
}
type OutcomeKind string
const (
    OutcomeReady OutcomeKind = "ready"
    OutcomeBlocked OutcomeKind = "blocked"
    OutcomeRails OutcomeKind = "rails"
    OutcomeErrored OutcomeKind = "errored"
)
type Outcome struct {
    Kind OutcomeKind
    Plan Plan
    Findings []finding.Finding
    Iterations int
    Failure string
}
```

Persist indented plan JSON plus newline before invocation and after transitions. Reset every failed attempt before retry/return.

- [ ] **Step 4: Add and implement barrier waves**

A strictly smaller post-wave blocker set creates another deterministic wave. Equal, larger, or rotated sets are stalemate. Any stuck batch blocks after barrier recording. A blocker-free barrier returns ready.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/flywheel -run Engine
go test ./...
git add internal/flywheel
git commit -m "Execute fix batches through the serial flywheel"
```


---

### Task 8: Enforce Suppression Integrity

**Files:**
- Create: `internal/flywheel/integrity.go`, `integrity_test.go`
- Create: `internal/flywheel/suppression.go`, `suppression_test.go`

- [ ] **Step 1: Test deterministic integrity findings**

Centralize:

```go
func integrityFinding(
    ruleID, file string, line int, snippet, message string,
) finding.Finding
```

Use gate `integrity`, language `go`, severity `error`, and tool-qualified IDs such as `togi/new-suppression`. Pass results through `finding.Group`.

- [ ] **Step 2: Write suppression matrix tests**

Compare original and attempted trees. Cover additions, movement, duplication, and broadening of `//nolint`, `//lint:ignore`, `#nosec`, `Skip`, `SkipNow`, `//go:build`, and `// +build`. Unchanged original suppressions pass. Any content change to tracked `.golangci.yml`, `.golangci.yaml`, `.golangci.toml`, or `.golangci.json` fails.

- [ ] **Step 3: Run tests and observe failure**

Run: `go test ./internal/flywheel -run 'IntegrityFinding|Suppression'`

Expected: FAIL.

- [ ] **Step 4: Implement syntax-aware comparison**

Use AST for calls/build constraints and normalized comments for lint/security directives. Compare path and enclosing declaration so moving a directive is visible. Malformed changed Go syntax is an infrastructure error, never zero violations.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/flywheel -run 'IntegrityFinding|Suppression'
go test ./...
git add internal/flywheel
git commit -m "Block new suppression mechanisms"
```

---

### Task 9: Protect Test Discovery, Behavior, And Fixtures

**Files:**
- Create: `internal/flywheel/test_integrity.go`, `test_integrity_test.go`
- Create fixtures: `internal/flywheel/testdata/integrity/*.go`

- [ ] **Step 1: Write discovery and fixture tests**

Cover deleted/renamed `TestXxx`, `BenchmarkXxx`, `FuzzXxx`, and `ExampleXxx`; invalid signatures; package/build exclusion; skip insertion; existing `testdata` change/deletion; and allowed new tests/fixtures.

- [ ] **Step 2: Write behavior tests**

Changing literals, expectations, operators, callees, arguments, cases, assertions, statement order, control flow, or existing comment text must produce `togi/test-behavior`. Formatting-only changes pass.

- [ ] **Step 3: Write witnessed-rename tests**

Prove these cases:

```text
production calculateTotal -> totalFor + matching test call  allowed
production package rename + matching test import            allowed
test-only identifier rename                                 blocked
one old declaration matching two new declarations           blocked
rename plus changed expected literal                        blocked
test discovery function rename                              blocked
```

- [ ] **Step 4: Run tests and observe failure**

Run: `go test ./internal/flywheel -run 'TestIntegrity|WitnessedRename|Fixture'`

Expected: FAIL.

- [ ] **Step 5: Implement structural snapshots**

Use:

```go
type TreeSnapshot struct { Files map[string][]byte }
type IntegrityResult struct {
    Findings []finding.Finding
    Err error
}
func CheckIntegrity(original, attempted TreeSnapshot) IntegrityResult
```

Add `SnapshotOriginal(ctx, repository, originalHead)` using `git ls-tree -rz`
and `git show`, plus `SnapshotAttempt(root)` using a path-confined filesystem
walk. Include all Go test files, existing `testdata` files, recognized tool
configs, and changed production Go files needed to prove renames. Normalize
production declarations and test bodies into AST token structures. A rename
witness exists only when old/new production declarations are structurally
equal after replacing the declared name. Apply unambiguous one-to-one mappings
to original test identifier/package tokens, then require equality. Compare
existing `testdata` bytes exactly. Fail closed on ambiguity or parse errors.

- [ ] **Step 6: Verify and commit**

```sh
go test ./internal/flywheel -run 'TestIntegrity|WitnessedRename|Fixture'
go test ./...
git add internal/flywheel
git commit -m "Protect tests while permitting witnessed renames"
```

---

### Task 10: Validate Actual Batch Changes Locally

**Files:**
- Create: `internal/flywheel/validate.go`, `validate_test.go`
- Modify: `internal/run/executor.go`, `executor_test.go`
- Modify: `internal/run/suite.go`, `suite_test.go`

- [ ] **Step 1: Test changed-package selection**

Define:

```go
func ChangedGoPackages(
    root string, changedFiles []string,
) (packages []string, full bool, err error)
```

`go.mod` or `go.sum` means `full=true`. Otherwise return sorted unique existing package directories containing changed Go files. Map deleted files to a surviving package in that directory. Reject escaping/symlink paths. Non-Go-only edits return no local package.

- [ ] **Step 2: Test gate selection**

Extract:

```go
func validationRequests(
    requests []Request, assigned []finding.Finding,
) []Request
```

Include all instant/fast gates plus slower gates owning assigned findings, retaining gauntlet order.

- [ ] **Step 3: Write validator tests**

Use neutral ports:

```go
type GateValidation struct {
    Blocking []finding.Finding
    Errored []string
}
type SuiteValidation struct {
    Passed bool
    InfrastructureError string
}
type AttemptValidator struct {
    Original TreeSnapshot
    RunGates func(context.Context, Batch) GateValidation
    RunPackages func(context.Context, []string, bool) SuiteValidation
}
```

Reject no-op edits, integrity findings, gate errors, persistent assigned findings, replacement blockers, non-shrinking multisets, and local test failure. Cross-file edits expand packages; module edits request full suite.

- [ ] **Step 4: Implement deterministic validation**

Failures given to retries are stable, repository-relative, bounded, and contain no raw output. Tool execution problems become infrastructure failures; code, test, finding, and integrity regressions become semantic failures.

- [ ] **Step 5: Verify and commit**

```sh
go test ./internal/flywheel ./internal/run -run 'ChangedGoPackages|ValidationRequests|AttemptValidator'
go test ./...
git add internal/flywheel internal/run
git commit -m "Validate each fix batch locally"
```

---

### Task 11: Build The Squash Commit And Guarded Landing

**Files:**
- Create: `internal/flywheel/landing.go`, `landing_test.go`
- Modify: `internal/flywheel/worktree.go`, `worktree_test.go`

- [ ] **Step 1: Write squash tests**

Starting with multiple validated batch commits, require:

```text
tree:    latest validated tree
parent:  original HEAD only
subject: togi: apply verified fixes
author:  resolved operator identity
hooks:   disabled
```

- [ ] **Step 2: Implement squash construction**

Use `git commit-tree` with explicit identity. Verify parent/tree, then compare-and-swap the Togi run ref from latest batch to squash. Do not move the run ref until commit creation succeeds.

- [ ] **Step 3: Write landing guard tests**

Define:

```go
type LandingStatus string
const (
    LandingNotNeeded LandingStatus = "not-needed"
    LandingComplete LandingStatus = "complete"
    LandingBlocked LandingStatus = "blocked"
)
```

Prove success updates feature ref and files. Prove refusal for dirty checkout, detached HEAD, wrong branch, moved HEAD/ref, missing worktree, and non-fast-forward. Refusal preserves the run branch.

- [ ] **Step 4: Implement guarded landing**

Recheck canonical path, symbolic branch, porcelain-v2 clean status, worktree HEAD, and feature ref immediately before:

```sh
git -c core.hooksPath=/dev/null merge --ff-only <squash>
```

Use a separate 30-second Git context. Never force-update the checked-out feature ref from the cache worktree.

- [ ] **Step 5: Test and implement cleanup**

Success removes cache worktree then run branch. Blocked/rails/post-edit errored outcomes reset invalid edits, remove the worktree, and retain a branch only with validated commits. Cleanup is idempotent; partial failure is surfaced.

- [ ] **Step 6: Verify and commit**

```sh
go test ./internal/flywheel -run 'Squash|Landing|Cleanup'
go test ./...
git add internal/flywheel
git commit -m "Land validated fixes as one guarded commit"
```

---

### Task 12: Integrate Fix Mode And Report Schema 4

**Files:**
- Create: `internal/run/fix.go`, `fix_test.go`
- Modify: `internal/run/run.go`, `report.go`, `ledger.go`, `render.go`
- Modify: `internal/run/run_test.go`, `ledger_test.go`, `phase6_test.go`
- Modify: `cmd/togi/command.go`
- Modify: `features/internal/harness/observation.go`

- [ ] **Step 1: Write schema-4 tests**

Set `ReportSchemaVersion = 4` and add:

```go
type AgentReport struct {
    Name string `json:"name"`
    Usage *adapter.Usage `json:"usage,omitempty"`
}
type RailsReport struct {
    MaxIterations int `json:"max_iterations"`
    Iterations int `json:"iterations"`
    MaxWallClockMS int64 `json:"max_wall_clock_ms"`
    ElapsedMS int64 `json:"elapsed_ms"`
}
type LandingReport struct {
    Status string `json:"status"`
    Commit string `json:"commit,omitempty"`
    PreservedBranch string `json:"preserved_branch,omitempty"`
    Error string `json:"error,omitempty"`
}
type FixReport struct {
    OriginalHead string `json:"original_head"`
    FeatureBranch string `json:"feature_branch"`
    Agent AgentReport `json:"agent"`
    Baseline SuiteResult `json:"baseline"`
    Final *SuiteResult `json:"final,omitempty"`
    Rails RailsReport `json:"rails"`
    Batches []flywheel.Batch `json:"batches"`
    Integrity []finding.Finding `json:"integrity"`
    Landing LandingReport `json:"landing"`
}
```

Add `Fix *FixReport `json:"fix,omitempty"`` to `Report`. Require it for Phase 3 verdicts and forbid it for report-only verdicts. Update every strict fixture; keep `DisallowUnknownFields`.

- [ ] **Step 2: Write orchestration-order tests**

Inject suite, adapter lookup, flywheel construction, and clock seams. Assert:

```text
validate options -> prepare repo/diff/gates -> ledger/lock
baseline full suite -> initial parallel gates
stop unverified/errored -> no-blocker unsealed
identity/worktree -> flywheel waves -> all-gate barrier
final go test ./... -> rail admission -> squash/guard/land
report/render -> cleanup while lock held
```

- [ ] **Step 3: Implement run adapters**

`fix.go` translates gate/report types into flywheel-neutral results, allocates raw sinks for every rerun, recomputes merge-base-to-current-HEAD changed lines, and adapts ledger artifacts without importing `run` from `flywheel`.

- [ ] **Step 4: Implement exact classification**

```text
passed/no fixes or landed -> unsealed / 6
stuck/integrity/stalemate/final tests/landing guard -> blocked / 2
rail admission failure -> rails / 3
gate/adapter/suite/Git infrastructure -> errored / 4
missing or red baseline -> unverified / 5
cannot produce valid report -> 70
```

Initial gate errors stop the adapter. Final test exit is blocked; inability to execute the previously green suite is errored.
Construct the rails at ledger start so the baseline and initial gates consume
wall-clock budget. Admit landing before beginning Git mutation. A cleanup
failure before landing is errored; a failure after fast-forward records the
landing as complete, returns errored, and never claims rollback.

- [ ] **Step 5: Render Phase 3 summaries**

After compiler-style findings, render baseline/final suite, completed/total batches, attempts, rail use, landing, preserved branch, and verdict. Never render cache paths, raw JSONL, credentials, or briefs.

- [ ] **Step 6: Wire production Codex**

`defaultServices` installs only `adapter.NewCodex("codex")`. Do not probe it at command construction; a missing binary creates a ledger-backed errored run before edits.

- [ ] **Step 7: Verify and commit**

```sh
go test ./internal/run ./cmd/togi
go test ./...
git add internal/run cmd/togi features/internal/harness/observation.go
git commit -m "Integrate the Phase 3 fix loop"
```

---

### Task 13: Add Executable Phase 3 Acceptance Specifications

**Files:**
- Create: `features/internal/harness/agent_tool.go`, `agent_tool_test.go`
- Modify: `features/internal/harness/driver.go`, `service_driver.go`, `cli_driver.go`
- Modify: `features/internal/harness/observation.go`, `repository.go`
- Create: `features/gauntlet/fixing_a_feature_diff.feature`
- Create: `features/gauntlet/fixing_a_feature_diff_test.go`
- Modify: `features/gauntlet/steps_test.go`
- Modify: `features/README.md`

- [ ] **Step 1: Build a fake Codex executable**

Use:

```go
type AgentBehavior struct {
    Edits map[string]string
    Delete []string
    ExitCode int
    MalformedJSONL bool
    Sleep time.Duration
    GitArgs []string
}
```

It validates production argv, reads stdin, records invocations, edits cwd, and emits Codex JSONL. Test cross-file edit, deletion, no-op, malformed output, timeout, and attempted Git mutation.

- [ ] **Step 2: Extend both acceptance drivers**

Add `ReportOnly`, `Agent`, `MaxIterations`, and `MaxWallClock` to `harness.RunRequest`. Service and CLI drivers pass identical values. Existing scenarios call a named report-only helper.

- [ ] **Step 3: Author the feature**

Use these rules:

```gherkin
Feature: Fixing a feature diff safely
  Rule: Mutation starts only from a green and complete baseline
  Rule: Each agent batch earns a rollback commit through local validation
  Rule: Integrity prevents an agent from weakening the evidence
  Rule: Rails and stalemate bound unattended execution
  Rule: Only a guarded squash commit reaches the feature branch
```

Cover success, no blockers, missing agent, missing/red suite, initial gate error, cross-file edits, no-op retry/stuck, unauthorized Git, suppression, test deletion, assertion change, witnessed rename, both rails, final-suite regression, and dirty/detached/moved landing.

- [ ] **Step 4: Bind observable assertions**

Assert only exit/verdict, commit graph/tree, preserved branch, report/plan/brief artifacts, and invocation counts. Keep fake setup out of Gherkin wording.

- [ ] **Step 5: Verify both drivers and commit**

```sh
go test ./features/gauntlet -run TestFixingAFeatureDiff -v
go test ./features/gauntlet -run TestFixingAFeatureDiff -v -args -acceptance.driver=cli
go test ./features/gauntlet -run TestFixingAFeatureDiff -v -args -acceptance.driver=all
go test ./...
git add features
git commit -m "Specify the Phase 3 fix-loop behavior"
```

Expected: PASS with no silently excluded examples.

---

### Task 14: Reconcile Documentation And Complete Verification

**Files:**
- Modify: `CONTEXT.md`
- Modify: `docs/design.md`
- Modify: `docs/implementation.md`
- Modify: `docs/roadmap.md`
- Modify: `docs/adr/0010-togi-owned-worktree-squash-landing.md`
- Modify: `AGENTS.md`

- [ ] **Step 1: Reconcile source-of-truth wording**

Record exactly:

```text
unverified = baseline absent/red; no adapter
unsealed = Phase 3 passed without Phase 5 seal; exit 6
initial errored gate = report all gate signal, no adapter
Codex first; Claude/Kimi later conformance adapters
original worktree isolated during fixing, updated once at guarded landing
iteration and wall-clock rails now; spend deferred
witnessed compilation-only test renames allowed; behavior protected
```

Update roadmap status/exit criteria. Add a dated ADR-0010 refinement rather than erasing historical rationale.

- [ ] **Step 2: Update repository instructions**

Replace stale phase claims in `AGENTS.md` with implemented tracer behavior, verification, adapter-free tests, and next slices: waivers and additional adapters before Phase 4 triage.

- [ ] **Step 3: Format and statically verify**

```sh
gofmt -w cmd internal features
git diff --check
go vet ./...
```

Expected: no diff-check output; vet exits 0.

- [ ] **Step 4: Run the complete matrix**

```sh
go build ./...
go test ./...
go test ./features/... -v -args -acceptance.driver=cli
```

Expected: PASS without installed Codex or external gates.

- [ ] **Step 5: Run report-only dogfood**

```sh
go run ./cmd/togi run --report-only --no-color
```

Expected: schema-4 ledger/report. Exit 4 is acceptable when configured external gates are absent, but the report must show explicit gate errors.

- [ ] **Step 6: Inspect state and commit**

```sh
git status --short
git branch --list 'togi/run-*'
git add AGENTS.md CONTEXT.md docs
git commit -m "Reconcile the Phase 3 source of truth"
```

Expected before commit: only intended docs; no cache worktree, temporary branch, or generated binary.

- [ ] **Step 7: Final verification**

```sh
go build ./...
go test ./...
git status --short
```

Expected: PASS and clean worktree.

---

## Completion Check

The implementation is complete only when automated tests demonstrate:

- No adapter runs without a green baseline and healthy initial gates.
- Codex receives a deterministic stdin brief in an external worktree; Togi judges the observed diff.
- Primary-file batches may edit related files, and actual changed packages receive local tests.
- Accepted batches strictly improve blockers, pass integrity, and create rollback commits.
- Failed attempts reset fully and retry once; infrastructure failure remains distinct from stuck code.
- Suppressions, test weakening, fixture changes, and Git ownership violations block; witnessed compilation-only renames pass.
- Rails bound attempts and elapsed work.
- Final all-gate/full-suite barriers precede one guarded squash fast-forward.
- Success is `unsealed`/6, never `merge-ready`/0.
- Build, unit tests, and compiled-CLI acceptance pass without real agent or gate installations.
