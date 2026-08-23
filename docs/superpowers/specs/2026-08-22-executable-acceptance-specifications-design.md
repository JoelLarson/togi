# Executable Acceptance Specifications Design

## Purpose

togi needs a readable, executable account of its user-level behavior. The
existing package tests are effective unit and integration tests, but their
organization follows implementation packages rather than user stories. A
reader cannot use them as a concise answer to "what does togi do?"

This design adds an acceptance-specification suite above the package tests.
Each feature describes a user story in Gherkin and exercises integrations
between application modules through a replaceable driver. The feature files
become the primary catalog of application behavior; package tests remain the
fast tool for building toward those behaviors and exhaustively covering edge
cases.

The same feature files run through two test-only drivers. The default service
driver targets application logic directly. An opt-in compiled-CLI driver
exercises the public process boundary without changing the specifications.
Cobra argument parsing is covered through that compiled boundary rather than
through a separate in-process driver.

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

The acceptance suite registers a Go test flag with the values `service`,
`cli`, and `all`. Omitting the flag selects `service`, so the normal repository
test command runs only the application-level suite:

```sh
go test ./...
go test ./features/... -v -args -acceptance.driver=cli
go test ./features/... -v -args -acceptance.driver=all
```

Selecting `cli` runs only the compiled-CLI driver. For each feature test,
selecting `all` runs the service driver first and the CLI driver second. An
unknown or unavailable driver is an error, never a skip or an empty successful
matrix. Because package-specific flags cannot safely be passed through
`go test ./...`, the explicit driver forms target `./features/...`
directly.

Every example runs through every selected driver except an example that
requires a test-only capability absent from the production boundary. The only
initial exception is simulated platform selection: those examples carry an
`@simulated-platform` tag, run through the service driver, and are excluded
from the CLI driver's Godog tag expression. The CLI driver still runs the
real-host platform example. The driver registry declares this matrix, and a
static matrix test fails if an example is excluded without a declared
capability. After host eligibility and driver tags are applied,
`FeatureOptions` fails if an instantiated feature has no eligible examples.
Packages or feature tests omitted by a user's `-run` filter never instantiate
a suite, and a host-ineligible group skips before this validation.

The scenarios require no installed gate tools or external network service.
Real Git repositories and remotes live under test-owned temporary directories.
Fake gate executables and XDG config/state/cache roots are isolated per
scenario. Verification runs in a network-denied environment after Go module
dependencies are available, so an accidental scenario dependency cannot pass.
Scenarios that execute the Phase 1/2 runtime run only on Linux. Other hosts
still compile the suite and execute the unsupported-platform specification;
Linux-only feature groups skip with an explicit reason. Host eligibility is a
suite precondition independent of the selected driver's capability tags.

The shared harness is an ordinary Go package under
`features/internal/harness` so separate acceptance-domain packages can
import it. Consequently `go build ./...` compiles the harness, but
`go build ./cmd/togi` does not include it in the production binary because
production code never imports it. Feature-specific state and bindings remain
in `*_test.go`. The CLI driver builds the production binary only when that
driver is selected.

## Suite Layout

The acceptance tree lives beside the production `internal/` tree because it
tests the assembled application rather than one production package boundary.
Its own nested `internal/harness` is shared acceptance infrastructure:

```text
features/
├── README.md
├── internal/
│   └── harness/
│       ├── main.go
│       ├── selector.go
│       ├── driver.go
│       ├── service_driver.go
│       ├── cli_driver.go
│       ├── observation.go
│       ├── repository.go
│       └── gate_tool.go
├── gauntlet/
│   ├── main_test.go
│   ├── running_the_gauntlet.feature
│   ├── running_the_gauntlet_test.go
│   ├── judging_a_feature_diff.feature
│   ├── judging_a_feature_diff_test.go
│   └── steps_test.go
├── gate/
│   ├── main_test.go
│   ├── customizing_gates.feature
│   ├── customizing_gates_test.go
│   └── steps_test.go
├── runledger/
│   ├── main_test.go
│   ├── keeping_run_history.feature
│   ├── keeping_run_history_test.go
│   └── steps_test.go
├── wiki/
│   ├── main_test.go
│   ├── using_principle_pages.feature
│   ├── using_principle_pages_test.go
│   └── steps_test.go
└── platform/
    ├── main_test.go
    ├── supporting_platforms.feature
    ├── supporting_platforms_test.go
    └── steps_test.go
```

