# togi — design notes

Gauntlet semantics that are settled but reversible, so they don't warrant
ADRs. Vocabulary is defined in [CONTEXT.md](../CONTEXT.md); load-bearing
decisions are in [docs/adr/](./adr/); Go-level build choices are in
[implementation.md](./implementation.md).

## Formats and defaults

- **TOML** for gate manifests and bindings (Go-idiomatic, comments allowed).
- **Markdown** for wiki principle pages, addenda, and assembled briefs.
- **JSON** for machine artifacts: findings, `plan.json`, `report.json`.
- **Base branch** for merge-base: explicit `--base`, falling back to
  auto-detected `origin/HEAD`.

## Phase 1 platform support

Phase 1 runtime support is Linux only. `togi run` and `togi status` return a
clear unsupported-platform error on every other operating system before
starting a gate process or reading, creating, pruning, or writing ledger state.
Platform seams and buildable stubs remain so Darwin, the BSDs, illumos, AIX,
Solaris, and Windows can gain tested backends later without changing the run
contract. `togi version` does not enter the runtime and remains available.

## Configuration

General user-authored configuration is deferred beyond the current roadmap.
The initial gauntlet uses shipped gate definitions and explicit run flags.
There is no `config.toml`, override-layer contract, provenance model, or
`togi config init/show` command yet. Gate-directory overrides remain the
phase-1 escape hatch for developing gate data.

This is an intentional pre-release simplification rather than a permanent
claim that all users should share one configuration. The stable configuration
surface will be designed after dogfooding identifies which settings actually
need to vary.

## Behavioral suite discovery

Per-language defaults: `go test ./...` / `cargo test` for the suite,
`go build ./...` / `cargo check` for the build. **Green means exit 0.**
Overrides for monorepos, build tags, and service subsets are deferred with the
rest of the general configuration surface.

If no suite exists, or the suite is red at baseline, togi reports that
*before* any fixing starts, and the run's verdict caps at **unverified** — the
fix loop may still run, but merge-ready is unreachable by definition.

## Severity and what blocks

Canonical severities are `error` | `warning` | `info`; each language binding
maps its tool's vocabulary onto them (ADR-0005). Each gate manifest declares
which levels block merge-ready. Default: **`error` and `warning` block; `info`
is advisory** — reported, never entering the fix loop and never blocking. This
is what makes "advisory gate" expressible without inventing a fix policy for
it, and it's the dial to reach for first when a gate proves noisy.

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

Hard rails: max iterations, wall-clock, agent spend/tokens — initially using
shipped defaults with explicit per-run flags.

Stalemate: the **finding set must strictly shrink** each iteration, compared
by `(fingerprint, occurrence count)`. Comparing sets rather than counts catches
whack-a-mole churn (count steady, findings rotating) as well as outright
stalls; including the occurrence count is what stops a partial fix — two of
three identical hits resolved — from reading as no progress at all, now that
identical findings group into one fingerprint (ADR-0005). On stalemate, togi
stops with a `blocked` report naming the persistent fingerprints — a stuck
finding usually means a missing wiki page or a wrong threshold.

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

One run per repo, enforced by an OS advisory lock held on an open, persistent
`lock` file in that repo's state dir. Linux uses `flock`. Before opening the
lock file, a process-local registry atomically claims its canonical path and
repository-directory identity. The JSON PID/start/token record is
informational; ownership is the local claim plus open-file lock, which Linux
releases when a process exits. The lock file is never unlinked, avoiding split
ownership across old and newly-created inodes. All non-Linux platforms return
the phase 1 unsupported-platform error before accessing ledger state. This is
the natural consequence of one writer in the worktree (ADR-0007).

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

1. Runner, findings schema, two report-only Go gates, XDG state, repo-id
2. Diff scoping and Go range enrichment
3. The fix loop — worktree, adapter, integrity gates, rails
4. Triage, the flywheel, and the wiki
5. Rust, cost-class scheduling, ratchet, and the seal

Full detail, dependencies, and exit criteria: [roadmap.md](./roadmap.md).
