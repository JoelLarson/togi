# The fix loop runs in a togi-owned worktree and lands its batch commits

Fixing happens in a `git worktree` under the cache dir, on a branch
`togi/run-<id>` cut from the feature branch's HEAD. The user's checkout is
never touched — no dirty-tree precondition, no stashing, and they can keep
working while a run is in flight.

Inside the worktree, **each validated batch is a commit**. That's not for
history's sake; it's the rollback point that makes "the behavioral suite must
be green after every fix batch" enforceable — a batch that breaks the suite or
trips an integrity gate is `git reset` back to the last green batch, and the
loop continues from known-good.

On success togi lands **one squashed commit** on the feature branch. The
iteration mess — thirty batch commits, resets, retries — is exactly what
shouldn't appear in a feature branch's history. If the feature branch moved
during the run, togi refuses to land and leaves `togi/run-<id>` in place for
manual merge rather than guessing.

## Consequences

- Worktrees live in the cache dir, which ADR-0003 says must be safe to delete;
  batch commits stay reachable via the real `togi/run-<id>` branch ref in the
  repo, so a wiped cache costs the checkout, not the work.
- A fresh worktree means a cold `target/` for Rust — a real cost, accepted for
  v1. A shared `CARGO_TARGET_DIR` is the obvious later fix.
- A dirty user checkout is a warning, not an error: togi branches from HEAD,
  so uncommitted work simply isn't part of what gets judged.

## Refinement: 2026-08-24

The Phase 3 tracer preserves the isolation decision and tightens its admission
and landing mechanics. The historical allowance for a dirty checkout above no
longer applies: the Phase 2 clean-worktree precondition requires a clean,
committed feature branch before the run ledger or any agent work begins.

- After admission, the original worktree remains isolated from all fixing.
  Agents and validators operate only in a togi-owned external worktree; the
  original worktree is updated once, at guarded landing.
- Each accepted batch is committed only from a validated immutable tree.
  Before landing, the all-gate barrier and full behavioral suite run against
  immutable snapshots of that exact candidate.
- Landing creates one verified squash commit with the exact validated tree,
  the original feature commit as its sole parent, a fixed subject, and explicit
  identity, then performs a hook-disabled fast-forward through descriptor-bound
  Git directories.
- A concurrent dirty checkout, detach, feature-ref move, or relevant Git
  ownership/config/control-state change refuses landing or triggers guarded
  recovery that preserves concurrent state. The validated `togi/run-<id>`
  branch remains available whenever landing is incomplete.
- Cleanup removes the cache worktree after every resolved disposition. It
  removes the run branch after a complete landing or when it contains no
  validated work beyond the original HEAD; a validated but unlanded branch is
  preserved for inspection. Any ambiguous cleanup state fails closed.

This refinement changes neither the external-worktree decision nor the
one-squash outcome; it records the evidence required to make both enforceable.

## Amendment: 2026-08-25 — landing preserves the batch commits

The one-squash conclusion above is reversed. Landing fast-forwards the
validated `togi/run-<id>` commits onto the feature branch as they stand: one
commit per accepted batch. The external-worktree decision, the batch-as-
rollback-point mechanism, and every landing guard are unchanged.

The original reasoning was that "thirty batch commits, resets, retries" is
iteration mess that does not belong in a feature branch's history. That
premise does not survive its own reset policy. A batch that fails validation
is reset away and leaves no commit; a retry that succeeds leaves one. What
remains on the run branch is already exactly the sequence of validated
batches — not mess, but the cleanest possible account of what changed and
why.

Two reasons to keep it:

- **Reviewability is the point.** One squashed commit touching a dozen files
  is the hardest thing in the pull request to review. One commit per batch,
  each naming the principle page it applied, is individually obvious.
- **The history teaches.** Every subject line carries the principle page
  name, which is the same string that indexes the wiki, so the commit log
  becomes a queryable record of which practices this codebase keeps
  violating.

Batch commit messages are deterministic — generated from the finding set, not
from the agent — so a retry that produces a different implementation still
produces the same message:

```
togi: small-composable-functions in internal/run/fix.go

gocyclo/complexity — resolveStagedDiff, prepareFixRun
Run 01JQ8F… · batch 3/6
```

Mechanically this is a simplification: landing was already a hook-disabled
fast-forward with the squash constructed on top, and that construction step
goes away.

### Reverting a run

The squash's real advantage was that undoing a run was one `git revert`. That
is preserved without it: landing fast-forwards onto a known feature HEAD, and
that commit is already recorded in the run ledger, so `git reset --hard
<pre-landing-sha>` undoes the entire run exactly. `togi status` surfaces that
sha so it never has to be dug out of `report.json`. A dedicated `togi revert`
was considered and rejected — it would be the only destructive Git operation
in a tool that otherwise only ever fast-forwards, and it earns nothing over a
reset the engineer can read before running.
