# Engine cleanup design

## Goal

Make `internal/flywheel/engine.go` easier to read without changing flywheel
behavior. The cleanup is limited to orchestration: attempt execution, retry
bookkeeping, stop checks, barrier evaluation, and the small consistency fixes
listed in `internal/flywheel/engine.go.cleanup.md`.

## Structure

`Execute` continues to own plan waves and terminal outcomes. Barrier execution
and classification move behind a focused helper that validates the barrier,
persists once, and returns a named continuation or terminal result.

`executeBatch` becomes the retry-policy driver. Attempt-scoped data moves into
a `batchAttempt` value, and its `run` method executes the linear attempt pipeline:
brief, sink, Git snapshot, adapter, integrity check, changed files, proof,
validation, commit, and persistence. A single checkpoint helper owns the paired
context and rail checks between stages.

Attempt execution returns a classified result. The batch loop has one decision
point for success, semantic failure, retryable infrastructure failure,
non-retryable infrastructure failure, and an already-formed terminal outcome.
This keeps retry, stuck, reset, and error policy out of individual pipeline
stages.

## Invariants

- Context and rail checks retain their current ordering around side effects.
- Git integrity errors retain precedence over adapter errors.
- Proof verification still follows validation and can replace its result when
  validation mutates the prepared workspace.
- Every failed attempt is persisted before reset.
- A completed attempt is persisted before post-commit cancellation is observed.
- A failed post-commit persist rolls back unless the plan was published.
- Semantic failures become stuck only after the second attempt; infrastructure
  failures retain their existing retryability and terminal classification.
- No public API, persisted JSON shape, or user-visible outcome changes.

## Supporting cleanup

Use a named maximum-attempt constant and typed attempt statuses, initialize
semantic findings when engine state is constructed, remove dead assignments and
the pass-through rail context wrapper, rename failure helpers for their actions,
and comment the three non-obvious ordering and compensation invariants identified
by the cleanup checklist.

## Testing

Add focused unit tests for newly explicit classification and status behavior
before introducing the implementation. Preserve the existing engine tests as the
behavioral characterization suite and run the package after each refactor slice.
Finish with the repository verification matrix. Each cleanup checklist item is
marked complete only after its implementation and relevant tests pass.
