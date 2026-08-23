# Executable Acceptance Specifications Implementation Plan

> **Status: Complete.** The service and compiled-CLI acceptance catalog is
> implemented under `features/`. This plan remains as a historical execution
> record; unchecked boxes preserve the prescribed sequence and do not identify
> outstanding work.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a human-first Gherkin catalog that executes togi's user-visible behavior through the service boundary by default and the compiled CLI boundary on demand.

**Architecture:** Independent acceptance-domain test packages co-locate each feature with its Godog entry point. `features/internal/harness` owns isolated scenario fixtures, raw observations, domain driver ports, the service and CLI drivers, strict suite options, and driver selection; `features/README.md` is the human entry point and mechanically indexes every feature.

**Tech Stack:** Go 1.25, `github.com/cucumber/godog` v0.16.0, Cobra through the compiled CLI, standard-library Git/process/filesystem fixtures, and the existing togi services.

**Design:** `docs/superpowers/specs/2026-08-22-executable-acceptance-specifications-design.md`

---

## Execution Preconditions

- Execute from a clean worktree containing commits `f8d1431` and `a5e883f`.
- Read `CONTEXT.md`, ADR-0002, ADR-0004, ADR-0005, ADR-0006, ADR-0008,
  ADR-0011, ADR-0012, and the approved design before changing tests.
- Do not delete or weaken existing package tests. Acceptance scenarios add a
  user-story view; package tests retain exhaustive edge and security coverage.
- Do not install golangci-lint or gocyclo. Every gate process in this suite is
  controlled by `features/internal/harness`.
- Run Linux-only domain packages on Linux. On other hosts they must compile
  and skip with the reason specified in Task 7.

## File Map

### Human entry point

- `features/README.md`: purpose, reading order, feature index, and driver
  commands.

### Shared acceptance harness

- `features/internal/harness/main.go`: module-root discovery, `TestMain`
  lifecycle, and one CLI build per domain test process.
- `features/internal/harness/selector.go`: `-acceptance.driver`, selected
  factory order, driver capability tags, and host eligibility.
- `features/internal/harness/driver.go`: scenario environment, domain driver
  ports, requests, and factory contract.
- `features/internal/harness/observation.go`: raw service/process
  observations, persisted-artifact decoding, and provenance-preserving outcome
  classification.
- `features/internal/harness/service_driver.go`: assembly of `run.Service`
  and `wiki.Service` with deterministic seams.
- `features/internal/harness/cli_driver.go`: compiled-process invocation with
  isolated streams, working directory, and environment.
- `features/internal/harness/repository.go`: error-returning, hermetic real
  Git repository builder.
- `features/internal/harness/gate_tool.go`: scenario-owned executable gate
  doubles and external TOML definitions.
- `features/internal/harness/steps.go`: step primitives whose exact meaning
  is shared by more than one acceptance domain.
- `features/internal/harness/*_test.go`: focused harness contracts, catalog
  enforcement, and driver conformance.
- `features/internal/harness/testdata/*.feature`: deliberately invalid
  strict-mode fixtures, excluded from the human catalog.

### Acceptance domains

- `features/gauntlet`: running gates and judging a feature diff.
- `features/gate`: XDG gate customization.
- `features/runledger`: durable history and status selection.
- `features/wiki`: principle-page inspection and customization.
- `features/platform`: supported and unsupported platform behavior.

Each domain has `main_test.go`, one `.feature`/matching `_test.go` pair per
feature, and `steps_test.go`. Test files use the package name matching the
directory, not an external `_test` package.

## Harness Contracts

The tasks below use these names consistently:

```go
type DriverFactory interface {
	Name() string
	CapabilityTags() string
	NewGauntlet(*Environment) (GauntletDriver, error)
	NewHistory(*Environment) (HistoryDriver, error)
	NewWiki(*Environment) (WikiDriver, error)
}

type GauntletDriver interface {
	Run(context.Context, RunRequest) (RunObservation, error)
	Close() error
}

type HistoryDriver interface {
	Status(context.Context, StatusRequest) (CommandObservation, error)
	Close() error
}

type WikiDriver interface {
	Show(context.Context, string) (CommandObservation, error)
	Lint(context.Context) (CommandObservation, error)
	Eject(context.Context, string) (CommandObservation, error)
	Close() error
}

type RunRequest struct {
	Root      string
	Base      string
	GateNames []string
	Verbose   bool
	NoColor   bool
}

type StatusRequest struct {
	Root    string
	NoColor bool
}
```

`DriverFactory` is the shared creation boundary; user actions remain split
into the three domain ports. An unsupported factory method returns
`ErrUnsupportedCapability`. Feature state requests only its own domain port.

Cross-domain bindings share one scenario-local `World`; it is test state, not
another application driver:

```go
type Capabilities uint8

const (
	NeedsGauntlet Capabilities = 1 << iota
	NeedsHistory
	NeedsWiki
)

type World struct {
	factory      DriverFactory
	capabilities Capabilities
	environment  *Environment
	repository   *Repository
	gauntlet     GauntletDriver
	history      HistoryDriver
	wiki         WikiDriver
	lastRun      RunObservation
	lastCommand  CommandObservation
}

func NewWorld(DriverFactory, Capabilities) *World
func (w *World) Before(context.Context, *godog.Scenario) (context.Context, error)
func (w *World) After(context.Context, *godog.Scenario, error) (context.Context, error)
func (w *World) BindRepositories(*godog.ScenarioContext)
func (w *World) BindGates(*godog.ScenarioContext)
func (w *World) BindReports(*godog.ScenarioContext)
func (w *World) Environment() *Environment
func (w *World) Repository() *Repository
func (w *World) UseRepository(*Repository) error
func (w *World) Gauntlet() (GauntletDriver, error)
func (w *World) History() (HistoryDriver, error)
func (w *World) Wiki() (WikiDriver, error)
func (w *World) Run(context.Context, RunRequest) error
func (w *World) Status(context.Context, StatusRequest) error
func (w *World) Show(context.Context, string) error
func (w *World) Lint(context.Context) error
func (w *World) Eject(context.Context, string) error
func (w *World) LastRun() RunObservation
func (w *World) LastCommand() CommandObservation
```

`Before` creates and activates one `Environment`, then constructs only the
requested domain ports. `After` releases a paused gate, closes constructed
ports, restores the environment, and removes scenario resources, joining all
cleanup errors. Domain feature state embeds the world and adds only the
actions/assertions unique to that story.

### Task 1: Pin Godog and Establish the Human Catalog

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `features/README.md`
- Create: `features/internal/harness/main.go`
- Create: `features/internal/harness/catalog_test.go`

- [ ] **Step 1: Write the failing catalog test**

Create `TestFeatureIndex` in `catalog_test.go`. It must locate the module by
walking upward to `go.mod`, read `features/README.md`, and enforce all of
these conditions in one test:

```go
const (
	indexStart = "<!-- feature-index:start -->"
	indexEnd   = "<!-- feature-index:end -->"
)

var indexLine = regexp.MustCompile(`^- \[[^]]+\]\(([^)]+\.feature)\)$`)
```

- exactly one start marker and one end marker, in that order;
- every nonblank line between them matches `indexLine`;
- recursively discover `features/<domain>/*.feature` while excluding path
  components named `internal` or `testdata`;
- compare slash-normalized paths relative to `features/` as sets with no
  duplicates on either side;
- require `name_test.go` beside every `name.feature`.

- [ ] **Step 2: Verify the test is red**

Run: `go test ./features/internal/harness -run TestFeatureIndex -v`

Expected: FAIL because `features/README.md` does not exist.

- [ ] **Step 3: Add the module-root helper and initial README**

Add this production-neutral harness helper to `main.go`:

