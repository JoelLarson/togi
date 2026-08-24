package flywheel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/gitcmd"
)

const (
	landingGitTimeout = 30 * time.Second
	squashSubject     = "togi: apply verified fixes"
)

type landingTransactionContextKey struct{}

// NewLandingTransactionContext starts the cancellation-independent bounded
// context shared by squash creation and guarded landing after rail admission.
func NewLandingTransactionContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), landingGitTimeout)
	return context.WithValue(ctx, landingTransactionContextKey{}, struct{}{}), cancel
}

// LandingStatus records whether a guarded landing was unnecessary, completed,
// or refused without claiming that the feature checkout was changed safely.
type LandingStatus string

const (
	LandingNotNeeded LandingStatus = "not-needed"
	LandingComplete  LandingStatus = "complete"
	LandingBlocked   LandingStatus = "blocked"
)

// CleanupDisposition declares whether the validated commit has landed or must
// remain reachable for inspection after a non-success terminal result.
type CleanupDisposition string

const (
	CleanupLanded            CleanupDisposition = "landed"
	CleanupPreserveValidated CleanupDisposition = "preserve-validated"
)

// SquashValidated creates the landing commit only from an active,
// authenticated latest-green snapshot owned by this workspace.
func (w *Workspace) SquashValidated(ctx context.Context, validated ValidatedTree) (string, error) {
	snapshot, ok := validated.(*ValidatedSnapshot)
	if !ok || snapshot == nil || snapshot.owner != w || snapshot.closed || snapshot.snapshot == nil || w.validationSnapshot != snapshot.snapshot {
		return "", errors.New("refuse squash: validated snapshot ownership changed")
	}
	if err := snapshot.Verify(ctx); err != nil {
		return "", fmt.Errorf("refuse squash: validated snapshot changed: %w", err)
	}
	return w.Squash(ctx)
}

// Squash creates and verifies one commit containing the latest validated tree,
// then advances the owned run ref by compare-and-swap. No commit is created
// when no validated batch exists.
func (w *Workspace) Squash(ctx context.Context) (string, error) {
	if w == nil {
		return "", errors.New("workspace is required")
	}
	if ctx == nil {
		return "", errors.New("squash context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w.green == w.originalHead {
		return "", nil
	}
	latest := w.green
	if err := w.requireDirectRunRef(ctx, latest, false); err != nil {
		return "", fmt.Errorf("refuse squash creation: %w", err)
	}
	if err := w.requireValidatedAncestry(ctx, latest); err != nil {
		return "", err
	}
	tree, err := gitPath(ctx, w.path, "--git-dir="+w.commonDir, "rev-parse", latest+"^{tree}")
	if err != nil || !validObjectID(tree) {
		return "", errors.New("resolve latest validated tree")
	}
	environment := map[string]string{
		"GIT_AUTHOR_NAME": w.identity.Name, "GIT_AUTHOR_EMAIL": w.identity.Email,
		"GIT_COMMITTER_NAME": w.identity.Name, "GIT_COMMITTER_EMAIL": w.identity.Email,
	}
	output, err := gitcmd.OutputEnv(ctx, w.path, gitcmd.Hermetic, gitOutputLimit, environment,
		"--git-dir="+w.commonDir, "-c", "core.hooksPath="+os.DevNull,
		"commit-tree", tree, "-p", w.originalHead, "-m", squashSubject)
	if err != nil {
		return "", fmt.Errorf("create squash commit: %w", err)
	}
	commit := strings.TrimSpace(string(output))
	if !validObjectID(commit) {
		return "", errors.New("create squash commit returned an invalid object ID")
	}
	if err := w.verifySquashObject(ctx, commit, tree); err != nil {
		return "", fmt.Errorf("verify squash commit: %w", err)
	}
	if err := w.requireDirectRunRef(ctx, latest, false); err != nil {
		return "", fmt.Errorf("refuse squash ref update: %w", err)
	}
	if w.beforeSquashRefUpdate != nil {
		if err := w.beforeSquashRefUpdate(); err != nil {
			return "", fmt.Errorf("before squash ref update: %w", err)
		}
	}
	if err := w.verifySquashObject(ctx, commit, tree); err != nil {
		return "", fmt.Errorf("refuse squash ref update after object drift: %w", err)
	}
	if err := w.requireDirectRunRef(ctx, latest, false); err != nil {
		return "", fmt.Errorf("refuse final squash ref update: %w", err)
	}
	ref := "refs/heads/" + w.branch
	if err := w.compareAndSwapBatchRef(ctx, ref, commit, latest); err != nil {
		recoveryCtx, cancel := recoveryContext(ctx)
		defer cancel()
		switch {
		case w.requireDirectRunRef(recoveryCtx, commit, false) == nil:
			if rollbackErr := w.updateBatchRefDirect(recoveryCtx, ref, latest, commit); rollbackErr != nil {
				return "", errors.New("squash ref update was ambiguous and rollback failed")
			}
			return "", errors.New("squash ref update was ambiguous and was rolled back")
		case w.requireDirectRunRef(recoveryCtx, latest, false) == nil:
			return "", errors.New("advance squash run ref by compare-and-swap")
		default:
			return "", errors.New("squash ref update failed with an unexpected run ref; concurrent state preserved")
		}
	}
	recoveryCtx, cancel := recoveryContext(ctx)
	defer cancel()
	postErr := error(nil)
	if w.afterSquashRefUpdate != nil {
		postErr = w.afterSquashRefUpdate()
	}
	if postErr == nil {
		postErr = context.Cause(ctx)
	}
	if postErr == nil {
		postErr = w.verifySquashObject(recoveryCtx, commit, tree)
	}
	if postErr == nil {
		postErr = w.requireDirectRunRef(recoveryCtx, commit, false)
	}
	if postErr != nil {
		if rollbackErr := w.updateBatchRefDirect(recoveryCtx, ref, latest, commit); rollbackErr != nil {
			return "", errors.New("squash ref update was ambiguous and rollback failed")
		}
		return "", fmt.Errorf("squash ref update verification failed: %w", postErr)
	}
	w.green = commit
	return commit, nil
}

func (w *Workspace) requireValidatedAncestry(ctx context.Context, latest string) error {
	if !validObjectID(w.originalHead) || !validObjectID(latest) {
		return errors.New("refuse squash: invalid validated ancestry")
	}
	if _, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "merge-base", "--is-ancestor", w.originalHead, latest); err != nil {
		return errors.New("refuse squash: latest validated commit does not descend from original HEAD")
	}
	return nil
}

