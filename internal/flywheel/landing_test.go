package flywheel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
)

func TestSquashUsesLatestValidatedTreeOriginalParentAndOperatorIdentity(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "squash-shape")
	first := commitTestBatch(t, workspace, "feature.txt", "first\n")
	second := commitTestBatch(t, workspace, "feature.txt", "second\n")

	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if squash == first || squash == second || squash == original {
		t.Fatalf("Squash() = %q, want new commit", squash)
	}
	if got := gitcmdtest.Git(t, repo, "show", "-s", "--format=%T|%P|%s|%an <%ae>|%cn <%ce>", squash); got !=
		gitcmdtest.Git(t, repo, "show", "-s", "--format=%T", second)+"|"+original+"|togi: apply verified fixes|Togi <togi@example.invalid>|Togi <togi@example.invalid>" {
		t.Fatalf("squash shape = %q", got)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-squash-shape"); got != squash {
		t.Fatalf("run ref = %q, want squash %q", got, squash)
	}
	if got := gitcmdtest.Git(t, repo, "show", squash+":feature.txt"); got != "second" {
		t.Fatalf("squash contents = %q", got)
	}
}

func TestSquashRefRacePreservesConcurrentRefAndRejectsObject(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "squash-race")
	latest := commitTestBatch(t, workspace, "feature.txt", "validated\n")
	workspace.beforeSquashRefUpdate = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/togi/run-squash-race", original, latest)
		return nil
	}

	if _, err := workspace.Squash(context.Background()); err == nil {
		t.Fatal("Squash() accepted concurrently moved run ref")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-squash-race"); got != original {
		t.Fatalf("concurrent run ref = %q, want preserved %q", got, original)
	}
}

func TestSquashAmbiguousCASRollsBackToLatestValidatedCommit(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "squash-ambiguous")
	latest := commitTestBatch(t, workspace, "feature.txt", "validated\n")
	workspace.updateBatchRef = func(ctx context.Context, ref, next, previous string) error {
		if err := workspace.updateBatchRefDirect(ctx, ref, next, previous); err != nil {
			return err
		}
		return errors.New("injected lost update acknowledgement")
	}

	if _, err := workspace.Squash(context.Background()); err == nil {
		t.Fatal("Squash() accepted ambiguous ref update")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-squash-ambiguous"); got != latest {
		t.Fatalf("ambiguous run ref = %q, want rolled back %q", got, latest)
	}
}

func TestLandingFastForwardsFeatureCheckoutAndDisablesHooks(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-success")
	commitTestBatch(t, workspace, "feature.txt", "landed\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-merge")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+hookMarker+"\"\nexit 91\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	status, err := workspace.Land(context.Background(), squash)
	if err != nil || status != LandingComplete {
		t.Fatalf("Land() = %q, %v", status, err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != squash {
		t.Fatalf("feature HEAD = %q, want %q", got, squash)
	}
	if contents, readErr := os.ReadFile(filepath.Join(repo, "feature.txt")); readErr != nil || string(contents) != "landed\n" {
		t.Fatalf("feature contents = %q, %v", contents, readErr)
	}
	if _, statErr := os.Lstat(hookMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("post-merge hook ran: %v", statErr)
	}
}

func TestLandingRecordsAuthenticatedEvidenceWhenRepositoryReflogsAreDisabled(t *testing.T) {
	repo, original := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "config", "core.logAllRefUpdates", "false")
	if err := os.Remove(filepath.Join(repo, ".git", "logs", "refs", "heads", "feature")); err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-forced-reflog")
	commitTestBatch(t, workspace, "feature.txt", "landed\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr != nil || status != LandingComplete {
		t.Fatalf("Land() = %q, %v", status, landErr)
	}
}

func TestLandingDisablesRepositoryConversionFilters(t *testing.T) {
	repo, _ := workspaceRepository(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	filter := filepath.Join(t.TempDir(), "smudge.sh")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\ntouch \""+marker+"\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "config", "filter.evidence.smudge", filter)
	gitcmdtest.Git(t, repo, "config", "filter.evidence.clean", "cat")
	gitcmdtest.Git(t, repo, "config", "filter.evidence.required", "true")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("filtered.txt filter=evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "add", ".gitattributes")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "attributes")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-filter")
	commitTestBatch(t, workspace, "filtered.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr != nil || status != LandingComplete {
		t.Fatalf("Land() = %q, %v", status, landErr)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository conversion filter executed: %v", err)
	}
}

func TestLandingRejectsLateConversionFilterBeforeExecution(t *testing.T) {
	repo, _ := workspaceRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("filtered.txt filter=late\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "add", ".gitattributes")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "attributes")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-late-filter")
	commitTestBatch(t, workspace, "filtered.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "filter-ran")
	filter := filepath.Join(t.TempDir(), "smudge.sh")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\ntouch \""+marker+"\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error {
		gitcmdtest.Git(t, repo, "config", "filter.late.smudge", filter)
		return nil
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want late-filter refusal", status, landErr)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late repository conversion filter executed: %v", err)
	}
}

func TestLandingNotNeededCreatesNoCommit(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-not-needed")
	squash, err := workspace.Squash(context.Background())
	if err != nil || squash != "" {
		t.Fatalf("Squash() = %q, %v; want no commit", squash, err)
	}
	status, err := workspace.Land(context.Background(), squash)
	if err != nil || status != LandingNotNeeded {
		t.Fatalf("Land() = %q, %v; want not-needed", status, err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != original {
		t.Fatalf("no-op run ref = %q, want %q", got, original)
	}
	for range 2 {
		if err := workspace.Cleanup(context.Background(), CleanupLanded); err != nil {
			t.Fatalf("CleanupLanded after not-needed = %v", err)
		}
	}
	if _, err := os.Lstat(workspace.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op worktree survived cleanup: %v", err)
	}
}

func TestWorkspaceRejectsSymbolicFeatureRefBeforeLandingLifecycle(t *testing.T) {
	repo, original := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "branch", "target", original)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/feature", "refs/heads/target")
	workspace, err := CreateWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: filepath.Join(t.TempDir(), "workspace"), RunID: "symbolic-feature",
		OriginalHead: original, FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	})
	if err == nil {
		_ = workspace.Cleanup(context.Background(), CleanupPreserveValidated)
		t.Fatal("CreateWorkspace() accepted symbolic feature branch ref")
	}
}

