# togi — implementation decisions

Go-level choices for building togi. Domain vocabulary is in
[CONTEXT.md](../CONTEXT.md), gauntlet semantics in [design.md](./design.md),
phase order in [roadmap.md](./roadmap.md).

## Module and toolchain

- Module path: `github.com/joellarson/togi`
- `go.mod` targets **go 1.25** — one release back from the local toolchain
  (1.26), since nothing in v1 needs 1.26-only features and one version of
  slack is the usual courtesy to anyone building from source.
- **Apache-2.0** (`SPDX-License-Identifier: Apache-2.0`), covering the whole
  repo. `LICENSE` is the verbatim text; copyright is asserted in `NOTICE`.
  Rationale and the rejected alternatives are in
  [ADR-0013](./adr/0013-apache-2-0-licence.md).

## Dependencies

Deliberately few, since togi is mostly subprocess orchestration.

- **`spf13/cobra`** — subcommand CLI. The de-facto standard, and ADR-0001
  commits to a command surface that grows into pipeline stages.
- **`pelletier/go-toml/v2`** — config, gate manifests, bindings. Faster than
  BurntSushi with markedly better decode errors, which matters when what's
  being parsed is hand-authored gate manifests.

Everything else — git, tool execution, hashing, JSON — is stdlib. Git is
driven by shelling to plumbing commands rather than go-git: the behaviours
togi depends on (merge-base, worktrees, root commits) are exactly where a
reimplementation would diverge from real git.

## Platform boundary

Phase 1 supports Linux runtime behavior only. The `run` and `status`
orchestration entry points check the platform before gate loading, subprocess
startup, or any ledger access and return `ErrUnsupportedPlatform` elsewhere.
Platform-specific files retain buildable interfaces and unsupported stubs for
future Darwin, BSD, illumos, AIX, Solaris, and Windows implementations; their
presence is not a claim of runtime support.

On Linux, each gate and version command starts in its own process group. A
timeout or cancellation sends `SIGKILL` to that group before waiting for and
draining the process, so descendants cannot outlive the errored gate or hold
capture pipes open.

## Package layout

`cmd/togi` thin main; `internal/` split by glossary term (ADR-0012):
`finding`, `gate`, `normalizer`, `enricher`, `triage`, `flywheel`, `adapter`,
`wiki`, `repoid`, `config`, `run`. No `pkg/`.

## Gate pipeline

Execute → normalize → **enrich** → collect. The enricher seam exists from
phase 1 (a no-op interface); the Go `go/ast` implementation lands in phase 2
where `end_line` is first consumed, and the Rust counterpart in phase 5.

## Shipped defaults

Gate definitions embed into the binary via `go:embed` from
`internal/gate/defaults/gates/`. Keeping the files below the owning package is
required by `go:embed`, which cannot traverse to a parent directory. togi reads
them from the embed and **never writes them into the config dir**; a same-named
directory under `$XDG_CONFIG_HOME/togi/gates/` overrides one wholesale. `togi
gate eject <name>` copies one out for editing.

Upgrades therefore propagate automatically to everything you haven't
customized, you own only what you explicitly ejected, and a fresh install
creates no surprise files. This is also how togi dogfoods itself: running togi
on togi uses the shipped defaults, so ADR-0002 holds (nothing is read from the
target tree) while the standards stay version-controlled and reviewable.

## Gate manifest and binding schema

**Proposed** — the first thing phase 1 will pressure-test, so treat the shape
as settled and the field names as revisable.

A gate is a directory: `gate.toml` plus one subdirectory per language.

```toml
# internal/gate/defaults/gates/complexity/gate.toml
name        = "complexity"
description = "Function-level complexity limits"
cost_class  = "fast"                  # instant | fast | slow | glacial
fix_policy  = "llm-fix"               # autofix-only | autofix-then-llm | llm-fix | report-only
scope       = "diff"                  # diff | repo
blocking    = ["error", "warning"]    # severities that block merge-ready
timeout     = "60s"                   # optional; cost_class implies a default
```

```toml
# internal/gate/defaults/gates/complexity/go/binding.toml
language   = "go"
tool       = "gocyclo"
command    = ["gocyclo", "-over", "{{.threshold}}", "."]
success_exit_codes = [0]
finding_exit_codes = [1]
normalizer = "regex:^(?P<value>\\d+) \\S+ (?P<symbol>\\S+) (?P<file>[^:]+):(?P<line>\\d+):\\d+$"
rule_id = "gocyclo/complexity"
message = "cyclomatic complexity {{.value}} in {{.symbol}}"

[settings]
threshold = 15                        # per-project overridable; substituted into command

[severity_map]
default = "warning"                   # tool vocabulary -> error | warning | info

[aliases]                             # rule_id -> principle page (ADR-0006)
"gocyclo/complexity" = "small-composable-functions"
```