```go
func findModuleRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(filepath.Join(current, "go.mod"))
		switch {
		case statErr == nil && !info.IsDir():
			return current, nil
		case statErr == nil:
			return "", errors.New("go.mod is not a file")
		case !errors.Is(statErr, os.ErrNotExist):
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("go.mod not found")
		}
		current = parent
	}
}
```

Create `features/README.md` with this initial body. Later domain tasks add
links between the markers.

~~~markdown
# Executable acceptance specifications

This directory is the human-readable catalog of togi's user-level behavior.
Start with the feature links below. Open the adjacent `_test.go` only when you
need to see how application language maps to a driver action.

<!-- feature-index:start -->
<!-- feature-index:end -->

Read **Running the gauntlet** first for the core report contract, then
**Judging a feature diff** for scope. The remaining features can be read by
domain: history, gate customization, principle pages, and platform support.

The default command runs the application service driver:

```sh
go test ./features/... -v
```

Use the compiled process boundary explicitly:

```sh
go test ./features/... -v -args -acceptance.driver=cli
go test ./features/... -v -args -acceptance.driver=all
```
~~~

- [ ] **Step 4: Verify the empty catalog is green**

Run: `go test ./features/internal/harness -run TestFeatureIndex -v`

Expected: PASS with zero discovered feature files.

- [ ] **Step 5: Pin Godog**

Run: `go get github.com/cucumber/godog@v0.16.0`

Expected: `go.mod` contains the exact direct requirement and `go.sum` contains
its transitive checksums. Run `go mod tidy` only after Task 2 imports Godog; at
this point `go get` may classify it as indirect temporarily.

- [ ] **Step 6: Verify and commit**

Run: `go test ./features/internal/harness -v`

Expected: PASS.

```bash
git add go.mod go.sum features/README.md features/internal/harness/main.go features/internal/harness/catalog_test.go
git commit -m "Establish the acceptance specification catalog" -m "Give human readers one checked entry point and pin the Gherkin runner before adding executable stories."
```

### Task 2: Preserve Raw Observations Across Driver Boundaries

**Files:**
- Create: `features/internal/harness/driver.go`
- Create: `features/internal/harness/observation.go`
- Create: `features/internal/harness/observation_test.go`

- [ ] **Step 1: Write failing provenance tests**

Test constructors for service and process observations with contradictory
channels:

```go
func TestReportComesOnlyFromPersistedBytes(t *testing.T) {
	obs := newServiceRunObservation(nil, nil, &run.ExitError{Code: 4}, nil, "")
	if _, err := obs.Report(); err == nil {
		t.Fatal("Report succeeded without persisted report bytes")
	}
}

func TestOutcomeDoesNotInferFromReport(t *testing.T) {
	obs := newProcessRunObservation(nil, nil, 1, validErroredReport, "report.json")
	got, err := obs.Outcome()
	if err != nil || got.Code != 1 {
		t.Fatalf("Outcome() = %#v, %v, want process exit 1", got, err)
	}
}
```

Also cover service `run.ExitError` codes 1, 4, and 5,
`wiki.ErrConflictingAliases` as outcome 1, an unexpected service error as
outcome 70, process exits 0, 1, 4, 5, and 70, malformed JSON, unknown report
fields, trailing JSON, report-path provenance, raw artifacts remaining
separate from rendered stdout, and defensive copies of byte slices/maps.

- [ ] **Step 2: Verify the tests are red**

Run: `go test ./features/internal/harness -run 'TestReport|TestOutcome|TestRaw' -v`

Expected: compilation fails because the observation types are undefined.

- [ ] **Step 3: Define domain requests, ports, and the scenario environment**

In `driver.go`, add the contracts from **Harness Contracts**, plus:

```go
var ErrUnsupportedCapability = errors.New("acceptance driver does not support capability")

type Environment struct {
	TempRoot   string
	Home       string
	ConfigRoot string
	StateRoot  string
	CacheRoot  string
	BinRoot    string
	GOOS       string
}

func NewEnvironment() (*Environment, error)
func (e *Environment) Setenv(key, value string) error
func (e *Environment) Activate() error
func (e *Environment) Close() error
func (e *Environment) Environ() []string
func (e *Environment) RepoState(context.Context, string) (string, error)
func (e *Environment) RepoResolutions() int64
```

`NewEnvironment` uses `os.MkdirTemp`, creates isolated home/XDG/bin roots, and
sets `GOOS` to `runtime.GOOS`. `ConfigRoot`, `StateRoot`, and `CacheRoot` are
the full `<XDG home>/togi` paths used by service assembly; `Environ` sets each
XDG variable to that path's parent so the CLI resolves the identical roots.
`Setenv` records scenario-specific variables before activation. `Activate`
installs the scenario environment in the current test process and records the
prior values; `Close` restores them before removing the temporary root and is
idempotent. Godog concurrency one and package-process isolation make one
active environment safe. `Environ` also supplies
`PATH=<BinRoot>:<inherited PATH>`, `LANG=C`, `LC_ALL=C`, and the validated
scenario variables. Store repository-resolution counts in an `atomic.Int64`
and expose only `RepoResolutions`.

- [ ] **Step 4: Implement raw observations and strict artifact decoding**

Use unexported source variants so callers cannot manufacture a decoded
business result:

```go
type Outcome struct {
	Code    int
	Message string
}

type CommandObservation struct {
	stdout []byte
	stderr []byte
	source exitSource
}

type RunObservation struct {
	CommandObservation
	reportBytes []byte
	reportPath  string
	rawPaths    map[string]string
}

func (o CommandObservation) Stdout() string
func (o CommandObservation) Stderr() string
func (o CommandObservation) Outcome() (Outcome, error)
func (o RunObservation) Report() (Report, error)
func (o RunObservation) ReportPath() string
func (o RunObservation) RawPath(gate, language, stream string) (string, bool)
```

Define the acceptance-side schema independently of service-returned values:

```go
type Report struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	RepoID        string       `json:"repo_id"`
	Diff          DiffReport   `json:"diff"`
	StartedAt     time.Time    `json:"started_at"`
	FinishedAt    time.Time    `json:"finished_at"`
	Verdict       string       `json:"verdict"`
	Gates         []GateReport `json:"gates"`
	Findings      []Finding    `json:"findings"`
	Counts        Counts       `json:"counts"`
}

type DiffReport struct {
	BaseRef      string `json:"base_ref"`
	BaseCommit   string `json:"base_commit"`
	MergeBase    string `json:"merge_base"`
	Head         string `json:"head"`
	ChangedFiles int    `json:"changed_files"`
	ChangedLines int    `json:"changed_lines"`
}

type GateReport struct {
	Gate            string    `json:"gate"`
	Language        string    `json:"language"`
	Status          string    `json:"status"`
	Findings        []Finding `json:"findings,omitempty"`
	DurationMS      int64     `json:"duration_ms"`
	ObservedVersion string    `json:"observed_version,omitempty"`
	Warnings        []string  `json:"warnings,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type Finding struct {
	Gate        string       `json:"gate"`
	Language    string       `json:"language"`
	RuleID      string       `json:"rule_id"`
	Severity    string       `json:"severity"`
	File        string       `json:"file"`
	Line        int          `json:"line"`
	EndLine     int          `json:"end_line,omitempty"`
	Snippet     string       `json:"snippet"`
	Occurrences []Occurrence `json:"occurrences,omitempty"`
	Message     string       `json:"message"`
	Fingerprint string       `json:"fingerprint"`
}

type Occurrence struct {
	Line    int `json:"line"`
	EndLine int `json:"end_line,omitempty"`
}

