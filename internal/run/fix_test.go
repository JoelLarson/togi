//go:build linux

package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/adapter"
	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/flywheel"
	"github.com/joellarson/togi/internal/gate/gatetest"
	"github.com/joellarson/togi/internal/runner"
)

type stubSuite struct {
	run func(context.Context, string, []string, bool) (SuiteResult, error)
}

func (suite stubSuite) Run(ctx context.Context, root string, packages []string, full bool) (SuiteResult, error) {
	return suite.run(ctx, root, packages, full)
}

type stubAdapter struct{}

func (stubAdapter) Name() string { return "codex" }
func (stubAdapter) Run(context.Context, adapter.Request) (adapter.Result, error) {
	return adapter.Result{}, nil
}

type stubWorkspace struct {
	root            string
	cleanup         func()
	snapshot        func() (flywheel.ValidatedTree, error)
	snapshotContext func(context.Context) (flywheel.ValidatedTree, error)
	squash          string
	landStatus      flywheel.LandingStatus
	landErr         error
	cleanupErr      error
	squashTree      flywheel.ValidatedTree
	squashCtx       context.Context
	landCtx         context.Context
	onSquash        func(context.Context) error
}

type stubValidatedTree struct {
	root      string
	verifyErr error
	closeErr  error
	verify    func(context.Context) error
	onVerify  func()
	onClose   func()
}

func (tree *stubValidatedTree) Root() string { return tree.root }
func (tree *stubValidatedTree) Verify(ctx context.Context) error {
	if tree.verify != nil {
		return tree.verify(ctx)
	}
	if tree.onVerify != nil {
		tree.onVerify()
	}
	return tree.verifyErr
}
func (tree *stubValidatedTree) Close() error {
	if tree.onClose != nil {
		tree.onClose()
	}
	return tree.closeErr
}

func (workspace *stubWorkspace) Root() string {
	if workspace.root != "" {
		return workspace.root
	}
	return "/owned/worktree"
}
func (workspace *stubWorkspace) SnapshotValidated(ctx context.Context) (flywheel.ValidatedTree, error) {
	if workspace.snapshotContext != nil {
		return workspace.snapshotContext(ctx)
	}
	if workspace.snapshot == nil {
		return nil, errors.New("validated snapshot unavailable")
	}
	return workspace.snapshot()
}

func TestFinalSnapshotWallClockExhaustionIsRailsAndCleansWorkspace(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Now = time.Now
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	cleanupCalls := 0
	workspace := &stubWorkspace{cleanup: func() { cleanupCalls++ }, snapshotContext: func(ctx context.Context) (flywheel.ValidatedTree, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) { return workspace, nil }
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	parent, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	report, err := service.Run(parent, Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: 25 * time.Millisecond, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || report.Verdict != VerdictRails || report.Fix == nil || report.Fix.Final != nil {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	if cleanupCalls != 1 || workspace.squashCtx != nil || workspace.landCtx != nil {
		t.Fatalf("cleanup=%d squash=%#v land=%#v", cleanupCalls, workspace.squashCtx, workspace.landCtx)
	}
}
func (*stubWorkspace) SnapshotGitState(context.Context) (flywheel.GitState, error) {
	return flywheel.GitState{}, nil
}
func (*stubWorkspace) CheckGitState(context.Context, flywheel.GitState) error { return nil }
func (*stubWorkspace) ChangedFiles(context.Context) ([]string, error)         { return nil, nil }
func (*stubWorkspace) PrepareBatch(context.Context, []string) (flywheel.BatchProof, error) {
	return flywheel.BatchProof{}, nil
}
func (*stubWorkspace) VerifyBatch(context.Context, flywheel.BatchProof) error { return nil }
func (*stubWorkspace) ResetAttempt(context.Context) error                     { return nil }
func (*stubWorkspace) CommitBatch(context.Context, string, flywheel.BatchProof) (string, error) {
	return "", nil
}
func (*stubWorkspace) RollbackBatch(context.Context, string) error { return nil }
func (workspace *stubWorkspace) Squash(context.Context) (string, error) {
	return workspace.squash, nil
}
func (workspace *stubWorkspace) SquashValidated(ctx context.Context, tree flywheel.ValidatedTree) (string, error) {
	workspace.squashTree = tree
	workspace.squashCtx = ctx
	if workspace.onSquash != nil {
		if err := workspace.onSquash(ctx); err != nil {
			return "", err
		}
	}
	return workspace.squash, nil
}
func (workspace *stubWorkspace) Land(ctx context.Context, _ string) (flywheel.LandingStatus, error) {
	workspace.landCtx = ctx
	return workspace.landStatus, workspace.landErr
}
func (workspace *stubWorkspace) Cleanup(context.Context, flywheel.CleanupDisposition) error {
	workspace.cleanup()
	return workspace.cleanupErr
}

func capturedResult(stdout string) runner.Result {
	result := runner.Result{Stdout: runner.NewBuffer(1<<20, nil), Stderr: runner.NewBuffer(1<<20, nil)}
	_, _ = result.Stdout.Write([]byte(stdout))
	return result
}

func TestFixOrchestrationStartsBaselineAndInitialGatesBeforeWorkspace(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	var order []string
	service := fixtureService(paths, new(bytes.Buffer))
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		order = append(order, "baseline")
		return SuiteResult{Command: []string{"go", "test", "./..."}, Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		order = append(order, "identity")
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		order = append(order, "snapshot")
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		order = append(order, "workspace")
		return &stubWorkspace{cleanup: func() { order = append(order, "cleanup") }}, nil
	}
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		order = append(order, "flywheel")
		return flywheel.Outcome{Kind: flywheel.OutcomeBlocked, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}, Failure: "stuck"}
	}
	service.RenderReport = func(writer io.Writer, report Report, options RenderOptions) error {
		order = append(order, "render")
		return Render(writer, report, options)
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 || report.Fix == nil {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	want := []string{"baseline", "identity", "snapshot", "workspace", "flywheel", "render", "cleanup"}
	if !slices.Equal(order, want) {
		t.Fatalf("orchestration order = %v, want %v", order, want)
	}
}

func TestFixCleansWorkspaceWhenReportStagingFails(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	cleanups := 0
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return &stubWorkspace{cleanup: func() { cleanups++ }}, nil
	}
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeBlocked, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}, Failure: "stuck"}
	}
	service.RenderReport = func(io.Writer, Report, RenderOptions) error { return errors.New("render failed") }
	if _, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true}); err == nil || !strings.Contains(err.Error(), "stage report rendering") {
		t.Fatalf("Run() error = %v", err)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups)
	}
}

