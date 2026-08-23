# Project State Housekeeping Design

## Purpose

The repository has moved beyond the phase described by `AGENTS.md`. Phases 1
and 2 are implemented, the executable acceptance-specification detour is
complete, and selected Phase 4 wiki mechanics landed early. The documentation
must give engineers and agents an accurate starting point without rewriting
historical implementation records.

## Approach

Use a status overlay and preserve history:

- Rewrite `AGENTS.md` as the operational handoff for Phase 3. It will summarize
  implemented behavior, identify the next boundary, retain the settled
  non-negotiables, and include the acceptance-driver verification commands.
- Add a concise implementation-status section to `docs/roadmap.md`. The phase
  definitions and exit criteria remain unchanged; the overlay identifies
  Phases 1 and 2 as complete, Phase 3 as next, and the Phase 4 wiki mechanics
  as partially implemented ahead of sequence.
- Add prominent completion notices to finished implementation plans. Their
  unchecked task lists remain unchanged because they record the prescribed
  execution sequence, including temporary failing-test states, rather than
  serving as the current project tracker.
- Correct present-tense or forward-looking claims in `docs/implementation.md`
  only where they now misdescribe implemented behavior. Historical rationale
  remains intact.

No production behavior, glossary vocabulary, ADR, roadmap requirement, or
phase boundary changes as part of this work.

## Verification

The housekeeping is complete when:

- repository-wide searches find no claim that the project contains only
  documentation or that Phase 1 is the current task;
- every completed plan clearly says it is a historical, completed record;
- links and paths named by the edited documents exist;
- `go build ./...` and `go test ./...` pass; and
- the service, CLI, and combined acceptance-driver commands remain documented
  in the operational handoff.
