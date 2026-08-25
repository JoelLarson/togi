# togi

**togi** (研ぎ — the staged craft of Japanese sword polishing) takes a feature
diff, runs it through a gauntlet of deterministic quality gates, then loops an
agent-driven fix cycle until the diff is merge-ready or a rail stops it.

An agent can write a feature in minutes. Judging whether it is actually
finished — no complexity creep, no weakened tests, no quietly added lint
suppressions — is the slow part, and the part that doesn't delegate well. togi
encodes one engineer's bar as data, applies it to the committed diff, and
drives an agent through the fixes under supervision it can't talk its way past.

> **Pre-release.** Linux only. Phases 1–2 are complete and the Phase 3 fix loop
> is a working tracer, so a successful run currently ends `unsealed` (exit 6)
> rather than merge-ready. Status per phase: [docs/roadmap.md](./docs/roadmap.md).

## What stops the agent from cheating

An agent asked to make findings go away has cheaper options than fixing the
code. togi checks for them deterministically, every batch:

- **Suppression counter** — new `//nolint`-style suppressions block the run.
- **Test integrity** — deleting, skipping, or weakening a test blocks.
  A compilation-only edit forced by a witnessed production rename is allowed;
  test identities, behavior, and fixtures are not.
- **Suite green after every batch** — behavioral evidence every time, not once
  at the end.
- **Stalemate** — the finding *set* must strictly shrink each iteration.
  Trading three findings for three different ones is not progress.
- **Rails** — max iterations and wall-clock bound the whole run.

The only way past an integrity violation is a **waiver**: a persisted approval
of one specific finding, with a reason and a timestamp. There are no mid-run
prompts — unattended runs are the primary mode, and an audited artifact beats a
keystroke nobody can reconstruct later.

## How it works

**Scope.** togi resolves the merge-base and judges only what the feature
touched — by *entity*, not by line. Change one line inside a function and that
whole function's structural health is yours. Base detection is local and never
hits the network.

**Gate.** A **gate** is a quality-check module defined as data: a TOML manifest
plus a per-language binding naming the tool, its command, its pinned version,
and which compiled-in **normalizer** parses its output. `lint`
(golangci-lint) and `complexity` (gocyclo) ship today; adding a third is a
directory, not a code change. Gates run concurrently, and every tool's output
becomes the same **finding**, identified by a line-independent fingerprint that
survives surrounding code moving. A tool that crashes, is missing, or
mismatches its pinned version is **errored** — a separate channel from "found
problems", which blocks merge-ready but never suppresses its siblings.

**Fix.** A single serial worker batches the findings, assembles a bounded brief
— finding JSON, `file:line` pointers, and the matching principle page from
togi's wiki, with no embedded code — and hands it to an agent through a
vendor-neutral adapter (Codex today; Claude and Kimi next). All of it happens
in a togi-owned external worktree, so your working tree is never a
construction site.

**Land.** A batch commits only when the gates improve, integrity is clean, and
the suite is green; a semantic failure resets and gets one retry. On success
the result is squash-applied onto your feature branch as one commit — refused
outright if the branch moved underneath it.

## Usage

```sh
togi run --report-only          # report findings, change nothing
togi run --agent codex          # fix, then land one squashed commit
togi status                     # the last completed run
```

`--base`, `--gate`, `--max-iterations` and `--max-wall-clock` narrow or bound a
run; `togi wiki show|lint|eject` works on the principle pages. Exit codes are
typed so CI composes without a `--json` retrofit:

| 0 | 1 | 2 | 3 | 4 | 5 | 6 | 70 |
|---|---|---|---|---|---|---|---|
| merge-ready | findings remain | blocked | rails exhausted | infrastructure errored | no green suite | unsealed | internal error |

**Requirements.** Linux, `git`, and a clean worktree with your feature
committed — togi judges `HEAD`, not your unsaved edits. `golangci-lint`
(>=2.12.2 <3.0.0) and `gocyclo` on `PATH`, plus the `codex` CLI for fix mode.
Go 1.25+ to build.

```sh
go install github.com/joellarson/togi/cmd/togi@latest
```

## Notable by design

**Nothing is written into your repository.** No `.togi/` directory, no config,
no state. Gate definitions live in XDG config and run ledgers in XDG state,
keyed by a repo-id derived from the root commit SHA so worktrees share history.

**Gates are data; normalizers are code.** Adding a tool means writing TOML.
Adding a genuinely new output format means writing a normalizer — and the
`exec` escape hatch delegates to any command that emits findings JSON.

The vocabulary above is load-bearing and defined in
[CONTEXT.md](./CONTEXT.md); [docs/language.md](./docs/language.md) is the
guided tour. Design notes are in [docs/](./docs/), decisions in
[docs/adr/](./docs/adr/), and repo conventions in
[CONTRIBUTING.md](./CONTRIBUTING.md).

Apache 2.0 — see [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