func TestLandingRejectsSquashWithoutOriginalHeadAsSoleParent(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-ancestry")
	latest := commitTestBatch(t, workspace, "feature.txt", "validated\n")
	tree := gitcmdtest.Git(t, repo, "show", "-s", "--format=%T", latest)
	tampered := gitcmdtest.Git(t, repo,
		"-c", "user.name=Togi", "-c", "user.email=togi@example.invalid",
		"commit-tree", tree, "-p", latest, "-m", squashSubject)
	gitcmdtest.Git(t, repo, "update-ref", "refs/heads/"+workspace.branch, tampered, latest)
	workspace.green = tampered

	status, landErr := workspace.Land(context.Background(), tampered)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want ancestry refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != original {
		t.Fatalf("ancestry refusal moved feature HEAD to %q", got)
	}
}

func TestLandingGuardsPreserveValidatedRunBranch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, string, *Workspace)
	}{
		{name: "dirty tracked", mutate: func(t *testing.T, repo, _ string, _ *Workspace) {
			writeWorkspaceFile(t, repo, "feature.txt", "operator\n")
		}},
		{name: "untracked", mutate: func(t *testing.T, repo, _ string, _ *Workspace) {
			writeWorkspaceFile(t, repo, "operator.txt", "operator\n")
		}},
		{name: "ignored", mutate: func(t *testing.T, repo, _ string, _ *Workspace) {
			if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("ignored.txt\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, repo, "ignored.txt", "operator\n")
		}},
		{name: "detached", mutate: func(t *testing.T, repo, original string, _ *Workspace) {
			gitcmdtest.Git(t, repo, "checkout", "--detach", original)
		}},
		{name: "wrong branch", mutate: func(t *testing.T, repo, original string, _ *Workspace) {
			gitcmdtest.Git(t, repo, "checkout", "-b", "operator", original)
		}},
		{name: "moved head", mutate: func(t *testing.T, repo, _ string, _ *Workspace) {
			writeWorkspaceFile(t, repo, "operator.txt", "operator\n")
			gitcmdtest.Git(t, repo, "add", "operator.txt")
			gitcmdtest.Git(t, repo, "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid", "commit", "-qm", "operator")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, original := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "guard-"+strings.ReplaceAll(test.name, " ", "-"))
			commitTestBatch(t, workspace, "feature.txt", "validated\n")
			squash, err := workspace.Squash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, repo, original, workspace)
			status, landErr := workspace.Land(context.Background(), squash)
			if landErr == nil || status != LandingBlocked {
				t.Fatalf("Land() = %q, %v; want blocked", status, landErr)
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
				t.Fatalf("run branch = %q, want preserved %q", got, squash)
			}
		})
	}
}