func (w *Workspace) verifySquashObject(ctx context.Context, commit, tree string) error {
	if !validObjectID(commit) || !validObjectID(tree) {
		return errors.New("invalid squash object identity")
	}
	raw, err := gitcmd.Output(ctx, w.path, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "cat-file", "commit", commit)
	if err != nil {
		return errors.New("read squash commit object")
	}
	header, message, found := bytes.Cut(raw, []byte("\n\n"))
	if !found || string(message) != squashSubject+"\n" {
		return errors.New("squash commit message differs from the fixed subject")
	}
	lines := strings.Split(string(header), "\n")
	wantAuthor := "author " + w.identity.Name + " <" + w.identity.Email + "> "
	wantCommitter := "committer " + w.identity.Name + " <" + w.identity.Email + "> "
	var trees, parents, authors, committers int
	for _, line := range lines {
		switch {
		case line == "tree "+tree:
			trees++
		case strings.HasPrefix(line, "tree "):
			return errors.New("squash commit has an unexpected tree")
		case line == "parent "+w.originalHead:
			parents++
		case strings.HasPrefix(line, "parent "):
			return errors.New("squash commit has an unexpected parent")
		case strings.HasPrefix(line, wantAuthor):
			authors++
		case strings.HasPrefix(line, "author "):
			return errors.New("squash commit has an unexpected author")
		case strings.HasPrefix(line, wantCommitter):
			committers++
		case strings.HasPrefix(line, "committer "):
			return errors.New("squash commit has an unexpected committer")
		}
	}
	if trees != 1 || parents != 1 || authors != 1 || committers != 1 {
		return errors.New("squash commit object shape is incomplete or ambiguous")
	}
	return nil
}

