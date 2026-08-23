# Phase 3 Fix-Loop Tracer Design

## Purpose

This design delivers the first end-to-end Phase 3 tracer: start from a clean,
committed feature branch; judge its merge-base-to-HEAD diff; let a headless
agent repair blocking findings in an isolated worktree; validate every small
batch; and land one guarded squash commit. The tracer proves the safety model
before Phase 4 replaces file-based batching with intelligent triage.

The feature branch is expected to contain the committed output of the
upstream gather, build, and verify workflow. Togi records that branch's HEAD as
the **original HEAD**. Everything from the merge base through the evolving
togi branch remains in judgment scope, while integrity checks use original
HEAD as their witness so upstream feature work is not mistaken for a Togi
edit.

This slice is deliberately focused on improving the feature diff, not the
entire repository. A primary file guides each batch, but the agent may edit
related files anywhere in the repository. Validation is localized after each
batch for speed and becomes repository-wide at the final barrier.

## Source-of-Truth Refinements

The approved behavior refines several statements in the current design
documents. The implementation must update those documents and their tests in
the same change rather than silently carrying both meanings:

- A missing or red behavioral suite stops fix mode before any adapter call.
  Fixes no longer proceed under an `unverified` cap.
- An initial errored quality gate stops fix mode before any adapter call. The
  other gates still complete and report their signal, but the tracer does not
  mutate under incomplete judgment.
- Codex is the first production adapter. Claude and Kimi remain later
  conformance implementations of the same process boundary.
- `unsealed` is a new successful Phase 3 verdict with exit code 6. Exit 0 and
  `merge-ready` remain unreachable until Phase 5 supplies a passing seal.
- The feature worktree remains untouched throughout fixing, then is updated
  once by a guarded fast-forward at landing. It must still be clean, on the
  recorded branch, and at original HEAD. This refines ADR-0010's broader claim
  that the user's checkout is never touched.

## User Contract

Fix mode is the default form of `togi run` and requires an explicit adapter:

```text
togi run --agent codex [--max-iterations 20] [--max-wall-clock 30m]
```

`--agent` initially accepts only `codex`. Omitting it from fix mode is a CLI
usage error detected before ledger creation or gate execution. `--report-only`
remains adapter-free and preserves its current behavior. Combining
`--report-only` with `--agent` or either fix-loop rail is rejected as
contradictory.

The shipped rails are:

- `--max-iterations 20`, where every adapter attempt, including a retry,
  consumes one iteration;
- `--max-wall-clock 30m`, measured from ledger creation and enforced as an
  admission deadline for adapter calls, validators, and landing.

Both values must be positive. Reaching either limit cancels active adapter or
validation child processes, removes unvalidated edits, and returns `rails`.
Rollback, a landing transaction admitted before the deadline, and cleanup run
under a separate 30-second Git-operation timeout instead of being interrupted
halfway through Git state changes. Codex token usage is recorded when present
but does not control execution because a subscription CLI does not expose
meaningful API spend.

## Architecture And Ownership

`run.Service` remains the application coordinator. It owns repository
preparation, the advisory lock, ledger lifetime, initial and final barriers,
report composition, rendering, and verdict selection. Report-only execution
continues through the existing path.

The Phase 3 path composes two glossary-owned packages:

- `adapter` defines a vendor-neutral request/result contract and implements
  the Codex process adapter. It owns process invocation and JSONL parsing but
  has no authority to accept edits.
- `flywheel` owns deterministic file batching, serial attempts, retry and
  stuck state, progress comparison, rails, the cache worktree, validated batch
  commits, rollback, integrity evaluators, squash construction, and guarded
  landing.

The flywheel receives immutable run facts and narrow validation callbacks. It
does not import `run`, render output, persist a final report, or choose process
exit codes. `run` may import both `adapter` and `flywheel`; neither imports
`run`. Existing `gate`, `runner`, `gitcmd`, `finding`, and `config` capabilities
remain the lower-level boundaries. `triage` stays untouched in this slice.

No new production package or domain term is introduced. The naive plan is an
**action plan**, and its file groups are **batches**, so the existing glossary
already names the new behavior.

## Preflight And Baseline Barrier

Existing repository eligibility rules remain in force: the checkout must be a
supported, clean Git repository with an admissible feature diff and no tracked
submodules. Fix mode additionally resolves and records:

- the canonical repository identity and feature worktree path;
- the feature branch's symbolic name, original HEAD, base ref, and merge base;
- an operator Git name and email for Togi-owned commits, resolved from
  repository-local then global `user.name` and `user.email` configuration;