type Counts struct {
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Info        int `json:"info"`
	Occurrences int `json:"occurrences"`
}
```

Decode with `json.Decoder.DisallowUnknownFields`, reject trailing input, and
validate only public schema invariants needed by Then steps. Do not call
`run.Ledger.Latest` and do not accept `run.Service.Run`'s returned report as
input.

`Outcome` classifies only its source: `serviceExit` uses `errors.As` for
`*run.ExitError`, maps `wiki.ErrConflictingAliases` to 1, maps nil to 0, and
maps other service errors to 70; `processExit` uses the captured process
status. It never consults report bytes or a decoded verdict.

- [ ] **Step 5: Verify, tidy, and commit**

Run: `go test ./features/internal/harness -v`

Expected: PASS.

Run: `go mod tidy`

Expected: Godog v0.16.0 is a direct requirement.

```bash
git add go.mod go.sum features/internal/harness/driver.go features/internal/harness/observation.go features/internal/harness/observation_test.go
git commit -m "Define acceptance driver observations" -m "Keep process outcomes and persisted reports as independent evidence so the harness cannot recreate togi's verdict semantics."
```

### Task 3: Build Hermetic Repository Fixtures

**Files:**
- Create: `features/internal/harness/repository.go`
- Create: `features/internal/harness/repository_test.go`

- [ ] **Step 1: Write failing repository-builder tests**

Exercise this error-returning API:

```go
type Repository struct {
	Root string
}

func NewRepository(root string) (*Repository, error)
func (r *Repository) Write(path, body string) error
func (r *Repository) WriteBytes(path string, body []byte) error
func (r *Repository) Remove(path string) error
func (r *Repository) Commit(message string) (string, error)
func (r *Repository) Branch(name string) error
func (r *Repository) Checkout(name string) error
func (r *Repository) SetOriginHEAD(branch, commit string) error
func (r *Repository) LinkedWorktree(path, branch string) (*Repository, error)
func (r *Repository) AddSubmodule(path, source string) error
func (r *Repository) Git(args ...string) (string, error)
func (r *Repository) Tree() ([]string, error)
```

Cover commits, diverged branches, local and remote bases, linked worktrees,
renames, deletions, binary files, dirty tracked/untracked state, a local
submodule, unrelated histories, spaces in filenames, and rejection of paths
escaping `Root`.

- [ ] **Step 2: Verify the tests are red**

Run: `go test ./features/internal/harness -run TestRepository -v`

Expected: compilation fails because `Repository` is undefined.

- [ ] **Step 3: Implement the builder over the existing Git policy**

Use `internal/gitcmd.Args(gitcmd.Hermetic, args...)` and
`internal/gitcmd.Env(gitcmd.Hermetic)` for every Git process. Add only the
fixture safety overrides used by `gitcmdtest`: disable signing and point hooks
at `os.DevNull`. Never inherit repository-selection variables.

`NewRepository` initializes `main`, configures fixture-local author identity,
and creates no commit. File methods validate slash-relative paths before using
an `os.Root`. `Commit` stages with `git add -A` and returns the full `HEAD` ID.
`AddSubmodule` adds only the caller's local fixture repository and supplies
`-c protocol.file.allow=always` for that command. `Tree` returns the sorted
output of `git ls-tree -r --name-only HEAD`.

- [ ] **Step 4: Verify and commit**

Run: `go test ./features/internal/harness -run TestRepository -v`

Expected: PASS.

```bash
git add features/internal/harness/repository.go features/internal/harness/repository_test.go
git commit -m "Add acceptance repository fixtures" -m "Build user-visible Git histories with the same hermetic command policy used by production tests."
```

### Task 4: Build Executable Gate Fixtures

**Files:**
- Create: `features/internal/harness/gate_tool.go`
- Create: `features/internal/harness/gate_tool_test.go`

- [ ] **Step 1: Write failing fake-tool and gate-definition tests**

Define and test:

```go
type ToolBehavior struct {
	Stdout        []byte
	Stderr        []byte
	ExitCode      int
	Delay         time.Duration
	VersionStdout []byte
	VersionStderr []byte
	VersionExit   int
	WaitFor       string
	StartedMarker string
	InvokedMarker string
}

func (e *Environment) InstallTool(name string, behavior ToolBehavior) (string, error)

type VersionDefinition struct {
	Command    []string
	Pattern    string
	Constraint string
}

type GateDefinition struct {
	Name, Description, Tool, Normalizer, RuleID, Message string
	Scope       string
	Location    string
	Timeout     time.Duration
	Command     []string
	Version     *VersionDefinition
	Settings    map[string]any
	SeverityMap map[string]string
	Aliases     map[string]string
}

