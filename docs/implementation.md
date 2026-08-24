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

Production dependencies are deliberately few, since togi is mostly subprocess
orchestration.

- **`spf13/cobra`** — subcommand CLI. The de-facto standard, and ADR-0001
  commits to a command surface that grows into pipeline stages.
- **`pelletier/go-toml/v2`** — config, gate manifests, bindings. Faster than
  BurntSushi with markedly better decode errors, which matters when what's
  being parsed is hand-authored gate manifests.
- **`cucumber/godog` v0.16.0** — pinned test-only dependency for the executable
  acceptance specifications under `features/`. It is not linked into the
  production command.

Production functionality beyond Cobra and go-toml — git, tool execution,
hashing, JSON — is stdlib. Git is driven by shelling to plumbing commands
rather than go-git: the behaviours togi depends on (merge-base, worktrees,
root commits) are exactly where a reimplementation would diverge from real
git.

## Platform boundary

The current runtime supports Linux only. The `run` and `status`
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
`wiki`, `repoid`, `config`, `runner`, `gitcmd`, `run`. No `pkg/`.

## Gate pipeline

Execute → Normalize → Group → Enrich → FilterTouched → terminal
Group → collect/report. The first Group creates occurrences so enrichment
can apply entity context to every location. FilterTouched may re-anchor a
finding to its first surviving occurrence, so the terminal Group restores
canonical report order. The Go `go/ast` enricher expands `entity` findings to
the smallest enclosing declaration before diff filtering; `point` findings
retain their reported line. The Rust counterpart lands in phase 5.

## Shipped defaults

Gate definitions embed into the binary via `go:embed` from
`internal/gate/defaults/gates/`. Keeping the files below the owning package is
required by `go:embed`, which cannot traverse to a parent directory. togi reads
them from the embed and **never writes them into the config dir**; a same-named
directory under `$XDG_CONFIG_HOME/togi/gates/` overrides one wholesale.
Operators create or copy an override there when they choose to own a gate
definition; there is no gate-ejection command yet.

Upgrades therefore propagate automatically to every definition you have not
overridden, you own only the operator copies you create, and a fresh install
creates no surprise files. This is also how togi dogfoods itself: running togi
on togi uses the shipped defaults, so ADR-0002 holds (nothing is read from the
target tree) while the standards stay version-controlled and reviewable.

## Gate manifest and binding schema

**Implemented contract.** The shipped gates and operator overrides use this
schema; changing its field names now requires an explicit compatibility
decision.

A gate is a directory: `gate.toml` plus one subdirectory per language.

```toml
# internal/gate/defaults/gates/complexity/gate.toml
name        = "complexity"
description = "Function-level complexity limits"
cost_class  = "fast"                  # instant | fast | slow | glacial
fix_policy  = "llm-fix"               # autofix-only | autofix-then-llm | llm-fix | report-only
scope       = "diff"                  # diff | repo
location    = "entity"                # point | entity
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
threshold = 15                        # substituted into command

[severity_map]
default = "warning"                   # tool vocabulary -> error | warning | info

[aliases]                             # rule_id -> principle page (ADR-0006)
"gocyclo/complexity" = "small-composable-functions"
```

Notes: `command` is a `text/template` over `[settings]`. General settings
overrides are deferred; a gate-directory override still replaces a shipped
gate wholesale. Aliases may glob (`"golangci-lint/*"`).
Bindings for tools emitting structured output name a compiled normalizer
(`golangci-json`, `sarif`, `clippy-json`) instead of a regex. A finding exit is
accepted only when normalization yields at least one valid finding and stderr
is empty. The gocyclo binding omits `[version]` because its CLI documents no
version flag; the installed module is pinned operationally instead.

## CLI surface

```
togi run --report-only [--base <ref>] [--gate <name>] [--verbose] [--no-color]
togi run --agent codex [--base <ref>] [--gate <name>] [--max-iterations <n>] [--max-wall-clock <duration>] [--verbose] [--no-color]
togi status [--no-color]
togi version
togi wiki show <page> | lint | eject <page>
```

Fix mode requires the implemented `codex` adapter. Claude and Kimi remain
later conformance adapters. The default rails are 20 iterations and 30 minutes;
token usage is recorded when the adapter reports it, while spend/token rails
are not enforced. Fingerprint-keyed `togi waive` is a remaining Phase 3 slice.

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

