package flywheel

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/joellarson/togi/internal/gitcmd"
)

const gitOutputLimit = 4 << 20
const indexByteLimit = 64 << 20
const batchIgnoredInventoryLimit = 1 << 20
const batchDirectoryEntryLimit = 1 << 16
const batchDirectoryPathLimit = 4 << 20
const batchDirectoryDepthLimit = 64

var defaultLooseRefLimits = looseRefLimits{
	maxEntries:   1 << 16,
	maxPathBytes: gitOutputLimit,
	maxDepth:     64,
	maxContents:  gitOutputLimit,
}

var defaultRawControlLimits = rawTreeLimits{
	maxEntries: 1 << 16, maxPathBytes: gitOutputLimit, maxDepth: 64,
	maxContents: gitOutputLimit, perFile: gitOutputLimit,
}

var validationSnapshotLimits = rawTreeLimits{
	maxEntries: 1 << 16, maxPathBytes: 4 << 20, maxDepth: 64,
	maxContents: indexByteLimit, perFile: indexByteLimit,
}

var defaultConfigIncludeBudget = configIncludeBudget{
	maxEntries:        1024,
	maxPathBytes:      1 << 20,
	maxDepth:          32,
	maxContents:       32 << 20,
	perFile:           gitOutputLimit,
	maxConditions:     128,
	maxConditionBytes: 64 << 10,
}

// Identity is the explicit author and committer identity for Togi-owned
// rollback commits.
type Identity struct {
	Name  string
	Email string
}

// WorkspaceSpec identifies a cache worktree owned by one run.
type WorkspaceSpec struct {
	RepositoryRoot string
	Path           string
	RunID          string
	OriginalHead   string
	FeatureBranch  string
	Identity       Identity
}

// Workspace owns the Git state used by the serial fix loop.
type Workspace struct {
	repositoryRoot                string
	path                          string
	gitDir                        string
	commonDir                     string
	dotGit                        exactFile
	rootInfo                      os.FileInfo
	gitDirInfo                    os.FileInfo
	commonInfo                    os.FileInfo
	indexPath                     string
	indexInfo                     os.FileInfo
	indexBytes                    []byte
	cacheRoot                     string
	cacheRootInfo                 os.FileInfo
	branch                        string
	green                         string
	identity                      Identity
	beforeIndexInstall            func() error
	beforeResetFinal              func() error
	beforeBatchRefUpdate          func() error
	afterBatchRefUpdate           func() error
	updateBatchRef                func(context.Context, string, string, string) error
	validationSnapshot            *validationSnapshot
	validationMaterializeFailure  func() error
	validationDiscardFailure      func() error
	validationBeforePrivateRemove func() error
}

// Root returns the adapter-visible worktree root.
func (w *Workspace) Root() string {
	return w.Path()
}

type workspaceCreateHooks struct {
	add      func(context.Context, string, string, string, string) error
	afterAdd func() error
}

// GitState is an opaque, deterministic snapshot of Git control state around
// an agent attempt.
type GitState struct {
	owner        *Workspace
	rootInfo     os.FileInfo
	dotGit       exactFile
	gitDir       string
	gitDirInfo   os.FileInfo
	commonDir    string
	commonInfo   os.FileInfo
	head         string
	symbolicHead string
	indexTree    string
	indexPath    string
	indexBytes   []byte
	indexInfo    os.FileInfo
	runRef       refState
	worktrees    []worktreeState
	configGraph  []byte
	configs      map[string]exactFile
	pseudorefs   map[string]exactFile
	refs         map[string]refState
	rawHooks     map[string]exactFile
	rawRefs      map[string]exactFile
	rawControl   map[string]exactFile
}

// BatchProof is opaque evidence binding a validated attempt to one staged
// tree and the stable worktree state from which it was prepared.
type BatchProof struct {
	owner           any
	tree            string
	changed         []string
	files           map[string]exactFile
	dirs            map[string]os.FileInfo
	protected       TreeSnapshot
	index           exactFile
	verify          func(context.Context, BatchProof) error
	validation      *validationSnapshot
	validationFiles map[string]exactFile
}

type validationSnapshot struct {
	private *privateTempDir
	files   map[string]exactFile
}

func (proof BatchProof) validFor(workspace *Workspace) bool {
	return workspace != nil && proof.owner == workspace && proof.present() && len(proof.changed) != 0 &&
		proof.files != nil && proof.dirs != nil && proof.protected.Files != nil && proof.index.exists &&
		proof.validation != nil && proof.validation == workspace.validationSnapshot && proof.validationFiles != nil
}

func (proof BatchProof) present() bool {
	return proof.owner != nil && validObjectID(proof.tree) && proof.verify != nil && proof.validation != nil
}

// ValidationRoot returns the private immutable root containing the exact
// staged tree. It is intentionally omitted from serialized validation state.
func (proof BatchProof) ValidationRoot() string {
	if proof.validation == nil || proof.validation.private == nil {
		return ""
	}
	return proof.validation.private.path
}

func cloneBatchProof(proof BatchProof) BatchProof {
	proof.changed = slices.Clone(proof.changed)
	proof.files = cloneExactFiles(proof.files)
	proof.dirs = cloneDirectoryInfos(proof.dirs)
	proof.protected = cloneTreeSnapshot(proof.protected)
	proof.index.bytes = slices.Clone(proof.index.bytes)
	proof.validationFiles = cloneExactFiles(proof.validationFiles)
	return proof
}

func cloneExactFiles(files map[string]exactFile) map[string]exactFile {
	if files == nil {
		return nil
	}
	cloned := make(map[string]exactFile, len(files))
	for name, file := range files {
		file.bytes = slices.Clone(file.bytes)
		cloned[name] = file
	}
	return cloned
}

func cloneDirectoryInfos(dirs map[string]os.FileInfo) map[string]os.FileInfo {
	if dirs == nil {
		return nil
	}
	cloned := make(map[string]os.FileInfo, len(dirs))
	for name, info := range dirs {
		cloned[name] = info
	}
	return cloned
}

var (
	// ErrGitStateRestored identifies an unauthorized mutation that was fully
	// restored to the exact captured baseline.
	ErrGitStateRestored = errors.New("unauthorized Git state mutation fully restored")
	// ErrGitStateUnsafe identifies a diagnostic failure or a mutation whose
	// exact baseline could not be fully restored.
	ErrGitStateUnsafe = errors.New("Git state check unsafe or restoration incomplete")
)

// GitStateCheckError classifies the result of a failed Git control-state
// check while preserving its detailed diagnostic error.
type GitStateCheckError struct {
	Restored bool
	Err      error
}

func (err *GitStateCheckError) Error() string {
	if err == nil || err.Err == nil {
		return ErrGitStateUnsafe.Error()
	}
	return err.Err.Error()
}

func (err *GitStateCheckError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func (err *GitStateCheckError) Is(target error) bool {
	if err == nil {
		return false
	}
	if err.Restored {
		return target == ErrGitStateRestored
	}
	return target == ErrGitStateUnsafe
}

func restoredGitStateError(err error) error {
	return &GitStateCheckError{Restored: true, Err: err}
}

func unsafeGitStateError(err error) error {
	return &GitStateCheckError{Err: err}
}

type gitStateHooks struct {
	beforeRestore func() error
	afterRestore  func() error
}

type gitRestoration struct {
	failures   []string
	runRefFile exactFile
	runRefRaw  map[string]exactFile
	indexInfo  os.FileInfo
}

type indexCASHooks struct {
	beforeInstall func(string) error
}

type exactPathHooks struct {
	afterRead func() error
}

type configGraphHooks struct {
	afterVisit func(string) error
}

type exactFile struct {
	path   string
	exists bool
	mode   os.FileMode
	bytes  []byte
	info   os.FileInfo
}

type privateTempDir struct {
	path         string
	name         string
	info         os.FileInfo
	parentPath   string
	parentInfo   os.FileInfo
	parent       *os.Root
	root         *os.Root
	beforeRemove func() error
}

type privateTempHooks struct {
	afterParentOpen      func() error
	afterDirectorySample func(string) error
}

type refState struct {
	object   string
	symbolic string
}

type worktreeState struct {
	path           string
	rawPath        string
	head           string
	branch         string
	detached       bool
	bare           bool
	locked         bool
	lockReason     string
	prunable       bool
	prunableReason string
}

type configOrigin struct {
	path     string
	required bool
}

type looseRefLimits struct {
	maxEntries   int
	maxPathBytes int
	maxDepth     int
	maxContents  int64
}

type rawTreeLimits struct {
	maxEntries   int
	maxPathBytes int
	maxDepth     int
	maxContents  int64
	perFile      int64
}

type rawTreeRoot struct {
	name string
	path string
}

type rawTreeHooks struct {
	afterDirectoryEOF func(string) error
	afterFileRead     func(string) error
}

type configIncludeBudget struct {
	maxEntries        int
	maxPathBytes      int
	maxDepth          int
	maxContents       int64
	perFile           int64
	maxConditions     int
	maxConditionBytes int
}

var protectedPseudorefs = []string{
	"MERGE_HEAD",
	"CHERRY_PICK_HEAD",
	"REVERT_HEAD",
	"ORIG_HEAD",
	"REBASE_HEAD",
	"BISECT_HEAD",
	"AUTO_MERGE",
	"FETCH_HEAD",
}

// CreateWorkspace creates and validates the run branch in an external cache
// worktree without changing the feature checkout.
func CreateWorkspace(ctx context.Context, spec WorkspaceSpec) (*Workspace, error) {
	return createWorkspace(ctx, spec, workspaceCreateHooks{})
}

func createWorkspace(ctx context.Context, spec WorkspaceSpec, hooks workspaceCreateHooks) (*Workspace, error) {
	if ctx == nil {
		return nil, errors.New("workspace context is required")
	}
	if !validRunID(spec.RunID) {
		return nil, fmt.Errorf("invalid run ID %q", spec.RunID)
	}
	if err := validateIdentity(spec.Identity); err != nil {
		return nil, err
	}

	repositoryRoot, err := canonicalExisting(spec.RepositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	path, err := canonicalCandidate(spec.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve cache worktree path: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("cache worktree path already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect cache worktree path: %w", err)
	}

	commonDir, err := gitPath(ctx, repositoryRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve common Git directory: %w", err)
	}
	commonDir, err = canonicalExisting(commonDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize common Git directory: %w", err)
	}
	commonInfo, err := noFollowDirectoryInfo(commonDir)
	if err != nil {
		return nil, fmt.Errorf("stat common Git directory: %w", err)
	}
	registrations, err := worktreeRegistrationsSnapshot(ctx, repositoryRoot)
	if err != nil {
		return nil, err
	}
	protected := []string{repositoryRoot, commonDir}
	for _, registration := range registrations {
		if filepath.Clean(registration.path) == path {
			return nil, fmt.Errorf("cache worktree registration already exists: %s", path)
		}
		protected = append(protected, registration.path)
	}
	for _, root := range protected {
		canonicalRoot, canonicalErr := canonicalCandidate(root)
		if canonicalErr != nil {
			return nil, fmt.Errorf("canonicalize protected Git path: %w", canonicalErr)
		}
		if pathWithin(path, canonicalRoot) {
			return nil, fmt.Errorf("cache worktree path must be external to repository and Git state: %s", path)
		}
	}

	currentBranch, err := gitPath(ctx, repositoryRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil || currentBranch != spec.FeatureBranch {
		return nil, fmt.Errorf("feature branch = %q, want %q", currentBranch, spec.FeatureBranch)
	}
	if err := checkBranchName(ctx, repositoryRoot, spec.FeatureBranch); err != nil {
		return nil, fmt.Errorf("invalid feature branch: %w", err)
	}
	resolvedHead, err := gitPath(ctx, repositoryRoot, "rev-parse", "--verify", spec.OriginalHead+"^{commit}")
	if err != nil || resolvedHead != spec.OriginalHead {
		return nil, fmt.Errorf("original HEAD is not an exact commit object")
	}
	featureHead, err := gitPath(ctx, repositoryRoot, "rev-parse", "HEAD")
	if err != nil || featureHead != spec.OriginalHead {
		return nil, fmt.Errorf("feature worktree HEAD moved from original HEAD")
	}

	branch := "togi/run-" + spec.RunID
	if err := checkBranchName(ctx, repositoryRoot, branch); err != nil {
		return nil, fmt.Errorf("invalid run branch: %w", err)
	}
	runRefName := "refs/heads/" + branch
	runRef, err := inspectRefState(ctx, repositoryRoot, runRefName)
	if err != nil {
		return nil, fmt.Errorf("inspect run branch: %w", err)
	}
	if runRef != (refState{}) {
		return nil, fmt.Errorf("run branch already exists: %s", branch)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create cache worktree parent: %w", err)
	}
	add := hooks.add
	if add == nil {
		add = addWorkspace
	}
	if err := add(ctx, repositoryRoot, path, branch, spec.OriginalHead); err != nil {
		cleanupErr := cleanupFailedWorkspaceCreation(ctx, repositoryRoot, commonDir, commonInfo, path, branch, spec.OriginalHead)
		return nil, errors.Join(fmt.Errorf("create cache worktree: %w", err), cleanupErr)
	}
	if hooks.afterAdd != nil {
		if err := hooks.afterAdd(); err != nil {
			cleanupErr := cleanupFailedWorkspaceCreation(ctx, repositoryRoot, commonDir, commonInfo, path, branch, spec.OriginalHead)
			return nil, errors.Join(fmt.Errorf("post-add creation hook: %w", err), cleanupErr)
		}
	}
	gitDir, err := validateCreatedWorkspacePath(ctx, repositoryRoot, path, commonDir, protected, branch, spec.OriginalHead)
	if err != nil {
		cleanupErr := cleanupFailedWorkspaceCreation(ctx, repositoryRoot, commonDir, commonInfo, path, branch, spec.OriginalHead)
		if cleanupErr == nil {
			cleanupErr = errors.New("creation cleanup incomplete: unsafe post-add path was preserved")
		}
		return nil, errors.Join(err, cleanupErr)
	}

	workspace := &Workspace{repositoryRoot: repositoryRoot, path: path, gitDir: gitDir, commonDir: commonDir, branch: branch, green: spec.OriginalHead, identity: spec.Identity}
	if err := workspace.bindControlState(); err != nil {
		cleanupErr := cleanupFailedWorkspaceCreation(ctx, repositoryRoot, commonDir, commonInfo, path, branch, spec.OriginalHead)
		return nil, errors.Join(errors.New("bind created workspace control state"), cleanupErr)
	}
	if err := workspace.validate(ctx); err != nil {
		cleanupErr := cleanupFailedWorkspaceCreation(ctx, repositoryRoot, commonDir, commonInfo, path, branch, spec.OriginalHead)
		return nil, errors.Join(err, cleanupErr)
	}
	return workspace, nil
}

func addWorkspace(ctx context.Context, repositoryRoot, path, branch, originalHead string) error {
	_, err := gitcmd.Output(ctx, repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"-c", "core.hooksPath="+os.DevNull,
		"worktree", "add", "-b", branch, path, originalHead)
	return err
}

// Path returns the canonical cache worktree path.
func (w *Workspace) Path() string { return w.path }

// ChangedFiles returns sorted repository-relative paths observed in the
// workspace. Rename and copy records include both source and destination.
func (w *Workspace) ChangedFiles(ctx context.Context) ([]string, error) {
	output, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit,
		"-c", "status.renames=copies", "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("list changed files: %w", err)
	}
	return parseChangedFilesV2(output)
}

func parseChangedFilesV2(output []byte) ([]string, error) {
	if len(output) == 0 {
		return []string{}, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("Git returned unterminated porcelain-v2 status")
	}
	fields := strings.Split(string(output), "\x00")
	fields = fields[:len(fields)-1]
	paths := make(map[string]struct{}, len(fields))
	for index := 0; index < len(fields); index++ {
		record := fields[index]
		if record == "" {
			return nil, errors.New("Git returned an empty porcelain-v2 status record")
		}
		var path string
		switch record[0] {
		case '1':
			parts := strings.SplitN(record, " ", 9)
			if len(parts) != 9 || parts[0] != "1" || !validTrackedMetadata(parts[1:8]) {
				return nil, errors.New("Git returned malformed ordinary status")
			}
			path = parts[8]
		case '2':
			parts := strings.SplitN(record, " ", 10)
			if len(parts) != 10 || parts[0] != "2" || !validTrackedMetadata(parts[1:8]) || !validRenameScore(parts[8]) {
				return nil, errors.New("Git returned malformed rename or copy status")
			}
			path = parts[9]
			index++
			if index >= len(fields) || fields[index] == "" {
				return nil, errors.New("Git returned rename or copy status without a source path")
			}
			if !validGitRelativePath(fields[index]) {
				return nil, errors.New("Git returned invalid rename or copy source path")
			}
			paths[fields[index]] = struct{}{}
		case 'u':
			parts := strings.SplitN(record, " ", 11)
			if len(parts) != 11 || parts[0] != "u" || !validUnmergedMetadata(parts[1:10]) {
				return nil, errors.New("Git returned malformed unmerged status")
			}
			path = parts[10]
		case '?':
			if !strings.HasPrefix(record, "? ") {
				return nil, errors.New("Git returned malformed untracked status")
			}
			path = record[2:]
		case '!':
			return nil, errors.New("Git returned an unexpected ignored status record")
		default:
			return nil, errors.New("Git returned unknown porcelain-v2 status record")
		}
		if !validGitRelativePath(path) {
			return nil, errors.New("Git returned invalid changed path")
		}
		paths[path] = struct{}{}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func validTrackedMetadata(fields []string) bool {
	return len(fields) == 7 && validXY(fields[0]) && validSubmodule(fields[1]) &&
		validMode(fields[2]) && validMode(fields[3]) && validMode(fields[4]) && validOID(fields[5]) && validOID(fields[6])
}

func validUnmergedMetadata(fields []string) bool {
	return len(fields) == 9 && validXY(fields[0]) && validSubmodule(fields[1]) && validMode(fields[2]) &&
		validMode(fields[3]) && validMode(fields[4]) && validMode(fields[5]) && validOID(fields[6]) && validOID(fields[7]) && validOID(fields[8])
}

func validXY(value string) bool {
	if len(value) != 2 {
		return false
	}
	const statuses = ".MADRCUT"
	return strings.ContainsRune(statuses, rune(value[0])) && strings.ContainsRune(statuses, rune(value[1]))
}

func validSubmodule(value string) bool {
	return len(value) == 4 && (value[0] == 'N' || value[0] == 'S') &&
		(value[1] == '.' || value[1] == 'C') && (value[2] == '.' || value[2] == 'M') && (value[3] == '.' || value[3] == 'U')
}

func validMode(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, digit := range []byte(value) {
		if digit < '0' || digit > '7' {
			return false
		}
	}
	return true
}

func validOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, digit := range []byte(value) {
		if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f')) {
			return false
		}
	}
	return true
}

func validRenameScore(value string) bool {
	if len(value) < 2 || len(value) > 4 || (value[0] != 'R' && value[0] != 'C') {
		return false
	}
	score := 0
	for _, digit := range []byte(value[1:]) {
		if digit < '0' || digit > '9' {
			return false
		}
		score = score*10 + int(digit-'0')
	}
	return score <= 100
}

func validGitRelativePath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

// ResetAttempt discards tracked, staged, and untracked attempt edits and
// restores the latest validated batch commit.
func (w *Workspace) ResetAttempt(ctx context.Context) error {
	if err := w.discardValidationSnapshot(nil); err != nil {
		return fmt.Errorf("discard validation snapshot: %w", err)
	}
	ref := "refs/heads/" + w.branch
	if err := w.requireDirectRunRef(ctx, w.green, true); err != nil {
		return fmt.Errorf("refuse attempt reset: %w", err)
	}
	if _, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "-c", "core.hooksPath="+os.DevNull, "update-ref", "--no-deref", ref, w.green, w.green); err != nil {
		return errors.New("refuse attempt reset: run ref moved from latest green commit")
	}
	if err := w.validateAttemptIndexBinding(); err != nil {
		return fmt.Errorf("refuse attempt tree reset: %w", err)
	}
	expectedTree, err := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.commonDir, "rev-parse", w.green+"^{tree}")
	if err != nil {
		return errors.New("resolve latest green tree")
	}
	if _, err := w.mutateOwnedIndex(ctx, true, expectedTree, "read-tree", "--reset", "-u", w.green); err != nil {
		return fmt.Errorf("verify reset index result: %w", err)
	}
	if err := w.validateControlBinding(); err != nil {
		return fmt.Errorf("refuse attempt clean: %w", err)
	}
	if _, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.gitDir, "--work-tree="+w.path, "-c", "core.hooksPath="+os.DevNull, "clean", "-ffdx"); err != nil {
		return fmt.Errorf("remove untracked attempt files: %w", err)
	}
	if w.beforeResetFinal != nil {
		if err := w.beforeResetFinal(); err != nil {
			return fmt.Errorf("before final attempt reset verification: %w", err)
		}
	}
	if err := w.requireDirectRunRef(ctx, w.green, false); err != nil {
		return fmt.Errorf("refuse completed attempt reset: %w", err)
	}
	return nil
}

