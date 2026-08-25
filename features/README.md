# Executable acceptance specifications

This directory is the human-readable catalog of togi's user-level behavior.
Start with the feature links below. Open the adjacent `_test.go` only when you
need to see how application language maps to a driver action.

<!-- feature-index:start -->
- [Running the gauntlet](gauntlet/running_the_gauntlet.feature)
- [Judging a feature diff](gauntlet/judging_a_feature_diff.feature)
- [Fixing a feature diff safely](gauntlet/fixing_a_feature_diff.feature)
- [Keeping run history](runledger/keeping_run_history.feature)
- [Customizing gates](gate/customizing_gates.feature)
- [Using principle pages](wiki/using_principle_pages.feature)
- [Customizing principle pages](wiki/customizing_principle_pages.feature)
- [Supporting platforms](platform/supporting_platforms.feature)
<!-- feature-index:end -->

Read **Running the gauntlet** first for the core report contract, then
**Judging a feature diff** for scope and **Fixing a feature diff safely** for
the guarded fix loop. The remaining features can be read by domain: history,
gate customization, principle pages, and platform support.

The default command runs the application service driver:

```sh
go test ./features/... -v
```

Use the compiled process boundary explicitly:

```sh
go test ./features/... -v -args -acceptance.driver=cli
go test ./features/... -v -args -acceptance.driver=all
```

## Authoring rules

Feature text is declarative application language, not a script of driver
operations. Each feature names the actor, capability, and benefit. Examples
normally use three to five steps, contain one event, and illustrate one
distinct user-visible contract. Causally relevant setup remains visible in
`Given`; `Then` steps assert documented output, errors, or persisted artifacts
observable at the selected driver boundary.

Gherkin's optional `Rule:` keyword groups examples under one business
invariant, such as `Rule: An errored gate never suppresses healthy findings`.
It is a Gherkin document heading, not a togi gate rule or finding `rule_id`.
Use it when it makes a feature easier to scan; do not add it mechanically.

Wording should survive a change of driver or implementation. Feature text
therefore does not mention Cobra, Go service construction, fake tools,
subprocess stdout, JSON decoding, or test helpers. Concrete names and values
are preferred where they make an example easier to follow.

The feature index above is scope: every listed user-visible behavior maps to a
named example or `Rule:` in its feature file. This traceability does not pull
exhaustive tool-rule or language-parser matrices into acceptance; those remain
with the owning package or future language module.
