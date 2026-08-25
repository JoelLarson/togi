# Rubric

Three sections: **A** and **B** are enforced, **C** is exempt and must never be
raised.

---

## A. Narrative header — the story card, preserved

The `Feature:` block plus its three narrative lines. This is the story card
itself, so it carries the full template.

### A1. Template conformance — *must*

Exactly this shape, one clause per line, in this order:

```gherkin
Feature: <capability, gerund phrase>
  As a <specific role>
  I want <goal>
  So that <benefit>
```

The verb is **`I want`**. This repo adopts Cohn's literal template; `I can`,
`I receive`, `I need`, and `I should be able to` are all findings. See §C5 —
this was decided, not left open, so report the deviation without re-arguing it.

When `I can` or `I receive` appears in more than one feature file, record it
**once** in the report's **Cross-file consistency** section rather than
repeating an identical finding per file.

`Feature:` names a capability, not a component: *Running the gauntlet*, not
*Gauntlet runner*.

### A2. Role specificity — *must*

The role is a specific user type with a stated situation. Cohn's cardinal rule
is that "As a user" is never acceptable, and a bare job title is barely better —
the qualifying clause is what makes the story concrete.

Good, from this repo: `As a developer returning to a repository`,
`As an operator evolving personal standards`.

Then check the role noun against `CONTEXT.md`:

- Not defined there → **must**, glossary gap. Propose a `CONTEXT.md` entry.
  Do not invent the distinction yourself (see SKILL.md constraints).
- Defined, but used inconsistently with that definition → **must**.

### A3. Goal names an outcome, not a mechanism — *must*

This carries Cohn's real concern. He prefers `I want` precisely because desire
leaves the mechanism open; a goal clause that already contains a design decision
has ended the conversation prematurely.

Ask: does the goal name *what the person is trying to achieve*, or *how the
system is built*? Words describing concurrency, storage, layering, file formats,
process structure, or algorithms are mechanism.

- Mechanism in the goal → **must**. Suggest the outcome it was standing in for.
- Genuinely ambiguous (the mechanism *is* the user-visible thing) → **note**.

### A4. `So that` carries value — *should*

This is INVEST's *Valuable*. The benefit must be something the role gains, and
must add information the goal does not already contain.

Failure modes:

- **Restatement** — the benefit paraphrases the goal. Test: delete the goal
  clause; if the benefit still tells you what the feature does and nothing about
  why it matters, it is a restatement.
- **Implementation reason** — "so that the code is cleaner". Not user value.
- **Missing** — Cohn treats `So that` as optional in general, but this repo
  requires it. Absence is a **must**.

### A5. One capability per file — *should*

The header describes a single coherent capability. A header joining two
unrelated goals with "and" is the signal. This is the only sense in which
*Small* survives (see §C2).

---

## B. Body — the conditions of satisfaction

### B1. Each `Scenario:` stands alone — *should*

Every scenario must be runnable **in isolation and in any order**: no dependency
on another scenario's outcome, no shared mutable state, no implied sequencing.
Gherkin and godog both require this outright, and a condition of satisfaction
that cannot be stated without referring to a sibling's result is a finding.

This is the only sense in which *Independent* survives (see §C3).

### B2. Scenarios are distinct conditions — *should*

Each `Scenario:` is one condition of satisfaction — this is where the mapping
lands, so this check carries the weight of the body. Each must prove something
its siblings do not. Two scenarios differing
only in a literal value are one `Scenario Outline` with an `Examples:` table.

Scenario names state the case being proven, not the mechanics:
*A version mismatch is advisory*, not *Test version check*.

### B3. Steps stay declarative — *must*

Steps speak the domain's language and stay above the implementation. No file
paths that are not the subject of the test, no flags, no function names, no CLI
argument strings, no assertions about internal data structures.

This repo is currently strong here; the check guards against regression more
than it fixes an existing problem. Say so rather than manufacturing findings.

### B4. Glossary vocabulary — *must*

Terms must match `CONTEXT.md`, and terms it marks `_Avoid_:` must not appear in
that sense. Read the current list from `CONTEXT.md` at review time rather than
trusting this file; as of writing it includes:

| Use | Avoid |
|---|---|
| gauntlet | pipeline, review |
| gate | check, rule, linter |
| finding | issue, violation, error |
| errored | failed |
| waiver | suppression |

Two cautions:

- `Rule:` is a **Gherkin keyword**. Its presence is never a violation of the
  "avoid *rule*" entry. Only prose using "rule" to mean *gate* is.
- A tool's own `rule_id` is part of the findings schema (ADR-0005) and is
  correct in that sense.

Introducing a concept that has no `CONTEXT.md` entry is a **must** — glossary
gap, propose the entry (ADR-0012 requires the entry and the package together).

### B5. Behaviour, not implementation, in outcomes — *should*

`Then` steps assert observable behaviour: output, exit outcome, persisted
artifacts, what the user sees. Flag assertions about internals the user cannot
observe.

Note that exit codes *are* the user-facing contract here (design.md defines
typed exit codes), so asserting them is correct.

---

## C. Exempt — declare, never raise

These are Cohn criteria that do not transfer to an executable specification.
Do not report them, and do not note that they pass. They are recorded here so
the question stays settled.

**C1. Negotiable.** Applies to the card's lifecycle — details withheld so the
team can negotiate. A checked-in executable spec is the settled outcome of that
negotiation by definition. Never fault a feature file for being specific.

**C2. Estimable, and Small in the sprint sense.** Both exist to serve planning
and velocity. These files are durable regression specs; nothing is being
estimated and nothing must fit an iteration. A file with ten scenarios is not
"too big". *Small* survives only as §A5.

**C3. Independent in the scheduling sense.** Files are organised by glossary
term per ADR-0012 (`gauntlet/`, `gate/`, `wiki/`, `runledger/`, `platform/`) —
the directory *is* the theme, so that coupling to the domain model is the
mapping working as designed, not an accident to refactor away. Never suggest
reorganising for independent scheduling. *Independent* survives only as §B1,
where it applies to scenarios rather than files.

**C4. Card brevity and the three Cs.** Cohn's card is index-card sized and its
*Confirmation* is a brief note on the back. Here the *Conversation* is recorded
in `docs/adr/` and the *Confirmation* is the executable file itself — the
ordering is inverted by design. Never suggest shortening a file toward card
length.

**C5. `I can` versus `I want`.** Resolved in favour of `I want` (§A1). Report
deviations against that rule, but do not re-open the choice or argue the merits
of `I can` in the report.

**C6. Testable.** Exempt by construction. The file executes, so a scenario whose
steps do not bind fails the harness — which is exactly what the `undefined`,
`pending`, and `ambiguous` fixtures under
`features/internal/harness/testdata/` exist to prove. Never report a scenario
as untestable.