// PrepareBatch stages the exact complete attempt once and binds validation to
// its tree, index, protected files, and stable worktree identities.
func (w *Workspace) PrepareBatch(ctx context.Context, changed []string) (BatchProof, error) {
	if ctx == nil {
		return BatchProof{}, errors.New("batch preparation context is required")
	}
	if w.validationSnapshot != nil {
		return BatchProof{}, errors.New("previous validation snapshot is still active")
	}
	want := slices.Clone(changed)
	slices.Sort(want)
	want = slices.Compact(want)
	if len(want) == 0 {
		return BatchProof{}, errors.New("cannot prepare an empty batch")
	}
	for _, name := range want {
		if !validRelativePath(name) {
			return BatchProof{}, errors.New("invalid changed batch path")
		}
	}
	observed, err := w.ChangedFiles(ctx)
	if err != nil || !slices.Equal(observed, want) {
		return BatchProof{}, errors.New("changed batch set moved before preparation")
	}
	if err := w.requireDirectRunRef(ctx, w.green, false); err != nil {
		return BatchProof{}, fmt.Errorf("refuse batch preparation: %w", err)
	}
	before, err := w.captureBatchWorktree(ctx, want)
	if err != nil {
		return BatchProof{}, err
	}
	tree, err := w.mutateOwnedIndex(ctx, false, "", "-c", "core.hooksPath="+os.DevNull, "add", "-A", "--", ".")
	if err != nil {
		return BatchProof{}, fmt.Errorf("stage prepared batch: %w", err)
	}
	observed, err = w.ChangedFiles(ctx)
	if err != nil || !slices.Equal(observed, want) {
		return BatchProof{}, errors.New("changed batch set moved during preparation")
	}
	proof, err := w.captureBatchProof(ctx, want, tree)
	if err != nil {
		return BatchProof{}, err
	}
	if !batchWorktreeStateEqual(before, proof) {
		return BatchProof{}, errors.New("prepared batch worktree changed while staging")
	}
	validation, err := w.materializeValidationSnapshot(ctx, tree)
	if err != nil {
		return BatchProof{}, err
	}
	w.validationSnapshot = validation
	proof.validation = validation
	proof.validationFiles = cloneExactFiles(validation.files)
	return cloneBatchProof(proof), nil
}

// VerifyBatch proves the staged tree and worktree still exactly match a
// prepared batch without mutating either surface.
func (w *Workspace) VerifyBatch(ctx context.Context, proof BatchProof) error {
	return w.verifyBatch(ctx, proof, w.green, true, true)
}

func (w *Workspace) verifyBatch(ctx context.Context, proof BatchProof, expectedRef string, expectAttemptChanges, verifyValidation bool) error {
	if ctx == nil {
		return errors.New("batch verification context is required")
	}
	if (verifyValidation && !proof.validFor(w)) || (!verifyValidation && (proof.owner != w || !validObjectID(proof.tree) || !proof.index.exists)) {
		return errors.New("batch proof is not owned by this workspace")
	}
	if err := w.requireDirectRunRef(ctx, expectedRef, false); err != nil {
		return fmt.Errorf("refuse batch verification: %w", err)
	}
	observed, err := w.ChangedFiles(ctx)
	wantChanged := proof.changed
	if !expectAttemptChanges {
		wantChanged = nil
	}
	if err != nil || !slices.Equal(observed, wantChanged) {
		return errors.New("prepared batch changed set drifted")
	}
	current, err := w.captureBatchProof(ctx, proof.changed, proof.tree)
	if err != nil {
		return err
	}
	if !batchProofStateEqual(proof, current) {
		return errors.New("prepared batch worktree or index drifted")
	}
	if verifyValidation {
		currentValidation, err := snapshotRawTrees([]rawTreeRoot{{name: "validation", path: proof.ValidationRoot()}}, validationSnapshotLimits)
		if err != nil || !exactFileStableMapsEqual(proof.validationFiles, currentValidation) {
			return errors.New("prepared validation snapshot drifted")
		}
	}
	return nil
}

func (w *Workspace) captureBatchProof(ctx context.Context, changed []string, tree string) (BatchProof, error) {
	proof, err := w.captureBatchWorktree(ctx, changed)
	if err != nil {
		return BatchProof{}, err
	}
	index, err := snapshotExactPath(w.indexPath, indexByteLimit)
	if err != nil || !index.exists || !index.mode.IsRegular() {
		return BatchProof{}, errors.New("prepared batch index is unavailable")
	}
	indexTree, err := gitPath(ctx, w.path, "--git-dir="+w.gitDir, "--work-tree="+w.path, "write-tree")
	if err != nil || indexTree != tree {
		return BatchProof{}, errors.New("prepared batch index tree drifted")
	}
	if err := w.validateStagedDirectories(ctx, proof.dirs); err != nil {
		return BatchProof{}, err
	}
	proof.owner = w
	proof.tree = tree
	proof.index = index
	proof.verify = w.VerifyBatch
	return proof, nil
}

func (w *Workspace) captureBatchWorktree(ctx context.Context, changed []string) (BatchProof, error) {
	ignored, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, batchIgnoredInventoryLimit,
		"--git-dir="+w.gitDir, "--work-tree="+w.path, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--")
	if err != nil {
		return BatchProof{}, errors.New("inspect ignored worktree entries")
	}
	if len(ignored) != 0 {
		return BatchProof{}, errors.New("unexpected ignored worktree entries")
	}
	protected, err := SnapshotAttempt(w.path)
	if err != nil {
		return BatchProof{}, fmt.Errorf("snapshot prepared protected tree: %w", err)
	}
	paths := make(map[string]struct{}, len(changed)+len(protected.Files))
	for _, name := range changed {
		paths[name] = struct{}{}
	}
	for name := range protected.Files {
		paths[name] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for name := range paths {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	files := make(map[string]exactFile, len(ordered))
	dirs, err := snapshotWorktreeDirectories(w.path)
	if err != nil {
		return BatchProof{}, errors.New("snapshot prepared worktree directories")
	}
	total := 0
	for _, name := range ordered {
		if !validRelativePath(name) {
			return BatchProof{}, errors.New("prepared batch contains invalid path")
		}
		current := w.path
		components := strings.Split(filepath.ToSlash(filepath.Dir(name)), "/")
		if filepath.Dir(name) != "." {
			for _, component := range components {
				current = filepath.Join(current, filepath.FromSlash(component))
				info, statErr := os.Lstat(current)
				if errors.Is(statErr, os.ErrNotExist) {
					break
				}
				if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return BatchProof{}, errors.New("prepared batch directory binding is unsafe")
				}
				relative, _ := filepath.Rel(w.path, current)
				dirs[filepath.ToSlash(relative)] = info
			}
		}
		file, snapErr := snapshotExactPath(filepath.Join(w.path, filepath.FromSlash(name)), gitOutputLimit)
		if snapErr != nil || file.exists && !file.mode.IsRegular() {
			return BatchProof{}, errors.New("prepared batch file binding is unsafe")
		}
		if len(file.bytes) > indexByteLimit-total {
			return BatchProof{}, errors.New("prepared batch files exceed capture limit")
		}
		total += len(file.bytes)
		files[name] = file
	}
	return BatchProof{changed: slices.Clone(changed), files: files, dirs: dirs, protected: protected}, nil
}

func (w *Workspace) validateStagedDirectories(ctx context.Context, directories map[string]os.FileInfo) error {
	output, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, batchIgnoredInventoryLimit,
		"--git-dir="+w.gitDir, "--work-tree="+w.path, "ls-files", "--cached", "-z", "--")
	if err != nil || len(output) != 0 && output[len(output)-1] != 0 {
		return errors.New("inspect prepared index paths")
	}
	allowed := map[string]struct{}{".": {}}
	for len(output) != 0 {
		end := bytes.IndexByte(output, 0)
		if end < 0 {
			return errors.New("inspect prepared index paths")
		}
		name := string(output[:end])
		output = output[end+1:]
		if !validRelativePath(name) {
			return errors.New("prepared index contains an invalid path")
		}
		for current := name; current != "."; current = path.Dir(current) {
			allowed[current] = struct{}{}
		}
	}
	for directory := range directories {
		if _, ok := allowed[directory]; !ok {
			return errors.New("worktree directory is absent from prepared index")
		}
	}
	return nil
}

type batchDirectoryBudget struct {
	entries   int
	pathBytes int
}

