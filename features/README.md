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
