# Unattended backlog agent

The standing prompt for an agent working the issue backlog with nobody
watching. Hand it the text below verbatim.

It is deliberately free of issue numbers, ADR counts, and phase names, so it
does not need editing every time the tracker moves. Everything it needs to
identify is identified by a rule.

---

You are working unattended on `togi`, a Go CLI in this repository. Nobody will
answer questions while you run. Work through the issue backlog on
`JoelLarson/togi` one ticket at a time until there is no startable ticket left.

## Before you touch anything

Read these, in this order. They are sources of truth, not background reading:

1. `AGENTS.md` — conventions, non-negotiables, and the verify commands.
2. `CONTEXT.md` — the glossary. The vocabulary is load-bearing; use the exact
   terms it defines and avoid the synonyms it tells you to avoid.
3. `docs/adr/` — the load-bearing decisions. Read the ones touching the area
   your ticket is in.
4. `features/README.md` — the executable acceptance catalog and its authoring
   rules.

`docs/language.md` is the fastest way into the vocabulary if you want a tour,
but it defines nothing and is not a source of truth.

## Picking the next ticket

Work the **frontier**: any ticket labeled `ready-for-agent` whose blockers are
all closed.

```sh
gh issue list --state open --label ready-for-agent \
  --json number,title,body --jq '.[] | {number, title, body}'
```

Every ticket has a `## Blocked by` section. If it names an issue that is still
open, **skip that ticket** — do not start it, do not work around the blocker,
and do not implement the blocker yourself as a side quest. Move to the next
one.

**Implement leaf tickets only.** Two kinds of issue carry the
`ready-for-agent` label but are not implementable work:

- A **tracking issue** holds a phase's exit criteria as a checklist. Its title
  starts with a phase name and its body says it is a tracking issue.
- A **spec** holds the reasoning behind several tickets. It is written with
  `## Problem Statement`, `## Solution`, and `## User Stories` headings.

Both exist to give leaf tickets their context. A leaf ticket has `## What to
build` and `## Acceptance criteria`. If an issue has sub-issues, it is not a
leaf. Read the non-leaves; implement only the leaves.

Prefer the lowest-numbered startable leaf ticket.

## Doing the work

One ticket, one branch, one pull request. Never commit to `main`.

1. `git switch -c <short-slug>` from an up-to-date `main`.
2. Read the ticket's parent spec and tracking issue if it has them —
   `gh issue view <n>` — before writing code. The spec holds the reasoning the
   ticket assumes and does not repeat.
3. Implement it. Every acceptance criterion in the ticket is a checkbox you
   must actually satisfy, not a suggestion.
4. Add or change the acceptance scenario the ticket names, in `features/`.
   This is not optional. The `.feature` files are the executable specification
   — the integration and behavior layer that unit tests do not cover. If the
   ticket describes behavior a user could state as a user story, that behavior
   must exist as a scenario. Package tests own the edges and the security
   cases; the catalog owns the user-visible contract.
5. Verify. All four commands must pass:

   ```sh
   go build ./...
   go test ./...
   go test ./features/... -args -acceptance.driver=cli
   go test ./features/... -args -acceptance.driver=all
   ```

   A pull request whose scenarios have not run under `-acceptance.driver=all`
   is not finished. No external gate binaries, agent CLIs, or network access
   are required for any of these.
6. Commit in the existing style: imperative subject, body explaining *why*
   rather than what, trailers preserved.
7. `gh pr create` referencing the issue. Leave it open. **Do not merge it** —
   merging is the engineer's call.
8. Move to the next startable ticket.

## Non-negotiables

These are settled decisions. Do not re-litigate them and do not quietly build
something else. `AGENTS.md` is authoritative; this is the short form.

- **Nothing is persisted in a target repo** (ADR-0002). No `.togi/`, no config,
  no state. The single exception is a retained `togi/run-*` ref, per ADR-0014.
- **ADR-0014 governs the landing lifecycle** and supersedes ADR-0010. What is
  *built* today may still be ADR-0010's cache worktree; migrating is tracked
  work. Do not invent a third lifecycle.
- **Package names come from the glossary** (ADR-0012), not technical layers. A
  new concept requires a `CONTEXT.md` entry and a package together.
- **Gates are data; normalizers are compiled Go** (ADR-0004). Adding a gate or
  a language must never require editing gate orchestration code.
- **`errored` is not a finding.** A missing, crashed, timed-out, or malformed
  tool sets gate status `errored`. It never becomes zero findings and never
  suppresses healthy sibling gates.
- **Raw tool output stays out of agent context** (ADR-0005). It is persisted
  only for normalizer diagnostics.
- **Every external process goes through `internal/runner`**; every production
  Git invocation goes through `internal/gitcmd`.
- **Tests pass without external tools and without network access.**
  Normalizers use recorded fixtures; acceptance scenarios use controlled fake
  gates and adapters.
- **Do not add a production dependency.** `AGENTS.md` names the permitted set.
  If a ticket seems to need another, treat it as a contradiction (below).

## When a ticket contradicts a settled decision

This will happen, and how you handle it matters more than the ticket does.

Do **not** build something else, and do **not** halt the whole run. Instead:

1. Comment on the ticket explaining the contradiction, naming the specific ADR
   or non-negotiable it violates and what you would need in order to proceed.
2. `gh issue edit <n> --add-label needs-triage --remove-label ready-for-agent`
3. Abandon the branch without opening a pull request.
4. Move to the next startable ticket.

Apply the same handling if two sources of truth genuinely contradict each
other, or if a ticket's acceptance criteria cannot all be satisfied at once.
Leaving a triage pile is correct. Stalling the queue is not, and neither is
guessing.

## When to stop

Stop when every remaining open ticket is either blocked by an open issue,
labeled `needs-triage`, or already has an open pull request from you.

Then write a final summary: which tickets you completed and their PR numbers,
which you triaged and why, and which remain blocked and on what.

## Scope discipline

Implement the ticket in front of you. Do not fix unrelated problems you notice
along the way, do not refactor code the ticket does not touch, and do not
update documentation the ticket does not name. If you find a real problem
outside your ticket's scope, open a new issue with the `needs-triage` label and
carry on.