func TestLandingFinalGuardCatchesRefMoveRace(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingMerge = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", squash, original)
		return nil
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want blocked race", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "--short", "HEAD"); got != "feature" {
		t.Fatalf("feature checkout branch = %q", got)
	}
}

func TestLandingFinalGuardPreservesConcurrentConfigAndIndexChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *Workspace)
	}{
		{name: "config", mutate: func(t *testing.T, repo string, _ *Workspace) {
			gitcmdtest.Git(t, repo, "config", "landing.operator", "preserve")
		}},
		{name: "index replacement", mutate: func(t *testing.T, _ string, workspace *Workspace) {
			contents, err := os.ReadFile(workspace.featureIndexPath)
			if err != nil {
				t.Fatal(err)
			}
			moved := workspace.featureIndexPath + ".operator"
			if err := os.Rename(workspace.featureIndexPath, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(workspace.featureIndexPath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, original := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-shared-"+strings.ReplaceAll(test.name, " ", "-"))
			commitTestBatch(t, workspace, "feature.txt", "validated\n")
			squash, err := workspace.Squash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			workspace.beforeLandingMerge = func() error {
				test.mutate(t, repo, workspace)
				return nil
			}
			status, landErr := workspace.Land(context.Background(), squash)
			if landErr == nil || status != LandingBlocked {
				t.Fatalf("Land() = %q, %v; want preserved shared-state refusal", status, landErr)
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != original {
				t.Fatalf("feature HEAD = %q, want original %q", got, original)
			}
		})
	}
}

func TestLandingFinalGuardCoversFeatureWorktreeConfig(t *testing.T) {
	repo, commonRepo, original := linkedFeatureRepository(t)
	gitcmdtest.Git(t, commonRepo, "config", "extensions.worktreeConfig", "true")
	gitcmdtest.Git(t, repo, "config", "--worktree", "landing.operator", "before")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-feature-config")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingMerge = func() error {
		gitcmdtest.Git(t, repo, "config", "--worktree", "landing.operator", "after")
		return nil
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want feature config race refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "config", "--worktree", "landing.operator"); got != "after" {
		t.Fatalf("operator feature config was not preserved: %q", got)
	}
}

func TestLandingRejectsMissingOrReplacedFeaturePath(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "replaced"}[replace], func(t *testing.T) {
			repo, original := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-path-"+map[bool]string{false: "missing", true: "replaced"}[replace])
			commitTestBatch(t, workspace, "feature.txt", "validated\n")
			squash, err := workspace.Squash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			moved := repo + ".operator"
			if err := os.Rename(repo, moved); err != nil {
				t.Fatal(err)
			}
			if replace {
				if err := os.Mkdir(repo, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			status, landErr := workspace.Land(context.Background(), squash)
			if landErr == nil || status != LandingBlocked {
				t.Fatalf("Land() = %q, %v; want path-binding refusal", status, landErr)
			}
		})
	}
}

func TestLandingImmediatePreExecGuardRejectsPathSubstitution(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-exec-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	moved := repo + ".operator"
	workspace.beforeLandingExec = func() error {
		if err := os.Rename(repo, moved); err != nil {
			return err
		}
		if err := os.Mkdir(repo, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(repo, "victim.txt"), []byte("preserve\n"), 0o600)
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want immediate path-race refusal", status, landErr)
	}
	if contents, err := os.ReadFile(filepath.Join(repo, "victim.txt")); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("replacement victim = %q, %v", contents, err)
	}
}

func TestLandingBoundLaunchRejectsPostBindSymlinkSubstitution(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-bind-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	moved := repo + ".operator"
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "victim.txt"), []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.afterLandingBind = func() error {
		if err := os.Rename(repo, moved); err != nil {
			return err
		}
		return os.Symlink(victim, repo)
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want post-bind path-race refusal", status, landErr)
	}
	if contents, err := os.ReadFile(filepath.Join(victim, "victim.txt")); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("post-bind victim = %q, %v", contents, err)
	}
	if got := gitcmdtest.Git(t, moved, "rev-parse", "HEAD"); got != original {
		t.Fatalf("post-bind refusal moved original checkout to %q", got)
	}
}

