# Phase 2 Diff Scoping Implementation Plan

> **Status: Complete.** Phase 2 diff scoping and Go range enrichment are
> implemented. This plan remains as a historical execution record; unchecked
> boxes preserve the prescribed sequence and do not identify outstanding work.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Restrict report-only gate results to the committed feature diff while expanding structural Go findings to their enclosing declarations.

**Architecture:** internal/run resolves a clean committed Git snapshot and changed-line ranges before ledger creation. Gate data declares point or entity semantics; internal/enricher derives Go declaration ranges, and internal/finding filters locations before grouping. Reports move directly to schema version 2 and persist the Git inputs that defined scope.

**Tech Stack:** Go 1.25, Cobra, pelletier/go-toml/v2, Go standard library go/ast, go/parser, go/token, os/exec, and real temporary Git repositories in tests.

---

## Execution Precondition

The working tree currently contains unrelated wiki implementation changes,
including edits to cmd/togi/command.go. Before executing this plan, commit that
work separately or use a worktree based on a commit containing it. Never stage,
reset, overwrite, or fold those changes into a Phase 2 commit.

## File Map

- internal/gate/model.go and load.go: point/entity location mode.
- internal/finding/scope.go: changed-line ranges and touched-location filtering.
- internal/enricher/go.go: Go AST declaration enrichment.
- internal/run/diff.go: clean-tree checks, Git resolution, and hunk parsing.
- internal/run/executor.go: enrichment and filtering before grouping.
- internal/run/report.go and ledger.go: schema-v2 metadata and validation.
- internal/run/run.go: scope orchestration before ledger creation.
- cmd/togi/command.go: base flag and production Go enricher wiring.

### Task 1: Declare Gate Location Semantics

**Files:**
- Modify: internal/gate/model.go
- Modify: internal/gate/load.go
- Modify: internal/gate/gate_test.go
- Modify: internal/gate/defaults/gates/complexity/gate.toml
- Modify: internal/gate/defaults/gates/lint/gate.toml

- [ ] **Step 1: Write the failing loader tests**

Extend the embedded-gate test with:

~~~go
if got := gates[0].Manifest.Location; got != EntityLocation {
    t.Fatalf("complexity location = %q, want %q", got, EntityLocation)
}
if got := gates[1].Manifest.Location; got != PointLocation {
    t.Fatalf("lint location = %q, want %q", got, PointLocation)
}
~~~

Add TestManifestLocation with omitted, point, entity, and invalid fixture
values. Omitted and point must yield PointLocation; entity must yield
EntityLocation; invalid must report invalid location.

- [ ] **Step 2: Verify the test is red**

Run: go test ./internal/gate -run 'TestLoadAllReadsEmbeddedGoBindings|TestManifestLocation'

Expected: compilation fails because Manifest.Location is undefined.

- [ ] **Step 3: Implement strict location decoding**

Add:

~~~go
type Location string

const (
    PointLocation  Location = "point"
    EntityLocation Location = "entity"
)
~~~

Add Location to Manifest and location to manifestWire. In toManifest, default
empty to PointLocation, accept only the two constants, and reject everything
else. Add location = "entity" to complexity and location = "point" to lint.

- [ ] **Step 4: Verify and commit**

Run: go test ./internal/gate

Expected: PASS.

~~~bash
git add internal/gate/model.go internal/gate/load.go internal/gate/gate_test.go internal/gate/defaults/gates/complexity/gate.toml internal/gate/defaults/gates/lint/gate.toml
git commit -m "Declare gate finding locations" -m "Keep structural range intent in gate data so point findings are not accidentally widened during diff scoping."
~~~

### Task 2: Filter Findings by Touched Lines

**Files:**
- Create: internal/finding/scope.go
- Create: internal/finding/scope_test.go

- [ ] **Step 1: Write the failing scope tests**

Define tests against:

~~~go
type LineRange struct {
    Start int
    End   int
}

type ChangedLines map[string][]LineRange

func FilterTouched([]Finding, ChangedLines) ([]Finding, error)
~~~

Use a finding with primary 10-20 and occurrences 30 and 40-50. Assert:
a change at 15 keeps only the primary; 21 removes a point at 10; 42 promotes
40-50; 32-45 keeps and sorts both occurrences; a different file removes the
finding. Also test slash-normalized paths, invalid ranges, unchanged input
storage, and unchanged fingerprints after promotion.

- [ ] **Step 2: Verify the test is red**

Run: go test ./internal/finding -run TestFilterTouched