// Land performs the guarded fast-forward in the bound feature checkout. Once
// admitted, Git work is isolated from the run deadline by a fixed transaction
// timeout so cancellation cannot interrupt recovery halfway through.
func (w *Workspace) Land(ctx context.Context, squash string) (status LandingStatus, resultErr error) {
	if w == nil {
		return LandingBlocked, errors.New("workspace is required")
	}
	if ctx == nil {
		return LandingBlocked, errors.New("landing context is required")
	}
	if squash == "" {
		if w.green == w.originalHead {
			w.landingComplete = true
			return LandingNotNeeded, nil
		}
		return LandingBlocked, errors.New("landing squash commit is required")
	}
	if squash != w.green || !validObjectID(squash) {
		return LandingBlocked, errors.New("landing commit is not the latest validated squash")
	}
	landingCtx, cancel := ctx, func() {}
	if ctx.Value(landingTransactionContextKey{}) == nil {
		newLandingContext := w.newLandingContext
		if newLandingContext == nil {
			newLandingContext = func() (context.Context, context.CancelFunc) {
				return NewLandingTransactionContext(ctx)
			}
		}
		landingCtx, cancel = newLandingContext()
	}
	if landingCtx == nil || cancel == nil {
		return LandingBlocked, errors.New("landing transaction context is unavailable")
	}
	defer cancel()
	if err := w.guardLanding(landingCtx, squash, w.originalHead); err != nil {
		return LandingBlocked, err
	}
	sharedBefore, err := w.SnapshotGitState(landingCtx)
	if err != nil {
		return LandingBlocked, fmt.Errorf("snapshot shared Git state before landing: %w", err)
	}
	featureBefore, err := w.snapshotFeatureControl(landingCtx)
	if err != nil {
		return LandingBlocked, fmt.Errorf("snapshot feature Git control state before landing: %w", err)
	}
	if w.beforeLandingMerge != nil {
		if err := w.beforeLandingMerge(); err != nil {
			return LandingBlocked, fmt.Errorf("before landing merge: %w", err)
		}
	}
	if err := w.guardLanding(landingCtx, squash, w.originalHead); err != nil {
		return LandingBlocked, fmt.Errorf("final landing guard: %w", err)
	}
	sharedAfter, err := w.SnapshotGitState(landingCtx)
	if err != nil || !observedGitStateEqual(sharedBefore, sharedAfter) {
		return LandingBlocked, errors.New("final landing guard: concurrent shared Git state changed and was preserved")
	}
	featureAfter, err := w.snapshotFeatureControl(landingCtx)
	if err != nil || !featureControlEqual(featureBefore, featureAfter) {
		return LandingBlocked, errors.New("final landing guard: feature Git control state changed and was preserved")
	}
	if w.beforeLandingExec != nil {
		if err := w.beforeLandingExec(); err != nil {
			return LandingBlocked, fmt.Errorf("before landing execution: %w", err)
		}
	}
	if err := w.guardLanding(landingCtx, squash, w.originalHead); err != nil {
		return LandingBlocked, fmt.Errorf("immediate landing guard: %w", err)
	}
	sharedFinal, err := w.SnapshotGitState(landingCtx)
	if err != nil || !observedGitStateEqual(sharedAfter, sharedFinal) {
		return LandingBlocked, errors.New("immediate landing guard: shared Git state changed and was preserved")
	}
	featureFinal, err := w.snapshotFeatureControl(landingCtx)
	if err != nil || !featureControlEqual(featureAfter, featureFinal) {
		return LandingBlocked, errors.New("immediate landing guard: feature Git control state changed and was preserved")
	}
	gitBinding, bindErr := gitcmd.BindDirectory(w.featureGitDir, w.featureGitDirInfo)
	if bindErr != nil {
		return LandingBlocked, fmt.Errorf("bind feature Git directory for landing: %w", bindErr)
	}
	workTreeBinding, bindErr := gitcmd.BindDirectory(w.repositoryRoot, w.featureRootInfo)
	if bindErr != nil {
		_ = gitBinding.Close()
		return LandingBlocked, fmt.Errorf("bind feature worktree for landing: %w", bindErr)
	}
	if w.afterLandingBind != nil {
		if err := w.afterLandingBind(); err != nil {
			_ = workTreeBinding.Close()
			_ = gitBinding.Close()
			return LandingBlocked, fmt.Errorf("after landing directory binding: %w", err)
		}
	}
	if err := w.guardLanding(landingCtx, squash, w.originalHead); err != nil {
		_ = workTreeBinding.Close()
		_ = gitBinding.Close()
		return LandingBlocked, fmt.Errorf("bound landing launch guard: %w", err)
	}
	sharedBound, err := w.SnapshotGitState(landingCtx)
	if err != nil || !observedGitStateEqual(sharedFinal, sharedBound) {
		_ = workTreeBinding.Close()
		_ = gitBinding.Close()
		return LandingBlocked, errors.New("bound landing launch guard: shared Git state changed and was preserved")
	}
	featureBound, err := w.snapshotFeatureControl(landingCtx)
	if err != nil || !featureControlEqual(featureFinal, featureBound) {
		_ = workTreeBinding.Close()
		_ = gitBinding.Close()
		return LandingBlocked, errors.New("bound landing launch guard: feature Git control changed and was preserved")
	}
	indexBefore, err := snapshotExactPath(w.featureIndexPath, indexByteLimit)
	if err != nil || !exactFileStableEqual(w.featureIndex, indexBefore) {
		_ = workTreeBinding.Close()
		_ = gitBinding.Close()
		return LandingBlocked, errors.New("bound landing launch guard: feature index changed and was preserved")
	}
	evidenceBefore, err := w.snapshotLandingEvidence()
	if err != nil {
		_ = workTreeBinding.Close()
		_ = gitBinding.Close()
		return LandingBlocked, fmt.Errorf("snapshot pre-merge evidence: %w", err)
	}
	bindingsClosed := false
	closeBindings := func() error {
		if bindingsClosed {
			return nil
		}
		bindingsClosed = true
		var seamErr error
		if w.beforeLandingBindingClose != nil {
			seamErr = w.beforeLandingBindingClose()
		}
		return errors.Join(seamErr, workTreeBinding.Close(), gitBinding.Close())
	}
	defer func() { _ = closeBindings() }()
	transactionEvidence, err := w.createLandingTransactionEvidence()
	if err != nil {
		return LandingBlocked, fmt.Errorf("create private landing transaction evidence: %w", errors.Join(err, closeBindings()))
	}
	defer func() { resultErr = errors.Join(resultErr, transactionEvidence.close()) }()
	if w.beforeLandingStart != nil {
		if err := w.beforeLandingStart(); err != nil {
			return LandingBlocked, fmt.Errorf("before landing start: %w", errors.Join(err, closeBindings()))
		}
	}
	launchControl, err := w.snapshotFeatureControl(landingCtx)
	if err != nil || !featureControlEqual(featureBound, launchControl) {
		return LandingBlocked, fmt.Errorf("landing launch control changed and was preserved: %w", errors.Join(err, closeBindings()))
	}
	attributeDrivers, err := w.landingAttributeDrivers(landingCtx, squash, transactionEvidence.private)
	if err != nil {
		return LandingBlocked, fmt.Errorf("resolve landing conversion filters: %w", errors.Join(err, closeBindings()))
	}
	landingConfig, err := landingCommandConfig(landingCtx, gitBinding, workTreeBinding, transactionEvidence.private.path, attributeDrivers)
	if err != nil {
		return LandingBlocked, fmt.Errorf("prepare landing command configuration: %w", errors.Join(err, closeBindings()))
	}
	launchControlAfter, err := w.snapshotFeatureControl(landingCtx)
	if err != nil || !featureControlEqual(launchControl, launchControlAfter) {
		return LandingBlocked, fmt.Errorf("landing launch control changed during filter isolation and was preserved: %w", errors.Join(err, closeBindings()))
	}
	_, mergeErr := gitcmd.OutputBoundDirectoriesWithEvidenceConfig(landingCtx, gitcmd.Hermetic, gitOutputLimit, gitBinding, workTreeBinding,
		transactionEvidence.writer, nil, landingConfig, "merge", "--ff-only", squash)
	transactionRaw, transactionErr, transactionCleanupErr := transactionEvidence.finish()
	mergeErr = errors.Join(mergeErr, transactionErr, transactionCleanupErr)
	_, indexAfterMergeErr := snapshotExactPath(w.featureIndexPath, indexByteLimit)
	mergeErr = errors.Join(mergeErr, indexAfterMergeErr)
	if w.afterLandingMerge != nil {
		mergeErr = errors.Join(mergeErr, w.afterLandingMerge())
	}
	evidenceAfter, evidenceSnapshotErr := w.snapshotLandingEvidence()
	actualHead, evidenceErr := authenticateLandingEvidence(transactionEvidence.nonce, transactionRaw, evidenceBefore, evidenceAfter, squash, "refs/heads/"+w.featureBranch)
	evidenceErr = errors.Join(transactionErr, evidenceSnapshotErr, evidenceErr)
	postFeature, postFeatureErr := w.snapshotFeatureControl(landingCtx)
	featureStable := postFeatureErr == nil && featureControlEqual(featureBound, postFeature)
	completeErr := w.verifyLandingResult(landingCtx, squash)
	if completeErr == nil && featureStable && evidenceErr == nil && actualHead == w.originalHead {
		w.landingComplete = true
		return LandingComplete, errors.Join(mergeErr, closeBindings())
	}
	// Once merge has launched, ambiguous or timed-out results receive a separate
	// bounded recovery window so cancellation cannot strand Git mid-recovery.
	newRecoveryContext := w.newLandingRecoveryContext
	if newRecoveryContext == nil {
		newRecoveryContext = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.WithoutCancel(parent), landingGitTimeout)
		}
	}
	recoveryCtx, recoveryCancel := newRecoveryContext(landingCtx)
	if recoveryCtx == nil || recoveryCancel == nil {
		return LandingBlocked, errors.New("landing recovery context is unavailable")
	}
	defer recoveryCancel()
	postFeature, postFeatureErr = w.snapshotFeatureControl(recoveryCtx)
	featureStable = postFeatureErr == nil && featureControlEqual(featureBound, postFeature)
	completeErr = w.verifyLandingResult(recoveryCtx, squash)
	if completeErr == nil && featureStable && evidenceErr == nil && actualHead == w.originalHead {
		w.landingComplete = true
		return LandingComplete, errors.Join(mergeErr, closeBindings())
	}
	if evidenceErr != nil {
		return LandingBlocked, fmt.Errorf("guarded fast-forward pre-merge evidence is incomplete or changed; preserve the run branch: %w", errors.Join(evidenceErr, closeBindings()))
	}
	if actualHead == w.originalHead {
		if intactErr := w.guardLanding(recoveryCtx, squash, w.originalHead); intactErr == nil {
			if mergeErr == nil {
				mergeErr = errors.New("fast-forward reported success without updating the feature checkout")
			}
			return LandingBlocked, fmt.Errorf("guarded fast-forward failed with original checkout intact: %w", errors.Join(mergeErr, closeBindings()))
		}
	}
	if w.beforeLandingRecovery != nil {
		if err := w.beforeLandingRecovery(); err != nil {
			return LandingBlocked, fmt.Errorf("guarded fast-forward recovery incomplete; preserve the run branch and inspect the feature checkout: %w", errors.Join(err, closeBindings()))
		}
	}
	if actualHead != w.originalHead || completeErr != nil {
		if closeErr := closeBindings(); closeErr != nil {
			return LandingBlocked, fmt.Errorf("guarded fast-forward recovery incomplete; automatic recovery cannot preserve concurrent ref, index, worktree, and ORIG_HEAD state atomically; descriptor closure failed: %w", closeErr)
		}
		return LandingBlocked, errors.New("guarded fast-forward recovery incomplete; automatic recovery cannot preserve concurrent ref, index, worktree, and ORIG_HEAD state atomically")
	}
	if intactErr := w.guardLanding(recoveryCtx, squash, w.originalHead); intactErr == nil {
		if mergeErr == nil {
			mergeErr = errors.New("fast-forward reported success without updating the feature checkout")
		}
		return LandingBlocked, fmt.Errorf("guarded fast-forward failed with original checkout intact: %w", errors.Join(mergeErr, closeBindings()))
	}
	if closeErr := closeBindings(); closeErr != nil {
		return LandingBlocked, fmt.Errorf("guarded fast-forward result is ambiguous and descriptor closure failed; preserve the run branch and inspect the feature checkout: %w", closeErr)
	}
	return LandingBlocked, errors.New("guarded fast-forward result is ambiguous; preserve the run branch and inspect the feature checkout")
}