- the selected adapter and rail values;
- a cache worktree path below
  `$XDG_CACHE_HOME/togi/<repo-id>/worktrees/<run-id>`.

A missing Git identity is reported as `errored` before Codex runs. The
repository lock and ledger are created before the baseline barrier so every
domain result has an audit artifact.

For Go repositories, the initial suite command is fixed at `go test ./...`.
Togi first discovers at least one runnable Go test, example, or fuzz target;
benchmarks alone do not make the default `go test` command a behavioral suite.
No suite, a nonzero result, timeout, or execution failure produces
`unverified`, and Codex is not invoked.

All selected quality gates run against merge-base through original HEAD. They
retain the current parallel collector behavior so one error does not suppress
healthy results. If any gate is `errored`, the run returns `errored` without
invoking Codex. If the suite is green, all gates are healthy, and no blocking
finding exists, Togi creates neither worktree nor commit and returns
`unsealed`.

## Action Plan And Briefs

Blocking findings are grouped by their primary repository-relative file.
Files and findings use the existing deterministic report ordering. One primary
file becomes one batch. A grouped finding may contain occurrences in other
files; the whole grouped finding belongs to its primary-file batch.

The initial action plan is written to `plan.json` before the first adapter
call. It contains the ordered batches, embedded findings, and
`pending`/`running`/`done`/`stuck` states. Status is atomically updated after
each attempt. This slice persists enough state for audit and later resume
support but does not expose a resume command.

Each attempt receives a fresh deterministic brief containing:

- the primary file and assigned blocking findings, including occurrences and
  file:line pointers;
- the original merge base and original HEAD;
- the rule that the full feature diff remains in judgment scope;
- permission to edit related files anywhere in the repository;
- the applicable repository instructions;
- the integrity rules and the statement that Togi exclusively owns Git state;
- on retry, a deterministic summary of the previous validation failure.

The brief contains only normalized finding material, not raw gate output. It
is written to the ledger before being sent to the adapter.

## Codex Adapter Contract

Every attempt launches a new Codex process with no persistent session:

```sh
codex --ask-for-approval never exec \
  --ephemeral \
  --json \
  --sandbox workspace-write \
  --ignore-user-config \
  --cd <cache-worktree> \
  -
```

The brief is supplied on stdin. Saved Codex authentication and repository
instruction files remain available, while user customization is ignored. The
workspace-write boundary makes the worktree writable while Git administrative
state remains outside the adapter's writable root. Togi also snapshots Git
control state as defense in depth.

The adapter persists bounded JSONL in the private run ledger and parses only
completion status, errors, and optional usage. Agent prose never determines
success. The proposal is the worktree diff observed by Togi after the process
exits.

A missing executable or invalid adapter setup is non-retryable and returns
`errored`. A process failure, timeout, malformed stream, or incomplete result
is retryable once when the wall-clock and iteration rails permit it.

The adapter interface contains no Codex-specific types. Later Claude and Kimi
implementations must be able to satisfy it without changing flywheel behavior.

## Worktree And Git Ownership

The flywheel creates `togi/run-<id>` at original HEAD and checks it out only
in the external cache worktree. Each attempt starts from the latest validated
batch commit. The agent may read Git history but may not create commits, move
or create refs, add worktrees, alter configuration, stage files, or otherwise
modify Git control state.

Togi compares the post-attempt Git state with its snapshot. An unauthorized
Git mutation invalidates the attempt. Togi restores only state proven to have
been changed by the attempt using compare-and-swap checks; it never rewinds a
ref that may have been moved concurrently by the operator. If safe restoration
cannot be proven, the run returns `errored` and does not touch the feature
worktree. The retry brief states that Togi exclusively owns Git state.

After an attempt passes every validator, Togi commits the complete observed
worktree change as `togi batch: <primary-file>`. Batch commits are rollback
points, not user-visible history. Togi supplies the resolved operator identity
directly and disables repository hooks for all Togi-owned Git mutations.

## Attempt Validation

Validation derives actual changed files from Git rather than trusting the
adapter response. A successful attempt must satisfy every condition below:

1. It produces a nonempty worktree change and leaves Git control state intact.
2. All integrity checks pass against original HEAD.
3. Every instant and fast selected gate passes without a new blocking
   finding. Every slower gate that produced an assigned finding is also rerun
   so disappearance is proved rather than inferred.
4. Every assigned grouped blocking finding disappears completely, including
   all occurrences. A partial reduction is not enough to complete that batch.
5. The complete blocking multiset, compared as `(fingerprint, occurrence
   count)`, strictly shrinks and does not rotate into replacement blockers.
6. Local behavioral tests pass for every Go package actually changed by the
   attempt. A `go.mod` or `go.sum` change instead runs `go test ./...`.

