# The findings schema is the insulation layer between tools and the LLM

Every tool's output normalizes to one schema, and **the fix agent never sees
raw tool output**. Without this, every new tool would change the shape of
every brief downstream, and prompt quality would silently depend on whichever
linter happened to fire.

```
{
  gate, language, rule_id, severity,
  file, line, end_line?, snippet,
  occurrences?: [{line, end_line?}],
  message, fingerprint
}
```

Five decisions give the schema its teeth.

**`end_line` is optional, making structural findings ranges rather than
points.** Triage's containment subordination — folding line-level findings
into the structural finding whose range encloses them — is impossible without
it, as is touched-entity scope (ADR-0008). Absent means a point finding. Most
tools report points, so togi derives ranges itself via a per-language range
enricher.

**`rule_id` is tool-qualified**: `gocyclo/complexity`,
`golangci-lint/errcheck`, `clippy/cognitive_complexity`. Globally unique,
globs cleanly for aliases (`gocyclo/*` → one principle page), and truthful
about which tool spoke. Gate-qualifying it instead would duplicate the `gate`
field and re-key every fingerprint in the repo whenever a gate swapped its
underlying tool.

**`severity` is a three-value canonical vocabulary** — `error` | `warning` |
`info` — that each language binding maps its tool's vocabulary onto
(`note`, `style`, `nursery`, and friends all land somewhere). Each gate
manifest declares which levels block merge-ready; by default `error` and
`warning` block and `info` is advisory and never enters the fix loop. One
consistent vocabulary is the whole point of insulation.

**`snippet` is the finding's primary source line, hashed after whitespace
normalization and displayed raw.** For a structural finding that line is the
function signature, which is exactly right: editing the body doesn't re-key
the finding, so a complexity finding survives a partial refactor and stalemate
tracking stays honest. Hashing the full range would re-key on every body edit;
hashing surrounding context would produce phantom findings when a neighbour
changed.

**`fingerprint` = hash(gate, rule_id, file, normalized snippet)** —
deliberately **line-independent**, because togi's whole job is editing files
and any identity keyed on line numbers dissolves on the first fix. It is the
key for stalemate accounting, ratchet baselines, and waivers.

## Identical findings are one finding with many occurrences

That fingerprint definition collides when one rule fires on identical source
lines in one file — the same magic number twice would be two findings with one
fingerprint. Rather than discriminating them apart, identical
`(gate, rule_id, file, snippet)` tuples are deliberately **one finding**:
`line`/`end_line` describe the first occurrence and `occurrences` carries the
rest.

This matches how the rest of the system already behaves. The flywheel batches
by (file, principle page), so those occurrences would be fixed together
anyway; a waiver against them reads naturally as "accept this rule's hits in
this file"; and there is no ordinal to drift when one of them is fixed.

The alternatives both fell short: an occurrence ordinal in the hash keeps one
finding per site but renumbers survivors when an earlier one is fixed, so a
waiver held against a later occurrence silently drifts onto the wrong target.
Adding the enclosing entity to the hash narrows collisions without closing
them — two identical lines in the same function still collide.

## Consequences

- **The stalemate rail compares `(fingerprint, occurrence count)`**, not
  fingerprints alone. Otherwise fixing two of three occurrences would leave
  the set unchanged and falsely read as a stalemate. See design.md.
- **Containment subordination uses the primary occurrence.** A group whose
  occurrences straddle a structural finding's boundary subordinates on where
  its first one falls, rather than splitting the group.
- **Touched-entity scope filters occurrences first**, then keeps the group if
  any survive — so a brief never points the agent at occurrences outside the
  diff.
- Renames and file moves re-key fingerprints, so findings in a moved file read
  as new. Accepted for v1; content-tracking machinery would be
  disproportionate to the payoff.