func landingCommandConfig(ctx context.Context, gitDir, workTree *gitcmd.BoundDirectory, hooksPath string, attributeDrivers []string) (map[string]string, error) {
	output, err := gitcmd.OutputBoundDirectories(ctx, gitcmd.Hermetic, gitOutputLimit, gitDir, workTree,
		"config", "--null", "--name-only", "--list")
	if err != nil {
		return nil, err
	}
	config := map[string]string{"core.hooksPath": hooksPath}
	filterBases := make(map[string]struct{})
	for _, driver := range attributeDrivers {
		if driver == "" || strings.IndexByte(driver, 0) >= 0 {
			return nil, errors.New("landing filter attribute is malformed")
		}
		filterBases["filter."+driver] = struct{}{}
	}
	for _, rawKey := range bytes.Split(output, []byte{0}) {
		if len(rawKey) == 0 {
			continue
		}
		key := string(rawKey)
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "filter.") {
			continue
		}
		lastDot := strings.LastIndexByte(lower, '.')
		if lastDot <= len("filter.") {
			continue
		}
		switch lower[lastDot+1:] {
		case "clean", "smudge", "process", "required":
			filterBases[key[:lastDot]] = struct{}{}
		}
	}
	for base := range filterBases {
		config[base+".clean"] = ""
		config[base+".smudge"] = ""
		config[base+".process"] = ""
		config[base+".required"] = "false"
	}
	return config, nil
}

func (w *Workspace) landingAttributeDrivers(ctx context.Context, squash string, private *privateTempDir) ([]string, error) {
	index, indexPath, err := private.createFile("landing-index")
	if err != nil {
		return nil, err
	}
	info, statErr := index.Stat()
	closeErr := index.Close()
	if statErr != nil || closeErr != nil {
		return nil, errors.Join(statErr, closeErr)
	}
	name := filepath.Base(indexPath)
	private.removeFile(name, info)
	defer func() {
		if current, snapshotErr := private.snapshotFile(name, indexByteLimit); snapshotErr == nil {
			private.removeFile(name, current.info)
		}
	}()
	if _, err := gitcmd.OutputWithIndex(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit, indexPath,
		"--git-dir="+w.commonDir, "read-tree", squash); err != nil {
		return nil, err
	}
	paths, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "ls-tree", "-rz", "-r", "--name-only", squash)
	if err != nil {
		return nil, err
	}
	attributes, err := gitcmd.OutputWithIndexInput(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit, indexPath, bytes.NewReader(paths),
		"--git-dir="+w.commonDir, "check-attr", "--cached", "-z", "--stdin", "filter")
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(attributes, []byte{0})
	if len(fields) == 0 || len(fields)%3 != 1 || len(fields[len(fields)-1]) != 0 {
		return nil, errors.New("Git returned malformed filter attributes")
	}
	drivers := make(map[string]struct{})
	for index := 0; index+2 < len(fields); index += 3 {
		if string(fields[index+1]) != "filter" {
			return nil, errors.New("Git returned an unexpected attribute name")
		}
		value := string(fields[index+2])
		if value != "unspecified" && value != "unset" && value != "set" && value != "" {
			drivers[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(drivers))
	for driver := range drivers {
		result = append(result, driver)
	}
	sort.Strings(result)
	return result, nil
}

type landingEvidence struct {
	origHead exactFile
}

type landingTransactionEvidence struct {
	private          *privateTempDir
	reader           *os.File
	writer           *os.File
	nonce            string
	hook             exactFile
	finished         bool
	cleanupAttempted bool
}

func (w *Workspace) createLandingTransactionEvidence() (result *landingTransactionEvidence, resultErr error) {
	private, err := w.createPrivateTempDir()
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, private.discard())
		}
	}()
	private.beforeRemove = w.landingEvidenceBeforeRemove
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(random)
	hook, err := private.root.OpenFile("reference-transaction", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return nil, err
	}
	script := "#!/bin/sh\n" +
		"stage=$1\n" +
		"while IFS=' ' read -r old new ref extra; do\n" +
		"  test -z \"$extra\" || exit 97\n" +
		"  printf '" + nonce + "\\t%s\\t%s\\t%s\\t%s\\n' \"$stage\" \"$old\" \"$new\" \"$ref\" >&5 || exit 98\n" +
		"done\n"
	if _, err := io.WriteString(hook, script); err != nil {
		_ = hook.Close()
		return nil, err
	}
	if err := hook.Sync(); err != nil {
		_ = hook.Close()
		return nil, err
	}
	_, statErr := hook.Stat()
	closeErr := hook.Close()
	if statErr != nil || closeErr != nil {
		return nil, errors.Join(statErr, closeErr)
	}
	hookSnapshot, err := private.snapshotFile("reference-transaction", gitOutputLimit)
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &landingTransactionEvidence{private: private, reader: reader, writer: writer, nonce: nonce, hook: hookSnapshot}, nil
}

