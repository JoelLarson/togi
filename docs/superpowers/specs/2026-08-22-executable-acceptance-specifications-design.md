# Executable Acceptance Specifications Design

## Purpose

togi needs a readable, executable account of its user-level behavior. The
existing package tests are effective unit and integration tests, but their
organization follows implementation packages rather than user stories. A
reader cannot use them as a concise answer to "what does togi do?"

This design adds an acceptance-specification suite above the package tests.
Each feature describes a user story in Gherkin and exercises integrations
between application modules through a replaceable driver. The specifications
become the primary behavioral catalog; package tests remain the fast tool for
building toward those behaviors and exhaustively covering edge cases.

The initial suite targets application logic directly. It does not test Cobra
argument parsing or the compiled process boundary. Those drivers can be added
later and run the same feature files without changing the specifications.

## Dependency and Execution Model

Use `github.com/cucumber/godog` v0.16.0 as a test dependency. Godog is the
official Cucumber implementation for Go, runs under `go test`, exposes
scenarios as Go subtests, and supports selecting individual feature paths.
Its `ScenarioInitializer` is the documented registry between Gherkin steps and
Go functions.

The dependency exception is deliberate. Human-readable feature files are the
goal of this work, and maintaining a local Gherkin runner would be more code,
less capable, and less familiar than accepting a focused test dependency.
Godog remains pre-1.0, so the version is pinned and upgrades are explicit.

The acceptance suite runs as part of:

```sh
go test ./...
```

It requires no installed gate tools and performs no network access. Real Git
repositories live under test-owned temporary directories. Fake gate
executables and XDG config/state/cache roots are isolated per scenario.
Scenarios that execute the Phase 1/2 runtime run only on Linux. Other hosts
still compile the suite and execute the unsupported-platform specification;
Linux-only feature groups skip with an explicit reason.

## Suite Layout

The suite lives outside `internal/` because it tests the assembled
application, not a production package boundary:

```text
acceptance/
├── features/
│   ├── running_the_gauntlet.feature
│   ├── judging_a_feature_diff.feature
│   ├── keeping_run_history.feature
│   ├── customizing_gates.feature
│   ├── using_principle_pages.feature
│   └── supporting_platforms.feature
├── running_the_gauntlet_test.go
├── judging_a_feature_diff_test.go
├── keeping_run_history_test.go
├── customizing_gates_test.go
├── using_principle_pages_test.go
├── supporting_platforms_test.go
├── driver_test.go
├── service_driver_test.go
├── repository_test.go
└── gate_tool_test.go
```

Each `.feature` file is the primary human-readable specification for one
cohesive capability. Its corresponding `_test.go` file owns that feature's
scenario state, step bindings, and step implementations. Shared files contain
only the driver port and test infrastructure; they do not contain story text.

This test-only `acceptance` package does not introduce an application module
or alter the glossary-owned package layout in ADR-0012.

## Godog Structure

Each feature group gets its own `godog.TestSuite` pointed at exactly one
feature path. This differs from the smallest official examples, which run a
whole `features/` directory through one initializer, but uses the same public
API and keeps unrelated step vocabularies from forming one global registry.

The suite uses Godog's documented feature-object pattern. The
`ScenarioInitializer` creates a new feature state object for every scenario,
then that object binds its methods directly with `Given`, `When`, and `Then`.
There is no separate `registerReportingSteps` layer.

Conceptually:

```go
func TestRunningTheGauntlet(t *testing.T) {
	forEachDriver(t, func(t *testing.T, factory DriverFactory) {
		suite := godog.TestSuite{
			Name: "running the gauntlet",
			ScenarioInitializer: func(sc *godog.ScenarioContext) {
				feature := &gauntletFeature{factory: factory}
				feature.InitializeScenario(sc)
			},
			Options: featureOptions(t, "features/running_the_gauntlet.feature"),
		}
		requireGodogSuccess(t, suite.Run())
	})
}

func (feature *gauntletFeature) InitializeScenario(sc *godog.ScenarioContext) {
	sc.Before(feature.startScenario)
	sc.After(feature.finishScenario)
	sc.Given(`^a committed Go repository$`, feature.aCommittedGoRepository)
	sc.When(`^I run the gauntlet$`, feature.iRunTheGauntlet)
	sc.Then(`^the complexity gate is errored$`, feature.theComplexityGateIsErrored)
}
```

`Before` creates the isolated driver and scenario resources. `After` closes
the driver and removes any resources not already owned by `testing.T`.
Scenarios run serially initially. The state lifecycle is nevertheless
scenario-local, so later parallelism does not require redesigning the step
definitions.

## Driver Boundary

Gherkin and step definitions speak only in application concepts: repository,
gate, finding, report, verdict, run history, and principle page. They do not
mention Cobra commands, concrete service construction, or subprocess output.

`DriverFactory` creates one isolated driver per scenario. `TogiDriver` exposes
the application actions required by the features:

- run the gauntlet on a repository with explicit options;
- read the latest completed run;
- show, lint, and eject principle pages;
- select a simulated runtime platform where the platform story requires it;
- close scenario-owned resources.

The driver returns test-side result values rather than calling `testing.T`.
Results expose reports, gate statuses, findings, verdicts, persisted artifact
locations, rendered output, and classified application errors. This keeps
assertions in Then steps and permits future drivers to translate Cobra output
or a compiled process back into the same result model.

