# Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `togi run --report-only` so a Go repository's gate output is
normalized, persisted outside the repository, and rendered with typed exit
codes.

**Architecture:** A thin Cobra command delegates to glossary-owned internal
packages. Gate definitions are embedded TOML data; compiled normalizers feed a
no-op enricher, then a concurrent runner deterministically collects findings
and writes an XDG run ledger.

**Tech Stack:** Go 1.25, Cobra v1.10.2, go-toml/v2 v2.4.3, Git plumbing, Go
stdlib subprocesses/concurrency/JSON/embed, golangci-lint v2.12.2, gocyclo
v0.6.0.

---

## File Map

- `go.mod`, `go.sum`: module and the only two runtime dependencies.
- `cmd/togi/main.go`: process entry point and typed exit handoff.
- `cmd/togi/command.go`: thin Cobra tree and dependency wiring.
- `cmd/togi/command_test.go`: CLI surface and exit-code integration tests.
- `internal/repoid/repoid.go`: Git root discovery and repo-id fallback chain.
- `internal/repoid/repoid_test.go`: real temporary Git repository tests.
- `internal/config/paths.go`: XDG path resolution.
- `internal/config/paths_test.go`: environment and fallback tests.
- `internal/finding/finding.go`: schema, severity, fingerprinting.
- `internal/finding/group.go`: occurrence grouping and deterministic order.
- `internal/finding/finding_test.go`: schema/fingerprint/grouping tests.
- `internal/gate/model.go`: manifest and binding domain types.
- `internal/gate/embed.go`: embedded shipped gate filesystem.
- `internal/gate/load.go`: strict TOML load, override, and validation.
- `internal/gate/template.go`: argument-template rendering.
- `internal/gate/version.go`: phase 1 version extraction and warning matching.
- `internal/gate/gate_test.go`: embedded, override, validation, and rendering
  tests.
- `internal/gate/defaults/gates/lint/gate.toml`: umbrella lint manifest.
- `internal/gate/defaults/gates/lint/go/binding.toml`: golangci-lint v2
  binding.
- `internal/gate/defaults/gates/complexity/gate.toml`: complexity manifest.
- `internal/gate/defaults/gates/complexity/go/binding.toml`: gocyclo binding.
- `internal/normalizer/normalizer.go`: registry and normalization context.
- `internal/normalizer/golangci.go`: golangci-lint v2 JSON parser.
- `internal/normalizer/regex.go`: named-capture regex parser.
- `internal/normalizer/normalizer_test.go`: golden tests.
- `internal/normalizer/testdata/{golangci,gocyclo}/`: recorded raw input,
  source input, and expected findings JSON.
- `internal/enricher/enricher.go`: interface and phase 1 no-op.
- `internal/enricher/enricher_test.go`: preservation test.
- `internal/run/report.go`: report, gate result, verdict, and count types.
- `internal/run/ledger.go`: run IDs, directories, atomic report write, latest,
  and pruning.
- `internal/run/lock.go`, `lock_unix.go`, `lock_windows.go`: exclusive lock and
  portable PID liveness.
- `internal/run/ledger_test.go`: ledger, stale lock, and pruning tests.
- `internal/run/executor.go`: subprocess execution, timeout, output cap, and
  version observation.
- `internal/run/collector.go`: bounded fan-out and deterministic barrier.
- `internal/run/executor_test.go`: fake-binary execution/error tests.
- `internal/run/render.go`: compiler-style report rendering.
- `internal/run/exit.go`: typed exit errors and precedence.
- `internal/run/run.go`: end-to-end run and status orchestration.
- `internal/run/run_test.go`: fake-gate report integration tests.
- `internal/{adapter,flywheel,triage,wiki}/doc.go`: glossary package stubs for
  later phases.

### Task 1: Buildable CLI Skeleton

**Files:**

- Create: `go.mod`
- Create: `go.sum`
- Create: `cmd/togi/main.go`
- Create: `cmd/togi/command.go`
- Create: `cmd/togi/command_test.go`
- Create: `internal/{adapter,config,enricher,finding,flywheel,gate,normalizer,repoid,run,triage,wiki}/doc.go`

- [ ] **Step 1: Write the failing version command test**

```go
func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(streams{out: &stdout, err: &stderr})
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()

	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "togi dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the test and verify RED**

Run: `go test ./cmd/togi -run TestVersionCommand -v`

Expected: FAIL because `newRootCommand` and `streams` do not exist.

- [ ] **Step 3: Add the module and minimal Cobra command**

Use this module declaration:

```go
module github.com/joellarson/togi

go 1.25

require github.com/spf13/cobra v1.10.2
```

Implement `command.go` around these exact contracts:

```go
var version = "dev"

type streams struct {
	out io.Writer
	err io.Writer
}