`features/README.md` is the mandatory human entry point. It explains the
purpose of the suite, gives a short capability index and reading order, and
shows the service, CLI, and all-driver commands. A reader follows the index
into a domain directory and opens its `.feature` files before any Go code. A
harness catalog test keeps that entry point complete. The index lives between
`<!-- feature-index:start -->` and `<!-- feature-index:end -->` markers and
contains one list item per line in the exact form
`- [Title](domain/name.feature)`. The test recursively discovers
`features/<domain>/*.feature`, excluding `internal` and `testdata`, normalizes
paths to slash-separated paths relative to `features/`, and requires the
discovered paths and indexed paths to match exactly once. It also requires
each feature to have an adjacent `_test.go` file with the same stem.

Each domain directory is an independent Go test package. Every `.feature` is
co-located with a thin matching `_test.go` file that owns the Godog entry
point, scenario-local state, and feature-specific actions. The domain's
`steps_test.go` owns vocabulary shared by features in that domain. Test files
declare the package name matching their directory, such as `package gauntlet`;
there is no empty production package to test externally.

`features/internal/harness` owns driver selection, domain driver ports,
service and CLI implementations, raw observations, repository fixtures, fake
gate tools, and cross-domain step primitives. It contains no story text. The
`internal` placement prevents production packages outside `features/` from
importing it.

These acceptance packages describe tests rather than application modules.
They do not alter the glossary-owned production layout in ADR-0012. Domain
directory names nevertheless use existing vocabulary where possible so a
reader can move between `CONTEXT.md`, production packages, and specifications
without translation.

## Godog Structure

Each feature gets its own `godog.TestSuite` pointed at the adjacent feature
file. This differs from the smallest official examples, which run a whole
directory through one initializer, but uses the same public API and permits
focused execution. Each suite composes cross-domain harness steps, its domain
steps, and its feature actions without registering unrelated vocabulary.

Cross-domain step expressions are declared once in the harness; expressions
shared only inside one domain are declared once in that domain's step set.
Feature initializers select those entries rather than redeclaring them. When
an authored step needs the same meaning in a second domain, its binding moves
to the harness instead of being copied. Strict mode still detects ambiguity
inside every assembled feature registry. Synonymous wording across domains is
a review concern; the design does not claim that a mechanical check can prove
semantic equivalence.

The suite uses Godog's documented feature-object pattern. The
`ScenarioInitializer` creates a new feature state object for every scenario,
then that object binds feature actions and the required domain step sets with
`Given`, `When`, and `Then`.

Conceptually:

```go
func TestRunningTheGauntlet(t *testing.T) {
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		suite := godog.TestSuite{
			Name: "running the gauntlet",
			ScenarioInitializer: func(sc *godog.ScenarioContext) {
				feature := newGauntletFeature(factory)
				feature.InitializeScenario(sc)
			},
			Options: harness.FeatureOptions(t, "running_the_gauntlet.feature"),
		}
		harness.RequireGodogSuccess(t, suite.Run())
	})
}

func (feature *gauntletFeature) InitializeScenario(sc *godog.ScenarioContext) {
	sc.Before(feature.startScenario)
	sc.After(feature.finishScenario)
	feature.repositories.Bind(sc)
	feature.gates.Bind(sc)
	feature.reports.Bind(sc)
	sc.When(`^I run the gauntlet$`, feature.iRunTheGauntlet)
}
```