Changed packages are computed after the adapter exits, so cross-repository
edits expand validation automatically. Non-Go files do not invent a Go package
test, but they remain subject to gates and integrity. The final repository-wide
barrier covers dependency effects outside directly changed packages.

On failure, Togi records the deterministic reason, resets tracked and
untracked worktree content to the latest validated batch commit, and retries
once in a fresh Codex context. The retry consumes another iteration. A second
semantic failure marks the batch `stuck`; its blockers remain in the plan, and
the flywheel continues with independent batches. A second infrastructure
failure, such as a gate process that cannot execute or a malformed adapter
stream, returns `errored` because it provides no evidence that the code batch
is unfixable. Deterministic setup failures may return `errored` without a
retry.

## Integrity Rules

Integrity compares original HEAD with the attempted tree. The original
feature diff is therefore trusted input to this phase; only Togi-era changes
are constrained.

### Suppression Integrity

An attempt may not introduce a new suppression, test skip, exclusion, or build
constraint. The initial Go implementation recognizes at least:

- `//nolint`, `//lint:ignore`, and `#nosec` directives;
- calls to Go test skip mechanisms such as `Skip` and `SkipNow`;
- new or broadened `//go:build` and legacy `// +build` constraints;
- any change to a tracked `.golangci.yml`, `.golangci.yaml`, `.golangci.toml`,
  or `.golangci.json` file. Parsing selective suppression changes is deferred,
  so the tracer protects these files conservatively.

Existing suppressions at original HEAD are baseline. Moving, broadening, or
duplicating one counts as a new suppression.

### Test Discovery Integrity

Every test, example, benchmark, and fuzz target discoverable at original HEAD
must remain discoverable with the same identity. Deletion, discovery-breaking
rename, signature change, package exclusion, or conversion into a skipped test
blocks the attempt. Production identifiers and files may be renamed.

### Test Behavior Integrity

Existing test declarations retain their statement and control-flow structure,
operators, calls, literals, assertions, cases, and expected values. Existing
fixture files below a `testdata` directory are byte-for-byte protected.
Formatting changes may be ignored structurally; existing comment text remains
protected and never permits a suppression.

New tests and new fixtures are allowed. They must be discoverable, compile,
and contain no suppression mechanism.

### Witnessed Rename Exception

An existing test may change only as a compilation repair for a one-to-one
rename witnessed in production code in the same attempt. The permitted edits
are:

- replacing an identifier with its witnessed production-declaration rename;
- updating an import path, package clause, alias, or selector for a witnessed
  production package rename.

The production declaration before and after the rename must be structurally
equivalent apart from the rename and formatting. The exception does not allow
different callees, arguments, cases, fixtures, literals, operators,
assertions, expected values, control flow, discovery names, skips, or build
constraints. Ambiguous or many-to-many mappings fail closed.

### Integrity Findings

Every violation is normalized into a deterministic integrity finding and
fingerprint. Integrity failures use the ordinary reset, retry, and stuck path.
Waivers are deliberately deferred; this slice only establishes stable
identities that the later `togi waive` workflow can consume.

## Barrier Loop And Stalemate

After the current batch wave finishes, Togi reruns every selected gate against
merge-base through the latest validated Togi commit. If blockers remain:

- any stuck batch makes the run `blocked` after the barrier is recorded;
- otherwise, a strictly smaller blocker multiset produces another
  deterministic file-based wave;
- an unchanged, larger, or rotated set is a stalemate and returns `blocked`
  with the persistent fingerprints.

Only a fully healthy, blocker-free all-gate barrier reaches final behavioral
verification. Togi then runs `go test ./...`. A completed test run with failing
tests is verified as unsafe and returns `blocked`, not `unverified` or
`errored`. An inability to execute the final suite is `errored` because the
green baseline already proved that a suite exists and can run.

## Landing Transaction

After all gates, integrity checks, and the full suite pass, Togi creates one
commit from the validated tree with original HEAD as its sole parent and the
fixed subject `togi: apply verified fixes`. The intermediate batch history is
not included.

Immediately before landing, Togi verifies that the original feature worktree:

- still exists at the recorded canonical path;
- is on the recorded feature branch;
- is clean;
- still has original HEAD checked out; and
- still has its branch ref at original HEAD.

Togi then runs a guarded fast-forward to the squash commit in that worktree.
It disables repository hooks and never force-updates the checked-out branch
ref from another worktree. A failed guard or fast-forward returns `blocked`,
leaves the feature checkout untouched by Togi, and preserves the validated run
branch for inspection.

