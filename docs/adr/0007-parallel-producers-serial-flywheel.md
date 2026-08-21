# Parallel gate execution, a barrier, deterministic triage, then one serial fix worker

Reads are parallel; writes are serial. All enabled gates for the current cost
tier run concurrently, streaming normalized findings to a collector. At the
barrier, **triage** groups them into an ordered action plan, and a single
**flywheel** worker executes it batch-by-batch — **never more than one writer
in the worktree**.

Collect-then-plan beats fixing as findings stream in, because structural fixes
invalidate line-level findings: extracting a function moves or deletes the
lint nits inside it. Fixing a nit that's about to be refactored away is wasted
spend and a merge conflict with your own next batch. That same logic sets the
ordering — structural before line-level.

**Triage is fully deterministic in v1**: containment subordination (findings
whose range falls inside a structural finding's range fold under it) → group
by (file, principle page), falling back to (file, rule_id) → order by gauntlet
position, then path. The output is `plan.json` in the run state dir: ordered
batches with embedded findings, page refs, and pending/done/stuck status —
resumable, inspectable, diffable.

Each batch runs in a **fresh agent context** with state only in files (the
ralph property); no conversation history carries between batches.

## Considered options

- **LLM-led or hybrid triage** — could catch cross-file root causes the
  heuristics miss, but puts cost, latency, and nondeterminism in the core
  loop. Reserved as a later opt-in flag, off by default, to be added only if
  deterministic plans prove dumb in practice.
- **Parallel fix workers (worktree per agent)** — faster, but concurrent
  writers to one tree turn every structural refactor into a merge problem.
  Explicitly out of scope for v1.