func (e *landingTransactionEvidence) finish() ([]byte, error, error) {
	if e == nil || e.finished {
		return nil, errors.New("landing transaction evidence is unavailable"), nil
	}
	e.finished = true
	writeErr := e.writer.Close()
	e.writer = nil
	raw, readErr := readBounded(e.reader, gitOutputLimit)
	readCloseErr := e.reader.Close()
	e.reader = nil
	if err := e.private.validatePathBinding(); err != nil {
		return nil, errors.Join(writeErr, readErr, readCloseErr, err), nil
	}
	hook, hookErr := e.private.snapshotFile("reference-transaction", gitOutputLimit)
	if hookErr != nil || !exactFileStableEqual(e.hook, hook) || hook.mode.Perm() != 0o700 {
		return nil, errors.Join(writeErr, readErr, readCloseErr, hookErr, errors.New("private landing hook changed")), nil
	}
	discardErr := e.private.discard()
	e.cleanupAttempted = true
	if discardErr == nil {
		e.private = nil
	}
	return raw, errors.Join(writeErr, readErr, readCloseErr), discardErr
}

func (e *landingTransactionEvidence) close() error {
	if e == nil {
		return nil
	}
	var result error
	if e.writer != nil {
		result = errors.Join(result, e.writer.Close())
		e.writer = nil
	}
	if e.reader != nil {
		result = errors.Join(result, e.reader.Close())
		e.reader = nil
	}
	if e.private != nil && !e.cleanupAttempted {
		result = errors.Join(result, e.private.discard())
		if result == nil {
			e.private = nil
		}
	}
	return result
}

func (w *Workspace) snapshotLandingEvidence() (landingEvidence, error) {
	origHeadPath := filepath.Join(w.featureGitDir, "ORIG_HEAD")
	if !pathWithin(origHeadPath, w.featureGitDir) {
		return landingEvidence{}, errors.New("landing evidence path escapes its bound Git directory")
	}
	first, err := snapshotExactPath(origHeadPath, gitOutputLimit)
	if err != nil {
		return landingEvidence{}, err
	}
	second, err := snapshotExactPath(origHeadPath, gitOutputLimit)
	if err != nil || !exactFileStableEqual(first, second) {
		return landingEvidence{}, errors.New("landing evidence changed while snapshotting")
	}
	return landingEvidence{origHead: second}, nil
}

func authenticateLandingEvidence(nonce string, transaction []byte, before, after landingEvidence, squash, featureRef string) (string, error) {
	if !after.origHead.exists || !after.origHead.mode.IsRegular() {
		return "", errors.New("ORIG_HEAD was not recorded as a regular file")
	}
	actual := strings.TrimSpace(string(after.origHead.bytes))
	if !validObjectID(actual) || strings.ContainsAny(string(after.origHead.bytes), " \t\r") {
		return "", errors.New("ORIG_HEAD is malformed")
	}
	_ = before
	state := 0
	for _, line := range bytes.Split(bytes.TrimSpace(transaction), []byte("\n")) {
		fields := bytes.Split(line, []byte("\t"))
		if len(fields) != 5 || string(fields[0]) != nonce {
			return "", errors.New("private transaction evidence is malformed or has the wrong nonce")
		}
		old, next, ref := string(fields[2]), string(fields[3]), string(fields[4])
		stage := string(fields[1])
		if stage != "preparing" && stage != "prepared" && stage != "committed" && stage != "aborted" {
			return "", fmt.Errorf("private transaction evidence has unknown stage %q", stage)
		}
		if ref != featureRef || stage == "preparing" {
			continue
		}
		if next != squash || old != actual {
			return "", errors.New("private transaction evidence contains a conflicting feature ref transition")
		}
		switch stage {
		case "prepared":
			if state != 0 {
				return "", fmt.Errorf("private feature transaction stage %q follows state %d", stage, state)
			}
			state = 1
		case "committed":
			if state != 1 {
				return "", fmt.Errorf("private feature transaction stage %q follows state %d", stage, state)
			}
			state = 2
		case "aborted":
			return "", errors.New("feature ref transaction aborted")
		}
	}
	if state != 2 {
		return "", errors.New("private transaction evidence is missing or duplicated")
	}
	return actual, nil
}

func (w *Workspace) guardLanding(ctx context.Context, squash, expectedHead string) error {
	if err := w.validateFeatureBinding(ctx, true); err != nil {
		return fmt.Errorf("feature checkout binding changed: %w", err)
	}
	if err := w.requireDirectRunRef(ctx, squash, false); err != nil {
		return fmt.Errorf("validated run branch changed: %w", err)
	}
	if err := w.verifySquashObject(ctx, squash, mustObjectTree(ctx, w, squash)); err != nil {
		return fmt.Errorf("squash object changed: %w", err)
	}
	headRef, err := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.featureGitDir, "--work-tree="+w.repositoryRoot, "symbolic-ref", "-q", "HEAD")
	if err != nil || headRef != "refs/heads/"+w.featureBranch {
		return errors.New("feature checkout is detached or on a different branch")
	}
	head, err := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.featureGitDir, "--work-tree="+w.repositoryRoot, "rev-parse", "HEAD")
	if err != nil || head != expectedHead {
		return errors.New("feature checkout HEAD moved")
	}
	ref, err := inspectRefStateAt(ctx, w.repositoryRoot, w.commonDir, "refs/heads/"+w.featureBranch)
	if err != nil || ref.symbolic != "" || ref.object != expectedHead {
		return errors.New("feature branch ref moved or became symbolic")
	}
	status, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.featureGitDir, "--work-tree="+w.repositoryRoot,
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return errors.New("inspect feature checkout status")
	}
	if len(status) != 0 {
		return errors.New("feature checkout is not clean, including untracked or ignored files")
	}
	return nil
}