func TestLandingBoundLaunchRejectsPostBindBranchSwitch(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-bind-branch")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "branch", "operator", original)
	workspace.afterLandingBind = func() error {
		gitcmdtest.Git(t, repo, "checkout", "-q", "operator")
		return nil
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want post-bind branch refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/operator"); got != original {
		t.Fatalf("operator branch moved to %q", got)
	}
	if contents, err := os.ReadFile(filepath.Join(repo, "feature.txt")); err != nil || string(contents) != "original\n" {
		t.Fatalf("operator checkout contents = %q, %v", contents, err)
	}
}

func TestLandingBoundLaunchRejectsPostBindFeatureConfigMutation(t *testing.T) {
	repo, commonRepo, original := linkedFeatureRepository(t)
	gitcmdtest.Git(t, commonRepo, "config", "extensions.worktreeConfig", "true")
	gitcmdtest.Git(t, repo, "config", "--worktree", "landing.operator", "before")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-bind-config")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.afterLandingBind = func() error {
		gitcmdtest.Git(t, repo, "config", "--worktree", "landing.operator", "after")
		return nil
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want post-bind config refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "config", "--worktree", "landing.operator"); got != "after" {
		t.Fatalf("operator config was not preserved: %q", got)
	}
}

func TestLandingAuthenticatesActualPreMergeHeadAndRestoresConcurrentState(t *testing.T) {
	repo, ancestor := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-pre-head-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", ancestor, original)
		return nil
	}
	if err := os.Remove(filepath.Join(workspace.featureGitDir, "ORIG_HEAD")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "recovery incomplete") {
		t.Fatalf("Land() = %q, %v; want authenticated race refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != squash {
		t.Fatalf("incomplete-recovery feature HEAD = %q, want preserved merge result %q", got, squash)
	}
	if got := gitcmdtest.Git(t, repo, "status", "--porcelain=v2", "--untracked-files=all", "--ignored=matching"); got != "" {
		t.Fatalf("recovered feature checkout is dirty: %q", got)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("run branch = %q, want preserved %q", got, squash)
	}
	if got, err := os.ReadFile(filepath.Join(workspace.featureGitDir, "ORIG_HEAD")); err != nil || strings.TrimSpace(string(got)) != ancestor {
		t.Fatalf("incomplete-recovery ORIG_HEAD = %q, %v; want preserved transaction value %q", got, err, ancestor)
	}
}

func TestLandingRecoveryDisablesConversionFiltersAndRestoresOrigHead(t *testing.T) {
	repo, ancestor := workspaceRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("filtered.txt filter=recovery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "filtered.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "add", ".gitattributes", "filtered.txt")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "filtered baseline")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	ancestor = gitcmdtest.Git(t, repo, "rev-parse", "HEAD^")
	marker := filepath.Join(t.TempDir(), "recovery-filter-ran")
	filter := filepath.Join(t.TempDir(), "smudge.sh")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\ntouch \""+marker+"\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "config", "filter.recovery.clean", "cat")
	gitcmdtest.Git(t, repo, "config", "filter.recovery.smudge", filter)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-filter-recovery")
	commitTestBatch(t, workspace, "filtered.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)
	origHeadPath := filepath.Join(workspace.featureGitDir, "ORIG_HEAD")
	origHeadBefore := []byte(original + "\n")
	if err := os.WriteFile(origHeadPath, origHeadBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", ancestor, original)
		return nil
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "recovery incomplete") {
		t.Fatalf("Land() = %q, %v; want incomplete race refusal", status, landErr)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conversion filter ran during recovery: %v", err)
	}
	if got, err := os.ReadFile(origHeadPath); err != nil || strings.TrimSpace(string(got)) != ancestor {
		t.Fatalf("incomplete-recovery ORIG_HEAD = %q, %v; want preserved transaction value %q", got, err, ancestor)
	}
}

func TestLandingSurfacesPrivateEvidenceCleanupFailureOnEarlyReturn(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-evidence-cleanup")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error { return errors.New("injected launch refusal") }
	workspace.landingEvidenceBeforeRemove = func() error { return errors.New("injected evidence cleanup failure") }

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "injected evidence cleanup failure") {
		t.Fatalf("Land() = %q, %v; want result-bearing evidence cleanup failure", status, landErr)
	}
}