func (e *Environment) WriteGate(GateDefinition) error
func (e *Environment) WriteInvalidGate(name, contents string) error
```

Test stdout/stderr/exit behavior, delayed timeout behavior, wait/release
markers for lock scenarios, version output, invocation markers, absolute and
name-based commands, TOML round trips through `gate.Loader`, an override of a
shipped gate, and an additional override-only gate.

- [ ] **Step 2: Verify the tests are red**

Run: `go test ./features/internal/harness -run 'TestInstallTool|TestWriteGate' -v`

Expected: compilation fails because the gate-fixture API is undefined.

- [ ] **Step 3: Implement scenario-owned executable tools**

On Linux and other Unix hosts, write mode-0700 POSIX shell executables into
`BinRoot`. The script selects version behavior when its first argument is
`version` or `--version`, writes the configured byte fixtures without shell
interpolation, creates markers atomically, waits while `WaitFor` exists, sleeps
for `Delay`, and exits with the configured status. Store payloads in sibling
files rather than embedding arbitrary bytes in shell source.

On Windows, return `ErrUnsupportedCapability` from `InstallTool`; runtime
feature groups skip there before installing tools. Wiki-only tests remain
portable.

Write TOML with `go-toml/v2`, then load it through `gate.Loader` in the test to
prove the fixture itself is valid. Default omitted values are `repo`, `point`,
five seconds, `golangci-json`, and warning severity.

- [ ] **Step 4: Verify and commit**

Run: `go test ./features/internal/harness -run 'TestInstallTool|TestWriteGate' -v`

Expected: PASS on Linux; executable-tool cases skip explicitly on Windows.

```bash
git add features/internal/harness/gate_tool.go features/internal/harness/gate_tool_test.go
git commit -m "Add executable acceptance gate fixtures" -m "Control tool output, timing, versions, and failures without requiring operator-installed gate binaries."
```

### Task 5: Assemble the Service Driver

**Files:**
- Create: `features/internal/harness/service_driver.go`
- Create: `features/internal/harness/service_driver_test.go`

- [ ] **Step 1: Write failing service-driver contract tests**

Use a committed repository and external gate definition to assert:

- `NewGauntlet` runs `run.Service` against the `RunRequest.Root` repository;
- the observation contains rendered stdout, a typed service exit source,
  persisted `report.json`, and persisted raw stdout/stderr paths;
- the returned `run.Report` is not used when persisted bytes are absent;
- `NewHistory` calls status and returns its rendered stdout, stderr, and raw
  application outcome without choosing a report artifact for the application;
- `NewWiki` shows, lints, and ejects through `wiki.Service`;
- clocks and randomness produce deterministic, increasing run IDs within one
  scenario;
- `Environment.GOOS` reaches `run.Service.GOOS`;
- `Environment.RepoResolutions()` increments through a wrapped `ResolveRepo`
  seam;
- closing a driver is idempotent and does not remove the environment before
  the scenario hook closes it.

- [ ] **Step 2: Verify the tests are red**

Run: `go test ./features/internal/harness -run TestServiceDriver -v`

Expected: compilation fails because `serviceFactory` is undefined.

- [ ] **Step 3: Implement deterministic service assembly**

Implement `newServiceFactory()` and the three factory methods. Every action
creates fresh buffers and the relevant service with:

```go
run.Service{
	Paths:      config.Paths{Config: env.ConfigRoot, State: env.StateRoot, Cache: env.CacheRoot},
	Loader:     gate.Loader{OverrideDir: filepath.Join(env.ConfigRoot, "gates")},
	Executor:   run.Executor{Enrichers: enricher.NewRegistry(), Now: env.clock.Now},
	Stdout:     &stdout,
	VerboseOut: &stderr,
	Now:        env.clock.Now,
	Random:     env.random,
	GOOS:       env.GOOS,
	ResolveRepo: env.resolveRepo,
}
```

`Run` always sets `ReportOnly: true`. Application errors become the raw
service exit source; only harness failures such as unreadable artifacts are
returned as the method error. Snapshot run-directory names before invocation
and attach persisted bytes only from the single newly created run directory
afterward. A rejected action therefore cannot inherit an older report. Do not
call `Ledger.Latest` and do not serialize the report returned from
`Service.Run`. Status returns a `CommandObservation`; its Then steps compare
rendered output with reports already captured from the runs that created them,
so the driver never independently chooses what "latest" means.

The scenario Before hook calls `Environment.Activate`; the After hook closes
the driver and environment. Do not hold an environment mutex around an action:
the run-history feature deliberately makes two concurrent calls in one
scenario. This process-global activation is acceptable only while Godog
concurrency is one; Task 7 adds the regression test that prevents parallel
execution without redesigning environment injection.

- [ ] **Step 4: Verify and commit**

Run: `go test ./features/internal/harness -run TestServiceDriver -v`

Expected: PASS on Linux; runtime cases skip elsewhere.

```bash
git add features/internal/harness/service_driver.go features/internal/harness/service_driver_test.go
git commit -m "Add the acceptance service driver" -m "Exercise assembled application services while observing only their public output and persisted artifacts."
```

### Task 6: Select and Run the Compiled CLI Driver

**Files:**
- Modify: `features/internal/harness/main.go`
- Create: `features/internal/harness/selector.go`
- Create: `features/internal/harness/cli_driver.go`
- Create: `features/internal/harness/main_test.go`
- Create: `features/internal/harness/selector_test.go`
- Create: `features/internal/harness/driver_conformance_test.go`

- [ ] **Step 1: Write failing selection and lifecycle tests**

Test pure selection before global flag wiring:

```go
func TestSelectDrivers(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{"", []string{"service"}},
		{"service", []string{"service"}},
		{"cli", []string{"cli"}},
		{"all", []string{"service", "cli"}},
		} {
			got, err := selectDriverNames(tc.value)
			if err != nil {
				t.Fatalf("selectDriverNames(%q): %v", tc.value, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("selectDriverNames(%q) = %v, want %v", tc.value, got, tc.want)
			}
		}
}
```

Assert unknown values fail, selecting CLI without a built binary fails,
service selection never builds, module-root walking works from every domain
depth, build failure is returned, the binary is built under a process-owned
temporary directory, and cleanup occurs after `m.Run` but before `Main`
returns its code. Put those operations behind an unexported
`mainLifecycle(selection string, run func() int, deps lifecycleDeps) int`
helper so unit tests inject build/remove functions and an event log; the public
`Main` passes `m.Run` rather than attempting to construct a `testing.M`.

- [ ] **Step 2: Write failing driver-conformance tests**

Run one small repository, one status call, and each wiki action through
service and CLI factories. Assert equal decoded run-report semantics, status
rendering, wiki rendering, and outcome codes, but do not require byte-identical
run IDs, timestamps, or durations. Assert both run drivers persist report and
raw artifacts outside the target repository.

- [ ] **Step 3: Verify the tests are red**

Run: `go test ./features/internal/harness -run 'TestSelect|TestMain|TestDriverConformance' -v`

Expected: compilation fails because selection, `Main`, and the CLI factory are
undefined.

- [ ] **Step 4: Implement flag parsing and `TestMain` lifecycle**

Register exactly one package flag:

```go
var acceptanceDriver = flag.String(
	"acceptance.driver",
	"service",
	"acceptance driver: service, cli, or all",
)
```

`Main(m *testing.M) int` calls `flag.Parse`, validates selection, finds the
module root from the package working directory, and builds
`./cmd/togi` only for `cli` or `all`. Use `os.MkdirTemp`, append `.exe` on
Windows, and run:

```text
go build -o <temporary binary> ./cmd/togi
```

with `cmd.Dir` at the module root. Store immutable selected factories for
`ForEachSelectedDriver`; call `m.Run`; remove the process temp directory; join
cleanup diagnostics to stderr; and return the test exit code. A cleanup failure
changes a zero test result to 1 but does not replace an existing nonzero test
result. Never call `os.Exit` inside the harness.

Each domain wrapper is exactly:

```go
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m))
}
```

- [ ] **Step 5: Implement CLI actions and raw process observations**

Use `exec.CommandContext` with the compiled binary, `cmd.Dir` set to the
request repository, `cmd.Env` set from `Environment.Environ`, and private
stdout/stderr buffers. Build arguments as follows:

```text
run:    run --report-only --no-color [--base VALUE] [--gate NAME ...]
status: status --no-color
show:   wiki show PAGE
lint:   wiki lint
eject:  wiki eject PAGE
```

Capture `*exec.ExitError.ExitCode()` as a raw process exit. A failure to start
or wait that is not an ordinary process exit is a harness error. Discover
run artifacts with the same before/after directory snapshot as the service
driver. Status and wiki actions return command observations and never attach a
report chosen by the driver.

- [ ] **Step 6: Verify and commit**

Run: `go test ./features/internal/harness -run 'TestSelect|TestMain|TestDriverConformance' -v`

Expected: PASS.

```bash
git add features/internal/harness/main.go features/internal/harness/main_test.go features/internal/harness/selector.go features/internal/harness/selector_test.go features/internal/harness/cli_driver.go features/internal/harness/driver_conformance_test.go
git commit -m "Add selectable acceptance drivers" -m "Keep service execution as the default while making the compiled process boundary and all-driver matrix explicit."
```

### Task 7: Enforce Strict, Deterministic Godog Suites

**Files:**
- Modify: `features/internal/harness/selector.go`
- Create: `features/internal/harness/godog.go`
- Create: `features/internal/harness/godog_test.go`
- Create: `features/internal/harness/testdata/undefined.feature`
- Create: `features/internal/harness/testdata/pending.feature`
- Create: `features/internal/harness/testdata/ambiguous.feature`

- [ ] **Step 1: Write failing suite-option tests**

Assert that the pure `buildFeatureOptions(factory, goos, path)` helper returns:

```go
&godog.Options{
	Format:      "pretty",
	Paths:       []string{path},
	Strict:      true,
	Concurrency: 1,
	Randomize:   0,
	Tags:        combineTags(factory.CapabilityTags(), hostTags(runtime.GOOS)),
}
```

Test `eligibleFeatureCount(options)` with `TestSuite.RetrieveFeatures` and
assert it returns zero for a fully filtered fixture. Then test the public
`FeatureOptions(t, factory, path)` fatal path in a helper subprocess so the
intentional parent failure cannot poison the containing unit test. The public
function adds `TestingT: t` only after validation. Test that service has no
capability exclusion, CLI excludes only `@simulated-platform`, and host
filters are composed separately.

The exact expressions are:

```text
service capability:  empty
CLI capability:      ~@simulated-platform
Linux host:           ~@unsupported-host
non-Linux host:       ~@linux
```

The three invalid fixtures contain one undefined step, one step whose handler
returns `godog.ErrPending`, and one step matched by two registered regular
expressions. Run each without `TestingT` and assert `TestSuite.Run()` is
nonzero. Also assert `RequireGodogSuccess(t, 1)` reports failure through a fake
testing interface and that options never allow concurrency above one.

- [ ] **Step 2: Verify the tests are red**

Run: `go test ./features/internal/harness -run 'TestFeatureOptions|TestStrict|TestDriverTags' -v`

Expected: compilation fails because the Godog helpers are undefined.

- [ ] **Step 3: Implement suite and host helpers**

Add:

```go
func FeatureOptions(t *testing.T, factory DriverFactory, path string) *godog.Options
func RequireGodogSuccess(t *testing.T, status int)
func ForEachSelectedDriver(t *testing.T, func(*testing.T, DriverFactory))
func RequireLinux(t *testing.T)
```

`ForEachSelectedDriver` creates ordered `service` and/or `cli` subtests.
`RequireLinux` calls `t.Skip("Phase 1/2 runtime acceptance specifications require Linux")`.
Call it at the beginning of Linux-only feature tests, before constructing
options; a test omitted by `-run` never calls it. The platform domain uses
host tag expressions rather than skipping its whole package.

Use `RetrieveFeatures` with the final options and require exactly one parsed
feature and at least one eligible pickle. Do not attempt to infer semantic
equivalence between differently worded steps.

- [ ] **Step 4: Verify and commit**

Run: `go test ./features/internal/harness -v`

Expected: PASS, including the nonzero strict-mode meta-tests.

```bash
git add features/internal/harness/selector.go features/internal/harness/godog.go features/internal/harness/godog_test.go features/internal/harness/testdata
git commit -m "Enforce strict acceptance suite execution" -m "Fail empty, undefined, pending, ambiguous, randomized, or unexpectedly parallel feature runs instead of accepting an incomplete specification."
```

### Task 8: Specify Running the Gauntlet

**Files:**
- Modify: `features/README.md`
- Create: `features/gauntlet/main_test.go`
- Create: `features/gauntlet/running_the_gauntlet.feature`
- Create: `features/gauntlet/running_the_gauntlet_test.go`
- Create: `features/gauntlet/steps_test.go`
- Create: `features/internal/harness/steps.go`

- [ ] **Step 1: Author the feature and index it**

Add this link inside the README index:

```markdown
- [Running the gauntlet](gauntlet/running_the_gauntlet.feature)
```

Author the feature with these exact rules and scenario names. Keep each
scenario at three to five business steps; use a Scenario Outline only for the
four infrastructure outcomes and the three verdict outcomes.

```gherkin
Feature: Running the gauntlet
  As a developer evaluating a feature
  I can run independent quality gates
  So that I receive a complete, trustworthy quality report

  Rule: Selected gates produce one normalized report
    Scenario: Shipped Go gates run without installed operator tools
      Given a committed Go repository with a changed function
      And the shipped Go gates report one complexity and one lint finding
      When I run the gauntlet
      Then the report contains the "complexity" and "lint" gates
      And the report contains both shipped findings

    Scenario: An operator can select one gate from the gauntlet
      Given a committed Go repository with a changed function
      And the "complexity" and "lint" gates report findings
      When I run the gauntlet with only the "lint" gate
      Then the report contains only the "lint" gate

    Scenario: Tool findings are normalized into the public schema
      Given a committed Go repository with a changed function
      And the "lint" gate reports rule "golangci-lint/errcheck" on "feature.go" line 4
      When I run the gauntlet
      Then the finding records its gate, language, rule, severity, location, message, and fingerprint

    Scenario: Repeated occurrences are grouped under one finding
      Given a committed Go repository with a changed function
      And one gate reports the same finding on lines 3, 8, and 13
      When I run the gauntlet
      Then the report contains one finding with two occurrences

    Scenario: An unchanged finding keeps its fingerprint across runs
      Given a committed Go repository with one gate finding
      When I run the unchanged gauntlet twice
      Then the finding fingerprint is identical in both reports

    Scenario: Gate reports are ordered independently of completion time
      Given a committed Go repository with "alpha" and "zeta" gates
      And the "alpha" gate finishes after the "zeta" gate
      When I run the gauntlet
      Then the report orders gates as "alpha,zeta"

    Scenario: Compiler-style output excludes raw tool diagnostics
      Given a committed Go repository with one gate finding
      And the gate writes the raw diagnostic "PRIVATE RAW DIAGNOSTIC"
      When I run the gauntlet
      Then stdout contains compiler-style findings
      And the raw diagnostic exists only in a persisted raw artifact

  Rule: An errored gate never suppresses a healthy sibling
    Scenario Outline: A gate infrastructure problem is reported as errored
      Given a committed Go repository with a healthy gate finding
      And a sibling gate experiences <problem>
      When I run the gauntlet
      Then the sibling gate is errored
      And the healthy gate finding remains in the report

      Examples:
        | problem                |
        | a missing tool         |
        | a crashed tool         |
        | a timed out tool       |
        | malformed output       |

    Scenario: A version mismatch is advisory
      Given a committed Go repository with a healthy versioned gate
      And the tool version is outside the gate constraint
      When I run the gauntlet
      Then the gate has a version warning
      And the gate is not errored

  Rule: The report and application outcome agree
    Scenario Outline: A completed run has a classified verdict
      Given a committed Go repository whose gate result is <gate result>
      When I run the gauntlet
      Then the report verdict is <verdict>
      And the application outcome is <outcome>

      Examples:
        | gate result | verdict    | outcome |
        | findings    | findings   | 1       |
        | errored     | errored    | 4       |
        | clear       | unverified | 5       |
