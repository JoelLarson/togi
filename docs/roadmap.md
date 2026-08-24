# togi — v1 phase plan

Five phases to a working gauntlet. Vocabulary in [CONTEXT.md](../CONTEXT.md),
decisions in [docs/adr/](./adr/), operational detail in
[design.md](./design.md).

Each phase ends in something runnable — togi is dogfooded on its own repo from
phase 1's first report, so every phase's output is exercised on real code
immediately rather than at the end.

## Implementation status

- **Phase 1: complete.** The report-only run engine, finding pipeline, two
  shipped Go gates, external run ledger, and Linux runtime boundary are built.
- **Phase 2: complete.** Committed-diff scoping, Go entity enrichment, clean
  repository preconditions, and conventional local trunk discovery are built.
- **Phase 3: tracer complete.** The safe serial fix loop is implemented with
  file-based batching, a Codex adapter, Go behavioral-suite evidence,
  integrity checks, retry, iteration/wall-clock rails, immutable validation,
  and guarded squash landing. Fingerprint-keyed waivers and Claude/Kimi
  conformance adapters remain before Phase 4.
- **Phase 4: partially implemented ahead of sequence.** Principle-page
  loading, alias resolution, `wiki show`, `wiki lint`, `wiki eject`, and the
  first shipped page are built. Richer containment triage, principle-aware
  plan/brief enrichment, and resume behavior remain.
- **Phase 5: not started.**

The completed executable-specification detour added the human-readable catalog
under [`features/`](../features/README.md). It exercises assembled Phase 1/2
and wiki behavior through service and compiled-CLI drivers without changing
the dependency-driven phase order below.

---

## Phase 1 — Run engine, findings, and two report-only Go gates

**Goal:** `togi run` on a Go repo on Linux produces a normalized, persisted
findings report. No diff scoping, no fixing. Other operating systems retain
buildable platform seams but return unsupported before gate execution or
ledger access until later phases add and verify their backends.

This phase is larger than the original sketch, deliberately: XDG state,
repo-id, and fingerprints land here rather than later. All three are small to
build and expensive to retrofit — everything writes state from day one, and
the fingerprint is a schema field that the ratchet, stalemate accounting, and
waivers all key on.

**Build:**

- **CLI skeleton** — `togi run`, `togi status`; subcommand-shaped from the
  start so pipeline stages can join later without restructuring (ADR-0001).
- **repo-id resolution** — root-commit SHA, falling back to normalized
  remote-URL hash, then absolute-path hash; its full key names the external
  state directory so worktrees share state without short-key collisions
  (ADR-0011).
- **XDG paths** — config/state/cache with standard fallbacks (ADR-0003).
- **Findings schema** — the Go struct, its JSON encoding, and fingerprint
  computation (ADR-0005).
- **Gate loading** — parse gate manifests and language bindings from TOML.
  Shipped defaults embed via `go:embed` and are overridden by anything in the
  config dir, so a fresh install works with no setup (ADR-0004).
- **Gate execution + collector** — run tools as subprocesses with timeouts,
  fan out concurrently, collect at a barrier (ADR-0007).
- **Normalizers** — `golangci-json` and a `regex` normalizer covering
  gocyclo's text output.
- **Enricher seam** — a no-op stage between normalizing and collecting; the
  real implementation lands in phase 2. See the note below.
- **Gate error channel** — errored gates recorded distinctly from findings.
- **Run ledger** — `state/<repo-id>/runs/<run-id>/` holding report.json;
  human summary to stdout; typed exit codes; persistent advisory lock for
  one-run-per-repo.

**Gates:** `golangci-lint` (umbrella lint — exercises the normalizer and
many-to-one aliases) and `gocyclo` (structural — exercises ranges and, later,
containment).

**Exit criteria:** on Linux, a report-only run on togi's own repo emits
report.json with findings from both gates; re-running with no code changes
produces an identical fingerprint set; a deliberately-broken gate binary
produces `errored` without suppressing the other gate's findings. A non-Linux
build returns unsupported before launching gates or accessing ledger state.

