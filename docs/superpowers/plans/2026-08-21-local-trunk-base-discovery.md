# Local Trunk Base Discovery Implementation Plan

> **Status: Complete.** Conventional local trunk discovery is implemented.
> This plan remains as a historical execution record; unchecked boxes preserve
> the prescribed sequence and do not identify outstanding work.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a completed feature worktree infer a conventional `main` or `master` comparison base without remote configuration or network access.

**Architecture:** `internal/run` keeps base selection beside committed diff resolution. A focused selector preserves explicit and `origin/HEAD` precedence, enumerates only four conventional fallback refs with sanitized Git plumbing, and returns the first existing candidate to the existing commit and merge-base validation path.

**Tech Stack:** Go 1.25, Go standard library, Git plumbing commands, real temporary Git repositories.

---

## File Map

- `internal/run/diff.go`: select the requested, symbolic, remote-tracking, or local trunk ref.
- `internal/run/diff_test.go`: prove candidate order and branch-level diff semantics.
- `internal/run/run_test.go`: prove local fallback at the service/report boundary and side-effect-free exhaustion.

### Task 1: Select Conventional Trunk Refs

**Files:**
- Modify: `internal/run/diff.go`
- Modify: `internal/run/diff_test.go`
- Modify: `internal/run/run_test.go`

- [ ] **Step 1: Write failing resolver tests**

Add table-driven real-Git coverage with these exact fallback candidates:

```go
tests := []struct {
	name string
	ref  string
	want string
}{
	{name: "remote main", ref: "refs/remotes/origin/main", want: "origin/main"},
	{name: "remote master", ref: "refs/remotes/origin/master", want: "origin/master"},
	{name: "local main", ref: "refs/heads/main", want: "main"},
	{name: "local master", ref: "refs/heads/master", want: "master"},
}
```

Also prove explicit `--base` beats every candidate, symbolic `origin/HEAD`
beats `origin/main`, `origin/main` beats stale local `main`, a remote-free
linked worktree includes two feature commits, a simulated rebase advances the
merge-base, running on trunk returns an empty nonnil line map, and absence of
all candidates names `main`, `master`, and `--base` in the error.

At the service boundary, create a fixture with no remote refs, local `main`
fixed at the base, and multiple commits on `feature`. Run `Service.Run` without
`Options.Base` and assert the report records local `main`, its commit, the
merge-base, and feature `HEAD`. Make the existing missing-default case remove
every conventional ref while retaining its no-helper and no-ledger assertions.

```go
if report.Diff.BaseRef != "main" || report.Diff.BaseCommit != base {
	t.Fatalf("diff base = %#v, want local main at %s", report.Diff, base)
}
if report.Diff.MergeBase != base || report.Diff.Head != head {
	t.Fatalf("diff commits = %#v", report.Diff)
}
```

- [ ] **Step 2: Run the focused tests and record RED**

```sh
go test ./internal/run -run 'TestResolveDiff(DetectsConventional|Prefers|UsesRemoteFree|MovesMergeBase|OnTrunk|RequiresBase)' -count=1
go test ./internal/run -run 'TestService(ResolvesLocalTrunkWithoutRemote|RejectsInvalidDiffInputsBeforeLedgerOrGates)' -count=1
```

Expected: fallback cases fail with the current `origin/HEAD is unavailable`
diagnostic. Correct test setup errors until the failure is behavioral.

- [ ] **Step 3: Implement one base-selection helper**

Replace inline discovery with:

```go
func resolveDiffBaseRef(ctx context.Context, root, requested string) (string, error)
```

It validates and returns an explicit base first. Otherwise it tries sanitized
`git symbolic-ref --quiet --short refs/remotes/origin/HEAD`, treating only exit
status 1 as missing and preserving cancellation or other failures. It then
enumerates exact refs with bounded, sanitized plumbing:

```text
git for-each-ref --format=%(refname) refs/remotes/origin/main refs/remotes/origin/master refs/heads/main refs/heads/master
```

Parse complete UTF-8 lines, reject malformed or unexpected output, and choose
from this fixed order:

```go
var automaticBaseRefs = []struct {
	full  string
	short string
}{
	{"refs/remotes/origin/main", "origin/main"},
	{"refs/remotes/origin/master", "origin/master"},
	{"refs/heads/main", "main"},
	{"refs/heads/master", "master"},
}
```

If none exists, return `no base was provided and no main or master trunk is
available; pass --base`. Leave `resolveDiffCommit` as the single commit
verification path, so a present non-commit candidate fails rather than falling
through. Introduce no `fetch`, `ls-remote`, or other network-facing command.

- [ ] **Step 4: Run focused and package tests GREEN**

```sh
go test ./internal/run -run 'TestResolveDiff(DetectsConventional|Prefers|UsesRemoteFree|MovesMergeBase|OnTrunk|RequiresBase)' -count=1
go test ./internal/run -run 'TestService(ResolvesExplicitAndDefaultBase|ResolvesLocalTrunkWithoutRemote|RejectsInvalidDiffInputsBeforeLedgerOrGates)' -count=1
gofmt -w internal/run/diff.go internal/run/diff_test.go internal/run/run_test.go
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
git diff --check
```

Expected: PASS.

- [ ] **Step 5: Commit resolver behavior**

```sh
git add internal/run/diff.go internal/run/diff_test.go internal/run/run_test.go
git commit -m "Discover conventional local trunks" -m "Judge complete feature branches without requiring remote metadata while preserving explicit and PR-target precedence."
```

- [ ] **Step 6: Dogfood remote-free discovery**

From the clean committed feature worktree, use isolated XDG directories and no
`--base`:

```sh
tmp=$(mktemp -d)
XDG_CONFIG_HOME="$tmp/config" XDG_STATE_HOME="$tmp/state" \
XDG_CACHE_HOME="$tmp/cache" XDG_DATA_HOME="$tmp/data" \
go run ./cmd/togi run --report-only
```

Expected: `BaseRef` is `main`. Missing external gate binaries may yield verdict
`errored` and exit 4, but the schema-version-2 report must contain exact base,
merge-base, and HEAD metadata.