func newRootCommand(s streams) *cobra.Command {
	root := &cobra.Command{
		Use:           "togi",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(s.out)
	root.SetErr(s.err)
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the togi version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "togi %s\n", version)
		},
	})
	return root
}
```

`main.go` executes the root command and exits nonzero on error. Each `doc.go`
contains only `package <name>` and a package comment; no behavior is added.

- [ ] **Step 4: Resolve dependencies and verify GREEN**

Run: `go mod tidy && go test ./... && go build ./...`

Expected: all packages build and `TestVersionCommand` passes.

- [ ] **Step 5: Commit the skeleton**

```bash
git add go.mod go.sum cmd internal
git commit -m "Build the togi command skeleton" \
  -m "Establish the Go 1.25 module, subcommand surface, and glossary-owned package layout before behavioral packages begin depending on one another."
```

### Task 2: Repository Identity

**Files:**

- Modify: `internal/repoid/doc.go`
- Create: `internal/repoid/repoid.go`
- Create: `internal/repoid/repoid_test.go`

- [ ] **Step 1: Write real-Git failing tests for every fallback**

Create helpers that run `git init`, set local author configuration, and commit
a file inside `t.TempDir()`. Cover these contracts:

```go
func TestResolveUsesRootCommit(t *testing.T) {
	repo := newCommittedRepo(t)
	want := gitOutput(t, repo, "rev-list", "--max-parents=0", "HEAD")
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil { t.Fatal(err) }

	got, err := Resolve(context.Background(), nested)

	if err != nil { t.Fatal(err) }
	if got.Key != want { t.Fatalf("Key = %q, want %q", got.Key, want) }
	if got.Root != repo { t.Fatalf("Root = %q, want %q", got.Root, repo) }
}

func TestResolveHashesNormalizedRemoteWithoutRoot(t *testing.T) {
	repo := newEmptyRepo(t)
	gitRun(t, repo, "remote", "add", "origin", "git@github.com:JoelLarson/togi.git")

	got, err := Resolve(context.Background(), repo)

	if err != nil { t.Fatal(err) }
	want := hash("github.com/JoelLarson/togi")
	if got.Key != want { t.Fatalf("Key = %q, want %q", got.Key, want) }
}

func TestResolveHashesCanonicalPathWithoutCommitOrRemote(t *testing.T) {
	repo := newEmptyRepo(t)

	got, err := Resolve(context.Background(), repo)

	if err != nil { t.Fatal(err) }
	real, _ := filepath.EvalSymlinks(repo)
	if got.Key != hash(real) { t.Fatalf("unexpected path fallback: %q", got.Key) }
}
```

Also test multiple roots by constructing unrelated histories and merging with
`--allow-unrelated-histories`; the expected key is the SHA-256 of sorted root
SHAs joined by a newline. Test directory naming as `<basename>-<12 hex>`.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/repoid -v`

Expected: FAIL because `Resolve`, `ID`, and normalization do not exist.

- [ ] **Step 3: Implement the fallback chain with Git plumbing**

Expose only:

```go
type ID struct {
	Key       string
	Directory string
	Root      string
}

func Resolve(ctx context.Context, start string) (ID, error)
```

Resolve the root with `git -C <start> rev-parse --show-toplevel`; run
`rev-list --max-parents=0 HEAD`; sort and hash multiple roots. Normalize SSH and
HTTPS remotes by lowercasing only the host, removing credentials, trailing
slash, and `.git`, while preserving the case-sensitive repository path; then
hash. Finally hash the evaluated absolute root path. Construct `Directory` with
a sanitized basename plus `Key[:12]`. Wrap Git errors with the command purpose,
never raw shell interpolation.

- [ ] **Step 4: Verify GREEN and regression coverage**

Run: `go test ./internal/repoid -v && go test ./...`

Expected: all repo-id fallback tests and the full suite pass.

- [ ] **Step 5: Commit**

```bash
git add internal/repoid
git commit -m "Derive stable repository identities" \
  -m "Key external state by root history when available while retaining deterministic remote and path fallbacks for shallow and empty repositories."
```

### Task 3: XDG Paths

**Files:**

- Modify: `internal/config/doc.go`
- Create: `internal/config/paths.go`
- Create: `internal/config/paths_test.go`

- [ ] **Step 1: Write failing path-resolution tests**

```go
func TestResolvePathsHonorsXDGEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_CACHE_HOME", "/xdg/cache")

	got, err := ResolvePaths("/home/test")

	if err != nil { t.Fatal(err) }
	want := Paths{
		Config: "/xdg/config/togi",
		State:  "/xdg/state/togi",
		Cache:  "/xdg/cache/togi",
	}
	if got != want { t.Fatalf("Paths = %#v, want %#v", got, want) }
}

func TestResolvePathsUsesStandardFallbacks(t *testing.T) {
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
	}
	got, err := ResolvePaths("/home/test")
	if err != nil { t.Fatal(err) }
	if got.Config != "/home/test/.config/togi" { t.Fatal(got.Config) }
	if got.State != "/home/test/.local/state/togi" { t.Fatal(got.State) }
	if got.Cache != "/home/test/.cache/togi" { t.Fatal(got.Cache) }
}
```

