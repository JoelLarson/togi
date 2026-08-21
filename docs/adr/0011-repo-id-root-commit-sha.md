# repo-id is the root-commit SHA, with a fallback chain

Because nothing lives in the target repo (ADR-0002), togi has to *derive* a
repo's identity to find its config, ratchet, waivers, and run ledgers. The key
is the repo's **first root commit** (`git rev-list --max-parents=0 HEAD`,
sorted and hashed when there are several).

A root commit is the one identifier that survives everything that actually
happens to repos: cloning, moving the directory, adding worktrees, renaming
the remote, migrating host. Forks share it — which is right: a fork of a
project I've configured should inherit that configuration.

Fallbacks, in order: normalized remote-URL hash (shallow clones with no root
commit available), then absolute-path hash (a repo with no commits yet). State
directories are named with the full repo-id. Using the key alone preserves the
identity across linked worktrees and directory renames, and avoids collisions
introduced by truncating it. Human-readable labels belong in display metadata,
not path identity. An explicit override lives in the global `repos.toml` — not
per-project config, which is itself keyed by repo-id. This correction precedes
the first release, so the earlier basename-and-short-id prototype has no
supported state to migrate.

## Considered options

- **Normalized remote-URL hash as primary** — human-explainable and stable
  across clones, but breaks on repos with no remote, needs ssh/https/`.git`
  normalization, splits forks apart, and dies on remote migrations.
- **Absolute root path hash as primary** — trivial and git-free, but a `mv`
  discards all accumulated state and every worktree gets its own ratchet,
  which is simply wrong.