func TestLandingReturnsCompleteWithPrivateEvidenceCleanupFailure(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-evidence-cleanup-complete")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.landingEvidenceBeforeRemove = func() error { return errors.New("injected post-landing evidence cleanup failure") }

	status, landErr := workspace.Land(context.Background(), squash)
	if status != LandingComplete || landErr == nil || !strings.Contains(landErr.Error(), "injected post-landing evidence cleanup failure") {
		t.Fatalf("Land() = %q, %v; want Complete with evidence cleanup failure", status, landErr)
	}
	if !workspace.landingComplete {
		t.Fatal("verified landing with cleanup failure was not recorded complete")
	}
}

func TestLandingRestoresRefMoveAfterGitReadsPreMergeHead(t *testing.T) {
	repo, ancestor := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-post-read-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refPath := filepath.Join(workspace.commonDir, "refs", "heads", "feature")
	lockPath := refPath + ".lock"
	raceDone := make(chan error, 1)
	workspace.beforeLandingStart = func() error {
		if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
			return err
		}
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				contents, readErr := os.ReadFile(filepath.Join(workspace.featureGitDir, "ORIG_HEAD"))
				if readErr == nil && strings.TrimSpace(string(contents)) == original {
					if writeErr := os.WriteFile(refPath, []byte(ancestor+"\n"), 0o600); writeErr != nil {
						raceDone <- writeErr
						return
					}
					raceDone <- os.Remove(lockPath)
					return
				}
				time.Sleep(time.Millisecond)
			}
			raceDone <- errors.New("Git did not record ORIG_HEAD")
		}()
		return nil
	}
	workspace.afterLandingMerge = func() error { return <-raceDone }

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "evidence") {
		t.Fatalf("Land() = %q, %v; want explicit incomplete post-read result", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != ancestor {
		t.Fatalf("concurrent feature ref = %q, want preserved %q", got, ancestor)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("run branch = %q, want preserved %q", got, squash)
	}
}

func TestLandingRejectsPairedRepositoryEvidenceForgery(t *testing.T) {
	repo, ancestor := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-paired-forgery")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", ancestor, original)
		return nil
	}
	workspace.afterLandingMerge = func() error {
		if err := os.WriteFile(filepath.Join(workspace.featureGitDir, "ORIG_HEAD"), []byte(original+"\n"), 0o600); err != nil {
			return err
		}
		logPath := filepath.Join(workspace.commonDir, "logs", "refs", "heads", "feature")
		log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		_, writeErr := fmt.Fprintf(log, "%s %s forged\n", original, squash)
		return errors.Join(writeErr, log.Close())
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "private transaction evidence") {
		t.Fatalf("Land() = %q, %v; want private-evidence refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("run branch = %q, want preserved %q", got, squash)
	}
}

func TestLandingRejectsSameInodePrivateHookRewrite(t *testing.T) {
	repo, ancestor := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-private-hook-forgery")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error {
		var hookPath string
		err := filepath.WalkDir(workspace.cacheRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Name() == "reference-transaction" {
				hookPath = path
			}
			return nil
		})
		if err != nil || hookPath == "" {
			return errors.Join(err, errors.New("private hook not found"))
		}
		originalHook, err := os.ReadFile(hookPath)
		if err != nil {
			return err
		}
		marker := []byte("printf '")
		start := bytes.Index(originalHook, marker)
		if start < 0 || start+len(marker)+64 > len(originalHook) {
			return errors.New("private hook nonce not found")
		}
		nonce := string(originalHook[start+len(marker) : start+len(marker)+64])
		forged := fmt.Sprintf("#!/bin/sh\nstage=$1\nwhile IFS=' ' read -r old new ref extra; do\n  if test \"$ref\" = refs/heads/feature; then printf '%s\\t%%s\\t%s\\t%s\\trefs/heads/feature\\n' \"$stage\" >&5; fi\ndone\n", nonce, original, squash)
		if err := os.WriteFile(hookPath, []byte(forged), 0o700); err != nil {
			return err
		}
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", ancestor, original)
		return nil
	}
	workspace.afterLandingMerge = func() error {
		return os.WriteFile(filepath.Join(workspace.featureGitDir, "ORIG_HEAD"), []byte(original+"\n"), 0o600)
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "private landing hook changed") {
		t.Fatalf("Land() = %q, %v; want same-inode hook-integrity refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("run branch = %q, want preserved %q", got, squash)
	}
}

