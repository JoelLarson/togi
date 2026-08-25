# togi

togi (研ぎ — the staged craft of Japanese sword polishing) is a CLI that takes
a feature implementation — usually AI-produced — and runs it through a
gauntlet of deterministic quality gates, then loops an agent-driven fix cycle
until the diff is merge-ready or a rail stops it. This context covers the
gauntlet (v1); the wider pipeline (spec → gather → build → verify) will join
it later.

Struggling to hold it all? [docs/language.md](docs/language.md) is a guided
tour — which terms are real today, what order they happen in, and which pairs
get confused. This file stays the definitions; that one defines nothing.

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

**Enricher**:
The language-aware step between normalizing and scoping that resolves an
entity finding to its enclosing top-level declaration — both its range and
its **snippet**, which becomes the declaration's signature. It is the only
step that reads source syntax, and it is dispatched by *language*, where a
**normalizer** is dispatched by binding. Point findings pass through
untouched; an entity finding with no enclosing declaration degrades to a
point.
_Avoid_: enrichment as a general term (it resolves entity findings against
the language's syntax, nothing else)

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
baselines, and waivers. For an entity finding the snippet is the declaration
signature the **enricher** resolved, so identity survives edits inside the
declaration and is independent of where its tool pointed.

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
runs, via the **enricher**: an entity finding's range is *replaced* by its
enclosing top-level declaration's range — both ends, regardless of where the
tool pointed — while a point finding stays on its own line. An entity finding
with no enclosing declaration (an import, a package clause) degrades to a
point rather than erroring. Both are then judged by the same touched-entity
overlap rule.
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
The deterministic post-barrier step that turns collected findings into the
action plan: today, grouping blocking findings by primary file. Containment
subordination, grouping by (file, principle page), and ordering by gauntlet
position are the decided refinement (Phase 4).

**Action plan**:
`plan.json` in the run state dir: the ordered batch list with embedded
findings and pending/running/done/**stuck** statuses; the flywheel consumes
and rewrites it after each state transition. Principle-page references and
resume behavior are the decided additions (Phase 4).

**Flywheel**:
The single serial fix worker consuming the action plan batch-by-batch;
never more than one writer in the worktree.

**Batch**:
One fix unit: the blocking findings sharing a primary file. Each attempt runs
in one fresh agent context and commits only when validation passes; a batch
whose attempts keep failing to change anything goes **stuck**. Grouping by
(file, principle page), falling back to (file, rule_id), is the decided
replacement (Phase 4).

**Behavior**:
What the software does that is meaningful to its operation — in practice,
what the behavioral suite asserts. A fix preserves behavior when it leaves
every assertion intact and never rewrites a test to accommodate a production
change; restructuring behind an unchanged assertion is the work togi exists
to do. Changing a package's exported surface is the risky edge of this, and
warrants more scrutiny than an internal refactor.
_Avoid_: correctness (a fix may not make wrong code right; it makes
maintainable code out of working code)

**Brief**:
The bounded, deterministic document handed to the fix agent: normalized
finding JSON, explicit file:line pointers, and authoritative constraints. It
contains no code beyond finding snippets; the agent reads the worktree itself.
Phase 4 adds matching principle pages and language addenda.

**Adapter**:
The vendor-neutral interface wrapping headless agent CLIs: brief in on stdin,
results read back as the worktree diff. Codex is the implemented adapter;
Claude and Kimi remain later conformance adapters.

**Landing**:
Squash-merging the **run branch**'s validated **batch** commits onto the
feature branch as one commit; refused if the feature branch moved during the
run. Fixing today happens in a togi-owned worktree, isolated from the
original, which is updated only by this guarded landing. Running in the
agent's own idle worktree, and retaining the run branch so its per-batch
commits stay readable, is the decided replacement (ADR-0014).

**Run branch**:
`togi/run-<run-id>`, forked from the feature branch tip: where each validated
**batch** commits, so a run's work is a readable sequence rather than a single
opaque diff. It is the only durable thing togi leaves in a target repo's ref
namespace, and is deleted today on a successful **landing**, preserved when
landing is refused (ADR-0014 retains it instead).

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

A run ends in exactly one **verdict**, and the verdict is what the exit code
names. The codes are part of each definition below, because the report and the
process outcome are one contract, not two.

**Merge-ready**:
The passing verdict (exit 0): all enabled gates pass, none errored, integrity
clean, behavioral suite green, seal passed. Not reachable until Phase 5 ships
the **seal**.

**Findings**:
The verdict (exit 1) when gates completed and reported at least one blocking
finding. A report-only run stops here; a fix run goes on to triage.

**Blocked**:
The verdict (exit 2) when togi refused to go further on evidence it does not
trust: a **stalemate**, an **integrity gate** trip, a regression caught after
local validation, or a **landing** the guards refused. The feature branch is
left untouched.

**Rails-exhausted**:
The verdict (exit 3) when a **rail** — max iterations or wall-clock — stopped
the fix loop before it reached a conclusion. Distinct from **blocked**: nothing
was distrusted, togi simply ran out of budget.

**Errored**:
The verdict (exit 4) when infrastructure, not the code under judgment, ended
the run: a gate **errored** during initial collection, or the selected agent
adapter was missing or unusable. Nothing about the diff's quality is claimed.
_Note_: a gate is also individually **errored** (see Findings) without the run
necessarily taking this verdict.

**Unverified**:
The verdict (exit 5) when togi produced no evidence the diff is merge-ready.
Two conditions reach it: the behavioral baseline is absent or red, so no agent
adapter runs; or a report-only run came back clear, which shows no gate
objected but verifies nothing. Clear is not the same as good.

**Unsealed**:
The successful verdict (exit 6) after every implemented barrier passes and the
guarded **landing** completes or is not needed. Phase 5's **seal** is absent,
so merge-ready is not yet reachable.

**Stalemate**:
The finding set, compared by fingerprint and occurrence count, failed to
strictly shrink across an iteration — covering both stalls and whack-a-mole
churn. It is a *cause*, not a verdict: it produces a **blocked** report naming
persistent fingerprints.

The three adjacent words, smallest to largest: a **batch** goes **stuck** when
its attempts stop changing anything; enough stuck work means the finding set
did not shrink, which is a **stalemate**; a stalemate ends the run with the
**blocked** verdict.

**Rail**:
A hard budget limit. Phase 3 enforces max iterations and wall-clock; agent
spend/tokens are deferred until adapter usage contracts are proven.

**Ratchet**:
"Never worse than last time" — optional repo-wide metric high-water marks
stored in external state, keyed by fingerprint.

**Config tier**:
Where a gate definition or principle page comes from — *shipped* (compiled
into the binary) or *operator* (XDG config, per machine). An operator copy
wholly overrides the shipped one; nothing is ever written into a target
repository (ADR-0002, ADR-0003). There is deliberately no per-project tier
yet: a repo cannot vary the gauntlet.

**Repo-id**:
A target repo's stable identity for external config/state: first root
commit SHA, falling back to normalized remote-URL hash, then absolute-path
hash. Its full hexadecimal value is the external directory name; checkout
names and shortened forms are not path identity.

**Run ledger**:
Everything a run persists in its state dir: public `report.json`, fix-loop
`plan.json`, briefs, private adapter protocol logs and gate output, plus the
private pre-cleanup report audit.

## Relationships

- A **gauntlet** is an ordered list of **gate** names, fixed by the shipped and
  **operator** tiers alone.
- A **gate** owns one **binding** per supported language; each **binding**
  declares its **aliases**; many rule_ids map onto one **principle page**.
- A **binding** produces **findings** through exactly one **normalizer**; the
  **enricher** for that binding's language then resolves each finding's range,
  and only then is **touched-entity scope** applied. An **enricher** is
  required only for a gate whose **location** is `entity`.
- **Triage** turns the collected **findings** into an **action plan** of
  **batches**; the **flywheel** executes them serially via the **adapter**.
- A **batch** commits only when instant/fast gates plus the assigned finding's
  owning gate, **integrity gates**, and the behavioral suite all pass; otherwise
  semantic failure is reset and retried once. Compilation-only test edits
  required by a witnessed production rename are allowed; test discovery
  identities and behavior remain protected.
- A **waiver** neutralizes exactly one **fingerprint**; the **ratchet** and
  **stalemate** accounting are both keyed by **fingerprint**.
- **Touched-entity scope** bounds which findings are *judged*, never which code
  a fix may *change*: a batch may refactor beyond the feature diff, and is held
  to the **integrity gates** and the behavioral suite rather than to a
  boundary.
- Validated **batch** commits accumulate on the **run branch**; **landing**
  squash-merges them onto the feature branch it forked from.
- **Merge-ready** requires the **seal**; Phase 3 success is **unsealed**, and
  **unverified** prevents adapter execution when no green baseline exists.
- A **stalemate** produces the **blocked** verdict; a **rail** produces
  **rails-exhausted**; infrastructure produces **errored**. Every verdict names
  its exit code, so the report and the process outcome never disagree.

## Example dialogue

> **Dev:** "clippy crashed — does that show up as a **finding**?"
> **Domain expert:** "No — that gate goes **errored**, which is a different
> channel. Initial collection still reports sibling gates, but no adapter runs;
> a validation or barrier error terminates the flywheel as infrastructure."
>
> **Dev:** "And if the agent just deletes the failing test?"
> **Domain expert:** "The test-integrity **integrity gate** trips and the run
> blocks. An operator approves that finding's **fingerprint** with a reason
> and re-runs — there is no mid-run prompt."
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
- "snippet" was ambiguous for a structural finding between the line its tool
  reported and the enclosing declaration's signature — resolved: the
  **enricher** resolves it to the signature, so an entity **fingerprint** is
  the declaration's identity rather than an arbitrary interior line. Two
  findings of one rule inside a single declaration therefore collapse into
  one finding, which is the intent: a declaration is over-complex as a whole,
  not at a line.
- "scope" was doing triple duty (gate diff/whole-repo scope, diff-scoping of
  judgment, config scope) — resolved: gate manifests declare *scope*
  (diff-scoped or whole-repo); **touched-entity scope** names the in-scope
  rule for diff-scoped judgment.