`harness.FeatureOptions` sets `Strict: true`, `TestingT: t`, `Concurrency: 1`,
`Randomize: 0`, and exactly one feature path. Strict mode makes undefined,
pending, and ambiguous steps fail the suite. A harness regression test proves
that undefined, pending, and ambiguous steps each produce a nonzero result.

`Before` creates the isolated driver and scenario resources. `After` closes
the driver and removes any resources not already owned by `testing.T`.
Hooks perform isolation and cleanup only. Every repository shape, gate
behavior, platform choice, or other condition that causes the expected result
appears in a visible `Given` step. Scenarios run serially initially.

## Gherkin Authoring Rules

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
are preferred where they make an example easier to follow. The feature
catalog below is scope: before implementation is complete, every listed
user-visible behavior maps to a named example or `Rule:` in its feature file.
This traceability does not pull exhaustive tool-rule or language-parser
matrices into acceptance; those remain with the owning package or future
language module.

## Driver Boundary

Gherkin and step definitions speak in application concepts: repository, gate,
finding, report, verdict, run history, and principle page. Driver code alone
knows whether those actions are service calls or process invocations.

`DriverFactory` creates one isolated set of domain drivers per scenario. The
port is split by capability rather than forming one broad interface:

- `GauntletDriver` runs the gauntlet on a repository with explicit options;
- `HistoryDriver` invokes status for a repository;
- `WikiDriver` shows, lints, and ejects principle pages;
- `Close` releases scenario-owned resources.

Platform simulation and fixture construction belong to the scenario
environment, not these user-action ports. A feature depends only on the
capabilities it exercises. Capability tags describe the required test seam,
not the production implementation or invocation mechanism.

Drivers return raw test-side observations rather than calling `testing.T`.
`RunObservation` preserves stdout, stderr, the typed service error or process
exit status, persisted report bytes, and artifact locations. Drivers do not
return a decoded verdict, finding set, gate status, or classified outcome.

A shared acceptance observation layer decodes documented artifacts and
classifies the driver-specific exit source for `Then` steps. Its constructors
retain provenance, so a report value can only come from report bytes and an
outcome can only come from the typed error or process status. It cannot infer
an errored report from exit code 4, infer an exit category from a report
verdict, or substitute the service's returned report for persisted bytes in a
persistence assertion. Focused harness tests feed contradictory observations,
such as exit 4 with no report, and prove the channels remain independent.
This keeps assertions in `Then` steps without making each driver a second
implementation of togi's semantics.

The service driver assembles `run.Service`, `wiki.Service`, gate
loaders, enrichers, XDG paths, and deterministic clocks/randomness. It is the
only driver allowed to import those concrete application services. The CLI
driver builds the production binary once per selected domain test process,
invokes it in each scenario's repository with isolated environment and
streams, and observes its exit status and external artifacts. Go's build cache
limits the repeated work across domain packages; sharing one binary across
test processes is deferred until measurement justifies the coordination.
Both drivers implement the same domain ports and run the same feature files.

Each domain's `main_test.go` delegates `TestMain` to `harness.Main`. The
harness parses the driver flag, walks upward from the domain package working
directory to the module `go.mod`, and, when CLI execution is selected, builds
the binary into a process-owned temporary directory. It makes that path
available to every scenario in the domain and removes the directory after
`m.Run` returns and before `harness.Main` returns its exit code. Service-only
runs do not build the binary.

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
    -> domain driver application action
    -> selected driver invokes a service or the compiled CLI
    -> RunObservation captures raw output, outcome source, and artifacts
Gherkin Then steps
    -> shared observation layer decodes artifacts for assertions
After hook
    -> driver/resource cleanup