```

- [ ] **Step 2: Write the Godog entry point and undefined bindings**

`TestRunningTheGauntlet` calls `harness.RequireLinux`, then
`ForEachSelectedDriver`. For each driver it creates one `godog.TestSuite`
pointed at `running_the_gauntlet.feature`. The initializer allocates one
`gauntletFeature`, registers Before/After hooks, calls shared repository/gate/
report binders, and binds only the feature-specific When action.

Initially bind every expression to `godog.ErrPending`. Keep the expressions
in a literal table in `steps_test.go` so exact duplicate registration is
visible in review.

- [ ] **Step 3: Verify strict mode makes the feature red**

Run: `go test ./features/gauntlet -run '^TestRunningTheGauntlet$' -v`

Expected: FAIL with pending-step diagnostics for the first scenario.

- [ ] **Step 4: Implement shared state and gauntlet assertions**

`harness/steps.go` owns only expressions reused by later domains, including a
committed Go repository, an explicit base, an XDG gate definition, running the
gauntlet, report verdict, outcome code, absence of target-repository files,
and persisted artifact checks. Domain steps own completion ordering,
occurrence grouping, stable fingerprints, compiler rendering, and sibling
survival.

Use concrete fixtures:

- shipped complexity emits `18 pkg.complex complex.go:3:1`;
- shipped lint emits one golangci JSON issue and a compatible version;
- repeated occurrences use one rule/file/snippet on lines 3, 8, and 13;
- ordering delays `complexity` after `lint` but expects alphabetical gate
  reports;
- raw stderr contains `PRIVATE RAW DIAGNOSTIC`, must exist in the persisted raw
  artifact, and must not occur in rendered stdout or report findings;
- infrastructure examples always pair the unhealthy gate with a healthy gate
  finding and assert the healthy finding remains;
- mismatch returns a syntactically valid older version and asserts a warning,
  not `errored`.

Then steps return errors such as:

```go
return fmt.Errorf("report verdict = %q, want %q; report: %#v", got, want, report)
```

They never call `t.Fatal` and never inspect a service-returned report.

- [ ] **Step 5: Verify both drivers and commit**

Run: `go test ./features/gauntlet -run '^TestRunningTheGauntlet$' -v`

Expected: PASS through the default service driver.

Run: `go test ./features/gauntlet -run '^TestRunningTheGauntlet$' -v -args -acceptance.driver=all`

Expected: every example runs first under `service`, then under `cli`, and
passes without installed gate tools.

```bash
git add features/README.md features/gauntlet features/internal/harness/steps.go
git commit -m "Specify running the gauntlet" -m "Describe gate execution, normalized findings, infrastructure errors, and verdicts as one executable user story."
```

### Task 9: Specify Judging a Feature Diff

**Files:**
- Modify: `features/README.md`
- Create: `features/gauntlet/judging_a_feature_diff.feature`
- Create: `features/gauntlet/judging_a_feature_diff_test.go`
- Modify: `features/gauntlet/steps_test.go`
- Modify: `features/internal/harness/steps.go`

- [ ] **Step 1: Author and index the diff feature**

Add:

```markdown
- [Judging a feature diff](gauntlet/judging_a_feature_diff.feature)
```

Use this scenario catalog:

```gherkin
Feature: Judging a feature diff
  As a developer evaluating committed work
  I can judge findings against the feature diff
  So that unrelated repository findings do not obscure my change

  Rule: The feature is measured from a merge base
    Scenario: An explicit base selects the comparison history
      Given a committed feature branch with two possible bases
      And a gate finding belongs only to the explicitly based diff
      When I run the gauntlet with the older base
      Then the report records the explicit base and its merge base
      And the finding is in scope

    Scenario: The base is discovered from origin HEAD
      Given a committed feature branch whose origin HEAD points to "release"
      When I run the gauntlet without a base
      Then the report base is "origin/release"

    Scenario: A conventional local trunk is discovered without a remote
      Given a committed feature branch from local "main" without a remote
      When I run the gauntlet without a base
      Then the report base is "main"

    Scenario: Diverged history uses the merge base rather than the base tip
      Given trunk and the feature branch diverged after a shared commit
      And a gate reports findings on both branches' changes
      When I run the gauntlet against trunk
      Then the report merge base is the shared commit
      And only the feature finding is in scope

  Rule: Gate scope determines which findings survive
    Scenario: A point finding survives only on a changed line
      Given a committed feature changes line 8 but not line 3
      And a point-scoped gate reports findings on lines 3 and 8
      When I run the gauntlet
      Then only the finding on line 8 remains

    Scenario: Touching a Go declaration includes its structural finding
      Given a committed feature changes the body of function "calculate"
      And an entity-scoped gate reports the function signature
      When I run the gauntlet
      Then the structural finding for "calculate" remains

    Scenario: A repository-scoped finding survives outside the diff
      Given a committed feature changes "feature.go"
      And a repository-scoped gate reports a finding in "legacy.go"
      When I run the gauntlet
      Then the finding in "legacy.go" remains

    Scenario: Deleting a line owns the adjacent deletion location
      Given a committed feature deletes line 5 from "feature.go"
      And a point-scoped gate reports the deletion location
      When I run the gauntlet
      Then the deletion finding remains in scope

    Scenario: A renamed file is judged at its new path
      Given a committed feature renames "before.go" to "after.go"
      And a gate reports a finding in "after.go"
      When I run the gauntlet
      Then the report records the finding in "after.go"

    Scenario: A binary change is recorded without inventing changed lines
      Given a committed feature changes the binary file "image.bin"
      When I run the gauntlet
      Then the diff records one changed file and zero changed lines

  Rule: Invalid repository state prevents a run from starting
    Scenario Outline: A repository precondition fails before tools or state
      Given a repository with <precondition>
      And a gate that records whether it starts
      When I run the gauntlet
      Then the run is rejected for the <precondition>
      And no gate, ledger, or target-repository file is created

      Examples:
        | precondition          |
        | dirty worktree        |
        | unsupported submodule |
        | missing base          |
        | invalid base          |
        | unrelated history     |