func TestCompletedLandingFailuresRemainCompleteAndErrored(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Command: []string{"go", "test", "./..."}, Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return &stubWorkspace{
			cleanup: func() {}, squash: strings.Repeat("a", 40), landStatus: flywheel.LandingComplete,
			landErr: errors.New("close evidence"), cleanupErr: errors.New("remove cache"),
			snapshot: func() (flywheel.ValidatedTree, error) { return &stubValidatedTree{root: "/owned/snapshot"}, nil },
		}, nil
	}
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || report.Verdict != VerdictErrored || report.Fix == nil || report.Fix.Landing.Status != "complete" || report.Fix.Landing.PreservedBranch != "" {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}

func TestCleanupFailureSupersedesFailedFinalSuite(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	suiteRuns := 0
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		suiteRuns++
		status := SuitePassed
		if suiteRuns == 2 {
			status = SuiteFailed
		}
		return SuiteResult{Status: status}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	var order []string
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return &stubWorkspace{cleanup: func() { order = append(order, "cleanup") }, cleanupErr: errors.New("cleanup failed"), snapshot: func() (flywheel.ValidatedTree, error) {
			return &stubValidatedTree{root: "/owned/snapshot"}, nil
		}}, nil
	}
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	service.RenderReport = func(writer io.Writer, report Report, options RenderOptions) error {
		order = append(order, "render")
		return Render(writer, report, options)
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || report.Fix == nil || report.Fix.Final == nil || report.Fix.Final.Status != SuiteFailed || report.Verdict != VerdictErrored {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	if want := []string{"render", "cleanup", "render"}; !slices.Equal(order, want) {
		t.Fatalf("publication order = %v, want %v", order, want)
	}
	for name, want := range map[string]Verdict{"report.pre-cleanup.json": VerdictBlocked, "report.json": VerdictErrored} {
		path := filepath.Join(report.Ref.Dir, name)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		var persisted Report
		if decodeErr := json.Unmarshal(raw, &persisted); decodeErr != nil || persisted.Verdict != want {
			t.Fatalf("%s verdict = %q, %v; want %q", name, persisted.Verdict, decodeErr, want)
		}
		if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v; want 0600", name, info, statErr)
		}
	}
}

