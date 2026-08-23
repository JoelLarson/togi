# Project State Housekeeping Implementation Plan

> **Status: Complete.** The project handoff and current-state documentation are
> reconciled. This plan remains as the execution record for that housekeeping.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile the repository documentation with the completed Phase 1, Phase 2, and acceptance-specification work and establish Phase 3 as the active boundary.

**Architecture:** `AGENTS.md` is the operational handoff, `docs/roadmap.md` owns phase status, and completed implementation plans remain immutable execution records with explicit status overlays. `docs/implementation.md` describes only the current implemented contract or labels future surface clearly.

**Tech Stack:** Markdown, Git, Go 1.25 verification commands.

**Design:** `docs/superpowers/specs/2026-08-22-project-state-housekeeping-design.md`

---

## File Map

- `AGENTS.md`: current implementation state, Phase 3 handoff, constraints, and verification matrix.
- `docs/roadmap.md`: concise phase-status overlay without changing phase requirements.
- `docs/implementation.md`: current dependency, schema, CLI, retention, and concurrency facts.
- `docs/superpowers/plans/2026-08-21-phase-1-implementation.md`: completed-plan notice.
- `docs/superpowers/plans/2026-08-21-phase-2-diff-scoping.md`: completed-plan notice.
- `docs/superpowers/plans/2026-08-21-local-trunk-base-discovery.md`: completed-plan notice.
- `docs/superpowers/plans/2026-08-22-executable-acceptance-specifications.md`: completed-plan notice.

### Task 1: Replace the Active Handoff

**Files:**
- Modify: `AGENTS.md`

- [x] **Step 1: Replace the documentation-only and Phase 1 task sections**

Describe Phases 1 and 2 as implemented, name the acceptance-specification detour and early wiki mechanics, and identify Phase 3 as the next design and implementation boundary. Point readers to `features/README.md` for the executable behavioral catalog.

- [x] **Step 2: Preserve and update the non-negotiables**

Retain target-repository isolation, glossary package names, data-defined gates, the `errored` channel, raw-output insulation, and tool-free tests. Add the `runner` and `gitcmd` ownership boundaries. State that Cobra and go-toml are production dependencies and Godog is the pinned acceptance-test exception.

- [x] **Step 3: Replace the verification block**

Document these exact commands:

```sh
go build ./...
go test ./...
go test ./features/... -args -acceptance.driver=cli
go test ./features/... -args -acceptance.driver=all
```

State that real gate tools are not prerequisites for the test suite. Keep dogfood separate because it requires installed gate binaries and a suitable committed feature diff.

- [x] **Step 4: Check the handoff for obsolete claims**

Run:

```sh
rg -n 'documentation only|Your task: phase 1|first commit is the skeleton|Write phase 1' AGENTS.md
```

Expected: no output and exit 1.

### Task 2: Add Roadmap Status Without Rewriting Scope

**Files:**
- Modify: `docs/roadmap.md`

- [x] **Step 1: Add an implementation-status section before Phase 1**

Record Phase 1 as complete; Phase 2 as complete, including conventional local trunk discovery; Phase 3 as the next active phase with no approved implementation plan; Phase 4 wiki mechanics and one shipped page as early partial work; Phase 5 as not started; and the executable specifications under `features/` as a completed cross-cutting detour.

- [x] **Step 2: Leave phase definitions and exit criteria unchanged**

Run `git diff --word-diff=porcelain -- docs/roadmap.md`.

Expected: only the status overlay is added; existing phase bodies are unchanged.

### Task 3: Mark Completed Plans as Historical Records

**Files:**
- Modify: `docs/superpowers/plans/2026-08-21-phase-1-implementation.md`
- Modify: `docs/superpowers/plans/2026-08-21-phase-2-diff-scoping.md`
- Modify: `docs/superpowers/plans/2026-08-21-local-trunk-base-discovery.md`
- Modify: `docs/superpowers/plans/2026-08-22-executable-acceptance-specifications.md`

- [x] **Step 1: Add the same status convention to every completed plan**

Immediately below each title, add a blockquote stating that implementation is complete, the document is retained as a historical execution record, and unchecked boxes preserve the prescribed sequence rather than indicate outstanding work. Use the plan's own subject in the completion sentence.

- [x] **Step 2: Verify every plan is unambiguous**

Run:

```sh
for plan in docs/superpowers/plans/2026-08-21-phase-1-implementation.md docs/superpowers/plans/2026-08-21-phase-2-diff-scoping.md docs/superpowers/plans/2026-08-21-local-trunk-base-discovery.md docs/superpowers/plans/2026-08-22-executable-acceptance-specifications.md; do
  rg -q 'Status: Complete' "$plan" || exit 1
done
```

Expected: exit 0.

### Task 4: Correct Current Implementation Facts

**Files:**
- Modify: `docs/implementation.md`

- [x] **Step 1: Correct dependency and platform wording**

Identify Cobra and go-toml as the only production dependencies, document Godog v0.16.0 as the pinned acceptance-test dependency, and describe Linux as the current runtime boundary rather than only a Phase 1 condition.

- [x] **Step 2: Correct schema and CLI status**

Replace the proposed-schema notice with an implemented-contract statement. Remove `togi gate list | eject <name>` from the implemented CLI block and label it as future surface if it remains useful to record. Do not claim that a current command ejects gate definitions.

- [x] **Step 3: Correct fixed limits and testing claims**

State that run retention is currently fixed at 20 and gate concurrency at `min(NumCPU, 4)`. Remove the claim that a real-tool build-tagged integration suite already exists; retain recorded normalizer fixtures and the tool-free acceptance harness as current testing strategy.

- [x] **Step 4: Search for the corrected stale wording**

Run:

```sh
rg -n '\*\*Proposed\*\*|togi gate list|togi gate eject|and is configurable|recent 20 at run start, configurable|Integration.*build-tagged' docs/implementation.md
```

Expected: no output and exit 1.

### Task 5: Verify and Commit the Reconciliation

**Files:**
- Modify: all files listed above.

- [x] **Step 1: Check Markdown paths and whitespace**

Run:

```sh
test -f features/README.md
test -f docs/superpowers/specs/2026-08-22-project-state-housekeeping-design.md
git diff --check
```

Expected: exit 0.

- [x] **Step 2: Build and test**

Run:

```sh
go build ./...
go test ./...
go test ./features/... -count=1 -args -acceptance.driver=cli
go test ./features/... -count=1 -args -acceptance.driver=all
```

Expected: every command exits 0.

- [x] **Step 3: Review the final documentation diff**

Run:

```sh
git diff --stat
git diff -- AGENTS.md docs/roadmap.md docs/implementation.md docs/superpowers/plans/
```

Expected: documentation-only changes matching the approved design; no phase requirements or production files changed.

- [x] **Step 4: Commit**

```sh
git add AGENTS.md docs/roadmap.md docs/implementation.md docs/superpowers/plans/2026-08-21-phase-1-implementation.md docs/superpowers/plans/2026-08-21-phase-2-diff-scoping.md docs/superpowers/plans/2026-08-21-local-trunk-base-discovery.md docs/superpowers/plans/2026-08-22-executable-acceptance-specifications.md docs/superpowers/plans/2026-08-22-project-state-housekeeping.md
git commit -m "Reconcile the project handoff" -m "Make Phase 3 the explicit next boundary while preserving completed plans as historical execution records."
```
