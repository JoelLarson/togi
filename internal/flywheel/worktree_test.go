package flywheel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
)

func TestWorkspaceCreatesCleanExternalRunBranchAtOriginalHead(t *testing.T) {
	repo, head := workspaceRepository(t)
	cache := filepath.Join(t.TempDir(), "cache", "run-42")
	before := featureObservation(t, repo)

	workspace, err := CreateWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo,
		Path:           cache,
		RunID:          "42",
		OriginalHead:   head,
		FeatureBranch:  "feature",
		Identity:       Identity{Name: "Togi", Email: "togi@example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Path() != cache {
		t.Fatalf("Path() = %q, want %q", workspace.Path(), cache)
	}
	if got := gitcmdtest.Git(t, cache, "symbolic-ref", "--short", "HEAD"); got != "togi/run-42" {
		t.Fatalf("workspace branch = %q", got)
	}
	if got := gitcmdtest.Git(t, cache, "rev-parse", "HEAD"); got != head {
		t.Fatalf("workspace HEAD = %q, want %q", got, head)
	}
	if got := gitcmdtest.Git(t, cache, "status", "--porcelain=v2", "--untracked-files=all"); got != "" {
		t.Fatalf("workspace is dirty: %q", got)
	}
	if after := featureObservation(t, repo); !reflect.DeepEqual(after, before) {
		t.Fatalf("feature worktree changed: before=%q after=%q", before, after)
	}
}

func TestValidatedSnapshotBindsImmutableLatestGreenTree(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "validated-snapshot")
	snapshot, err := workspace.SnapshotValidated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	if got, err := os.ReadFile(filepath.Join(snapshot.Root(), "feature.txt")); err != nil || string(got) != "original\n" {
		t.Fatalf("snapshot feature = %q, %v", got, err)
	}
	if err := snapshot.Verify(context.Background()); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if err := os.Chmod(snapshot.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, snapshot.Root(), "injected.txt", "mutated\n")
	if err := snapshot.Verify(context.Background()); err == nil {
		t.Fatal("Verify accepted a mutated immutable snapshot")
	}
	if _, err := os.Stat(filepath.Join(workspace.Path(), "injected.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot mutation reached source workspace: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := workspace.SnapshotValidated(context.Background())
	if err != nil {
		t.Fatalf("next SnapshotValidated() error = %v", err)
	}
	writeWorkspaceFile(t, workspace.Path(), "source-drift.txt", "drift\n")
	if err := next.Verify(context.Background()); err == nil {
		t.Fatal("Verify accepted source-worktree drift")
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSquashValidatedConsumesOnlyOwnedUnchangedSnapshot(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "squash-validated")
	foreign := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "foreign"), "squash-foreign")
	snapshot, err := workspace.SnapshotValidated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	if _, err := foreign.SquashValidated(context.Background(), snapshot); err == nil {
		t.Fatal("SquashValidated accepted another workspace's proof")
	}
	writeWorkspaceFile(t, workspace.Path(), "source-drift.txt", "drift\n")
	if _, err := workspace.SquashValidated(context.Background(), snapshot); err == nil {
		t.Fatal("SquashValidated accepted source drift after validation")
	}
}

func TestWorkspaceRejectsInternalAndSymlinkedInternalPaths(t *testing.T) {
	repo, head := workspaceRepository(t)
	outside := t.TempDir()
	link := filepath.Join(outside, "repository-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(repo, "cache-worktree"),
		filepath.Join(repo, ".git", "cache-worktree"),
		filepath.Join(link, "missing", "cache-worktree"),
	} {
		_, err := CreateWorkspace(context.Background(), WorkspaceSpec{
			RepositoryRoot: repo, Path: path, RunID: "internal", OriginalHead: head,
			FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
		})
		if err == nil || !strings.Contains(err.Error(), "external") {
			t.Fatalf("CreateWorkspace(%q) error = %v, want external-path rejection", path, err)
		}
	}
}

func TestWorkspaceRejectsCollisionsAndInvalidRunIDs(t *testing.T) {
	repo, head := workspaceRepository(t)
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []WorkspaceSpec{
		{RepositoryRoot: repo, Path: path, RunID: "collision", OriginalHead: head, FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"}},
		{RepositoryRoot: repo, Path: filepath.Join(t.TempDir(), "run"), RunID: "../escape", OriginalHead: head, FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"}},
		{RepositoryRoot: repo, Path: filepath.Join(t.TempDir(), "run"), RunID: "bad\nref", OriginalHead: head, FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"}},
	}
	for _, spec := range tests {
		if _, err := CreateWorkspace(context.Background(), spec); err == nil {
			t.Fatalf("CreateWorkspace(%+v) succeeded, want rejection", spec)
		}
	}
}

func TestBatchChangedFilesAreSortedAndIncludeRenames(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "changed")
	if err := os.Rename(filepath.Join(workspace.Path(), "feature.txt"), filepath.Join(workspace.Path(), "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "zeta.txt", "zeta\n")
	writeWorkspaceFile(t, workspace.Path(), "alpha.txt", "alpha\n")

	got, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha.txt", "feature.txt", "renamed.txt", "zeta.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedFiles() = %q, want %q", got, want)
	}
}

func mustChangedFiles(t *testing.T, workspace *Workspace) []string {
	t.Helper()
	changed, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func mustPrepareBatchProof(t *testing.T, workspace *Workspace) BatchProof {
	t.Helper()
	proof, err := workspace.PrepareBatch(context.Background(), mustChangedFiles(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.discardValidationSnapshot(nil) })
	return proof
}

func TestBatchChangedFilesIncludesBothPathsForStagedRename(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "staged-rename")
	gitcmdtest.Git(t, workspace.Path(), "mv", "--", "feature.txt", "renamed file.txt")

	got, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feature.txt", "renamed file.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedFiles() = %q, want %q", got, want)
	}
}

func TestBatchPorcelainV2CopyIncludesBytePathsAndDeduplicates(t *testing.T) {
	hash := strings.Repeat("0", 40)
	destination := "copied file\xff.txt"
	source := "source file\xfe.txt"
	raw := []byte("2 C. N... 100644 100644 100644 " + hash + " " + hash + " C100 " + destination + "\x00" + source + "\x00? " + destination + "\x00")

	got, err := parseChangedFilesV2(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{destination, source}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseChangedFilesV2() = %q, want %q", got, want)
	}
}

func TestBatchPorcelainV2RejectsRenameWithoutSource(t *testing.T) {
	hash := strings.Repeat("0", 40)
	raw := []byte("2 R. N... 100644 100644 100644 " + hash + " " + hash + " R100 renamed.txt\x00")
	if _, err := parseChangedFilesV2(raw); err == nil {
		t.Fatal("parseChangedFilesV2 accepted rename without source")
	}
}

func TestBatchPorcelainV2RequiresStrictFramingAndMetadata(t *testing.T) {
	hash := strings.Repeat("0", 40)
	invalid := [][]byte{
		[]byte("? unterminated.txt"),
		[]byte("? first.txt\x00\x00? second.txt\x00"),
		[]byte("1 M. BAD 100644 100644 100644 " + hash + " " + hash + " file.txt\x00"),
		[]byte("2 R. N... 100644 100644 100644 " + hash + " " + hash + " X100 new.txt\x00old.txt\x00"),
		[]byte("! ignored.txt\x00"),
		[]byte("1x M. N... 100644 100644 100644 " + hash + " " + hash + " file.txt\x00"),
		[]byte("2x R. N... 100644 100644 100644 " + hash + " " + hash + " R100 new.txt\x00old.txt\x00"),
	}
	for _, raw := range invalid {
		if _, err := parseChangedFilesV2(raw); err == nil {
			t.Fatalf("parseChangedFilesV2(%q) succeeded", raw)
		}
	}
}

func TestBatchPorcelainV2AllowsNewlineAndTabInRelativePaths(t *testing.T) {
	want := []string{"line\nname.txt", "tab\tname.txt"}
	raw := []byte("? " + want[0] + "\x00? " + want[1] + "\x00")
	got, err := parseChangedFilesV2(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseChangedFilesV2() = %q, want %q", got, want)
	}
}

func TestBatchChangedFilesIncludesStagedCopySourceAndDestination(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "staged-copy")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "original\nmodified\n")
	writeWorkspaceFile(t, workspace.Path(), "copied file.txt", "original\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt", "copied file.txt")

	got, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"copied file.txt", "feature.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedFiles() = %q, want %q", got, want)
	}
}

func TestBatchResetAttemptRestoresLatestGreenCommit(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "reset")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	writeWorkspaceFile(t, workspace.Path(), ".gitignore", "ignored.log\n")
	green, err := workspace.CommitBatch(context.Background(), "feature.txt", mustPrepareBatchProof(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "invalid\n")
	writeWorkspaceFile(t, workspace.Path(), "untracked.txt", "invalid\n")
	writeWorkspaceFile(t, workspace.Path(), "ignored.log", "invalid\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")

	if err := workspace.ResetAttempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD"); got != green {
		t.Fatalf("HEAD after reset = %q, want latest green %q", got, green)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "status", "--porcelain=v2", "--untracked-files=all"); got != "" {
		t.Fatalf("status after reset = %q, want clean", got)
	}
	if got, err := os.ReadFile(filepath.Join(workspace.Path(), "feature.txt")); err != nil || string(got) != "validated\n" {
		t.Fatalf("feature contents after reset = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path(), "untracked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untracked file survived reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path(), "ignored.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored untracked file survived reset: %v", err)
	}
}

func TestBatchRollbackRestoresExactJustCommittedGreenState(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "rollback-batch")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	commit, err := workspace.CommitBatch(context.Background(), "feature.txt", mustPrepareBatchProof(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.RollbackBatch(context.Background(), commit); err != nil {
		t.Fatal(err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD=%q want %q", got, head)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "status", "--porcelain=v2", "--untracked-files=all"); got != "" {
		t.Fatalf("status=%q", got)
	}
	if got, err := os.ReadFile(filepath.Join(workspace.Path(), "feature.txt")); err != nil || string(got) != "original\n" {
		t.Fatalf("feature=%q err=%v", got, err)
	}
	if err := workspace.RollbackBatch(context.Background(), commit); err == nil {
		t.Fatal("RollbackBatch accepted a commit that is no longer current")
	}
}

func TestBatchRollbackPreservesConcurrentlyMovedRunRef(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "rollback-moved")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	commit, err := workspace.CommitBatch(context.Background(), "feature.txt", mustPrepareBatchProof(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	tree := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD^{tree}")
	moved := gitcmdtest.Git(t, repo, "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid", "commit-tree", tree, "-p", commit, "-m", "concurrent move")
	gitcmdtest.Git(t, repo, "update-ref", "refs/heads/togi/run-rollback-moved", moved, commit)
	if err := workspace.RollbackBatch(context.Background(), commit); err == nil {
		t.Fatal("RollbackBatch rewound concurrently moved ref")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-rollback-moved"); got != moved {
		t.Fatalf("run ref=%q want %q", got, moved)
	}
}

func TestBatchResetAttemptDoesNotRewindMovedRunRef(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "moved-reset")
	tree := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD^{tree}")
	moved := gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid", "commit-tree", tree, "-p", head, "-m", "concurrent move")
	gitcmdtest.Git(t, workspace.Path(), "update-ref", "refs/heads/togi/run-moved-reset", moved, head)

	if err := workspace.ResetAttempt(context.Background()); err == nil {
		t.Fatal("ResetAttempt succeeded after the run ref moved")
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "refs/heads/togi/run-moved-reset"); got != moved {
		t.Fatalf("moved run ref was rewound to %q, want %q", got, moved)
	}
}

func TestBatchResetAttemptPreservesSymbolicRunRefAndOperatorTarget(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "symbolic-reset")
	gitcmdtest.Git(t, repo, "branch", "operator-reset-target", head)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-reset", "refs/heads/operator-reset-target")

	if err := workspace.ResetAttempt(context.Background()); err == nil {
		t.Fatal("ResetAttempt accepted a symbolic run ref")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/operator-reset-target"); got != head {
		t.Fatalf("operator reset target moved to %q, want %q", got, head)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-reset"); got != "refs/heads/operator-reset-target" {
		t.Fatalf("symbolic run ref was rewritten to %q", got)
	}
}

func TestBatchResetAttemptRejectsRepointedCopiedGitDir(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "repointed-reset")
	copiedGitDir := filepath.Join(workspace.commonDir, "worktrees", "repointed-reset-copy")
	copyTestTree(t, workspace.gitDir, copiedGitDir)
	if err := os.WriteFile(filepath.Join(workspace.Path(), ".git"), []byte("gitdir: "+copiedGitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := workspace.ResetAttempt(context.Background()); err == nil {
		t.Fatal("ResetAttempt accepted a repointed copied Git directory")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-repointed-reset"); got != head {
		t.Fatalf("run ref moved to %q, want %q", got, head)
	}
}

func TestBatchResetAttemptRejectsSamePathWorktreeRootReplacement(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "root-replacement")
	originalRoot := workspace.Path() + "-original"
	if err := os.Rename(workspace.Path(), originalRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(originalRoot, ".git"), filepath.Join(workspace.Path(), ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path(), "feature.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := workspace.ResetAttempt(context.Background()); err == nil {
		t.Fatal("ResetAttempt accepted a replaced worktree root inode")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-root-replacement"); got != head {
		t.Fatalf("run ref moved to %q, want %q", got, head)
	}
}

func TestResetAttemptRechecksRunRefAtCompletion(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "reset-final-ref")
	operatorCommit := gitcmdtest.Git(t, repo, "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid",
		"commit-tree", head+"^{tree}", "-p", head, "-m", "operator")
	workspace.beforeResetFinal = func() error {
		gitcmdtest.Git(t, repo, "update-ref", "refs/heads/togi/run-reset-final-ref", operatorCommit, head)
		return nil
	}
	if err := workspace.ResetAttempt(context.Background()); err == nil {
		t.Fatal("ResetAttempt returned success after its run ref moved at completion")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-reset-final-ref"); got != operatorCommit {
		t.Fatalf("operator ref = %q, want preserved %q", got, operatorCommit)
	}
}

func TestWorkspaceOperationsRejectSymlinkReboundRootToSameInode(t *testing.T) {
	for _, operation := range []string{"reset", "commit", "snapshot", "check"} {
		t.Run(operation, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "root-symlink-"+operation)
			var before GitState
			var err error
			if operation == "check" {
				before, err = workspace.SnapshotGitState(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			if operation == "commit" {
				writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
			}
			var proof BatchProof
			if operation == "commit" {
				proof = mustPrepareBatchProof(t, workspace)
			}
			moved := workspace.Path() + "-moved"
			if err := os.Rename(workspace.Path(), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, workspace.Path()); err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "reset":
				err = workspace.ResetAttempt(context.Background())
			case "commit":
				_, err = workspace.CommitBatch(context.Background(), "feature.txt", proof)
			case "snapshot":
				_, err = workspace.SnapshotGitState(context.Background())
			case "check":
				err = workspace.CheckGitState(context.Background(), before)
			}
			if err == nil {
				t.Fatalf("%s accepted a symlink-rebound workspace root", operation)
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-root-symlink-"+operation); got != head {
				t.Fatalf("run ref moved to %q, want %q", got, head)
			}
		})
	}
}