func TestLandingRefusesTamperedPreMergeEvidence(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-evidence-tamper")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.afterLandingMerge = func() error {
		return os.WriteFile(filepath.Join(workspace.featureGitDir, "ORIG_HEAD"), []byte(strings.Repeat("0", 40)+"\n"), 0o600)
	}

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "pre-merge evidence") {
		t.Fatalf("Land() = %q, %v; want evidence refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("run branch = %q, want preserved %q", got, squash)
	}
}

func TestLandingReportsIncompleteRecoveryAndPreservesRunBranch(t *testing.T) {
	repo, ancestor := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
	original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-recovery-failure")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingStart = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", ancestor, original)
		return nil
	}
	workspace.beforeLandingRecovery = func() error { return errors.New("injected recovery failure") }

	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "recovery incomplete") {
		t.Fatalf("Land() = %q, %v; want incomplete recovery", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("run branch = %q, want preserved %q", got, squash)
	}
}

func TestLandingRecoveryPreservesConcurrentTrackedEdits(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string, string, string, *Workspace)
	}{
		{name: "concurrent pre-merge head", setup: func(t *testing.T, repo, original, ancestor string, workspace *Workspace) {
			workspace.beforeLandingStart = func() error {
				gitcmdtest.Git(t, repo, "update-ref", "refs/heads/feature", ancestor, original)
				return nil
			}
			workspace.beforeLandingRecovery = func() error {
				return os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("operator\n"), 0o600)
			}
		}},
		{name: "normal merge verification failure", setup: func(_ *testing.T, repo, _, _ string, workspace *Workspace) {
			workspace.afterLandingMerge = func() error {
				return os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("operator\n"), 0o600)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, ancestor := workspaceRepository(t)
			gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "admitted")
			original := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
			workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-recovery-edit-"+strings.ReplaceAll(test.name, " ", "-"))
			commitTestBatch(t, workspace, "feature.txt", "validated\n")
			squash, err := workspace.Squash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			test.setup(t, repo, original, ancestor, workspace)

			status, landErr := workspace.Land(context.Background(), squash)
			if landErr == nil || status != LandingBlocked || !strings.Contains(landErr.Error(), "recovery incomplete") {
				t.Fatalf("Land() = %q, %v; want incomplete recovery", status, landErr)
			}
			if got, err := os.ReadFile(filepath.Join(repo, "feature.txt")); err != nil || string(got) != "operator\n" {
				t.Fatalf("concurrent tracked edit = %q, %v; want preserved", got, err)
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
				t.Fatalf("run branch = %q, want preserved %q", got, squash)
			}
		})
	}
}

func TestLandingReturnsVerifiedCompleteWithAcknowledgementError(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-ack-error")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.afterLandingMerge = func() error { return errors.New("injected lost merge acknowledgement") }

	status, landErr := workspace.Land(context.Background(), squash)
	if status != LandingComplete || landErr == nil || !strings.Contains(landErr.Error(), "injected lost merge acknowledgement") {
		t.Fatalf("Land() = %q, %v; want verified Complete with acknowledgement error", status, landErr)
	}
	if !workspace.landingComplete {
		t.Fatal("verified landing was not recorded complete")
	}
}

func TestLandingReturnsVerifiedCompleteWithDescriptorCloseError(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-close-error")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.beforeLandingBindingClose = func() error { return errors.New("injected descriptor close failure") }

	status, landErr := workspace.Land(context.Background(), squash)
	if status != LandingComplete || landErr == nil || !strings.Contains(landErr.Error(), "injected descriptor close failure") {
		t.Fatalf("Land() = %q, %v; want verified Complete with descriptor close error", status, landErr)
	}
	if !workspace.landingComplete {
		t.Fatal("verified landing was not recorded complete")
	}
}