Expected: compilation fails because the API is undefined.

- [ ] **Step 3: Implement non-mutating filtering**

Implement FilterTouched by validating every changed range, cloning each
finding, flattening primary plus occurrences, retaining locations whose
inclusive ranges overlap a changed range, sorting survivors with the existing
occurrence ordering, and promoting the earliest survivor. Treat EndLine zero
as Line. Use normalizeFile for lookup and return an empty slice only when no
location survives.

Core overlap rule:

~~~go
func locationTouches(location Occurrence, changed LineRange) bool {
    end := location.EndLine
    if end == 0 {
        end = location.Line
    }
    return location.Line <= changed.End && end >= changed.Start
}
~~~

- [ ] **Step 4: Verify and commit**

Run: go test ./internal/finding

Expected: PASS.

~~~bash
git add internal/finding/scope.go internal/finding/scope_test.go
git commit -m "Filter findings to touched locations" -m "Filter every occurrence before regrouping so a surviving site can become primary without changing fingerprint identity."
~~~

### Task 3: Enrich Go Entity Findings

**Files:**
- Modify: internal/enricher/enricher.go
- Modify: internal/enricher/enricher_test.go
- Create: internal/enricher/go.go
- Create: internal/enricher/go_test.go

- [ ] **Step 1: Write failing Go enrichment tests**

Create sample.go with a type, function, and method. Call:

~~~go
ctx := Context{
    Root: root, Language: "go", Location: gate.EntityLocation,
}
got, err := (Go{}).Enrich(context.Background(), ctx, findings)
~~~

Assert primary and occurrence EndLine values equal the smallest enclosing
ast.Decl end. Assert one parse per file, a package-clause finding remains a
point, point mode performs no reads, input is unchanged, invalid Go errors,
traversal paths fail, canceled context fails, and an unsupported entity
language errors.

- [ ] **Step 2: Verify the test is red**

Run: go test ./internal/enricher -run 'TestGo|TestNoop'

Expected: compilation fails because Go and Context.Location are undefined.

- [ ] **Step 3: Implement rooted, cached AST enrichment**

Add gate.Location to Context. Implement Go with os.OpenRoot, Root.ReadFile,
token.NewFileSet, and parser.ParseFile. Cache parsed files by normalized
filename. For each zero-EndLine primary and occurrence, choose the containing
declaration with the shortest inclusive line range. Preserve existing ranges.
Check context cancellation before each read. Keep Noop and its no-allocation
contract intact.

- [ ] **Step 4: Verify and commit**

Run: go test ./internal/enricher

Expected: PASS.

~~~bash
git add internal/enricher/enricher.go internal/enricher/enricher_test.go internal/enricher/go.go internal/enricher/go_test.go
git commit -m "Enrich Go declaration findings" -m "Derive structural ranges from the committed source tree while leaving line-level findings as points."
~~~

### Task 4: Resolve the Committed Git Diff

**Files:**
- Create: internal/run/diff.go
- Create: internal/run/diff_test.go

- [ ] **Step 1: Write failing real-Git tests**

Test this contract using repositories under t.TempDir:

~~~go
type Diff struct {
    BaseRef      string
    BaseCommit   string
    MergeBase    string
    Head         string
    ChangedFiles int
    ChangedLines int
    Lines        finding.ChangedLines
}

func resolveDiff(context.Context, string, string) (Diff, error)
~~~

Cover detected origin/main, explicit base precedence, missing origin/HEAD,
invalid base, an option-looking base, unrelated histories, additions, edits,
overlapping hunk normalization, pure-deletion anchors at the next or final
line, empty/deleted files, renames, spaces and tabs in paths, binary files, and
cancellation. Independently assert staged, unstaged, and untracked states are
rejected.

- [ ] **Step 2: Verify the test is red**

Run: go test ./internal/run -run 'TestResolveDiff|TestRequireCleanWorktree'

Expected: compilation fails because Diff and resolveDiff are undefined.

- [ ] **Step 3: Implement Git plumbing**

Run commands directly with exec.CommandContext and cmd.Dir:

~~~text
git status --porcelain=v1 -z --untracked-files=all
git rev-parse --verify HEAD^{commit}
git symbolic-ref --quiet --short refs/remotes/origin/HEAD
git rev-parse --verify --end-of-options BASE^{commit}
git merge-base HEAD-OID BASE-OID
git diff --name-only --diff-filter=ACMRTUXB --no-ext-diff --find-renames -z MERGE HEAD-OID --
git diff --unified=0 --no-color --no-ext-diff --find-renames MERGE HEAD-OID -- PATH
~~~