func TestOperationsRejectSameByteIndexInodeReplacement(t *testing.T) {
	for _, operation := range []string{"reset", "commit", "check"} {
		t.Run(operation, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "index-inode-"+operation)
			var before GitState
			if operation == "check" {
				var err error
				before, err = workspace.SnapshotGitState(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			if operation == "commit" {
				writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
			}
			var proof BatchProof
			if operation == "commit" {
				proof = mustPrepareBatchProof(t, workspace)
			}
			indexPath := filepath.Join(workspace.gitDir, "index")
			contents, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Lstat(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			replacement := indexPath + ".replacement"
			if err := os.WriteFile(replacement, contents, originalInfo.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, indexPath); err != nil {
				t.Fatal(err)
			}
			replacementInfo, err := os.Lstat(indexPath)
			if err != nil {
				t.Fatal(err)
			}

			switch operation {
			case "reset":
				err = workspace.ResetAttempt(context.Background())
			case "commit":
				_, err = workspace.CommitBatch(context.Background(), "feature.txt", proof)
			case "check":
				err = workspace.CheckGitState(context.Background(), before)
			}
			if err == nil {
				t.Fatalf("%s accepted a byte-identical replacement index inode", operation)
			}
			if operation == "check" && !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("check error = %v, want incomplete identity-only violation", err)
			}
			currentInfo, statErr := os.Lstat(indexPath)
			if statErr != nil || !os.SameFile(replacementInfo, currentInfo) {
				t.Fatalf("%s replaced the operator index inode: %v", operation, statErr)
			}
		})
	}
}

func TestGitStatePreservesIndexTimestampOnlyMutation(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "index-timestamp")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changedTime := before.indexInfo.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(workspace.indexPath, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	mutatedInfo, err := os.Lstat(workspace.indexPath)
	if err != nil {
		t.Fatal(err)
	}

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "index") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want incomplete timestamp-only index violation", err)
	}
	currentInfo, statErr := os.Lstat(workspace.indexPath)
	if statErr != nil || !os.SameFile(mutatedInfo, currentInfo) || !currentInfo.ModTime().Equal(changedTime) {
		t.Fatalf("operator index timestamp was not preserved: %v", statErr)
	}
}

func TestOwnedIndexMutationRejectsSubstitutionAfterGitCommand(t *testing.T) {
	for _, operation := range []string{"reset", "commit"} {
		t.Run(operation, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "index-race-"+operation)
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
			workspace.beforeIndexInstall = func() error {
				contents, err := os.ReadFile(workspace.indexPath)
				if err != nil {
					return err
				}
				replacement := workspace.indexPath + ".operator"
				if err := os.WriteFile(replacement, contents, 0o600); err != nil {
					return err
				}
				return os.Rename(replacement, workspace.indexPath)
			}

			var err error
			if operation == "reset" {
				err = workspace.ResetAttempt(context.Background())
			} else {
				_, err = workspace.PrepareBatch(context.Background(), mustChangedFiles(t, workspace))
			}
			if err == nil {
				t.Fatalf("%s accepted an index substitution after its Git command", operation)
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-index-race-"+operation); got != head {
				t.Fatalf("run ref = %q, want %q", got, head)
			}
		})
	}
}

func TestPrivateGitStateDirectoryIgnoresAmbientTempAndIsOwnerOnly(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("TMPDIR", ambient)
	root := t.TempDir()
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	private, err := openPrivateTempDir(root, rootInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer private.close()
	if filepath.Dir(private.path) != root {
		t.Fatalf("private directory parent = %q, want %q", filepath.Dir(private.path), root)
	}
	if private.info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory mode = %o, want 700", private.info.Mode().Perm())
	}
	file, filePath, err := private.createFile("index")
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	defer private.removeFile(filepath.Base(filePath), fileInfo)
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private file mode = %o, want 600", fileInfo.Mode().Perm())
	}
}

func TestPrivateGitStateRejectsReboundCacheParent(t *testing.T) {
	repo, head := workspaceRepository(t)
	cacheParent := t.TempDir()
	workspace := createTestWorkspace(t, repo, head, filepath.Join(cacheParent, "workspace"), "cache-parent-swap")
	movedParent := filepath.Join(repo, "operator-cache")
	if err := os.Rename(cacheParent, movedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(movedParent, cacheParent); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.createPrivateTempDir(); err == nil {
		t.Fatal("private state creation accepted a symlink-rebound cache parent")
	}
	entries, err := os.ReadDir(movedParent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".togi-private-") {
			t.Fatalf("private state was written through rebound parent: %q", entry.Name())
		}
	}
}

func TestPrivateGitStateRootRefusesSymlinkWithoutChangingTargetMode(t *testing.T) {
	target := t.TempDir()
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openPrivateTempDir(link, targetInfo); err == nil {
		t.Fatal("private state root accepted a symlink")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != targetInfo.Mode().Perm() {
		t.Fatalf("target mode changed from %o to %o", targetInfo.Mode().Perm(), after.Mode().Perm())
	}
}

func TestPrivateGitStateRechecksParentAfterOpeningRoot(t *testing.T) {
	root := t.TempDir()
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	_, err = openPrivateTempDirWithHooks(root, rootInfo, privateTempHooks{afterParentOpen: func() error {
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		return os.Symlink(moved, root)
	}})
	if err == nil {
		t.Fatal("private state creation accepted a parent path changed to a symlink")
	}
	entries, readErr := os.ReadDir(moved)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("private state was created before parent path recheck: %v", entries)
	}
}

func TestPrivateGitStateRechecksCreatedDirectoryAfterOpeningRoot(t *testing.T) {
	root := t.TempDir()
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "operator")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = openPrivateTempDirWithHooks(root, rootInfo, privateTempHooks{afterDirectorySample: func(name string) error {
		if err := os.Rename(filepath.Join(root, name), filepath.Join(root, name+"-owned")); err != nil {
			return err
		}
		return os.Symlink("operator", filepath.Join(root, name))
	}})
	if err == nil {
		t.Fatal("private state creation accepted a replaced child directory")
	}
	entries, readErr := os.ReadDir(sibling)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("private state was written through replaced child: %v", entries)
	}
}

func TestBatchCommitStagesWholeTreeUsesIdentityAndDisablesHooks(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "commit")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
	writeWorkspaceFile(t, workspace.Path(), "other.txt", "other\n")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 91\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	commit, err := workspace.CommitBatch(context.Background(), "feature.txt", mustPrepareBatchProof(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	if commit == head {
		t.Fatal("CommitBatch did not advance HEAD")
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "show", "-s", "--format=%s"); got != "togi batch: feature.txt" {
		t.Fatalf("subject = %q", got)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "show", "-s", "--format=%an <%ae>|%cn <%ce>"); got != "Togi <togi@example.invalid>|Togi <togi@example.invalid>" {
		t.Fatalf("identity = %q", got)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "show", "--format=", "--name-only", "HEAD"); !strings.Contains(got, "feature.txt") || !strings.Contains(got, "other.txt") {
		t.Fatalf("commit does not contain whole observed tree: %q", got)
	}
}

func TestBatchProofCommitsExactlyPreparedTreeWithoutRestaging(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-commit")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	writeWorkspaceFile(t, workspace.Path(), "other.txt", "other\n")
	changed, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := workspace.PrepareBatch(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := workspace.CommitBatch(context.Background(), "feature.txt", proof)
	if err != nil {
		t.Fatal(err)
	}
	if commit == head {
		t.Fatal("CommitBatch did not advance the prepared tree")
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "show", "HEAD:feature.txt"); got != "validated" {
		t.Fatalf("committed feature = %q", got)
	}
}

func TestBatchProofMaterializesReadOnlyValidationSnapshot(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-validation-snapshot")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged bytes\n")
	proof := mustPrepareBatchProof(t, workspace)
	root := proof.ValidationRoot()
	if root == "" || root == workspace.Path() || filepath.Dir(root) != workspace.cacheRoot {
		t.Fatalf("ValidationRoot() = %q, want cryptorandom private sibling", root)
	}
	contents, err := os.ReadFile(filepath.Join(root, "feature.txt"))
	if err != nil || string(contents) != "staged bytes\n" {
		t.Fatalf("validation feature = %q, %v", contents, err)
	}
	fileInfo, err := os.Lstat(filepath.Join(root, "feature.txt"))
	if err != nil || fileInfo.Mode().Perm()&0o222 != 0 {
		t.Fatalf("validation file mode = %v, %v; want read-only", fileInfo.Mode(), err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode().Perm()&0o222 != 0 {
		t.Fatalf("validation root mode = %v, %v; want read-only", rootInfo.Mode(), err)
	}

	if _, err := workspace.CommitBatch(context.Background(), "feature.txt", proof); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation snapshot survived commit: %v", err)
	}
}

func TestBatchProofValidationSnapshotIgnoresExportAttributes(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-raw-tree")
	writeWorkspaceFile(t, workspace.Path(), ".gitattributes", "hidden.go export-ignore\nsubstitute.txt export-subst\n")
	writeWorkspaceFile(t, workspace.Path(), "hidden.go", "package hidden\n")
	writeWorkspaceFile(t, workspace.Path(), "substitute.txt", "$Format:%H$\n")
	proof := mustPrepareBatchProof(t, workspace)
	for name, want := range map[string]string{
		"hidden.go":      "package hidden\n",
		"substitute.txt": "$Format:%H$\n",
	} {
		contents, err := os.ReadFile(filepath.Join(proof.ValidationRoot(), name))
		if err != nil || string(contents) != want {
			t.Fatalf("validation %s = %q, %v; want raw staged bytes", name, contents, err)
		}
	}
}

func TestBatchProofRetainsIncompleteSnapshotForResetCleanup(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-cleanup-retry")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
	workspace.validationMaterializeFailure = func() error { return errors.New("injected materialization failure") }
	discardCalls := 0
	workspace.validationBeforePrivateRemove = func() error {
		discardCalls++
		if discardCalls == 1 {
			return errors.New("injected cleanup failure")
		}
		return nil
	}
	if _, err := workspace.PrepareBatch(context.Background(), mustChangedFiles(t, workspace)); err == nil || !strings.Contains(err.Error(), "discard incomplete") {
		t.Fatalf("PrepareBatch() error = %v, want cleanup failure", err)
	}
	if workspace.validationSnapshot == nil {
		t.Fatal("failed cleanup lost ownership of incomplete validation snapshot")
	}
	root := workspace.validationSnapshot.private.path
	if err := workspace.ResetAttempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete snapshot survived reset: %v", err)
	}
}

func TestBatchProofCleanupRejectsPrivateDirectorySubstitution(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-cleanup-substitution")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
	workspace.validationMaterializeFailure = func() error { return errors.New("injected materialization failure") }
	var owned, moved, replacement string
	swapped := false
	workspace.validationBeforePrivateRemove = func() error {
		if swapped {
			return nil
		}
		swapped = true
		entries, err := os.ReadDir(workspace.cacheRoot)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".togi-private-") {
				owned = filepath.Join(workspace.cacheRoot, entry.Name())
				break
			}
		}
		moved = owned + ".moved"
		if owned == "" || os.Rename(owned, moved) != nil {
			t.Fatal("rename owned private directory")
		}
		if err := os.Mkdir(owned, 0o700); err != nil {
			t.Fatal(err)
		}
		replacement = owned
		return nil
	}
	if _, err := workspace.PrepareBatch(context.Background(), mustChangedFiles(t, workspace)); err == nil || !strings.Contains(err.Error(), "discard incomplete") {
		t.Fatalf("PrepareBatch() error = %v, want binding failure", err)
	}
	replacementInfo, err := os.Lstat(replacement)
	if err != nil {
		t.Fatalf("replacement was deleted: %v", err)
	}
	if err := os.Remove(replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, owned); err != nil {
		t.Fatal(err)
	}
	if err := workspace.ResetAttempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned snapshot survived retry: %v (replacement inode %v)", err, replacementInfo)
	}
}