```

- [ ] **Step 2: Add pending bindings and verify red**

Create `TestJudgingAFeatureDiff` with the same initializer pattern and
Linux precondition as Task 8. Reuse exact shared expressions by binding the
harness step set; do not copy them into the gauntlet package.

Run: `go test ./features/gauntlet -run '^TestJudgingAFeatureDiff$' -v`

Expected: FAIL on pending diff-specific steps.

- [ ] **Step 3: Implement repository shapes and assertions**

Build every history with `Repository`; do not mock Git. Use one representative
finding for point, entity, and repo scope. For deletions assert the changed
line count and surviving location documented by the report. For renames assert
the report finding path is the destination. For binary changes assert the file
count increases while changed lines remain zero.

For every precondition row, install a gate tool with `InvokedMarker`, snapshot
the target tree, invoke run, then assert:

```text
outcome = application error (70 at CLI boundary)
report.json does not exist
state repository directory does not exist
tool invocation marker does not exist
target tree is unchanged
```

The service driver may expose the underlying typed error text; shared Then
steps compare the stable user-facing classification and required diagnostic
substring, not concrete wrapped error types.

- [ ] **Step 4: Verify both drivers and commit**

Run: `go test ./features/gauntlet -run '^TestJudgingAFeatureDiff$' -v -args -acceptance.driver=all`

Expected: PASS under service and CLI.

Run: `go test ./features/gauntlet -v`

Expected: both gauntlet features PASS under service.

```bash
git add features/README.md features/gauntlet/judging_a_feature_diff.feature features/gauntlet/judging_a_feature_diff_test.go features/gauntlet/steps_test.go features/internal/harness/steps.go
git commit -m "Specify judging a feature diff" -m "Make merge-base scope and repository preconditions readable at the user-story boundary."
```

### Task 10: Specify Durable Run History

**Files:**
- Modify: `features/README.md`
- Create: `features/runledger/main_test.go`
- Create: `features/runledger/keeping_run_history.feature`
- Create: `features/runledger/keeping_run_history_test.go`
- Create: `features/runledger/steps_test.go`
- Modify: `features/internal/harness/steps.go`

- [ ] **Step 1: Author and index the history feature**

Add:

```markdown
- [Keeping run history](runledger/keeping_run_history.feature)
```

Use:

```gherkin
Feature: Keeping run history
  As a developer returning to a repository
  I can inspect durable run history
  So that results survive checkouts without modifying the target repository

  Rule: Completed runs live outside the target repository
    Scenario: A run persists its report and raw gate output externally
      Given a committed Go repository with one gate finding
      When I run the gauntlet
      Then report.json and both raw gate streams are persisted under XDG state

    Scenario: Running togi adds no files to the target repository
      Given a committed Go repository with one gate finding
      When I run the gauntlet
      Then the target repository tree and status are unchanged

    Scenario: Linked worktrees share one repository history
      Given a repository with primary and linked feature worktrees
      And a completed run in the linked worktree
      When I inspect status from the primary worktree
      Then status renders the linked worktree run

  Rule: The repository history has one active writer
    Scenario: A second run is rejected while the first run is active
      Given a committed Go repository with a gate paused after startup
      When I start another gauntlet run for the repository
      Then the second run is rejected as locked
      And the first run can complete after the gate resumes

    Scenario: An abandoned unlocked lock file does not block a new run
      Given a committed Go repository with an abandoned unlocked ledger file
      When I run the gauntlet
      Then a completed report is persisted

  Rule: History remains bounded and readable
    Scenario: Starting a run prunes history to the retention limit
      Given a committed Go repository with 20 completed runs
      When I complete one more run
      Then only the newest 20 run directories remain

    Scenario: Status selects the latest complete valid report
      Given a committed Go repository with two completed runs
      When I inspect repository status
      Then status renders the newer completed run

    Scenario: Status ignores newer incomplete and corrupt runs
      Given a committed Go repository with one completed run
      And newer incomplete and corrupt run directories
      When I inspect repository status
      Then status renders the completed run