Reject any nonempty status without echoing raw output. Validate object IDs as
lowercase 40- or 64-character hexadecimal strings. Parse new-side hunk ranges
with:

~~~go
var hunkPattern = regexp.MustCompile(
    `^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`,
)
~~~

An omitted count means one. A zero count anchors at the new start if it exists,
otherwise the current file's final line; an empty file adds no anchor.
Normalize destination paths and merge adjacent or overlapping ranges before
counting them.

- [ ] **Step 4: Verify and commit**

Run: go test ./internal/run -run 'TestResolveDiff|TestRequireCleanWorktree'

Expected: PASS.

~~~bash
git add internal/run/diff.go internal/run/diff_test.go
git commit -m "Resolve committed diff scope" -m "Require a clean worktree and derive changed lines from verified Git objects so gate input and scope describe the same snapshot."
~~~

### Task 5: Insert Scope Filtering into Gate Execution

**Files:**
- Modify: internal/run/executor.go
- Modify: internal/run/executor_test.go

- [ ] **Step 1: Write failing executor tests**

Add ChangedLines finding.ChangedLines to Request in test expectations. Prove:
entity mode reaches the enricher; diff gates filter after enrichment and before
grouping; repo gates bypass filtering; an empty nonnil map removes all
diff-scoped findings; nil scope on a diff gate is invalid wiring; filtering
errors become GateErrored; and one errored request does not suppress siblings.

- [ ] **Step 2: Verify the test is red**

Run: go test ./internal/run -run 'TestExecute.*Scope|TestCollect.*Sibling'

Expected: compilation fails because Request.ChangedLines is undefined.

- [ ] **Step 3: Implement the pipeline stage**

Pass Manifest.Location through enricher.Context. After enrichment:

~~~go
scoped := enriched
if req.Gate.Manifest.Scope == gate.Diff {
    scoped, err = finding.FilterTouched(enriched, req.ChangedLines)
    if err != nil {
        return errored(report, errors.New(
            "scope findings: invalid changed-line scope",
        ))
    }
}
grouped, err := finding.Group(scoped)
~~~

Require nonnil ChangedLines only for diff gates. Preserve raw-output redaction.

- [ ] **Step 4: Verify and commit**

Run: go test -race ./internal/run -run 'TestExecute|TestCollect'

Expected: PASS.

~~~bash
git add internal/run/executor.go internal/run/executor_test.go
git commit -m "Scope gate findings before collection" -m "Apply gate-specific location semantics at the normalize-enrich-group boundary without coupling sibling gate outcomes."
~~~

### Task 6: Publish Schema-Version-2 Diff Metadata

**Files:**
- Modify: internal/run/report.go
- Modify: internal/run/run.go
- Modify: internal/run/ledger.go
- Modify: internal/run/ledger_test.go
- Modify: internal/run/run_test.go

- [ ] **Step 1: Write failing report tests**

Add this required report member:

~~~go
type DiffReport struct {
    BaseRef      string `json:"base_ref"`
    BaseCommit   string `json:"base_commit"`
    MergeBase    string `json:"merge_base"`
    Head         string `json:"head"`
    ChangedFiles int    `json:"changed_files"`
    ChangedLines int    `json:"changed_lines"`
}
~~~

Assert buildReport emits schema 2. Make schemas 1 and 3 invalid. Add tamper
cases for empty base ref, malformed object IDs, negative counts, and positive
line count with zero files. Assert Latest skips schema-1 reports.

- [ ] **Step 2: Verify the tests are red**

Run: go test ./internal/run -run 'TestWriteReport|TestLatest|TestBuildReport'

Expected: reports still use schema 1 and lack diff metadata.

- [ ] **Step 3: Implement schema v2 only**

Add Diff DiffReport to Report. Pass Diff into buildReport and set schema 2.
Validate exactly version 2, nonempty BaseRef, valid commit IDs, nonnegative
counts, and count consistency. Update every complete ledger fixture with
stable 40-character IDs. Do not add a v1 compatibility path.

- [ ] **Step 4: Verify and commit**

Run: go test ./internal/run -run 'TestWriteReport|TestLatest|TestBuildReport|TestServicePersists'

Expected: PASS.

