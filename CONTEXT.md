# togi

togi (研ぎ — the staged craft of Japanese sword polishing) is a CLI that takes
a feature implementation — usually AI-produced — and runs it through a
gauntlet of deterministic quality gates, then loops an agent-driven fix cycle
until the diff is merge-ready or a rail stops it. This context covers the
gauntlet (v1); the wider pipeline (spec → gather → build → verify) will join
it later.

## Language

### People

**Engineer**:
The single person togi serves: the owner of the standards it enforces. togi
encodes one engineer's bar, not a team's shared CI policy (ADR-0002). The two
roles below are this person's modes, not separate populations — the same
person on the same machine, doing two different things.

**Developer**:
The engineer judging a feature diff: running the gauntlet, reading findings,
understanding why one matters. The mode that consumes togi's output.
_Avoid_: user (the adjective is fine — user-visible, user-facing — but it is
never a role)

**Operator**:
The engineer tuning togi itself: gate definitions, principle pages, and the
machine it runs on. The mode that shapes togi's behaviour.
_Avoid_: user

### The gauntlet

**Gauntlet**:
The full quality phase for one feature diff: run gates → triage → fix →
re-check, looping until merge-ready or stopped.
_Avoid_: pipeline (reserved for the future spec→build stages), review

**Gate**:
A self-contained quality-check module: manifest + per-language bindings +
normalizer reference + wiki aliases, defined as data in config.
_Avoid_: check, rule, linter

**Binding**:
A gate's per-language implementation: which tool, how to run it, its pinned
version, which normalizer parses it, and its rule_id aliases.

**Normalizer**:
A named, compiled-in parser that converts one tool's raw output into
findings; `exec` is the escape-hatch normalizer that delegates to an
external command emitting findings JSON.

**Cost class**:
A gate's declared runtime tier — `instant` / `fast` / `slow` / `glacial` —
driving which loop tier it runs in.

**Fix policy**:
How a gate's findings get resolved: `autofix-only` / `autofix-then-llm` /
`llm-fix` / `report-only`.

**Integrity gate**:
A deterministic anti-gaming check: suppression counter, test integrity,
naming integrity, suite-green-after-every-batch.

**Seal**:
The one-time final run of glacial gates (e.g. mutation testing) before
declaring merge-ready.

### Findings

**Finding**:
One normalized check result:
`{gate, language, rule_id, severity, file, line, end_line?, snippet, occurrences?, message, fingerprint}`.
_Avoid_: issue, violation, error

**Occurrence**:
One site of a finding that fires identically more than once in a file; the
first is described by the finding's own `line`, the rest by `occurrences`.
Identical hits are one finding, not many.

**Fingerprint**:
A finding's line-independent identity: hash of (gate, rule_id, file,
whitespace-normalized snippet); the key for stalemate accounting, ratchet
baselines, and waivers.

**Snippet**:
The finding's primary source line — for a structural finding, the enclosing
declaration's signature — normalized before hashing and shown raw.

**Severity**:
A finding's canonical weight — `error` / `warning` / `info` — mapped from
the tool's own vocabulary by its binding; the gate manifest declares which
levels block merge-ready.

**Touched-entity scope**:
The diff-scope rule: a finding is in scope when its range intersects, or
contains, any line changed vs. the merge-base — touch a function, own its
structural health.

**Location**:
Whether a gate's findings identify a single point or a structural entity —
`point` / `entity` — declared in the gate manifest alongside, and
independently of, its scope. Location fixes a finding's range before scoping
runs: an entity finding is widened to its enclosing declaration, a point
finding stays on its own line. Both are then judged by the same
touched-entity overlap rule.
_Avoid_: point-scoped, entity-scoped (scope is diff/whole-repo; location is
a separate axis)