```

- [ ] **Step 2: Add pending bindings and verify red**

Create the exact `TestMain` wrapper and `TestKeepingRunHistory`; call
`RequireLinux` before driver iteration.

Run: `go test ./features/runledger -run '^TestKeepingRunHistory$' -v`

Expected: FAIL on pending history steps.

- [ ] **Step 3: Implement ledger fixtures and status assertions**

Use external state discovered through `Environment.RepoState`; never import or
call unexported ledger helpers. Required fixtures:

- persist a unique raw marker to each stream and assert paths are under
  `XDG_STATE_HOME/togi/<full-repo-id>/runs/<run-id>/raw`;
- compare `git status --porcelain=v1 -z` and the committed tree before/after;
- create a linked worktree, run there, then call status in the primary worktree
  and compare run ID;
- block the first gate tool on a release file, await `StartedMarker`, invoke a
  second run, assert the locked diagnostic and no second run directory, then
  release and join the first invocation;
- leave a regular mode-0600 `lock` file with an old JSON record but no live
  advisory owner, then assert the next run completes;
- create 21 completed runs with the deterministic clock and assert only the 20
  newest valid run directories remain;
- after a valid run, add lexically newer validly named directories containing
  no report and malformed report JSON, then assert status returns the valid
  run and rendering.

All goroutines have explicit completion channels and a five-second test
deadline; After cleanup releases a blocked tool before waiting, so a failed
step cannot strand the test process.

- [ ] **Step 4: Verify both drivers and commit**

Run: `go test ./features/runledger -v -args -acceptance.driver=all`

Expected: PASS under service and CLI.

```bash
git add features/README.md features/runledger features/internal/harness/steps.go
git commit -m "Specify durable run history" -m "Exercise external persistence, repository-wide locking, pruning, and latest-valid status selection as user behavior."
```

### Task 11: Specify Gate Customization

**Files:**
- Modify: `features/README.md`
- Create: `features/gate/main_test.go`
- Create: `features/gate/customizing_gates.feature`
- Create: `features/gate/customizing_gates_test.go`
- Create: `features/gate/steps_test.go`
- Modify: `features/internal/harness/steps.go`

- [ ] **Step 1: Author and index the gate feature**

Add:

```markdown
- [Customizing gates](gate/customizing_gates.feature)
```

Use:

```gherkin
Feature: Customizing gates
  As an operator evolving personal standards
  I can customize gates outside the target repository
  So that my gauntlet changes without imposing repository files on collaborators

  Rule: Shipped definitions work without repository configuration
    Scenario: Shipped gates normalize representative Go tool output
      Given a committed Go repository with a changed function
      And the shipped Go gates report representative findings
      When I run the gauntlet
      Then the shipped findings are normalized without repository configuration

  Rule: XDG definitions replace or extend the shipped gauntlet
    Scenario: An XDG definition wholly overrides a shipped gate
      Given a committed Go repository with an XDG override for the "lint" gate
      When I run the gauntlet
      Then the report contains the overridden lint behavior only
      And the target repository contains no gate definition

    Scenario: An XDG-only gate joins the shipped gates
      Given a committed Go repository with an additional "architecture" gate in XDG config
      When I run the gauntlet
      Then the report contains the shipped gates and the "architecture" gate
      And the target repository contains no gate definition

  Rule: Invalid gate data prevents tool execution
    Scenario: An invalid definition is rejected before any gate starts
      Given a committed Go repository with an invalid XDG gate definition
      And every available gate records whether it starts
      When I run the gauntlet
      Then the run is rejected for invalid gate data
      And no gate, ledger, or target-repository file is created
```

- [ ] **Step 2: Add pending bindings and verify red**

Create the domain `TestMain`, call `RequireLinux`, and assemble one suite for
`customizing_gates.feature`.

Run: `go test ./features/gate -v`

Expected: FAIL on pending customization steps.

- [ ] **Step 3: Implement customization fixtures and assertions**

For shipped behavior install name-based `gocyclo` and `golangci-lint` tools in
`BinRoot` but write no gate TOML. For wholesale override, redefine `lint` with
a unique rule ID/message and assert no shipped lint finding leaks through. For
the additional gate, define `architecture` and assert it appears in sorted
order beside shipped gates.

For invalid data, use a well-formed manifest with an unknown field so strict
TOML rejection is the observed contract. Give every potential tool an
`InvokedMarker`; assert none exist, no ledger was created, and the repository
tree is unchanged.

- [ ] **Step 4: Verify both drivers and commit**

Run: `go test ./features/gate -v -args -acceptance.driver=all`

Expected: PASS under service and CLI.

```bash
git add features/README.md features/gate features/internal/harness/steps.go
git commit -m "Specify gate customization" -m "Show that operator-owned XDG definitions replace and extend shipped gates without touching collaborator repositories."
```

### Task 12: Specify Principle Pages

**Files:**
- Modify: `features/README.md`
- Create: `features/wiki/main_test.go`
- Create: `features/wiki/using_principle_pages.feature`
- Create: `features/wiki/using_principle_pages_test.go`
- Create: `features/wiki/steps_test.go`

- [ ] **Step 1: Author and index the wiki feature**

Add:

```markdown
- [Using principle pages](wiki/using_principle_pages.feature)
```

Use:

```gherkin
Feature: Using principle pages
  As a developer understanding a finding
  I can inspect and customize principle pages
  So that tool-specific rules lead to stable engineering guidance

  Rule: Pages explain the principles behind findings
    Scenario: A shipped principle page can be shown
      Given no operator copy of "small-composable-functions"
      When I show the "small-composable-functions" principle page
      Then the shipped page body and provenance are displayed

    Scenario: An operator page overrides the shipped page
      Given an operator copy of "small-composable-functions"
      When I show the "small-composable-functions" principle page
      Then the operator page body and provenance are displayed

    Scenario: Page aliases are displayed in deterministic order
      Given several gate aliases for "small-composable-functions"
      When I show the "small-composable-functions" principle page
      Then its aliases are displayed in gate, language, and rule order

  Rule: Alias problems are explicit
    Scenario: A dangling alias produces a warning without failing lint
      Given a gate alias whose principle page does not exist
      When I lint the principle pages
      Then the dangling alias is warned and the outcome is 0

    Scenario: Conflicting aliases fail wiki lint
      Given one rule is aliased to two principle pages
      When I lint the principle pages
      Then both conflicting pages are reported and the outcome is 1

  Rule: Ejection preserves operator work
    Scenario: Ejecting a page creates an operator copy
      Given no operator copy of "small-composable-functions"
      When I eject the "small-composable-functions" principle page
      Then the operator copy equals the shipped page

    Scenario: Ejecting never overwrites an existing operator copy
      Given an existing operator copy of "small-composable-functions"
      When I eject the "small-composable-functions" principle page
      Then the eject is rejected and the operator copy is unchanged
