package flywheel

import (
	"context"
	"errors"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/finding"
)

const (
	maxChangedPackageFiles     = 4_096
	maxChangedPackagePathBytes = 4_096
	maxValidationFindings      = 10_000
	maxValidationOccurrences   = 100_000
	maxValidationEvidenceBytes = 32 << 20
	maxValidationGateErrors    = 8_192
	maxChangedPackageEntries   = 65_536
)

// GateValidation is normalized gate evidence for one attempted batch.
type GateValidation struct {
	Blocking []finding.Finding
	Errored  []string
}

// SuiteValidation is normalized local behavioral-suite evidence.
type SuiteValidation struct {
	Passed              bool
	InfrastructureError string
}

// AttemptValidator composes integrity, gate, progress, and local-suite checks.
type AttemptValidator struct {
	Original TreeSnapshot
	Baseline []finding.Finding
	// WaivedFingerprints removes only explicitly approved integrity findings.
	WaivedFingerprints map[string]struct{}

	RunGates    func(context.Context, string, Batch) GateValidation
	RunPackages func(context.Context, string, []string, bool) SuiteValidation
}

// Validate judges the actual repository changes made by one adapter attempt.
func (validator AttemptValidator) Validate(ctx context.Context, root string, changedFiles []string, batch Batch) ValidationResult {
	changed := slices.Clone(changedFiles)
	result := ValidationResult{}
	if ctx == nil {
		return validationFailure(result, ValidationInfrastructureFailure, "validation context is required", nil)
	}
	if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
		return stopped
	}
	if len(changed) == 0 {
		return validationFailure(result, ValidationSemanticFailure, "agent produced no worktree changes", nil)
	}
	if !batch.proof.present() {
		return validationFailure(result, ValidationInfrastructureFailure, "prepared batch proof is required", nil)
	}
	validationRoot := batch.proof.ValidationRoot()
	if validationRoot == "" {
		return validationFailure(result, ValidationInfrastructureFailure, "prepared validation snapshot is required", nil)
	}
	if err := ValidateModuleConfinement(validationRoot); err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "validation module is not confined", nil)
	}
	packages, full, err := ChangedGoPackages(validationRoot, changed)
	if err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "select changed Go packages: "+err.Error(), nil)
	}
	slices.Sort(changed)
	changed = slices.Compact(changed)
	result.ChangedFiles = changed
	if validator.RunGates == nil || validator.RunPackages == nil {
		return validationFailure(result, ValidationInfrastructureFailure, "attempt validator callbacks are required", nil)
	}
	if !validationEvidenceWithinBounds(validator.Baseline) {
		return validationFailure(result, ValidationInfrastructureFailure, "baseline blocker evidence exceeds resource limits", nil)
	}
	baseline, err := BlockingMultiset(validator.Baseline)
	if err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "validate baseline blocker evidence", nil)
	}
	if !validationEvidenceWithinBounds(batch.Findings) {
		return validationFailure(result, ValidationInfrastructureFailure, "assigned blocker evidence exceeds resource limits", nil)
	}
	assigned, err := BlockingMultiset(batch.Findings)
	if err != nil || len(assigned) == 0 {
		return validationFailure(result, ValidationInfrastructureFailure, "validate assigned blocker evidence", nil)
	}
	if !multisetSubset(assigned, baseline) {
		return validationFailure(result, ValidationInfrastructureFailure, "assigned blockers are absent from baseline evidence", nil)
	}

	attempted, err := SnapshotAttempt(root)
	if err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "snapshot attempted tree: "+err.Error(), nil)
	}
	if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
		return stopped
	}
	integrity := CheckIntegrity(cloneTreeSnapshot(validator.Original), cloneTreeSnapshot(attempted))
	if integrity.Err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "evaluate integrity evidence: "+integrity.Err.Error(), nil)
	}
	if len(integrity.Findings) != 0 {
		unwaived := integrity.Findings[:0]
		for _, item := range integrity.Findings {
			if _, waived := validator.WaivedFingerprints[item.Fingerprint]; !waived {
				unwaived = append(unwaived, item)
			}
		}
		if len(unwaived) != 0 {
			return validationFailure(result, ValidationSemanticFailure, "integrity validation found regressions", unwaived)
		}
	}
	if err := verifyPreparedBatchRecovering(ctx, batch.proof); err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "prepared batch invalid after integrity validation: "+err.Error(), nil)
	}
	if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
		return stopped
	}

	gates := validator.RunGates(ctx, validationRoot, cloneBatch(batch))
	if err := verifyPreparedBatchRecovering(ctx, batch.proof); err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "prepared batch invalid after gate validation: "+err.Error(), nil)
	}
	if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
		return stopped
	}
	if !validationEvidenceWithinBounds(gates.Blocking) || len(gates.Errored) > maxValidationGateErrors {
		return validationFailure(result, ValidationInfrastructureFailure, "gate validation evidence exceeds resource limits", nil)
	}
	blocking := cloneFindings(gates.Blocking)
	after, err := BlockingMultiset(blocking)
	if err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "gate validation returned malformed finding evidence", nil)
	}
	gateErrors, err := canonicalGateErrors(gates.Errored)
	if err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "gate validation returned malformed error evidence", nil)
	}
	if len(gateErrors) != 0 {
		return validationFailure(result, ValidationInfrastructureFailure, "gate validation errored: "+strings.Join(gateErrors, ", "), blocking)
	}
	for fingerprint := range assigned {
		if after[fingerprint] != 0 {
			return validationFailure(result, ValidationSemanticFailure, "assigned blocking finding remains", blocking)
		}
	}
	if !multisetSubset(after, baseline) {
		return validationFailure(result, ValidationSemanticFailure, "gate validation introduced replacement blockers", blocking)
	}
	if !StrictlyShrinks(after, baseline) {
		return validationFailure(result, ValidationSemanticFailure, "blocking finding multiset did not strictly shrink", blocking)
	}

	if full || len(packages) != 0 {
		if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
			return stopped
		}
		suite := validator.RunPackages(ctx, validationRoot, slices.Clone(packages), full)
		if err := verifyPreparedBatchRecovering(ctx, batch.proof); err != nil {
			return validationFailure(result, ValidationInfrastructureFailure, "prepared batch invalid after behavioral suite: "+err.Error(), nil)
		}
		if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
			return stopped
		}
		if strings.TrimSpace(suite.InfrastructureError) != "" {
			return validationFailure(result, ValidationInfrastructureFailure, "behavioral suite infrastructure failed", blocking)
		}
		if !suite.Passed {
			return validationFailure(result, ValidationSemanticFailure, "local behavioral suite failed", blocking)
		}
	}
	if err := verifyPreparedBatchRecovering(ctx, batch.proof); err != nil {
		return validationFailure(result, ValidationInfrastructureFailure, "prepared batch invalid at validation completion: "+err.Error(), nil)
	}
	if stopped := validationCancellation(ctx, result); stopped.Kind != "" {
		return stopped
	}
	result.Kind = ValidationPassed
	result.Findings = nil
	result.Proof = cloneBatchProof(batch.proof)
	return cloneValidationResult(result)
}