The initial service driver assembles `run.Service`, `wiki.Service`, gate
loaders, enrichers, XDG paths, and deterministic clocks/randomness. It is the
only acceptance code allowed to import those concrete application services.
Future Cobra and compiled-CLI drivers implement the same test-side port.

Repository setup is separate test infrastructure rather than part of the
driver. It creates real repositories, commits, branches, linked worktrees,
remotes, dirty states, renames, deletions, binary files, and submodules. Gate
fixtures create fake executable tools and external gate definitions without
writing to the target repository.

## Scenario Data Flow

Each scenario follows the same lifecycle:

```text
Gherkin Given steps
    -> real temporary Git repository + isolated XDG roots + fake gates
Gherkin When step
    -> TogiDriver application action
    -> service driver invokes assembled application services
    -> structured acceptance result captures report, artifacts, and error
Gherkin Then steps
    -> assertions over user-observable behavior
After hook
    -> driver/resource cleanup
```

Expected togi outcomes are data, not harness failures. For example,
`run.Service.Run` returns both a report and a typed exit error when findings
remain or a gate is errored. The service driver records both in `RunResult`,
the When step succeeds, and Then steps assert the report, verdict, and error
classification. Setup failures, unreadable fixtures, or an inability to call
the application remain step errors and fail the scenario immediately.

Step functions return descriptive errors rather than calling `t.Fatal`.
Failure messages name the expected business outcome and the observed report.
Raw gate output may be inspected only through persisted-artifact assertions;
it never enters the structured finding or rendered-report model.

## Feature Catalog

### Running the gauntlet

As a developer evaluating a feature, I can run independent quality gates so
that I receive a complete, trustworthy quality report.

Scenarios cover shipped and selected gates, normalized findings, occurrence
grouping, stable fingerprints across unchanged runs, gate ordering independent
of completion order, compiler-style rendering without raw tool output, and
verdict/error classification. Infrastructure-failure examples cover missing
tools, crashes, timeouts, and malformed output while proving a healthy
sibling's findings survive. Version mismatches remain advisory.

### Judging a feature diff

As a developer evaluating committed work, I can judge findings against the
feature diff so that unrelated repository findings do not obscure my change.

Scenarios cover explicit and automatically discovered bases, merge-base scope
across diverged history, point versus touched-entity findings, repo-scoped
gates, deletions, renames, and binary changes. Preconditions cover dirty
worktrees, unsupported submodules, missing or invalid bases, and unrelated
histories, all before ledger creation or gate execution.

### Keeping run history

As a developer returning to a repository, I can inspect durable run history
so that results survive checkouts without modifying the target repository.

Scenarios cover external report and raw-output persistence, absence of togi
files in the target, state shared across linked worktrees, one active run per
repository, recovery from an abandoned lock, pruning, and selection of the
latest complete valid report through status while incomplete or corrupt runs
are ignored.

### Customizing gates

As an operator evolving personal standards, I can customize gates outside the
target repository so that my gauntlet changes without imposing repository
files on collaborators.

Scenarios cover working shipped defaults, wholesale XDG overrides, additional
override-only gates, and rejection of invalid definitions before tools run.

### Using principle pages

As a developer understanding a finding, I can inspect and customize principle
pages so that tool-specific rules lead to stable engineering guidance.

Scenarios cover shipped and overridden pages, deterministic alias display,
dangling-alias warnings, conflicting-alias failures, and ejection without
overwriting an existing operator copy.

### Supporting platforms

As an operator running togi, I receive an explicit platform result so that an
unsupported machine cannot appear to have passed its gates.

Scenarios cover Linux service execution and rejection of other simulated
platforms before repository resolution, gate startup, or ledger access.

## Acceptance and Unit-Test Boundary

Acceptance scenarios establish user-meaningful guarantees and integrations.
They use one representative example for a behavior unless distinct examples
change the user-visible contract. Existing package tests remain in place
during adoption, even where some coverage overlaps.

Package tests continue to own:

- malformed TOML, template, normalizer, and finding validation matrices;
- exact parser fixtures and source-line boundary cases;
- buffer-size, timing, sorting, and clock edge cases;
- Git environment, attribute, and conversion-filter hardening matrices;
- symlink, path traversal, filesystem replacement, and handle-anchoring
  permutations;
- cancellation checkpoints and process-tree implementation details;
- platform-specific build and lock mechanics.

A security or failure behavior appears once in the acceptance suite when it
defines a user-facing guarantee. Exhaustive attack and error permutations stay
in the owning unit tests. After the acceptance suite has proven stable, exact
story-level duplicates may be removed deliberately; initial implementation
does not delete existing tests.

## Determinism and Performance

The service driver injects clocks and randomness where the application
already provides seams. Scenarios compare semantic report content and stable
fingerprints rather than run IDs or wall-clock durations unless those values
are the behavior under test.

All Git behavior uses real local repositories. Gate behavior uses fake tools
whose stdout, stderr, exit status, delay, and version output are controlled by
the scenario. Tests never require golangci-lint or gocyclo to be installed.

The suite begins with Godog concurrency set to one. This favors deterministic
output and makes filesystem ownership obvious. The driver boundary and
per-scenario initializer preserve the option to parallelize later after
measuring suite cost.

## Verification

Implementation is complete when:

```sh
go test ./acceptance -v
go test ./...
go build ./...
```

pass without network access or installed gate tools. Each feature can be run
independently through its Go test, and Godog exposes individual scenarios as
subtests for focused execution. The existing package suite continues to pass
unchanged except for deliberate test-infrastructure reuse or exact duplicate
removal approved after adoption.