```

Expected togi outcomes are data, not harness failures. For example,
`run.Service.Run` returns both a report and a typed exit error when findings
remain or a gate is errored, while the CLI exposes the corresponding report
artifact and process status. The selected driver records those independent
channels in `RunObservation`; the When step succeeds, and Then steps decode and
assert the report, verdict, and outcome. Setup failures, unreadable fixtures,
or an inability to call the application remain step errors and fail the
scenario immediately.

Step functions return descriptive errors rather than calling `t.Fatal`.
Failure messages name the expected business outcome and the observed report.
Raw gate output may be inspected only through persisted-artifact assertions;
it never enters the structured finding or rendered-report model.

## Feature Catalog

### Running the gauntlet

As a developer evaluating a feature, I want to run independent quality
gates so that I receive a complete, trustworthy quality report.

Scenarios cover shipped and selected gates, normalized findings, occurrence
grouping, stable fingerprints across unchanged runs, gate ordering independent
of completion order, compiler-style rendering without raw tool output, and
verdict/error classification. Infrastructure-failure examples cover missing
tools, crashes, timeouts, and malformed output while proving a healthy
sibling's findings survive. Version mismatches remain advisory.

### Judging a feature diff

As a developer evaluating committed work, I want to judge findings against
the feature diff so that unrelated repository findings do not obscure my
change.

Scenarios cover explicit and automatically discovered bases, merge-base scope
across diverged history, point versus entity findings, whole-repo scoped
gates, deletions, renames, and binary changes. Preconditions cover dirty
worktrees, unsupported submodules, missing or invalid bases, and unrelated
histories, all before ledger creation or gate execution.

### Keeping run history

As a developer returning to a repository, I want to inspect durable run
history so that results survive checkouts without modifying the target
repository.

Scenarios cover external report and raw-output persistence, absence of togi
files in the target, state shared across linked worktrees, one active run per
repository, recovery from an abandoned lock, pruning, and selection of the
latest complete valid report through status while incomplete or corrupt runs
are ignored.

### Customizing gates

As an operator evolving personal standards, I want to customize gates
outside the target repository so that my gauntlet changes without imposing
repository files on collaborators.

Scenarios cover shipped definitions loading and normalizing representative
recorded output, wholesale XDG overrides, additional override-only gates, and
rejection of invalid definitions before tools run.

### Using principle pages

As a developer understanding a finding, I want to inspect and customize
principle pages so that tool-specific rules lead to stable engineering
guidance.

Scenarios cover shipped and overridden pages, deterministic alias display,
dangling-alias warnings, conflicting-alias failures, and ejection without
overwriting an existing operator copy.

### Supporting platforms

As an operator running togi, I want an explicit platform result so that an
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

Language-specific normalizer fixtures and tool-rule matrices remain below the
acceptance boundary even if languages later become their own modules. The
acceptance suite keeps one representative language example wherever language
changes do not alter the user-visible contract.

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
This proves togi's behavior against representative tool contracts, not that a
new external-tool release remains compatible. Any real-tool compatibility
check stays tagged and outside the default acceptance run.

The suite begins with Godog concurrency set to one. This favors deterministic
output and makes filesystem ownership obvious. Later parallelism additionally
requires command-specific environment injection and forbids process-global
working-directory or environment mutation; scenario-local objects alone are
not sufficient.

## Verification

Implementation is complete when:

```sh
go test ./features/... -v
go test ./features/... -v -args -acceptance.driver=cli
go test ./features/... -v -args -acceptance.driver=all
go test ./...
go build ./...
```

pass in a network-denied environment, after module dependencies are available,
and without installed gate tools. The default commands execute the service
driver only; CLI and all-driver verification are explicit. A domain package,
feature test, or Godog scenario can be selected independently with normal Go
test path and `-run` filtering while using the same driver flag. On Linux,
required scenarios may not be skipped. Strict mode rejects pending, undefined,
and ambiguous steps. The existing package suite continues to pass unchanged
except for deliberate test-infrastructure reuse or exact duplicate removal
approved after adoption.