Notes: `command` is a `text/template` over `[settings]`, which is what makes
thresholds per-project overridable. Aliases may glob (`"golangci-lint/*"`).
Bindings for tools emitting structured output name a compiled normalizer
(`golangci-json`, `sarif`, `clippy-json`) instead of a regex. A finding exit is
accepted only when normalization yields at least one valid finding and stderr
is empty. The gocyclo binding omits `[version]` because its CLI documents no
version flag; the installed module is pinned operationally instead.

## CLI surface

```
togi run [--report-only] [--base <ref>] [--gate <name>] [--verbose] [--no-color]
togi status
togi version
togi gate list | eject <name>
togi config init | show
togi wiki lint | show <page>          # phase 4
togi waive <fingerprint> --reason ""  # phase 3
```

`--report-only` exists from day one even though it is phase 1's only
behaviour, so that when fixing becomes the default in phase 3 the flag surface
doesn't change under existing scripts or shell history.

## Report output

Compiler-style, grouped by file, sorted by path then line:

```
internal/gate/run.go:42: warning: cyclomatic complexity 18 (gocyclo/complexity)
internal/gate/run.go:88: error: unchecked error (golangci-lint/errcheck)
    +2 more at lines 91, 104
```

Every line is clickable in editors and greppable in terminals. A tail
summarises counts, per-gate status including `errored`, and the verdict.
Colour only when stdout is a TTY; `--no-color` forces it off.

## Run ledger

Run IDs are nanosecond-resolution UTC timestamps with a random suffix —
`20260821T151230.123456789Z-a3f1` — so concurrent starts in the same second
still sort chronologically and `togi status` needs no `latest` symlink or
pointer file.

```
$XDG_STATE_HOME/togi/<repo-id>/
├── runs/<run-id>/
│   ├── report.json
│   ├── plan.json          # phase 4
│   ├── briefs/            # phase 4
│   └── raw/<gate>.<lang>.stdout
├── waivers.toml
├── ratchet.json           # phase 5
└── lock
```

Runs prune to the most recent 20 at run start, configurable.

`lock` is a persistent regular file. togi holds an OS advisory lock on its open
file handle for the run lifetime and overwrites its informational
PID/start/token JSON only while locked. Process exit releases ownership; close
unlocks and closes without unlinking. Ledger directories and artifacts are
opened through retained `os.Root` handles, so replacing a state pathname cannot
redirect pruning, raw output, report publication, or status reads.

Completed reports publish by linking a synced same-directory temporary file to
`report.json`. The hard-link operation is atomic and refuses an existing name,
so concurrent publishers cannot clobber one another and readers cannot observe
partial JSON. Linux uses `flock`. Before opening the lock file, the backend
acquires a unique process-local claim keyed by the canonical lock path and
retained repository identity. Only the matching owner releases it, after
successful OS unlock and handle close. Linux verifies `0700` directory modes
and syncs directories after publication. The orchestration boundary returns
`ErrUnsupportedPlatform` on every non-Linux target before `Ledger.Start` or
`Ledger.Latest` can access state.

**Raw tool output is always persisted**, size-capped around 1 MB per gate with
a truncation marker. ADR-0005's insulation governs what reaches the LLM, not
what lands on disk — and the failure it exists to debug (a normalizer silently
yielding zero findings from perfectly valid output) is not an error condition,
so capturing only on failure would miss the case that matters most.

## Tool version handling

A binding may declare a version command, an extraction regex, and a semver
constraint. Phase 1 records the observed version in report.json and warns on
mismatch; hard enforcement — mismatch produces `errored` — lands in phase 3
with the rest of the reproducibility story. This keeps phase 1 from stalling
on `--version` parsing for two tools before a single finding exists.

## Scheduling

A gate manifest may declare a timeout; otherwise its cost class implies one:
`instant` 10s, `fast` 60s, `slow` 10m, `glacial` 60m. **A timeout produces
`errored`, never zero findings.**

Gate concurrency defaults to `min(NumCPU, 4)` and is configurable. golangci-
lint and cargo already saturate cores on their own, so oversubscription rather
than idle CPU is the real risk.

## Testing

- **Normalizers**: golden files — `testdata/<tool>/output.raw` → expected
  findings JSON. The unit suite must pass with no tools installed.
- **Git-touching code** (repo-id, diff scoping, worktrees): real repositories
  built in `t.TempDir()`, not a mocked git. The bugs in that code are all in
  real git's behaviour, so mocking the subprocess boundary would mock exactly
  the layer worth testing.
- **Integration**: a build-tagged suite exercising the real tools.
- No network in any test.