func TestBatchProofPreparationRejectsMutationBetweenStagingAndCapture(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-prepare-race")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "malicious\n")
	workspace.beforeIndexInstall = func() error {
		workspace.beforeIndexInstall = nil
		return os.WriteFile(filepath.Join(workspace.Path(), "feature.txt"), []byte("safe\n"), 0o644)
	}
	changed, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.PrepareBatch(context.Background(), changed); err == nil {
		t.Fatal("PrepareBatch accepted a staged tree different from captured worktree bytes")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-proof-prepare-race"); got != head {
		t.Fatalf("run ref = %q, want %q", got, head)
	}
}

func TestBatchProofRejectsIgnoredEntriesAtPreparation(t *testing.T) {
	repo, head := workspaceRepositoryWithIgnoredGenerated(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-ignored-baseline")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	if err := os.Mkdir(filepath.Join(workspace.Path(), "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "generated/output.txt", "ignored\n")
	changed, err := workspace.ChangedFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.PrepareBatch(context.Background(), changed); err == nil {
		t.Fatal("PrepareBatch accepted an unexpected ignored entry")
	}
}

func TestBatchProofRejectsUnrepresentableEmptyDirectoriesAtPreparation(t *testing.T) {
	for _, directory := range []string{"empty", "generated", "outer/inner"} {
		t.Run(directory, func(t *testing.T) {
			repo, head := workspaceRepositoryWithIgnoredGenerated(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-empty-"+strings.ReplaceAll(directory, "/", "-"))
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
			if err := os.MkdirAll(filepath.Join(workspace.Path(), filepath.FromSlash(directory)), 0o755); err != nil {
				t.Fatal(err)
			}
			changed, err := workspace.ChangedFiles(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := workspace.PrepareBatch(context.Background(), changed); err == nil {
				t.Fatal("PrepareBatch accepted a directory absent from the staged tree")
			}
		})
	}
}

func TestBatchProofAllowsDirectoryRepresentedByTrackedFile(t *testing.T) {
	repo, _ := workspaceRepository(t)
	if err := os.Mkdir(filepath.Join(repo, "tracked"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, repo, "tracked/kept.txt", "kept\n")
	gitcmdtest.Git(t, repo, "add", "--", "tracked/kept.txt")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "track directory")
	head := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-tracked-directory")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	_ = mustPrepareBatchProof(t, workspace)
}

func TestBatchProofRejectsIgnoredEntriesAfterPreparation(t *testing.T) {
	for _, name := range []string{"generated/output.txt", "existing/ignored.txt"} {
		t.Run(name, func(t *testing.T) {
			repo, head := workspaceRepositoryWithIgnoredGenerated(t)
			if strings.HasPrefix(name, "existing/") {
				if err := os.Mkdir(filepath.Join(repo, "existing"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeWorkspaceFile(t, repo, "existing/kept.txt", "kept\n")
				gitcmdtest.Git(t, repo, "add", "--", "existing/kept.txt")
				gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "track existing directory")
				head = gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
			}
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-ignored-"+strings.ReplaceAll(name, "/", "-"))
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
			proof := mustPrepareBatchProof(t, workspace)
			if strings.HasPrefix(name, "generated/") {
				if err := os.Mkdir(filepath.Join(workspace.Path(), filepath.Dir(name)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeWorkspaceFile(t, workspace.Path(), name, "ignored\n")
			if err := workspace.VerifyBatch(context.Background(), proof); err == nil {
				t.Fatal("VerifyBatch accepted an ignored entry created after preparation")
			}
			if _, err := workspace.CommitBatch(context.Background(), "feature.txt", proof); err == nil {
				t.Fatal("CommitBatch accepted an ignored entry created after validation")
			}
		})
	}
}

func TestBatchProofRejectsPostValidationMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Workspace)
	}{
		{name: "same bytes restored", mutate: func(t *testing.T, workspace *Workspace) {
			path := filepath.Join(workspace.Path(), "feature.txt")
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "tampered!\n")
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
			if _, err := os.Lstat(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "new untracked path", mutate: func(t *testing.T, workspace *Workspace) {
			writeWorkspaceFile(t, workspace.Path(), "late.txt", "late\n")
		}},
		{name: "directory symlink swap", mutate: func(t *testing.T, workspace *Workspace) {
			directory := filepath.Join(workspace.Path(), "pkg")
			backup := filepath.Join(workspace.Path(), "pkg-real")
			if err := os.Rename(directory, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(backup, directory); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "index replacement", mutate: func(t *testing.T, workspace *Workspace) {
			contents, err := os.ReadFile(workspace.indexPath)
			if err != nil {
				t.Fatal(err)
			}
			replacement := workspace.indexPath + ".replacement"
			if err := os.WriteFile(replacement, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, workspace.indexPath); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-"+strings.ReplaceAll(test.name, " ", "-"))
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
			if err := os.Mkdir(filepath.Join(workspace.Path(), "pkg"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, workspace.Path(), "pkg/value.go", "package pkg\n")
			changed, err := workspace.ChangedFiles(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			proof, err := workspace.PrepareBatch(context.Background(), changed)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = workspace.discardValidationSnapshot(nil) })
			test.mutate(t, workspace)
			if _, err := workspace.CommitBatch(context.Background(), "feature.txt", proof); err == nil {
				t.Fatal("CommitBatch accepted worktree or index drift after validation")
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-proof-"+strings.ReplaceAll(test.name, " ", "-")); got != head {
				t.Fatalf("run ref = %q, want %q", got, head)
			}
		})
	}
}

func TestBatchProofRejectsMutationDuringCommit(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*Workspace, func() error)
	}{
		{name: "before ref update", set: func(workspace *Workspace, mutate func() error) { workspace.beforeBatchRefUpdate = mutate }},
		{name: "after ref update", set: func(workspace *Workspace, mutate func() error) { workspace.afterBatchRefUpdate = mutate }},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-during-"+strings.ReplaceAll(test.name, " ", "-"))
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
			proof := mustPrepareBatchProof(t, workspace)
			test.set(workspace, func() error {
				return os.WriteFile(filepath.Join(workspace.Path(), "late.txt"), []byte("late\n"), 0o644)
			})

			if _, err := workspace.CommitBatch(context.Background(), "feature.txt", proof); err == nil {
				t.Fatal("CommitBatch accepted a commit-time worktree mutation")
			}
			ref := "refs/heads/togi/run-proof-during-" + strings.ReplaceAll(test.name, " ", "-")
			if got := gitcmdtest.Git(t, repo, "rev-parse", ref); got != head {
				t.Fatalf("run ref = %q, want original %q", got, head)
			}
		})
	}
}

func TestBatchProofRollsBackRefWhenCancellationFollowsUpdate(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-canceled-update")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	proof := mustPrepareBatchProof(t, workspace)
	ctx, cancel := context.WithCancel(context.Background())
	workspace.afterBatchRefUpdate = func() error {
		cancel()
		return ctx.Err()
	}

	if _, err := workspace.CommitBatch(ctx, "feature.txt", proof); err == nil {
		t.Fatal("CommitBatch accepted cancellation after ref update")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-proof-canceled-update"); got != head {
		t.Fatalf("run ref = %q, want rolled back %q", got, head)
	}
}

func TestBatchProofReconcilesAmbiguousRefUpdateFailure(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "proof-ambiguous-update")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "validated\n")
	proof := mustPrepareBatchProof(t, workspace)
	workspace.updateBatchRef = func(ctx context.Context, ref, next, previous string) error {
		if err := workspace.updateBatchRefDirect(ctx, ref, next, previous); err != nil {
			return err
		}
		return errors.New("lost update-ref result")
	}

	if _, err := workspace.CommitBatch(context.Background(), "feature.txt", proof); err == nil {
		t.Fatal("CommitBatch accepted an ambiguous ref update result")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-proof-ambiguous-update"); got != head {
		t.Fatalf("run ref = %q, want reconciled %q", got, head)
	}
}

func TestBatchCommitPreservesSymbolicRunRefAndOperatorTarget(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "symbolic-commit")
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
	gitcmdtest.Git(t, repo, "branch", "operator-commit-target", head)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-commit", "refs/heads/operator-commit-target")

	if _, err := workspace.CommitBatch(context.Background(), "feature.txt", BatchProof{}); err == nil {
		t.Fatal("CommitBatch accepted a symbolic run ref")
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/operator-commit-target"); got != head {
		t.Fatalf("operator commit target moved to %q, want %q", got, head)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-commit"); got != "refs/heads/operator-commit-target" {
		t.Fatalf("symbolic commit run ref was rewritten to %q", got)
	}
}

func TestWorkspaceCreationRejectsDanglingSymbolicRunRefCollision(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-collision", "refs/heads/operator-missing")
	_, err := CreateWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: filepath.Join(t.TempDir(), "workspace"), RunID: "symbolic-collision", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateWorkspace error = %v, want dangling symbolic collision", err)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-collision"); got != "refs/heads/operator-missing" {
		t.Fatalf("dangling symbolic collision was rewritten to %q", got)
	}
}

func TestTogiRefMutationsDisableReferenceTransactionHook(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "hook-reset")
		marker := installReferenceTransactionHook(t, repo)
		if err := workspace.ResetAttempt(context.Background()); err != nil {
			t.Fatalf("ResetAttempt with target reference hook = %v", err)
		}
		assertHookNotRun(t, marker)
	})

	t.Run("commit", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "hook-commit")
		marker := installReferenceTransactionHook(t, repo)
		writeWorkspaceFile(t, workspace.Path(), "feature.txt", "changed\n")
		if _, err := workspace.CommitBatch(context.Background(), "feature.txt", mustPrepareBatchProof(t, workspace)); err != nil {
			t.Fatalf("CommitBatch with target reference hook = %v", err)
		}
		assertHookNotRun(t, marker)
	})

	t.Run("integrity restoration", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "hook-restore")
		marker := installReferenceTransactionHook(t, repo)
		before, err := workspace.SnapshotGitState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		writeWorkspaceFile(t, workspace.Path(), "agent.txt", "agent\n")
		gitcmdtest.Git(t, workspace.Path(), "add", "--", "agent.txt")
		gitcmdtest.Git(t, workspace.Path(), "-c", "core.hooksPath="+os.DevNull, "-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "-qm", "agent")
		if err := workspace.CheckGitState(context.Background(), before); err == nil || strings.Contains(err.Error(), "restoration incomplete") {
			t.Fatalf("CheckGitState with target reference hook = %v", err)
		}
		assertHookNotRun(t, marker)
	})

	t.Run("failed creation cleanup", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		marker := installReferenceTransactionHook(t, repo)
		_, err := createWorkspace(context.Background(), WorkspaceSpec{
			RepositoryRoot: repo, Path: filepath.Join(t.TempDir(), "workspace"), RunID: "hook-cleanup", OriginalHead: head,
			FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
		}, workspaceCreateHooks{add: func(_ context.Context, repositoryRoot, _ string, branch, originalHead string) error {
			gitcmdtest.Git(t, repositoryRoot, "-c", "core.hooksPath="+os.DevNull, "branch", branch, originalHead)
			return errors.New("injected add failure")
		}})
		if err == nil {
			t.Fatal("createWorkspace unexpectedly succeeded")
		}
		if got := gitcmdtest.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/togi/run-hook-cleanup"); got != "" {
			t.Fatalf("owned cleanup branch remains: %q", got)
		}
		assertHookNotRun(t, marker)
	})
}

func installReferenceTransactionHook(t *testing.T, repo string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "reference-hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "reference-transaction")
	script := "#!/bin/sh\nprintf invoked > \"" + marker + "\"\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return marker
}

func assertHookNotRun(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reference-transaction hook ran: %v", err)
	}
}

func TestGitStateAllowsOnlyUnstagedWorktreeEdits(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "plain-edit")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "proposal\n")
	if err := workspace.CheckGitState(context.Background(), before); err != nil {
		t.Fatalf("ordinary worktree edit rejected: %v", err)
	}
}

func TestGitStateRejectsAndUnstagesAgentStaging(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "staging")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("CheckGitState error = %v, want index violation", err)
	}
	var stateErr *GitStateCheckError
	if !errors.Is(err, ErrGitStateRestored) || errors.Is(err, ErrGitStateUnsafe) || !errors.As(err, &stateErr) || !stateErr.Restored {
		t.Fatalf("CheckGitState error contract = %#v, want restored mutation", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("staged paths after restoration = %q", got)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "diff", "--name-only"); got != "feature.txt" {
		t.Fatalf("unstaged paths after restoration = %q, want feature.txt", got)
	}
}