> **New scope discovered: the range enricher.** Neither phase-1 tool emits
> ranges. golangci-lint's JSON carries a single `Pos`, and gocyclo's text
> output is `complexity package function file:line:col` — both points. But
> `end_line` is what containment subordination (phase 4) and touched-entity
> scope (phase 2) both depend on, so ranges have to come from somewhere.
>
> That somewhere is togi: a per-language enricher that answers "given
> file:line, what is the enclosing entity's extent?" using `go/ast` for Go.
> It's a general capability rather than a gocyclo workaround — most tools
> report points — so it belongs in the pipeline between normalizing and
> collecting, and it needs a Rust counterpart in phase 5. Verify both output
> formats when installing the tools; if a `-json` mode with ranges exists,
> the enricher gets easier but doesn't go away.
>
> **Split across phases 1 and 2.** Phase 1 builds only the seam — a no-op
> stage in the right place — and phase 2 implements the Go enricher, where
> touched-entity scope becomes its first real consumer. Nothing in phase 1
> reads `end_line`, and fingerprints don't include it (ADR-0005), so
> deferring the implementation re-keys nothing and slims a phase that is
> already the largest departure from the original sketch.

---

## Phase 2 — Diff scoping and Go range enrichment

**Goal:** togi judges only the feature's committed diff.

**Build:**

- **Merge-base and changed-line sets** — `git merge-base HEAD <base>`, then
  per-file changed-line sets from the diff. Base branch from `--base`, falling
  back to `origin/HEAD`, conventional remote-tracking `main` or `master`, then
  local `main` or `master`, without network access.
- **Go range enricher** — `go/ast`-based, filling phase 1's seam: given
  file:line, the enclosing declaration's extent.
- **Touched-entity filter** — a finding is in scope if its range intersects
  *or contains* a changed line (ADR-0008). Occurrences are filtered first,
  and a grouped finding survives if any occurrence does (ADR-0005).
- **Gate scope honored** — diff-scoped vs. whole-repo, per the manifest;
  whole-repo gates may run report-only.
- **Clean-worktree precondition** — staged, unstaged, and untracked changes
  stop the run before ledger creation or gate execution, keeping gate input
  aligned with the committed `HEAD` diff.
- **Submodule limit** — any tracked gitlink is unsupported in Phase 2 and is
  rejected before status, ledger creation, or gate execution. Recursive
  submodule support is deferred.

**Exit criteria:** on a branch with one small change, only findings touching
that change are reported, and a structural finding on a touched function
appears even though its reported line is unchanged; dirty worktrees are
rejected without creating run state, and repositories with initialized or
uninitialized tracked submodules are rejected before status or run state.

---

## Phase 3 — The fix loop

**Goal:** togi fixes what it finds, safely, and stops when it should. The
largest and riskiest phase.

**Build, roughly in this order:**

- **Worktree lifecycle** — create `togi/run-<id>` from feature HEAD in the
  cache dir; commit per validated batch; squash-land on success; refuse to
  land if the feature branch moved (ADR-0010).
- **Agent adapter** — the vendor-neutral interface; brief on stdin, cwd is
  the worktree, results read back as the diff (ADR-0009). Codex first; Claude
  and Kimi remain as conformance adapters.
- **Naive batching** — group by file, no intelligence. The loop has to run
  before phase 4 can make it smart; phase 4 replaces this wholesale.
- **Suite discovery** — the Go default is discovered and run before any
  adapter. An absent/red baseline returns `unverified`; an errored initial gate
  still reports all sibling gate signal, then prevents adapter execution.
  Overrides are deferred with the general configuration surface.
- **Batch validation** — instant/fast plus the assigned finding's owning gate
  must strictly improve blockers, integrity gates must be clean, and changed Go
  packages (or the full suite when selection is unsafe) must be green; semantic
  failure resets to the last green batch.
- **Integrity gates** — suppression and test-integrity checks are implemented.
  Compilation-only test edits required by an unambiguous production rename are
  allowed; test discovery identities, behavior, and fixtures remain protected.
- **Retry policy** — semantic failures receive one fresh-context retry with a
  deterministic failure note, then the batch is marked stuck. A repeated
  retryable adapter error is `errored`; other infrastructure failure remains
  terminally distinct from stuck code.
- **Rails and stalemate** — max iterations and wall-clock are enforced, and
  the finding set must strictly shrink each iteration. Spend rails are
  deferred; adapter usage is optional evidence.
- **`togi waive`** — fingerprint-keyed operator approvals, persisted with
  reason and timestamp, remain the final Phase 3 slice.

