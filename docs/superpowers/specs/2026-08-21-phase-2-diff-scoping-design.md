# Phase 2 Diff Scoping Design

## Purpose

Phase 2 makes togi judge only committed changes between a feature branch and
its merge-base. It implements Go declaration-range enrichment so structural
findings on touched entities remain visible even when the tool reports only an
unchanged declaration line.

The phase deliberately does not add `config.toml`, project overrides,
provenance, or `togi config init/show`. Shipped gate definitions remain the
behavioral defaults. General user-authored configuration is deferred until
real use demonstrates which settings need a stable public contract.

## Repository Preconditions

Every run requires the target worktree to be fully clean before ledger state
is created or gates execute. Staged, unstaged, and untracked files all cause a
clear error. This protects pending work and ensures gates inspect the same
committed tree used to compute diff scope.

Any tracked Git submodule (gitlink) is unsupported in Phase 2, whether it is
initialized or uninitialized, clean or dirty. Gitlinks are detected from the
index and rejected before `git status`, ledger creation, or gate execution;
recursive submodule scope and cleanliness handling is deferred.

Phase 2 continues to inspect the caller's worktree in place and never writes
to it. The isolated worktree lifecycle remains Phase 3 work.

## Base and Diff Resolution

The base ref resolves in this order:

1. `togi run --base <ref>`
2. the symbolic ref at `refs/remotes/origin/HEAD`

If neither is available, the run fails and asks for `--base`; it does not
guess conventional branch names or use `HEAD^`. The selected ref is verified
as a commit before `git merge-base HEAD <base>` runs. Git commands receive
argument arrays directly and never pass through a shell.

Changed paths come from Git's NUL-delimited diff output so unusual filenames
remain unambiguous. Zero-context diffs provide changed line ranges on the new
side. For a pure-deletion hunk, the anchor is its new-side start line when that
line exists, otherwise the file's final line; an empty file has no anchor.
This allows a deletion inside a declaration to mark that declaration as
touched. Deleted files do not produce current-tree findings. Renamed files use
their new path.

Scope resolution happens before ledger creation or gate execution. A missing
Git binary, dirty worktree, invalid base, absent `origin/HEAD`, unrelated
history, cancellation, or malformed Git output returns a clear internal error
without publishing a report.

## Finding Location Semantics

The current gate model cannot distinguish structural findings that need an
enclosing declaration range from point findings that must remain points. Phase
2 adds a manifest field:

```toml
location = "entity"
```

Allowed values are `point` and `entity`; omission defaults safely to `point`.
The shipped complexity gate declares `entity`, while the lint gate remains a
point gate. This keeps gate semantics in data and language parsing in compiled
Go, consistent with ADR-0004.

The executor passes the manifest's location mode into the enricher. Point
findings are returned unchanged. For an entity finding in a Go file, the Go
enricher parses the file with `go/parser`, finds the smallest enclosing
declaration at the reported line, and sets `end_line` to the declaration's end.
A finding outside a declaration remains a point. A parse or source-access
failure marks that gate `errored`; sibling gates continue normally.

## Touched-Entity Filtering

The gate pipeline becomes:

```text
execute -> normalize -> enrich -> filter scope -> group -> collect
```

Repo-scoped gates bypass filtering. For diff-scoped gates, a location survives
when its inclusive `[line, end_line]` range contains a changed line. An absent
`end_line` is treated as `line`, so point findings require direct intersection.

Filtering considers a finding's primary location and every occurrence before
grouping. When the primary is removed but a later occurrence survives, the
earliest surviving occurrence becomes primary. The fingerprint remains stable
because location is intentionally absent from its identity. A grouped finding
is removed only when none of its locations survive.

The responsibilities remain in existing glossary-named packages:

- `internal/run` owns repository checks, Git resolution, changed-line data,
  scope metadata, and orchestration.
- `internal/enricher` owns Go AST range enrichment.
- `internal/finding` owns touched-entity and occurrence filtering.
- `internal/gate` owns location-mode decoding and validation.
- `cmd/togi` exposes `--base` and wires the Go enricher.

No technical-layer package is introduced.

## Report Contract

Phase 2 reports use schema version 2 exclusively. The project is pre-release,
so `togi status` does not retain compatibility with Phase 1 report files.

The report records the requested or detected base ref, resolved base commit,
merge-base commit, feature `HEAD`, and changed file and line counts. This is
enough to explain which committed snapshot was judged without persisting the
entire diff in the ledger.

## CLI Contract

`togi run --base <ref>` becomes active. Without it, the run uses
`origin/HEAD`. Existing `--gate`, `--report-only`, `--verbose`, and
`--no-color` behavior remains unchanged. `togi config init` and
`togi config show` are not added in this phase.

## Testing and Verification

All automated tests remain network-free and require no external gate tools.
Git behavior is exercised with real repositories under `t.TempDir`; gate
execution uses fixture commands.

Coverage includes branch divergence, explicit and detected bases, missing
`origin/HEAD`, unrelated histories, additions, pure deletions, renames,
unusual paths, dirty staged/unstaged/untracked states, structural versus point
findings, smallest-declaration selection, findings outside declarations,
occurrence promotion, repo-scoped bypass, Go syntax failures, deterministic
reports, sibling-gate survival after enrichment failure, and pre-status
rejection of clean, dirty, initialized, and uninitialized gitlinks.

Completion verification remains:

```sh
go build ./...
go test ./...
go run ./cmd/togi run --report-only
```

The dogfood run must use a clean feature branch with a resolvable base. A
change inside a function must surface that function's structural finding even
when the finding's reported declaration line is unchanged, while unrelated
repository findings remain excluded.