func verifyPreparedBatch(ctx context.Context, proof BatchProof) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("prepared batch verification context is canceled")
	}
	if !proof.present() {
		return errors.New("prepared batch proof is required")
	}
	return proof.verify(ctx, cloneBatchProof(proof))
}

func verifyPreparedBatchRecovering(ctx context.Context, proof BatchProof) error {
	recoveryCtx, cancelRecovery := recoveryContext(ctx)
	defer cancelRecovery()
	return verifyPreparedBatch(recoveryCtx, proof)
}

func validationCancellation(ctx context.Context, result ValidationResult) ValidationResult {
	if ctx.Err() == nil {
		return ValidationResult{}
	}
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return validationFailure(result, ValidationInfrastructureFailure, "attempt validation canceled: "+cause.Error(), nil)
}

func validationFailure(result ValidationResult, kind ValidationKind, failure string, findings []finding.Finding) ValidationResult {
	result.Kind = kind
	result.Failure = boundValidationFailure(failure)
	result.Findings = cloneFindings(findings)
	result.Proof = BatchProof{}
	return cloneValidationResult(result)
}

func boundValidationFailure(failure string) string {
	failure = strings.ToValidUTF8(strings.ReplaceAll(failure, "\x00", ""), "\uFFFD")
	if len(failure) <= maxBriefDiagnosticFieldBytes {
		return failure
	}
	limit := maxBriefDiagnosticFieldBytes - len(failureTruncationMarker)
	for limit > 0 && !utf8.RuneStart(failure[limit]) {
		limit--
	}
	return failure[:limit] + failureTruncationMarker
}