func TestFinalSuiteWallClockExhaustionIsRailsAndClosesSnapshot(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Now = time.Now
	suiteRuns := 0
	var finalSuiteCtx, verifyCtx context.Context
	service.Suite = stubSuite{run: func(ctx context.Context, _ string, _ []string, _ bool) (SuiteResult, error) {
		suiteRuns++
		if suiteRuns == 1 {
			return SuiteResult{Status: SuitePassed}, nil
		}
		finalSuiteCtx = ctx
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 100*time.Millisecond {
			return SuiteResult{Status: SuiteErrored}, errors.New("final suite lacked remaining rail deadline")
		}
		<-ctx.Done()
		return SuiteResult{Status: SuiteErrored}, context.Cause(ctx)
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	closed, verified := 0, 0
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return &stubWorkspace{cleanup: func() {}, snapshot: func() (flywheel.ValidatedTree, error) {
			return &stubValidatedTree{root: t.TempDir(), verify: func(ctx context.Context) error {
				verified++
				verifyCtx = ctx
				return context.Cause(ctx)
			}, onClose: func() { closed++ }}, nil
		}}, nil
	}
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	parent, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	report, err := service.Run(parent, Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: 25 * time.Millisecond, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || report.Verdict != VerdictRails || report.Fix == nil || report.Fix.Final == nil || report.Fix.Final.Status != SuiteErrored {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	if verified != 1 || closed != 1 {
		t.Fatalf("snapshot lifecycle = verified %d closed %d", verified, closed)
	}
	if finalSuiteCtx == nil || finalSuiteCtx != verifyCtx {
		t.Fatalf("final validation contexts = suite %#v verify %#v", finalSuiteCtx, verifyCtx)
	}
}

func TestParentCancellationAfterFinalVerificationStopsBeforeLanding(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	parent, cancelParent := context.WithCancel(context.Background())
	suiteRuns := 0
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		suiteRuns++
		if suiteRuns == 2 {
			cancelParent()
		}
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	workspace := &stubWorkspace{cleanup: func() {}, squash: strings.Repeat("a", 40), landStatus: flywheel.LandingComplete, snapshot: func() (flywheel.ValidatedTree, error) {
		return &stubValidatedTree{root: t.TempDir()}, nil
	}}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) { return workspace, nil }
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	report, err := service.Run(parent, Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || report.Verdict != VerdictErrored || workspace.squashCtx != nil || workspace.landCtx != nil {
		t.Fatalf("Run() = %#v, %v; squash=%#v land=%#v", report, err, workspace.squashCtx, workspace.landCtx)
	}
}

func TestParentCancellationAtLandingAdmissionStopsBeforeSquash(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	started := time.Now()
	armed := false
	canceled := false
	service := fixtureService(paths, new(bytes.Buffer))
	service.Now = func() time.Time {
		if armed && !canceled {
			canceled = true
			cancelParent()
		}
		return started
	}
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	workspace := &stubWorkspace{
		cleanup: func() {}, squash: strings.Repeat("a", 40), landStatus: flywheel.LandingComplete,
		snapshot: func() (flywheel.ValidatedTree, error) {
			return &stubValidatedTree{root: t.TempDir(), onVerify: func() { armed = true }}, nil
		},
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) { return workspace, nil }
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	report, err := service.Run(parent, Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || report.Verdict != VerdictErrored || !canceled {
		t.Fatalf("Run() = %#v, %v; canceled=%v", report, err, canceled)
	}
	if workspace.squashCtx != nil || workspace.landCtx != nil || report.Fix.Landing.PreservedBranch == "" {
		t.Fatalf("squash=%#v land=%#v landing=%#v", workspace.squashCtx, workspace.landCtx, report.Fix.Landing)
	}
}

func TestFixReportRedactsWorkspacePaths(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return nil, errors.New("collision at /private/cache/worktree")
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || report.Fix == nil || strings.Contains(report.Fix.Landing.Error, "/private/cache") {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}

func TestFixReportIncludesEngineIntegrityEvidence(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return &stubWorkspace{cleanup: func() {}}, nil
	}
	integrity := reportFinding(t, "integrity", finding.Error, "feature.go", 1)
	service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
		return flywheel.Outcome{Kind: flywheel.OutcomeBlocked, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}, Findings: []finding.Finding{integrity}, Failure: "integrity validation found regressions"}
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 || report.Fix == nil || len(report.Fix.Integrity) != 1 || report.Fix.Integrity[0].Fingerprint != integrity.Fingerprint {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}

func TestResolveStagedDiffIncludesAttemptWithoutRequiringCleanWorktree(t *testing.T) {
	root, _, base, _ := scopedFixtureRepository(t)
	writeDiffTestFile(t, root, "scope.go", "package fixture\n\nfunc complexity() int {\n\treturn 3\n}\n")
	gitFixture(t, root, "add", "scope.go")
	diff, err := resolveStagedDiff(context.Background(), root, base)
	if err != nil {
		t.Fatalf("resolveStagedDiff() error = %v", err)
	}
	if diff.MergeBase != base || diff.Head == "" || diff.ChangedFiles != 1 || len(diff.Lines["scope.go"]) == 0 {
		t.Fatalf("staged diff = %#v", diff)
	}
}

func TestFixStopsBeforeAdapterForUnverifiedBaselineAndNoBlockers(t *testing.T) {
	for _, test := range []struct {
		name       string
		suite      SuiteStatus
		gateOutput string
		want       Verdict
		landing    string
	}{
		{name: "missing baseline", suite: SuiteMissing, gateOutput: fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), want: VerdictUnverified, landing: "blocked"},
		{name: "no blockers", suite: SuitePassed, gateOutput: `{"Issues":[]}`, want: VerdictUnsealed, landing: "not-needed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, paths := fixtureRepository(t)
			command := fixtureCommand(t, test.gateOutput, false)
			if test.gateOutput == `{"Issues":[]}` {
				command = []string{helperBinary(t), "emit", fmt.Sprintf("%q", test.gateOutput), fmt.Sprintf("%q", ""), "0"}
			}
			gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(command...))
			service := fixtureService(paths, new(bytes.Buffer))
			service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
				return SuiteResult{Status: test.suite}, nil
			}}
			service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
			called := false
			service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
				called = true
				return flywheel.Outcome{}
			}
			report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || report.Verdict != test.want || report.Fix == nil || report.Fix.Landing.Status != test.landing {
				t.Fatalf("Run() = %#v, %v", report, err)
			}
			if called {
				t.Fatal("adapter flywheel started")
			}
		})
	}
}

