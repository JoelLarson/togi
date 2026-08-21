# Phase 1 Implementation Design

## Purpose

Phase 1 makes `togi run --report-only` execute two deterministic Go gates,
normalize their output into findings, and persist a reproducible report outside
the target repository. It includes the CLI skeleton, repo identity, XDG paths,
gate loading, normalizers, the no-op enricher seam, concurrent execution, the
run ledger, report rendering, and typed exit codes. Diff scoping, config
merging, range enrichment, fixing, and agent integration remain out of scope.

The design in `CONTEXT.md`, the ADRs, `docs/design.md`,
`docs/implementation.md`, and `docs/roadmap.md` remains authoritative. This
document resolves only the implementation boundaries needed to build phase 1.

## Delivery Approach

Build package by package in the sequence from `AGENTS.md`, using
red-green-refactor cycles for every behavioral contract. Each package reaches a
stable, tested API before a downstream package consumes it. Commits remain
small and independently buildable, with the skeleton first and the real-tool
dogfood run last.

## Architecture

`cmd/togi` owns only process startup and Cobra command wiring. Domain behavior
lives in glossary-named packages under `internal/`:

- `repoid` derives the stable repository identity by invoking Git plumbing.
- `config` resolves XDG config, state, and cache paths without performing the
  phase 2 configuration merge.
- `finding` owns the JSON schema, fingerprinting, and occurrence grouping.
- `gate` owns embedded and overridden gate definitions plus TOML decoding and
  validation.
- `normalizer` converts raw tool output into findings using registered,
  compiled parsers.
- `enricher` defines the normalization-to-collection seam and returns findings
  unchanged in phase 1.
- `run` coordinates execution, collection, persistence, locking, pruning,
  rendering, and exit selection.

No additional package is introduced. In particular, execution and persistence
remain parts of a run rather than becoming technical-layer packages.

## CLI Contract

The first binary exposes `togi version`, `togi run`, and `togi status`.
`version` prints build metadata with a development fallback. `run` accepts the
documented phase 1 flags, although `--report-only` is the only behavior.
`status` reads and renders the most recent persisted report; it never executes
gates.

Cobra errors and internal failures are rendered without a stack trace and map
to exit code 70. Findings map to exit code 1 and any errored enabled gate maps
to exit code 4. Gate errors take precedence over findings because the report
is incomplete. A report containing no findings and no errored gates is
`unverified` and maps to exit code 5: phase 1 has no behavioral-suite,
integrity, or seal evidence and therefore cannot claim merge-ready. Exit code 0
first becomes reachable in phase 5 as required by `docs/roadmap.md`.

## Repository Identity and Paths

`repoid` first finds the repository root, then obtains all root commits using
`git rev-list --max-parents=0 HEAD`. One root commit is used directly; multiple
roots are sorted and hashed. When roots are unavailable, a normalized remote
URL is hashed; when no usable remote exists, the absolute, symlink-resolved
repository path is hashed. The human-facing directory name combines the root
basename with a short form of the resulting identity.