func mustObjectTree(ctx context.Context, w *Workspace, commit string) string {
	tree, _ := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.commonDir, "rev-parse", commit+"^{tree}")
	return tree
}

func (w *Workspace) verifyLandingResult(ctx context.Context, squash string) error {
	if err := w.validateFeatureBinding(ctx, false); err != nil {
		return err
	}
	headRef, err := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.featureGitDir, "--work-tree="+w.repositoryRoot, "symbolic-ref", "-q", "HEAD")
	if err != nil || headRef != "refs/heads/"+w.featureBranch {
		return errors.New("feature checkout branch changed")
	}
	head, err := gitPath(ctx, w.repositoryRoot, "--git-dir="+w.featureGitDir, "--work-tree="+w.repositoryRoot, "rev-parse", "HEAD")
	if err != nil || head != squash {
		return errors.New("feature checkout did not reach squash")
	}
	ref, err := inspectRefStateAt(ctx, w.repositoryRoot, w.commonDir, "refs/heads/"+w.featureBranch)
	if err != nil || ref.symbolic != "" || ref.object != squash {
		return errors.New("feature branch did not reach squash")
	}
	status, err := gitcmd.Output(ctx, w.repositoryRoot, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.featureGitDir, "--work-tree="+w.repositoryRoot,
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil || len(status) != 0 {
		return errors.New("landed feature checkout is not clean")
	}
	return nil
}