func snapshotWorktreeDirectories(rootPath string) (map[string]os.FileInfo, error) {
	rootInfo, err := noFollowDirectoryInfo(rootPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directories := map[string]os.FileInfo{".": rootInfo}
	budget := &batchDirectoryBudget{}
	if err := snapshotWorktreeDirectory(root, "", rootInfo, 0, budget, directories); err != nil {
		return nil, err
	}
	return directories, nil
}

func snapshotWorktreeDirectory(root *os.Root, relative string, expected os.FileInfo, depth int, budget *batchDirectoryBudget, directories map[string]os.FileInfo) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	bound, err := directory.Stat()
	if err != nil || !stableFileInfo(expected, bound) {
		return errors.New("worktree directory binding changed")
	}
	for {
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			childRelative := name
			if relative != "" {
				childRelative = relative + "/" + name
			}
			budget.entries++
			budget.pathBytes += len(childRelative)
			if budget.entries > batchDirectoryEntryLimit || budget.pathBytes > batchDirectoryPathLimit || depth+1 > batchDirectoryDepthLimit {
				return errors.New("worktree directory inventory exceeds limits")
			}
			info, err := root.Lstat(name)
			if err != nil {
				return errors.New("inspect worktree directory entry")
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			child, err := root.OpenRoot(name)
			if err != nil {
				return errors.New("open worktree child directory")
			}
			childBound, childErr := child.Stat(".")
			namedAgain, namedErr := root.Lstat(name)
			if childErr != nil || namedErr != nil || !stableFileInfo(info, childBound) || !stableFileInfo(info, namedAgain) {
				_ = child.Close()
				return errors.New("worktree child directory binding changed")
			}
			directories[childRelative] = childBound
			if err := snapshotWorktreeDirectory(child, childRelative, childBound, depth+1, budget, directories); err != nil {
				_ = child.Close()
				return err
			}
			_ = child.Close()
			namedAfter, err := root.Lstat(name)
			if err != nil || !stableFileInfo(childBound, namedAfter) {
				return errors.New("worktree child directory changed")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return errors.New("read worktree directory")
		}
	}
	final, err := root.Stat(".")
	namedFinal, namedErr := root.Lstat(".")
	if err != nil || namedErr != nil || !stableFileInfo(expected, final) || !stableFileInfo(expected, namedFinal) {
		return errors.New("worktree directory changed while reading")
	}
	return nil
}

func batchWorktreeStateEqual(expected, current BatchProof) bool {
	return slices.Equal(expected.changed, current.changed) && exactFileStableMapsEqual(expected.files, current.files) &&
		directoryInfosEqual(expected.dirs, current.dirs) && reflect.DeepEqual(expected.protected.Files, current.protected.Files)
}

func batchProofStateEqual(expected, current BatchProof) bool {
	return expected.owner == current.owner && expected.tree == current.tree && slices.Equal(expected.changed, current.changed) &&
		exactFileStableMapsEqual(expected.files, current.files) && directoryInfosEqual(expected.dirs, current.dirs) &&
		reflect.DeepEqual(expected.protected.Files, current.protected.Files) && exactFileStableEqual(expected.index, current.index)
}

func directoryInfosEqual(expected, current map[string]os.FileInfo) bool {
	if len(expected) != len(current) {
		return false
	}
	for name, left := range expected {
		right, exists := current[name]
		if !exists || !stableFileInfo(left, right) {
			return false
		}
	}
	return true
}

func (w *Workspace) rawValidationArchive(ctx context.Context, tree string) ([]byte, error) {
	listing, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, 16<<20,
		"--git-dir="+w.commonDir, "ls-tree", "-rz", "-r", "--full-tree", tree)
	if err != nil || len(listing) != 0 && listing[len(listing)-1] != 0 {
		return nil, errors.New("list prepared validation tree")
	}
	records := bytes.Split(listing, []byte{0})
	if len(records) > 0 && len(records[len(records)-1]) == 0 {
		records = records[:len(records)-1]
	}
	if len(records) > validationSnapshotLimits.maxEntries {
		return nil, errors.New("prepared validation tree exceeds shape limits")
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	pathBytes := 0
	var contentBytes int64
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		metadata, rawName, found := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		name := string(rawName)
		pathBytes += len(name)
		if !found || len(fields) != 3 || fields[1] != "blob" || !validObjectID(fields[2]) || name == "" ||
			path.Clean(name) != name || !validRelativePath(filepath.FromSlash(name)) || strings.Contains(name, `\`) ||
			pathBytes > validationSnapshotLimits.maxPathBytes || strings.Count(name, "/")+1 > validationSnapshotLimits.maxDepth {
			return nil, errors.New("prepared validation tree contains an unsafe entry")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, errors.New("prepared validation tree contains duplicate entries")
		}
		seen[name] = struct{}{}
		mode := int64(0o644)
		if fields[0] == "100755" {
			mode = 0o755
		} else if fields[0] != "100644" {
			return nil, errors.New("prepared validation tree contains links or special entries")
		}
		contents, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, int(validationSnapshotLimits.perFile)+1,
			"--git-dir="+w.commonDir, "cat-file", "blob", fields[2])
		contentBytes += int64(len(contents))
		if err != nil || contentBytes > validationSnapshotLimits.maxContents {
			return nil, errors.New("read prepared validation blob")
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			return nil, errors.New("encode prepared validation tree")
		}
		if _, err := writer.Write(contents); err != nil {
			return nil, errors.New("encode prepared validation blob")
		}
	}
	if err := writer.Close(); err != nil || archive.Len() > int(indexByteLimit)+(16<<20) {
		return nil, errors.New("encode prepared validation tree")
	}
	return archive.Bytes(), nil
}

func (w *Workspace) materializeValidationSnapshot(ctx context.Context, tree string) (snapshot *validationSnapshot, resultErr error) {
	archive, err := w.rawValidationArchive(ctx, tree)
	if err != nil {
		return nil, errors.New("materialize prepared validation tree")
	}
	private, err := w.createPrivateTempDir()
	if err != nil {
		return nil, errors.New("create private validation snapshot")
	}
	private.beforeRemove = w.validationBeforePrivateRemove
	remove := true
	defer func() {
		if remove {
			if discardErr := w.discardPrivateValidation(private); discardErr != nil {
				w.validationSnapshot = &validationSnapshot{private: private}
				resultErr = errors.Join(resultErr, errors.New("discard incomplete validation snapshot"))
			}
		}
	}()
	if w.validationMaterializeFailure != nil {
		if err := w.validationMaterializeFailure(); err != nil {
			return nil, errors.New("materialize prepared validation tree")
		}
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	types := make(map[string]byte)
	files := make(map[string]os.FileMode)
	directories := map[string]struct{}{".": {}}
	entries, pathBytes := 0, 0
	var contentBytes int64
	for {
		header, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, errors.New("decode prepared validation tree")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "" || path.Clean(name) != name || !validRelativePath(filepath.FromSlash(name)) || strings.Contains(name, `\`) {
			return nil, errors.New("prepared validation tree contains an unsafe path")
		}
		entries++
		pathBytes += len(name)
		if entries > validationSnapshotLimits.maxEntries || pathBytes > validationSnapshotLimits.maxPathBytes || strings.Count(name, "/")+1 > validationSnapshotLimits.maxDepth {
			return nil, errors.New("prepared validation tree exceeds shape limits")
		}
		native := filepath.FromSlash(name)
		for parent := filepath.Dir(native); parent != "."; parent = filepath.Dir(parent) {
			directories[parent] = struct{}{}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if prior, exists := types[name]; exists && prior != tar.TypeDir {
				return nil, errors.New("prepared validation tree contains conflicting entries")
			}
			if err := private.root.MkdirAll(native, 0o700); err != nil {
				return nil, errors.New("create prepared validation directory")
			}
			types[name] = tar.TypeDir
			directories[native] = struct{}{}
		case tar.TypeReg, tar.TypeRegA:
			contentBytes += header.Size
			if _, exists := types[name]; exists || header.Size < 0 || header.Size > indexByteLimit || contentBytes > indexByteLimit {
				return nil, errors.New("prepared validation tree contains an invalid file")
			}
			if err := private.root.MkdirAll(filepath.Dir(native), 0o700); err != nil {
				return nil, errors.New("create prepared validation parent")
			}
			file, err := private.root.OpenFile(native, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, errors.New("create prepared validation file")
			}
			written, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil || written != header.Size {
				return nil, errors.New("write prepared validation file")
			}
			types[name] = tar.TypeReg
			mode := os.FileMode(0o444)
			if header.Mode&0o111 != 0 {
				mode = 0o555
			}
			files[native] = mode
		default:
			return nil, errors.New("prepared validation tree contains links or special entries")
		}
	}
	for name, mode := range files {
		info, err := private.root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("bind prepared validation file")
		}
		if err := private.root.Chmod(name, mode); err != nil {
			return nil, errors.New("seal prepared validation file")
		}
	}
	directoryNames := make([]string, 0, len(directories))
	for name := range directories {
		directoryNames = append(directoryNames, name)
	}
	slices.SortFunc(directoryNames, func(left, right string) int {
		return strings.Count(right, string(filepath.Separator)) - strings.Count(left, string(filepath.Separator))
	})
	for _, name := range directoryNames {
		if err := private.root.Chmod(name, 0o555); err != nil {
			return nil, errors.New("seal prepared validation directory")
		}
	}
	captured, err := snapshotRawTrees([]rawTreeRoot{{name: "validation", path: private.path}}, validationSnapshotLimits)
	if err != nil {
		return nil, errors.New("snapshot prepared validation tree")
	}
	remove = false
	return &validationSnapshot{private: private, files: captured}, nil
}

func (w *Workspace) discardValidationSnapshot(proof *BatchProof) error {
	snapshot := w.validationSnapshot
	if snapshot == nil {
		return nil
	}
	if proof != nil && proof.validation != snapshot {
		return errors.New("validation snapshot is not owned by batch proof")
	}
	if snapshot.private == nil {
		return errors.New("validation snapshot private state is unavailable")
	}
	if err := w.discardPrivateValidation(snapshot.private); err != nil {
		return err
	}
	w.validationSnapshot = nil
	return nil
}

func (w *Workspace) discardPrivateValidation(private *privateTempDir) error {
	if w.validationDiscardFailure != nil {
		if err := w.validationDiscardFailure(); err != nil {
			return err
		}
	}
	return private.discard()
}

// CommitBatch records the exact prepared tree without restaging it. The
// latest green commit advances only after the proof survives final checks.
func (w *Workspace) CommitBatch(ctx context.Context, primaryFile string, proof BatchProof) (string, error) {
	if !validRelativePath(primaryFile) {
		return "", fmt.Errorf("invalid primary file %q", primaryFile)
	}
	if err := w.VerifyBatch(ctx, proof); err != nil {
		if discardErr := w.discardValidationSnapshot(&proof); discardErr != nil {
			return "", fmt.Errorf("refuse batch commit and discard validation snapshot: %w", discardErr)
		}
		return "", fmt.Errorf("refuse batch commit: %w", err)
	}
	if err := w.discardValidationSnapshot(&proof); err != nil {
		return "", fmt.Errorf("discard validated snapshot before commit: %w", err)
	}
	if err := w.verifyBatch(ctx, proof, w.green, true, false); err != nil {
		return "", fmt.Errorf("refuse batch commit after snapshot cleanup: %w", err)
	}
	environment := map[string]string{
		"GIT_AUTHOR_NAME":     w.identity.Name,
		"GIT_AUTHOR_EMAIL":    w.identity.Email,
		"GIT_COMMITTER_NAME":  w.identity.Name,
		"GIT_COMMITTER_EMAIL": w.identity.Email,
	}
	if err := w.requireDirectRunRef(ctx, w.green, false); err != nil {
		return "", fmt.Errorf("refuse staged batch commit: %w", err)
	}
	tree := proof.tree
	if err := w.validateControlBinding(); err != nil {
		return "", fmt.Errorf("refuse batch object creation: %w", err)
	}
	commitOutput, err := gitcmd.OutputEnv(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit, environment,
		"--git-dir="+w.commonDir, "commit-tree", tree, "-p", w.green, "-m", "togi batch: "+primaryFile)
	if err != nil {
		return "", fmt.Errorf("create batch commit: %w", err)
	}
	commit := strings.TrimSpace(string(commitOutput))
	if !validObjectID(commit) {
		return "", errors.New("create batch commit returned an invalid object ID")
	}
	if err := w.verifyBatch(ctx, proof, w.green, true, false); err != nil {
		return "", fmt.Errorf("refuse batch ref update: %w", err)
	}
	if err := w.requireDirectRunRef(ctx, w.green, false); err != nil {
		return "", fmt.Errorf("refuse batch ref update: %w", err)
	}
	if w.beforeBatchRefUpdate != nil {
		if err := w.beforeBatchRefUpdate(); err != nil {
			return "", fmt.Errorf("before batch ref update: %w", err)
		}
	}
	if err := w.verifyBatch(ctx, proof, w.green, true, false); err != nil {
		return "", fmt.Errorf("refuse final batch ref update: %w", err)
	}
	ref := "refs/heads/" + w.branch
	if err := w.compareAndSwapBatchRef(ctx, ref, commit, w.green); err != nil {
		recoveryCtx, cancelRecovery := recoveryContext(ctx)
		defer cancelRecovery()
		if w.requireDirectRunRef(recoveryCtx, commit, false) == nil {
			if rollbackErr := w.updateBatchRefDirect(recoveryCtx, ref, w.green, commit); rollbackErr != nil {
				return "", errors.New("batch ref update was ambiguous and rollback failed")
			}
		} else if w.requireDirectRunRef(recoveryCtx, w.green, false) != nil {
			return "", errors.New("batch ref update failed with an unexpected run ref")
		}
		return "", errors.New("advance batch run ref by compare-and-swap")
	}
	recoveryCtx, cancelRecovery := recoveryContext(ctx)
	defer cancelRecovery()
	postUpdateErr := error(nil)
	if w.afterBatchRefUpdate != nil {
		postUpdateErr = w.afterBatchRefUpdate()
	}
	if postUpdateErr == nil {
		if cause := context.Cause(ctx); cause != nil {
			postUpdateErr = cause
		}
	}
	if verifyErr := w.verifyBatch(recoveryCtx, proof, commit, false, false); postUpdateErr == nil {
		postUpdateErr = verifyErr
	}
	if postUpdateErr == nil {
		postUpdateErr = context.Cause(ctx)
	}
	if postUpdateErr != nil {
		if rollbackErr := w.updateBatchRefDirect(recoveryCtx, ref, w.green, commit); rollbackErr != nil {
			return "", errors.New("batch proof drifted after ref update and rollback failed")
		}
		return "", fmt.Errorf("batch proof drifted after ref update: %w", postUpdateErr)
	}
	w.green = commit
	return commit, nil
}

func (w *Workspace) compareAndSwapBatchRef(ctx context.Context, ref, next, previous string) error {
	if w.updateBatchRef != nil {
		return w.updateBatchRef(ctx, ref, next, previous)
	}
	return w.updateBatchRefDirect(ctx, ref, next, previous)
}

func (w *Workspace) updateBatchRefDirect(ctx context.Context, ref, next, previous string) error {
	_, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "-c", "core.hooksPath="+os.DevNull, "update-ref", "--no-deref", ref, next, previous)
	return err
}

// RollbackBatch restores the parent of the exact most recently committed
// batch. The owned run ref is moved only by compare-and-swap.
func (w *Workspace) RollbackBatch(ctx context.Context, commit string) error {
	if !validObjectID(commit) || commit != w.green {
		return errors.New("refuse batch rollback: commit is not the latest green commit")
	}
	if err := w.requireDirectRunRef(ctx, commit, false); err != nil {
		return fmt.Errorf("refuse batch rollback: %w", err)
	}
	parent, err := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.commonDir, "rev-parse", commit+"^")
	if err != nil || !validObjectID(parent) {
		return errors.New("refuse batch rollback: resolve exact parent")
	}
	ref := "refs/heads/" + w.branch
	if _, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "-c", "core.hooksPath="+os.DevNull, "update-ref", "--no-deref", ref, parent, commit); err != nil {
		return errors.New("refuse batch rollback: run ref moved from committed batch")
	}
	w.green = parent
	if err := w.ResetAttempt(ctx); err != nil {
		return fmt.Errorf("batch ref rolled back but tree restoration failed: %w", err)
	}
	return nil
}

func (w *Workspace) requireDirectRunRef(ctx context.Context, expected string, allowAttemptIndex bool) error {
	var bindingErr error
	if allowAttemptIndex {
		bindingErr = w.validateAttemptIndexBinding()
	} else {
		bindingErr = w.validateControlBinding()
	}
	if bindingErr != nil {
		return bindingErr
	}
	head, err := gitPath(ctx, w.path, "--git-dir="+w.gitDir, "--work-tree="+w.path, "symbolic-ref", "-q", "HEAD")
	if err != nil || head != "refs/heads/"+w.branch {
		return errors.New("workspace HEAD is not the owned run branch")
	}
	state, err := inspectRefStateAt(ctx, w.repositoryRoot, w.commonDir, head)
	if err != nil {
		return errors.New("inspect owned run ref")
	}
	if state.symbolic != "" || state.object == "" || state.object != expected {
		return errors.New("owned run ref is symbolic, missing, or unexpected")
	}
	return nil
}

// SnapshotGitState captures every Git control surface an agent is forbidden
// to mutate. Ordinary worktree file bytes are deliberately excluded.
func (w *Workspace) SnapshotGitState(ctx context.Context) (GitState, error) {
	if err := w.validateControlBinding(); err != nil {
		return GitState{}, fmt.Errorf("snapshot workspace control binding: %w", err)
	}
	return w.snapshotGitState(ctx, false)
}

func (w *Workspace) snapshotGitState(ctx context.Context, allowDamagedHEAD bool) (GitState, error) {
	rootInfo, err := noFollowDirectoryInfo(w.path)
	if err != nil {
		return GitState{}, errors.New("snapshot workspace root failed")
	}
	dotGit, err := snapshotExactPath(filepath.Join(w.path, ".git"), gitOutputLimit)
	if err != nil {
		return GitState{}, fmt.Errorf("snapshot .git indirection: %w", err)
	}
	if !dotGit.exists || !dotGit.mode.IsRegular() {
		return GitState{}, errors.New("workspace .git indirection is not a regular file")
	}
	gitDir, err := gitPath(ctx, w.path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return GitState{}, fmt.Errorf("resolve linked worktree Git directory: %w", err)
	}
	gitDir, err = canonicalExisting(gitDir)
	if err != nil {
		return GitState{}, fmt.Errorf("canonicalize linked worktree Git directory: %w", err)
	}
	commonDir, err := gitPath(ctx, w.path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return GitState{}, fmt.Errorf("resolve common Git directory: %w", err)
	}
	commonDir, err = canonicalExisting(commonDir)
	if err != nil {
		return GitState{}, fmt.Errorf("canonicalize common Git directory: %w", err)
	}
	if !allowDamagedHEAD && (gitDir != w.gitDir || commonDir != w.commonDir) {
		return GitState{}, errors.New("workspace Git control paths changed")
	}
	gitDirInfo, err := noFollowDirectoryInfo(gitDir)
	if err != nil {
		return GitState{}, fmt.Errorf("stat linked worktree Git directory: %w", err)
	}
	commonInfo, err := noFollowDirectoryInfo(commonDir)
	if err != nil {
		return GitState{}, fmt.Errorf("stat common Git directory: %w", err)
	}
	head, headErr := gitPath(ctx, w.path, "rev-parse", "--verify", "HEAD")
	symbolicHead, symbolicErr := gitPath(ctx, w.path, "symbolic-ref", "-q", "HEAD")
	if !allowDamagedHEAD {
		if headErr != nil {
			return GitState{}, fmt.Errorf("snapshot workspace HEAD: %w", headErr)
		}
		if symbolicErr != nil || symbolicHead != "refs/heads/"+w.branch {
			return GitState{}, errors.New("snapshot workspace HEAD is not the owned run branch")
		}
	} else {
		if headErr != nil {
			head = ""
		}
		if symbolicErr != nil {
			symbolicHead = ""
		}
	}
	indexPath, indexTree, indexBytes, indexInfo, err := w.snapshotIndex(ctx, gitDir, commonDir)
	if err != nil {
		return GitState{}, err
	}
	worktreesOutput, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return GitState{}, fmt.Errorf("snapshot worktree registrations: %w", err)
	}
	worktrees, err := parseWorktreeStates(worktreesOutput)
	if err != nil {
		return GitState{}, fmt.Errorf("snapshot worktree registrations: %w", err)
	}
	configGraph, configs, err := w.snapshotLocalConfig(ctx, gitDir, commonDir)
	if err != nil {
		return GitState{}, errors.New("snapshot local config include closure failed")
	}
	pseudorefs, err := w.snapshotGitFiles(ctx, protectedPseudorefs, gitDir, commonDir)
	if err != nil {
		return GitState{}, fmt.Errorf("snapshot pseudorefs: %w", err)
	}
	refFormat, err := gitPath(ctx, w.path, "rev-parse", "--show-ref-format")
	if err != nil || refFormat != "files" {
		return GitState{}, errors.New("snapshot refs requires the files ref backend")
	}
	rawHooks, err := snapshotRawTrees([]rawTreeRoot{
		{name: "common", path: filepath.Join(commonDir, "hooks")},
		{name: "worktree", path: filepath.Join(gitDir, "hooks")},
	}, defaultRawControlLimits)
	if err != nil {
		return GitState{}, errors.New("snapshot raw hook storage failed")
	}
	rawControl, err := snapshotRawControlFiles(gitDir, commonDir)
	if err != nil {
		return GitState{}, errors.New("snapshot raw control file failed")
	}
	looseRoots := []string{filepath.Join(commonDir, "refs"), filepath.Join(gitDir, "refs")}
	rawRefRoots := []rawTreeRoot{
		{name: "common", path: looseRoots[0]},
		{name: "worktree", path: looseRoots[1]},
	}
	rawRefsBefore, err := snapshotRawTrees(rawRefRoots, defaultRawControlLimits)
	if err != nil {
		return GitState{}, errors.New("snapshot raw ref storage failed")
	}
	looseBefore, err := snapshotLooseSymbolicRefsWithLimits(looseRoots, defaultLooseRefLimits)
	if err != nil {
		return GitState{}, errors.New("snapshot loose symbolic refs failed")
	}
	refsOutput, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)")
	if err != nil {
		return GitState{}, fmt.Errorf("snapshot refs: %w", err)
	}
	refs, err := parseRefs(refsOutput)
	if err != nil {
		return GitState{}, err
	}
	looseAfter, err := snapshotLooseSymbolicRefsWithLimits(looseRoots, defaultLooseRefLimits)
	if err != nil || !refMapsEqualExcept(looseBefore, looseAfter, "") {
		return GitState{}, errors.New("loose symbolic refs changed while snapshotting")
	}
	if err := mergeLooseSymbolicRefs(refs, looseAfter); err != nil {
		return GitState{}, err
	}
	rawRefsAfter, err := snapshotRawTrees(rawRefRoots, defaultRawControlLimits)
	if err != nil || !exactFileStableMapsEqual(rawRefsBefore, rawRefsAfter) {
		return GitState{}, errors.New("raw ref storage changed while snapshotting")
	}
	rawHooksAfter, err := snapshotRawTrees([]rawTreeRoot{
		{name: "common", path: filepath.Join(commonDir, "hooks")},
		{name: "worktree", path: filepath.Join(gitDir, "hooks")},
	}, defaultRawControlLimits)
	if err != nil || !exactFileStableMapsEqual(rawHooks, rawHooksAfter) {
		return GitState{}, errors.New("raw hook storage changed while snapshotting")
	}
	rawControlAfter, err := snapshotRawControlFiles(gitDir, commonDir)
	if err != nil || !exactFileStableMapsEqual(rawControl, rawControlAfter) {
		return GitState{}, errors.New("raw control files changed while snapshotting")
	}
	runRef := refs["refs/heads/"+w.branch]
	if !allowDamagedHEAD && (runRef.object != head || runRef.symbolic != "") {
		return GitState{}, errors.New("snapshot owned run ref is not a direct ref at workspace HEAD")
	}
	return GitState{
		owner: w, rootInfo: rootInfo, dotGit: dotGit, gitDir: gitDir, gitDirInfo: gitDirInfo,
		commonDir: commonDir, commonInfo: commonInfo,
		head: head, symbolicHead: symbolicHead, indexTree: indexTree,
		indexPath: indexPath, indexBytes: indexBytes, indexInfo: indexInfo, runRef: runRef,
		worktrees: worktrees, configGraph: configGraph, configs: configs, pseudorefs: pseudorefs, refs: refs,
		rawHooks: rawHooksAfter, rawRefs: rawRefsAfter, rawControl: rawControlAfter,
	}, nil
}

// CheckGitState rejects unauthorized Git mutations and restores only state
// whose current value still exactly matches the observed mutation.
func (w *Workspace) CheckGitState(ctx context.Context, before GitState) error {
	return w.checkGitState(ctx, before, gitStateHooks{})
}

func (w *Workspace) checkGitState(ctx context.Context, before GitState, hooks gitStateHooks) error {
	if before.owner != w {
		return unsafeGitStateError(errors.New("Git state snapshot does not belong to this workspace"))
	}
	currentDotGit, err := snapshotExactPath(filepath.Join(w.path, ".git"), gitOutputLimit)
	if err != nil || !exactFileStableEqual(before.dotGit, currentDotGit) {
		return unsafeGitStateError(errors.New("unauthorized Git control binding mutation; restoration incomplete: .git indirection preserved"))
	}
	after, err := w.snapshotGitState(ctx, true)
	if err != nil {
		return unsafeGitStateError(errors.New("unauthorized Git state mutation; restoration incomplete: Git control binding diagnostic snapshot failed"))
	}
	if before.rootInfo == nil || after.rootInfo == nil || !os.SameFile(before.rootInfo, after.rootInfo) ||
		before.gitDir != after.gitDir || before.commonDir != after.commonDir || before.indexPath != after.indexPath ||
		!os.SameFile(before.gitDirInfo, after.gitDirInfo) || !os.SameFile(before.commonInfo, after.commonInfo) {
		return unsafeGitStateError(errors.New("unauthorized Git control binding mutation; restoration incomplete: foreign Git control paths preserved"))
	}
	violations := w.gitStateViolations(before, after)
	if len(violations) == 0 {
		return nil
	}
	if hooks.beforeRestore != nil {
		if err := hooks.beforeRestore(); err != nil {
			return unsafeGitStateError(fmt.Errorf("unauthorized Git state mutation; restoration incomplete: pre-restoration hook: %w", err))
		}
	}
	if err := w.verifyRestorationBinding(before, after); err != nil {
		return unsafeGitStateError(fmt.Errorf("unauthorized Git state mutation; restoration incomplete: Git control binding changed before restoration: %w", err))
	}
	preRestore, err := w.snapshotGitState(ctx, true)
	if err != nil || !gitStateControlBound(before, preRestore) {
		return unsafeGitStateError(errors.New("unauthorized Git state mutation; restoration incomplete: bound pre-restoration snapshot failed"))
	}
	if !observedGitStateEqual(after, preRestore) {
		return unsafeGitStateError(errors.New("unauthorized Git state mutation; restoration incomplete: concurrent shared Git state change preserved"))
	}
	if err := w.verifyRestorationBinding(before, preRestore); err != nil {
		return unsafeGitStateError(fmt.Errorf("unauthorized Git state mutation; restoration incomplete: Git control binding changed at restoration: %w", err))
	}
	restoration := w.restoreGitState(ctx, before, after)
	restoreErrors := restoration.failures
	if hooks.afterRestore != nil {
		if err := hooks.afterRestore(); err != nil {
			restoreErrors = append(restoreErrors, "post-restoration hook failed")
		}
	}
	final, finalErr := w.snapshotGitState(ctx, true)
	if finalErr != nil || !gitStateControlBound(before, final) {
		restoreErrors = append(restoreErrors, "bound final snapshot failed")
	} else {
		if !w.sharedGitStateEqual(after, final, restoration) {
			restoreErrors = append(restoreErrors, "shared Git state changed during restoration")
		}
		ownedMatches := w.ownedGitStateEqual(before, final, restoration)
		if !ownedMatches {
			restoreErrors = append(restoreErrors, "owned Git state does not match baseline")
		} else if !indexStateChanged(before, after) || after.indexInfo != nil && final.indexInfo != nil && !os.SameFile(after.indexInfo, final.indexInfo) {
			w.indexInfo = final.indexInfo
			w.indexBytes = append(w.indexBytes[:0], final.indexBytes...)
		}
	}
	message := "unauthorized Git state mutation: " + strings.Join(violations, ", ")
	if len(restoreErrors) != 0 {
		message += "; restoration incomplete: " + strings.Join(restoreErrors, "; ")
	}
	diagnostic := errors.New(message)
	if len(restoreErrors) == 0 {
		return restoredGitStateError(diagnostic)
	}
	return unsafeGitStateError(diagnostic)
}

func (w *Workspace) verifyRestorationBinding(before, observed GitState) error {
	rootInfo, err := noFollowDirectoryInfo(w.path)
	if err != nil || before.rootInfo == nil || !os.SameFile(before.rootInfo, rootInfo) {
		return errors.New("workspace root identity changed")
	}
	currentDotGit, err := snapshotExactPath(filepath.Join(w.path, ".git"), gitOutputLimit)
	if err != nil || !exactFileStableEqual(before.dotGit, currentDotGit) {
		return errors.New(".git indirection changed")
	}
	gitDir, err := linkedGitDir(w.path)
	if err != nil || gitDir != before.gitDir {
		return errors.New("linked Git directory changed")
	}
	commonDir, err := linkedCommonDir(gitDir)
	if err != nil || commonDir != before.commonDir {
		return errors.New("common Git directory changed")
	}
	gitDirInfo, gitDirErr := noFollowDirectoryInfo(gitDir)
	commonInfo, commonErr := noFollowDirectoryInfo(commonDir)
	indexInfo, indexErr := os.Stat(before.indexPath)
	if gitDirErr != nil || commonErr != nil || indexErr != nil || !os.SameFile(before.gitDirInfo, gitDirInfo) ||
		!os.SameFile(before.commonInfo, commonInfo) || !os.SameFile(observed.indexInfo, indexInfo) {
		return errors.New("Git control object identity changed")
	}
	return nil
}

func (w *Workspace) gitStateViolations(before, after GitState) []string {
	var violations []string
	if before.head != after.head || before.symbolicHead != after.symbolicHead {
		violations = append(violations, "workspace HEAD")
	}
	if indexStateChanged(before, after) {
		violations = append(violations, "index")
	}
	if before.runRef != after.runRef {
		violations = append(violations, "run ref")
	}
	if !w.sharedWorktreesEqual(before.worktrees, after.worktrees) {
		violations = append(violations, "worktrees")
	}
	if !bytes.Equal(before.configGraph, after.configGraph) || !exactFileStableMapsEqual(before.configs, after.configs) {
		violations = append(violations, "config")
	}
	if !exactFileStableMapsEqual(before.pseudorefs, after.pseudorefs) {
		violations = append(violations, "pseudorefs")
	}
	if !exactFileStableMapsEqual(before.rawHooks, after.rawHooks) {
		violations = append(violations, "hooks")
	}
	if !rawRefMapsEqualForTransition(before.rawRefs, after.rawRefs, w.rawRunRefKey(), before.runRef != after.runRef) {
		violations = append(violations, "raw refs")
	}
	if !exactFileStableMapsEqual(before.rawControl, after.rawControl) {
		violations = append(violations, "raw control")
	}
	newRefs, deletedRefs, movedRefs := refChanges(before.refs, after.refs)
	if len(newRefs)+len(deletedRefs) != 0 {
		violations = append(violations, "refs")
	}
	if len(movedRefs) != 0 {
		violations = append(violations, "moved refs")
	}
	return violations
}

func (w *Workspace) restoreGitState(ctx context.Context, before, observed GitState) gitRestoration {
	var restoration gitRestoration
	failures := restoration.failures
	if !w.sharedWorktreesEqual(before.worktrees, observed.worktrees) {
		failures = append(failures, "unproven worktree registration changes preserved")
	}
	if !bytes.Equal(before.configGraph, observed.configGraph) || !exactFileStableMapsEqual(before.configs, observed.configs) {
		failures = append(failures, "repository config changes preserved")
	}
	if !exactFileStableMapsEqual(before.pseudorefs, observed.pseudorefs) {
		failures = append(failures, "pseudoref changes preserved")
	}
	if !exactFileStableMapsEqual(before.rawHooks, observed.rawHooks) {
		failures = append(failures, "hook storage changes preserved")
	}
	if !rawRefMapsEqualForTransition(before.rawRefs, observed.rawRefs, w.rawRunRefKey(), before.runRef != observed.runRef) {
		failures = append(failures, "raw ref storage changes preserved")
	}
	if !exactFileStableMapsEqual(before.rawControl, observed.rawControl) {
		failures = append(failures, "raw control file changes preserved")
	}
	if before.symbolicHead != observed.symbolicHead {
		failures = append(failures, "workspace symbolic HEAD cannot be safely restored")
	}
	newRefs, deletedRefs, movedRefs := refChanges(before.refs, observed.refs)
	runRefName := "refs/heads/" + w.branch
	if len(newRefs) != 0 {
		failures = append(failures, "unproven new refs preserved")
	}
	if containsRefOtherThan(deletedRefs, runRefName) {
		failures = append(failures, "deleted operator refs preserved")
	}
	if containsRefOtherThan(movedRefs, runRefName) {
		failures = append(failures, "moved operator refs preserved")
	}

	if before.runRef != observed.runRef {
		if before.runRef.symbolic != "" || observed.runRef.symbolic != "" {
			failures = append(failures, "symbolic owned run ref preserved")
			restoration.failures = failures
			return restoration
		}
		baselineRaw := before.rawRefs[w.rawRunRefKey()]
		observedRaw, observedRawExists := observed.rawRefs[w.rawRunRefKey()]
		observedStorageSafe := observed.runRef.object == "" && !observedRawExists ||
			rawDirectRunRefMatches(observedRaw, observed.runRef.object) && observedRaw.mode == baselineRaw.mode
		if !rawDirectRunRefMatches(baselineRaw, before.runRef.object) || !observedStorageSafe {
			failures = append(failures, "non-regular owned run ref storage preserved")
			restoration.failures = failures
			return restoration
		}
		expected := observed.runRef.object
		if expected == "" {
			expected = strings.Repeat("0", len(before.runRef.object))
		}
		if _, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
			"--git-dir="+before.commonDir, "-c", "core.hooksPath="+os.DevNull,
			"update-ref", "--no-deref", runRefName, before.runRef.object, expected); err != nil {
			failures = append(failures, "restore run ref by compare-and-swap")
		} else {
			installedRefs, snapshotErr := snapshotRawTrees([]rawTreeRoot{{name: "common", path: filepath.Join(before.commonDir, "refs")}}, defaultRawControlLimits)
			installed, installedExists := installedRefs[w.rawRunRefKey()]
			if snapshotErr != nil || !installedExists || !rawDirectRunRefMatches(installed, before.runRef.object) || installed.mode != baselineRaw.mode {
				failures = append(failures, "bind restored run ref storage")
			} else {
				restoration.runRefFile = installed
				restoration.runRefRaw = runRefStorageProof(installedRefs, w.rawRunRefKey())
			}
		}
	}

	if indexStateChanged(before, observed) {
		if before.indexPath != observed.indexPath {
			failures = append(failures, "workspace index path changed")
		} else if !indexContentChanged(before, observed) {
			failures = append(failures, "workspace index identity or metadata changes preserved")
		} else if before.indexInfo == nil || observed.indexInfo == nil || before.indexInfo.Mode() != observed.indexInfo.Mode() {
			failures = append(failures, "workspace index metadata changes preserved")
		} else if installed, err := replaceIndexCASBound(before.indexPath, observed.indexBytes, observed.indexInfo, before.indexBytes, indexCASHooks{}); err != nil {
			failures = append(failures, "restore index by compare-and-swap: "+err.Error())
		} else {
			restoration.indexInfo = installed
		}
	}
	restoration.failures = failures
	return restoration
}

func rawDirectRunRefMatches(file exactFile, object string) bool {
	return file.exists && file.mode.IsRegular() && file.mode&os.ModeSymlink == 0 &&
		string(file.bytes) == object+"\n" && validObjectID(object)
}

func gitStateControlBound(expected, current GitState) bool {
	return expected.rootInfo != nil && current.rootInfo != nil && os.SameFile(expected.rootInfo, current.rootInfo) &&
		exactFileStableEqual(expected.dotGit, current.dotGit) &&
		expected.gitDir == current.gitDir && expected.commonDir == current.commonDir && expected.indexPath == current.indexPath &&
		expected.gitDirInfo != nil && current.gitDirInfo != nil && os.SameFile(expected.gitDirInfo, current.gitDirInfo) &&
		expected.commonInfo != nil && current.commonInfo != nil && os.SameFile(expected.commonInfo, current.commonInfo)
}

func indexStateChanged(left, right GitState) bool {
	return !indexStateExactEqual(left, right)
}

func indexContentChanged(left, right GitState) bool {
	return left.indexTree != right.indexTree || !bytes.Equal(left.indexBytes, right.indexBytes)
}

func indexStateExactEqual(left, right GitState) bool {
	return left.indexPath == right.indexPath && left.indexTree == right.indexTree && bytes.Equal(left.indexBytes, right.indexBytes) &&
		left.indexInfo != nil && right.indexInfo != nil && stableFileInfo(left.indexInfo, right.indexInfo)
}

func gitStateControlExactEqual(left, right GitState) bool {
	return left.owner == right.owner && gitStateControlBound(left, right)
}

// observedGitStateEqual compares two snapshots taken before Togi has performed
// any restoration. No owned-state exception is valid in this window.
func observedGitStateEqual(left, right GitState) bool {
	return gitStateControlExactEqual(left, right) &&
		left.head == right.head && left.symbolicHead == right.symbolicHead &&
		left.runRef == right.runRef && indexStateExactEqual(left, right) &&
		worktreeStatesEqual(left.worktrees, right.worktrees) &&
		bytes.Equal(left.configGraph, right.configGraph) && exactFileStableMapsEqual(left.configs, right.configs) &&
		exactFileStableMapsEqual(left.pseudorefs, right.pseudorefs) &&
		exactFileStableMapsEqual(left.rawHooks, right.rawHooks) &&
		exactFileStableMapsEqual(left.rawRefs, right.rawRefs) &&
		exactFileStableMapsEqual(left.rawControl, right.rawControl) &&
		refMapsEqualExcept(left.refs, right.refs, "")
}

func (w *Workspace) sharedGitStateEqual(left, right GitState, restoration gitRestoration) bool {
	allowRunRefStorageReplacement := restoration.runRefFile.exists
	allowIndexReplacement := restoration.indexInfo != nil
	rawRefsEqual := exactFileStableMapsEqual(left.rawRefs, right.rawRefs)
	if allowRunRefStorageReplacement {
		rawRefsEqual = rawRefMapsEqualForTransition(left.rawRefs, right.rawRefs, w.rawRunRefKey(), true)
	}
	worktreesEqual := worktreeStatesEqual(left.worktrees, right.worktrees)
	if allowRunRefStorageReplacement {
		worktreesEqual = w.sharedWorktreesEqual(left.worktrees, right.worktrees)
	}
	if !gitStateControlExactEqual(left, right) ||
		!bytes.Equal(left.configGraph, right.configGraph) || !exactFileStableMapsEqual(left.configs, right.configs) ||
		!exactFileStableMapsEqual(left.pseudorefs, right.pseudorefs) || !worktreesEqual ||
		!exactFileStableMapsEqual(left.rawHooks, right.rawHooks) ||
		!rawRefsEqual ||
		!exactFileStableMapsEqual(left.rawControl, right.rawControl) {
		return false
	}
	if !allowIndexReplacement && !indexStateExactEqual(left, right) {
		return false
	}
	runRefName := "refs/heads/" + w.branch
	if allowRunRefStorageReplacement {
		return refMapsEqualExcept(left.refs, right.refs, runRefName)
	}
	return refMapsEqualExcept(left.refs, right.refs, "")
}

func (w *Workspace) rawRunRefKey() string {
	return "common:" + filepath.ToSlash(filepath.Join("heads", w.branch))
}

func rawRefMapsEqualExcept(left, right map[string]exactFile, excluded string) bool {
	for name, leftFile := range left {
		if name == excluded {
			continue
		}
		rightFile, exists := right[name]
		if !exists || !exactFileStableEqual(leftFile, rightFile) {
			return false
		}
	}
	for name := range right {
		if name == excluded {
			continue
		}
		if _, exists := left[name]; !exists {
			return false
		}
	}
	return true
}

func rawRefMapsEqualForTransition(left, right map[string]exactFile, runRef string, logicalRunRefChanged bool) bool {
	if !logicalRunRefChanged {
		return exactFileStableMapsEqual(left, right)
	}
	for name, leftFile := range left {
		if name == runRef {
			continue
		}
		rightFile, exists := right[name]
		if !exists {
			return false
		}
		switch {
		case isRunRefStorageAncestor(name, runRef):
			if !exactFileIdentityEqual(leftFile, rightFile) {
				return false
			}
		case !exactFileStableEqual(leftFile, rightFile):
			return false
		}
	}
	for name := range right {
		if name == runRef {
			continue
		}
		if _, exists := left[name]; !exists {
			return false
		}
	}
	return true
}

func isRunRefStorageAncestor(name, runRef string) bool {
	namespaceEnd := strings.IndexByte(runRef, ':')
	if namespaceEnd < 0 || !strings.HasPrefix(name, runRef[:namespaceEnd+1]) {
		return false
	}
	return strings.HasPrefix(runRef, name+"/")
}

func runRefStorageProof(files map[string]exactFile, runRef string) map[string]exactFile {
	proof := make(map[string]exactFile)
	for name, file := range files {
		if name == runRef || isRunRefStorageAncestor(name, runRef) {
			proof[name] = file
		}
	}
	return proof
}

func runRefStorageProofMatches(proof, current map[string]exactFile) bool {
	if len(proof) == 0 {
		return false
	}
	for name, expected := range proof {
		observed, exists := current[name]
		if !exists || !exactFileStableEqual(expected, observed) {
			return false
		}
	}
	return true
}

func (w *Workspace) ownedGitStateEqual(baseline, current GitState, restoration gitRestoration) bool {
	baselineRaw, baselineExists := baseline.rawRefs[w.rawRunRefKey()]
	currentRaw, currentExists := current.rawRefs[w.rawRunRefKey()]
	rawEqual := baselineExists == currentExists && (!baselineExists || exactFileStableEqual(baselineRaw, currentRaw))
	if restoration.runRefFile.exists {
		rawEqual = baselineExists == currentExists && (!baselineExists || exactFileEqual(baselineRaw, currentRaw)) &&
			currentExists && exactFileStableEqual(restoration.runRefFile, currentRaw) &&
			runRefStorageProofMatches(restoration.runRefRaw, current.rawRefs)
	}
	indexEqual := indexStateExactEqual(baseline, current)
	if restoration.indexInfo != nil {
		indexEqual = baseline.indexPath == current.indexPath && baseline.indexTree == current.indexTree &&
			bytes.Equal(baseline.indexBytes, current.indexBytes) && baseline.indexInfo != nil && current.indexInfo != nil &&
			baseline.indexInfo.Mode() == current.indexInfo.Mode() && stableFileInfo(restoration.indexInfo, current.indexInfo)
	}
	return baseline.head == current.head && baseline.symbolicHead == current.symbolicHead &&
		baseline.runRef == current.runRef && indexEqual && rawEqual
}

func refMapsEqualExcept(left, right map[string]refState, excluded string) bool {
	for name, state := range left {
		if name == excluded {
			continue
		}
		if other, exists := right[name]; !exists || state != other {
			return false
		}
	}
	for name := range right {
		if name == excluded {
			continue
		}
		if _, exists := left[name]; !exists {
			return false
		}
	}
	return true
}

func worktreeStatesEqual(left, right []worktreeState) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (w *Workspace) sharedWorktreesEqual(left, right []worktreeState) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		leftState, rightState := left[index], right[index]
		if leftState.path == w.path && rightState.path == w.path {
			leftState.head = ""
			rightState.head = ""
		}
		if leftState != rightState {
			return false
		}
	}
	return true
}

func (w *Workspace) snapshotIndex(ctx context.Context, gitDir, commonDir string) (string, string, []byte, os.FileInfo, error) {
	indexPath, err := gitPath(ctx, w.path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("resolve workspace index: %w", err)
	}
	indexPath, err = canonicalExisting(indexPath)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("canonicalize workspace index: %w", err)
	}
	if !pathWithin(indexPath, gitDir) && !pathWithin(indexPath, commonDir) {
		return "", "", nil, nil, errors.New("workspace index resolves outside Git control directories")
	}
	for range 3 {
		before, readErr := snapshotExactPath(indexPath, indexByteLimit)
		if readErr != nil || !before.exists || !before.mode.IsRegular() {
			return "", "", nil, nil, fmt.Errorf("read workspace index: %w", readErr)
		}
		tree, treeErr := gitPath(ctx, w.path, "write-tree")
		if treeErr != nil {
			return "", "", nil, nil, fmt.Errorf("snapshot index tree: %w", treeErr)
		}
		after, readErr := snapshotExactPath(indexPath, indexByteLimit)
		if readErr != nil || !after.exists || !after.mode.IsRegular() {
			return "", "", nil, nil, fmt.Errorf("reread workspace index: %w", readErr)
		}
		if exactFileStableEqual(before, after) {
			return indexPath, tree, after.bytes, after.info, nil
		}
	}
	return "", "", nil, nil, errors.New("workspace index changed while taking Git state snapshot")
}

func (w *Workspace) snapshotGitFiles(ctx context.Context, names []string, gitDir, commonDir string) (map[string]exactFile, error) {
	before, err := w.snapshotGitFilesOnce(ctx, names, gitDir, commonDir)
	if err != nil {
		return nil, err
	}
	after, err := w.snapshotGitFilesOnce(ctx, names, gitDir, commonDir)
	if err != nil || !exactFileStableMapsEqual(before, after) {
		return nil, errors.New("Git control files changed while snapshotting")
	}
	return after, nil
}

func (w *Workspace) snapshotGitFilesOnce(ctx context.Context, names []string, gitDir, commonDir string) (map[string]exactFile, error) {
	files := make(map[string]exactFile, len(names))
	for _, name := range names {
		path, err := gitPath(ctx, w.path, "rev-parse", "--path-format=absolute", "--git-path", name)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", name, err)
		}
		path, err = canonicalCandidate(path)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s: %w", name, err)
		}
		if !pathWithin(path, gitDir) && !pathWithin(path, commonDir) {
			return nil, fmt.Errorf("%s resolves outside Git control directories", name)
		}
		file, err := snapshotExactPath(path, gitOutputLimit)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		files[name] = file
	}
	return files, nil
}

func (w *Workspace) snapshotLocalConfig(ctx context.Context, gitDir, commonDir string) ([]byte, map[string]exactFile, error) {
	base, err := w.snapshotGitFiles(ctx, []string{"config", "config.worktree"}, gitDir, commonDir)
	if err != nil {
		return nil, nil, err
	}
	localGraph, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit,
		"config", "--local", "--includes", "--show-origin", "--null", "--list")
	if err != nil {
		return nil, nil, err
	}
	origins, err := parseConfigOrigins(localGraph)
	if err != nil {
		return nil, nil, err
	}
	worktreeEnabled, err := configGraphEnablesWorktreeConfig(localGraph)
	if err != nil {
		return nil, nil, err
	}
	var worktreeGraph []byte
	if worktreeEnabled && base["config.worktree"].exists {
		worktreeGraph, err = gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit,
			"config", "--worktree", "--includes", "--show-origin", "--null", "--list")
		if err != nil {
			return nil, nil, err
		}
		worktreeOrigins, err := parseConfigOrigins(worktreeGraph)
		if err != nil {
			return nil, nil, err
		}
		origins = mergeConfigOrigins(origins, worktreeOrigins)
	}
	activeOrigins := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		activeOrigins[filepath.Clean(origin.path)] = struct{}{}
		canonical, err := canonicalCandidate(origin.path)
		if err != nil {
			return nil, nil, err
		}
		activeOrigins[canonical] = struct{}{}
	}
	roots := []string{base["config"].path}
	if worktreeEnabled {
		roots = append(roots, base["config.worktree"].path)
	}
	private, err := w.createPrivateTempDir()
	if err != nil {
		return nil, nil, errors.New("create private config snapshot state")
	}
	defer private.close()
	includeGraph, err := validateContextConfigIncludeGraph(ctx, w.path, private, roots, activeOrigins, defaultConfigIncludeBudget)
	if err != nil {
		return nil, nil, errors.New("local config include graph exceeds safety bounds")
	}
	graph := frameConfigGraphs(localGraph, worktreeGraph)
	files := make(map[string]exactFile, len(includeGraph)+len(origins)+1)
	entries, pathBytes := len(includeGraph), 0
	contents := int64(0)
	for path, file := range includeGraph {
		files["graph:"+path] = file
		pathBytes += len(path)
		contents += int64(len(file.bytes))
	}
	if !worktreeEnabled {
		files["inactive-root:config.worktree"] = base["config.worktree"]
	}
	for _, origin := range origins {
		canonical, err := canonicalCandidate(origin.path)
		if err != nil {
			return nil, nil, err
		}
		graphFile, covered := includeGraph[canonical]
		if !covered {
			return nil, nil, errors.New("active local config origin is outside validated include graph")
		}
		raw, err := snapshotExactPath(origin.path, defaultConfigIncludeBudget.perFile)
		if err != nil {
			return nil, nil, err
		}
		if origin.required && !raw.exists {
			return nil, nil, errors.New("local config origin is unavailable")
		}
		if raw.exists && !raw.mode.IsRegular() && raw.mode&os.ModeSymlink == 0 {
			return nil, nil, errors.New("local config origin has unsupported mode")
		}
		if raw.mode&os.ModeSymlink != 0 {
			if entries >= defaultConfigIncludeBudget.maxEntries || len(origin.path) > defaultConfigIncludeBudget.maxPathBytes-pathBytes ||
				int64(len(raw.bytes)) > defaultConfigIncludeBudget.maxContents-contents {
				return nil, nil, errors.New("local config symlink origins exceed aggregate budget")
			}
			entries++
			pathBytes += len(origin.path)
			contents += int64(len(raw.bytes))
			files["origin:"+origin.path] = raw
		} else if !exactFileStableEqual(graphFile, raw) {
			return nil, nil, errors.New("local config origin differs from validated include graph")
		}
		rawAfter, err := snapshotExactPath(origin.path, gitOutputLimit)
		if err != nil || !exactFileStableEqual(raw, rawAfter) {
			return nil, nil, errors.New("local config origin changed while snapshotting")
		}
		targetAfter, err := snapshotExactPath(canonical, defaultConfigIncludeBudget.perFile)
		if err != nil || !exactFileStableEqual(graphFile, targetAfter) {
			return nil, nil, errors.New("local config target changed while snapshotting")
		}
	}
	baseAfter, err := w.snapshotGitFiles(ctx, []string{"config", "config.worktree"}, gitDir, commonDir)
	if err != nil || !exactFileStableMapsEqual(base, baseAfter) {
		return nil, nil, errors.New("local config roots changed while snapshotting")
	}
	return append([]byte(nil), graph...), files, nil
}

func validateConfigIncludeGraph(ctx context.Context, dir string, roots []string, budget configIncludeBudget) (map[string]exactFile, error) {
	tempRoot := filepath.Dir(dir)
	rootInfo, err := os.Lstat(tempRoot)
	if err != nil {
		return nil, err
	}
	private, err := openPrivateTempDir(tempRoot, rootInfo)
	if err != nil {
		return nil, err
	}
	defer private.close()
	return validateContextConfigIncludeGraph(ctx, dir, private, roots, nil, budget)
}

func validateContextConfigIncludeGraph(ctx context.Context, dir string, private *privateTempDir, roots []string, activeOrigins map[string]struct{}, budget configIncludeBudget) (map[string]exactFile, error) {
	return validateContextConfigIncludeGraphWithHooks(ctx, dir, private, roots, activeOrigins, budget, configGraphHooks{})
}

func validateContextConfigIncludeGraphWithHooks(ctx context.Context, dir string, private *privateTempDir, roots []string, activeOrigins map[string]struct{}, budget configIncludeBudget, hooks configGraphHooks) (map[string]exactFile, error) {
	if budget.maxEntries <= 0 || budget.maxPathBytes <= 0 || budget.maxDepth <= 0 || budget.maxContents <= 0 || budget.perFile <= 0 {
		return nil, errors.New("invalid config include budget")
	}
	files := make(map[string]exactFile)
	visiting := make(map[string]bool)
	entries, pathBytes := 0, 0
	contents := int64(0)
	conditionFile, conditionPath, err := private.createFile("condition")
	if err != nil {
		return nil, errors.New("create config condition probe")
	}
	probeInfo, err := conditionFile.Stat()
	if err != nil {
		_ = conditionFile.Close()
		return nil, errors.New("bind config condition probe")
	}
	defer private.removeFile(filepath.Base(conditionPath), probeInfo)
	probeID := filepath.Base(conditionPath)
	probeKey := "togi-probe." + probeID + ".active"
	probeValue := "active-" + probeID
	probeContents := "[togi-probe \"" + probeID + "\"]\nactive = " + probeValue + "\n"
	if _, err := conditionFile.WriteString(probeContents); err != nil || conditionFile.Close() != nil {
		_ = conditionFile.Close()
		return nil, errors.New("write config condition probe")
	}
	probe, err := private.snapshotFile(filepath.Base(conditionPath), int64(len(probeContents)))
	if err != nil || !probe.exists || !probe.mode.IsRegular() {
		return nil, errors.New("bind config condition probe")
	}
	conditionCache := make(map[string]bool)
	maxConditions := budget.maxConditions
	if maxConditions <= 0 {
		maxConditions = budget.maxEntries
	}
	maxConditionBytes := budget.maxConditionBytes
	if maxConditionBytes <= 0 {
		maxConditionBytes = budget.maxPathBytes
	}
	conditionBytes := 0
	var visit func(string, int) error
	visit = func(path string, depth int) error {
		if depth > budget.maxDepth {
			return errors.New("config include depth exceeds budget")
		}
		if len(path) > budget.maxPathBytes-pathBytes {
			return errors.New("config include path bytes exceed budget")
		}
		canonical, err := canonicalCandidate(path)
		if err != nil {
			return errors.New("resolve config include graph node")
		}
		if visiting[canonical] {
			return errors.New("config include graph contains a cycle")
		}
		if _, seen := files[canonical]; seen {
			return nil
		}
		if entries >= budget.maxEntries || len(canonical) > budget.maxPathBytes-pathBytes {
			return errors.New("config include graph size exceeds budget")
		}
		entries++
		pathBytes += len(canonical)
		remaining := budget.maxContents - contents
		if remaining < 0 {
			return errors.New("config include contents exceed budget")
		}
		limit := budget.perFile
		if remaining < limit {
			limit = remaining
		}
		file, err := snapshotExactPath(canonical, limit)
		if err != nil {
			return errors.New("read config include graph node")
		}
		files[canonical] = file
		if !file.exists {
			if hooks.afterVisit != nil {
				return hooks.afterVisit(canonical)
			}
			return nil
		}
		if !file.mode.IsRegular() {
			return errors.New("config include graph node has unsupported mode")
		}
		contents += int64(len(file.bytes))
		visiting[canonical] = true
		output, err := gitcmd.Output(ctx, dir, gitcmd.Hermetic, gitOutputLimit,
			"config", "--file="+canonical, "--no-includes", "--null", "--list")
		if err != nil {
			delete(visiting, canonical)
			return errors.New("parse config include graph node")
		}
		err = walkConfigIncludeTargets(output, filepath.Dir(canonical), func(included, condition, conditionOrigin string) error {
			if condition != "" && activeOrigins != nil {
				_, emitted := activeOrigins[filepath.Clean(included)]
				if !emitted {
					cacheKey := conditionOrigin + "\x00" + condition
					active, known := conditionCache[cacheKey]
					if !known {
						if len(conditionCache) >= maxConditions || len(cacheKey) > maxConditionBytes-conditionBytes {
							return errors.New("config include conditions exceed budget")
						}
						conditionBytes += len(cacheKey)
						var conditionErr error
						active, conditionErr = gitConfigConditionActive(ctx, dir, condition, conditionOrigin, private, filepath.Base(conditionPath), conditionPath, probeKey, probeValue, probe)
						if conditionErr != nil {
							return conditionErr
						}
						conditionCache[cacheKey] = active
					}
					if !active {
						return nil
					}
				}
			}
			return visit(included, depth+1)
		})
		if err != nil {
			delete(visiting, canonical)
			return err
		}
		delete(visiting, canonical)
		after, err := snapshotExactPath(canonical, budget.perFile)
		if err != nil || !exactFileStableEqual(file, after) {
			return errors.New("config include graph changed while validating")
		}
		if hooks.afterVisit != nil {
			if err := hooks.afterVisit(canonical); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := visit(root, 1); err != nil {
			return nil, err
		}
	}
	recheckEntries, recheckPathBytes := 0, 0
	recheckContents := int64(0)
	for canonical, file := range files {
		if recheckEntries >= budget.maxEntries || len(canonical) > budget.maxPathBytes-recheckPathBytes ||
			int64(len(file.bytes)) > budget.maxContents-recheckContents {
			return nil, errors.New("config include graph recheck exceeds aggregate budget")
		}
		recheckEntries++
		recheckPathBytes += len(canonical)
		recheckContents += int64(len(file.bytes))
		after, err := snapshotExactPath(canonical, int64(len(file.bytes)))
		if err != nil || !exactFileStableEqual(file, after) {
			return nil, errors.New("config include graph changed after traversal")
		}
	}
	return files, nil
}

func walkConfigIncludeTargets(output []byte, originDir string, visit func(string, string, string) error) error {
	if len(output) == 0 {
		return nil
	}
	if output[len(output)-1] != 0 {
		return errors.New("malformed config include graph output")
	}
	remaining := output[:len(output)-1]
	for {
		end := bytes.IndexByte(remaining, 0)
		var entry []byte
		if end < 0 {
			entry = remaining
		} else {
			entry = remaining[:end]
		}
		key, value, found := bytes.Cut(entry, []byte{'\n'})
		if !found {
			return errors.New("malformed config include graph entry")
		}
		keyText := strings.ToLower(string(key))
		conditional := strings.HasPrefix(keyText, "includeif.") && strings.HasSuffix(keyText, ".path")
		if keyText == "include.path" || conditional {
			path, err := resolveConfigIncludePath(originDir, string(value))
			if err != nil {
				return errors.New("resolve config include graph target")
			}
			condition := ""
			if conditional {
				keyString := string(key)
				condition = keyString[len("includeif.") : len(keyString)-len(".path")]
			}
			if err := visit(path, condition, originDir); err != nil {
				return err
			}
		}
		if end < 0 {
			break
		}
		remaining = remaining[end+1:]
	}
	return nil
}

func gitConfigConditionActive(ctx context.Context, dir, condition, originDir string, private *privateTempDir, probeName, probePath, probeKey, probeValue string, probe exactFile) (bool, error) {
	lower := strings.ToLower(condition)
	if strings.HasPrefix(lower, "gitdir:./") || strings.HasPrefix(lower, "gitdir/i:./") {
		colon := strings.IndexByte(condition, ':')
		trailingSlash := strings.HasSuffix(condition, "/")
		condition = condition[:colon+1] + filepath.ToSlash(filepath.Join(originDir, condition[colon+3:]))
		if trailingSlash && !strings.HasSuffix(condition, "/") {
			condition += "/"
		}
	}
	key := "includeIf." + condition + ".path"
	if err := private.validatePathBinding(); err != nil {
		return false, errors.New("config condition private path changed")
	}
	output, err := gitcmd.OutputWithConfig(ctx, dir, gitcmd.Hermetic, 512, key, probePath,
		"config", "--includes", "--show-origin", "--null", "--get", "--default=inactive", probeKey)
	if err != nil {
		return false, errors.New("evaluate local config include condition")
	}
	if err := private.validatePathBinding(); err != nil {
		return false, errors.New("config condition private path changed")
	}
	currentProbe, err := private.snapshotFile(probeName, int64(len(probe.bytes)))
	if err != nil || !exactFileStableEqual(probe, currentProbe) {
		return false, errors.New("config condition probe identity changed")
	}
	if len(output) == 0 || output[len(output)-1] != 0 {
		return false, errors.New("unexpected local config include condition result")
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	if len(fields) != 2 {
		return false, errors.New("unexpected local config include condition result")
	}
	origin, value := string(fields[0]), string(fields[1])
	if value == "inactive" && origin == "command line:" {
		return false, nil
	}
	if value != probeValue || !strings.HasPrefix(origin, "file:") {
		return false, errors.New("unexpected local config include condition result")
	}
	originPath, err := canonicalExisting(strings.TrimPrefix(origin, "file:"))
	probeCanonical, probeErr := canonicalExisting(probePath)
	if err != nil || probeErr != nil || originPath != probeCanonical {
		return false, errors.New("local config include condition has foreign origin")
	}
	return true, nil
}

func removeSameFile(path string, owned os.FileInfo) {
	if current, err := os.Lstat(path); err == nil && owned != nil && os.SameFile(owned, current) {
		_ = os.Remove(path)
	}
}

func (w *Workspace) createPrivateTempDir() (*privateTempDir, error) {
	if err := w.validateStaticControlBinding(); err != nil {
		return nil, err
	}
	return openPrivateTempDir(w.cacheRoot, w.cacheRootInfo)
}

func openPrivateTempDir(path string, expected os.FileInfo) (*privateTempDir, error) {
	return openPrivateTempDirWithHooks(path, expected, privateTempHooks{})
}

func openPrivateTempDirWithHooks(path string, expected os.FileInfo, hooks privateTempHooks) (*privateTempDir, error) {
	current, err := os.Lstat(path)
	if err != nil || expected == nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		return nil, errors.New("private state root identity changed")
	}
	parent, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			_ = parent.Close()
			return nil, err
		}
	}
	bound, err := parent.Stat(".")
	namedParent, namedErr := os.Lstat(path)
	if err != nil || namedErr != nil || !namedParent.IsDir() || namedParent.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(expected, bound) || !os.SameFile(expected, namedParent) {
		_ = parent.Close()
		return nil, errors.New("private state root binding changed")
	}
	name, err := randomPrivateName(".togi-private")
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	if err := parent.Mkdir(name, 0o700); err != nil {
		_ = parent.Close()
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = parent.Close()
		return nil, errors.New("bind private state directory")
	}
	if hooks.afterDirectorySample != nil {
		if err := hooks.afterDirectorySample(name); err != nil {
			removeRootEntrySameFile(parent, name, info)
			_ = parent.Close()
			return nil, err
		}
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		removeRootEntrySameFile(parent, name, info)
		_ = parent.Close()
		return nil, err
	}
	boundDirectory, boundErr := root.Stat(".")
	namedDirectory, namedErr := parent.Lstat(name)
	if boundErr != nil || namedErr != nil || !namedDirectory.IsDir() || namedDirectory.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(info, boundDirectory) || !os.SameFile(info, namedDirectory) {
		_ = root.Close()
		removeRootEntrySameFile(parent, name, info)
		_ = parent.Close()
		return nil, errors.New("private state directory binding changed")
	}
	return &privateTempDir{
		path: filepath.Join(path, name), name: name, info: info,
		parentPath: path, parentInfo: expected, parent: parent, root: root,
	}, nil
}

func removeRootEntrySameFile(root *os.Root, name string, owned os.FileInfo) {
	if current, err := root.Lstat(name); err == nil && owned != nil && os.SameFile(owned, current) {
		_ = root.Remove(name)
	}
}

func (d *privateTempDir) createFile(prefix string) (*os.File, string, error) {
	name, err := randomPrivateName(prefix)
	if err != nil {
		return nil, "", err
	}
	file, err := d.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	return file, filepath.Join(d.path, name), nil
}

func (d *privateTempDir) removeFile(name string, owned os.FileInfo) {
	if current, err := d.root.Lstat(name); err == nil && owned != nil && os.SameFile(owned, current) {
		_ = d.root.Remove(name)
	}
}

func (d *privateTempDir) validatePathBinding() error {
	parent, err := os.Lstat(d.parentPath)
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || !os.SameFile(d.parentInfo, parent) {
		return errors.New("private state parent path changed")
	}
	directory, err := d.parent.Lstat(d.name)
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 || !os.SameFile(d.info, directory) {
		return errors.New("private state directory binding changed")
	}
	return nil
}

func (d *privateTempDir) snapshotFile(name string, limit int64) (exactFile, error) {
	file, err := d.root.Open(name)
	if err != nil {
		return exactFile{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return exactFile{}, errors.New("private state file is unavailable")
	}
	contents, err := readBounded(file, limit)
	if err != nil {
		return exactFile{}, err
	}
	pathInfo, err := d.root.Lstat(name)
	if err != nil || !os.SameFile(info, pathInfo) {
		return exactFile{}, errors.New("private state file binding changed")
	}
	return exactFile{path: filepath.Join(d.path, name), exists: true, mode: info.Mode(), bytes: contents, info: info}, nil
}

func (d *privateTempDir) close() {
	_ = d.root.Close()
	if current, err := d.parent.Lstat(d.name); err == nil && d.info != nil && os.SameFile(d.info, current) {
		_ = d.parent.Remove(d.name)
	}
	_ = d.parent.Close()
}

func (d *privateTempDir) discard() error {
	if d == nil || d.root == nil || d.parent == nil {
		return errors.New("private state directory is unavailable")
	}
	if err := d.validatePathBinding(); err != nil {
		return err
	}
	if err := makePrivateTreeWritable(d.root); err != nil {
		return errors.New("make private state removable")
	}
	directory, err := d.root.Open(".")
	if err != nil {
		return errors.New("open private state for removal")
	}
	entries, readErr := directory.ReadDir(-1)
	_ = directory.Close()
	if readErr != nil {
		return errors.New("read private state for removal")
	}
	for _, entry := range entries {
		if err := d.root.RemoveAll(entry.Name()); err != nil {
			return errors.New("remove private state contents")
		}
	}
	if d.beforeRemove != nil {
		if err := d.beforeRemove(); err != nil {
			return err
		}
	}
	current, err := d.parent.Lstat(d.name)
	if err != nil || d.info == nil || !os.SameFile(d.info, current) {
		return errors.New("private state directory binding changed before removal")
	}
	if err := d.parent.Remove(d.name); err != nil {
		return errors.New("remove private state directory")
	}
	_ = d.root.Close()
	d.root = nil
	_ = d.parent.Close()
	d.parent = nil
	return nil
}

func makePrivateTreeWritable(root *os.Root) error {
	if err := root.Chmod(".", 0o700); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	_ = directory.Close()
	if readErr != nil {
		return readErr
	}
	for _, entry := range entries {
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		child, err := root.OpenRoot(entry.Name())
		if err != nil {
			return err
		}
		err = makePrivateTreeWritable(child)
		_ = child.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func randomPrivateName(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(entropy[:]), nil
}

func configGraphEnablesWorktreeConfig(graph []byte) (bool, error) {
	if len(graph) == 0 || graph[len(graph)-1] != 0 {
		return false, errors.New("malformed local config graph")
	}
	fields := bytes.Split(graph[:len(graph)-1], []byte{0})
	if len(fields)%2 != 0 {
		return false, errors.New("malformed local config graph")
	}
	enabled := false
	for index := 1; index < len(fields); index += 2 {
		key, value, found := bytes.Cut(fields[index], []byte{'\n'})
		if !found || strings.ToLower(string(key)) != "extensions.worktreeconfig" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(string(value))) {
		case "", "true", "yes", "on", "1":
			enabled = true
		case "false", "no", "off", "0":
			enabled = false
		default:
			return false, errors.New("invalid worktreeConfig boolean")
		}
	}
	return enabled, nil
}

func mergeConfigOrigins(groups ...[]configOrigin) []configOrigin {
	merged := make(map[string]bool)
	for _, group := range groups {
		for _, origin := range group {
			merged[origin.path] = merged[origin.path] || origin.required
		}
	}
	result := make([]configOrigin, 0, len(merged))
	for path, required := range merged {
		result = append(result, configOrigin{path: path, required: required})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].path < result[right].path })
	return result
}

func frameConfigGraphs(local, worktree []byte) []byte {
	framed := fmt.Appendf(nil, "%d:", len(local))
	framed = append(framed, local...)
	framed = fmt.Appendf(framed, "%d:", len(worktree))
	return append(framed, worktree...)
}

func parseConfigOrigins(output []byte) ([]configOrigin, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("malformed local config origin stream")
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	if len(fields)%2 != 0 {
		return nil, errors.New("malformed local config origin pairs")
	}
	origins := make(map[string]bool)
	for index := 0; index < len(fields); index += 2 {
		origin := string(fields[index])
		entry := fields[index+1]
		if !strings.HasPrefix(origin, "file:") || len(origin) == len("file:") || len(entry) == 0 || !bytes.Contains(entry, []byte{'\n'}) {
			return nil, errors.New("unsupported local config origin")
		}
		path := strings.TrimPrefix(origin, "file:")
		if !filepath.IsAbs(path) {
			return nil, errors.New("non-absolute local config origin")
		}
		origins[filepath.Clean(path)] = true
	}
	result := make([]configOrigin, 0, len(origins))
	for path, required := range origins {
		result = append(result, configOrigin{path: path, required: required})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].path < result[right].path })
	return result, nil
}

func resolveConfigIncludePath(originDir, value string) (string, error) {
	if value == "" || strings.Contains(value, "%(prefix)") || strings.HasPrefix(value, "~") && !strings.HasPrefix(value, "~/") {
		return "", errors.New("unsupported local config include path")
	}
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	} else if !filepath.IsAbs(value) {
		value = filepath.Join(originDir, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func snapshotExactPath(path string, limit int64) (exactFile, error) {
	return snapshotExactPathWithHooks(path, limit, exactPathHooks{})
}

func snapshotExactPathWithHooks(path string, limit int64, hooks exactPathHooks) (exactFile, error) {
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if hooks.afterRead != nil {
			if err := hooks.afterRead(); err != nil {
				return exactFile{}, err
			}
		}
		if _, secondErr := os.Lstat(path); !errors.Is(secondErr, os.ErrNotExist) {
			return exactFile{}, errors.New("absent file binding changed while snapshotting")
		}
		return exactFile{path: path}, nil
	}
	if err != nil {
		return exactFile{}, err
	}
	file := exactFile{path: path, exists: true, mode: pathInfo.Mode(), info: pathInfo}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return exactFile{}, err
		}
		if len(target) > int(limit) {
			return exactFile{}, errors.New("symbolic link target exceeds capture limit")
		}
		if hooks.afterRead != nil {
			if err := hooks.afterRead(); err != nil {
				return exactFile{}, err
			}
		}
		after, statErr := os.Lstat(path)
		targetAfter, readErr := os.Readlink(path)
		if statErr != nil || readErr != nil || !stableFileInfo(pathInfo, after) || target != targetAfter {
			return exactFile{}, errors.New("symbolic link changed while snapshotting")
		}
		file.bytes = []byte(target)
		return file, nil
	}
	if !pathInfo.Mode().IsRegular() {
		if hooks.afterRead != nil {
			if err := hooks.afterRead(); err != nil {
				return exactFile{}, err
			}
		}
		after, statErr := os.Lstat(path)
		if statErr != nil || !stableFileInfo(pathInfo, after) {
			return exactFile{}, errors.New("file changed while snapshotting")
		}
		return file, nil
	}
	opened, err := os.Open(path)
	if err != nil {
		return exactFile{}, err
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil {
		return exactFile{}, err
	}
	currentPathInfo, err := os.Lstat(path)
	if err != nil || !stableFileInfo(pathInfo, openedInfo) || !stableFileInfo(openedInfo, currentPathInfo) {
		return exactFile{}, errors.New("file binding changed while opening")
	}
	contents, err := readBounded(opened, limit)
	if err != nil {
		return exactFile{}, err
	}
	if hooks.afterRead != nil {
		if err := hooks.afterRead(); err != nil {
			return exactFile{}, err
		}
	}
	afterOpened, statErr := opened.Stat()
	afterNamed, namedErr := os.Lstat(path)
	if statErr != nil || namedErr != nil || !stableFileInfo(openedInfo, afterOpened) || !stableFileInfo(openedInfo, afterNamed) {
		return exactFile{}, errors.New("file changed while reading")
	}
	file.mode = afterOpened.Mode()
	file.info = afterOpened
	file.bytes = contents
	return file, nil
}

func exactFileEqual(left, right exactFile) bool {
	return left.path == right.path && left.exists == right.exists && left.mode == right.mode && bytes.Equal(left.bytes, right.bytes)
}

func exactFileIdentityEqual(left, right exactFile) bool {
	if !exactFileEqual(left, right) {
		return false
	}
	if !left.exists {
		return true
	}
	return os.SameFile(left.info, right.info)
}

func exactFileMapsEqual(left, right map[string]exactFile) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftFile := range left {
		rightFile, exists := right[name]
		if !exists || !exactFileEqual(leftFile, rightFile) {
			return false
		}
	}
	return true
}

func exactFileIdentityMapsEqual(left, right map[string]exactFile) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftFile := range left {
		rightFile, exists := right[name]
		if !exists || !exactFileIdentityEqual(leftFile, rightFile) {
			return false
		}
	}
	return true
}

func exactFileStableEqual(left, right exactFile) bool {
	if !exactFileIdentityEqual(left, right) {
		return false
	}
	return !left.exists || stableFileInfo(left.info, right.info)
}

func exactFileStableMapsEqual(left, right map[string]exactFile) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftFile := range left {
		rightFile, exists := right[name]
		if !exists || !exactFileStableEqual(leftFile, rightFile) {
			return false
		}
	}
	return true
}

func replaceIndexCAS(path string, expected []byte, expectedInfo os.FileInfo, replacement []byte, hooks indexCASHooks) error {
	_, err := replaceIndexCASBound(path, expected, expectedInfo, replacement, hooks)
	return err
}

func replaceIndexCASBound(path string, expected []byte, expectedInfo os.FileInfo, replacement []byte, hooks indexCASHooks) (os.FileInfo, error) {
	parentPath := filepath.Dir(path)
	parentNamed, err := noFollowDirectoryInfo(parentPath)
	if err != nil {
		return nil, fmt.Errorf("bind index parent: %w", err)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open index parent: %w", err)
	}
	defer parent.Close()
	parentBound, boundErr := parent.Stat(".")
	parentNamedAgain, namedErr := noFollowDirectoryInfo(parentPath)
	if boundErr != nil || namedErr != nil || !os.SameFile(parentNamed, parentBound) || !os.SameFile(parentNamed, parentNamedAgain) {
		return nil, errors.New("index parent binding changed")
	}
	indexName := filepath.Base(path)
	lockName := indexName + ".lock"
	lockPath := path + ".lock"
	lock, err := parent.OpenFile(lockName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire index lock: %w", err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("stat acquired index lock: %w", err)
	}
	committed := false
	defer func() {
		_ = lock.Close()
		if !committed {
			if current, statErr := parent.Lstat(lockName); statErr == nil && os.SameFile(lockInfo, current) {
				_ = parent.Remove(lockName)
			}
		}
	}()

	index, err := parent.Open(indexName)
	if err != nil {
		return nil, fmt.Errorf("open locked index: %w", err)
	}
	defer index.Close()
	currentInfo, err := index.Stat()
	if err != nil || !stableFileInfo(expectedInfo, currentInfo) {
		return nil, errors.New("index identity no longer matches observed file")
	}
	current, err := readBounded(index, indexByteLimit)
	if err != nil {
		return nil, fmt.Errorf("read locked index: %w", err)
	}
	if !bytes.Equal(current, expected) {
		return nil, errors.New("index no longer matches observed bytes")
	}
	if err := lock.Chmod(currentInfo.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("preserve index mode: %w", err)
	}
	if _, err := lock.Write(replacement); err != nil {
		return nil, fmt.Errorf("write replacement index: %w", err)
	}
	if err := lock.Sync(); err != nil {
		return nil, fmt.Errorf("sync replacement index: %w", err)
	}
	writtenLockInfo, err := lock.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat replacement index: %w", err)
	}
	if hooks.beforeInstall != nil {
		if err := hooks.beforeInstall(lockPath); err != nil {
			return nil, fmt.Errorf("before index install: %w", err)
		}
	}
	currentPathInfo, err := parent.Lstat(indexName)
	if err != nil || !stableFileInfo(currentInfo, currentPathInfo) {
		return nil, errors.New("index binding changed before install")
	}
	currentLockInfo, err := parent.Lstat(lockName)
	if err != nil || !stableFileInfo(writtenLockInfo, currentLockInfo) {
		return nil, errors.New("index lock binding changed before install")
	}
	if err := lock.Close(); err != nil {
		return nil, fmt.Errorf("close replacement index: %w", err)
	}
	if err := parent.Rename(lockName, indexName); err != nil {
		return nil, fmt.Errorf("install replacement index: %w", err)
	}
	committed = true
	installed, err := parent.Open(indexName)
	if err != nil {
		return nil, fmt.Errorf("open installed index: %w", err)
	}
	defer installed.Close()
	installedInfo, err := installed.Stat()
	if err != nil || !os.SameFile(writtenLockInfo, installedInfo) || installedInfo.Mode() != currentInfo.Mode() {
		return nil, errors.New("installed index identity or mode changed")
	}
	installedBytes, err := readBounded(installed, indexByteLimit)
	if err != nil || !bytes.Equal(installedBytes, replacement) {
		return nil, errors.New("installed index bytes changed")
	}
	installedAfter, statErr := installed.Stat()
	installedNamed, namedErr := parent.Lstat(indexName)
	parentNamedFinal, parentErr := noFollowDirectoryInfo(parentPath)
	if statErr != nil || namedErr != nil || parentErr != nil ||
		!stableFileInfo(installedInfo, installedAfter) || !stableFileInfo(installedInfo, installedNamed) ||
		!os.SameFile(parentNamed, parentNamedFinal) {
		return nil, errors.New("installed index binding changed while verifying")
	}
	return installedAfter, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readBounded(file, limit)
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("file exceeds capture limit")
	}
	return contents, nil
}

func parseWorktreeStates(output []byte) ([]worktreeState, error) {
	if len(output) < 2 || !bytes.HasSuffix(output, []byte{0, 0}) {
		return nil, errors.New("Git returned unterminated worktree registrations")
	}
	recordBytes := bytes.Split(output[:len(output)-2], []byte{0, 0})
	states := make([]worktreeState, 0, len(recordBytes))
	seen := make(map[string]struct{}, len(recordBytes))
	for _, record := range recordBytes {
		if len(record) == 0 {
			return nil, errors.New("Git returned an empty worktree registration")
		}
		fields := bytes.Split(record, []byte{0})
		if len(fields) == 0 || !bytes.HasPrefix(fields[0], []byte("worktree ")) || len(fields[0]) == len("worktree ") {
			return nil, errors.New("Git returned malformed worktree identity")
		}
		rawPath := string(fields[0][len("worktree "):])
		path, err := canonicalCandidate(rawPath)
		if err != nil {
			return nil, errors.New("Git returned an invalid worktree path")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, errors.New("Git returned duplicate worktree identity")
		}
		seen[path] = struct{}{}
		state := worktreeState{path: path, rawPath: rawPath}
		for _, rawField := range fields[1:] {
			field := string(rawField)
			switch {
			case strings.HasPrefix(field, "HEAD ") && validObjectID(strings.TrimPrefix(field, "HEAD ")) && state.head == "":
				state.head = strings.TrimPrefix(field, "HEAD ")
			case strings.HasPrefix(field, "branch refs/") && len(field) > len("branch refs/") && state.branch == "" && !state.detached:
				state.branch = strings.TrimPrefix(field, "branch ")
			case field == "detached" && state.branch == "" && !state.detached:
				state.detached = true
			case field == "bare" && !state.bare:
				state.bare = true
			case (field == "locked" || strings.HasPrefix(field, "locked ") && len(field) > len("locked ")) && !state.locked:
				state.locked = true
				state.lockReason = strings.TrimPrefix(field, "locked")
				state.lockReason = strings.TrimPrefix(state.lockReason, " ")
			case (field == "prunable" || strings.HasPrefix(field, "prunable ") && len(field) > len("prunable ")) && !state.prunable:
				state.prunable = true
				state.prunableReason = strings.TrimPrefix(field, "prunable")
				state.prunableReason = strings.TrimPrefix(state.prunableReason, " ")
			default:
				return nil, errors.New("Git returned malformed or ambiguous worktree metadata")
			}
		}
		if state.bare {
			if state.head != "" || state.branch != "" || state.detached {
				return nil, errors.New("Git returned contradictory bare worktree metadata")
			}
		} else if state.head == "" || (state.branch == "") == !state.detached {
			return nil, errors.New("Git returned incomplete worktree metadata")
		}
		states = append(states, state)
	}
	sort.Slice(states, func(left, right int) bool {
		if states[left].path == states[right].path {
			return states[left].rawPath < states[right].rawPath
		}
		return states[left].path < states[right].path
	})
	return states, nil
}

func containsRefOtherThan(refs []string, excluded string) bool {
	for _, ref := range refs {
		if ref != excluded {
			return true
		}
	}
	return false
}

func refChanges(before, after map[string]refState) (newRefs, deletedRefs, movedRefs []string) {
	for ref, value := range after {
		old, existed := before[ref]
		if !existed {
			newRefs = append(newRefs, ref)
		} else if old != value {
			movedRefs = append(movedRefs, ref)
		}
	}
	for ref := range before {
		if _, exists := after[ref]; !exists {
			deletedRefs = append(deletedRefs, ref)
		}
	}
	sort.Strings(newRefs)
	sort.Strings(deletedRefs)
	sort.Strings(movedRefs)
	return newRefs, deletedRefs, movedRefs
}

func parseRefs(output []byte) (map[string]refState, error) {
	refs := make(map[string]refState)
	if len(output) == 0 {
		return refs, nil
	}
	if output[len(output)-1] != '\n' {
		return nil, errors.New("Git returned unterminated refs")
	}
	for _, line := range bytes.Split(output[:len(output)-1], []byte{'\n'}) {
		parts := bytes.Split(line, []byte{0})
		if len(parts) != 3 || !strings.HasPrefix(string(parts[0]), "refs/") || !validObjectID(string(parts[1])) ||
			(len(parts[2]) != 0 && !strings.HasPrefix(string(parts[2]), "refs/")) {
			return nil, errors.New("Git returned malformed refs")
		}
		name := string(parts[0])
		if _, duplicate := refs[name]; duplicate {
			return nil, errors.New("Git returned duplicate refs")
		}
		refs[name] = refState{object: string(parts[1]), symbolic: string(parts[2])}
	}
	return refs, nil
}

func snapshotLooseSymbolicRefsWithLimits(roots []string, limits looseRefLimits) (map[string]refState, error) {
	if limits.maxEntries <= 0 || limits.maxPathBytes <= 0 || limits.maxDepth <= 0 || limits.maxContents <= 0 {
		return nil, errors.New("invalid loose ref traversal limits")
	}
	refs := make(map[string]refState)
	entries, pathBytes := 0, 0
	contents := int64(0)
	seenRoots := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = filepath.Clean(root)
		if _, seen := seenRoots[root]; seen {
			continue
		}
		seenRoots[root] = struct{}{}
		rootInfo, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !rootInfo.IsDir() {
			return nil, errors.New("loose ref root is unavailable")
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return errors.New("resolve loose ref path")
			}
			if relative == "." {
				return nil
			}
			entries++
			pathBytes += len(relative)
			depth := strings.Count(relative, string(filepath.Separator)) + 1
			if entries > limits.maxEntries || pathBytes > limits.maxPathBytes || depth > limits.maxDepth {
				return errors.New("loose ref traversal exceeds shape limits")
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !entry.Type().IsRegular() {
				return errors.New("unsupported loose ref file type")
			}
			remaining := limits.maxContents - contents
			if remaining <= 0 {
				return errors.New("loose refs exceed content limit")
			}
			file, err := snapshotExactPath(path, remaining)
			if err != nil || !file.exists || !file.mode.IsRegular() {
				return errors.New("read loose ref failed")
			}
			contents += int64(len(file.bytes))
			if !bytes.HasPrefix(file.bytes, []byte("ref: ")) {
				return nil
			}
			target := strings.TrimSuffix(string(bytes.TrimPrefix(file.bytes, []byte("ref: "))), "\n")
			name := filepath.ToSlash(filepath.Join("refs", relative))
			if !strings.HasPrefix(target, "refs/") || strings.ContainsAny(target, "\x00\r\n") || strings.ContainsAny(name, "\x00\r\n") {
				return errors.New("malformed loose symbolic ref")
			}
			if _, duplicate := refs[name]; duplicate {
				return errors.New("duplicate loose symbolic ref storage")
			}
			refs[name] = refState{symbolic: target}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func snapshotRawTrees(roots []rawTreeRoot, limits rawTreeLimits) (map[string]exactFile, error) {
	return snapshotRawTreesWithHooks(roots, limits, rawTreeHooks{})
}

func snapshotRawTreesWithHooks(roots []rawTreeRoot, limits rawTreeLimits, hooks rawTreeHooks) (map[string]exactFile, error) {
	if limits.maxEntries <= 0 || limits.maxPathBytes <= 0 || limits.maxDepth <= 0 || limits.maxContents <= 0 || limits.perFile <= 0 {
		return nil, errors.New("invalid raw control traversal limits")
	}
	result := make(map[string]exactFile)
	budget := rawTreeBudget{limits: limits}
	seenNames := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root.name == "" || strings.ContainsAny(root.name, "\x00:") {
			return nil, errors.New("invalid raw control root name")
		}
		if _, duplicate := seenNames[root.name]; duplicate {
			return nil, errors.New("duplicate raw control root name")
		}
		seenNames[root.name] = struct{}{}
		root.path = filepath.Clean(root.path)
		rootFile, err := snapshotExactPath(root.path, limits.perFile)
		if err != nil {
			return nil, errors.New("snapshot raw control root")
		}
		result[root.name+":."] = rootFile
		if !rootFile.exists {
			continue
		}
		if !rootFile.mode.IsDir() || rootFile.mode&os.ModeSymlink != 0 {
			return nil, errors.New("raw control root is not a directory")
		}
		opened, err := os.OpenRoot(root.path)
		if err != nil {
			return nil, errors.New("open raw control root")
		}
		bound, boundErr := opened.Stat(".")
		named, namedErr := os.Lstat(root.path)
		if boundErr != nil || namedErr != nil || named.Mode()&os.ModeSymlink != 0 ||
			!stableFileInfo(rootFile.info, bound) || !stableFileInfo(rootFile.info, named) {
			_ = opened.Close()
			return nil, errors.New("raw control root binding changed")
		}
		if err := snapshotRawDirectory(opened, root.path, root.name, "", rootFile.info, 0, &budget, result, hooks); err != nil {
			_ = opened.Close()
			return nil, err
		}
		_ = opened.Close()
		after, err := os.Lstat(root.path)
		if err != nil || after.Mode()&os.ModeSymlink != 0 || !stableFileInfo(rootFile.info, after) {
			return nil, errors.New("raw control root changed while snapshotting")
		}
	}
	return result, nil
}

type rawTreeBudget struct {
	limits    rawTreeLimits
	entries   int
	pathBytes int
	contents  int64
}

func snapshotRawDirectory(root *os.Root, displayRoot, namespace, relative string, expected os.FileInfo, depth int, budget *rawTreeBudget, result map[string]exactFile, hooks rawTreeHooks) error {
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open raw control directory")
	}
	defer directory.Close()
	directoryInfo, err := directory.Stat()
	if err != nil || !stableFileInfo(expected, directoryInfo) {
		return errors.New("raw control directory binding changed")
	}
	for {
		batch, readErr := directory.ReadDir(256)
		for _, entry := range batch {
			name := entry.Name()
			childRelative := name
			if relative != "" {
				childRelative = relative + "/" + name
			}
			childDepth := depth + 1
			budget.entries++
			budget.pathBytes += len(childRelative)
			if budget.entries > budget.limits.maxEntries || budget.pathBytes > budget.limits.maxPathBytes || childDepth > budget.limits.maxDepth {
				return errors.New("raw control tree exceeds shape limits")
			}
			info, err := root.Lstat(name)
			if err != nil {
				return errors.New("stat raw control entry")
			}
			path := filepath.Join(displayRoot, filepath.FromSlash(childRelative))
			file, err := snapshotRawRootEntry(root, name, path, info, budget, hooks)
			if err != nil {
				return err
			}
			result[namespace+":"+childRelative] = file
			if info.IsDir() {
				child, err := root.OpenRoot(name)
				if err != nil {
					return errors.New("open raw control child directory")
				}
				childBound, childErr := child.Stat(".")
				namedAgain, namedErr := root.Lstat(name)
				if childErr != nil || namedErr != nil || namedAgain.Mode()&os.ModeSymlink != 0 ||
					!stableFileInfo(info, childBound) || !stableFileInfo(info, namedAgain) {
					_ = child.Close()
					return errors.New("raw control child directory binding changed")
				}
				if err := snapshotRawDirectory(child, displayRoot, namespace, childRelative, info, childDepth, budget, result, hooks); err != nil {
					_ = child.Close()
					return err
				}
				_ = child.Close()
				namedAfter, err := root.Lstat(name)
				if err != nil || !stableFileInfo(info, namedAfter) {
					return errors.New("raw control child changed while snapshotting")
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return errors.New("read raw control directory")
		}
	}
	if hooks.afterDirectoryEOF != nil {
		if err := hooks.afterDirectoryEOF(filepath.Join(displayRoot, filepath.FromSlash(relative))); err != nil {
			return errors.New("after raw control directory read")
		}
	}
	final, err := root.Stat(".")
	namedFinal, namedErr := root.Lstat(".")
	if err != nil || namedErr != nil || !stableFileInfo(expected, final) || !stableFileInfo(expected, namedFinal) {
		return errors.New("raw control directory changed while reading")
	}
	return nil
}

func snapshotRawRootEntry(root *os.Root, name, path string, info os.FileInfo, budget *rawTreeBudget, hooks rawTreeHooks) (exactFile, error) {
	file := exactFile{path: path, exists: true, mode: info.Mode(), info: info}
	if info.IsDir() {
		return file, nil
	}
	remaining := budget.limits.maxContents - budget.contents
	if remaining < 0 {
		return exactFile{}, errors.New("raw control tree exceeds content limit")
	}
	limit := budget.limits.perFile
	if remaining < limit {
		limit = remaining
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(name)
		if err != nil || int64(len(target)) > limit {
			return exactFile{}, errors.New("read raw control symlink")
		}
		file.bytes = []byte(target)
	} else if info.Mode().IsRegular() {
		opened, err := root.Open(name)
		if err != nil {
			return exactFile{}, errors.New("open raw control file")
		}
		openedInfo, err := opened.Stat()
		if err != nil || !os.SameFile(info, openedInfo) {
			_ = opened.Close()
			return exactFile{}, errors.New("raw control file binding changed")
		}
		contents, err := readBounded(opened, limit)
		if err != nil {
			_ = opened.Close()
			return exactFile{}, errors.New("read raw control file")
		}
		if hooks.afterFileRead != nil {
			if err := hooks.afterFileRead(path); err != nil {
				_ = opened.Close()
				return exactFile{}, errors.New("after raw control file read")
			}
		}
		afterOpenedInfo, statErr := opened.Stat()
		_ = opened.Close()
		if statErr != nil || !stableFileInfo(openedInfo, afterOpenedInfo) {
			return exactFile{}, errors.New("raw control file changed while reading")
		}
		file.bytes = contents
		file.mode = openedInfo.Mode()
		file.info = openedInfo
	} else {
		return exactFile{}, errors.New("raw control tree has unsupported file type")
	}
	budget.contents += int64(len(file.bytes))
	namedAgain, err := root.Lstat(name)
	if err != nil || !stableFileInfo(info, namedAgain) || namedAgain.Mode() != file.mode {
		return exactFile{}, errors.New("raw control entry changed while snapshotting")
	}
	return file, nil
}

func stableFileInfo(left, right os.FileInfo) bool {
	if left == nil || right == nil || !os.SameFile(left, right) || left.Mode() != right.Mode() ||
		left.Size() != right.Size() || !left.ModTime().Equal(right.ModTime()) {
		return false
	}
	leftSec, leftNSec, leftOK := linuxChangeTime(left)
	rightSec, rightNSec, rightOK := linuxChangeTime(right)
	if runtime.GOOS == "linux" && (!leftOK || !rightOK) {
		return false
	}
	return leftOK == rightOK && (!leftOK || leftSec == rightSec && leftNSec == rightNSec)
}

func linuxChangeTime(info os.FileInfo) (int64, int64, bool) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, 0, false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, 0, false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, 0, false
	}
	change := value.FieldByName("Ctim")
	if !change.IsValid() || change.Kind() != reflect.Struct {
		return 0, 0, false
	}
	seconds, nanoseconds := change.FieldByName("Sec"), change.FieldByName("Nsec")
	if !seconds.IsValid() || !nanoseconds.IsValid() || !seconds.CanInt() || !nanoseconds.CanInt() {
		return 0, 0, false
	}
	return seconds.Int(), nanoseconds.Int(), true
}

func snapshotRawControlFiles(gitDir, commonDir string) (map[string]exactFile, error) {
	return snapshotRawControlFilesWithHooks(gitDir, commonDir, rawTreeHooks{})
}

func snapshotRawControlFilesWithHooks(gitDir, commonDir string, hooks rawTreeHooks) (map[string]exactFile, error) {
	result := make(map[string]exactFile, 2)
	budget := rawTreeBudget{limits: defaultRawControlLimits}
	for name, directory := range map[string]string{
		"common:packed-refs":   commonDir,
		"worktree:packed-refs": gitDir,
	} {
		directoryInfo, err := noFollowDirectoryInfo(directory)
		if err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, err
		}
		bound, boundErr := root.Stat(".")
		named, namedErr := noFollowDirectoryInfo(directory)
		if boundErr != nil || namedErr != nil || !stableFileInfo(directoryInfo, bound) || !stableFileInfo(directoryInfo, named) {
			_ = root.Close()
			return nil, errors.New("raw control directory binding changed")
		}
		info, err := root.Lstat("packed-refs")
		if errors.Is(err, os.ErrNotExist) {
			result[name] = exactFile{path: filepath.Join(directory, "packed-refs")}
		} else if err != nil {
			_ = root.Close()
			return nil, errors.New("stat raw control file")
		} else {
			file, snapshotErr := snapshotRawRootEntry(root, "packed-refs", filepath.Join(directory, "packed-refs"), info, &budget, hooks)
			if snapshotErr != nil {
				_ = root.Close()
				return nil, snapshotErr
			}
			result[name] = file
		}
		final, finalErr := root.Stat(".")
		namedFinal, namedFinalErr := noFollowDirectoryInfo(directory)
		_ = root.Close()
		if finalErr != nil || namedFinalErr != nil || !stableFileInfo(directoryInfo, final) || !stableFileInfo(directoryInfo, namedFinal) {
			return nil, errors.New("raw control directory changed while snapshotting")
		}
	}
	return result, nil
}

func mergeLooseSymbolicRefs(refs, loose map[string]refState) error {
	for name, symbolic := range loose {
		if existing, exists := refs[name]; exists {
			if existing.symbolic != symbolic.symbolic {
				return errors.New("symbolic ref sources disagree")
			}
			continue
		}
		refs[name] = symbolic
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range []byte(value) {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return false
	}
	for _, character := range path {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (w *Workspace) validate(ctx context.Context) error {
	gitDir, err := gitPath(ctx, w.path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return fmt.Errorf("validate linked worktree Git directory: %w", err)
	}
	gitDir, err = canonicalExisting(gitDir)
	if err != nil || gitDir != w.gitDir {
		return errors.New("cache worktree Git directory changed")
	}
	branch, err := gitPath(ctx, w.path, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != w.branch {
		return fmt.Errorf("validate cache worktree branch: got %q, want %q", branch, w.branch)
	}
	if err := w.requireDirectRunRef(ctx, w.green, false); err != nil {
		return fmt.Errorf("validate cache worktree run ref: %w", err)
	}
	commonDir, err := gitPath(ctx, w.path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("validate cache repository: %w", err)
	}
	commonDir, err = canonicalExisting(commonDir)
	if err != nil || commonDir != w.commonDir {
		return errors.New("cache worktree belongs to a different repository")
	}
	status, err := gitPath(ctx, w.path, "status", "--porcelain=v2", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("validate cache worktree status: %w", err)
	}
	if status != "" {
		return errors.New("cache worktree is not clean")
	}
	return nil
}

func (w *Workspace) bindControlState() error {
	rootInfo, err := noFollowDirectoryInfo(w.path)
	if err != nil {
		return errors.New("workspace root is unavailable")
	}
	cacheRoot := filepath.Dir(w.path)
	cacheRootInfo, err := os.Lstat(cacheRoot)
	if err != nil || !cacheRootInfo.IsDir() || cacheRootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace cache root is unavailable")
	}
	dotGit, err := snapshotExactPath(filepath.Join(w.path, ".git"), gitOutputLimit)
	if err != nil || !dotGit.exists || !dotGit.mode.IsRegular() {
		return errors.New("workspace .git indirection is unavailable")
	}
	gitDir, err := linkedGitDir(w.path)
	if err != nil || gitDir != w.gitDir {
		return errors.New("workspace linked Git directory changed")
	}
	commonDir, err := linkedCommonDir(gitDir)
	if err != nil || commonDir != w.commonDir {
		return errors.New("workspace common Git directory changed")
	}
	gitDirInfo, gitErr := noFollowDirectoryInfo(gitDir)
	commonInfo, commonErr := noFollowDirectoryInfo(commonDir)
	indexPath, indexErr := canonicalExisting(filepath.Join(gitDir, "index"))
	index, indexReadErr := snapshotExactPath(indexPath, indexByteLimit)
	if gitErr != nil || commonErr != nil || indexErr != nil || indexReadErr != nil || !index.exists || !index.mode.IsRegular() {
		return errors.New("workspace control object is unavailable")
	}
	w.dotGit = dotGit
	w.rootInfo = rootInfo
	w.gitDirInfo = gitDirInfo
	w.commonInfo = commonInfo
	w.indexPath = indexPath
	w.indexInfo = index.info
	w.indexBytes = append([]byte(nil), index.bytes...)
	w.cacheRoot = cacheRoot
	w.cacheRootInfo = cacheRootInfo
	return nil
}

func (w *Workspace) validateControlBinding() error {
	if err := w.validateStaticControlBinding(); err != nil {
		return err
	}
	indexInfo, err := os.Stat(w.indexPath)
	if err != nil || w.indexInfo == nil || !os.SameFile(w.indexInfo, indexInfo) {
		return errors.New("workspace index identity changed")
	}
	return nil
}

func (w *Workspace) validateAttemptIndexBinding() error {
	if err := w.validateStaticControlBinding(); err != nil {
		return err
	}
	index, err := snapshotExactPath(w.indexPath, indexByteLimit)
	if err != nil || !index.exists || !index.mode.IsRegular() {
		return errors.New("workspace attempt index is unavailable")
	}
	if w.indexInfo != nil && os.SameFile(w.indexInfo, index.info) {
		return nil
	}
	if bytes.Equal(w.indexBytes, index.bytes) {
		return errors.New("workspace index identity changed without attempt content")
	}
	return nil
}

func (w *Workspace) validateStaticControlBinding() error {
	cacheRootInfo, cacheRootErr := os.Lstat(w.cacheRoot)
	if cacheRootErr != nil || w.cacheRootInfo == nil || !cacheRootInfo.IsDir() || cacheRootInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(w.cacheRootInfo, cacheRootInfo) {
		return errors.New("workspace cache root identity changed")
	}
	rootInfo, err := noFollowDirectoryInfo(w.path)
	if err != nil || w.rootInfo == nil || !os.SameFile(w.rootInfo, rootInfo) {
		return errors.New("workspace root identity changed")
	}
	dotGit, err := snapshotExactPath(filepath.Join(w.path, ".git"), gitOutputLimit)
	if err != nil || !exactFileStableEqual(w.dotGit, dotGit) {
		return errors.New("workspace .git indirection changed")
	}
	gitDir, err := linkedGitDir(w.path)
	if err != nil || gitDir != w.gitDir {
		return errors.New("workspace linked Git directory changed")
	}
	commonDir, err := linkedCommonDir(gitDir)
	if err != nil || commonDir != w.commonDir {
		return errors.New("workspace common Git directory changed")
	}
	gitDirInfo, gitErr := noFollowDirectoryInfo(gitDir)
	commonInfo, commonErr := noFollowDirectoryInfo(commonDir)
	indexPath, indexErr := canonicalExisting(filepath.Join(gitDir, "index"))
	if gitErr != nil || commonErr != nil || indexErr != nil || !os.SameFile(w.gitDirInfo, gitDirInfo) ||
		!os.SameFile(w.commonInfo, commonInfo) || indexPath != w.indexPath {
		return errors.New("workspace control binding identity changed")
	}
	return nil
}

func (w *Workspace) mutateOwnedIndex(ctx context.Context, allowAttempt bool, expectedTree string, args ...string) (string, error) {
	if err := w.validateStaticControlBinding(); err != nil {
		return "", err
	}
	current, err := snapshotExactPath(w.indexPath, indexByteLimit)
	if err != nil || !current.exists || !current.mode.IsRegular() || current.info == nil {
		return "", errors.New("owned index input is unavailable")
	}
	if w.indexInfo == nil {
		return "", errors.New("owned index binding is unavailable")
	}
	if !os.SameFile(w.indexInfo, current.info) {
		if !allowAttempt || bytes.Equal(w.indexBytes, current.bytes) {
			return "", errors.New("owned index input identity changed")
		}
	}
	private, err := w.createPrivateTempDir()
	if err != nil {
		return "", errors.New("create private index directory")
	}
	defer private.close()
	temporary, temporaryPath, err := private.createFile("index")
	if err != nil {
		return "", errors.New("create private index")
	}
	ownedTemporaryInfo, err := temporary.Stat()
	if err != nil {
		_ = temporary.Close()
		return "", errors.New("bind private index")
	}
	defer func() {
		private.removeFile(filepath.Base(temporaryPath), ownedTemporaryInfo)
	}()
	if _, err := temporary.Write(current.bytes); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		_ = temporary.Close()
		return "", errors.New("prepare private index contents")
	}
	gitArgs := append([]string{"--git-dir=" + w.gitDir, "--work-tree=" + w.path}, args...)
	if err := private.validatePathBinding(); err != nil {
		return "", errors.New("private index path changed")
	}
	if _, err := gitcmd.OutputWithIndex(ctx, w.path, gitcmd.Hermetic, gitOutputLimit, temporaryPath, gitArgs...); err != nil {
		return "", err
	}
	if err := private.validatePathBinding(); err != nil {
		return "", errors.New("private index path changed")
	}
	treeOutput, err := gitcmd.OutputWithIndex(ctx, w.path, gitcmd.Hermetic, gitOutputLimit, temporaryPath,
		"--git-dir="+w.gitDir, "--work-tree="+w.path, "write-tree")
	tree := strings.TrimSpace(string(treeOutput))
	if err != nil || private.validatePathBinding() != nil || !validObjectID(tree) || expectedTree != "" && tree != expectedTree {
		return "", errors.New("owned index result has an unexpected tree")
	}
	result, err := private.snapshotFile(filepath.Base(temporaryPath), indexByteLimit)
	if err != nil || !result.exists || !result.mode.IsRegular() {
		return "", errors.New("owned index result is unavailable")
	}
	ownedTemporaryInfo = result.info
	if err := w.validateStaticControlBinding(); err != nil {
		return "", err
	}
	if w.beforeIndexInstall != nil {
		if err := w.beforeIndexInstall(); err != nil {
			return "", errors.New("before owned index install")
		}
	}
	installedInfo, err := replaceIndexCASBound(w.indexPath, current.bytes, current.info, result.bytes, indexCASHooks{})
	if err != nil {
		return "", fmt.Errorf("install owned index result: %w", err)
	}
	installed, err := snapshotExactPath(w.indexPath, indexByteLimit)
	if err != nil || !installed.exists || installed.info == nil || !os.SameFile(installedInfo, installed.info) || !bytes.Equal(installed.bytes, result.bytes) {
		return "", errors.New("verify installed owned index")
	}
	w.indexInfo = installedInfo
	w.indexBytes = append(w.indexBytes[:0], installed.bytes...)
	return tree, nil
}

func validateCreatedWorkspacePath(ctx context.Context, repositoryRoot, path, commonDir string, protected []string, branch, originalHead string) (string, error) {
	registrations, err := worktreeRegistrationsSnapshot(ctx, repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("relist created worktree registration: %w", err)
	}
	refName := "refs/heads/" + branch
	found := 0
	for _, registration := range registrations {
		if filepath.Clean(registration.path) == path {
			if registration.head != originalHead || registration.branch != refName {
				return "", errors.New("created worktree registration does not match the requested HEAD and branch")
			}
			found++
			continue
		}
		protected = append(protected, registration.path)
	}
	if found != 1 {
		return "", errors.New("created worktree registration is missing")
	}
	runRef, err := inspectRefState(ctx, repositoryRoot, refName)
	if err != nil || runRef.symbolic != "" || runRef.object != originalHead {
		return "", errors.New("created worktree run ref is not a direct ref at original HEAD")
	}
	actual, err := canonicalExisting(path)
	if err != nil {
		return "", fmt.Errorf("recanonicalize created cache worktree: %w", err)
	}
	if actual != path {
		return "", errors.New("created cache worktree path is no longer external at its validated canonical location")
	}
	for _, root := range protected {
		canonicalRoot, canonicalErr := canonicalCandidate(root)
		if canonicalErr != nil {
			return "", fmt.Errorf("recanonicalize protected Git path: %w", canonicalErr)
		}
		if pathWithin(actual, canonicalRoot) {
			return "", errors.New("created cache worktree path is not external to protected Git state")
		}
	}
	gitDir, err := linkedGitDir(path)
	if err != nil {
		return "", fmt.Errorf("validate created .git indirection: %w", err)
	}
	if !pathWithin(gitDir, commonDir) || gitDir == commonDir {
		return "", errors.New("created linked Git directory is outside the expected common repository")
	}
	linkedCommon, err := linkedCommonDir(gitDir)
	if err != nil || linkedCommon != commonDir {
		return "", errors.New("created linked Git directory names a different common repository")
	}
	registeredGitDir, err := uniqueRegisteredGitDir(commonDir, path)
	if err != nil || registeredGitDir != gitDir {
		return "", errors.New("created .git indirection does not name the unique registered linked Git directory")
	}
	return gitDir, nil
}

func uniqueRegisteredGitDir(commonDir, worktreePath string) (string, error) {
	root := filepath.Join(commonDir, "worktrees")
	directory, err := os.Open(root)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	wantBacklink := filepath.Join(worktreePath, ".git")
	var matches []string
	count := 0
	for {
		entries, readErr := directory.Readdir(256)
		for _, entry := range entries {
			count++
			if count > 100_000 {
				return "", errors.New("too many linked Git directories")
			}
			if !entry.IsDir() {
				continue
			}
			candidate := filepath.Join(root, entry.Name())
			backlink, snapshotErr := snapshotExactPath(filepath.Join(candidate, "gitdir"), gitOutputLimit)
			if snapshotErr != nil || !backlink.exists || !backlink.mode.IsRegular() {
				continue
			}
			backlinkPath := strings.TrimSpace(string(backlink.bytes))
			if !filepath.IsAbs(backlinkPath) {
				backlinkPath = filepath.Join(candidate, backlinkPath)
			}
			backlinkPath, snapshotErr = canonicalExisting(backlinkPath)
			if snapshotErr == nil && backlinkPath == wantBacklink {
				canonical, canonicalErr := canonicalExisting(candidate)
				if canonicalErr != nil {
					return "", canonicalErr
				}
				matches = append(matches, canonical)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("found %d linked Git directories for requested worktree", len(matches))
	}
	return matches[0], nil
}

func linkedGitDir(worktreePath string) (string, error) {
	control, err := snapshotExactPath(filepath.Join(worktreePath, ".git"), gitOutputLimit)
	if err != nil {
		return "", err
	}
	if !control.exists || !control.mode.IsRegular() {
		return "", errors.New(".git is not a regular indirection file")
	}
	line := strings.TrimSpace(string(control.bytes))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", errors.New(".git indirection is malformed")
	}
	path := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktreePath, path)
	}
	return canonicalExisting(path)
}

func linkedCommonDir(gitDir string) (string, error) {
	commondir, err := snapshotExactPath(filepath.Join(gitDir, "commondir"), gitOutputLimit)
	if err != nil {
		return "", err
	}
	if !commondir.exists || !commondir.mode.IsRegular() {
		return "", errors.New("linked Git directory has no regular commondir file")
	}
	path := strings.TrimSpace(string(commondir.bytes))
	if path == "" {
		return "", errors.New("linked Git directory has an empty commondir")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(gitDir, path)
	}
	return canonicalExisting(path)
}

type workspaceRegistration struct {
	path   string
	head   string
	branch string
}

func cleanupFailedWorkspaceCreation(ctx context.Context, repositoryRoot, commonDir string, commonInfo os.FileInfo, path, branch, originalHead string) error {
	if err := validateDirectoryIdentity(commonDir, commonInfo); err != nil {
		return errors.New("creation cleanup incomplete: common Git directory binding changed")
	}
	registrations, err := worktreeRegistrationsSnapshotAt(ctx, repositoryRoot, commonDir)
	if err != nil {
		return fmt.Errorf("creation cleanup incomplete: %w", err)
	}
	refName := "refs/heads/" + branch
	var requested *workspaceRegistration
	for index := range registrations {
		if filepath.Clean(registrations[index].path) == path {
			requested = &registrations[index]
		}
	}
	ref, refErr := inspectRefStateAt(ctx, repositoryRoot, commonDir, refName)
	if refErr != nil {
		return fmt.Errorf("creation cleanup incomplete: inspect run ref before path cleanup: %w", refErr)
	}
	refOwned := ref.symbolic == "" && ref.object == originalHead

	var cleanupErrors []error
	pathAbsent := false
	if requested != nil {
		cleanupErrors = append(cleanupErrors, errors.New("creation cleanup incomplete: partial worktree registration/path preserved for diagnosis"))
	} else {
		_, statErr := os.Lstat(path)
		switch {
		case statErr == nil:
			cleanupErrors = append(cleanupErrors, errors.New("creation cleanup incomplete: partial path preserved for diagnosis"))
		case errors.Is(statErr, os.ErrNotExist):
			pathAbsent = true
		default:
			cleanupErrors = append(cleanupErrors, fmt.Errorf("creation cleanup incomplete: inspect partial path: %w", statErr))
		}
	}

	if err := validateDirectoryIdentity(commonDir, commonInfo); err != nil {
		cleanupErrors = append(cleanupErrors, errors.New("creation cleanup incomplete: common Git directory binding changed"))
		return errors.Join(cleanupErrors...)
	}
	registrations, registrationErr := worktreeRegistrationsSnapshotAt(ctx, repositoryRoot, commonDir)
	if registrationErr != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("creation cleanup incomplete: relist worktrees: %w", registrationErr))
		return errors.Join(cleanupErrors...)
	}
	attached := false
	for _, registration := range registrations {
		if registration.branch == refName {
			attached = true
		}
	}
	ref, refErr = inspectRefStateAt(ctx, repositoryRoot, commonDir, refName)
	if refErr != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("creation cleanup incomplete: inspect run ref: %w", refErr))
	} else if ref != (refState{}) {
		if refOwned && ref.symbolic == "" && ref.object == originalHead && !attached && requested == nil && pathAbsent {
			if err := validateDirectoryIdentity(commonDir, commonInfo); err != nil {
				cleanupErrors = append(cleanupErrors, errors.New("creation cleanup incomplete: common Git directory binding changed before ref cleanup"))
				return errors.Join(cleanupErrors...)
			}
			if _, deleteErr := gitcmd.Output(ctx, repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
				"--git-dir="+commonDir, "-c", "core.hooksPath="+os.DevNull,
				"update-ref", "--no-deref", "-d", refName, originalHead); deleteErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("creation cleanup incomplete: delete owned run ref by compare-and-swap: %w", deleteErr))
			}
		} else {
			cleanupErrors = append(cleanupErrors, errors.New("creation cleanup incomplete: unproven or attached run ref preserved"))
		}
	}
	return errors.Join(cleanupErrors...)
}

func validateDirectoryIdentity(path string, expected os.FileInfo) error {
	current, err := noFollowDirectoryInfo(path)
	if err != nil || expected == nil || !os.SameFile(expected, current) {
		return errors.New("directory identity changed")
	}
	return nil
}

func noFollowDirectoryInfo(path string) (os.FileInfo, error) {
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("named path is not a directory")
	}
	canonical, err := canonicalExisting(path)
	if err != nil || canonical != filepath.Clean(path) {
		return nil, errors.New("directory canonical path changed")
	}
	return current, nil
}

func inspectRefState(ctx context.Context, dir, ref string) (refState, error) {
	return inspectRefStateAt(ctx, dir, "", ref)
}

func inspectRefStateAt(ctx context.Context, dir, gitDir, ref string) (refState, error) {
	var previous *refState
	for range 3 {
		prefix := []string(nil)
		if gitDir != "" {
			prefix = append(prefix, "--git-dir="+gitDir)
		}
		symbolicArgs := append(append([]string(nil), prefix...), "symbolic-ref", "-q", ref)
		symbolicOutput, symbolicErr := gitcmd.Output(ctx, dir, gitcmd.Hermetic, gitOutputLimit, symbolicArgs...)
		refArgs := append(append([]string(nil), prefix...), "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", ref)
		output, err := gitcmd.Output(ctx, dir, gitcmd.Hermetic, gitOutputLimit, refArgs...)
		if err != nil {
			return refState{}, err
		}
		refs, err := parseRefs(output)
		if err != nil {
			return refState{}, err
		}
		state := refs[ref]
		if symbolicErr == nil {
			target := strings.TrimSpace(string(symbolicOutput))
			if !strings.HasPrefix(target, "refs/") {
				return refState{}, errors.New("Git returned malformed symbolic ref target")
			}
			state.symbolic = target
		}
		if previous != nil && *previous == state {
			return state, nil
		}
		previous = &state
	}
	return refState{}, errors.New("ref changed while inspecting direct state")
}

func checkBranchName(ctx context.Context, dir, branch string) error {
	_, err := gitcmd.Output(ctx, dir, gitcmd.Hermetic, gitOutputLimit, "check-ref-format", "--branch", branch)
	return err
}

func gitPath(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := gitcmd.Output(ctx, dir, gitcmd.Hermetic, gitOutputLimit, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func worktreeRegistrationsSnapshot(ctx context.Context, dir string) ([]workspaceRegistration, error) {
	return worktreeRegistrationsSnapshotAt(ctx, dir, "")
}

func worktreeRegistrationsSnapshotAt(ctx context.Context, dir, gitDir string) ([]workspaceRegistration, error) {
	args := []string(nil)
	if gitDir != "" {
		args = append(args, "--git-dir="+gitDir)
	}
	args = append(args, "worktree", "list", "--porcelain", "-z")
	output, err := gitcmd.Output(ctx, dir, gitcmd.Hermetic, gitOutputLimit, args...)
	if err != nil {
		return nil, fmt.Errorf("list registered worktrees: %w", err)
	}
	states, err := parseWorktreeStates(output)
	if err != nil {
		return nil, err
	}
	registrations := make([]workspaceRegistration, 0, len(states))
	for _, state := range states {
		registrations = append(registrations, workspaceRegistration{path: state.rawPath, head: state.head, branch: state.branch})
	}
	return registrations, nil
}

func canonicalExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func canonicalCandidate(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	ancestor := abs
	var suffix []string
	for {
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("no existing path ancestor")
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
	ancestor, err = filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for index := len(suffix) - 1; index >= 0; index-- {
		ancestor = filepath.Join(ancestor, suffix[index])
	}
	return filepath.Clean(ancestor), nil
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validRunID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, character := range id {
		if unicode.IsControl(character) || character == '/' || character == '\\' || unicode.IsSpace(character) {
			return false
		}
	}
	return !strings.Contains(id, "..")
}

func validateIdentity(identity Identity) error {
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.Email) == "" {
		return errors.New("Git identity name and email are required")
	}
	if strings.ContainsRune(identity.Name, 0) || strings.ContainsRune(identity.Email, 0) || strings.ContainsAny(identity.Name, "\r\n") || strings.ContainsAny(identity.Email, "\r\n") {
		return errors.New("Git identity contains invalid characters")
	}
	return nil
}