func TestGitStateRejectsAgentCommitAndRestoresOwnedHeadByCAS(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "agent-commit")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "agent.txt", "agent\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "agent.txt")
	gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "-qm", "agent commit")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("CheckGitState error = %v, want HEAD violation", err)
	}
	if strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("owned agent commit was not fully restored: %v", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %q, want restored %q", got, head)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "refs/heads/togi/run-agent-commit"); got != head {
		t.Fatalf("run ref = %q, want restored %q", got, head)
	}
}

func TestGitStatePreservesConcurrentRunRefReplacementBeforeOwnedRestoration(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "concurrent-agent-ref")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "agent.txt", "agent\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "agent.txt")
	gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "-qm", "agent commit")
	agentHead := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD")
	runRef := filepath.Join(repo, ".git", "refs", "heads", "togi", "run-concurrent-agent-ref")
	var replacementInfo os.FileInfo

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
		contents, err := os.ReadFile(runRef)
		if err != nil {
			return err
		}
		replacement := runRef + ".replacement"
		if err := os.WriteFile(replacement, contents, 0o644); err != nil {
			return err
		}
		if err := os.Rename(replacement, runRef); err != nil {
			return err
		}
		replacementInfo, err = os.Lstat(runRef)
		return err
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "concurrent shared") {
		t.Fatalf("checkGitState error = %v, want concurrent run-ref storage refusal", err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-concurrent-agent-ref"); got != agentHead {
		t.Fatalf("run ref = %q, want preserved agent commit %q", got, agentHead)
	}
	currentInfo, statErr := os.Lstat(runRef)
	if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, currentInfo) {
		t.Fatalf("operator replacement inode was not preserved: %v", statErr)
	}
}

func TestGitStateRejectsConcurrentIndexModeChangeBeforeRestoration(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "concurrent-index-mode")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
	observedInfo, err := os.Lstat(workspace.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	operatorMode := observedInfo.Mode().Perm() ^ 0o100

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
		return os.Chmod(workspace.indexPath, operatorMode)
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "concurrent shared") {
		t.Fatalf("checkGitState error = %v, want concurrent index-mode refusal", err)
	}
	currentInfo, statErr := os.Lstat(workspace.indexPath)
	if statErr != nil || currentInfo.Mode().Perm() != operatorMode {
		t.Fatalf("operator index mode = %v, %v; want preserved %v", currentInfo.Mode().Perm(), statErr, operatorMode)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "diff", "--cached", "--name-only"); got != "feature.txt" {
		t.Fatalf("index was restored across concurrent mode change: %q", got)
	}
}

func TestGitStateFinalSnapshotRejectsIndexReplacementWithoutOwnedIndexCAS(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "final-index-replacement")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, workspace.Path(), "update-ref", "-d", "refs/heads/togi/run-final-index-replacement", head)
	var replacementInfo os.FileInfo

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{afterRestore: func() error {
		contents, err := os.ReadFile(workspace.indexPath)
		if err != nil {
			return err
		}
		replacement := workspace.indexPath + ".replacement"
		if err := os.WriteFile(replacement, contents, 0o644); err != nil {
			return err
		}
		if err := os.Rename(replacement, workspace.indexPath); err != nil {
			return err
		}
		replacementInfo, err = os.Lstat(workspace.indexPath)
		return err
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("checkGitState error = %v, want unowned final index replacement refusal", err)
	}
	currentInfo, statErr := os.Lstat(workspace.indexPath)
	if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, currentInfo) {
		t.Fatalf("operator index replacement was not preserved: %v", statErr)
	}
}

func TestGitStateFinalSnapshotRejectsIndexReplacementAfterOwnedIndexCAS(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "final-owned-index-replacement")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
	var replacementInfo os.FileInfo

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{afterRestore: func() error {
		replacement := workspace.indexPath + ".replacement"
		if err := os.WriteFile(replacement, before.indexBytes, before.indexInfo.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Rename(replacement, workspace.indexPath); err != nil {
			return err
		}
		replacementInfo, err = os.Lstat(workspace.indexPath)
		return err
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("checkGitState error = %v, want post-CAS index replacement refusal", err)
	}
	currentInfo, statErr := os.Lstat(workspace.indexPath)
	if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, currentInfo) {
		t.Fatalf("operator index replacement was not preserved: %v", statErr)
	}
}

func TestGitStateFinalSnapshotRejectsRunRefReplacementAfterOwnedCAS(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "final-owned-ref-replacement")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "agent.txt", "agent\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "agent.txt")
	gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "-qm", "agent commit")
	runRef := filepath.Join(repo, ".git", "refs", "heads", "togi", "run-final-owned-ref-replacement")
	var replacementInfo os.FileInfo

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{afterRestore: func() error {
		replacement := runRef + ".replacement"
		if err := os.WriteFile(replacement, []byte(head+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.Rename(replacement, runRef); err != nil {
			return err
		}
		replacementInfo, err = os.Lstat(runRef)
		return err
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("checkGitState error = %v, want post-CAS run-ref replacement refusal", err)
	}
	currentInfo, statErr := os.Lstat(runRef)
	if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, currentInfo) {
		t.Fatalf("operator run-ref replacement was not preserved: %v", statErr)
	}
}

func TestGitStateRejectsAndPreservesIndexModeMutation(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "index-mode")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operatorMode := before.indexInfo.Mode().Perm() ^ 0o100
	if err := os.Chmod(workspace.indexPath, operatorMode); err != nil {
		t.Fatal(err)
	}

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "index") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want incomplete index-mode violation", err)
	}
	currentInfo, statErr := os.Lstat(workspace.indexPath)
	if statErr != nil || currentInfo.Mode().Perm() != operatorMode {
		t.Fatalf("operator index mode = %v, %v; want preserved %v", currentInfo.Mode().Perm(), statErr, operatorMode)
	}
}

func TestGitStateRejectsConcurrentSameValueSharedFileReplacementBeforeRestoration(t *testing.T) {
	tests := []struct {
		name string
		path func(string, *Workspace) string
		seed func(*testing.T, string)
	}{
		{
			name: "config",
			path: func(repo string, _ *Workspace) string { return filepath.Join(repo, ".git", "config") },
			seed: func(*testing.T, string) {},
		},
		{
			name: "pseudoref",
			path: func(_ string, workspace *Workspace) string { return filepath.Join(workspace.gitDir, "MERGE_HEAD") },
			seed: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(strings.Repeat("a", 40)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "concurrent-shared-file-"+test.name)
			path := test.path(repo, workspace)
			test.seed(t, path)
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
			gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
			var replacementInfo os.FileInfo

			err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
				contents, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				info, err := os.Lstat(path)
				if err != nil {
					return err
				}
				replacement := path + ".replacement"
				if err := os.WriteFile(replacement, contents, info.Mode().Perm()); err != nil {
					return err
				}
				if err := os.Rename(replacement, path); err != nil {
					return err
				}
				replacementInfo, err = os.Lstat(path)
				return err
			}})
			if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "concurrent shared") {
				t.Fatalf("checkGitState error = %v, want concurrent %s replacement refusal", err, test.name)
			}
			currentInfo, statErr := os.Lstat(path)
			if statErr != nil || replacementInfo == nil || !os.SameFile(replacementInfo, currentInfo) {
				t.Fatalf("operator %s replacement was not preserved: %v", test.name, statErr)
			}
			if got := gitcmdtest.Git(t, workspace.Path(), "diff", "--cached", "--name-only"); got != "feature.txt" {
				t.Fatalf("index was restored across concurrent %s replacement: %q", test.name, got)
			}
		})
	}
}

func TestGitStateRejectsConcurrentStableMetadataMutationBeforeRestoration(t *testing.T) {
	surfaces := []struct {
		name string
		path func(*testing.T, string, *Workspace, string) string
	}{
		{name: "config", path: func(_ *testing.T, repo string, _ *Workspace, _ string) string {
			return filepath.Join(repo, ".git", "config")
		}},
		{name: "pseudoref", path: func(t *testing.T, _ string, workspace *Workspace, head string) string {
			path := filepath.Join(workspace.gitDir, "MERGE_HEAD")
			if err := os.WriteFile(path, []byte(head+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "raw-control", path: func(t *testing.T, repo string, _ *Workspace, _ string) string {
			gitcmdtest.Git(t, repo, "pack-refs", "--all")
			return filepath.Join(repo, ".git", "packed-refs")
		}},
	}
	mutations := []struct {
		name   string
		mutate func(string, []byte, os.FileInfo) error
	}{
		{name: "chtimes", mutate: func(path string, _ []byte, info os.FileInfo) error {
			changed := info.ModTime().Add(2 * time.Second)
			return os.Chtimes(path, changed, changed)
		}},
		{name: "rewrite-restore-mtime", mutate: func(path string, contents []byte, info os.FileInfo) error {
			if err := os.WriteFile(path, contents, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(path, info.ModTime(), info.ModTime())
		}},
	}
	for _, surface := range surfaces {
		for _, mutation := range mutations {
			t.Run(surface.name+"/"+mutation.name, func(t *testing.T) {
				repo, head := workspaceRepository(t)
				workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "stable-pre-"+surface.name+"-"+mutation.name)
				path := surface.path(t, repo, workspace, head)
				before, err := workspace.SnapshotGitState(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
				gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
				var mutatedInfo os.FileInfo

				err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
					contents, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					info, err := os.Lstat(path)
					if err != nil {
						return err
					}
					if err := mutation.mutate(path, contents, info); err != nil {
						return err
					}
					mutatedInfo, err = os.Lstat(path)
					return err
				}})
				if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "concurrent shared") {
					t.Fatalf("checkGitState error = %v, want concurrent stable-metadata refusal", err)
				}
				currentInfo, statErr := os.Lstat(path)
				if statErr != nil || mutatedInfo == nil || !stableFileInfo(mutatedInfo, currentInfo) {
					t.Fatalf("operator mutation was not preserved: %v", statErr)
				}
				if got := gitcmdtest.Git(t, workspace.Path(), "diff", "--cached", "--name-only"); got != "feature.txt" {
					t.Fatalf("index was restored across concurrent mutation: %q", got)
				}
			})
		}
	}
}

func TestExactPathSnapshotRejectsMutationDuringSample(t *testing.T) {
	t.Run("same bytes restored mtime", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "control")
		contents := []byte("same bytes\n")
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = snapshotExactPathWithHooks(path, gitOutputLimit, exactPathHooks{afterRead: func() error {
			if err := os.WriteFile(path, contents, before.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(path, before.ModTime(), before.ModTime())
		}})
		if err == nil {
			t.Fatal("snapshot accepted an in-sample same-byte write with restored mtime")
		}
	})

	t.Run("absent becomes present", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pseudoref")
		_, err := snapshotExactPathWithHooks(path, gitOutputLimit, exactPathHooks{afterRead: func() error {
			return os.WriteFile(path, []byte("created\n"), 0o600)
		}})
		if err == nil {
			t.Fatal("snapshot accepted a pseudoref created during absent sampling")
		}
	})
}

func TestGitStateFinalSnapshotRejectsStableMetadataMutationOfRestoredState(t *testing.T) {
	for _, surface := range []string{"index", "run-ref"} {
		for _, mutation := range []string{"chtimes", "rewrite-restore-mtime"} {
			t.Run(surface+"/"+mutation, func(t *testing.T) {
				repo, head := workspaceRepository(t)
				workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "stable-final-"+surface+"-"+mutation)
				before, err := workspace.SnapshotGitState(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if surface == "index" {
					writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
					gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
				} else {
					writeWorkspaceFile(t, workspace.Path(), "agent.txt", "agent\n")
					gitcmdtest.Git(t, workspace.Path(), "add", "--", "agent.txt")
					gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit", "-qm", "agent commit")
				}
				path := workspace.indexPath
				if surface == "run-ref" {
					path = filepath.Join(repo, ".git", "refs", "heads", "togi", "run-stable-final-"+surface+"-"+mutation)
				}
				var mutatedInfo os.FileInfo

				err = workspace.checkGitState(context.Background(), before, gitStateHooks{afterRestore: func() error {
					contents, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					info, err := os.Lstat(path)
					if err != nil {
						return err
					}
					if mutation == "chtimes" {
						changed := info.ModTime().Add(2 * time.Second)
						err = os.Chtimes(path, changed, changed)
					} else {
						err = os.WriteFile(path, contents, info.Mode().Perm())
						if err == nil {
							err = os.Chtimes(path, info.ModTime(), info.ModTime())
						}
					}
					if err != nil {
						return err
					}
					mutatedInfo, err = os.Lstat(path)
					return err
				}})
				if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
					t.Fatalf("checkGitState error = %v, want restored-state metadata refusal", err)
				}
				currentInfo, statErr := os.Lstat(path)
				if statErr != nil || mutatedInfo == nil || !stableFileInfo(mutatedInfo, currentInfo) {
					t.Fatalf("operator mutation was not preserved: %v", statErr)
				}
			})
		}
	}
}

