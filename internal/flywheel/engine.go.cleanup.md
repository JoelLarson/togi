# engine.go cleanup plan

Readability improvements for `engine.go`, ranked by impact. The core problem is not
the file's length — it is that `Execute` (~125 lines) and `executeBatch` (~250 lines)
each interleave three concerns that could be read separately: the happy-path pipeline,
cancellation/rail checking, and failure/retry bookkeeping.

The bottom third of the file is already in good shape (`validateAttemptResult`,
`validateBarrierResult`, `boundedFailure`, `validateEngineInput` are single-purpose,
flat, and under 30 lines), and the `Ports`/`WorkspacePort` seam design is sound. The
work is confined to the orchestration bodies.

---

## 1. Extract the attempt pipeline out of `executeBatch`

`executeBatch` (engine.go:254) is a 2-iteration retry loop whose body is the entire
attempt: brief → sink → git snapshot → adapter run → integrity check → changed files →
prepare proof → validate → commit → persist. A reader has to hold all of it to
understand any of it.

**Change:** split the loop body into a `runAttempt` function (or several small step
functions), so the retry policy reads in ~15 lines and each step reads in isolation.

## 2. Collapse the stop-check boilerplate

The pair

```go
if outcome, stopped := stopAttemptForContext(...); stopped { return outcome, true }
if outcome, stopped := stopAttemptForRail(...); stopped { return outcome, true }
```

appears eight times inside `executeBatch`, between nearly every step. It is the
biggest source of the "so many ifs" feeling, and it is fragile: easy to forget one,
and a reader cannot tell whether any given placement is deliberate or copy-paste.

**Change (minimum):** merge the pair into one `checkStops(...)` helper so each
checkpoint is one line instead of six.

**Change (better):** structure the attempt as a list of steps driven by a small loop
that performs the stop checks between steps in exactly one place. The policy "we
check context and rails between every step" becomes a stated invariant instead of
something verified by eyeball.

## 3. Unify the retry epilogue

This shape appears at engine.go:332, 352, 371, 382, 397, 443, 453, 477 with small
variations:

```go
if resetErr := failAndReset(...); resetErr != nil { return errored... }
if attempt == 2 { return .../Outcome{}, false }
retryFailure = failure
continue
```

The variations encode a real distinction — infrastructure failures on the last
attempt return `OutcomeErrored`, semantic failures mark the batch `BatchStuck` and
keep going — but that distinction is currently smeared across eight call sites.

**Change:** have each step return a classified result
(`ok | semanticFailure | infraFailure(retryable) | stop`) and let one switch at the
bottom of the loop own the entire retry/stuck/errored decision. This is the single
change that most reduces nesting.

## 4. Deduplicate the barrier section of `Execute`

The block `if err := state.persist(); err != nil { return ..."write barrier plan"... }`
repeats five times (engine.go:192, 200, 211, 217, 222) because persistence is
re-attempted before each possible early return.

**Change:** classify the barrier result first and persist once — or extract a
`persistOr(outcome)` helper. Extracting an `evaluateBarrier(ctx, state, barrier) Outcome`
function also gives the wave loop in `Execute` a readable shape:
run wave → barrier → decide → grow plan.

## 5. Replace the `(Outcome, bool)` convention

Nearly every helper returns `(Outcome, bool)` where `Outcome{}, false` means "keep
going". The reader must learn the convention and track which bool means what, and
`executeBatch` ends in `panic("unreachable")` (engine.go:504) because the compiler
cannot see the invariant either.

**Change:** introduce a tiny result type
(`type stepResult struct { outcome Outcome; stop bool }`) or return `*Outcome`
where nil means continue, so the semantics are named at every call site.

## 6. Reduce the parameter caravans

`(ctx, request, ports, state, index, attempt)` is threaded through six helpers
(`failedInfrastructure`, `semanticFailure`, `stopAttemptForContext`,
`stopAttemptForRail`, ...).

**Change:** bundle the attempt-scoped values into a struct and make the helpers
methods on it:

```go
type batchAttempt struct {
    request Request
    ports   Ports
    state   *engineState
    index   int
    attempt int
}
```

Call sites shrink from `semanticFailure(ctx, request, ports, state, index, attempt, failure)`
to `a.semanticFailure(ctx, failure)`, and the file's call graph becomes scannable.

## 7. Smaller, cheap wins

- **`const maxAttempts = 2`** — the literal `attempt == 2` appears eight times; the
  retry policy deserves a name.
- **Typed attempt statuses** — `"running"`, `"passed"`, `"failed"` (engine.go:274,
  483, 530) are raw strings while batch status gets proper constants (`BatchDone`,
  `BatchStuck`). Inconsistent, and a typo would compile fine.
- **Dead code** — `retryable := false` at engine.go:345 is immediately overwritten on
  line 347; `railExecutionContext` (engine.go:692) is a pass-through wrapper that
  adds only indirection.
- **Lazy map init** — initialize `semanticFindings` in an `engineState` constructor
  and delete the nil check at engine.go:435.
- **Naming** — `semanticFailure` and `failedInfrastructure` read as nouns but are
  actions with different signatures and different stop behavior; something like
  `abortAttemptSemantic` / `abortAttemptInfra` signals both the action and the
  asymmetry. Likewise, `stopAttemptForContext` also resets the workspace (unlike
  `stopForContext`) — invisible from the name.

## 8. Comment the genuinely subtle invariants

A few places are correct but look wrong, which is where comments earn their keep:

- **engine.go:307–316** — git state is snapshotted, then stop checks run *before*
  the error is inspected. Deliberate ordering, but a reader will assume it is a bug.
- **engine.go:404–414** — validation runs, *then* the proof is verified, and a
  verify failure retroactively overwrites the validation result. The why
  (detecting workspace tampering during validation, presumably) is undocumented.
- **engine.go:486–494** — a persist failure after commit triggers rollback *unless*
  `planWasPublished`; the duck-typed `PlanPublished()` interface (engine.go:558)
  and this compensation path deserve a sentence each.

---

## Suggested first move

Items 2 + 3 together (step pipeline with centralized stop checks and one
retry-decision switch): that alone would likely cut `executeBatch` from ~250 lines
of nested ifs to a readable ~60-line driver plus small step functions, without
changing any behavior.
