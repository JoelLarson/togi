# The findings schema is the insulation layer between tools and the LLM

Every tool's output normalizes to one schema —
`{file, line, end_line?, rule_id, severity, message, snippet, gate, language, fingerprint}`
— and **the fix agent never sees raw tool output**. Without this, every new
tool would change the shape of every brief downstream, and prompt quality
would silently depend on whichever linter happened to fire.

Two fields carry more weight than they look:

- **`end_line`** (optional) makes structural findings ranges rather than
  points. Triage's containment subordination — folding line-level findings
  into the structural finding whose range encloses them — is impossible
  without it, as is touched-entity scope (ADR-0008). Absent means a point
  finding.
- **`fingerprint`** = hash(gate, rule_id, file, whitespace-normalized
  snippet) — deliberately **line-independent**, because togi's whole job is
  editing files, and any identity keyed on line numbers dissolves on the
  first fix. It is the key for stalemate accounting, ratchet baselines, and
  waivers.

## Considered options

- **Positional identity (rule_id + file + line)** — cheap, and wrong: every
  fix that shifts lines above a finding re-keys it.
- **Count-only stalemate detection, no identity** — simpler, but invisible to
  whack-a-mole churn (steady count, rotating set), and waivers and the
  ratchet would each need to invent a key later anyway.

## Consequences

Renames and file moves re-key fingerprints, so findings in a moved file read
as new. Accepted for v1 — the alternative is content-tracking machinery
disproportionate to the payoff.