func TestGitStateRejectsAndRestoresDeletedOwnedRunRefByCAS(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "deleted-run-ref")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, workspace.Path(), "update-ref", "-d", "refs/heads/togi/run-deleted-run-ref", head)

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "run ref") {
		t.Fatalf("CheckGitState error = %v, want run-ref violation", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "refs/heads/togi/run-deleted-run-ref"); got != head {
		t.Fatalf("run ref = %q, want restored %q", got, head)
	}
}

func TestGitStateRejectsAndPreservesUnprovenNewRefsTagsAndWorktrees(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "new-state")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	extraWorktree := filepath.Join(t.TempDir(), "agent-worktree")
	gitcmdtest.Git(t, workspace.Path(), "tag", "agent-tag")
	gitcmdtest.Git(t, workspace.Path(), "worktree", "add", "-q", "-b", "agent-branch", extraWorktree, "HEAD")
	writeWorkspaceFile(t, extraWorktree, "operator.txt", "preserve\n")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "refs") || !strings.Contains(err.Error(), "worktrees") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want incomplete refs and worktrees violations", err)
	}
	var stateErr *GitStateCheckError
	if !errors.Is(err, ErrGitStateUnsafe) || errors.Is(err, ErrGitStateRestored) || !errors.As(err, &stateErr) || stateErr.Restored {
		t.Fatalf("CheckGitState error contract = %#v, want unsafe/incomplete", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "for-each-ref", "--format=%(refname)", "refs/tags/agent-tag", "refs/heads/agent-branch"); !strings.Contains(got, "refs/heads/agent-branch") || !strings.Contains(got, "refs/tags/agent-tag") {
		t.Fatalf("unproven new refs were deleted: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(extraWorktree, "operator.txt")); err != nil || string(got) != "preserve\n" {
		t.Fatalf("unproven worktree content was removed: %q, %v", got, err)
	}
}

func TestGitStateRejectsConfigEditsAndPreservesMovedOperatorRefs(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "branch", "operator", head)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "preserve-operator")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, workspace.Path(), "config", "agent.changed", "true")
	tree := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD^{tree}")
	newCommit := gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Agent", "-c", "user.email=agent@example.invalid", "commit-tree", tree, "-p", head, "-m", "operator move")
	gitcmdtest.Git(t, workspace.Path(), "update-ref", "refs/heads/operator", newCommit, head)

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "moved refs") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want config and moved-ref violations", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "refs/heads/operator"); got != newCommit {
		t.Fatalf("operator ref was rewound to %q, want %q", got, newCommit)
	}
}

func TestGitStateNeverRewritesBranchSelectedByUnauthorizedHEADChange(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "head-switch")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tree := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "HEAD^{tree}")
	operatorHead := gitcmdtest.Git(t, workspace.Path(), "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid", "commit-tree", tree, "-p", head, "-m", "operator")
	gitcmdtest.Git(t, workspace.Path(), "update-ref", "refs/heads/operator", operatorHead)
	gitcmdtest.Git(t, workspace.Path(), "symbolic-ref", "HEAD", "refs/heads/operator")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want incomplete HEAD restoration", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "refs/heads/operator"); got != operatorHead {
		t.Fatalf("operator branch was rewritten to %q, want %q", got, operatorHead)
	}
}

func TestGitStateSnapshotRejectsMissingHEAD(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "strict-snapshot")
	gitcmdtest.Git(t, workspace.Path(), "update-ref", "-d", "refs/heads/togi/run-strict-snapshot", head)
	if _, err := workspace.SnapshotGitState(context.Background()); err == nil {
		t.Fatal("SnapshotGitState accepted a missing HEAD")
	}
}

func TestGitStateRejectsForeignAndZeroSnapshotsWithoutMutatingRunRef(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "snapshot-owner")
	otherRepo, otherHead := workspaceRepository(t)
	other := createTestWorkspace(t, otherRepo, otherHead, filepath.Join(t.TempDir(), "other-workspace"), "other")
	foreign, err := other.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []GitState{{}, foreign} {
		if err := workspace.CheckGitState(context.Background(), snapshot); err == nil {
			t.Fatal("CheckGitState accepted an invalid or foreign snapshot")
		}
		if got := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "refs/heads/togi/run-snapshot-owner"); got != head {
			t.Fatalf("run ref changed to %q, want %q", got, head)
		}
	}
}

func TestGitStateIndexCompareAndSwapPreservesUnexpectedCurrentBytes(t *testing.T) {
	index := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(index, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceIndexCAS(index, []byte("observed"), info, []byte("before"), indexCASHooks{}); err == nil {
		t.Fatal("replaceIndexCAS accepted unexpected current bytes")
	}
	if got, err := os.ReadFile(index); err != nil || string(got) != "newer" {
		t.Fatalf("index bytes = %q, %v; want preserved newer bytes", got, err)
	}
}

func TestGitStateIndexCompareAndSwapRejectsSameByteInodeReplacement(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index")
	if err := os.WriteFile(index, []byte("observed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observedInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacementPath, []byte("observed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, index); err != nil {
		t.Fatal(err)
	}
	if err := replaceIndexCAS(index, []byte("observed"), observedInfo, []byte("before"), indexCASHooks{}); err == nil {
		t.Fatal("replaceIndexCAS accepted a same-byte replacement inode")
	}
	if got, err := os.ReadFile(index); err != nil || string(got) != "observed" {
		t.Fatalf("replacement index changed: %q, %v", got, err)
	}
}

func TestGitStateIndexCompareAndSwapPreservesSubstitutedOperatorLock(t *testing.T) {
	index := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(index, []byte("observed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observedInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	err = replaceIndexCAS(index, []byte("observed"), observedInfo, []byte("before"), indexCASHooks{
		beforeInstall: func(lockPath string) error {
			if err := os.Remove(lockPath); err != nil {
				return err
			}
			return os.WriteFile(lockPath, []byte("operator lock"), 0o600)
		},
	})
	if err == nil {
		t.Fatal("replaceIndexCAS installed a substituted lock")
	}
	if got, readErr := os.ReadFile(index); readErr != nil || string(got) != "observed" {
		t.Fatalf("index changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(index + ".lock"); readErr != nil || string(got) != "operator lock" {
		t.Fatalf("operator lock changed: %q, %v", got, readErr)
	}
}

func TestGitStateRechecksControlBindingImmediatelyBeforeRestoration(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "restore-repoint")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
	originalGitDir := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "--path-format=absolute", "--git-dir")
	commonDir := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	copiedGitDir := filepath.Join(commonDir, "worktrees", "restore-copy")
	copyTestTree(t, originalGitDir, copiedGitDir)

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
		return os.WriteFile(filepath.Join(workspace.Path(), ".git"), []byte("gitdir: "+copiedGitDir+"\n"), 0o600)
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "control binding") {
		t.Fatalf("checkGitState error = %v, want pre-restoration binding refusal", err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-restore-repoint"); got != head {
		t.Fatalf("run ref changed through repointed gitdir: %q", got)
	}
}

func TestGitStateRejectsCopiedGitDirRepointBeforeRestoration(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "gitdir-repoint")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	originalGitDir := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "--path-format=absolute", "--git-dir")
	commonDir := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	copiedGitDir := filepath.Join(commonDir, "worktrees", "copied-gitdir")
	copyTestTree(t, originalGitDir, copiedGitDir)
	repoint := []byte("gitdir: " + copiedGitDir + "\n")
	if err := os.WriteFile(filepath.Join(workspace.Path(), ".git"), repoint, 0o600); err != nil {
		t.Fatal(err)
	}

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "Git control binding") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want incomplete control-binding violation", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(workspace.Path(), ".git")); readErr != nil || !reflect.DeepEqual(got, repoint) {
		t.Fatalf("repointed .git was changed: %q, %v", got, readErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-gitdir-repoint"); got != head {
		t.Fatalf("owned run ref changed through foreign gitdir: %q", got)
	}
}

func TestGitStateDetectsCommentOnlyConfigMutation(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "config-bytes")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repo, ".git", "config")
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append(append([]byte(nil), original...), []byte("\n# agent comment only\n")...)
	if err := os.WriteFile(configPath, mutated, 0o600); err != nil {
		t.Fatal(err)
	}

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want incomplete exact-config violation", err)
	}
	if got, readErr := os.ReadFile(configPath); readErr != nil || !reflect.DeepEqual(got, mutated) {
		t.Fatalf("config mutation was not preserved: %q, %v", got, readErr)
	}
}

func TestGitStateDetectsMergeHeadCreationChangeAndRemoval(t *testing.T) {
	tests := []struct {
		name   string
		before []byte
		after  []byte
	}{
		{name: "creation", after: []byte(strings.Repeat("a", 40) + "\n")},
		{name: "change", before: []byte(strings.Repeat("a", 40) + "\n"), after: []byte(strings.Repeat("b", 40) + "\n")},
		{name: "removal", before: []byte(strings.Repeat("a", 40) + "\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "merge-head-"+test.name)
			gitDir := gitcmdtest.Git(t, workspace.Path(), "rev-parse", "--path-format=absolute", "--git-dir")
			mergeHead := filepath.Join(gitDir, "MERGE_HEAD")
			if test.before != nil {
				if err := os.WriteFile(mergeHead, test.before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if test.after == nil {
				if err := os.Remove(mergeHead); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(mergeHead, test.after, 0o600); err != nil {
				t.Fatal(err)
			}

			err = workspace.CheckGitState(context.Background(), before)
			if err == nil || !strings.Contains(err.Error(), "pseudoref") || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("CheckGitState error = %v, want incomplete pseudoref violation", err)
			}
		})
	}
}

func TestGitStateRejectsConcurrentSameValueRunRefReplacementWithoutRunRefCAS(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "concurrent-raw-run")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")
	runRef := filepath.Join(repo, ".git", "refs", "heads", "togi", "run-concurrent-raw-run")
	err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
		contents, err := os.ReadFile(runRef)
		if err != nil {
			return err
		}
		replacement := runRef + ".replacement"
		if err := os.WriteFile(replacement, contents, 0o644); err != nil {
			return err
		}
		return os.Rename(replacement, runRef)
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "concurrent shared") {
		t.Fatalf("checkGitState error = %v, want concurrent raw run-ref refusal", err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-concurrent-raw-run"); got != head {
		t.Fatalf("logical run ref moved to %q, want %q", got, head)
	}
}

func TestGitStateConcurrentSharedMutationBlocksOwnedIndexRestoration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *Workspace, string)
	}{
		{name: "ref", mutate: func(t *testing.T, _ string, workspace *Workspace, head string) {
			gitcmdtest.Git(t, workspace.Path(), "update-ref", "refs/heads/operator-concurrent", head)
		}},
		{name: "config", mutate: func(t *testing.T, repo string, _ *Workspace, _ string) {
			config := filepath.Join(repo, ".git", "config")
			file, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("\n# concurrent operator config\n"); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pseudoref", mutate: func(t *testing.T, _ string, workspace *Workspace, head string) {
			if err := os.WriteFile(filepath.Join(workspace.gitDir, "MERGE_HEAD"), []byte(head+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "worktree", mutate: func(t *testing.T, _ string, workspace *Workspace, head string) {
			gitcmdtest.Git(t, workspace.Path(), "worktree", "add", "-q", "-b", "operator-concurrent-worktree", filepath.Join(t.TempDir(), "operator-worktree"), head)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "concurrent-"+test.name)
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
			gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")

			err = workspace.checkGitState(context.Background(), before, gitStateHooks{beforeRestore: func() error {
				test.mutate(t, repo, workspace, head)
				return nil
			}})
			if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("checkGitState error = %v, want incomplete concurrent shared-state violation", err)
			}
			if got := gitcmdtest.Git(t, workspace.Path(), "diff", "--cached", "--name-only"); got != "feature.txt" {
				t.Fatalf("index was restored across concurrent shared mutation: %q", got)
			}
		})
	}
}

func TestGitStateFinalSnapshotRejectsSharedMutationDuringRestoration(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "final-shared-check")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "feature.txt", "staged\n")
	gitcmdtest.Git(t, workspace.Path(), "add", "--", "feature.txt")

	err = workspace.checkGitState(context.Background(), before, gitStateHooks{afterRestore: func() error {
		gitcmdtest.Git(t, workspace.Path(), "update-ref", "refs/heads/operator-during-restore", head)
		return nil
	}})
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "shared Git state changed") {
		t.Fatalf("checkGitState error = %v, want failed final shared-state verification", err)
	}
}

