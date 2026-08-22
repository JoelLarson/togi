// Package gitcmd is togi's one doorway to the git CLI. Every invocation
// declares an isolation policy as data: hermetic (user, system, and global
// config ignored; deterministic locale — used for diff scoping) or
// config-honouring (global URL rewrites and includes respected, because they
// are part of a repo's identity — used for repo-id resolution). The
// divergence lives in one policy value, not in hand-maintained environment
// builders.
package gitcmd
