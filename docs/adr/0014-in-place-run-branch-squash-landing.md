# The fix loop runs in the agent's own worktree and squash-lands a retained run branch

Supersedes [ADR-0010](./0010-togi-owned-worktree-batch-landing.md).

togi runs after an agent has finished building a feature. By the time the
gauntlet starts, that agent's worktree is *idle*: the work is committed, the
branch is at its tip, and nothing else is writing there. ADR-0010 built an
isolated worktree in the cache dir to protect a checkout that, in the workflow
togi is actually for, has no one in it.

So the fix loop runs **in place**, in the worktree the agent left behind.

## Decision

- **Entry is a refusal, not a negotiation.** togi refuses to start unless the
  worktree is clean and on the expected feature branch. There is no stashing,
  no HEAD juggling, and no crash-time restoration to get right — the only
  failure mode is "togi wouldn't start," which the engineer resolves by
  committing or switching back.
- **`togi/run-<id>` is cut from the feature branch tip** and checked out in
  that worktree. Each validated batch commits to it, exactly as ADR-0010
  described: the commit is the rollback point that makes "suite green after
  every batch" enforceable.
- **Landing squash-merges the run branch** onto the branch it forked from, as
  one commit. If the feature branch moved during the run — possible from
  another checkout of the same repo — landing is refused: the squash would
  otherwise sit on a base nothing validated.
- **The run branch is retained** after a successful landing.

## Why squash, and why retain

ADR-0010's amendment reversed its own squash decision to make the run
reviewable: one commit per batch, each naming its principle page. The goal was
right; the mechanism put the iteration record in the wrong place.

Retaining `togi/run-<id>` gets both properties at once. The per-batch commits
survive and stay readable — the amendment's whole point — while the feature
branch receives the single commit that belongs in a pull request. Reviewers who
want the sequence check out the run branch; reviewers who want the change read
one diff. Neither audience is served by forcing the other's view on them.

Reverting a run stays a `git reset --hard <pre-landing-sha>` against a sha the
run ledger records and `togi status` surfaces.

## Consequences

- **This is the first thing togi durably leaves in a target repo.** ADR-0002
  says the code diff is the only thing that ever touches a target repo; a
  retained `togi/run-*` ref is a real exception to that, narrowed to the ref
  namespace — still no `.togi/`, no config, no state, nothing in the tree.
- **Run branches accumulate.** Pruning is deliberately unspecified for now;
  refs are cheap and the retention rule can be chosen once there is evidence
  about how many runs an engineer actually revisits.
- **A bad fix lands in the engineer's real working tree** rather than a scratch
  one. It is committed on the run branch and never on the feature branch, but
  the files on disk are the ones they will open next. The clean-and-expected
  entry check is what makes that acceptable: togi is never competing with
  in-flight work.
- **The cache worktree disappears**, and with it ADR-0010's cold-`target/`
  problem for Rust — the agent's build artifacts are already warm.
- The existing dirty / detached / branch-moved landing refusals are rewritten:
  the first two become entry preconditions, and only branch-moved stays a
  landing-time guard.
