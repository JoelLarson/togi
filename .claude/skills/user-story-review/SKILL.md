---
name: user-story-review
description: Review this repo's acceptance .feature files against how Mike Cohn writes user stories. Use when the user asks to evaluate, review, or critique .feature files, Gherkin narratives, acceptance specs, or user story quality — including mentions of Cohn, INVEST, or "As a / I want / So that". Reports findings only; never edits feature files.
---

# User story review

Evaluate this repo's acceptance specs against Mike Cohn's user story practice,
adapted for executable specifications.

## Governing model

A `.feature` file is the **terminal state** of a Cohn story: the story after
the conversation happened, the detail got fleshed out, and the behaviour was
proven. Cohn's *Negotiable* and his "deliberately incomplete card" describe the
story's **lifecycle**, not its final form — so do not fault a feature file for
being complete and specific. Judge the artifact the story became.

Structural mapping this review assumes:

| Cohn | Gherkin |
|---|---|
| Theme | directory (`gauntlet/`, `gate/`, `wiki/`, `runledger/`, `platform/`) |
| Story (here, always epic-sized) | `Feature:` + narrative header |
| Condition of satisfaction | `Scenario:` |

An epic is a *large story*, not a different artifact — so the `As a / I want /
So that` template belongs to `Feature:`, and the story is the file. Every file
here is epic-sized by construction; that is not a finding (rubric §C2). The
theme is the directory, per ADR-0012.

### Keywords with no Cohn counterpart

`Rule:`, `Background:`, tags, doc strings, and data tables map to nothing in
Cohn. Review their *wording* where the body checks apply — a `Background:` step
is still subject to rubric §B3 and §B4 — but never their presence or absence. A
missing `Background:`, an untagged file, and a file with or without `Rule:` are
all outside this review.

`Rule:` in particular is a Gherkin document heading that godog does not execute.
design.md says to use it only when it makes a feature easier to scan, so whether
a file groups its scenarios is an authoring choice, never a finding.

The tags in `features/platform/supporting_platforms.feature` (`@linux`,
`@unsupported-host`, `@simulated-platform`) are functional — `selector.go` and
`godog.go` under `features/internal/harness/` consume them to select scenarios
by host — not decorative.

## Procedure

1. Read `CONTEXT.md` in full — it is the glossary and the source of truth for
   vocabulary (ADR-0012). List the `docs/adr/` filenames for context; read an
   ADR only when a finding turns on it.
2. Read § *Authoring rules* in `features/README.md`. It is the authoring
   convention of record — declarative language, three to five steps, one event
   per example.
3. Find the specs:

   ```sh
   find features -name '*.feature' -not -path '*/testdata/*'
   ```

   **Never report on `**/testdata/*.feature`.** Those are harness fixtures
   (`undefined`, `pending`, `ambiguous`) that are malformed on purpose.
4. Read every matched file in full.
5. Apply `references/rubric.md`. Read it before judging anything — it carries
   the enforced checks *and* the list of Cohn criteria that are deliberately
   exempt here.
6. Report.

## Report format

One section per file, in `find` order:

```
### features/<path>.feature

- **[severity] <check name>** — <what is wrong, one sentence>
  Suggested: <concrete rewrite, quoting the replacement line(s)>
```

Severities: **must** (violates an enforced rule), **should** (weakens the story
but is defensible), **note** (observation, no action implied).

State "No findings." for a clean file rather than omitting it.

Close with a **Cross-file consistency** section covering things only visible in
aggregate: verb form drift, role vocabulary drift, glossary gaps, and structural
outliers.

## Constraints

- **Report only.** Never edit a `.feature` file, `CONTEXT.md`, or anything else.
  Suggested rewrites go in the report as text.
- **Never decide domain questions.** If a role, term, or concept is undefined in
  `CONTEXT.md`, report it as a glossary gap and propose an entry. The user
  decides; `CONTEXT.md` is the source of truth.
- **Do not raise exempt criteria** (rubric section C), even to note they pass.
