# The fix loop runs in a togi-owned worktree and lands one squashed commit

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