The persisted report schema is version 4. Fix-mode reports add the original
HEAD and feature branch, baseline and final suite evidence, agent identity and
optional usage, iteration/wall-clock rails, batches with attempts and commits,
integrity findings, and guarded-landing status. Phase 3 success is `unsealed`
with exit 6 because the Phase 5 seal is not implemented; a clean run with no
blocking findings is also unsealed and records landing as not needed.

## Run ledger

Run IDs are nanosecond-resolution UTC timestamps with a random suffix —
`20260821T151230.123456789Z-a3f1` — so concurrent starts in the same second
still sort chronologically and `togi status` needs no `latest` symlink or
pointer file.

```
$XDG_STATE_HOME/togi/<repo-id>/
├── runs/<run-id>/
│   ├── report.json
│   ├── report.pre-cleanup.json
│   ├── plan.json
│   ├── briefs/attempt-<digest>.txt
│   ├── adapter/attempt-<digest>.jsonl
│   └── raw/gate-<digest>.{stdout,stderr}
├── waivers.toml          # deferred Phase 3 slice
├── ratchet.json           # phase 5
└── lock
```

Raw filenames fold the gate and language into a bounded SHA-256 digest, so any
compiled gate name persists without a path component derived from it. The name
is therefore opaque: readers derive it with `run.RawOutputName` rather than
parsing identity back out of a directory listing.

Runs prune to the most recent 20 at run start. The retention limit is currently
fixed.

`<repo-id>` is the full stable hexadecimal repository key. The durable path
does not include the checkout basename and does not truncate the key, so linked
worktrees and renamed checkouts share ledger and lock state without introducing
short-key collisions. A human-readable label can be stored as display metadata
later; it is not part of path identity.

`lock` is a persistent regular file. togi holds an OS advisory lock on its open
file handle for the run lifetime and overwrites its informational
PID/start/token JSON only while locked. Process exit releases ownership; close
unlocks and closes without unlinking. Ledger directories and artifacts are
opened through retained `os.Root` handles, so replacing a state pathname cannot
redirect pruning, raw output, report publication, or status reads.

Fix mode first composes and validates the final evidence and stages its human
rendering, then atomically publishes a private, bounded
`report.pre-cleanup.json` audit artifact. If a workspace was created, cleanup
then removes the owned worktree and removes the run branch when its disposition
permits, while retaining the ledger lock. A branch containing validated
unlanded work is preserved. If cleanup changes the public outcome, the report
is recomposed and restaged;
only then is bounded `report.json` published and made visible to `togi status`,
after which stdout is emitted and the ledger closes. The audit artifact is
immutable and excluded from latest-run discovery, preserving pre-cleanup
evidence even when cleanup fails. If audit publication fails, deferred cleanup
still runs and no completed report is published.

Reports publish by linking a synced same-directory temporary file to their
final name. The hard-link operation is atomic and refuses an existing name, so
concurrent publishers cannot clobber one another and readers cannot observe
partial JSON. Report files are capped at 16 MiB. Plan updates use atomic
replacement; briefs and adapter JSONL logs are immutable per batch attempt,
and adapter logs are capped at 1 MiB with an explicit truncation marker.
Artifact names fold untrusted identities into bounded SHA-256 digests. Files
are mode `0600` under the external `0700` ledger and never enter the target
repository. Linux uses `flock`. Before opening the lock file, the backend
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
constraint. The observed version is recorded in report.json; a mismatch is an
`errored` gate and therefore prevents adapter execution in fix mode.

## Scheduling

A gate manifest may declare a timeout; otherwise its cost class implies one:
`instant` 10s, `fast` 60s, `slow` 10m, `glacial` 60m. **A timeout produces
`errored`, never zero findings.**

Gate concurrency is currently fixed at `min(NumCPU, 4)`. golangci-lint and
cargo already saturate cores on their own, so oversubscription rather than idle
CPU is the real risk.

## Testing

- **Normalizers**: golden files — `testdata/<tool>/output.raw` → expected
  findings JSON. The unit suite must pass with no tools installed.
- **Git-touching code** (repo-id, diff scoping, worktrees): real repositories
  built in `t.TempDir()`, not a mocked git. The bugs in that code are all in
  real git's behaviour, so mocking the subprocess boundary would mock exactly
  the layer worth testing.
- **Acceptance**: service and compiled-CLI drivers use scenario-owned fake
  gates and adapters, real temporary Git repositories, and isolated XDG roots.
- **Real tools**: verify their external CLI contracts separately when
  installed; the automated suite does not require them.
- No network in any test.
