# togi — design notes

Operational decisions that are settled but reversible, so they don't warrant
ADRs. Vocabulary is defined in [CONTEXT.md](../CONTEXT.md); load-bearing
decisions are in [docs/adr/](./adr/).

## Formats and defaults

- **TOML** for global config, per-project config, gate manifests and bindings
  (Go-idiomatic, comments allowed).
- **Markdown** for wiki principle pages, addenda, and assembled briefs.
- **JSON** for machine artifacts: findings, `plan.json`, `report.json`.
- **Base branch** for merge-base: per-project config, falling back to
  auto-detected `origin/HEAD`.

## Config resolution

Three layers, deep-merged per key: **shipped defaults → global `config.toml`
→ `projects/<repo-id>/config.toml`**.

Per-project may override: the gauntlet (gate list and order), per-gate
thresholds and settings, fix policies, model and adapter per role, rails,
suite/build commands, ratchet on/off.

Not overridable per project: the findings schema and normalizer behavior.
Integrity gate *thresholds* are tunable, but disabling an integrity gate
requires an explicit global opt-out — the anti-gaming checks shouldn't be
quietly switchable per repo.

`togi config init` scaffolds a project config; `togi config show` prints the
merged result with per-key provenance.

## Behavioral suite discovery

Per-language defaults: `go test ./...` / `cargo test` for the suite,
`go build ./...` / `cargo check` for the build. **Green means exit 0.**
Per-project config overrides both (monorepos, build tags, service subsets).

If no suite exists, or the suite is red at baseline, togi reports that
*before* any fixing starts, and the run's verdict caps at **unverified** — the
fix loop may still run, but merge-ready is unreachable by definition.

## Gate execution errors

A tool that crashes, is missing, emits unparseable output, or mismatches its
pinned version puts the gate in **errored** — a channel distinct from
findings. An infrastructure failure must never silently become "zero
findings," which is how a missing mutation tool would let a seal pass that
shouldn't.

Consequences: other gates keep running (you still get their signal), the fix
loop may proceed on healthy gates' findings, and merge-ready is impossible
while any enabled gate is errored. Errored gates are surfaced loudly in the
report.

## Batch validation and retry

A batch is validated before commit: instant/fast gates must not regress,
integrity gates must be clean, the suite must be green.

On failure: reset to the last green batch, then **retry once in a fresh agent
context** with a deterministic failure note appended to the brief ("the
previous attempt broke X"). A second failure marks the batch **stuck**; its
findings carry into stalemate accounting and the flywheel moves on.

One retry catches a flaky test or a bad sample. A batch that fails twice
usually needs a wiki page or a different threshold — an operator decision, not
a third sample — so spending more rail on it is spending it on the least
promising work available.

## Rails and stalemate

Hard rails: max iterations, wall-clock, agent spend/tokens — set globally and
overridable per project and per run.

Stalemate: the **finding set must strictly shrink** each iteration, compared
by fingerprint. Comparing sets rather than counts catches whack-a-mole churn
(count steady, findings rotating) as well as outright stalls. On stalemate,
togi stops with a `blocked` report naming the persistent fingerprints — a
stuck finding usually means a missing wiki page or a wrong threshold.

## Operator approvals: waivers

Integrity violations halt the run as `blocked`, listing each violation's
fingerprint. The operator runs:

```
togi waive <fingerprint> --reason "…"
```

Waivers persist in the repo's state dir with reason and timestamp, appear in
every subsequent report, and are honored on re-run.

No interactive mid-run prompts: unattended runs are the primary mode, and an
audited artifact beats a y/n keystroke nobody can reconstruct later. Waivers
are also the relief valve for touched-entity scope (ADR-0008).

## Concurrency

One run per repo, enforced by a lockfile in that repo's state dir; stale locks
are detected by pid. This is the natural consequence of one writer in the
worktree (ADR-0007).

## Run contract

`togi run` exits with typed codes so CI and shell composition work without a
`--json` retrofit later:

| Code | Meaning |
|---|---|
| 0 | merge-ready |
| 1 | findings remain (report-only mode) |
| 2 | blocked (integrity violation or stalemate) |
| 3 | rails exhausted |
| 4 | a gate errored |
| 5 | unverified (no green suite) |
| 70 | togi internal error |

A human summary goes to stdout; the machine-readable `report.json` (findings,
batches, timings, spend, verdict) lands in the run state dir. `togi status`
reads the latest run ledger.

## Brief assembly

Deterministic concatenation, in order: findings (with their snippets) →
matching principle pages → language addenda where they exist → explicit
`file:line` pointers → constraints.

**No code is embedded beyond the finding snippets.** The agent runs inside the
worktree with its own file tools; embedding code duplicates what it will read
anyway, inflates assembly, and goes stale mid-batch as earlier fixes land.
This is what "minimal code context" resolves to.

## Cost-class scheduling

- `instant` / `fast` — every fix iteration, and in batch validation.
- `slow` — periodically across iterations.
- `glacial` — exactly once, as the **seal**, before declaring merge-ready.
  Mutation testing must never sit in the inner loop.

## Phasing

The v1 arc. Amendments to the original sketch are noted inline.

1. **Runner + findings schema + two report-only Go gates.**
   Gates: `golangci-lint` (umbrella lint — stresses the normalizer and
   many-to-one aliases) and `gocyclo` (structural ranges — stresses
   `end_line` and containment).
   *Amended:* XDG state and repo-id land **here**, not in phase 2 — they're
   small, and everything writes state from day one; retrofitting storage is
   worse than building on it. Fingerprints land **with the schema**, not
   later — they're a schema field, and the ratchet, stalemate, and waivers
   all key on them.
   *Amended:* dogfood togi on togi from this phase's first report-only run.

2. **Diff-scoping** (merge-base, touched-entity) **+ config resolution**
   (three-layer merge, `config init` / `config show`).

3. **Fix loop + adapter + integrity gates + budgets.**
   *Amended:* explicitly includes the worktree, per-batch commits, and squash
   landing — that machinery is fix-loop infrastructure, not a later concern.

4. **Triage + flywheel + wiki + briefs** — `plan.json`, containment
   subordination, principle pages, deterministic brief assembly.

5. **Rust bindings + cost-class scheduler + ratchet + one glacial gate**
   (mutation testing as the seal).