func TestCleanupLandedRemovesWorktreeThenOwnedRunRefAndIsIdempotent(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-landed")
	commitTestBatch(t, workspace, "feature.txt", "landed\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, landErr := workspace.Land(context.Background(), squash); landErr != nil || status != LandingComplete {
		t.Fatalf("Land() = %q, %v", status, landErr)
	}

	for range 2 {
		if err := workspace.Cleanup(context.Background(), CleanupLanded); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(workspace.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache worktree survived cleanup: %v", err)
	}
	if _, err := gitcmdtest.GitErr(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+workspace.branch); err == nil {
		t.Fatal("run branch survived landed cleanup")
	}
}

func TestCleanupLandedRequiresVerifiedLanding(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-unlanded")
	latest := commitTestBatch(t, workspace, "feature.txt", "validated\n")
	if err := workspace.Cleanup(context.Background(), CleanupLanded); err == nil {
		t.Fatal("CleanupLanded deleted artifacts without verified landing")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != latest {
		t.Fatalf("unlanded run ref = %q, want preserved %q", got, latest)
	}
	if _, err := os.Lstat(workspace.Path()); err != nil {
		t.Fatalf("unlanded worktree was removed: %v", err)
	}
}

func TestCleanupBlockedResetsEditsDiscardsValidationAndPreservesOnlyValidatedBranch(t *testing.T) {
	for _, validated := range []bool{false, true} {
		t.Run(map[bool]string{false: "no validated commit", true: "validated commit"}[validated], func(t *testing.T) {
			repo, original := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-preserve-"+map[bool]string{false: "no", true: "yes"}[validated])
			if validated {
				commitTestBatch(t, workspace, "feature.txt", "validated\n")
			}
			writeWorkspaceFile(t, workspace.Path(), "attempt.txt", "invalid\n")
			proof, err := workspace.PrepareBatch(context.Background(), mustChangedFiles(t, workspace))
			if err != nil {
				t.Fatal(err)
			}
			validationRoot := proof.ValidationRoot()
			if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(validationRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation snapshot survived cleanup: %v", err)
			}
			if _, err := os.Lstat(workspace.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree survived cleanup: %v", err)
			}
			_, refErr := gitcmdtest.GitErr(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+workspace.branch)
			exists := refErr == nil
			if exists != validated {
				t.Fatalf("run branch exists = %v, want %v", exists, validated)
			}
		})
	}
}

func TestCleanupRetryCompletesAfterPostRemovalFailure(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-partial")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	calls := 0
	workspace.afterWorktreeRemove = func() error {
		calls++
		if calls == 1 {
			return errors.New("injected post-removal failure")
		}
		return nil
	}
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err == nil {
		t.Fatal("Cleanup() did not surface partial removal failure")
	}
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err != nil {
		t.Fatalf("Cleanup() retry = %v", err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != workspace.green {
		t.Fatalf("preserved run branch = %q, want %q", got, workspace.green)
	}
}

func TestCleanupBlockedDoesNotDependOnMissingFeatureCheckoutPath(t *testing.T) {
	repo, commonRepo, original := linkedFeatureRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-missing-feature")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repo, repo+".operator"); err != nil {
		t.Fatal(err)
	}
	if status, err := workspace.Land(context.Background(), squash); err == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want missing-checkout refusal", status, err)
	}
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err != nil {
		t.Fatalf("Cleanup() after missing feature checkout = %v", err)
	}
	if _, err := os.Lstat(workspace.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache worktree survived cleanup: %v", err)
	}
	if got := gitcmdtest.Git(t, commonRepo, "rev-parse", "refs/heads/"+workspace.branch); got != squash {
		t.Fatalf("preserved run ref = %q, want %q", got, squash)
	}
}

func linkedFeatureRepository(t *testing.T) (string, string, string) {
	t.Helper()
	commonRepo := t.TempDir()
	gitcmdtest.Git(t, commonRepo, "init", "-q", "-b", "main")
	writeWorkspaceFile(t, commonRepo, "feature.txt", "original\n")
	gitcmdtest.Git(t, commonRepo, "add", "feature.txt")
	gitcmdtest.Git(t, commonRepo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "original")
	feature := filepath.Join(t.TempDir(), "feature")
	gitcmdtest.Git(t, commonRepo, "worktree", "add", "-q", "-b", "feature", feature, "HEAD")
	return feature, commonRepo, gitcmdtest.Git(t, feature, "rev-parse", "HEAD")
}

func TestCleanupRetryRecoversAmbiguousWorktreeRemoval(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-remove-ambiguous")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	workspace.removeWorktree = func(ctx context.Context) error {
		if err := workspace.removeWorktreeDirect(ctx); err != nil {
			return err
		}
		return errors.New("injected lost removal acknowledgement")
	}
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err == nil {
		t.Fatal("Cleanup() did not surface ambiguous removal")
	}
	workspace.removeWorktree = nil
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err != nil {
		t.Fatalf("Cleanup() retry = %v", err)
	}
}

func TestCleanupRefRacePreservesConcurrentRef(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-ref-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, err := workspace.Land(context.Background(), squash); err != nil || status != LandingComplete {
		t.Fatalf("Land() = %q, %v", status, err)
	}
	workspace.beforeCleanupRefDelete = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/"+workspace.branch, original, squash)
		return nil
	}
	if err := workspace.Cleanup(context.Background(), CleanupLanded); err == nil {
		t.Fatal("Cleanup() accepted concurrently moved run ref")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/"+workspace.branch); got != original {
		t.Fatalf("concurrent ref = %q, want preserved %q", got, original)
	}
}

