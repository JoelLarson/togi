# Go, distributed as a single static binary with a subcommand CLI

togi ultimately has to run wherever a target repo is checked out, without
adding a runtime, a virtualenv, or a package manager to that machine. Go gives
a single static binary that drops onto PATH, plus first-class subprocess and
concurrency handling — which is most of what togi does: fan out tool
executions, collect their output, shell out to agent CLIs. Phase 1 validates
that architecture on Linux only; other platform backends remain buildable
extension points until their locking and process-lifecycle semantics are
implemented and tested.

The CLI is subcommand-shaped (`togi run`, `togi status`, …) rather than a
single-purpose command, because v1's gauntlet is one stage of an eventual
spec → gather → build → verify pipeline. Adding stages later must not mean
restructuring the command surface.

## Considered options

- **Rust** — equally deployable, and a first-class target language, but Go's
  subprocess/concurrency ergonomics fit an orchestrator better and the
  build/iteration loop is faster.
- **Python/Node** — fastest to prototype, but every target machine would need
  a matching runtime, and "install this tool globally" becomes a dependency
  negotiation. Disqualified by the same instinct as ADR-0002.
