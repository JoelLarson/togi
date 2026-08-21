# Fixes go through a vendor-neutral adapter over headless CLIs

Fix batches shell out to headless agent CLIs (`claude -p`, `codex exec`, kimi)
behind a small adapter interface, configurable per gate and per role (triage
vs. fix). No vendor SDK, no API keys — subscription CLIs only, and all
vendors stay symmetric.

Taking any vendor's SDK would make that vendor structurally special: its
features would shape togi's internals, and the others would be second-class
forever. The adapter keeps the seam at the process boundary, where all three
are genuinely interchangeable.

The transport is deliberately boring: **the brief goes in on stdin** (no
arg-length limits, nothing sensitive in `ps`) and is also written to the run
state dir as an audit trail. **The agent's working directory is the togi
worktree and it edits files directly** — that's what these CLIs are built to
do, and "results coming back" is simply the worktree diff. Validation before a
batch is committed: instant/fast gates, integrity gates, suite green; failure
resets to the last green batch (ADR-0010).

## Considered options

- **Agent emits a patch on stdout that togi applies** — a stronger sandbox on
  paper, but models are markedly worse at emitting valid unified diffs than at
  editing files, so it trades real fix quality for theoretical safety the
  worktree already provides.
- **Claude Agent SDK** — schema-enforced outputs and hooks are genuinely
  useful; revisit per-stage if they become necessary, accepting the asymmetry
  knowingly rather than by default.