```

- [ ] **Step 2: Add pending bindings and verify red**

Create the domain `TestMain` and Godog suite. Do not call `RequireLinux`; wiki
service and CLI behavior are portable and use no gate executables.

Run: `go test ./features/wiki -v`

Expected: FAIL on pending page steps.

- [ ] **Step 3: Implement wiki fixtures and assertions**

Write page overrides below `ConfigRoot/wiki` and gate definitions below
`ConfigRoot/gates`. Assert shipped body/provenance, override body/provenance,
and exact sorted alias lines. For dangling aliases assert outcome 0, warning on
stderr, and summary on stdout. For conflicts assert outcome 1 and both page
names. For eject assert the file equals the shipped page bytes; pre-create a
different body before the second eject and prove the bytes remain unchanged.

Expected application outcomes remain observations. Step errors are reserved
for inability to create/read fixtures or invoke a driver.

- [ ] **Step 4: Verify both drivers and commit**

Run: `go test ./features/wiki -v -args -acceptance.driver=all`

Expected: PASS under service and CLI on every build host.

```bash
git add features/README.md features/wiki
git commit -m "Specify principle page behavior" -m "Make shipped guidance, XDG overrides, alias diagnostics, and non-destructive ejection executable user stories."
```

### Task 13: Specify Platform Support

**Files:**
- Modify: `features/README.md`
- Create: `features/platform/main_test.go`
- Create: `features/platform/supporting_platforms.feature`
- Create: `features/platform/supporting_platforms_test.go`
- Create: `features/platform/steps_test.go`
- Modify: `features/internal/harness/selector.go`

- [ ] **Step 1: Author and index the platform feature**

Add:

```markdown
- [Supporting platforms](platform/supporting_platforms.feature)
```

Use host and capability tags explicitly:

```gherkin
Feature: Supporting platforms
  As an operator running togi
  I receive an explicit platform result
  So that an unsupported machine cannot appear to have passed its gates

  @linux
  Scenario: The real Linux host executes the gauntlet
    Given a committed Go repository with a clear gate
    When I run the gauntlet on the real host
    Then a completed unverified report is persisted

  @unsupported-host
  Scenario: A real unsupported host rejects the gauntlet before startup
    Given a committed Go repository with a gate that records whether it starts
    When I run the gauntlet on the real host
    Then the platform is rejected before repository, gate, or ledger access

  @simulated-platform
  Scenario Outline: A simulated unsupported platform is rejected before startup
    Given a committed Go repository with a gate that records whether it starts
    And the runtime platform is <platform>
    When I run the gauntlet
    Then the platform is rejected before repository, gate, or ledger access

    Examples:
      | platform |
      | darwin   |
      | windows  |
      | freebsd  |
```

- [ ] **Step 2: Add pending bindings and verify red**

The platform package uses `harness.Main` but never skips the whole feature.
Host filters are:

```text
Linux:     exclude @unsupported-host
non-Linux: exclude @linux
CLI:       additionally exclude @simulated-platform
```

Run: `go test ./features/platform -v`

Expected on Linux: FAIL on the real-Linux and simulated-platform pending
steps. Expected elsewhere: FAIL on the real-unsupported and simulated-platform
pending steps.

- [ ] **Step 3: Implement platform assertions**

For Linux install a passing gate and assert a persisted unverified report with
outcome 5. For the real unsupported host, run the actual service or CLI and
assert the unsupported-platform diagnostic and outcome 70. For simulated
platforms set `Environment.GOOS` before creating the service driver.

Every rejection points its gate command at the current platform-domain test
binary with `-test.run=^TestGateStartupProbe$`. That helper writes the path in
`TOGI_ACCEPTANCE_GATE_MARKER` when the variable is present and then returns; in
ordinary parent execution it returns without writing. This produces a
cross-platform startup marker without requiring a shell executable.

Snapshot the target tree and record `Environment.RepoResolutions`. Assert zero
repository resolutions, no tool marker, no repo state directory, and an
unchanged target tree. This proves the check occurs before repository
resolution, gate startup, and ledger access.

- [ ] **Step 4: Verify driver matrices and commit**

Run: `go test ./features/platform -v`

Expected: service runs all host-eligible scenarios.

Run: `go test ./features/platform -v -args -acceptance.driver=cli`

Expected: CLI runs exactly the real-host scenario and never silently succeeds
with zero examples.

Run: `go test ./features/platform -v -args -acceptance.driver=all`

Expected: service includes simulated examples; CLI excludes only those
examples and runs the real-host example.

```bash
git add features/README.md features/platform features/internal/harness/selector.go
git commit -m "Specify supported platform outcomes" -m "Prove unsupported systems stop before repository, gate, or ledger work and cannot look like a passing gauntlet."
```

### Task 14: Audit the Executable Catalog and Full Driver Matrix

**Files:**
- Modify: `features/README.md`
- Modify: `features/internal/harness/catalog_test.go`
- Modify: acceptance files only when the audit finds a concrete gap

- [ ] **Step 1: Add the static driver-exclusion audit**

Extend the catalog test to parse every indexed feature through Godog and
collect scenario tags. Maintain the only initial capability exclusion as:

```go
var capabilityExclusions = map[string]map[string]string{
	"cli": {
		"@simulated-platform": "requires test-only platform selection",
	},
}
```

Fail when a driver tag expression excludes a capability tag absent from this
map, when a declared exclusion tag appears nowhere in indexed features, or
when an exclusion has an empty reason. Treat `@linux` and
`@unsupported-host` as host eligibility, not driver capability exclusions.

- [ ] **Step 2: Verify the human catalog mechanically**

Run: `go test ./features/internal/harness -run 'TestFeatureIndex|TestDriverCapabilityMatrix' -v`

Expected: PASS with exactly six indexed features, six adjacent matching test
files, and one declared CLI capability exclusion.

- [ ] **Step 3: Audit feature-catalog coverage by name**

Read the approved design's **Feature Catalog** section beside the six feature
files. Check off each named behavior against a Scenario or Rule. Specifically
confirm these easily missed contracts are present:

```text
healthy sibling survives every infrastructure failure
raw tool output appears only as a persisted artifact
merge-base behavior covers diverged history
preconditions run before ledger and tools
linked worktrees share state
status ignores both incomplete and corrupt runs
invalid gate definitions start no tool
aliases display deterministically
unsupported platforms resolve no repository
```

If any item has no scenario, add one with bindings and assertions before
continuing. Do not add parser matrices or filesystem attack permutations; the
approved unit-test boundary leaves those in package tests.

- [ ] **Step 4: Run default offline verification**

With module dependencies already present, disable network access for the test
process using the environment's available network sandbox and run:

```sh
go test ./features/... -v
go test ./...
go build ./...
```

Expected: PASS. The first two commands run only the service acceptance driver;
no scenario requires installed gate tools or network access.

- [ ] **Step 5: Run explicit CLI and all-driver verification**

Still without network access or installed gate tools, run:

```sh
go test ./features/... -v -args -acceptance.driver=cli
go test ./features/... -v -args -acceptance.driver=all
```

Expected: PASS. CLI-only runs no service examples; all-driver output shows
service before CLI for each feature. On Linux, no required scenario is
skipped.

- [ ] **Step 6: Verify production linkage**

Run:

```sh
go list -deps ./cmd/togi | rg 'cucumber|features/internal/harness'
```

Expected: no output and exit 1 from `rg`, proving the production command does
not depend on Godog or the acceptance harness.

Then run:

```sh
go build ./features/internal/harness
```

Expected: PASS, proving the ordinary shared harness still compiles under
`go build ./...`.

- [ ] **Step 7: Commit the completed acceptance suite**

Only create this commit when the audit required real corrections. If Tasks
1-13 already satisfy every check, leave the prior domain commits as the final
history rather than creating an empty commit.

```bash
git add acceptance
git commit -m "Complete the executable acceptance catalog" -m "Keep the human index, capability matrix, and both driver boundaries synchronized after the full behavioral audit."
```

## Completion Evidence

Before declaring implementation complete, record the exit status and scenario
counts from:

```sh
go test ./features/... -v
go test ./features/... -v -args -acceptance.driver=cli
go test ./features/... -v -args -acceptance.driver=all
go test ./...
go build ./...
```

Also record that `go list -deps ./cmd/togi` contains neither Godog nor the
acceptance harness, that the catalog contains exactly six feature links, and
that the target repository trees remained unchanged in the relevant
scenarios.