func TestCleanupQuarantineNeverDeletesSubstitutedWorktreePath(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-substitution")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	moved := workspace.Path() + ".operator"
	workspace.beforeWorktreeQuarantine = func() error {
		if err := os.Rename(workspace.Path(), moved); err != nil {
			return err
		}
		if err := os.Mkdir(workspace.Path(), 0o700); err != nil {
			return err
		}
		control, err := os.ReadFile(filepath.Join(moved, ".git"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace.Path(), ".git"), control, 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(workspace.Path(), "victim.txt"), []byte("preserve\n"), 0o600)
	}
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err == nil {
		t.Fatal("Cleanup() accepted substituted worktree path")
	}
	if contents, err := os.ReadFile(filepath.Join(workspace.Path(), "victim.txt")); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("replacement victim = %q, %v", contents, err)
	}
}

func TestCleanupRegistrationRemovalNeverRecursivelyDeletesRecreatedPath(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-post-quarantine-race")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	workspace.beforeRegistrationRemove = func() error {
		if err := os.Mkdir(workspace.Path(), 0o700); err != nil {
			return err
		}
		control, err := os.ReadFile(filepath.Join(workspace.cleanupQuarantine.path, ".git"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace.Path(), ".git"), control, 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(workspace.Path(), "victim.txt"), []byte("preserve\n"), 0o600)
	}
	if err := workspace.Cleanup(context.Background(), CleanupPreserveValidated); err == nil {
		t.Fatal("Cleanup() accepted recreated registered path")
	}
	if contents, err := os.ReadFile(filepath.Join(workspace.Path(), "victim.txt")); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("recreated victim = %q, %v", contents, err)
	}
}

func TestCleanupRecoversAmbiguousSuccessfulRefDeletion(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "cleanup-delete-ambiguous")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, err := workspace.Land(context.Background(), squash); err != nil || status != LandingComplete {
		t.Fatalf("Land() = %q, %v", status, err)
	}
	workspace.deleteRunRef = func(ctx context.Context, ref, expected string) error {
		if err := workspace.deleteRunRefDirect(ctx, ref, expected); err != nil {
			return err
		}
		return errors.New("injected lost deletion acknowledgement")
	}
	if err := workspace.Cleanup(context.Background(), CleanupLanded); err != nil {
		t.Fatalf("Cleanup() ambiguous successful deletion = %v", err)
	}
	if _, err := gitcmdtest.GitErr(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+workspace.branch); err == nil {
		t.Fatalf("run ref at %q survived deletion", squash)
	}
}

func TestLandingUsesIndependentBoundedContextAfterAdmission(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-context")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, landErr := workspace.Land(ctx, squash)
	if landErr != nil || status != LandingComplete {
		t.Fatalf("Land(canceled after admission) = %q, %v", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != squash {
		t.Fatalf("admitted landing HEAD = %q, want %q", got, squash)
	}
	if landingGitTimeout != 30*time.Second {
		t.Fatalf("landing timeout = %s", landingGitTimeout)
	}
}

func TestLandingTransactionTimeoutBeforeMergePreservesOriginalCheckout(t *testing.T) {
	repo, original := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, original, filepath.Join(t.TempDir(), "workspace"), "landing-timeout")
	commitTestBatch(t, workspace, "feature.txt", "validated\n")
	squash, err := workspace.Squash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace.newLandingContext = func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, func() {}
	}
	status, landErr := workspace.Land(context.Background(), squash)
	if landErr == nil || status != LandingBlocked {
		t.Fatalf("Land() = %q, %v; want timeout refusal", status, landErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != original {
		t.Fatalf("timed-out landing moved HEAD to %q", got)
	}
}

func commitTestBatch(t *testing.T, workspace *Workspace, file, contents string) string {
	t.Helper()
	writeWorkspaceFile(t, workspace.Path(), file, contents)
	commit, err := workspace.CommitBatch(context.Background(), file, mustPrepareBatchProof(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	return commit
}