func cloneValidationResult(result ValidationResult) ValidationResult {
	result.Findings = cloneFindings(result.Findings)
	result.ChangedFiles = slices.Clone(result.ChangedFiles)
	result.Proof = cloneBatchProof(result.Proof)
	return result
}

func cloneTreeSnapshot(snapshot TreeSnapshot) TreeSnapshot {
	if snapshot.Files == nil {
		return TreeSnapshot{}
	}
	cloned := TreeSnapshot{Files: make(map[string][]byte, len(snapshot.Files))}
	for file, contents := range snapshot.Files {
		cloned.Files[file] = slices.Clone(contents)
	}
	return cloned
}

func multisetSubset(candidate, baseline map[string]int) bool {
	for fingerprint, count := range candidate {
		if count <= 0 || count > baseline[fingerprint] {
			return false
		}
	}
	return true
}

func validationEvidenceWithinBounds(items []finding.Finding) bool {
	if len(items) > maxValidationFindings {
		return false
	}
	occurrences, bytes := 0, 0
	for _, item := range items {
		if len(item.Occurrences) > maxValidationOccurrences-occurrences {
			return false
		}
		occurrences += len(item.Occurrences)
		for _, field := range []string{item.Gate, item.Language, item.RuleID, item.File, item.Fingerprint} {
			if len(field) > maxBriefIdentityFieldBytes || len(field) > maxValidationEvidenceBytes-bytes {
				return false
			}
			bytes += len(field)
		}
		for _, field := range []string{item.Snippet, item.Message} {
			if len(field) > maxBriefDiagnosticFieldBytes || len(field) > maxValidationEvidenceBytes-bytes {
				return false
			}
			bytes += len(field)
		}
	}
	return true
}