func TestFixTreatsFeatureBranchResolutionFailureAsInfrastructure(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, *Service)
	}{
		{name: "detached HEAD", mutate: func(t *testing.T, root string, _ *Service) {
			gitFixture(t, root, "checkout", "--detach", "-q")
		}},
		{name: "Git failure", mutate: func(_ *testing.T, _ string, service *Service) {
			service.ResolveFeatureBranch = func(context.Context, string) (string, error) {
				return "", errors.New("Git infrastructure unavailable")
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, paths := fixtureRepository(t)
			gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(helperBinary(t), "emit", fmt.Sprintf("%q", `{"Issues":[]}`), fmt.Sprintf("%q", ""), "0"))
			service := fixtureService(paths, new(bytes.Buffer))
			service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
				return SuiteResult{Status: SuitePassed}, nil
			}}
			service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
			called := false
			service.ExecuteFlywheel = func(context.Context, flywheel.Request, flywheel.Ports) flywheel.Outcome {
				called = true
				return flywheel.Outcome{}
			}
			test.mutate(t, root, &service)
			report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 4 || report.Verdict != VerdictErrored || report.Fix == nil {
				t.Fatalf("Run() = %#v, %v", report, err)
			}
			if report.Fix.FeatureBranch != "" || report.Fix.FeatureBranch == "unresolved" {
				t.Fatalf("feature branch = %q", report.Fix.FeatureBranch)
			}
			if called {
				t.Fatal("fix loop started after feature branch resolution failed")
			}
		})
	}
}