Add a test rejecting an empty/non-absolute home when a fallback needs it, a
test showing a relative XDG value is ignored in favor of its standard fallback,
and a test proving resolution creates no directories.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/config -v`

Expected: FAIL because `Paths` and `ResolvePaths` do not exist.

- [ ] **Step 3: Implement pure path computation**

```go
type Paths struct {
	Config string
	State  string
	Cache  string
}

func ResolvePaths(home string) (Paths, error)

func (p Paths) RepoState(directory string) string {
	return filepath.Join(p.State, directory)
}

func (p Paths) GateOverrides() string {
	return filepath.Join(p.Config, "gates")
}
```

Use environment values only when absolute and non-empty. Ignore relative XDG
values as required by the XDG base-directory specification. Return a
descriptive error only when the home needed for a fallback is invalid.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/config -v && go test ./...`

Expected: all tests pass and no test leaves XDG directories behind.

```bash
git add internal/config
git commit -m "Resolve external togi storage paths" \
  -m "Keep hand-authored configuration, durable run state, and reconstructible cache separate without touching target repositories."
```

### Task 4: Finding Schema, Fingerprints, and Occurrences

**Files:**

- Modify: `internal/finding/doc.go`
- Create: `internal/finding/finding.go`
- Create: `internal/finding/group.go`
- Create: `internal/finding/finding_test.go`
- Create: `internal/finding/path_unix_test.go`
- Create: `internal/finding/path_windows_test.go`

- [ ] **Step 1: Write failing schema and fingerprint tests**

Define the expected API through tests:

```go
func TestFingerprintIgnoresLineAndWhitespaceShape(t *testing.T) {
	a := Finding{Gate: "lint", RuleID: "golangci-lint/errcheck", File: "a.go", Line: 8, Snippet: "if  err != nil {"}
	b := a
	b.Line = 80
	b.Snippet = "\tif err != nil {  "

	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("line or whitespace changed fingerprint")
	}
}

func TestFingerprintSeparatesLengthAmbiguousInputs(t *testing.T) {
	a := Finding{Gate: "ab", RuleID: "c", File: "d", Snippet: "e"}
	b := Finding{Gate: "a", RuleID: "bc", File: "d", Snippet: "e"}
	if Fingerprint(a) == Fingerprint(b) { t.Fatal("ambiguous encoding") }
}

func TestGroupMakesEarliestLocationPrimary(t *testing.T) {
	input := []Finding{
		{Gate: "lint", Language: "go", RuleID: "tool/rule", Severity: Warning, File: "a.go", Line: 9, Snippet: "x()", Message: "bad"},
		{Gate: "lint", Language: "go", RuleID: "tool/rule", Severity: Warning, File: "a.go", Line: 3, Snippet: "x()", Message: "bad"},
	}
	got, err := Group(input)
	if err != nil { t.Fatal(err) }
	if got[0].Line != 3 || len(got[0].Occurrences) != 1 || got[0].Occurrences[0].Line != 9 {
		t.Fatalf("grouped = %#v", got)
	}
}
```

Add JSON assertions that `end_line` and `occurrences` are omitted when empty,
canonical severity values round-trip, duplicate locations do not duplicate an
occurrence, and ordering is file/line/gate/rule/fingerprint.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/finding -v`

Expected: FAIL because schema and functions do not exist.

- [ ] **Step 3: Implement the ADR-0005 schema**

```go
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
)

type Occurrence struct {
	Line    int `json:"line"`
	EndLine int `json:"end_line,omitempty"`
}

type Finding struct {
	Gate        string       `json:"gate"`
	Language    string       `json:"language"`
	RuleID      string       `json:"rule_id"`
	Severity    Severity     `json:"severity"`
	File        string       `json:"file"`
	Line        int          `json:"line"`
	EndLine     int          `json:"end_line,omitempty"`
	Snippet     string       `json:"snippet"`
	Occurrences []Occurrence `json:"occurrences,omitempty"`
	Message     string       `json:"message"`
	Fingerprint string       `json:"fingerprint"`
}

func Fingerprint(f Finding) string
func Validate(f Finding) error
func Group(input []Finding) ([]Finding, error)
```

Normalize snippet whitespace with `strings.Fields`, normalize file paths with
native `filepath.Clean` and `filepath.ToSlash`, and feed each fingerprint field
to SHA-256 as an 8-byte big-endian length followed by bytes. `Validate` rejects
malformed findings and stale supplied fingerprints. `Group` validates and
recomputes canonical fingerprints, sorts locations, and never mutates callers'
slices.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/finding -v && go test ./...`

Expected: all schema, hash, grouping, and full-suite tests pass.

```bash
git add internal/finding
git commit -m "Define stable normalized findings" \
  -m "Make line-independent fingerprints and grouped occurrences the durable identity consumed by later stalemate, ratchet, and waiver behavior."
```

### Task 5: Gate Data Model, Embedded Defaults, and Overrides

**Files:**