If no initial blocker existed, there is no landing transaction and no empty
commit. The successful result is still `unsealed`.

## Verdicts And Cleanup

The Phase 3 terminal mapping is:

| Verdict | Exit | Meaning in this slice |
|---|---:|---|
| `unsealed` | 6 | Phase 3 passed; fixes landed or none were needed; seal not run |
| `blocked` | 2 | Stuck batch, integrity blocker, stalemate, final test failure, or landing guard failure |
| `rails` | 3 | Iteration or wall-clock limit exhausted |
| `errored` | 4 | Gate, adapter, suite infrastructure, Git, or other external execution failed |
| `unverified` | 5 | No green baseline behavioral suite exists |
| internal error | 70 | No valid domain report can be produced |

Exit 0 remains reserved for `merge-ready` after Phase 5 sealing. Report-only
keeps its existing findings, errored, and unverified behavior.

On successful landing, Togi removes the cache worktree and
`togi/run-<id>` branch while retaining the ledger. On `blocked`, `rails`, or a
post-edit `errored` result, it first discards any invalid in-progress attempt,
then removes the cache worktree and preserves validated batch commits on the
run branch. A failure before any validated commit needs no preserved branch.
Cleanup failure before landing produces `errored` because repository state is
uncertain. Cleanup failure after a successful fast-forward records landing as
complete, returns `errored`, and retains every artifact that could not be
removed; it never claims that the feature commit was rolled back.

## Ledger And Report Schema

`report.json` advances to schema version 4. In addition to existing diff,
gate, finding, timing, and count data, it records:

- adapter name and optional usage;
- original HEAD and feature branch;
- baseline and final suite status and bounded diagnostics;
- rail limits and consumed iterations/time;
- ordered batch and attempt outcomes, actual changed files, validation
  results, and validated commit IDs;
- integrity findings;
- final barrier status;
- landing status and preserved branch name when applicable.

Arrays and maps use deterministic ordering. Paths in public data remain
repository-relative where possible; external cache paths and credentials do
not enter prompts or the public report. `plan.json`, briefs, and bounded Codex
JSONL remain private audit artifacts under the run directory. Raw gate output
continues to be persisted separately and never enters an agent brief.

`togi status` accepts and renders schema version 4, including the final
verdict and concise batch or landing failure summary. Pre-1.0 policy remains
that only the current report schema is accepted.

## Testing Strategy

Automated tests require neither Codex nor external gate binaries. They use
fake executables, temporary Git repositories, isolated XDG roots, and the
existing process runner.

Focused package tests cover:

- adapter command construction, stdin transport, bounded JSONL parsing,
  usage extraction, cancellation, and malformed output;
- deterministic batches and briefs, retry notes, iteration accounting,
  wall-clock cancellation, progress comparison, and stalemate;
- every integrity rule, including legal production renames, witnessed test
  compilation repairs, forbidden assertion changes, deleted tests, modified
  fixtures, skips, suppressions, and ambiguous renames;
- cache worktree creation, rollback of tracked and untracked files, protected
  Git metadata, batch commits, squash construction, guarded fast-forward,
  successful cleanup, and preserved blocked branches;
- report schema validation, stable ordering, exit mappings, and status
  rendering.

Executable acceptance specifications cover at least:

- successful Codex-style repair and one-commit landing;
- a green run with no blockers and no empty commit;
- missing `--agent` and contradictory report-only flags;
- missing or red baseline suite before any adapter call;
- an initial errored gate before any adapter call;
- cross-file edits validated by the packages actually changed;
- no-op, unauthorized Git mutation, failed validation, retry, and stuck batch;
- suppression, test deletion, assertion change, and legal witnessed rename;
- iteration and wall-clock exhaustion;
- final full-suite regression;
- a dirty, detached, or moved original feature worktree at landing.

The CLI acceptance driver uses a fake `codex` binary on `PATH`. Any optional
real-Codex smoke test is explicitly tagged and excluded from normal
verification. Completion requires:

```sh
go build ./...
go test ./...
go run ./cmd/togi run --report-only
```

The implementation plan must preserve small red-green steps and keep package
tests focused while adding the Phase 3 scenarios to the executable behavior
catalog.

The Codex process contract is based on OpenAI's official
[non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode)
and [CLI command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).

## Deferred Work

This tracer intentionally defers:

- Claude and Kimi adapter implementations;
- spend or token rails;
- `togi waive` and waiver persistence;
- intelligent triage, principle-page assembly, addenda, and wiki-guided
  batching;
- resumable interrupted runs;
- configuration-file overrides for suite discovery and rail defaults;
- Phase 5 ratchets, seal execution, `merge-ready`, and exit 0.