func TestGitStateSnapshotsLocalConfigIncludeClosureExactly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{name: "comment", mutate: func(t *testing.T, included, _ string) {
			if err := os.WriteFile(included, []byte("[agent]\n\tvalue = same\n# changed comment\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "value", mutate: func(t *testing.T, included, _ string) {
			if err := os.WriteFile(included, []byte("[agent]\n\tvalue = changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink target", mutate: func(t *testing.T, included, gitDir string) {
			other := filepath.Join(gitDir, "included-other.conf")
			if err := os.WriteFile(other, []byte("[agent]\n\tvalue = same\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(included); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, included); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			gitDir := filepath.Join(repo, ".git")
			included := filepath.Join(gitDir, "included.conf")
			if err := os.WriteFile(included, []byte("[agent]\n\tvalue = same\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitcmdtest.Git(t, repo, "config", "include.path", "included.conf")
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "include-"+strings.ReplaceAll(test.name, " ", "-"))
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, included, gitDir)

			err = workspace.CheckGitState(context.Background(), before)
			if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("CheckGitState error = %v, want included-config violation", err)
			}
		})
	}
}

func TestGitStateDetectsIncludedConfigSymlinkRepointWithIdenticalContents(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitDir := filepath.Join(repo, ".git")
	first := filepath.Join(gitDir, "include-first.conf")
	second := filepath.Join(gitDir, "include-second.conf")
	contents := []byte("[agent]\n\tvalue = identical\n")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	included := filepath.Join(gitDir, "included.conf")
	if err := os.Symlink(first, included); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "config", "include.path", "included.conf")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "include-symlink-repoint")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(included); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, included); err != nil {
		t.Fatal(err)
	}

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want included-config symlink binding violation", err)
	}
	if target, readErr := os.Readlink(included); readErr != nil || target != second {
		t.Fatalf("included config symlink was rewritten to %q, %v", target, readErr)
	}
}

func TestGitStateDetectsSameObjectSymbolicRefRetarget(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "branch", "operator-a", head)
	gitcmdtest.Git(t, repo, "branch", "operator-b", head)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/operator-alias", "refs/heads/operator-a")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "symref")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/operator-alias", "refs/heads/operator-b")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "moved refs") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want symbolic-ref violation", err)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/operator-alias"); got != "refs/heads/operator-b" {
		t.Fatalf("operator symbolic ref rewritten to %q", got)
	}
}

func TestGitStateNeverDereferencesSymbolicOwnedRunRefDuringRestoration(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "symbolic-run-ref")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tree := gitcmdtest.Git(t, repo, "rev-parse", head+"^{tree}")
	operator := gitcmdtest.Git(t, repo, "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid", "commit-tree", tree, "-p", head, "-m", "operator")
	gitcmdtest.Git(t, repo, "update-ref", "refs/heads/operator-symbolic-target", operator)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-run-ref", "refs/heads/operator-symbolic-target")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want unsafe symbolic run-ref refusal", err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/operator-symbolic-target"); got != operator {
		t.Fatalf("operator target was rewritten through symbolic run ref: %q, want %q", got, operator)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-run-ref"); got != "refs/heads/operator-symbolic-target" {
		t.Fatalf("unsafe symbolic run ref was rewritten to %q", got)
	}
}

func TestGitStateRejectsSameObjectSymbolicOwnedRunRef(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "same-object-symbolic-run")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "branch", "operator-same-object", head)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-same-object-symbolic-run", "refs/heads/operator-same-object")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want same-object symbolic run-ref refusal", err)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-same-object-symbolic-run"); got != "refs/heads/operator-same-object" {
		t.Fatalf("same-object symbolic run ref was rewritten to %q", got)
	}
}

func TestGitStateDetectsDanglingSymbolicRefRetarget(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/operator-dangling", "refs/heads/missing-a")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "dangling-symref")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/operator-dangling", "refs/heads/missing-b")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "refs") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want dangling symbolic-ref violation", err)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/operator-dangling"); got != "refs/heads/missing-b" {
		t.Fatalf("dangling symbolic ref was rewritten to %q", got)
	}
}

func TestGitStateDetectsPerWorktreeDanglingSymbolicRefRetarget(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "per-worktree-dangling")
	gitcmdtest.Git(t, workspace.Path(), "symbolic-ref", "refs/worktree/operator-dangling", "refs/heads/missing-a")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, workspace.Path(), "symbolic-ref", "refs/worktree/operator-dangling", "refs/heads/missing-b")

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "refs") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want per-worktree dangling symbolic-ref violation", err)
	}
	if got := gitcmdtest.Git(t, workspace.Path(), "symbolic-ref", "refs/worktree/operator-dangling"); got != "refs/heads/missing-b" {
		t.Fatalf("per-worktree dangling symbolic ref was rewritten to %q", got)
	}
}

func TestLooseSymbolicRefSnapshotBoundsTraversalShape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "refs")
	if err := os.MkdirAll(filepath.Join(root, "heads", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "heads", "one"), filepath.Join(root, "heads", "two")} {
		if err := os.WriteFile(path, []byte(strings.Repeat("a", 40)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, limits := range map[string]looseRefLimits{
		"entries":    {maxEntries: 2, maxPathBytes: 1 << 20, maxDepth: 64, maxContents: 1 << 20},
		"path bytes": {maxEntries: 100, maxPathBytes: 3, maxDepth: 64, maxContents: 1 << 20},
		"depth":      {maxEntries: 100, maxPathBytes: 1 << 20, maxDepth: 1, maxContents: 1 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := snapshotLooseSymbolicRefsWithLimits([]string{root}, limits); err == nil {
				t.Fatal("snapshotLooseSymbolicRefsWithLimits accepted traversal beyond its bound")
			}
		})
	}
}

func TestRawControlTraversalIsBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large"), []byte("private-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := rawTreeLimits{maxEntries: 1, maxPathBytes: 8, maxDepth: 1, maxContents: 4, perFile: 4}
	_, err := snapshotRawTrees([]rawTreeRoot{{name: "private", path: root}}, limits)
	if err == nil {
		t.Fatal("snapshotRawTrees accepted traversal beyond aggregate/per-file bounds")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "private-contents") {
		t.Fatalf("raw control error leaked private state: %v", err)
	}
}

func TestRawControlTraversalRejectsMutationDuringStableSample(t *testing.T) {
	limits := rawTreeLimits{maxEntries: 100, maxPathBytes: 1 << 20, maxDepth: 10, maxContents: 1 << 20, perFile: 1 << 20}
	t.Run("directory mode after EOF", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		_, err = snapshotRawTreesWithHooks([]rawTreeRoot{{name: "raw", path: root}}, limits, rawTreeHooks{
			afterDirectoryEOF: func(path string) error {
				if path == root {
					if err := os.Chmod(root, 0o700); err != nil {
						return err
					}
					return os.Chtimes(root, before.ModTime(), before.ModTime())
				}
				return nil
			},
		})
		if err == nil {
			t.Fatal("raw traversal accepted directory chmod after EOF")
		}
	})
	t.Run("entry insertion after EOF", func(t *testing.T) {
		root := t.TempDir()
		before, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		_, err = snapshotRawTreesWithHooks([]rawTreeRoot{{name: "raw", path: root}}, limits, rawTreeHooks{
			afterDirectoryEOF: func(path string) error {
				if path == root {
					if err := os.WriteFile(filepath.Join(root, "late"), []byte("late"), 0o600); err != nil {
						return err
					}
					return os.Chtimes(root, before.ModTime(), before.ModTime())
				}
				return nil
			},
		})
		if err == nil {
			t.Fatal("raw traversal accepted an entry inserted after EOF")
		}
	})
	t.Run("in-place file write after read", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "entry")
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = snapshotRawTreesWithHooks([]rawTreeRoot{{name: "raw", path: root}}, limits, rawTreeHooks{
			afterFileRead: func(readPath string) error {
				if readPath == path {
					if err := os.WriteFile(path, []byte("after!"), 0o600); err != nil {
						return err
					}
					return os.Chtimes(path, before.ModTime(), before.ModTime())
				}
				return nil
			},
		})
		if err == nil {
			t.Fatal("raw traversal accepted an in-place write after file read")
		}
	})
}

func TestGitStateSnapshotsWorktreeConfigIncludeClosure(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "config", "extensions.worktreeConfig", "true")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "worktree-config-include")
	included := filepath.Join(workspace.gitDir, "worktree-included.conf")
	if err := os.WriteFile(included, []byte("[agent]\n\tvalue = same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, workspace.Path(), "config", "--worktree", "include.path", "worktree-included.conf")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(included, []byte("[agent]\n\tvalue = same\n# operator comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want worktree config include violation", err)
	}
}

func TestGitStateAllowsEnabledWorktreeConfigWithAbsentScopeFile(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "config", "extensions.worktreeConfig", "true")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "absent-worktree-config")
	if _, err := workspace.SnapshotGitState(context.Background()); err != nil {
		t.Fatalf("SnapshotGitState with absent worktree config = %v", err)
	}
}

func TestConfigIncludeGraphBudgetsFailClosed(t *testing.T) {
	defaultBudget := configIncludeBudget{maxEntries: 100, maxPathBytes: 1 << 20, maxDepth: 20, maxContents: 1 << 20, perFile: 1 << 20}
	tests := []struct {
		name   string
		budget configIncludeBudget
		build  func(*testing.T, string) string
	}{
		{name: "many unique includes", budget: func() configIncludeBudget { value := defaultBudget; value.maxEntries = 2; return value }(), build: func(t *testing.T, dir string) string {
			root := filepath.Join(dir, "root.conf")
			writeConfigFixture(t, root, "[include]\npath = one.conf\npath = two.conf\npath = three.conf\n")
			for _, name := range []string{"one.conf", "two.conf", "three.conf"} {
				writeConfigFixture(t, filepath.Join(dir, name), "# bounded child\n")
			}
			return root
		}},
		{name: "cumulative paths", budget: func() configIncludeBudget { value := defaultBudget; value.maxPathBytes = 10; return value }(), build: func(t *testing.T, dir string) string {
			root := filepath.Join(dir, "root.conf")
			writeConfigFixture(t, root, "[include]\npath = a-very-long-config-name.conf\n")
			writeConfigFixture(t, filepath.Join(dir, "a-very-long-config-name.conf"), "# child\n")
			return root
		}},
		{name: "deep chain", budget: func() configIncludeBudget { value := defaultBudget; value.maxDepth = 2; return value }(), build: func(t *testing.T, dir string) string {
			writeConfigFixture(t, filepath.Join(dir, "root.conf"), "[include]\npath = one.conf\n")
			writeConfigFixture(t, filepath.Join(dir, "one.conf"), "[include]\npath = two.conf\n")
			writeConfigFixture(t, filepath.Join(dir, "two.conf"), "# deepest\n")
			return filepath.Join(dir, "root.conf")
		}},
		{name: "aggregate contents", budget: func() configIncludeBudget { value := defaultBudget; value.maxContents = 80; return value }(), build: func(t *testing.T, dir string) string {
			writeConfigFixture(t, filepath.Join(dir, "root.conf"), "[include]\npath = comments.conf\n")
			writeConfigFixture(t, filepath.Join(dir, "comments.conf"), "# "+strings.Repeat("private-comment-", 10)+"\n")
			return filepath.Join(dir, "root.conf")
		}},
		{name: "cycle", budget: defaultBudget, build: func(t *testing.T, dir string) string {
			writeConfigFixture(t, filepath.Join(dir, "root.conf"), "[include]\npath = other.conf\n")
			writeConfigFixture(t, filepath.Join(dir, "other.conf"), "[include]\npath = root.conf\n")
			return filepath.Join(dir, "root.conf")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			root := test.build(t, dir)
			_, err := validateConfigIncludeGraph(context.Background(), dir, []string{root}, test.budget)
			if err == nil {
				t.Fatal("validateConfigIncludeGraph accepted an over-budget or cyclic graph")
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "private-comment") {
				t.Fatalf("private config data leaked in error: %v", err)
			}
		})
	}
}

func TestConfigIncludeGraphRejectsMutationAfterNodeVisit(t *testing.T) {
	budget := configIncludeBudget{maxEntries: 100, maxPathBytes: 1 << 20, maxDepth: 20, maxContents: 1 << 20, perFile: 1 << 20}
	openPrivate := func(t *testing.T) *privateTempDir {
		t.Helper()
		root := t.TempDir()
		info, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		private, err := openPrivateTempDir(root, info)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(private.close)
		return private
	}

	t.Run("absent node becomes present", func(t *testing.T) {
		dir := t.TempDir()
		absent := filepath.Join(dir, "absent.conf")
		later := filepath.Join(dir, "later.conf")
		writeConfigFixture(t, later, "# later\n")
		_, err := validateContextConfigIncludeGraphWithHooks(context.Background(), dir, openPrivate(t), []string{absent, later}, nil, budget, configGraphHooks{
			afterVisit: func(path string) error {
				if path == absent {
					return os.WriteFile(absent, []byte("# created\n"), 0o600)
				}
				return nil
			},
		})
		if err == nil {
			t.Fatal("config graph accepted an absent node created after its visit")
		}
	})

	t.Run("completed sibling changes", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "root.conf")
		first := filepath.Join(dir, "first.conf")
		second := filepath.Join(dir, "second.conf")
		writeConfigFixture(t, root, "[include]\npath = first.conf\npath = second.conf\n")
		writeConfigFixture(t, first, "# first\n")
		writeConfigFixture(t, second, "# second\n")
		firstInfo, err := os.Lstat(first)
		if err != nil {
			t.Fatal(err)
		}
		_, err = validateContextConfigIncludeGraphWithHooks(context.Background(), dir, openPrivate(t), []string{root}, nil, budget, configGraphHooks{
			afterVisit: func(path string) error {
				if path != first {
					return nil
				}
				if err := os.WriteFile(first, []byte("# first\n"), firstInfo.Mode().Perm()); err != nil {
					return err
				}
				return os.Chtimes(first, firstInfo.ModTime(), firstInfo.ModTime())
			},
		})
		if err == nil {
			t.Fatal("config graph accepted a completed sibling mutation")
		}
	})

	t.Run("aggregate recheck rejects growth at captured lengths", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "root.conf")
		first := filepath.Join(dir, "first.conf")
		second := filepath.Join(dir, "second.conf")
		writeConfigFixture(t, root, "[include]\npath = first.conf\npath = second.conf\n")
		writeConfigFixture(t, first, "# one\n")
		writeConfigFixture(t, second, "# two\n")
		tiny := budget
		tiny.maxContents = 128
		tiny.perFile = 128
		_, err := validateContextConfigIncludeGraphWithHooks(context.Background(), dir, openPrivate(t), []string{root}, nil, tiny, configGraphHooks{
			afterVisit: func(path string) error {
				if path != first && path != second {
					return nil
				}
				return os.WriteFile(path, []byte("# "+strings.Repeat("grown", 20)+"\n"), 0o600)
			},
		})
		if err == nil {
			t.Fatal("config graph aggregate recheck accepted grown include files")
		}
	})
}

