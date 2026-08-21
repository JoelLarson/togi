# XDG-compliant storage, split three ways by durability

Because nothing lives in target repos (ADR-0002), togi owns a meaningful
amount of external state, and conflating "things I hand-wrote" with "things a
run generated" would make it impossible to back up the former or safely
delete the latter. State is split by durability:

- `$XDG_CONFIG_HOME/togi/` — hand-authored and durable: global config,
  per-project overrides, gate definitions, the wiki. Back this up; it *is*
  the engineering standard.
- `$XDG_STATE_HOME/togi/<repo-id>/` — machine-generated but must survive:
  run ledgers, findings, baselines, ratchets, waivers, locks.
- `$XDG_CACHE_HOME/togi/` — reconstructible: worktrees, scratch.

The env vars are honored with the standard fallbacks (`~/.config`,
`~/.local/state`, `~/.cache`).

## Consequences

Deleting the cache dir must always be safe. That constrains ADR-0010: fix
worktrees live in cache, so their per-batch commits are kept reachable by a
real branch ref in the repo rather than by the worktree's existence.
