# Local Trunk Base Discovery Design

## Purpose

togi runs after an AI agent finishes a feature in a linked Git worktree. The
feature branch is expected to merge into a trunk branch, normally `main` or
`master`, and every commit since that branch's merge-base belongs to the scope
of the run.

Requiring `origin/HEAD` makes that workflow unnecessarily depend on remote
metadata. A repository can have a valid local trunk without any remote, and a
clone can have `origin/main` without a symbolic `origin/HEAD`. Base discovery
therefore needs deterministic conventional fallbacks while retaining
`--base` for repositories that use a different topology.

## Resolution Order

When `togi run` resolves the comparison base, it tries these candidates in
order:

1. the explicit value of `--base`;
2. the symbolic ref at `refs/remotes/origin/HEAD`;
3. `refs/remotes/origin/main`;
4. `refs/remotes/origin/master`;
5. `refs/heads/main`;
6. `refs/heads/master`.

The first existing candidate wins. Every candidate is verified as a commit
before use. A candidate that exists but is not a commit fails immediately;
only a missing candidate advances discovery to the next ref. An explicit
invalid base likewise never falls through to an inferred candidate.

Remote-tracking refs are local Git data. Discovery does not fetch, contact a
remote, or otherwise use the network. Preferring an available remote-tracking
trunk keeps the inferred base aligned with the PR target when the corresponding
local branch is stale.

If no candidate is available, the run fails before ledger creation or gate
execution and asks the operator to pass `--base`.

## Diff Semantics

After selecting a base ref, togi continues to compute:

```text
git merge-base HEAD <base>
```

The merge-base, not the current tip of the selected trunk, defines the start
of the feature diff. A multi-commit feature branch is judged as one unit. If
the trunk advances and the feature branch is rebased onto it, the next run's
merge-base advances with the rebase.

Running togi directly on the selected trunk yields an empty diff. It does not
fall back to `HEAD^`, because a single commit is not the product boundary:
the completed feature branch is.

The report persists the candidate name that won discovery, along with the
resolved base commit, merge-base, and feature `HEAD`, under the existing
schema-version-2 contract.

## Safety And Errors

Discovery uses the existing sanitized, bounded Git-command path. It inherits
the current cancellation, output-size, environment-isolation, and no-target-
write guarantees. Adding fallback candidates does not weaken clean-worktree,
submodule, attributes, conversion-filter, or object-ID validation.

Failure diagnostics distinguish an explicit invalid base from exhaustion of
automatic candidates. The automatic failure names the supported conventional
trunks and recommends `--base` for any other branch model.

## Testing

Real temporary Git repositories cover:

- explicit `--base` precedence over every inferred ref;
- symbolic `origin/HEAD` precedence;
- `origin/main`, `origin/master`, local `main`, and local `master` fallback;
- remote-tracking precedence when the corresponding local trunk is stale;
- a remote-free linked feature worktree with multiple commits;
- merge-base movement after a simulated rebase;
- an empty diff when running on the selected trunk;
- failure before ledger or gate execution when no candidate exists;
- proof that discovery invokes no network-facing Git command.

Existing malformed-ref, unrelated-history, cancellation, cleanliness, and
report-metadata tests remain in force.