func (w *Workspace) bindFeatureCheckout(ctx context.Context) error {
	rootInfo, err := noFollowDirectoryInfo(w.repositoryRoot)
	if err != nil {
		return err
	}
	dotGit, err := snapshotExactPath(filepath.Join(w.repositoryRoot, ".git"), gitOutputLimit)
	if err != nil || !dotGit.exists || dotGit.mode&os.ModeSymlink != 0 {
		return errors.New("feature .git control path is unavailable")
	}
	gitDir, err := gitPath(ctx, w.repositoryRoot, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return err
	}
	gitDir, err = canonicalExisting(gitDir)
	if err != nil {
		return err
	}
	gitDirInfo, err := noFollowDirectoryInfo(gitDir)
	if err != nil {
		return err
	}
	commonDir, err := gitPath(ctx, w.repositoryRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	commonDir, err = canonicalExisting(commonDir)
	if err != nil || commonDir != w.commonDir {
		return errors.New("feature checkout belongs to a different common Git directory")
	}
	indexPath, err := canonicalExisting(filepath.Join(gitDir, "index"))
	if err != nil {
		return errors.New("feature index is unavailable")
	}
	index, err := snapshotExactPath(indexPath, indexByteLimit)
	if err != nil || !index.exists || !index.mode.IsRegular() {
		return errors.New("feature index is unavailable")
	}
	w.featureRootInfo = rootInfo
	w.featureDotGit = dotGit
	w.featureGitDir = gitDir
	w.featureGitDirInfo = gitDirInfo
	w.featureIndexPath = indexPath
	w.featureIndex = index
	return w.validateFeatureBinding(ctx, true)
}

func (w *Workspace) validateFeatureBinding(ctx context.Context, bindIndex bool) error {
	rootInfo, err := noFollowDirectoryInfo(w.repositoryRoot)
	if err != nil || w.featureRootInfo == nil || !os.SameFile(w.featureRootInfo, rootInfo) {
		return errors.New("feature worktree root identity changed")
	}
	dotGit, err := snapshotExactPath(filepath.Join(w.repositoryRoot, ".git"), gitOutputLimit)
	if err != nil || !exactFileIdentityEqual(w.featureDotGit, dotGit) {
		return errors.New("feature .git control path changed")
	}
	gitDir, err := gitPath(ctx, w.repositoryRoot, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return errors.New("resolve bound feature Git directory")
	}
	gitDir, err = canonicalExisting(gitDir)
	if err != nil || gitDir != w.featureGitDir {
		return errors.New("feature Git directory changed")
	}
	gitDirInfo, err := noFollowDirectoryInfo(gitDir)
	if err != nil || !os.SameFile(w.featureGitDirInfo, gitDirInfo) {
		return errors.New("feature Git directory identity changed")
	}
	if bindIndex {
		indexPath, indexErr := canonicalExisting(filepath.Join(gitDir, "index"))
		index, snapshotErr := snapshotExactPath(indexPath, indexByteLimit)
		if indexErr != nil || snapshotErr != nil || indexPath != w.featureIndexPath || !exactFileIdentityEqual(w.featureIndex, index) {
			return errors.New("feature index identity or contents changed")
		}
	}
	if err := validateDirectoryIdentity(w.commonDir, w.commonInfo); err != nil {
		return errors.New("common Git directory identity changed")
	}
	registrations, err := worktreeRegistrationsSnapshotAt(ctx, w.repositoryRoot, w.commonDir)
	if err != nil {
		return errors.New("inspect feature worktree registration")
	}
	found := 0
	for _, registration := range registrations {
		if filepath.Clean(registration.path) == w.repositoryRoot {
			if registration.branch != "refs/heads/"+w.featureBranch {
				return errors.New("feature worktree registration branch changed")
			}
			found++
		}
	}
	if found != 1 {
		return errors.New("feature worktree registration is missing or ambiguous")
	}
	return nil
}

type featureControlSnapshot struct {
	configGraph []byte
	configs     map[string]exactFile
	hooks       map[string]exactFile
	control     map[string]exactFile
}

func (w *Workspace) snapshotFeatureControl(ctx context.Context) (featureControlSnapshot, error) {
	graph, configs, err := w.snapshotLocalConfigAt(ctx, w.repositoryRoot, w.featureGitDir, w.commonDir)
	if err != nil {
		return featureControlSnapshot{}, err
	}
	hooks, err := snapshotRawTrees([]rawTreeRoot{
		{name: "common", path: filepath.Join(w.commonDir, "hooks")},
		{name: "feature", path: filepath.Join(w.featureGitDir, "hooks")},
	}, defaultRawControlLimits)
	if err != nil {
		return featureControlSnapshot{}, err
	}
	control, err := snapshotRawControlFiles(w.featureGitDir, w.commonDir)
	if err != nil {
		return featureControlSnapshot{}, err
	}
	return featureControlSnapshot{configGraph: graph, configs: configs, hooks: hooks, control: control}, nil
}

func featureControlEqual(left, right featureControlSnapshot) bool {
	return bytes.Equal(left.configGraph, right.configGraph) &&
		exactFileStableMapsEqual(left.configs, right.configs) &&
		exactFileStableMapsEqual(left.hooks, right.hooks) &&
		exactFileStableMapsEqual(left.control, right.control)
}

// Cleanup removes the owned cache worktree before conditionally deleting its
// direct run ref. Unsafe or concurrent shared state is preserved and returned.
func (w *Workspace) Cleanup(ctx context.Context, disposition CleanupDisposition) error {
	if w == nil {
		return errors.New("workspace is required")
	}
	if ctx == nil {
		return errors.New("cleanup context is required")
	}
	if disposition != CleanupLanded && disposition != CleanupPreserveValidated {
		return errors.New("invalid cleanup disposition")
	}
	if disposition == CleanupLanded && !w.landingComplete {
		return errors.New("refuse landed cleanup without a verified successful landing")
	}
	if w.cleanupDisposition != "" && w.cleanupDisposition != disposition {
		return errors.New("cleanup disposition changed across attempts")
	}
	w.cleanupDisposition = disposition
	cleanupCtx, cancel := recoveryContext(ctx)
	defer cancel()

	if !w.cleanupWorktreeRemoved {
		if w.cleanupQuarantine == nil && !w.cleanupRegistrationRemoved {
			if disposition == CleanupPreserveValidated {
				if err := w.ResetAttempt(cleanupCtx); err != nil {
					return fmt.Errorf("reset invalid attempt before cleanup: %w", err)
				}
			} else if err := w.discardValidationSnapshot(nil); err != nil {
				return fmt.Errorf("discard validation snapshot before cleanup: %w", err)
			}
			if err := w.requireDirectRunRef(cleanupCtx, w.green, false); err != nil {
				return fmt.Errorf("refuse worktree cleanup: %w", err)
			}
			if w.beforeWorktreeRemove != nil {
				if err := w.beforeWorktreeRemove(); err != nil {
					return fmt.Errorf("before worktree cleanup: %w", err)
				}
			}
			if err := w.requireDirectRunRef(cleanupCtx, w.green, false); err != nil {
				return fmt.Errorf("refuse final worktree cleanup: %w", err)
			}
			if w.beforeWorktreeQuarantine != nil {
				if err := w.beforeWorktreeQuarantine(); err != nil {
					return fmt.Errorf("before worktree quarantine: %w", err)
				}
			}
			quarantine, err := w.quarantineOwnedWorktree()
			if err != nil {
				return fmt.Errorf("refuse worktree quarantine: %w", err)
			}
			w.cleanupQuarantine = quarantine
		}
		var removeErr error
		if !w.cleanupRegistrationRemoved {
			if w.beforeRegistrationRemove != nil {
				if err := w.beforeRegistrationRemove(); err != nil {
					return fmt.Errorf("before worktree registration removal: %w", err)
				}
			}
			removeWorktree := w.removeWorktree
			if removeWorktree == nil {
				removeWorktree = w.removeWorktreeDirect
			}
			removeErr = removeWorktree(cleanupCtx)
			if err := w.verifyWorktreeRemoved(cleanupCtx); err != nil {
				if removeErr != nil {
					return fmt.Errorf("remove cache worktree registration: %w", removeErr)
				}
				return err
			}
			w.cleanupRegistrationRemoved = true
		}
		if w.cleanupQuarantine != nil {
			if err := w.cleanupQuarantine.discard(); err != nil {
				return fmt.Errorf("remove quarantined cache worktree: %w", err)
			}
			w.cleanupQuarantine = nil
		}
		if err := w.verifyWorktreeRemoved(cleanupCtx); err != nil {
			return err
		}
		w.cleanupWorktreeRemoved = true
		if removeErr != nil {
			return fmt.Errorf("cache worktree registration removal succeeded with an ambiguous command result: %w", removeErr)
		}
		if w.afterWorktreeRemove != nil {
			if err := w.afterWorktreeRemove(); err != nil {
				return fmt.Errorf("after worktree cleanup: %w", err)
			}
		}
	} else if err := w.verifyWorktreeRemoved(cleanupCtx); err != nil {
		return err
	}

	preserve := disposition == CleanupPreserveValidated && w.green != w.originalHead
	if preserve {
		ref, err := inspectRefStateAt(cleanupCtx, w.commonDir, w.commonDir, "refs/heads/"+w.branch)
		if err != nil || ref.symbolic != "" || ref.object != w.green {
			return errors.New("validated run branch changed during cleanup")
		}
		w.cleanupPreserved = true
		return nil
	}
	if w.cleanupRunRefDeleted {
		ref, err := inspectRefStateAt(cleanupCtx, w.commonDir, w.commonDir, "refs/heads/"+w.branch)
		if err != nil || ref != (refState{}) {
			return errors.New("deleted run branch was recreated or became ambiguous")
		}
		return nil
	}
	refName := "refs/heads/" + w.branch
	ref, err := inspectRefStateAt(cleanupCtx, w.commonDir, w.commonDir, refName)
	if err != nil || ref.symbolic != "" || ref.object != w.green {
		return errors.New("refuse run branch cleanup: ref is symbolic, missing, or unexpected")
	}
	if w.beforeCleanupRefDelete != nil {
		if err := w.beforeCleanupRefDelete(); err != nil {
			return fmt.Errorf("before run branch cleanup: %w", err)
		}
	}
	if err := w.verifyWorktreeRemoved(cleanupCtx); err != nil {
		return err
	}
	if err := validateDirectoryIdentity(w.commonDir, w.commonInfo); err != nil {
		return errors.New("refuse run branch cleanup: common Git directory changed")
	}
	deleteRef := w.deleteRunRef
	if deleteRef == nil {
		deleteRef = w.deleteRunRefDirect
	}
	if err := deleteRef(cleanupCtx, refName, w.green); err != nil {
		observed, inspectErr := inspectRefStateAt(cleanupCtx, w.commonDir, w.commonDir, refName)
		switch {
		case inspectErr == nil && observed == (refState{}):
			w.cleanupRunRefDeleted = true
			return nil
		case inspectErr == nil && observed.symbolic == "" && observed.object == w.green:
			return errors.New("delete owned run branch by compare-and-swap")
		default:
			return errors.New("run branch deletion failed with unexpected concurrent state preserved")
		}
	}
	ref, err = inspectRefStateAt(cleanupCtx, w.commonDir, w.commonDir, refName)
	if err != nil || ref != (refState{}) {
		return errors.New("run branch deletion result is ambiguous")
	}
	w.cleanupRunRefDeleted = true
	return nil
}

func (w *Workspace) deleteRunRefDirect(ctx context.Context, ref, expected string) error {
	_, err := gitcmd.Output(ctx, w.commonDir, gitcmd.Hermetic, gitOutputLimit,
		"--git-dir="+w.commonDir, "-c", "core.hooksPath="+os.DevNull,
		"update-ref", "--no-deref", "-d", ref, expected)
	return err
}

func (w *Workspace) quarantineOwnedWorktree() (*privateTempDir, error) {
	if err := validateDirectoryIdentity(w.cacheRoot, w.cacheRootInfo); err != nil {
		return nil, errors.New("cache root binding changed")
	}
	parent, err := os.OpenRoot(w.cacheRoot)
	if err != nil {
		return nil, errors.New("open bound cache root")
	}
	closeParent := true
	defer func() {
		if closeParent {
			_ = parent.Close()
		}
	}()
	boundParent, boundErr := parent.Stat(".")
	namedParent, namedErr := noFollowDirectoryInfo(w.cacheRoot)
	if boundErr != nil || namedErr != nil || !os.SameFile(w.cacheRootInfo, boundParent) || !os.SameFile(w.cacheRootInfo, namedParent) {
		return nil, errors.New("cache root changed while opening quarantine")
	}
	ownedName := filepath.Base(w.path)
	owned, err := parent.Lstat(ownedName)
	if err != nil || w.rootInfo == nil || !owned.IsDir() || owned.Mode()&os.ModeSymlink != 0 || !os.SameFile(w.rootInfo, owned) {
		return nil, errors.New("owned worktree path identity changed")
	}
	quarantineName, err := randomPrivateName(".togi-remove")
	if err != nil {
		return nil, errors.New("allocate worktree quarantine name")
	}
	if err := parent.Rename(ownedName, quarantineName); err != nil {
		return nil, errors.New("atomically quarantine owned worktree")
	}
	quarantined, err := parent.Lstat(quarantineName)
	if err != nil || !quarantined.IsDir() || quarantined.Mode()&os.ModeSymlink != 0 || !os.SameFile(w.rootInfo, quarantined) {
		return nil, errors.New("quarantined path is not the owned worktree; preserved without deletion")
	}
	if _, err := parent.Lstat(ownedName); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("owned worktree path was concurrently recreated; quarantine preserved")
	}
	root, err := parent.OpenRoot(quarantineName)
	if err != nil {
		return nil, errors.New("open quarantined worktree")
	}
	bound, boundErr := root.Stat(".")
	named, namedErr := parent.Lstat(quarantineName)
	if boundErr != nil || namedErr != nil || !os.SameFile(w.rootInfo, bound) || !os.SameFile(w.rootInfo, named) {
		_ = root.Close()
		return nil, errors.New("quarantined worktree binding changed")
	}
	closeParent = false
	return &privateTempDir{
		path: filepath.Join(w.cacheRoot, quarantineName), name: quarantineName, info: named,
		parentPath: w.cacheRoot, parentInfo: w.cacheRootInfo, parent: parent, root: root,
	}, nil
}