**Errored**:
A gate status distinct from findings: its tool crashed, is missing, emitted
unparseable output, or mismatched its pinned version; blocks merge-ready
but never halts other gates.
_Avoid_: failed (that's a gate with findings)

**Waiver**:
An operator's persisted approval of one specific fingerprint, with reason
and timestamp; the only way past an integrity violation.
_Avoid_: suppression (that's the thing integrity gates count in code)

### Wiki

**Principle page**:
A language-agnostic wiki entry: one engineering practice, why it matters,
named refactoring techniques, one canonical example, constraints.

**Alias**:
The many-to-one mapping from a tool's rule_ids to a principle page,
declared in the gate's language binding.

**Addendum**:
A per-language note attached to a principle page, created only after a fix
has actually failed for language-specific reasons — never speculatively.

**Eject**:
Copying a shipped principle page into the operator tier so it can be edited.
Never overwrites an existing operator copy.

### The fix loop

**Triage**:
The deterministic post-barrier step: containment subordination, grouping by
(file, principle page), ordering by gauntlet position — producing the
action plan.

**Action plan**:
`plan.json` in the run state dir: the ordered batch list with embedded
findings, page refs, and pending/done/stuck statuses; the resumable
artifact the flywheel consumes.

**Flywheel**:
The single serial fix worker consuming the action plan batch-by-batch;
never more than one writer in the worktree.

**Batch**:
One fix unit (grouped by file or rule), executed in one fresh agent
context, committed only when validation passes.

**Brief**:
The deterministic concatenation handed to the fix agent: findings (with
their snippets), principle pages, addenda, file:line pointers, constraints
— no embedded code beyond finding snippets; the agent reads the worktree
itself.

**Adapter**:
The vendor-neutral interface wrapping headless agent CLIs (claude / codex /
kimi): brief in on stdin, results read back as the worktree diff.

**Landing**:
Squash-applying the togi branch's finished result onto the feature branch
as one commit; refused if the feature branch moved during the run.

### Execution

**Runner**:
The single bounded-execution primitive beneath every external process togi
starts: one command, a context deadline, whole-process-group kill on
cancellation, and stdout/stderr captured into fixed-size buffers with an
explicit truncation marker. Gates, version probes, and git all go through
it; nothing else in togi spawns a process.
_Avoid_: executor (that names the gate-level orchestration in `run`)

**Gitcmd**:
togi's one doorway to the git CLI. Every invocation declares an isolation
policy as data: *hermetic* (user, system, and global config ignored;
deterministic locale; used for diff scoping) or *config-honouring* (global
URL rewrites and includes respected, because they are part of a repo's
identity; used for repo-id resolution). The divergence lives in one policy
value, not in hand-maintained environment builders.
_Avoid_: raw `exec.Command("git", ...)` anywhere else

### Verdicts, rails, and state

**Merge-ready**:
The passing verdict: all enabled gates pass, none errored, integrity clean,
behavioral suite green, seal passed.

**Unverified**:
The verdict cap when no green behavioral suite exists; fixes may still run,
merge-ready may not be declared.

**Stalemate**:
The finding set, compared by fingerprint and occurrence count, failed to
strictly shrink across an iteration — covering both stalls and whack-a-mole
churn; togi stops with a `blocked` report naming persistent fingerprints.

**Rail**:
A hard budget limit: max iterations, wall-clock, agent spend/tokens.

**Ratchet**:
"Never worse than last time" — optional repo-wide metric high-water marks
stored in external state, keyed by fingerprint.

**Config tier**:
Where a gate definition or principle page comes from — *shipped* (compiled
into the binary), *operator* (XDG config, per machine), or *repository* (per
project). An operator copy wholly overrides the shipped one; nothing is ever
written into a target repository (ADR-0002, ADR-0003).

**Repo-id**:
A target repo's stable identity for external config/state: first root
commit SHA, falling back to normalized remote-URL hash, then absolute-path
hash. Its full hexadecimal value is the external directory name; checkout
names and shortened forms are not path identity.

**Run ledger**:
Everything a run persists in its state dir: report.json, plan.json, briefs,
timings, spend.

## Relationships

- A **gauntlet** is an ordered list of **gate** names; a repo's per-project
  config may override the list, order, and thresholds.
- A **gate** owns one **binding** per supported language; each **binding**
  declares its **aliases**; many rule_ids map onto one **principle page**.
- A **binding** produces **findings** through exactly one **normalizer**.
- **Triage** turns the collected **findings** into an **action plan** of
  **batches**; the **flywheel** executes them serially via the **adapter**.
- A **batch** commits only when instant/fast gates, **integrity gates**, and
  the behavioral suite all pass; otherwise it is reset and retried once.
- A **waiver** neutralizes exactly one **fingerprint**; the **ratchet** and
  **stalemate** accounting are both keyed by **fingerprint**.
- **Merge-ready** requires the **seal**; **unverified** overrides
  merge-ready when no green suite exists.

## Example dialogue

> **Dev:** "clippy crashed on the fix branch — does that show up as a
> **finding**?"
> **Domain expert:** "No — that gate goes **errored**, which is a different
> channel. The **flywheel** keeps working the other gates' findings, but you
> can't reach **merge-ready** while anything is errored."
>
> **Dev:** "And if the agent just deletes the failing test?"
> **Domain expert:** "The test-integrity **integrity gate** trips and the run
> blocks. If the deletion was legitimate, you issue a **waiver** for that
> finding's **fingerprint** and re-run — there's no mid-run prompt."
>
> **Dev:** "The count of findings didn't go down this iteration but they're
> all different ones."
> **Domain expert:** "That's still a **stalemate** — the rail is that the
> finding *set* strictly shrinks, precisely so churn can't masquerade as
> progress."

## Flagged ambiguities

- "pipeline" was used for both the gauntlet and the future
  spec→gather→build→verify arc — resolved: **gauntlet** is the v1 quality
  phase; *pipeline* is reserved for the full future arc.
- "failed gate" was ambiguous between "tool broke" and "found problems" —
  resolved: **errored** (infrastructure) vs. a gate *with findings*
  (signal); they are different channels with different consequences.
- "suppression" was ambiguous between code-level lint suppressions and
  operator approvals — resolved: suppressions are what integrity gates
  count; **waiver** is the operator mechanism.
- "scope" was doing triple duty (gate diff/whole-repo scope, diff-scoping of
  judgment, config scope) — resolved: gate manifests declare *scope*
  (diff-scoped or whole-repo); **touched-entity scope** names the in-scope
  rule for diff-scoped judgment.
