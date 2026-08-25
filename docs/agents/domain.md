# Domain Docs

How the engineering skills should consume this repo's domain documentation when
exploring the codebase.

This is a **single-context repo**: one `CONTEXT.md` and one `docs/adr/` at the
root. There is no `CONTEXT-MAP.md` and no per-context ADR directory.

## Before exploring, read these

- **[`CONTEXT.md`](../../CONTEXT.md)** at the repo root — the glossary, and
  non-negotiable vocabulary.
- **[`docs/adr/`](../adr/)** — read the ADRs that touch the area you are about
  to work in. There are 14, numbered `0001`–`0014`.

Both exist and are load-bearing here, so there is no "proceed silently if
absent" case to fall back on.

## File structure

```
/
├── CONTEXT.md              ← the glossary
├── AGENTS.md               ← working agreement, non-negotiables, verify commands
├── docs/
│   ├── adr/                ← 0001–0014, the load-bearing decisions
│   ├── design.md           ← gauntlet semantics: config, rails, verdicts, exit codes
│   ├── implementation.md   ← Go-level choices: layout, CLI surface, ledger, schemas
│   ├── language.md         ← guided tour of the vocabulary (not a source of truth)
│   └── roadmap.md          ← the five phases and their exit criteria
├── features/               ← executable acceptance specifications
├── cmd/
└── internal/
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor
proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`.
Don't drift to synonyms the glossary explicitly avoids.

This repo enforces that more strongly than most: per ADR-0012, Go package names
come from the glossary rather than from technical layers, so a new concept
requires a glossary entry and a package together.

If the concept you need isn't in the glossary yet, that's a signal — either
you're inventing language the project doesn't use (reconsider) or there's a
real gap (note it, and let `/grill-with-docs` resolve it).

## Flag ADR conflicts — and stop

`AGENTS.md` says settled decisions are not to be re-litigated. If your output
contradicts an existing ADR, surface the contradiction explicitly and stop.
Do not quietly build something else.

> _Contradicts ADR-0002 (nothing embedded in target repos) — flagging rather
> than proceeding._

The same applies if two sources of truth genuinely contradict each other:
surface it immediately.
