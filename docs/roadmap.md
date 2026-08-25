# togi — v1 phase plan

Five phases to a working gauntlet. Vocabulary in [CONTEXT.md](../CONTEXT.md),
decisions in [docs/adr/](./adr/), operational detail in
[design.md](./design.md).

**This file holds the reasoning; the tracker holds the work.** Phase goals,
dependency order, accepted risks, and deliberate deferrals live here. What is
actually left to build lives as GitHub issues on
[`JoelLarson/togi`](https://github.com/JoelLarson/togi), one tracking issue per
phase, carrying that phase's exit criteria as a checklist:

| Phase | Tracking issue |
|---|---|
| Phase 3 — finish the fix loop | [#12](https://github.com/JoelLarson/togi/issues/12) |
| Phase 4 — triage, enriched plans and briefs, resume | [#13](https://github.com/JoelLarson/togi/issues/13) |
| Phase 5 — cost-class scheduling and the seal | [#14](https://github.com/JoelLarson/togi/issues/14) |

Phases 1 and 2 are complete and have no tracking issue.

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
  and guarded squash landing. `togi waive` records fingerprint-keyed approvals
  and a fix run honors them. Remaining: the waiver slices, the Claude/Kimi
  conformance adapters, and the ADR-0014 migration to in-place execution.
- **Phase 4: partially implemented ahead of sequence.** Principle-page
  loading, alias resolution, `wiki show`, `wiki lint`, `wiki eject`, and four
  shipped pages are built. Triage, enriched plans and briefs, and resume
  remain.
- **Phase 5: not started**, and reduced — see the phase note below.

The completed executable-specification detour added the human-readable catalog
under [`features/`](../features/README.md). It exercises assembled Phase 1/2
and wiki behavior through service and compiled-CLI drivers without changing
the dependency-driven phase order below.

---

## Phase 1 — Run engine, findings, and two report-only Go gates

**Goal:** `togi run` on a Go repo on Linux produces a normalized, persisted
findings report. No diff scoping, no fixing.

This phase was larger than the original sketch, deliberately: XDG state,
repo-id, and fingerprints landed here rather than later. All three are small to
build and expensive to retrofit — everything writes state from day one, and
the fingerprint is a schema field that the ratchet, stalemate accounting, and
waivers all key on.

Its gates were chosen to exercise the machinery rather than for coverage:
`golangci-lint` as an umbrella lint, which stresses the normalizer and
many-to-one aliases, and `gocyclo` as a structural gate, which stresses ranges
and, later, containment.

> **The range enricher, and why it split across two phases.** Neither phase-1
> tool emits ranges. golangci-lint's JSON carries a single `Pos`, and gocyclo's
> text output is `complexity package function file:line:col` — both points. But
> `end_line` is what containment subordination and touched-entity scope both
> depend on, so ranges have to come from somewhere.
>
> That somewhere is togi: a per-language enricher answering "given file:line,
> what is the enclosing entity's extent?" using `go/ast` for Go. It's a general
> capability rather than a gocyclo workaround — most tools report points — so it
> belongs in the pipeline between normalizing and collecting.
>
> Phase 1 built only the seam, a no-op stage in the right place; phase 2
> implemented the Go enricher, where touched-entity scope became its first real
> consumer. Nothing in phase 1 reads `end_line`, and fingerprints don't include
> it (ADR-0005), so deferring the implementation re-keyed nothing.

---

## Phase 2 — Diff scoping and Go range enrichment

**Goal:** togi judges only the feature's committed diff.

Merge-base resolution and per-file changed-line sets, the `go/ast` enricher
filling phase 1's seam, and the touched-entity filter: a finding is in scope if
its range intersects *or contains* a changed line (ADR-0008). Clean-worktree
preconditions keep gate input aligned with the committed `HEAD` diff, and
tracked gitlinks are rejected outright — recursive submodule support is
deferred.

---

## Phase 3 — The fix loop

**Goal:** togi fixes what it finds, safely, and stops when it should. The
largest and riskiest phase.

The tracer is complete: worktree lifecycle, the vendor-neutral agent adapter
with Codex, naive file-based batching, suite discovery, batch validation,
integrity gates, retry policy, rails and stalemate detection, and `togi waive`.
A successful Phase 3 run is `unsealed` with exit 6 because the Phase 5 seal is
not implemented.

What remains is tracked in [#12](https://github.com/JoelLarson/togi/issues/12).

**ADR-0014 supersedes ADR-0010** and is part of this phase: the fix loop moves
out of the cache worktree into the agent's own idle worktree, refusing to start
unless it is clean and on the expected branch, and the run branch is retained
after a squash landing. The dirty and detached landing refusals become entry
preconditions; only branch-moved stays a landing-time guard.

> **Risk: spend rails depend on the vendor CLIs.** Iteration and wall-clock
> rails are togi's to enforce, but token/cost accounting has to come from
> whatever each CLI reports, and that will differ across the three. Spend rails
> are deferred, not scheduled: the adapter interface treats usage as optional,
> and the other two rails must be sufficient on their own.

> **Risk: behavior preservation is only as strong as the test suite.** togi's
> promise is that a fix restructures code without changing what the software
> does, and until Phase 5 the entire enforcement of that is "integrity gates
> stay clean and the affected packages' tests stay green." The `complexity`
> gate's fix policy is `llm-fix`: when gocyclo flags a declaration, the agent
> restructures it, and the only thing between that and a silent behavior
> change is whether a test happened to cover the branch it got wrong. On a
> thinly-covered declaration, togi will confidently land a refactor that
> changed what the code does.
>
> Phase 5's **seal** is the designed answer — mutation testing is what
> actually proves behavior survived — but it is glacial and can never enter
> the inner loop, so it protects a run, not a batch. The cheap interim, if
> this bites before Phase 5, is to gate `llm-fix` on coverage: refuse to
> restructure a declaration whose statements a `go test -coverprofile` run
> shows are uncovered, intersected against the finding's enriched range.
> Accepted as-is for now because the engineer reviews every landing.

---

## Phase 4 — Triage, enriched plans and briefs, resume, and the wiki

**Goal:** fixes happen in the right order, with the right context.

Containment subordination folds line-level findings under an enclosing
structural finding (ADR-0005), consuming phase 2's `end_line`. Grouping and
ordering replace phase 3's naive batching. `plan.json` and the brief are
enriched with triage ordering and principle-page references while preserving
determinism (ADR-0006).

The wiki mechanics — page format, alias resolution, `wiki lint`, `wiki show` —
landed ahead of sequence. What remains is tracked in
[#13](https://github.com/JoelLarson/togi/issues/13).

> **Principle page content is written reactively.** Pages are driven by fixes
> that actually came out wrong while dogfooding, never speculatively. The
> tickets build the mechanism that carries page references into plans and
> briefs; which pages get written stays a human judgment made at the moment a
> fix disappoints.

---

## Phase 5 — Cost-class scheduling and the seal

**Goal:** `merge-ready` becomes reachable. Until the seal exists every
successful run is `unsealed` with exit 6, so togi cannot yet make its central
claim about a diff.

The cost-class scheduler runs instant and fast gates every iteration, slow
gates periodically, and glacial gates exactly once — mutation testing must
never enter the inner loop. The seal is go-mutesting as that one-time final
gate. Exit 0 becomes reachable when all gates pass, none errored, integrity is
clean, the suite is green, and the seal passed.

Tracked in [#14](https://github.com/JoelLarson/togi/issues/14).

**This phase is reduced from its original scope.** Two items moved out:

- **Rust bindings and the Rust range enricher.** Too early. The second
  language waits until the Go path is finished; clippy via a `clippy-json`
  normalizer plus a complexity gate remains purely additive when it comes, and
  which complexity lint backs it is still an open question —
  `clippy::cognitive_complexity` has lived in the nursery, so an external tool
  may be needed. A fresh worktree also means a cold `target/` for Rust; ADR-0014
  removes that problem by reusing the agent's warm worktree.
- **The ratchet.** Optional repo-wide high-water marks keyed by fingerprint,
  for brownfield repos healing slowly. Not what togi is dogfooded on. Roadmap
  prose until a brownfield repo needs it.

---

## Cross-cutting notes

- **Dogfooding from phase 1** means togi's own gauntlet config is the first
  real gate content written, and togi's own findings drive which principle
  pages get written first.
- **General user-authored configuration is deferred.** The current roadmap
  uses shipped gate defaults plus explicit run flags. A stable `config.toml`
  schema, override layers, provenance, and `config init/show` will be designed
  after real use identifies the settings that need to be public. Suite-discovery
  overrides are deferred *into* it — that is why that gap exists.
- **Runtime support is Linux only.** Platform seams and buildable stubs exist
  for Darwin, the BSDs, illumos, AIX, Solaris, and Windows, and the catalog
  already asserts they reject cleanly before repository, gate, or ledger
  access. No platform backend is scheduled for v1. Darwin is the only one
  plausibly worth building, and it becomes a small spec against seams that
  already exist on the day someone wants togi on a Mac.
- **Phase ordering is dependency-driven, not preference-driven.** Integrity
  gates need diff scoping (2 → 3); containment needs ranges (1 → 4); triage
  needs a working loop to improve (3 → 4). The one place to resist compressing
  is 2 into 3: landing the agent loop on an unproven findings pipeline means
  debugging both at once.
- **A whole-repo report needs no whole-repo mode.** `--base <root-commit>`
  makes every line a changed line, so touched-entity scope admits every
  finding. Diff-scoped judgment stays the only model, and the brownfield
  sweep is a flag value rather than a second product.
- **Out of scope throughout v1:** pipeline stages before the gauntlet,
  languages beyond Go, parallel fix workers, any daemon or scheduled
  execution, RAG of any kind.

## Deferred, deliberately

Decided against for now, recorded so the reasoning is not re-litigated. None of
these has a ticket, and that is the point — an open issue is an invitation to
re-litigate.

- **`--path` scoping.** Narrowing a run to one file. A scratch branch touching
  that file does the same thing through the real code path. Revisit only if
  the scratch-branch loop actually becomes annoying; it is a small filter
  applied where occurrences are already filtered.
- **An oversized-base guard on fix mode.** `--base <root-commit>` in fix mode
  licenses the agent to rewrite the entire repository, bounded only by the
  iteration and wall-clock rails — precisely the unbounded brownfield sweep
  the diff-scoped model exists to avoid, reachable from an innocent-looking
  flag. Fix mode should refuse above some changed-file threshold without an
  explicit override. Deferred while the engineer is the only user — **this is
  the deferral that ages worst, and it expires the day anyone else runs togi.**
- **Exported-surface integrity.** Restructuring behind an unchanged assertion
  is safe; changing a package's exported surface is the risky edge (see
  **behavior** in CONTEXT.md). This fits the existing machinery exactly — an
  **integrity gate** that trips when a batch changes an exported
  declaration's signature, released per-fingerprint by a **waiver**, which is
  what "needs more scrutiny" means operationally. Deferred as a real but
  not-yet-felt problem.
- **A written run summary.** Per-batch landing commits carry what was fixed,
  and `togi status` carries the verdict, stuck batches, and honored waivers.
  A generated Markdown summary was designed and dropped as speculative;
  reconsider only if `togi status` output keeps getting copied by hand.
- **Run-branch pruning.** ADR-0014 retains `togi/run-*` refs and deliberately
  leaves pruning unspecified. Refs are cheap; a retention rule can be chosen
  once there is evidence about how many runs an engineer actually revisits.