~~~bash
git add internal/run/report.go internal/run/run.go internal/run/ledger.go internal/run/ledger_test.go internal/run/run_test.go
git commit -m "Record diff scope in reports" -m "Make schema version 2 explain the exact commits and changed-line totals used to judge a run without retaining pre-release compatibility."
~~~

### Task 7: Orchestrate Diff Scope and Activate Base Selection

**Files:**
- Modify: internal/run/run.go
- Modify: internal/run/run_test.go
- Modify: cmd/togi/command.go
- Modify: cmd/togi/command_test.go

- [ ] **Step 1: Write failing service and CLI tests**

Replace the Phase 1 rejection with:

~~~go
func TestRunCommandPassesBase(t *testing.T) {
    service := &fakeService{}
    cmd := newRootCommandWithService(
        streams{out: io.Discard, err: io.Discard}, service,
    )
    cmd.SetArgs([]string{"run", "--base", "origin/main"})
    if err := cmd.Execute(); err != nil {
        t.Fatal(err)
    }
    if service.runOptions.Base != "origin/main" {
        t.Fatalf("base = %q", service.runOptions.Base)
    }
}
~~~

Add service tests proving dirty or unresolved-base failures create no state and
execute no helper. Add an end-to-end committed feature fixture: complexity is
entity/diff and reports an unchanged function signature; lint is point/diff
and reports an unchanged line; a function-body edit keeps complexity and drops
lint; a repo-scoped finding survives. Repeated runs must retain fingerprints
and exact Git metadata.

- [ ] **Step 2: Verify the tests are red**

Run: go test ./cmd/togi ./internal/run -run 'TestRunCommandPassesBase|TestService.*Diff|TestService.*Dirty'

Expected: Options.Base and service scope orchestration are absent.

- [ ] **Step 3: Wire preparation before ledger creation**

Add Base string to Options and use:

~~~go
type preparedRun struct {
    repository repoid.ID
    repoState  string
    diff       Diff
    requests   []Request
}
~~~

The fixed order is platform and wiring validation, repository identity,
external-state validation, resolveDiff, gate loading, ledger start, execution.
Copy the same nonnil Diff.Lines into diff-scoped requests and pass Diff to
buildReport.

In cmd/togi/command.go, remove the Phase 1 base rejection, pass flags.base, and
wire enricher.Go instead of Noop. Preserve every existing wiki command and
service seam. Update temp-repo fixtures to create refs/remotes/origin/main and
symbolic refs/remotes/origin/HEAD without network access.

- [ ] **Step 4: Verify and commit**

Run: go test -race ./...

Expected: PASS.

Run: go build ./...

Expected: PASS.

Before staging, inspect cmd/togi/command.go and stop if unrelated wiki changes
are not already present in the execution branch base.

~~~bash
git add internal/run/run.go internal/run/run_test.go cmd/togi/command.go cmd/togi/command_test.go
git commit -m "Judge committed feature diffs" -m "Resolve scope before ledger creation and expose explicit base selection while preserving report-only execution semantics."
~~~

### Task 8: Verify Phase 2 End to End

**Files:**
- Modify only for a focused defect discovered by verification.

- [ ] **Step 1: Run formatting and full verification**

~~~bash
gofmt -w internal/gate internal/finding internal/enricher internal/run cmd/togi
go test -race ./...
go vet ./...
go build ./...
~~~

Expected: every command exits 0. Inspect formatting changes and exclude
unrelated work.

- [ ] **Step 2: Repeat the no-tool integration contract**

Run: go test ./internal/run -run TestServiceScopesCommittedDiff -count=2

Expected: PASS twice with stable fingerprint assertions.

- [ ] **Step 3: Dogfood a clean committed Phase 2 branch**

Choose the commit immediately before the Phase 2 implementation as BASE:

~~~bash
go run ./cmd/togi run --report-only --base BASE
~~~

Expected: exit 1, 4, or 5 according to installed tools and a schema-v2 report.
If both real tools are installed, neither gate errors. Confirm head, base,
merge-base, changed-file count, and changed-line count against Git.

- [ ] **Step 4: Verify the clean-tree guard safely**

Use isolated temporary repositories for staged and unstaged checks. For the
main checkout, create only phase2-clean-tree-check.tmp, verify the run exits 70
before creating a run directory, then remove only
phase2-clean-tree-check.tmp. Never reset or clean the user's checkout.

- [ ] **Step 5: Commit only if dogfood required a correction**

~~~bash
git commit -m "Complete phase 2 diff scoping" -m "Resolve the focused behavior found by end-to-end dogfood verification."
~~~

Do not create an empty commit.
