# Go packages are named for glossary terms, not technical layers

`internal/` is split by domain concept — `finding`, `gate`, `normalizer`,
`enricher`, `triage`, `flywheel`, `adapter`, `wiki`, `repoid`, `config`, `run`
— with package names taken straight from [CONTEXT.md](../../CONTEXT.md), and a
thin `cmd/togi` main. There is no `pkg/`: nothing here is a public API.

The usual Go instinct is to split by technical layer (`cli`, `exec`, `parse`,
`store`, `git`), and for a small tool that's perfectly workable. It was
rejected because it scatters one concept across three packages: "finding"
would live in `parse`, `store`, and `cli` at once, so changing the schema
means touching all of them and no single package owns the concept. That is
precisely the low-cohesion shape togi's own gates are built to flag — a tool
that enforces module boundaries should not fail its own check.

The payoff beyond cohesion is navigability: a reader who has read the glossary
can map every term to a package without translation, and so can an agent
working in this repo.

## Consequences

New vocabulary and new packages move together. If a concept earns a package,
it earns a glossary entry — and if two packages keep reaching into each other,
that's evidence the glossary has two names for one thing.
