# The wiki is language-agnostic principle pages, aliased from bindings, with no RAG

Briefs need to explain *why* a finding matters and what good looks like. That
knowledge lives in a wiki of **principle pages**: one engineering practice per
page — what it means, why I care, named refactoring techniques (Fowler-catalog
names), one canonical before/after example in any single language, and
constraints. The wiki doubles as my human-readable engineering standards
document.

Three decisions make it hold together:

**Pages are language-agnostic.** Many tool rule_ids across many languages map
onto one page (`gocyclo/*` and `clippy::cognitive_complexity` → the same
page). Translating a principle into idiomatic Go or Rust is the fix agent's
job — that is exactly the judgment worth paying a model for. No parallel
per-language mapping wiki.

**Aliases are declared in the gate's language binding, not in page
frontmatter.** A page shouldn't have to know every tool pointing at it, and a
gate+language stays self-contained (ADR-0004). `togi wiki lint` catches
dangling and conflicting mappings; `togi wiki show <page>` computes the
reverse index on demand.

**Retrieval is deterministic concatenation, never search.** Findings carry
rule_ids, aliases map them to pages, briefs concatenate. No RAG, no embeddings
— identical inputs must produce an identical brief, and a lookup table beats
similarity search for a mapping we already have exactly.

## Consequences

Growth is lazy and failure-driven: the first time a rule fires and the fix
comes out wrong, I write the page, and it's fixed forever after. Per-language
addenda are created **only** after a fix has actually failed for
language-specific reasons — speculation doesn't create pages. A gate with no
page yet still works; the brief is just thinner. Multiple gates sharing a page
is a feature: same page = root-cause grouping signal for triage (ADR-0007).