func TestGitStateIgnoresInactiveConditionalAndDisabledWorktreeIncludes(t *testing.T) {
	t.Run("inactive includeIf", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		inactive := filepath.Join(repo, ".git", "inactive.conf")
		writeConfigFixture(t, inactive, "[include]\npath = inactive.conf\n")
		gitcmdtest.Git(t, repo, "config", "includeIf.gitdir:/definitely-not-this-repository/.path", "inactive.conf")
		workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "inactive-include-if")
		if _, err := workspace.SnapshotGitState(context.Background()); err != nil {
			t.Fatalf("inactive includeIf blocked snapshot: %v", err)
		}
	})

	t.Run("inactive onbranch", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		inactive := filepath.Join(repo, ".git", "inactive-onbranch.conf")
		writeConfigFixture(t, inactive, "[include]\npath = inactive-onbranch.conf\n")
		gitcmdtest.Git(t, repo, "config", "includeIf.onbranch:not-feature.path", "inactive-onbranch.conf")
		workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "inactive-onbranch")
		if _, err := workspace.SnapshotGitState(context.Background()); err != nil {
			t.Fatalf("inactive onbranch includeIf blocked snapshot: %v", err)
		}
	})

	t.Run("disabled worktree config", func(t *testing.T) {
		repo, head := workspaceRepository(t)
		workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "disabled-worktree-config")
		writeConfigFixture(t, filepath.Join(workspace.gitDir, "config.worktree"), "[include]\npath = config.worktree\n")
		if _, err := workspace.SnapshotGitState(context.Background()); err != nil {
			t.Fatalf("disabled worktree config blocked snapshot: %v", err)
		}
	})
}

func TestConfigIncludeConditionEvaluationsAreBounded(t *testing.T) {
	repo, _ := workspaceRepository(t)
	root := filepath.Join(repo, ".git", "condition-budget.conf")
	target := filepath.Join(repo, ".git", "comments.conf")
	writeConfigFixture(t, target, "# comments only\n")
	writeConfigFixture(t, root, strings.Join([]string{
		"[includeIf \"gitdir:/inactive/one\"]\npath = comments.conf\n",
		"[includeIf \"gitdir:/inactive/two\"]\npath = comments.conf\n",
		"[includeIf \"gitdir:/inactive/three\"]\npath = comments.conf\n",
	}, ""))
	budget := configIncludeBudget{
		maxEntries: 100, maxPathBytes: 1 << 20, maxDepth: 20, maxContents: 1 << 20, perFile: 1 << 20,
		maxConditions: 2, maxConditionBytes: 1 << 10,
	}
	tempRoot := t.TempDir()
	tempRootInfo, err := os.Lstat(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	private, err := openPrivateTempDir(tempRoot, tempRootInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer private.close()
	if _, err := validateContextConfigIncludeGraph(context.Background(), repo, private, []string{root}, map[string]struct{}{}, budget); err == nil {
		t.Fatal("config include graph accepted more condition evaluations than its budget")
	}
}

func TestGitStateCapturesActiveConditionalInclude(t *testing.T) {
	repo, head := workspaceRepository(t)
	included := filepath.Join(repo, ".git", "active-conditional.conf")
	writeConfigFixture(t, included, "[agent]\nvalue = same\n")
	pattern := filepath.ToSlash(filepath.Join(repo, ".git", "worktrees")) + "/**"
	gitcmdtest.Git(t, repo, "config", "includeIf.gitdir:"+pattern+".path", "active-conditional.conf")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "active-conditional")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, included, "[agent]\nvalue = changed\n")
	if err := workspace.CheckGitState(context.Background(), before); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("CheckGitState error = %v, want active conditional config violation", err)
	}
}

func TestGitStateCapturesActiveCommentOnlyConditionalInclude(t *testing.T) {
	repo, head := workspaceRepository(t)
	included := filepath.Join(repo, ".git", "active-comment-only.conf")
	writeConfigFixture(t, included, "# active comment one\n")
	pattern := filepath.ToSlash(filepath.Join(repo, ".git", "worktrees")) + "/**"
	gitcmdtest.Git(t, repo, "config", "includeIf.gitdir:"+pattern+".path", "active-comment-only.conf")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "active-comment-only")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, included, "# active comment two\n")
	if err := workspace.CheckGitState(context.Background(), before); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("CheckGitState error = %v, want active comment-only config violation", err)
	}
}

func TestGitStateCapturesRawHookStorage(t *testing.T) {
	for _, mutation := range []string{"replace", "add", "delete", "symlink", "mode"} {
		t.Run(mutation, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			hooks := filepath.Join(repo, ".git", "hooks")
			hook := filepath.Join(hooks, "integrity-hook")
			if mutation != "add" {
				if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "raw-hook-"+mutation)
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "replace":
				replacement := filepath.Join(hooks, "replacement")
				if err := os.WriteFile(replacement, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, hook); err != nil {
					t.Fatal(err)
				}
			case "add":
				if err := os.WriteFile(hook, []byte("added\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(hook); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(hook); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("operator-target", hook); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(hook, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = workspace.CheckGitState(context.Background(), before)
			if err == nil || !strings.Contains(err.Error(), "hooks") || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("CheckGitState error = %v, want preserved raw hook violation", err)
			}
		})
	}
}

func TestGitStateCapturesMalformedLooseRefStorage(t *testing.T) {
	for _, mutation := range []string{"add", "change", "remove"} {
		t.Run(mutation, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "raw-ref-"+mutation)
			broken := filepath.Join(repo, ".git", "refs", "heads", "agent-broken")
			if mutation != "add" {
				if err := os.WriteFile(broken, []byte("garbage-before\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "add":
				err = os.WriteFile(broken, []byte("garbage-added\n"), 0o600)
			case "change":
				err = os.WriteFile(broken, []byte("garbage-after\n"), 0o600)
			case "remove":
				err = os.Remove(broken)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = workspace.CheckGitState(context.Background(), before)
			if err == nil || !strings.Contains(err.Error(), "raw refs") || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("CheckGitState error = %v, want preserved malformed ref violation", err)
			}
			if mutation != "remove" {
				if _, statErr := os.Lstat(broken); statErr != nil {
					t.Fatalf("malformed ref was removed: %v", statErr)
				}
			}
		})
	}
}

func TestGitStateCapturesSameLogicalOwnedRunRefStorageMutation(t *testing.T) {
	for _, mutation := range []string{"replace", "mode", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "raw-owned-"+mutation)
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			runRef := filepath.Join(repo, ".git", "refs", "heads", "togi", "run-raw-owned-"+mutation)
			contents, err := os.ReadFile(runRef)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "replace":
				replacement := runRef + ".replacement"
				if err := os.WriteFile(replacement, contents, 0o644); err != nil {
					t.Fatal(err)
				}
				err = os.Rename(replacement, runRef)
			case "mode":
				err = os.Chmod(runRef, 0o600)
			case "symlink":
				err = os.Remove(runRef)
				if err == nil {
					err = os.Symlink("../feature", runRef)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-raw-owned-"+mutation); got != head {
				t.Fatalf("logical run ref = %q, want unchanged %q", got, head)
			}
			err = workspace.CheckGitState(context.Background(), before)
			if err == nil || !strings.Contains(err.Error(), "raw refs") || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("CheckGitState error = %v, want same-logical raw run-ref violation", err)
			}
		})
	}
}

func TestGitStateRefusesMovedRunRefWithNonCanonicalRawBytes(t *testing.T) {
	repo, head := workspaceRepository(t)
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "raw-moved-noncanonical")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operatorCommit := gitcmdtest.Git(t, repo, "-c", "user.name=Operator", "-c", "user.email=operator@example.invalid",
		"commit-tree", head+"^{tree}", "-p", head, "-m", "operator")
	runRef := filepath.Join(repo, ".git", "refs", "heads", "togi", "run-raw-moved-noncanonical")
	if err := os.WriteFile(runRef, []byte(operatorCommit+"\n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/togi/run-raw-moved-noncanonical"); got != operatorCommit {
		t.Fatalf("logical run ref = %q, want %q", got, operatorCommit)
	}
	err = workspace.CheckGitState(context.Background(), before)
	if err == nil || !strings.Contains(err.Error(), "restoration incomplete") || !strings.Contains(err.Error(), "non-regular owned run ref storage") {
		t.Fatalf("CheckGitState error = %v, want noncanonical raw storage preserved", err)
	}
	contents, readErr := os.ReadFile(runRef)
	if readErr != nil || string(contents) != operatorCommit+"\n\t\n" {
		t.Fatalf("noncanonical run ref changed: %q, %v", contents, readErr)
	}
}

func TestGitStateCapturesRawPackedRefsMutation(t *testing.T) {
	repo, head := workspaceRepository(t)
	gitcmdtest.Git(t, repo, "pack-refs", "--all")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "raw-packed")
	before, err := workspace.SnapshotGitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(repo, ".git", "packed-refs")
	packedBytes, err := os.ReadFile(packed)
	if err != nil {
		t.Fatal(err)
	}
	lineEnd := bytes.IndexByte(packedBytes, '\n')
	if lineEnd < 0 {
		t.Fatal("packed-refs has no header line")
	}
	mutated := append([]byte(nil), packedBytes[:lineEnd]...)
	mutated = append(mutated, ' ', '\n')
	mutated = append(mutated, packedBytes[lineEnd+1:]...)
	if err := os.WriteFile(packed, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.snapshotGitState(context.Background(), true); err != nil {
		t.Fatalf("packed-refs mutation prevented diagnostic snapshot: %v", err)
	}
	if err := workspace.CheckGitState(context.Background(), before); err == nil || !strings.Contains(err.Error(), "raw control") || !strings.Contains(err.Error(), "restoration incomplete") {
		t.Fatalf("CheckGitState error = %v, want preserved packed-refs violation", err)
	}
}

func TestRawPackedRefsSnapshotRejectsPostReadMutation(t *testing.T) {
	for _, mutation := range []string{"replace", "in-place"} {
		t.Run(mutation, func(t *testing.T) {
			repo, _ := workspaceRepository(t)
			gitcmdtest.Git(t, repo, "pack-refs", "--all")
			commonDir := filepath.Join(repo, ".git")
			packed := filepath.Join(commonDir, "packed-refs")
			contents, err := os.ReadFile(packed)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(packed)
			if err != nil {
				t.Fatal(err)
			}
			_, err = snapshotRawControlFilesWithHooks(commonDir, commonDir, rawTreeHooks{afterFileRead: func(path string) error {
				if path != packed {
					return nil
				}
				if mutation == "replace" {
					replacement := packed + ".replacement"
					if err := os.WriteFile(replacement, contents, before.Mode().Perm()); err != nil {
						return err
					}
					return os.Rename(replacement, packed)
				}
				changed := append([]byte(nil), contents...)
				changed[len(changed)-1] ^= 1
				if err := os.WriteFile(packed, changed, before.Mode().Perm()); err != nil {
					return err
				}
				return os.Chtimes(packed, before.ModTime(), before.ModTime())
			}})
			if err == nil {
				t.Fatalf("packed-refs snapshot accepted %s mutation after read", mutation)
			}
		})
	}
}

func TestGitStateCapturesGitCompatibleCommentOnlyConditionalIncludes(t *testing.T) {
	tests := []struct {
		name      string
		condition func(string, string) string
		configure func(*testing.T, string)
	}{
		{
			name: "relative gitdir",
			condition: func(_ string, _ string) string {
				return "gitdir:worktrees/"
			},
		},
		{
			name: "origin relative trailing slash",
			condition: func(_ string, _ string) string {
				return "gitdir:./worktrees/"
			},
		},
		{
			name: "interior double star",
			condition: func(repo, _ string) string {
				return "gitdir:" + filepath.ToSlash(filepath.Join(repo, ".git")) + "/**/workspace"
			},
		},
		{
			name: "hasconfig remote url",
			condition: func(_, _ string) string {
				return "hasconfig:remote.*.url:https://example.com/**?x=*"
			},
			configure: func(t *testing.T, repo string) {
				gitcmdtest.Git(t, repo, "config", "remote.origin.url", "https://example.com/project.git?x=one")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			if test.configure != nil {
				test.configure(t, repo)
			}
			included := filepath.Join(repo, ".git", "conditional-comments.conf")
			writeConfigFixture(t, included, "# before\n")
			condition := test.condition(repo, head)
			gitcmdtest.Git(t, repo, "config", "includeIf."+condition+".path", "conditional-comments.conf")
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "conditional-"+strings.ReplaceAll(test.name, " ", "-"))
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, included, "# after\n")
			if err := workspace.CheckGitState(context.Background(), before); err == nil || !strings.Contains(err.Error(), "config") {
				t.Fatalf("CheckGitState error = %v, want active conditional config violation", err)
			}
		})
	}
}

func writeConfigFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGitStateSnapshotsCompleteWorktreeRegistrationSemantics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{name: "same commit branch switch", mutate: func(t *testing.T, repo, other string) {
			gitcmdtest.Git(t, other, "symbolic-ref", "HEAD", "refs/heads/operator-b")
		}},
		{name: "detach", mutate: func(t *testing.T, _ string, other string) {
			gitcmdtest.Git(t, other, "checkout", "-q", "--detach")
		}},
		{name: "lock metadata", mutate: func(t *testing.T, repo, other string) {
			gitcmdtest.Git(t, repo, "worktree", "lock", "--reason", "operator lock", other)
		}},
		{name: "prunable metadata", mutate: func(t *testing.T, _ string, other string) {
			if err := os.Rename(other, other+"-moved"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, head := workspaceRepository(t)
			gitcmdtest.Git(t, repo, "branch", "operator-b", head)
			other := filepath.Join(t.TempDir(), "other")
			gitcmdtest.Git(t, repo, "worktree", "add", "-q", "-b", "operator-a", other, head)
			workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "worktree-"+strings.ReplaceAll(test.name, " ", "-"))
			before, err := workspace.SnapshotGitState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, repo, other)

			err = workspace.CheckGitState(context.Background(), before)
			if err == nil || !strings.Contains(err.Error(), "worktrees") || !strings.Contains(err.Error(), "restoration incomplete") {
				t.Fatalf("CheckGitState error = %v, want complete worktree-registration violation", err)
			}
		})
	}
}

func TestWorktreeStateParserRejectsMalformedDuplicateAndUnknownRecords(t *testing.T) {
	head := strings.Repeat("a", 40)
	valid := "worktree /tmp/one\x00HEAD " + head + "\x00branch refs/heads/one"
	for _, output := range []string{
		valid + "\x00unknown data\x00\x00",
		valid + "\x00\x00" + valid + "\x00\x00",
		"worktree /tmp/one\x00HEAD invalid\x00branch refs/heads/one\x00\x00",
		"worktree /tmp/one\x00HEAD " + head + "\x00locked \x00branch refs/heads/one\x00\x00",
		valid + "\x00",
	} {
		if _, err := parseWorktreeStates([]byte(output)); err == nil {
			t.Fatalf("parseWorktreeStates accepted malformed stream %q", output)
		}
	}
}

func TestWorktreeStateParserPreservesExactRawPathSpelling(t *testing.T) {
	rawPath := t.TempDir() + string(filepath.Separator) + "one" + string(filepath.Separator) + ".." + string(filepath.Separator) + "two"
	output := "worktree " + rawPath + "\x00HEAD " + strings.Repeat("a", 40) + "\x00branch refs/heads/one\x00\x00"
	states, err := parseWorktreeStates([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].rawPath != rawPath {
		t.Fatalf("raw worktree path = %q, want exact %q", states[0].rawPath, rawPath)
	}
}

func TestWorkspacePostAddRecheckRejectsAncestorSymlinkSwapWithoutDeletingOperatorContent(t *testing.T) {
	repo, head := workspaceRepository(t)
	external := t.TempDir()
	parent := filepath.Join(external, "cache")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(parent, "run")
	operatorRoot := filepath.Join(repo, "operator-cache")
	operatorRun := filepath.Join(operatorRoot, "run")
	if err := os.MkdirAll(operatorRun, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, operatorRun, "operator.txt", "preserve\n")

	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "swap", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{afterAdd: func() error {
		if err := os.Rename(parent, parent+"-moved"); err != nil {
			return err
		}
		return os.Symlink(operatorRoot, parent)
	}})
	if err == nil || !strings.Contains(err.Error(), "external") || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("createWorkspace error = %v, want incomplete external-path rejection", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(operatorRun, "operator.txt")); readErr != nil || string(got) != "preserve\n" {
		t.Fatalf("operator content changed: %q, %v", got, readErr)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("feature HEAD changed to %q", got)
	}
}

func TestWorkspacePostAddRejectsCopiedUnregisteredGitDir(t *testing.T) {
	repo, head := workspaceRepository(t)
	requested := filepath.Join(t.TempDir(), "workspace")
	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "copied-control", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{afterAdd: func() error {
		originalGitDir := gitcmdtest.Git(t, requested, "rev-parse", "--path-format=absolute", "--git-dir")
		commonDir := gitcmdtest.Git(t, requested, "rev-parse", "--path-format=absolute", "--git-common-dir")
		copiedGitDir := filepath.Join(commonDir, "worktrees", "copied-post-add")
		copyTestTree(t, originalGitDir, copiedGitDir)
		return os.WriteFile(filepath.Join(requested, ".git"), []byte("gitdir: "+copiedGitDir+"\n"), 0o600)
	}})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("createWorkspace error = %v, want copied-control rejection", err)
	}
}

func TestWorkspaceFailedAddCleansOwnedBranchOnlyLeak(t *testing.T) {
	repo, head := workspaceRepository(t)
	requested := filepath.Join(t.TempDir(), "workspace")
	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "branch-leak", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{add: func(_ context.Context, repositoryRoot, path, branch, originalHead string) error {
		gitcmdtest.Git(t, repositoryRoot, "branch", branch, originalHead)
		return errors.New("injected add failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "injected add failure") {
		t.Fatalf("createWorkspace error = %v", err)
	}
	if got := gitcmdtest.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/togi/run-branch-leak"); got != "" {
		t.Fatalf("owned leaked branch survived: %q", got)
	}
}

func TestWorkspaceFailedAddCleanupRejectsSymlinkReboundCommonDir(t *testing.T) {
	repo, head := workspaceRepository(t)
	requested := filepath.Join(t.TempDir(), "workspace")
	commonDir := filepath.Join(repo, ".git")
	movedCommonDir := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-moved-git")
	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "common-rebound", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{add: func(_ context.Context, repositoryRoot, _ string, branch, originalHead string) error {
		gitcmdtest.Git(t, repositoryRoot, "branch", branch, originalHead)
		if err := os.Rename(commonDir, movedCommonDir); err != nil {
			return err
		}
		if err := os.Symlink(movedCommonDir, commonDir); err != nil {
			return err
		}
		return errors.New("injected rebound add failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "cleanup incomplete") || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("createWorkspace error = %v, want incomplete common-dir binding refusal", err)
	}
	if got := gitcmdtest.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/togi/run-common-rebound"); got != "refs/heads/togi/run-common-rebound" {
		t.Fatalf("owned ref was deleted through rebound common dir: %q", got)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("feature ref moved to %q, want %q", got, head)
	}
}

func TestWorkspaceFailedAddCleanupPreservesSymbolicRunRefAndOperatorTarget(t *testing.T) {
	repo, head := workspaceRepository(t)
	path := filepath.Join(t.TempDir(), "workspace")
	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: path, RunID: "symbolic-cleanup", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{add: func(_ context.Context, repositoryRoot, _ string, _ string, _ string) error {
		gitcmdtest.Git(t, repositoryRoot, "branch", "operator-cleanup-target", head)
		gitcmdtest.Git(t, repositoryRoot, "symbolic-ref", "refs/heads/togi/run-symbolic-cleanup", "refs/heads/operator-cleanup-target")
		return errors.New("injected add failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "cleanup incomplete") {
		t.Fatalf("createWorkspace error = %v, want incomplete cleanup", err)
	}
	if got := gitcmdtest.Git(t, repo, "rev-parse", "refs/heads/operator-cleanup-target"); got != head {
		t.Fatalf("operator cleanup target moved to %q, want %q", got, head)
	}
	if got := gitcmdtest.Git(t, repo, "symbolic-ref", "refs/heads/togi/run-symbolic-cleanup"); got != "refs/heads/operator-cleanup-target" {
		t.Fatalf("symbolic cleanup run ref was rewritten to %q", got)
	}
}

func TestWorkspaceFailedAddPreservesRefWhenUnregisteredPathExists(t *testing.T) {
	repo, head := workspaceRepository(t)
	requested := filepath.Join(t.TempDir(), "workspace")
	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "path-leak", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{add: func(_ context.Context, repositoryRoot, path, branch, originalHead string) error {
		gitcmdtest.Git(t, repositoryRoot, "branch", branch, originalHead)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		writeWorkspaceFile(t, path, "operator.txt", "preserve\n")
		return errors.New("injected path failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "cleanup incomplete") {
		t.Fatalf("createWorkspace error = %v, want incomplete cleanup", err)
	}
	if got := gitcmdtest.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/togi/run-path-leak"); got != "refs/heads/togi/run-path-leak" {
		t.Fatalf("run ref was deleted despite partial path: %q", got)
	}
	if got, readErr := os.ReadFile(filepath.Join(requested, "operator.txt")); readErr != nil || string(got) != "preserve\n" {
		t.Fatalf("partial path content changed: %q, %v", got, readErr)
	}
}

func TestWorkspaceFailedAddPreservesPartialRegistrationPathAndBranchForDiagnosis(t *testing.T) {
	repo, head := workspaceRepository(t)
	requested := filepath.Join(t.TempDir(), "workspace")
	_, err := createWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "partial-leak", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	}, workspaceCreateHooks{add: func(_ context.Context, repositoryRoot, path, branch, originalHead string) error {
		gitcmdtest.Git(t, repositoryRoot, "worktree", "add", "-q", "-b", branch, path, originalHead)
		return errors.New("injected post-add failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "injected post-add failure") {
		t.Fatalf("createWorkspace error = %v", err)
	}
	if !strings.Contains(err.Error(), "cleanup incomplete") {
		t.Fatalf("createWorkspace error = %v, want incomplete cleanup", err)
	}
	if _, statErr := os.Stat(requested); statErr != nil {
		t.Fatalf("partial path was removed: %v", statErr)
	}
	if got := gitcmdtest.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/togi/run-partial-leak"); got != "refs/heads/togi/run-partial-leak" {
		t.Fatalf("partial branch was not preserved: %q", got)
	}
}

func TestWorkspaceFailedSmudgeCleansOnlyProvenCreationState(t *testing.T) {
	repo, _ := workspaceRepository(t)
	writeWorkspaceFile(t, repo, ".gitattributes", "feature.txt filter=fail-checkout\n")
	gitcmdtest.Git(t, repo, "add", "--", ".gitattributes")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "add filter")
	head := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	failingFilter := filepath.Join(t.TempDir(), "fail-filter")
	if err := os.WriteFile(failingFilter, []byte("#!/bin/sh\nexit 73\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "config", "filter.fail-checkout.smudge", failingFilter)
	gitcmdtest.Git(t, repo, "config", "filter.fail-checkout.required", "true")
	requested := filepath.Join(t.TempDir(), "workspace")

	_, err := CreateWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: requested, RunID: "smudge-failure", OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	})
	if err == nil || !strings.Contains(err.Error(), "create cache worktree") {
		t.Fatalf("CreateWorkspace error = %v, want checkout failure", err)
	}
	if got := gitcmdtest.Git(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/togi/run-smudge-failure"); got != "" {
		t.Fatalf("failed add leaked run branch: %q", got)
	}
	if got := gitcmdtest.Git(t, repo, "worktree", "list", "--porcelain"); strings.Contains(got, requested) {
		t.Fatalf("failed add leaked worktree registration: %q", got)
	}
	if _, statErr := os.Stat(requested); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed add leaked path: %v", statErr)
	}
}

func copyTestTree(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
}

func createTestWorkspace(t *testing.T, repo, head, path, runID string) *Workspace {
	t.Helper()
	workspace, err := CreateWorkspace(context.Background(), WorkspaceSpec{
		RepositoryRoot: repo, Path: path, RunID: runID, OriginalHead: head,
		FeatureBranch: "feature", Identity: Identity{Name: "Togi", Email: "togi@example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func writeWorkspaceFile(t *testing.T, root, path, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func workspaceRepository(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	gitcmdtest.Git(t, repo, "init", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "add", "--", "feature.txt")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "original")
	return repo, gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
}

func workspaceRepositoryWithIgnoredGenerated(t *testing.T) (string, string) {
	t.Helper()
	repo, _ := workspaceRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("generated/\nexisting/ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitcmdtest.Git(t, repo, "add", "--", ".gitignore")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "ignore generated")
	return repo, gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
}

func featureObservation(t *testing.T, repo string) []string {
	t.Helper()
	return []string{
		gitcmdtest.Git(t, repo, "rev-parse", "HEAD"),
		gitcmdtest.Git(t, repo, "symbolic-ref", "--short", "HEAD"),
		gitcmdtest.Git(t, repo, "status", "--porcelain=v2", "--untracked-files=all"),
	}
}
