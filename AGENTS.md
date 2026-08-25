# Working in this repo

togi (研ぎ) is a Go CLI that takes a feature diff, runs it through a
**gauntlet** of deterministic quality gates, then loops an agent-driven fix
cycle until the diff is merge-ready or a budget rail stops it.

**Current state:** Phases 1 and 2 and the Phase 3 tracer are implemented.
`togi run` resolves a committed feature diff, executes data-defined Go gates
concurrently, normalizes and enriches their findings, filters them to touched
entities, and persists a schema-4 report in the external run ledger. In fix
mode it requires the Codex adapter, establishes a green Go behavioral baseline
and healthy initial gates, then fixes file-based batches in a togi-owned
external worktree. Accepted batches pass validation and integrity checks before
one guarded squash landing. Successful Phase 3 runs are `unsealed` with exit 6.
`togi status` reads completed history, and `togi waive` records a
fingerprint-keyed operator approval that nothing yet honors.
Principle-page loading and the `togi wiki show`, `lint`, and `eject` commands
also landed early from Phase 4.

The executable acceptance specifications in [`features/`](./features/README.md)
are the user-visible contract for the assembled behavior. They run through the
service boundary by default and through the compiled CLI on demand.

## Read these first

The design is settled. These documents are the source of truth:

| File | What it holds |
|---|---|
| [CONTEXT.md](./CONTEXT.md) | The glossary. Non-negotiable vocabulary. |
| [docs/adr/](./docs/adr/) | 13 ADRs: the load-bearing decisions and why. |
| [docs/design.md](./docs/design.md) | Gauntlet semantics: config, rails, verdicts, exit codes. |
| [docs/implementation.md](./docs/implementation.md) | Go-level choices: module, deps, layout, CLI surface, ledger, TOML schemas, testing. |
| [docs/roadmap.md](./docs/roadmap.md) | The five phases, current status, and exit criteria. |
| [features/README.md](./features/README.md) | The executable user-story catalog, authoring rules, and driver commands. |

Not a source of truth, but the fastest way in:
[docs/language.md](./docs/language.md) tours the vocabulary, which terms are
built, which name later phases, and which pairs get confused.

**Do not re-litigate settled decisions.** If you think one is wrong, say so
and stop; do not quietly build something else. If two sources of truth
genuinely contradict each other, surface that immediately.

## Active boundary: finish Phase 3

The Phase 3 tracer is implemented. It includes the external worktree and
guarded landing lifecycle, the vendor-neutral adapter seam with Codex, primary
file batching, Go suite discovery, validation and rollback, integrity checks,
one retry, stalemate detection, and iteration/wall-clock rails. An absent/red
baseline is `unverified` and an errored initial gate retains all sibling gate
signal; neither condition invokes the adapter. Spend rails remain deferred.

Waiver persistence has landed. The next slices honor a waiver: past an
integrity violation on re-run, as touched-entity scope relief, and in the
report of every run that honored one. Claude/Kimi adapter conformance is the
other half of Phase 3's remaining exit criterion, but it is deferred: Codex is
the primary target, so Phase 3 stays formally open once waivers land.
Complete the waiver work before Phase 4's richer containment triage,
principle-aware plan and brief assembly, and resume behavior. Phase 3 already
persists its simple `plan.json`, bounded briefs, private adapter logs, and
schema-4 fix report; do not confuse those tracer artifacts with Phase 4's
richer semantics.

## Non-negotiables

- **Never persist config or state in a target repo** (ADR-0002). No `.togi/`
  directory. Phase 3 may modify code only inside a togi-owned external
  worktree, then land through the guarded lifecycle defined by ADR-0010.
- **Package names come from the glossary** (ADR-0012), not technical layers.
  A new concept requires a glossary entry and package together.
- **Gates are data; normalizers are compiled Go** (ADR-0004). Adding a gate
  or language must not require editing gate orchestration code.
- **`errored` is not a finding.** A missing, crashed, timed-out, or malformed
  tool sets gate status `errored`; it never becomes zero findings and never
  suppresses healthy sibling gates.
- **Raw tool output stays out of agent context** (ADR-0005). It is persisted
  only for normalizer diagnostics.
- **Every external process goes through `internal/runner`.** It owns bounded
  capture, timeouts, cancellation, and process-tree termination.
- **Every production Git invocation goes through `internal/gitcmd`.** Callers
  choose the declared hermetic or config-honouring policy. Real-repository test
  fixtures may invoke Git directly to construct their inputs.
- **Tests pass without external gate or agent tools and without network
  access.** Normalizers use recorded fixtures; acceptance scenarios use
  controlled fake gates and adapters.
- **Production dependencies are Cobra and pelletier/go-toml/v2.** Godog
  v0.16.0 is the pinned acceptance-test exception. Ask before adding another
  dependency.

## Verify

The default suite exercises package tests and the service acceptance driver.
The explicit commands exercise the compiled CLI and the full driver matrix:

```sh
go build ./...
go test ./...
go test ./features/... -args -acceptance.driver=cli
go test ./features/... -args -acceptance.driver=all
```

No external gate binaries are required for these commands.

Dogfood separately when the gate binaries are installed and the repository is
on a clean committed feature branch with a discoverable or explicit base:

```sh
go run ./cmd/togi run --report-only
```

Dogfood fix mode only when Codex is also installed, by replacing
`--report-only` with `--agent codex`. The automated verification matrix uses
controlled fakes and needs neither Codex nor external gate binaries.

## Things to check rather than assume

- **External CLI contracts.** Confirm real gate and agent command formats
  against the pinned versions before relying on them. Vendor usage reporting
  may be approximate or absent; adapters must treat it as optional.
- **Remaining Phase 3 boundaries.** Honoring a waiver and additional adapter
  conformance must preserve the implemented worktree, validation, integrity,
  rail, and guarded-landing contracts.
- **`go 1.25`.** The module version against a Go 1.26 local toolchain is
  deliberate.
- **Platform support.** Runtime orchestration remains Linux-only. Other
  platforms compile but return unsupported before repository, gate, or ledger
  access.

## Conventions

Match the existing commit style: imperative subject, body explaining why
rather than what, trailers preserved. Update `CONTEXT.md` when introducing
domain vocabulary. Add an ADR only when a decision is hard to reverse,
surprising without context, and the result of a real trade-off.

Treat the acceptance catalog as the behavior map and package tests as the
exhaustive edge and security coverage. Add or change user-visible behavior in
both layers at the boundary that owns it.

## Agent skills

### Issue tracker

Issues live as GitHub issues on `JoelLarson/togi`, via the `gh` CLI.
See [`docs/agents/issue-tracker.md`](./docs/agents/issue-tracker.md).

### Triage labels

The five canonical triage labels are used verbatim, and all five exist on
GitHub. See [`docs/agents/triage-labels.md`](./docs/agents/triage-labels.md).

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root.
See [`docs/agents/domain.md`](./docs/agents/domain.md).