func canonicalGateErrors(errored []string) ([]string, error) {
	unique := make(map[string]struct{}, len(errored))
	for _, name := range errored {
		if name == "" || len(name) > maxBriefIdentityFieldBytes || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n") {
			return nil, errors.New("invalid gate error identity")
		}
		unique[name] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for name := range unique {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

// ChangedGoPackages returns the canonical packages affected by actual changed
// paths. Dependency metadata expands validation to the full module.
func ChangedGoPackages(root string, changedFiles []string) ([]string, bool, error) {
	return changedGoPackages(root, changedFiles, ValidationGoBuildContext())
}

// ValidationGoBuildContext is the explicit Go file-selection universe shared
// by package selection and behavioral-suite execution.
func ValidationGoBuildContext() build.Context {
	buildContext := build.Default
	buildContext.BuildTags = nil
	buildContext.ToolTags = make([]string, 0, len(build.Default.ToolTags)+3)
	for _, tag := range build.Default.ToolTags {
		if !validationPinnedToolTag(tag) {
			buildContext.ToolTags = append(buildContext.ToolTags, tag)
		}
	}
	_, _, featureTags := ValidationGoArchitecture()
	buildContext.ToolTags = append(buildContext.ToolTags, featureTags...)
	buildContext.ReleaseTags = slices.Clone(build.Default.ReleaseTags)
	return buildContext
}

// ValidationGoArchitecture returns the one pinned feature environment setting
// and cumulative build tags for the selected architecture.
func ValidationGoArchitecture() (string, string, []string) {
	switch build.Default.GOARCH {
	case "amd64":
		return "GOAMD64", "v1", []string{"amd64.v1"}
	case "386":
		return "GO386", "sse2", []string{"386.sse2"}
	case "arm":
		return "GOARM", "7", []string{"arm.5", "arm.6", "arm.7"}
	case "arm64":
		return "GOARM64", "v8.0", []string{"arm64.v8.0"}
	case "mips", "mipsle":
		return "GOMIPS", "hardfloat", []string{"mips.hardfloat"}
	case "mips64", "mips64le":
		return "GOMIPS64", "hardfloat", []string{"mips64.hardfloat"}
	case "ppc64", "ppc64le":
		return "GOPPC64", "power8", []string{"ppc64.power8"}
	case "riscv64":
		return "GORISCV64", "rva20u64", []string{"riscv64.rva20u64"}
	case "wasm":
		return "GOWASM", "", nil
	default:
		return "", "", nil
	}
}

func validationPinnedToolTag(tag string) bool {
	if strings.HasPrefix(tag, "goexperiment.") {
		return true
	}
	for _, prefix := range []string{"amd64.", "386.", "arm.", "arm64.", "mips.", "mips64.", "ppc64.", "riscv64.", "wasm."} {
		if strings.HasPrefix(tag, prefix) {
			return true
		}
	}
	return false
}

func changedGoPackages(root string, changedFiles []string, buildContext build.Context) ([]string, bool, error) {
	if strings.TrimSpace(root) == "" {
		return nil, false, errors.New("changed-package root is required")
	}
	if len(changedFiles) > maxChangedPackageFiles {
		return nil, false, errors.New("changed-package input exceeds file limit")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("changed-package root must be a directory, not a symbolic link")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, false, errors.New("resolve changed-package root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, false, errors.New("resolve changed-package root")
	}

	canonical := make([]string, len(changedFiles))
	full := false
	goChanged := false
	for index, file := range changedFiles {
		clean, err := canonicalChangedPath(file)
		if err != nil {
			return nil, false, err
		}
		base, directory := path.Base(clean), path.Dir(clean)
		if (base == "go.mod" || base == "go.sum") && directory != "." {
			return nil, false, errors.New("changed module marker belongs to a nested module")
		}
		if path.Ext(clean) == ".go" && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")) {
			return nil, false, errors.New("changed Go filename is ignored by the Go tool")
		}
		canonical[index] = clean
		full = full || clean == "go.mod" || clean == "go.sum"
		goChanged = goChanged || path.Ext(clean) == ".go"
		if err := validateChangedPath(rootAbs, resolvedRoot, clean); err != nil {
			return nil, false, err
		}
	}
	if full {
		return nil, true, nil
	}
	if goChanged {
		policy, err := readValidationModulePolicy(rootAbs)
		if err != nil {
			return nil, false, err
		}
		if policy.hasIgnore {
			return nil, true, nil
		}
	}

	unique := make(map[string]struct{})
	remainingEntries := maxChangedPackageEntries
	for _, clean := range canonical {
		if path.Ext(clean) != ".go" {
			continue
		}
		directory := path.Dir(clean)
		info, statErr := os.Lstat(filepath.Join(rootAbs, filepath.FromSlash(directory)))
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return nil, false, errors.New("inspect changed Go package")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(rootAbs, filepath.FromSlash(directory)))
		if err != nil || !withinResolvedRoot(resolvedRoot, resolved) {
			return nil, false, errors.New("changed Go package escapes the repository")
		}
		candidate := filepath.Join(resolved, filepath.FromSlash(path.Base(clean)))
		_, statErr = os.Lstat(candidate)
		exists := statErr == nil
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, false, errors.New("inspect changed Go file")
		}
		if exists {
			exists, err = activeGoFile(buildContext, resolved, path.Base(clean))
		} else {
			exists, err = survivingGoPackage(buildContext, resolved, &remainingEntries)
		}
		if err != nil {
			return nil, false, errors.New("inspect changed Go package")
		}
		if !exists {
			continue
		}
		pkg := "."
		if directory != "." {
			pkg = "./" + directory
		}
		unique[pkg] = struct{}{}
	}
	if len(unique) == 0 {
		return nil, false, nil
	}
	packages := make([]string, 0, len(unique))
	for pkg := range unique {
		packages = append(packages, pkg)
	}
	slices.Sort(packages)
	return packages, false, nil
}