- Modify: `go.mod`, `go.sum`, `internal/gate/doc.go`
- Create: `internal/gate/{model,embed,load,template,version}.go`
- Create: `internal/gate/gate_test.go`
- Create: `internal/gate/defaults/gates/{lint,complexity}/gate.toml`
- Create: `internal/gate/defaults/gates/{lint,complexity}/go/binding.toml`

- [ ] **Step 1: Write failing loader and override tests**

```go
func TestLoadAllReadsEmbeddedGoBindings(t *testing.T) {
	got, err := (Loader{}).LoadAll()
	if err != nil { t.Fatal(err) }
	if names := gateNames(got); !slices.Equal(names, []string{"complexity", "lint"}) {
		t.Fatalf("names = %v", names)
	}
	if got[0].Bindings["go"].Normalizer == "" { t.Fatal("missing normalizer") }
}

func TestConfigDirectoryOverridesEmbeddedGateWholesale(t *testing.T) {
	overrides := t.TempDir()
	writeGateFixture(t, overrides, "lint", 7*time.Second, "fake-lint")

	got, err := (Loader{OverrideDir: overrides}).Load("lint")

	if err != nil { t.Fatal(err) }
	if got.Manifest.Timeout != 7*time.Second { t.Fatal(got.Manifest.Timeout) }
	if got.Bindings["go"].Tool != "fake-lint" { t.Fatal(got.Bindings["go"].Tool) }
}
```

Add table tests for unknown TOML fields, missing binding commands, invalid enum
values, invalid duration, an absent requested gate, template expansion with
settings, bad templates, version extraction, and `>=` version comparison.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/gate -v`

Expected: FAIL because loader/model APIs do not exist.

- [ ] **Step 3: Add TOML dependency and implement strict loading**

Add `github.com/pelletier/go-toml/v2 v2.4.3`. Use these core types:

```go
type Manifest struct {
	Name        string
	Description string
	CostClass   CostClass
	FixPolicy   FixPolicy
	Scope       Scope
	Blocking    []finding.Severity
	Timeout     time.Duration
}

type Version struct {
	Command    []string
	Pattern    string
	Constraint string
}

type CostClass string
type FixPolicy string
type Scope string

const (
	Instant CostClass = "instant"
	Fast    CostClass = "fast"
	Slow    CostClass = "slow"
	Glacial CostClass = "glacial"
)

type Binding struct {
	Language         string
	Tool             string
	Command          []string
	SuccessExitCodes []int
	Normalizer       string
	RuleID           string
	Message          string
	Settings         map[string]any
	SeverityMap      map[string]finding.Severity
	Version          Version
	Aliases          map[string]string
}

type Gate struct {
	Manifest Manifest
	Bindings map[string]Binding
}

type Loader struct { OverrideDir string }

func (l Loader) Load(name string) (Gate, error)
func (l Loader) LoadAll() ([]Gate, error)
func (b Binding) RenderCommand() ([]string, error)
func (v Version) Observe(raw string) (observed string, matches bool, err error)
```

Decode wire structs using `toml.NewDecoder(...).DisallowUnknownFields()`, then
validate and convert durations. `embed.go` owns:

```go
//go:embed defaults/gates
var shipped embed.FS
```

Embedded paths start at `defaults/gates`; override paths start directly at the
configured gates directory. Never merge an override with embedded files.

- [ ] **Step 4: Add the two shipped definitions as data**

Use `lint` and `complexity` manifests with `cost_class = "fast"`, blocking
error/warning, and a 60-second timeout. Both are `scope = "diff"`; phase 1 runs
them repo-wide because scope filtering does not exist yet. `lint` uses
`fix_policy = "autofix-then-llm"` and `complexity` uses
`fix_policy = "llm-fix"`, preserving their later-phase meaning. Bindings use:

```toml
language = "go"
tool = "golangci-lint"
command = ["golangci-lint", "run", "--output.json.path=stdout", "--show-stats=false", "./..."]
success_exit_codes = [0, 1]
normalizer = "golangci-json"

[severity_map]
error = "error"
warning = "warning"
default = "warning"

[version]
command = ["golangci-lint", "version"]
pattern = "v?(\\d+\\.\\d+\\.\\d+)"
constraint = ">=2.12.2"
```

```toml
language = "go"
tool = "gocyclo"
command = ["gocyclo", "-over", "{{.threshold}}", "."]
success_exit_codes = [0, 1]
normalizer = "regex:^(?P<value>\\d+) \\S+ (?P<symbol>\\S+) (?P<file>[^:]+):(?P<line>\\d+):\\d+$"
rule_id = "gocyclo/complexity"
message = "cyclomatic complexity {{.value}} in {{.symbol}}"

[settings]
threshold = 15

[severity_map]
default = "warning"
```

The official gocyclo CLI documents no version flag, so its optional `[version]`
block is deliberately absent. The installed binary is pinned operationally in
Task 11.

- [ ] **Step 5: Verify GREEN and commit**

Run: `go mod tidy && go test ./internal/gate -v && go test ./...`

Expected: embedded and override tests pass with no external tools installed.

```bash
git add go.mod go.sum internal/gate
git commit -m "Load gates from embedded TOML data" \
  -m "Ship useful defaults in the binary while allowing explicit XDG overrides to replace a gate without coupling gate additions to Go code."
