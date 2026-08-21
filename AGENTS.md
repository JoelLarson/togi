# Working in this repo

togi (研ぎ) is a Go CLI that takes a feature diff — usually AI-produced — runs
it through a **gauntlet** of deterministic quality gates, then loops an
agent-driven fix cycle until the diff is merge-ready or a budget rail stops
it.

**Current state: documentation only. There is no code yet.** Your job is
phase 1, and the first commit is the skeleton.

## Read these first

The design is settled. These four documents are the source of truth, and they
disagree with nothing:

| File | What it holds |
|---|---|
| [CONTEXT.md](./CONTEXT.md) | The glossary. Non-negotiable vocabulary. |
| [docs/adr/](./docs/adr/) | 13 ADRs — the load-bearing decisions and why. |
| [docs/design.md](./docs/design.md) | Gauntlet semantics: config, rails, verdicts, exit codes. |
| [docs/implementation.md](./docs/implementation.md) | Go-level choices: module, deps, layout, CLI surface, ledger, TOML schemas, testing. |
| [docs/roadmap.md](./docs/roadmap.md) | The five phases, with exit criteria. |

**Do not re-litigate settled decisions.** If you think one is wrong, say so
and stop — don't quietly build something else. If two of them genuinely
contradict each other, that's worth surfacing immediately.

## Your task: phase 1

Read [roadmap.md § Phase 1](./docs/roadmap.md) for scope and exit criteria.
In short: `togi run` on a Go repo produces a normalized, persisted findings
report. No diff scoping, no fixing.

A sequence that keeps every step independently testable:

1. **Skeleton** — `go.mod` (`github.com/joellarson/togi`, go 1.25), `cmd/togi`
   with the cobra root and `version`, empty `internal/` packages.
2. **`internal/repoid`** — root-commit SHA with fallbacks (ADR-0011).
3. **`internal/config`** — XDG path resolution with standard fallbacks
   (ADR-0003). Full three-layer merge is phase 2; phase 1 needs paths.
4. **`internal/finding`** — the schema struct, fingerprint, occurrence
   grouping, JSON encoding (ADR-0005). Get this right; everything keys on it.
5. **`internal/gate`** — manifest and binding parsing, `go:embed` of
   `defaults/gates/`, config-dir override.
6. **`internal/normalizer`** — `golangci-json` and `regex`, golden-file tested.
7. **`internal/enricher`** — the no-op seam only. Real implementation is
   phase 2.
8. **`internal/run`** — parallel execution with timeouts, the errored channel,
   the collector, run ledger, lockfile, pruning.
9. **Report rendering** — compiler-style output and typed exit codes.
10. **`defaults/gates/`** — golangci-lint and gocyclo definitions.
11. **Dogfood** — run togi on togi; fix what it finds.

## Non-negotiables

These are the ones an agent plausibly breaks by accident:

- **Never write anything into a target repo** (ADR-0002). No `.togi/`, no
  config, no state. Config lives under XDG; the only thing that ever touches
  a target repo is the code diff itself.
- **Package names come from the glossary** (ADR-0012), not technical layers.
  New concept ⇒ new glossary entry *and* new package, together.
- **Gates are data; normalizers are compiled Go** (ADR-0004). Adding a gate
  or a language must not require touching Go code.
- **`errored` is not a finding** (design.md). A tool that crashes, is missing,
  times out, or emits garbage sets gate status `errored`. It never becomes
  zero findings, and it never halts the other gates.
- **Raw tool output is persisted to the run dir but never reaches an LLM**
  (ADR-0005). Persisting it is for debugging normalizers.
- **Tests must pass with no external tools installed.** Normalizers are
  tested against recorded fixtures; anything needing real golangci-lint goes
  behind a build tag.
- **Dependencies are cobra and pelletier/go-toml/v2.** Everything else is
  stdlib. Git is driven by shelling to plumbing commands, not go-git. Ask
  before adding anything.

## Verify

```sh
go build ./...
go test ./...
go run ./cmd/togi run --report-only     # dogfood: togi on togi
```

Phase 1 is done when a report-only run on this repo emits `report.json` with
findings from both gates, a second run with no code changes produces an
identical fingerprint set, and a deliberately-broken gate binary yields
`errored` without suppressing the other gate's findings.

## Things to check rather than assume

- **Tool output formats.** Neither gocyclo nor golangci-lint is installed on
  the machine where these docs were written, so both formats in
  implementation.md are unverified. golangci-lint's output flag changed
  between v1 and v2 — confirm against the version you install.
- **The gate TOML schema** in implementation.md is marked *proposed*. Its
  shape is settled; field names are yours to refine as you build the parser.
- **`go 1.25`** in go.mod against a 1.26 local toolchain is deliberate, not a
  mistake.

## Conventions

Match the existing commit style: imperative subject, body explaining *why*
rather than what, trailers preserved. Update `CONTEXT.md` when you introduce
domain vocabulary, and add an ADR only when a decision is hard to reverse,
surprising without context, and the result of a real trade-off.

togi is meant to gate its own commits eventually. Write phase 1 as though it
already does.