**Tracer exit criteria: complete.** On a deliberately degraded branch, togi
fixes the findings in a togi-owned external worktree and updates the original
worktree once with one guarded squashed commit. Test weakening and new
suppressions block; compilation-only test edits required by a witnessed
production rename pass without weakening test behavior. An absent/red baseline
or errored initial gate runs no adapter. Iteration and wall-clock rails bound
work. A successful Phase 3 run is `unsealed` with exit 6 because the Phase 5
seal is not implemented.

**Remaining Phase 3 exit criterion:** a fingerprint-keyed waiver unblocks an
otherwise accepted integrity violation; Claude and Kimi pass the adapter
conformance contract without weakening Codex isolation.

> **Risk: spend rails depend on the vendor CLIs.** Iteration and wall-clock
> rails are togi's to enforce, but token/cost accounting has to come from
> whatever each CLI reports, and that will differ across the three. Expect
> spend rails to be approximate on some adapters and absent on others — the
> adapter interface should treat usage as optional, and the other two rails
> must be sufficient on their own.

---

## Phase 4 — Triage, enriched plans and briefs, resume, and the wiki

**Goal:** fixes happen in the right order, with the right context.

**Build:**

- **Containment subordination** — line-level findings inside a structural
  finding's range fold under it, subordinating on the primary occurrence
  (ADR-0005). Consumes `end_line` from phase 2's enricher.
- **Grouping and ordering** — by (file, principle page), falling back to
  (file, rule_id); ordered by gauntlet position, then path.
- **`plan.json`** — enrich Phase 3's inspectable primary-file plan with
  triage ordering, principle-page references, and resumable execution.
- **Wiki mechanics** — principle page format, alias resolution from language
  bindings, `togi wiki lint` for dangling and conflicting mappings,
  `togi wiki show <page>` for the computed reverse index.
- **Brief assembly** — enrich Phase 3's bounded finding brief with principle
  pages and addenda while preserving deterministic ordering (ADR-0006).
- **The first principle pages** — written lazily, driven by fixes that
  actually came out wrong while dogfooding. Not written speculatively.

**Exit criteria:** plan.json shows lint findings folded under an enclosing
complexity finding, with the structural batch ordered first; assembling the
same brief twice is byte-identical; an interrupted run resumes at the first
pending batch.

---

## Phase 5 — Rust, scheduling, ratchet, and the seal

**Goal:** the second language and the expensive gates; `merge-ready` becomes
reachable.

**Build:**

- **Rust bindings** — clippy via a `clippy-json` normalizer, plus a
  complexity gate. Purely additive: new binding directories under existing
  gates, no changes elsewhere. (Confirm which complexity lint is usable —
  `clippy::cognitive_complexity` has lived in the nursery, so an external
  tool may be needed instead.)
- **Rust range enricher** — the phase-1 enricher's counterpart.
- **Cost-class scheduler** — instant/fast every iteration, slow periodically,
  glacial exactly once. Mutation testing must never enter the inner loop.
- **Ratchet** — optional repo-wide high-water marks keyed by fingerprint, for
  brownfield repos healing slowly.
- **The seal** — go-mutesting / cargo-mutants as the one-time final gate.
- **Exit 0 becomes reachable** — all gates pass, none errored, integrity
  clean, suite green, seal passed.

**Exit criteria:** a full clean run on a Rust repo; the glacial gate runs
exactly once per run; a genuinely clean diff exits 0.

> **Known cost:** a fresh worktree means a cold `target/` for Rust, which
> makes the loop slow on Rust repos. Accepted for v1; a shared
> `CARGO_TARGET_DIR` is the obvious follow-up.

---

## Cross-cutting notes

- **Dogfooding from phase 1** means togi's own gauntlet config is the first
  real gate content written, and togi's own findings drive which principle
  pages get written first.
- **General user-authored configuration is deferred.** The current roadmap
  uses shipped gate defaults plus explicit run flags. A stable `config.toml`
  schema, override layers, provenance, and `config init/show` will be designed
  after real use identifies the settings that need to be public.
- **Phase ordering is dependency-driven, not preference-driven.** Integrity
  gates need diff scoping (2 → 3); containment needs ranges (1 → 4); triage
  needs a working loop to improve (3 → 4). The one place to resist compressing
  is 2 into 3: landing the agent loop on an unproven findings pipeline means
  debugging both at once.
- **Out of scope throughout v1:** pipeline stages before the gauntlet,
  languages beyond Go and Rust, parallel fix workers, any daemon or scheduled
  execution, RAG of any kind.