`config` honors `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, and `XDG_CACHE_HOME`, with
the documented home-directory fallbacks. It computes paths only. Directory
creation occurs only for state needed by an explicit command and never inside
the target repository.

## Findings

The `finding.Finding` JSON representation follows ADR-0005 exactly. Optional
`end_line` and `occurrences` are omitted when empty. Fingerprints use a
documented SHA-256 encoding of gate, tool-qualified rule ID, slash-normalized
relative file path, and whitespace-normalized snippet. Length-delimited inputs
avoid concatenation ambiguity.

Grouping is deterministic. Findings with the same fingerprint become one
finding; the earliest location remains primary and later distinct locations
become occurrences. Output sorts by file, line, gate, rule ID, and fingerprint
so subprocess completion order cannot affect `report.json`.

## Gate Definitions and Loading

Shipped TOML definitions live below their owner at
`internal/gate/defaults/gates/` so `internal/gate` can embed them. The binary
never extracts these files. At load time, a same-named directory under
`$XDG_CONFIG_HOME/togi/gates/` replaces the embedded gate directory wholesale.

Manifests and bindings use strict TOML decoding and explicit validation for
required fields, enum values, command templates, normalizer references,
timeouts, and severity mappings. Commands are argument arrays rendered with
`text/template`; they never pass through a shell. Phase 1 records tool version
information and mismatch warnings but does not turn a mismatch into an errored
gate.

The golangci-lint binding targets the v2 CLI and writes JSON to stdout using
`--output.json.path=stdout`. The gocyclo binding uses its documented text
format and treats exit code 1 from `-over` as successful execution with
findings.

## Normalization and Enrichment

The opaque normalizer registry maps stable names to compiled Go
implementations without exposing its mutable function map.
`golangci-json` decodes the recorded v2 JSON shape, qualifies rule IDs with
`golangci-lint/`, maps binding severities, reads source snippets from the
target root, and rejects malformed output. The regex normalizer compiles the
named-capture pattern declared by the binding and requires captures needed to
construct a finding. gocyclo findings use `gocyclo/complexity` and include the
reported complexity in the message.

Each normalization batch opens one rooted source session. Relative tool paths
are resolved beneath that root; absolute paths are accepted only when they can
be converted to a path beneath the canonical root. Source access rejects
non-regular files and caps a snippet line at 64 KiB. Normalizer errors describe
the error class and direct operators to persisted raw output without repeating
raw tool lines or capture values.

An enricher interface receives normalized findings and repository context.
The phase 1 implementation returns the input unchanged. The runner still calls
it so phase 2 can add Go ranges without restructuring the pipeline.

## Execution and Error Handling

The runner resolves all requested Go bindings before starting work and runs
them through a worker limit of `min(runtime.NumCPU(), 4)`. Each binding gets a
context deadline from its manifest or cost-class default. Commands run with the
target repository root as their working directory.

Each execution produces a gate result containing status, findings, timing,
version observations, warnings, and structured error details. Missing tools,
unexpected nonzero exits, timeouts, invalid command templates, and malformed
normalizer output produce status `errored`. They do not produce synthetic
findings and do not cancel sibling gates. The collector waits for every gate
before deciding the verdict.

Both stdout and stderr are captured with independent size limits and a visible
truncation marker. Raw bytes are persisted before normalization so parser
failures remain diagnosable. Raw output is never included in any agent-facing
type or artifact.

## Run Ledger

An exclusive per-repository lock is acquired before pruning or creating a run
directory. The lock records enough process information to detect the stale PID
case required by `docs/design.md`; releasing it is deferred across all run
outcomes.

Run IDs are UTC sortable timestamps with a cryptographically random suffix.
At run start, old run directories are pruned to retain the configured maximum
of 20. A run persists raw outputs and then writes `report.json` atomically by
renaming a completed temporary file in the same directory. Interrupted runs
may retain their directory and raw diagnostics but never expose a partial
report as complete.

`report.json` records schema version, run and repository identity, timestamps,
verdict, gate results, grouped findings, counts, and timings. It excludes
machine-specific run-directory paths from fingerprint-bearing data. `status`
chooses the newest complete report by sorting run directory names.

## Rendering

Human output is derived from the same report model that is encoded to JSON.
Findings render in compiler form, grouped and sorted by file and line, with
additional occurrences summarized below the primary location. A tail reports
severity counts, each gate status, warnings, and the final verdict. Color is
enabled only for a terminal and is disabled by `--no-color`; tests assert the
plain representation.

## Testing and Verification

Unit tests require no external gate tools and no network. Repository identity
tests create real Git repositories in `t.TempDir`. Gate-loader tests use
embedded fixtures plus temporary XDG overrides. Normalizer golden tests compare
recorded raw output with stable expected finding JSON. Runner tests put small
fake executables on a temporary `PATH` to exercise success, findings,
malformed output, nonzero exits, timeouts, parallel completion, raw-output
truncation, locking, pruning, and the rule that one errored gate cannot suppress
another gate's findings.

After unit verification, pinned `golangci-lint` and `gocyclo` binaries are
installed in the user's global Go binary directory. The build is dogfooded on
this repository twice and the fingerprint sets are compared independent of run
metadata. A temporary XDG gate override supplies a deliberately broken binary
for the final error-channel check. No verification step writes into the target
repository.