func validateChangedPath(rootAbs, resolvedRoot, clean string) error {
	directory := path.Dir(clean)
	if excludedChangedDirectory(directory) {
		return errors.New("changed path is outside the active module package universe")
	}
	linked, err := changedPathHasSymlink(rootAbs, clean)
	if err != nil {
		return errors.New("inspect changed path")
	}
	if linked {
		return errors.New("changed path must not contain symbolic links")
	}
	if nestedModule(rootAbs, directory) {
		return errors.New("changed path belongs to a nested module")
	}
	candidate := filepath.Join(rootAbs, filepath.FromSlash(clean))
	if info, err := os.Lstat(candidate); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("changed path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect changed file")
	}
	resolvedDirectory, err := resolveNearestChangedPath(filepath.Join(rootAbs, filepath.FromSlash(directory)))
	if err != nil || !withinResolvedRoot(resolvedRoot, resolvedDirectory) {
		return errors.New("changed path escapes the repository")
	}
	return nil
}

func changedPathHasSymlink(root, clean string) (bool, error) {
	current := root
	for _, component := range strings.Split(clean, "/") {
		current = filepath.Join(current, filepath.FromSlash(component))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

func resolveNearestChangedPath(candidate string) (string, error) {
	for {
		if _, err := os.Lstat(candidate); err == nil {
			return filepath.EvalSymlinks(candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", os.ErrNotExist
		}
		candidate = parent
	}
}

func canonicalChangedPath(file string) (string, error) {
	if file == "" || len(file) > maxChangedPackagePathBytes || strings.TrimSpace(file) != file || strings.ContainsRune(file, 0) || strings.Contains(file, `\`) {
		return "", errors.New("invalid changed repository path")
	}
	if path.IsAbs(file) || strings.HasPrefix(file, "//") || windowsChangedPath(file) {
		return "", errors.New("changed path must be repository-relative")
	}
	clean := path.Clean(file)
	if clean != file || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || windowsChangedPath(clean) {
		return "", errors.New("changed path must be canonical and repository-relative")
	}
	return clean, nil
}

func windowsChangedPath(file string) bool {
	return len(file) >= 2 && ((file[0] >= 'a' && file[0] <= 'z') || (file[0] >= 'A' && file[0] <= 'Z')) && file[1] == ':'
}

func excludedChangedDirectory(directory string) bool {
	if directory == "." {
		return false
	}
	for _, component := range strings.Split(directory, "/") {
		if component == "vendor" || component == "testdata" || strings.HasPrefix(component, ".") || strings.HasPrefix(component, "_") {
			return true
		}
	}
	return false
}

func nestedModule(root, directory string) bool {
	for current := directory; current != "."; current = path.Dir(current) {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(current), "go.mod")); err == nil {
			return true
		}
	}
	return false
}

func survivingGoPackage(buildContext build.Context, directory string, remaining *int) (bool, error) {
	handle, err := os.Open(directory)
	if err != nil {
		return false, err
	}
	defer handle.Close()
	for {
		entries, readErr := handle.ReadDir(256)
		if len(entries) > *remaining {
			return false, errors.New("changed-package directory entry limit exceeded")
		}
		*remaining -= len(entries)
		for _, entry := range entries {
			name := entry.Name()
			if entry.Type().IsRegular() && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") && strings.HasSuffix(name, ".go") {
				active, matchErr := activeGoFile(buildContext, directory, name)
				if matchErr != nil {
					return false, matchErr
				}
				if active {
					return true, nil
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
	}
}

func activeGoFile(buildContext build.Context, directory, name string) (bool, error) {
	matched, err := buildContext.MatchFile(directory, name)
	if err != nil || !matched {
		return matched, err
	}
	if buildContext.CgoEnabled {
		return true, nil
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, name), nil, parser.ImportsOnly)
	if err != nil {
		return false, err
	}
	for _, imported := range parsed.Imports {
		if imported.Path.Value == `"C"` {
			return false, nil
		}
	}
	return true, nil
}

func withinResolvedRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