func (w *Workspace) removeWorktreeDirect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.cleanupQuarantine == nil {
		return errors.New("owned worktree quarantine is required")
	}
	if err := validateDirectoryIdentity(w.commonDir, w.commonInfo); err != nil {
		return errors.New("common Git directory binding changed")
	}
	control, err := snapshotExactPath(filepath.Join(w.cleanupQuarantine.path, ".git"), gitOutputLimit)
	if err != nil || !control.exists || !control.mode.IsRegular() || w.dotGit.info == nil || !os.SameFile(w.dotGit.info, control.info) || !bytes.Equal(w.dotGit.bytes, control.bytes) {
		return errors.New("quarantined worktree .git binding changed")
	}
	linked, err := linkedGitDir(w.cleanupQuarantine.path)
	if err != nil || linked != w.gitDir {
		return errors.New("quarantined worktree registration binding changed")
	}
	parentPath := filepath.Dir(w.gitDir)
	parentInfo, err := noFollowDirectoryInfo(parentPath)
	if err != nil || !pathWithin(parentPath, w.commonDir) {
		return errors.New("linked worktree administration parent is unavailable")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return errors.New("open linked worktree administration parent")
	}
	name := filepath.Base(w.gitDir)
	admin, err := parent.Lstat(name)
	if err != nil || w.gitDirInfo == nil || !admin.IsDir() || admin.Mode()&os.ModeSymlink != 0 || !os.SameFile(w.gitDirInfo, admin) {
		_ = parent.Close()
		return errors.New("linked worktree administration identity changed")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		_ = parent.Close()
		return errors.New("open linked worktree administration directory")
	}
	bound, boundErr := root.Stat(".")
	named, namedErr := parent.Lstat(name)
	if boundErr != nil || namedErr != nil || !os.SameFile(w.gitDirInfo, bound) || !os.SameFile(w.gitDirInfo, named) {
		_ = root.Close()
		_ = parent.Close()
		return errors.New("linked worktree administration binding changed while opening")
	}
	owned := &privateTempDir{
		path: w.gitDir, name: name, info: named,
		parentPath: parentPath, parentInfo: parentInfo, parent: parent, root: root,
	}
	if err := owned.discard(); err != nil {
		return errors.New("remove exact linked worktree administration directory")
	}
	return nil
}

func (w *Workspace) verifyWorktreeRemoved(ctx context.Context) error {
	if err := validateDirectoryIdentity(w.commonDir, w.commonInfo); err != nil {
		return errors.New("cleanup common Git directory binding changed")
	}
	if _, err := os.Lstat(w.path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("cache worktree path remains or was replaced after removal")
	}
	registrations, err := worktreeRegistrationsSnapshotAt(ctx, w.commonDir, w.commonDir)
	if err != nil {
		return errors.New("inspect worktree registrations after cleanup")
	}
	for _, registration := range registrations {
		if filepath.Clean(registration.path) == w.path || registration.branch == "refs/heads/"+w.branch {
			return errors.New("cache worktree registration remains or run branch is still attached")
		}
	}
	return nil
}