func TestBarrierAndFinalSuiteUseAuthenticatedValidatedSnapshots(t *testing.T) {
	root, paths := fixtureRepository(t)
	snapshotRoots := []string{t.TempDir(), t.TempDir()}
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command("fixture"))
	service := fixtureService(paths, new(bytes.Buffer))
	var gateRoots []string
	gateRuns := 0
	service.Executor.runCommand = func(_ context.Context, executionRoot string, _ []string) runner.Result {
		gateRoots = append(gateRoots, executionRoot)
		gateRuns++
		if gateRuns == 1 {
			return capturedResult(fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"))
		}
		return capturedResult(`{"Issues":[]}`)
	}
	var suiteRoots []string
	service.Suite = stubSuite{run: func(_ context.Context, executionRoot string, _ []string, _ bool) (SuiteResult, error) {
		suiteRoots = append(suiteRoots, executionRoot)
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	var snapshots, verified, closed int
	workspace := &stubWorkspace{
		root: root, cleanup: func() {}, squash: strings.Repeat("a", 40), landStatus: flywheel.LandingComplete,
		snapshot: func() (flywheel.ValidatedTree, error) {
			snapshots++
			return &stubValidatedTree{root: snapshotRoots[snapshots-1], onVerify: func() { verified++ }, onClose: func() { closed++ }}, nil
		},
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) { return workspace, nil }
	service.ExecuteFlywheel = func(ctx context.Context, _ flywheel.Request, ports flywheel.Ports) flywheel.Outcome {
		if result := ports.Barrier(ctx); result.Kind != flywheel.ValidationPassed {
			return flywheel.Outcome{Kind: flywheel.OutcomeErrored, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}, Failure: result.Failure}
		}
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 6 || report.Verdict != VerdictUnsealed {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	if want := []string{root, snapshotRoots[0]}; !slices.Equal(gateRoots, want) {
		t.Fatalf("gate roots = %v, want %v", gateRoots, want)
	}
	if want := []string{root, snapshotRoots[1]}; !slices.Equal(suiteRoots, want) {
		t.Fatalf("suite roots = %v, want %v", suiteRoots, want)
	}
	if snapshots != 2 || verified != 2 || closed != 2 {
		t.Fatalf("snapshot lifecycle = created %d verified %d closed %d", snapshots, verified, closed)
	}
	if workspace.squashTree == nil {
		t.Fatal("squash did not consume the final validated snapshot")
	}
}

func TestLandingTransactionIgnoresParentCancellationAfterAdmission(t *testing.T) {
	root, paths := fixtureRepository(t)
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	gateRuns := 0
	service.Executor.runCommand = func(context.Context, string, []string) runner.Result {
		gateRuns++
		if gateRuns == 1 {
			return capturedResult(fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"))
		}
		return capturedResult(`{"Issues":[]}`)
	}
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	parent, cancelParent := context.WithCancel(context.Background())
	workspace := &stubWorkspace{
		root: root, cleanup: func() {}, squash: strings.Repeat("a", 40), landStatus: flywheel.LandingComplete,
		snapshot: func() (flywheel.ValidatedTree, error) { return &stubValidatedTree{root: t.TempDir()}, nil },
		onSquash: func(ctx context.Context) error {
			cancelParent()
			return ctx.Err()
		},
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) { return workspace, nil }
	service.ExecuteFlywheel = func(ctx context.Context, _ flywheel.Request, ports flywheel.Ports) flywheel.Outcome {
		if barrier := ports.Barrier(ctx); barrier.Kind != flywheel.ValidationPassed {
			return flywheel.Outcome{Kind: flywheel.OutcomeErrored, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}, Failure: barrier.Failure}
		}
		return flywheel.Outcome{Kind: flywheel.OutcomeReady, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}}
	}
	report, err := service.Run(parent, Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 6 || report.Verdict != VerdictUnsealed {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	if workspace.squashCtx == nil || workspace.squashCtx != workspace.landCtx {
		t.Fatalf("transaction contexts = squash %#v land %#v", workspace.squashCtx, workspace.landCtx)
	}
	deadline, ok := workspace.squashCtx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
		t.Fatalf("transaction deadline = %v, %v", deadline, ok)
	}
}

func TestBarrierRejectsSnapshotMutation(t *testing.T) {
	root, paths := fixtureRepository(t)
	mutatedSnapshot := t.TempDir()
	gatetest.Write(t, paths.GateOverrides(), "lint", gatetest.Command(fixtureCommand(t, fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"), false)...))
	service := fixtureService(paths, new(bytes.Buffer))
	gateRuns := 0
	service.Executor.runCommand = func(context.Context, string, []string) runner.Result {
		gateRuns++
		if gateRuns == 1 {
			return capturedResult(fixtureJSON("feature.go", 1, "fixture/blocker", "fix me"))
		}
		return capturedResult(`{"Issues":[]}`)
	}
	service.Suite = stubSuite{run: func(context.Context, string, []string, bool) (SuiteResult, error) {
		return SuiteResult{Status: SuitePassed}, nil
	}}
	service.Adapters = map[string]adapter.Adapter{"codex": stubAdapter{}}
	service.ResolveFixIdentity = func(context.Context, string) (string, flywheel.Identity, error) {
		return "feature", flywheel.Identity{Name: "Togi", Email: "togi@example.invalid"}, nil
	}
	service.SnapshotOriginal = func(context.Context, string, string) (flywheel.TreeSnapshot, error) {
		return flywheel.TreeSnapshot{Files: map[string][]byte{}}, nil
	}
	service.WorkspaceFactory = func(context.Context, flywheel.WorkspaceSpec) (FixWorkspace, error) {
		return &stubWorkspace{cleanup: func() {}, snapshot: func() (flywheel.ValidatedTree, error) {
			return &stubValidatedTree{root: mutatedSnapshot, verifyErr: errors.New("snapshot tree changed")}, nil
		}}, nil
	}
	service.ExecuteFlywheel = func(ctx context.Context, _ flywheel.Request, ports flywheel.Ports) flywheel.Outcome {
		result := ports.Barrier(ctx)
		if result.Kind != flywheel.ValidationInfrastructureFailure {
			t.Fatalf("Barrier() = %#v", result)
		}
		return flywheel.Outcome{Kind: flywheel.OutcomeErrored, Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}, Failure: result.Failure}
	}
	report, err := service.Run(context.Background(), Options{Root: root, GateNames: []string{"lint"}, Agent: "codex", MaxIterations: 2, MaxWallClock: time.Minute, NoColor: true})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 || report.Verdict != VerdictErrored {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}

func TestRenderFixSummaryOmitsPrivateArtifacts(t *testing.T) {
	report := Report{Verdict: VerdictBlocked, Findings: []finding.Finding{}, Gates: []GateReport{}, Fix: &FixReport{
		Baseline: SuiteResult{Status: SuitePassed}, Final: &SuiteResult{Status: SuiteFailed},
		Rails:     RailsReport{MaxIterations: 20, Iterations: 2, MaxWallClockMS: 1800000, ElapsedMS: 25},
		Batches:   []flywheel.Batch{{Status: flywheel.BatchDone, Attempts: []flywheel.Attempt{{Number: 1, Status: "done"}}}, {Status: flywheel.BatchStuck, Attempts: []flywheel.Attempt{{Number: 1}, {Number: 2}}}},
		Integrity: []finding.Finding{}, Landing: LandingReport{Status: "blocked", PreservedBranch: "togi/run-safe", Error: "guard refused /cache/private raw.jsonl brief"},
	}}
	var output bytes.Buffer
	if err := Render(&output, report, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"baseline suite: passed", "final suite: failed", "batches: 1/2 complete (3 attempts)", "rails: 2/20 iterations", "landing: blocked", "preserved branch: togi/run-safe", "verdict: blocked"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered summary missing %q:\n%s", want, got)
		}
	}
	for _, private := range []string{"/cache/private", "raw.jsonl", "brief"} {
		if strings.Contains(got, private) {
			t.Fatalf("rendered summary exposed %q:\n%s", private, got)
		}
	}
}

func TestSchema4FixReportRoundTrips(t *testing.T) {
	report := completeReportFixture("20260821T120000.000000000Z-0000")
	report.Verdict = VerdictUnsealed
	report.Fix = &FixReport{
		OriginalHead:  report.Diff.Head,
		FeatureBranch: "feature",
		Agent:         AgentReport{Name: "codex", Usage: &adapter.Usage{InputTokens: 3, OutputTokens: 2}},
		Baseline:      SuiteResult{Command: []string{"go", "test", "./..."}, Status: SuitePassed},
		Final:         &SuiteResult{Command: []string{"go", "test", "./..."}, Status: SuitePassed},
		Rails:         RailsReport{MaxIterations: 20, Iterations: 1, MaxWallClockMS: int64((30 * time.Minute) / time.Millisecond), ElapsedMS: 5},
		Batches:       []flywheel.Batch{},
		Integrity:     []finding.Finding{},
		Landing:       LandingReport{Status: string(flywheel.LandingNotNeeded)},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"schema_version":4`) || !strings.Contains(string(raw), `"fix":`) {
		t.Fatalf("schema-4 JSON = %s", raw)
	}
	if err := validateReport(report, report.RunID); err != nil {
		t.Fatalf("validateReport() error = %v", err)
	}
}

func TestComposeFixReportDeepCopiesNestedEvidence(t *testing.T) {
	batchFinding := reportFinding(t, "lint", finding.Error, "source.go", 1)
	batchFinding.Occurrences = []finding.Occurrence{{Line: 2}}
	integrityFinding := reportFinding(t, "integrity", finding.Error, "source.go", 1)
	integrityFinding.Occurrences = []finding.Occurrence{{Line: 3}}
	usage := &adapter.Usage{InputTokens: 3, OutputTokens: 2}
	baseline := SuiteResult{Command: []string{"go", "test", "./..."}, Packages: []string{"./..."}, Status: SuitePassed}
	final := SuiteResult{Command: []string{"go", "test", "./..."}, Packages: []string{"./..."}, Status: SuitePassed}
	batch := flywheel.Batch{
		ID: "batch-" + strings.Repeat("a", 64), PrimaryFile: "source.go", Findings: []finding.Finding{batchFinding}, Status: flywheel.BatchDone,
		Attempts: []flywheel.Attempt{{Number: 1, Status: "passed", ChangedFiles: []string{"source.go"}, Commit: strings.Repeat("a", 40)}},
	}
	rails, err := flywheel.NewRails(flywheel.RailConfig{MaxIterations: 2, MaxWallClock: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	execution := fixExecution{
		baseline: baseline, final: &final, usage: usage, verdict: VerdictBlocked,
		gates:     []GateReport{{Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: "report-only", Status: GatePassed}},
		outcome:   flywheel.Outcome{Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{batch}}},
		integrity: []finding.Finding{integrityFinding}, landing: LandingReport{Status: "blocked"}, failure: errors.New("blocked"), branch: "feature",
	}
	report, err := composeFixReport("20260821T120000.000000000Z-0000", strings.Repeat("d", 40), time.Now().Add(-time.Second), time.Now(), fixtureDiff(), execution, rails, "codex")
	if err != nil {
		t.Fatal(err)
	}
	usage.InputTokens = 99
	baseline.Command[0], baseline.Packages[0] = "mutated", "mutated"
	final.Command[0], final.Packages[0] = "mutated", "mutated"
	batch.Findings[0].Message = "mutated"
	batch.Findings[0].Occurrences[0].Line = 99
	batch.Attempts[0].ChangedFiles[0] = "mutated.go"
	execution.outcome.Plan.Batches[0].Status = flywheel.BatchStuck
	execution.outcome.Plan.Batches[0].Attempts[0].Status = "failed"
	execution.integrity[0].Message = "mutated"
	integrityFinding.Occurrences[0].Line = 99

	if report.Fix.Agent.Usage.InputTokens != 3 || report.Fix.Baseline.Command[0] != "go" || report.Fix.Baseline.Packages[0] != "./..." || report.Fix.Final.Command[0] != "go" || report.Fix.Final.Packages[0] != "./..." {
		t.Fatalf("suite or usage evidence aliased source: %#v", report.Fix)
	}
	gotBatch := report.Fix.Batches[0]
	if gotBatch.Status != flywheel.BatchDone || gotBatch.Attempts[0].Status != "passed" || gotBatch.Findings[0].Message == "mutated" || gotBatch.Findings[0].Occurrences[0].Line != 2 || gotBatch.Attempts[0].ChangedFiles[0] != "source.go" {
		t.Fatalf("batch evidence aliased source: %#v", gotBatch)
	}
	if report.Fix.Integrity[0].Message == "mutated" || report.Fix.Integrity[0].Occurrences[0].Line != 3 {
		t.Fatalf("integrity evidence aliased source: %#v", report.Fix.Integrity)
	}
}

func TestComposeFixReportPreservesPositiveSubMillisecondRail(t *testing.T) {
	started := time.Now()
	rails, err := flywheel.NewRails(flywheel.RailConfig{MaxIterations: 2, MaxWallClock: 500 * time.Microsecond}, func() time.Time { return started })
	if err != nil {
		t.Fatal(err)
	}
	execution := fixExecution{
		baseline: SuiteResult{Status: SuitePassed}, verdict: VerdictErrored,
		gates:     []GateReport{{Gate: "lint", Language: "go", Blocking: []finding.Severity{finding.Error}, FixPolicy: "report-only", Status: GatePassed}},
		outcome:   flywheel.Outcome{Plan: flywheel.Plan{SchemaVersion: 1, Batches: []flywheel.Batch{}}},
		integrity: []finding.Finding{}, landing: LandingReport{Status: "blocked"}, failure: errors.New("failed"), branch: "feature",
	}
	report, err := composeFixReport("20260821T120000.000000000Z-0000", strings.Repeat("d", 40), started, started, fixtureDiff(), execution, rails, "codex")
	if err != nil {
		t.Fatalf("composeFixReport() error = %v", err)
	}
	if report.Fix == nil || report.Fix.Rails.MaxWallClockMS != 1 {
		t.Fatalf("rails = %#v, want positive millisecond representation", report.Fix)
	}
	if rails.Snapshot().MaxWallClock != 500*time.Microsecond {
		t.Fatalf("actual rail duration = %s, want 500us", rails.Snapshot().MaxWallClock)
	}
}

func TestReportRequiresFixOnlyForPhase3Verdicts(t *testing.T) {
	for _, verdict := range []Verdict{VerdictBlocked, VerdictRails, VerdictUnsealed} {
		report := completeReportFixture("20260821T120000.000000000Z-0000")
		report.Verdict = verdict
		if err := validateReport(report, report.RunID); err == nil {
			t.Fatalf("validateReport accepted %q without fix report", verdict)
		}
	}
	report := completeReportFixture("20260821T120000.000000000Z-0000")
	report.Verdict = VerdictFindings
	report.Fix = &FixReport{}
	if err := validateReport(report, report.RunID); err == nil {
		t.Fatal("validateReport accepted report-only verdict with fix report")
	}
}

func TestSchema4RejectsContradictoryAndMalformedFixEvidence(t *testing.T) {
	valid := func() Report {
		report := completeReportFixture("20260821T120000.000000000Z-0000")
		report.Verdict = VerdictUnsealed
		report.Fix = &FixReport{
			OriginalHead: report.Diff.Head, FeatureBranch: "feature", Agent: AgentReport{Name: "codex"},
			Baseline: SuiteResult{Status: SuitePassed}, Rails: RailsReport{MaxIterations: 2, MaxWallClockMS: 1000},
			Batches: []flywheel.Batch{}, Integrity: []finding.Finding{}, Landing: LandingReport{Status: "not-needed"},
		}
		return report
	}
	validBatch := func() flywheel.Batch {
		return flywheel.Batch{
			ID: "batch-" + strings.Repeat("a", 64), PrimaryFile: "source.go", Findings: []finding.Finding{reportFinding(t, "lint", finding.Error, "source.go", 1)}, Status: flywheel.BatchDone,
			Attempts: []flywheel.Attempt{{Number: 1, Status: "passed", ChangedFiles: []string{"source.go"}, Commit: strings.Repeat("a", 40)}},
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "errored gate unsealed", mutate: func(report *Report) { report.Gates[0].Status = GateErrored; report.Gates[0].Error = "missing" }},
		{name: "blocking gate unsealed", mutate: func(report *Report) {
			item := reportFinding(t, "lint", finding.Error, "source.go", 1)
			report.Gates[0].Status, report.Gates[0].Findings = GateFindings, []finding.Finding{item}
			report.Findings, report.Counts = []finding.Finding{item}, countFindings([]finding.Finding{item})
		}},
		{name: "unexhausted rails claim", mutate: func(report *Report) {
			report.Verdict, report.Fix.Landing.Status = VerdictRails, "blocked"
			report.Fix.Final = &SuiteResult{Status: SuiteErrored}
			report.Fix.Rails.Iterations, report.Fix.Rails.ElapsedMS = 0, 0
		}},
		{name: "red baseline unsealed", mutate: func(report *Report) { report.Fix.Baseline.Status = SuiteFailed }},
		{name: "blocked landing unsealed", mutate: func(report *Report) { report.Fix.Landing.Status = "blocked" }},
		{name: "integrity unsealed", mutate: func(report *Report) {
			report.Fix.Integrity = []finding.Finding{reportFinding(t, "integrity", finding.Error, "source.go", 1)}
		}},
		{name: "malformed batch", mutate: func(report *Report) {
			report.Fix.Batches = []flywheel.Batch{{ID: "batch-invalid", PrimaryFile: "source.go", Findings: []finding.Finding{}, Status: flywheel.BatchPending, Attempts: []flywheel.Attempt{}}}
		}},
		{name: "pending batch with attempt", mutate: func(report *Report) {
			batch := validBatch()
			batch.Status = flywheel.BatchPending
			report.Fix.Batches = []flywheel.Batch{batch}
		}},
		{name: "running batch without attempt", mutate: func(report *Report) {
			batch := validBatch()
			batch.Status, batch.Attempts = flywheel.BatchRunning, []flywheel.Attempt{}
			report.Fix.Batches = []flywheel.Batch{batch}
		}},
		{name: "running batch after success", mutate: func(report *Report) {
			batch := validBatch()
			batch.Status = flywheel.BatchRunning
			report.Fix.Batches = []flywheel.Batch{batch}
		}},
		{name: "second attempt after success", mutate: func(report *Report) {
			batch := validBatch()
			batch.Attempts = append(batch.Attempts, flywheel.Attempt{Number: 2, Status: "running"})
			report.Fix.Batches = []flywheel.Batch{batch}
		}},
		{name: "done batch without final success", mutate: func(report *Report) {
			batch := validBatch()
			batch.Attempts = []flywheel.Attempt{{Number: 1, Status: "failed", Failure: "failed"}}
			report.Fix.Batches = []flywheel.Batch{batch}
		}},
		{name: "integrity finding owned by another gate", mutate: func(report *Report) {
			report.Verdict, report.Fix.Landing.Status = VerdictBlocked, "blocked"
			report.Fix.Integrity = []finding.Finding{reportFinding(t, "lint", finding.Error, "source.go", 1)}
		}},
		{name: "duplicate integrity finding", mutate: func(report *Report) {
			report.Verdict, report.Fix.Landing.Status = VerdictBlocked, "blocked"
			item := reportFinding(t, "integrity", finding.Error, "source.go", 1)
			report.Fix.Integrity = []finding.Finding{item, item}
		}},
		{name: "complete without commit", mutate: func(report *Report) { report.Fix.Landing.Status = "complete" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := valid()
			test.mutate(&report)
			if err := validateReport(report, report.RunID); err == nil {
				t.Fatal("validateReport accepted contradictory fix evidence")
			}
		})
	}
}

func TestSchema4AllowsCleanupErrorToSupersedeFailedFinalSuite(t *testing.T) {
	report := completeReportFixture("20260821T120000.000000000Z-0000")
	report.Verdict = VerdictErrored
	report.Fix = &FixReport{
		OriginalHead: report.Diff.Head, FeatureBranch: "feature", Agent: AgentReport{Name: "codex"},
		Baseline: SuiteResult{Status: SuitePassed}, Final: &SuiteResult{Status: SuiteFailed},
		Rails: RailsReport{MaxIterations: 2, MaxWallClockMS: 1000}, Batches: []flywheel.Batch{}, Integrity: []finding.Finding{},
		Landing: LandingReport{Status: "blocked", Error: "fix loop infrastructure failed"},
	}
	if err := validateReport(report, report.RunID); err != nil {
		t.Fatalf("validateReport() error = %v", err)
	}
}