```

### Task 6: Golden-Tested Normalizers

**Files:**

- Modify: `internal/normalizer/doc.go`
- Create: `internal/normalizer/{normalizer,golangci,regex}.go`
- Create: `internal/normalizer/normalizer_test.go`
- Create: `internal/normalizer/testdata/golangci/{output.raw,source.go,want.json}`
- Create: `internal/normalizer/testdata/gocyclo/{output.raw,source.go,want.json}`

- [ ] **Step 1: Record fixtures and write failing golden tests**

The golangci fixture contains two v2 JSON issues, including two identical
source lines at different locations. The gocyclo fixture follows the official
`complexity package function file:line:column` format. Copy fixture source into
a temporary repo root, normalize, group, JSON-indent, and compare to
`want.json`:

```go
func TestGoldenNormalizers(t *testing.T) {
	for _, tc := range []struct{ name, normalizer string }{
		{"golangci", "golangci-json"},
		{"gocyclo", `regex:^(?P<value>\d+) \S+ (?P<symbol>\S+) (?P<file>[^:]+):(?P<line>\d+):\d+$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, raw, want := goldenFixture(t, tc.name)
			got, err := Registry().Normalize(tc.normalizer, ctx, raw)
			if err != nil { t.Fatal(err) }
			grouped, err := finding.Group(got)
			if err != nil { t.Fatal(err) }
			assertGoldenJSON(t, grouped, want)
		})
	}
}
```

Add failing tests for malformed JSON, nonempty unparseable regex output,
missing required named captures, path traversal outside the repo root, missing
source files, and empty output yielding zero findings.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/normalizer -v`

Expected: FAIL because registry and parsers do not exist.

- [ ] **Step 3: Implement the compiled registry and parsers**

```go
type Context struct {
	Gate    string
	Root    string
	Binding gate.Binding
}

type Func func(Context, []byte) ([]finding.Finding, error)

type Registry map[string]Func

func NewRegistry() Registry
func (r Registry) Normalize(name string, ctx Context, raw []byte) ([]finding.Finding, error)
```

`NewRegistry` registers `golangci-json`; `Normalize` recognizes `regex:` and
compiles the suffix. The JSON parser decodes the documented envelope and the
subset of issue fields togi consumes, allowing unrelated fields for forward
compatibility, then explicitly validates `FromLinter`, `Text`, and
`Pos.Filename/Line`. The regex parser requires `file` and `line`; `value` and
`symbol` populate binding message templates. Both resolve slash-normalized
relative paths beneath `Context.Root`, read exactly the reported source line
for `Snippet`, apply severity maps, and return ungrouped findings with
fingerprints set.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/normalizer -v && go test ./...`

Expected: both golden comparisons and all malformed-output cases pass without
external binaries.

```bash
git add internal/normalizer
git commit -m "Normalize Go gate output into findings" \
  -m "Insulate downstream behavior from tool-specific JSON and text formats while retaining fixture-backed evidence for every parser contract."
```

### Task 7: No-op Enricher Seam

**Files:**

- Modify: `internal/enricher/doc.go`
- Create: `internal/enricher/enricher.go`
- Create: `internal/enricher/enricher_test.go`

- [ ] **Step 1: Write the failing preservation test**

```go
func TestNoopPreservesFindings(t *testing.T) {
	want := []finding.Finding{{Gate: "lint", File: "a.go", Line: 1}}
	got, err := (Noop{}).Enrich(context.Background(), Context{Root: "/repo", Language: "go"}, want)
	if err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(got, want) { t.Fatalf("got %#v, want %#v", got, want) }
}
```

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/enricher -v`

Expected: FAIL because `Noop` and `Context` do not exist.

- [ ] **Step 3: Implement the seam without phase 2 behavior**

```go
type Context struct {
	Root     string
	Language string
}

type Enricher interface {
	Enrich(context.Context, Context, []finding.Finding) ([]finding.Finding, error)
}

type Noop struct{}

func (Noop) Enrich(_ context.Context, _ Context, in []finding.Finding) ([]finding.Finding, error) {
	return in, nil
}
```

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/enricher -v && go test ./...`

Expected: preservation and full-suite tests pass.

```bash
git add internal/enricher
git commit -m "Place the enricher seam in the gate flow" \
  -m "Keep phase 1 behavior unchanged while giving phase 2 range enrichment a stable insertion point before collection."
```

### Task 8: Run Ledger, Lock, and Pruning

**Files:**

- Modify: `internal/run/doc.go`
- Create: `internal/run/{report,ledger,lock,lock_unix,lock_windows}.go`
- Create: `internal/run/ledger_test.go`

- [ ] **Step 1: Write failing ledger lifecycle tests**

Use a deterministic clock and random reader:

```go
func TestLedgerCreatesSortableRunAndAtomicReport(t *testing.T) {
	root := t.TempDir()
	l := Ledger{RepoState: root, Now: fixedNow, Random: bytes.NewReader([]byte{0xa3, 0xf1})}
	run, err := l.Start()
	if err != nil { t.Fatal(err) }
	defer run.Close()
	if got, want := filepath.Base(run.Dir), "20260821T151230Z-a3f1"; got != want { t.Fatalf("%q", got) }
	report := Report{SchemaVersion: 1, RunID: filepath.Base(run.Dir)}
	if err := run.WriteReport(report); err != nil { t.Fatal(err) }
	if _, err := os.Stat(filepath.Join(run.Dir, "report.json")); err != nil { t.Fatal(err) }
}
```

Add tests for O_EXCL lock contention, stale PID recovery, lock removal on
close, pruning 25 complete/incomplete run directories down to the newest 20,
latest selecting only a parseable complete report, raw filename sanitization,
and a 1 MiB capped raw file ending in `\n[togi: output truncated]\n`.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/run -run 'Ledger|Lock|Prune|Raw|Latest' -v`

Expected: FAIL because ledger/report types do not exist.

- [ ] **Step 3: Implement report and ledger contracts**

```go
type GateStatus string
const (
	GatePassed   GateStatus = "passed"
	GateFindings GateStatus = "findings"
	GateErrored  GateStatus = "errored"
)

type Verdict string
const (
	VerdictUnverified Verdict = "unverified"
	VerdictFindings Verdict = "findings"
	VerdictErrored  Verdict = "errored"
)

type GateReport struct {
	Gate            string            `json:"gate"`
	Language        string            `json:"language"`
	Status          GateStatus        `json:"status"`
	Findings        []finding.Finding `json:"findings,omitempty"`
	DurationMS      int64             `json:"duration_ms"`
	ObservedVersion string            `json:"observed_version,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	Error           string            `json:"error,omitempty"`
}

type Counts struct {
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Info        int `json:"info"`
	Occurrences int `json:"occurrences"`
}

type Report struct {
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	RepoID        string            `json:"repo_id"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Verdict       Verdict           `json:"verdict"`
	Gates         []GateReport      `json:"gates"`
	Findings      []finding.Finding `json:"findings"`
	Counts        Counts            `json:"counts"`
}

type Ledger struct {
	RepoState string
	Keep      int
	Now       func() time.Time
	Random    io.Reader
}

func (l Ledger) Start() (*RunLedger, error)
func (l Ledger) Latest() (Report, error)
func (r *RunLedger) WriteRaw(gate, language, stream string, raw []byte) error
func (r *RunLedger) WriteReport(Report) error
func (r *RunLedger) Close() error
```

Create the lock with `os.O_CREATE|os.O_EXCL`; store JSON containing PID and
start timestamp. Implement liveness in build-tagged files so Linux/macOS use
signal 0 and Windows uses `OpenProcess`. Only remove a stale lock after its PID
is confirmed absent. Prune before creating the new run. Write JSON through a
same-directory temporary file, `Sync`, `Close`, and `Rename`.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/run -run 'Ledger|Lock|Prune|Raw|Latest' -v && go test ./...`

Expected: ledger lifecycle tests pass, including stale/live lock distinction.

```bash
git add internal/run
git commit -m "Persist an exclusive external run ledger" \
  -m "Make reports durable and inspectable without writing target repositories, while bounding retained history and preventing concurrent runs for one repo."
```

### Task 9: Gate Executor and Concurrent Collector

**Files:**

- Create: `internal/run/executor.go`
- Create: `internal/run/collector.go`
- Create: `internal/run/executor_test.go`

- [ ] **Step 1: Write failing fake-process tests**

Build tiny Go helper binaries during tests rather than using shell scripts, so
tests remain cross-platform. Helpers select behavior from their executable
name or first argument. Cover: valid JSON/text, findings exit 1, crash exit 2,
missing executable, sleep past timeout, malformed output, >1 MiB output, and
one slow errored gate alongside one fast healthy gate.

The central regression test is:

```go
func TestCollectKeepsHealthyFindingsWhenSiblingErrors(t *testing.T) {
	requests := []Request{
		fakeRequest(t, "broken", "crash"),
		fakeRequest(t, "healthy", "gocyclo-findings"),
	}
	got := Collect(context.Background(), Executor{Registry: normalizer.NewRegistry(), Enricher: enricher.Noop{}}, requests, 2)

	if got[0].Status != GateErrored { t.Fatalf("broken = %#v", got[0]) }
	if got[1].Status != GateFindings || len(got[1].Findings) == 0 {
		t.Fatalf("healthy = %#v", got[1])
	}
}
```

Add a concurrency test using synchronized helpers and an atomic peak counter;
assert peak execution never exceeds the supplied limit and results remain in
gate/language order regardless of completion order.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/run -run 'Execute|Collect' -v`

Expected: FAIL because executor and collector APIs do not exist.

- [ ] **Step 3: Implement timeout-aware execution**

```go
type Request struct {
	Gate     gate.Gate
	Binding  gate.Binding
	Root     string
	RawStore interface {
		WriteRaw(gate, language, stream string, raw []byte) error
	}
}

type Executor struct {
	Registry normalizer.Registry
	Enricher enricher.Enricher
	Now      func() time.Time
}

func (e Executor) Execute(ctx context.Context, req Request) GateReport
func Collect(ctx context.Context, e Executor, requests []Request, limit int) []GateReport
```

Render arguments without a shell and invoke `exec.CommandContext` with
`Cmd.Dir = req.Root`. Capture stdout/stderr through capped writers, persist
both before parsing, and interpret only binding-declared success exit codes as
normalizable. Run the optional version command separately; extraction failures
become warnings in phase 1. Convert deadline expiry, persistence failure,
normalizer error, and enricher error to `GateErrored`. Never cancel sibling
requests because one result errored.

Use a fixed worker pool, collect every indexed result, group findings after
enrichment, and return request order. The caller computes global deterministic
ordering later.

- [ ] **Step 4: Verify GREEN, race behavior, and commit**

Run: `go test -race ./internal/run -run 'Execute|Collect' -v && go test ./...`

Expected: all executor/error/concurrency tests pass with no races and no real
gate tools installed.

```bash
git add internal/run
git commit -m "Execute gates concurrently without losing errors" \
  -m "Separate infrastructure failures from findings, persist raw diagnostics, and preserve every healthy gate's signal across the collection barrier."
```

### Task 10: Rendering, Typed Exits, and Orchestration

**Files:**

- Create: `internal/run/{render,exit,run}.go`
- Create: `internal/run/run_test.go`
- Modify: `cmd/togi/{main,command,command_test}.go`

- [ ] **Step 1: Write failing rendering and exit precedence tests**

```go
func TestRenderUsesCompilerStyleAndOccurrences(t *testing.T) {
	report := fixtureReportWithOccurrences()
	var out bytes.Buffer
	if err := Render(&out, report, RenderOptions{Color: false}); err != nil { t.Fatal(err) }
	want := "internal/gate/run.go:42: warning: cyclomatic complexity 18 (gocyclo/complexity)\n" +
		"    +2 more at lines 91, 104\n"
	if !strings.Contains(out.String(), want) { t.Fatalf("output:\n%s", out.String()) }
}

func TestExitCodePrecedence(t *testing.T) {
	for _, tc := range []struct{ verdict Verdict; want int }{
		{VerdictUnverified, 5}, {VerdictFindings, 1}, {VerdictErrored, 4},
	} {
		if got := ExitCode(tc.verdict); got != tc.want { t.Fatalf("%s = %d", tc.verdict, got) }
	}
}
```

Add tests that counts include occurrences, output sorts by file/line, errored
gate summaries are loud, `--no-color` disables ANSI, `status` reads rather than
executes, `--gate` filters by exact name, and errored plus findings exits 4.
Before any orchestration code exists, add the hermetic phase-level regression:

```go
func TestReportOnlyRunIsStableAndKeepsErroredGateSeparate(t *testing.T) {
	first := runFixtureGauntlet(t, fixtureOptions{brokenLint: false})
	second := runFixtureGauntlet(t, fixtureOptions{brokenLint: false})
	if diff := cmpFingerprintSets(first.Findings, second.Findings); diff != "" {
		t.Fatal(diff)
	}

	broken := runFixtureGauntlet(t, fixtureOptions{brokenLint: true})
	if gateStatus(broken, "lint") != GateErrored { t.Fatal("lint was not errored") }
	if countGateFindings(broken, "complexity") == 0 { t.Fatal("healthy findings lost") }
	assertNoTogiFilesInRepo(t)
}
```

The fixture creates a real temporary Git repository, a temporary XDG tree,
and wholesale overrides for both shipped gates pointing at the compiled helper
binary from Task 9. Assert raw output exists under XDG state, `report.json`
round-trips, two runs have different IDs but identical fingerprint sets, and
the target repository tree is unchanged except for its original fixture files.
Implement `cmpFingerprintSets` locally with sorted strings; do not add a test
dependency.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./internal/run ./cmd/togi -run 'Render|Exit|Run|Status|Gate' -v`

Expected: FAIL because rendering/orchestration APIs and subcommands are absent.

- [ ] **Step 3: Implement runner orchestration**

```go
type Options struct {
	Root       string
	GateNames  []string
	ReportOnly bool
	Verbose    bool
	NoColor    bool
}

type Service struct {
	Paths    config.Paths
	Loader   gate.Loader
	Executor Executor
	Stdout   io.Writer
	Now      func() time.Time
	Random   io.Reader
}

func (s Service) Run(ctx context.Context, opts Options) (Report, error)
func (s Service) Status(ctx context.Context, root string, noColor bool) (Report, error)

type ExitError struct { Code int; Err error }
func (e *ExitError) Error() string
func (e *ExitError) Unwrap() error
func ExitCode(Verdict) int

type RenderOptions struct { Color bool }
func Render(io.Writer, Report, RenderOptions) error
```

`Run` resolves repo-id, creates the repo ledger, loads gates, selects Go
bindings, collects results with `min(runtime.NumCPU(), 4)`, flattens and groups
findings, calculates counts/verdict, writes `report.json`, then renders it.
Always close the ledger and preserve the primary error. `Status` resolves the
same repo state, reads the newest complete report, and renders it.

- [ ] **Step 4: Wire the thin Cobra commands**

Add flags exactly as documented:

```go
type runFlags struct {
	reportOnly bool
	base       string
	gates      []string
	verbose    bool
	noColor    bool
}
```

`--base` is accepted for command-surface stability but returns a clear error if
set in phase 1 because diff scoping does not exist yet; never silently ignore
it. Repeated `--gate` filters gates. `main` maps `*run.ExitError` to its code
and all other errors to 70. Tests call commands directly and assert returned
typed errors rather than spawning the process.

- [ ] **Step 5: Verify GREEN and commit**

Run: `go test ./internal/run ./cmd/togi -v && go test ./... && go build ./...`

Expected: CLI integration, rendering, typed exit, full suite, and build pass.

```bash
git add internal/run cmd/togi
git commit -m "Report complete report-only gauntlet runs" \
  -m "Tie identity, gate data, normalization, collection, and the ledger into a stable CLI contract with compiler-style output and machine-meaningful exits."
```

### Task 11: Install, Dogfood, and Record Phase 1 Verification

**Files:**

- Modify as findings require: phase 1 Go files only
- Modify if verified CLI differs: `internal/gate/defaults/gates/*/go/binding.toml`
- Modify with regression fixtures if output differs: `internal/normalizer/testdata/**`

- [ ] **Step 1: Run all hermetic verification before installing tools**

Run:

```bash
go build ./...
go test -race ./...
```

Expected: build succeeds and every test passes while `golangci-lint` and
`gocyclo` are still absent from `PATH`.

- [ ] **Step 2: Install the user-approved pinned global binaries**

Run:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0
```

Then ensure `$(go env GOPATH)/bin` is on the task command's `PATH` and run:

```bash
golangci-lint version
go version -m "$(go env GOPATH)/bin/gocyclo"
golangci-lint run --help
```

Expected: golangci-lint matches its binding, gocyclo installs at the pinned
module version, and help contains `--output.json.path stdout`. Do not add an
undocumented gocyclo version command to its binding.

- [ ] **Step 3: Capture real raw formats and re-run golden tests**

Run both binding commands on the repository. Compare their raw output shapes
to recorded fixtures. If either shape differs, first replace the recorded raw
fixture with a minimal representative excerpt and run the golden test to watch
it fail; then correct the compiled normalizer and expected JSON until it passes.

Run: `go test ./internal/normalizer -v && go test ./...`

Expected: fixtures reflect installed tool formats and all tests pass.

- [ ] **Step 4: Dogfood twice and compare fingerprints**

Run:

```bash
go run ./cmd/togi run --report-only
go run ./cmd/togi run --report-only
```

Exit 1 is expected when findings exist. Locate the two latest reports beneath
`$XDG_STATE_HOME/togi/<repo-id>/runs/`, extract sorted fingerprints using a
small read-only Go helper or `jq` if already installed, and compare them.

Expected: both gates appear in `report.json`; both runs have byte-identical
sorted `(fingerprint, occurrence-count)` sets. Preserve raw outputs only in the
external run ledger.

- [ ] **Step 5: Prove the broken-gate channel with an XDG override**

Create a temporary XDG config directory containing a wholesale `lint`
override whose command names a nonexistent binary. Point only this invocation
at the temporary config and run `togi`; do not edit the target repository.

Expected: exit 4, lint status `errored`, complexity still runs, its findings
remain in the report, and both raw streams are persisted where applicable.

- [ ] **Step 6: Fix dogfood findings test-first**

For each legitimate finding, write or strengthen the narrowest package test
that exposes the underlying behavior, verify it fails, make the smallest
correction, and re-run the package test. Do not suppress findings or lower
standards merely to make dogfood green.

- [ ] **Step 7: Run final verification**

Run:

```bash
go build ./...
go test -race ./...
go run ./cmd/togi run --report-only
```

Expected: build and tests pass; the report-only command emits and persists a
valid report. Its exit is 5 when no findings remain because phase 1 cannot
establish merge-ready, 1 for legitimate remaining findings, or 4 only if a real
gate infrastructure failure remains and is reported explicitly.

- [ ] **Step 8: Commit verified dogfood corrections**

```bash
git add cmd internal go.mod go.sum
git commit -m "Dogfood the phase 1 gauntlet" \
  -m "Verify both real Go gate formats, stable fingerprinting across repeated runs, and the independent infrastructure-error channel before declaring the runner usable."
```

Do not commit anything under XDG state/config/cache. Confirm `git status` shows
only intentional repository files before committing.
