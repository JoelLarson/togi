# Gates are data modules; normalizers are compiled into togi

A gate is a self-contained directory: a TOML manifest (cost class, fix policy,
scope) plus one subdirectory per supported language holding that language's
binding — tool command, pinned version, normalizer name, and rule_id aliases.
Language support *is* which binding directories exist, so adding a language to
a gate is purely additive and touches nothing else.

The manifests and bindings are pure data, but the code that parses tool output
is not: **normalizers are named, compiled-in Go functions** (`sarif`,
`golangci-json`, `clippy-json`, `regex:<pattern>`), referenced by name from a
binding. An `exec` normalizer is the escape hatch — it delegates to an external
command that must emit findings-schema JSON.

The split is deliberate: adding a gate or a language is a data edit with no
rebuild, but parsing logic — the part with edge cases that deserves tests and
types — stays in Go rather than rotting into untested jq.

## Considered options

- **Fully declarative with exec scripts for everything** — no togi release
  ever needed, but normalizer logic becomes untyped shell that drags in extra
  runtime dependencies, on a machine we just promised to keep clean.
- **Gates as compiled Go packages** — strongest typing, but every new gate or
  threshold tweak means rebuilding the binary, and it contradicts ADR-0003's
  claim that gate definitions are hand-authored config.

## Consequences

A genuinely novel output format needs a togi release. Acceptable: most tools
emit SARIF or a JSON shape one of the existing normalizers already covers, and
`exec` unblocks the rest in the meantime.
