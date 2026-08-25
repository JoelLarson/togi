package run

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/flywheel"
	gatepkg "github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/repoid"
	"github.com/joellarson/togi/internal/waiver"
)

var (
	// ErrClosed means the run ledger has already been closed.
	ErrClosed = errors.New("run ledger is closed")
	// ErrRunIDCollision means a generated run ID already exists.
	ErrRunIDCollision = errors.New("run ID already exists")
	// ErrNoCompleteRuns means the ledger contains no parseable report.
	ErrNoCompleteRuns = errors.New("no complete runs")
	// ErrReportExists means this run already has a completed report.
	ErrReportExists = errors.New("run report already exists")
	// ErrUninitialized means a RunLedger was not returned by Ledger.Start.
	ErrUninitialized = errors.New("run ledger is uninitialized")
)

var errIncompleteRun = errors.New("incomplete run")

const defaultRunRetention = 20

const rawOutputLimit = 1 << 20
const reportOutputLimit = 16 << 20

var rawTruncationMarker = []byte("\n[togi: output truncated]\n")

const runTimestampLayout = "20060102T150405.000000000Z"

var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{4}$`)
var fixBatchIDPattern = regexp.MustCompile(`^batch-[0-9a-f]{64}$`)

// Ledger manages run artifacts below one repository's external state path.
type Ledger struct {
	RepoID        string
	RepoState     string
	RunsDir       string
	Keep          int
	Now           func() time.Time
	Random        io.Reader
	syncDirectory func(*os.Root) error
}

// RunLedger is one active, exclusively locked run directory.
type RunLedger struct {
	Dir           string
	runID         string
	repoID        string
	lock          *stateLock
	repoRoot      *os.Root
	runsRoot      *os.Root
	runRoot       *os.Root
	rawRoot       *os.Root
	briefRoot     *os.Root
	adapterRoot   *os.Root
	mu            sync.Mutex
	marker        *RunLedger
	closing       bool
	closed        bool
	syncDirectory func(*os.Root) error
}

// PlanWriteError reports whether the exact supplied plan was published before
// persistence durability failed.
type PlanWriteError struct {
	Published bool
	Err       error
}

func (err *PlanWriteError) Error() string {
	if err == nil || err.Err == nil {
		return "write plan failed"
	}
	return err.Err.Error()
}

func (err *PlanWriteError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// PlanPublished reports whether plan.json contains the exact new plan.
func (err *PlanWriteError) PlanPublished() bool { return err != nil && err.Published }

type directoryBoundary struct {
	path     string
	identity os.FileInfo
}

// Start acquires the repository lock and creates a new run directory.
func (ledger Ledger) Start() (result *RunLedger, resultErr error) {
	syncDirectory := syncRootDirectory
	if ledger.syncDirectory != nil {
		syncDirectory = ledger.syncDirectory
	}
	if !repoid.ValidKey(ledger.RepoID) {
		return nil, errors.New("repository ID must be a full lowercase hexadecimal Git or SHA-256 identity")
	}
	if ledger.RepoState == "" {
		return nil, errors.New("repository state path is required")
	}
	if ledger.RunsDir == "" {
		return nil, errors.New("runs directory path is required")
	}
	runsName, err := ledger.runsName()
	if err != nil {
		return nil, err
	}
	if err := ensureLockPlatform(); err != nil {
		return nil, err
	}
	repoDirectory, err := ensureDirectory(ledger.RepoState)
	if err != nil {
		return nil, fmt.Errorf("prepare repository state: %w", err)
	}
	repoRoot, err := openBoundaryRoot(repoDirectory)
	if err != nil {
		return nil, fmt.Errorf("open repository state root: %w", err)
	}
	runsDirectory, err := ensureChildDirectoryAt(repoRoot, runsName, ledger.RunsDir)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("prepare runs directory: %w", err), repoRoot.Close())
	}
	if err := syncDirectory(repoRoot); err != nil {
		return nil, errors.Join(fmt.Errorf("sync repository state directory: %w", err), repoRoot.Close())
	}
	runsRoot, err := openChildBoundaryRoot(repoRoot, runsName, runsDirectory)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open runs root: %w", err), repoRoot.Close())
	}
	keepRoots := false
	defer func() {
		if !keepRoots {
			resultErr = errors.Join(resultErr, runsRoot.Close(), repoRoot.Close())
		}
	}()
	startedAt, lock, err := ledger.lockAndPrune(repoRoot, runsRoot, repoDirectory, runsDirectory)
	if err != nil {
		return nil, err
	}
	runID, err := ledger.newRunID(startedAt)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("generate run ID: %w", err), lock.release())
	}
	if err := validateDirectories(repoDirectory, runsDirectory); err != nil {
		return nil, errors.Join(err, lock.release())
	}
	runDir := filepath.Join(ledger.RunsDir, runID)
	runRoot, err := createChildRoot(runsRoot, runID)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.Join(fmt.Errorf("%w: %s", ErrRunIDCollision, runID), lock.release())
		}
		return nil, errors.Join(fmt.Errorf("create run directory: %w", err), lock.release())
	}
	var rawRoot, briefRoot, adapterRoot *os.Root
	cleanupRun := func(primary error) error {
		var cleanupErr error
		for _, root := range []struct {
			name string
			root *os.Root
		}{
			{name: "adapter", root: adapterRoot},
			{name: "briefs", root: briefRoot},
			{name: "raw", root: rawRoot},
			{name: "run", root: runRoot},
		} {
			if root.root == nil {
				continue
			}
			if err := root.root.Close(); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close partial %s root: %w", root.name, err))
			}
		}
		if err := runsRoot.RemoveAll(runID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove partial run directory: %w", err))
		}
		if err := syncDirectory(runsRoot); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync cleanup runs directory: %w", err))
		}
		return errors.Join(primary, cleanupErr, lock.release())
	}
	rawRoot, err = createChildRoot(runRoot, "raw")
	if err != nil {
		return nil, cleanupRun(fmt.Errorf("create raw directory: %w", err))
	}
	briefRoot, err = createChildRoot(runRoot, "briefs")
	if err != nil {
		return nil, cleanupRun(fmt.Errorf("create briefs directory: %w", err))
	}
	adapterRoot, err = createChildRoot(runRoot, "adapter")
	if err != nil {
		return nil, cleanupRun(fmt.Errorf("create adapter directory: %w", err))
	}
	if err := syncDirectory(runRoot); err != nil {
		return nil, cleanupRun(fmt.Errorf("sync run directory: %w", err))
	}
	if err := syncDirectory(runsRoot); err != nil {
		return nil, cleanupRun(fmt.Errorf("sync runs directory: %w", err))
	}
	keepRoots = true
	run := &RunLedger{
		Dir:           runDir,
		runID:         runID,
		repoID:        ledger.RepoID,
		lock:          lock,
		repoRoot:      repoRoot,
		runsRoot:      runsRoot,
		runRoot:       runRoot,
		rawRoot:       rawRoot,
		briefRoot:     briefRoot,
		adapterRoot:   adapterRoot,
		syncDirectory: syncDirectory,
	}
	run.marker = run
	return run, nil
}

func (ledger Ledger) lockAndPrune(repoRoot, runsRoot *os.Root, boundaries ...directoryBoundary) (time.Time, *stateLock, error) {
	now := time.Now
	if ledger.Now != nil {
		now = ledger.Now
	}
	startedAt := now().UTC()
	if err := validateDirectories(boundaries...); err != nil {
		return time.Time{}, nil, err
	}
	lock, err := acquireStateLock(repoRoot, startedAt)
	if err != nil {
		return time.Time{}, nil, err
	}
	keep := ledger.Keep
	if keep <= 0 {
		keep = defaultRunRetention
	}
	if err := pruneRuns(runsRoot, keep-1); err != nil {
		return time.Time{}, nil, errors.Join(err, lock.release())
	}
	return startedAt, lock, nil
}

func (ledger Ledger) newRunID(startedAt time.Time) (string, error) {
	random := ledger.Random
	if random == nil {
		random = rand.Reader
	}
	suffix := make([]byte, 2)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return "", err
	}
	return startedAt.Format(runTimestampLayout) + "-" + hex.EncodeToString(suffix), nil
}

func ensureDirectory(path string) (directoryBoundary, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return directoryBoundary{}, err
		}
		info, err = os.Lstat(path)
	}
	return existingDirectoryInfo(path, info, err)
}

func existingDirectory(path string) (directoryBoundary, error) {
	info, err := os.Lstat(path)
	return existingDirectoryInfo(path, info, err)
}

func ensureChildDirectoryAt(parent *os.Root, name, displayPath string) (directoryBoundary, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return directoryBoundary{}, err
		}
		info, err = parent.Lstat(name)
	}
	return existingChildDirectoryInfoAt(parent, name, displayPath, info, err)
}

func existingChildDirectoryAt(parent *os.Root, name, displayPath string) (directoryBoundary, error) {
	info, err := parent.Lstat(name)
	return existingChildDirectoryInfoAt(parent, name, displayPath, info, err)
}

func existingChildDirectoryInfoAt(
	parent *os.Root,
	name string,
	displayPath string,
	info os.FileInfo,
	err error,
) (directoryBoundary, error) {
	if err != nil {
		return directoryBoundary{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return directoryBoundary{}, fmt.Errorf("%s is not a real directory", displayPath)
	}
	directory, err := parent.Open(name)
	if err != nil {
		return directoryBoundary{}, err
	}
	defer func() { _ = directory.Close() }()
	opened, err := directory.Stat()
	if err != nil {
		return directoryBoundary{}, err
	}
	if !opened.IsDir() || !os.SameFile(info, opened) {
		return directoryBoundary{}, fmt.Errorf("%s changed while opening", displayPath)
	}
	if err := directory.Chmod(0o700); err != nil {
		return directoryBoundary{}, fmt.Errorf("restrict %s: %w", displayPath, err)
	}
	restricted, err := directory.Stat()
	if err != nil {
		return directoryBoundary{}, fmt.Errorf("verify permissions for %s: %w", displayPath, err)
	}
	if !privateDirectoryMode(restricted.Mode()) {
		return directoryBoundary{}, fmt.Errorf("permissions for %s are %04o, want 0700", displayPath, restricted.Mode().Perm())
	}
	current, err := parent.Lstat(name)
	if err != nil {
		return directoryBoundary{}, err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(restricted, current) {
		return directoryBoundary{}, fmt.Errorf("%s changed while restricting permissions", displayPath)
	}
	return directoryBoundary{path: displayPath, identity: restricted}, nil
}

func existingDirectoryInfo(path string, info os.FileInfo, err error) (directoryBoundary, error) {
	if err != nil {
		return directoryBoundary{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return directoryBoundary{}, fmt.Errorf("%s is not a real directory", path)
	}
	directory, err := os.Open(path)
	if err != nil {
		return directoryBoundary{}, err
	}
	defer func() { _ = directory.Close() }()
	opened, err := directory.Stat()
	if err != nil {
		return directoryBoundary{}, err
	}
	if !opened.IsDir() || !os.SameFile(info, opened) {
		return directoryBoundary{}, fmt.Errorf("%s changed while opening", path)
	}
	if err := directory.Chmod(0o700); err != nil {
		return directoryBoundary{}, fmt.Errorf("restrict %s: %w", path, err)
	}
	restricted, err := directory.Stat()
	if err != nil {
		return directoryBoundary{}, fmt.Errorf("verify permissions for %s: %w", path, err)
	}
	if !privateDirectoryMode(restricted.Mode()) {
		return directoryBoundary{}, fmt.Errorf("permissions for %s are %04o, want 0700", path, restricted.Mode().Perm())
	}
	current, err := os.Lstat(path)
	if err != nil {
		return directoryBoundary{}, err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(restricted, current) {
		return directoryBoundary{}, fmt.Errorf("%s changed while restricting permissions", path)
	}
	return directoryBoundary{path: path, identity: restricted}, nil
}

func (directory directoryBoundary) validate() error {
	current, err := os.Lstat(directory.path)
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(directory.identity, current) || !privateDirectoryMode(current.Mode()) {
		return fmt.Errorf("directory boundary %s changed", directory.path)
	}
	return nil
}

func validateDirectories(directories ...directoryBoundary) error {
	for _, directory := range directories {
		if err := directory.validate(); err != nil {
			return err
		}
	}
	return nil
}

func openBoundaryRoot(directory directoryBoundary) (*os.Root, error) {
	root, err := os.OpenRoot(directory.path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Lstat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(directory.identity, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("directory boundary %s changed while opening root", directory.path)
	}
	return root, nil
}

func openChildBoundaryRoot(parent *os.Root, name string, directory directoryBoundary) (*os.Root, error) {
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Lstat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(directory.identity, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("directory boundary %s changed while opening child root", directory.path)
	}
	return root, nil
}

func createChildRoot(parent *os.Root, name string) (*os.Root, error) {
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, err
	}
	created, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !created.IsDir() || created.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("created child %s was replaced", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if err := root.Chmod(".", 0o700); err != nil {
		_ = root.Close()
		return nil, err
	}
	info, err := root.Lstat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !info.IsDir() || !os.SameFile(created, info) || !privateDirectoryMode(info.Mode()) {
		_ = root.Close()
		return nil, fmt.Errorf("created directory %s is not private", name)
	}
	return root, nil
}

func openExistingChildRoot(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errIncompleteRun
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errIncompleteRun
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := root.Lstat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, errIncompleteRun
	}
	return root, nil
}

// Latest returns the newest complete, parseable report in the ledger.
func (ledger Ledger) Latest() (Report, error) {
	if !repoid.ValidKey(ledger.RepoID) {
		return Report{}, errors.New("repository ID must be a full lowercase hexadecimal Git or SHA-256 identity")
	}
	if ledger.RepoState == "" {
		return Report{}, errors.New("repository state path is required")
	}
	if ledger.RunsDir == "" {
		return Report{}, errors.New("runs directory path is required")
	}
	runsName, err := ledger.runsName()
	if err != nil {
		return Report{}, err
	}
	repoDirectory, err := existingDirectory(ledger.RepoState)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, ErrNoCompleteRuns
	} else if err != nil {
		return Report{}, fmt.Errorf("validate repository state: %w", err)
	}
	repoRoot, err := openBoundaryRoot(repoDirectory)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = repoRoot.Close() }()
	runsDirectory, err := existingChildDirectoryAt(repoRoot, runsName, ledger.RunsDir)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, ErrNoCompleteRuns
	} else if err != nil {
		return Report{}, fmt.Errorf("validate runs directory: %w", err)
	}
	if err := validateDirectories(repoDirectory, runsDirectory); err != nil {
		return Report{}, err
	}
	runsRoot, err := openChildBoundaryRoot(repoRoot, runsName, runsDirectory)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = runsRoot.Close() }()
	report, err := latestFromRunsRoot(runsRoot, ledger.RepoID)
	if err != nil {
		return Report{}, err
	}
	report.Ref = RunRef{ID: report.RunID, Dir: filepath.Join(ledger.RunsDir, report.RunID)}
	return report, nil
}

func latestFromRunsRoot(runsRoot *os.Root, repoID string) (Report, error) {
	entries, err := fs.ReadDir(runsRoot.FS(), ".")
	if err != nil {
		return Report{}, fmt.Errorf("read runs directory: %w", err)
	}
	runIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && validRunID(entry.Name()) {
			runIDs = append(runIDs, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(runIDs)))
	for _, runID := range runIDs {
		runRoot, err := openExistingChildRoot(runsRoot, runID)
		if errors.Is(err, errIncompleteRun) {
			continue
		}
		if err != nil {
			return Report{}, err
		}
		report, err := readCompleteReport(runRoot, runID, repoID)
		closeErr := runRoot.Close()
		if err == nil && closeErr != nil {
			return Report{}, closeErr
		}
		if errors.Is(err, errIncompleteRun) {
			continue
		}
		if err != nil {
			return Report{}, err
		}
		return report, nil
	}
	return Report{}, ErrNoCompleteRuns
}

func readCompleteReport(runRoot *os.Root, runID, repoID string) (Report, error) {
	const reportName = "report.json"
	before, err := runRoot.Lstat(reportName)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, errIncompleteRun
	}
	if err != nil {
		return Report{}, err
	}
	if !before.Mode().IsRegular() {
		return Report{}, errIncompleteRun
	}
	file, err := runRoot.Open(reportName)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil {
		return Report{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return Report{}, errIncompleteRun
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return Report{}, errIncompleteRun
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Report{}, errIncompleteRun
	}
	if err := validateReport(report, runID); err != nil {
		return Report{}, errIncompleteRun
	}
	if report.RepoID != repoID {
		return Report{}, errIncompleteRun
	}
	report.Ref = RunRef{ID: runID}
	return report, nil
}

func validateReport(report Report, runID string) error {
	if err := validateReportHeader(report, runID); err != nil {
		return err
	}
	flattened := make([]finding.Finding, 0)
	for index, gate := range report.Gates {
		if err := validateGateReport(gate, index); err != nil {
			return err
		}
		flattened = append(flattened, gate.Findings...)
		if index > 0 && report.Gates[index-1].Position == gate.Position {
			return errors.New("report gate positions are duplicated")
		}
		if index > 0 && compareGateReports(report.Gates[index-1], gate) >= 0 {
			return errors.New("report gates are duplicated or out of order")
		}
	}
	return validateReportSummary(report, flattened)
}

func validateReportHeader(report Report, runID string) error {
	if report.SchemaVersion != ReportSchemaVersion {
		return fmt.Errorf("unsupported report schema version %d", report.SchemaVersion)
	}
	if report.RunID != runID {
		return fmt.Errorf("report run ID %q does not match ledger run ID %q", report.RunID, runID)
	}
	if !repoid.ValidKey(report.RepoID) {
		return errors.New("report repository ID must be a full lowercase hexadecimal Git or SHA-256 identity")
	}
	if report.StartedAt.IsZero() || report.FinishedAt.IsZero() {
		return errors.New("report timestamps are required")
	}
	if report.Verdict != VerdictUnverified && report.Verdict != VerdictFindings && report.Verdict != VerdictBlocked && report.Verdict != VerdictRails && report.Verdict != VerdictErrored && report.Verdict != VerdictUnsealed {
		return fmt.Errorf("invalid verdict %q", report.Verdict)
	}
	if report.Fix == nil {
		if report.Verdict == VerdictBlocked || report.Verdict == VerdictRails || report.Verdict == VerdictUnsealed {
			return fmt.Errorf("verdict %q requires a fix report", report.Verdict)
		}
	} else {
		if report.Verdict == VerdictFindings {
			return errors.New("report-only findings verdict cannot contain a fix report")
		}
		if err := validateFixReport(*report.Fix, report.Diff, report.Verdict, report.Gates, report.RunID); err != nil {
			return fmt.Errorf("invalid fix report: %w", err)
		}
	}
	if report.FinishedAt.Before(report.StartedAt) {
		return errors.New("report finished before it started")
	}
	if len(report.Gates) == 0 {
		return errors.New("report must contain at least one gate")
	}
	if report.Findings == nil {
		return errors.New("report findings array is required")
	}
	if report.Waivers == nil {
		return errors.New("report waivers array is required")
	}
	if err := validateReportWaivers(report.Waivers); err != nil {
		return err
	}
	return validateDiffReport(report.Diff)
}

func validateReportWaivers(records []waiver.Record) error {
	seen := make(map[string]struct{}, len(records))
	for index, record := range records {
		if !finding.ValidFingerprint(record.Fingerprint) {
			return fmt.Errorf("waiver %d fingerprint is invalid", index)
		}
		if strings.TrimSpace(record.Reason) == "" || strings.ContainsAny(record.Reason, "\x00\r\n") {
			return fmt.Errorf("waiver %d reason is invalid", index)
		}
		if record.ApprovedAt.IsZero() {
			return fmt.Errorf("waiver %d approval time is required", index)
		}
		if _, exists := seen[record.Fingerprint]; exists {
			return fmt.Errorf("waiver %d fingerprint is duplicated", index)
		}
		seen[record.Fingerprint] = struct{}{}
	}
	return nil
}

func (ledger Ledger) runsName() (string, error) {
	repoState := filepath.Clean(ledger.RepoState)
	runsDir := filepath.Clean(ledger.RunsDir)
	if filepath.Dir(runsDir) != repoState {
		return "", errors.New("runs directory must be a direct child of repository state")
	}
	return filepath.Base(runsDir), nil
}

func validateDiffReport(diff DiffReport) error {
	if err := validateRequestedBase(diff.BaseRef); err != nil {
		return fmt.Errorf("report diff base ref is invalid: %w", err)
	}
	for _, character := range diff.BaseRef {
		if unicode.IsControl(character) {
			return errors.New("report diff base ref contains a control character")
		}
	}
	objectIDs := []struct {
		name  string
		value string
	}{
		{name: "base commit", value: diff.BaseCommit},
		{name: "merge base", value: diff.MergeBase},
		{name: "head", value: diff.Head},
	}
	for _, objectID := range objectIDs {
		if !validObjectIDString(objectID.value) {
			return fmt.Errorf("report diff %s is invalid", objectID.name)
		}
	}
	if len(diff.BaseCommit) != len(diff.MergeBase) || len(diff.BaseCommit) != len(diff.Head) {
		return errors.New("report diff object IDs have inconsistent lengths")
	}
	if diff.ChangedFiles < 0 || diff.ChangedLines < 0 {
		return errors.New("report diff counts cannot be negative")
	}
	if diff.ChangedFiles == 0 && diff.ChangedLines != 0 {
		return errors.New("report diff with no changed files cannot contain changed lines")
	}
	return nil
}

func validObjectIDString(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character < '0' || (character > '9' && character < 'a') || character > 'f' {
			return false
		}
	}
	return true
}

func validateReportSummary(report Report, flattened []finding.Finding) error {
	for index, item := range report.Findings {
		if err := finding.Validate(item); err != nil {
			return fmt.Errorf("finding %d: %w", index, err)
		}
	}
	grouped, err := finding.Group(flattened)
	if err != nil || !equalFindings(grouped, report.Findings) {
		return errors.New("top-level findings do not match gate findings")
	}
	if counts := countFindings(report.Findings); counts != report.Counts {
		return fmt.Errorf("report counts are inconsistent: got %+v, want %+v", report.Counts, counts)
	}
	if verdict := verdictFor(report.Gates); report.Fix == nil && verdict != report.Verdict {
		return fmt.Errorf("report verdict %q is inconsistent with %q", report.Verdict, verdict)
	}
	return nil
}

func validateFixReport(fix FixReport, diff DiffReport, verdict Verdict, gates []GateReport, runID string) error {
	if !validObjectIDString(fix.OriginalHead) || fix.OriginalHead != diff.Head {
		return errors.New("original HEAD must match the report diff HEAD")
	}
	if (strings.TrimSpace(fix.FeatureBranch) == "" && verdict != VerdictErrored) || strings.ContainsAny(fix.FeatureBranch, "\x00\r\n") {
		return errors.New("feature branch is required")
	}
	if strings.TrimSpace(fix.Agent.Name) == "" || strings.ContainsAny(fix.Agent.Name, "\x00\r\n") {
		return errors.New("agent name is required")
	}
	if fix.Agent.Usage != nil && (fix.Agent.Usage.InputTokens < 0 || fix.Agent.Usage.CachedInputTokens < 0 || fix.Agent.Usage.OutputTokens < 0 || fix.Agent.Usage.CachedInputTokens > fix.Agent.Usage.InputTokens) {
		return errors.New("agent usage cannot be negative")
	}
	if err := validateSuiteReport(fix.Baseline); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if fix.Final != nil {
		if err := validateSuiteReport(*fix.Final); err != nil {
			return fmt.Errorf("final: %w", err)
		}
	}
	if fix.Rails.MaxIterations <= 0 || fix.Rails.Iterations < 0 || fix.Rails.Iterations > fix.Rails.MaxIterations || fix.Rails.MaxWallClockMS <= 0 || fix.Rails.ElapsedMS < 0 {
		return errors.New("invalid rail accounting")
	}
	if verdict == VerdictRails && fix.Rails.Iterations < fix.Rails.MaxIterations && fix.Rails.ElapsedMS < fix.Rails.MaxWallClockMS {
		return errors.New("rails verdict lacks exhausted rail evidence")
	}
	if fix.Batches == nil || fix.Integrity == nil {
		return errors.New("batches and integrity arrays are required")
	}
	if err := validateFixBatches(fix.Batches, len(diff.Head)); err != nil {
		return err
	}
	for index, item := range fix.Integrity {
		if err := finding.Validate(item); err != nil {
			return fmt.Errorf("integrity finding %d: %w", index, err)
		}
		if item.Gate != "integrity" {
			return fmt.Errorf("integrity finding %d has a non-integrity gate", index)
		}
	}
	groupedIntegrity, err := finding.Group(fix.Integrity)
	if err != nil || !equalFindings(groupedIntegrity, fix.Integrity) {
		return errors.New("integrity findings are duplicated or non-canonical")
	}
	switch fix.Landing.Status {
	case string(flywheel.LandingNotNeeded), string(flywheel.LandingComplete), string(flywheel.LandingBlocked):
	default:
		return errors.New("invalid landing status")
	}
	if fix.Landing.Commit != "" && (!validObjectIDString(fix.Landing.Commit) || len(fix.Landing.Commit) != len(diff.Head)) {
		return errors.New("invalid landing commit")
	}
	if strings.ContainsAny(fix.Landing.PreservedBranch, "\x00\r\n") || strings.ContainsAny(fix.Landing.Error, "\x00\r\n") {
		return errors.New("landing text contains a control character")
	}
	if fix.Landing.Status == string(flywheel.LandingComplete) && (fix.Landing.Commit == "" || fix.Landing.PreservedBranch != "") {
		return errors.New("completed landing requires a commit and cannot preserve a branch")
	}
	if fix.Landing.Status == string(flywheel.LandingNotNeeded) && (fix.Landing.Commit != "" || fix.Landing.PreservedBranch != "" || fix.Landing.Error != "") {
		return errors.New("unnecessary landing contains contradictory evidence")
	}
	if fix.Landing.Status == string(flywheel.LandingNotNeeded) && verdict != VerdictUnsealed {
		return errors.New("unnecessary landing requires an unsealed verdict")
	}
	if fix.Landing.Status == string(flywheel.LandingComplete) && verdict != VerdictUnsealed && verdict != VerdictErrored {
		return errors.New("completed landing has an invalid verdict")
	}
	if fix.Landing.PreservedBranch != "" && (fix.Landing.Status != string(flywheel.LandingBlocked) || fix.Landing.PreservedBranch != "togi/run-"+runID) {
		return errors.New("preserved branch contradicts landing evidence")
	}
	if gatesErrored(gates) && verdict != VerdictErrored {
		return errors.New("errored gates require an errored verdict")
	}
	switch fix.Baseline.Status {
	case SuiteMissing, SuiteFailed:
		if verdict != VerdictUnverified && !gatesErrored(gates) {
			return errors.New("unverified baseline requires an unverified verdict")
		}
	case SuiteErrored:
		if verdict != VerdictErrored {
			return errors.New("errored baseline requires an errored verdict")
		}
	}
	if fix.Final != nil {
		switch fix.Final.Status {
		case SuiteFailed:
			if verdict != VerdictBlocked && verdict != VerdictErrored {
				return errors.New("failed final suite requires a blocked or cleanup-errored verdict")
			}
		case SuiteMissing:
			if verdict != VerdictErrored {
				return errors.New("unavailable final suite requires an errored verdict")
			}
		case SuiteErrored:
			if verdict != VerdictErrored && verdict != VerdictRails {
				return errors.New("errored final suite requires an errored or rails verdict")
			}
		}
	}
	if verdict == VerdictUnsealed {
		if fix.Baseline.Status != SuitePassed || len(blockingFindings(gates)) != 0 || len(fix.Integrity) != 0 || (fix.Final != nil && fix.Final.Status != SuitePassed) || (fix.Landing.Status != string(flywheel.LandingComplete) && fix.Landing.Status != string(flywheel.LandingNotNeeded)) {
			return errors.New("unsealed verdict contradicts fix evidence")
		}
	}
	if len(fix.Integrity) != 0 && verdict != VerdictBlocked && verdict != VerdictErrored {
		return errors.New("integrity findings require a blocked or errored verdict")
	}
	if verdict == VerdictErrored && fix.Landing.Status != string(flywheel.LandingBlocked) && fix.Landing.Status != string(flywheel.LandingComplete) {
		return errors.New("errored verdict has an invalid landing status")
	}
	if (verdict == VerdictBlocked || verdict == VerdictRails || verdict == VerdictUnverified) && fix.Landing.Status != string(flywheel.LandingBlocked) {
		return errors.New("non-success verdict has an invalid landing status")
	}
	return nil
}

func validateFixBatches(batches []flywheel.Batch, objectIDLength int) error {
	seen := make(map[string]struct{}, len(batches))
	for index, batch := range batches {
		if !fixBatchIDPattern.MatchString(batch.ID) {
			return fmt.Errorf("batch %d has an invalid ID", index)
		}
		if _, exists := seen[batch.ID]; exists {
			return fmt.Errorf("batch %d duplicates an ID", index)
		}
		seen[batch.ID] = struct{}{}
		parsed, err := finding.ParsePath(batch.PrimaryFile)
		if err != nil || parsed.String() != batch.PrimaryFile {
			return fmt.Errorf("batch %d has an invalid primary file", index)
		}
		if len(batch.Findings) == 0 || batch.Attempts == nil {
			return fmt.Errorf("batch %d arrays are required", index)
		}
		for findingIndex, item := range batch.Findings {
			if err := finding.Validate(item); err != nil || item.File != batch.PrimaryFile {
				return fmt.Errorf("batch %d finding %d is invalid", index, findingIndex)
			}
		}
		grouped, err := finding.Group(batch.Findings)
		if err != nil || !equalFindings(grouped, batch.Findings) {
			return fmt.Errorf("batch %d findings are not canonical", index)
		}
		switch batch.Status {
		case flywheel.BatchPending, flywheel.BatchRunning, flywheel.BatchDone, flywheel.BatchStuck:
		default:
			return fmt.Errorf("batch %d has an invalid status", index)
		}
		if len(batch.Attempts) > 2 {
			return fmt.Errorf("batch %d has too many attempts", index)
		}
		for attemptIndex, attempt := range batch.Attempts {
			if attempt.Number != attemptIndex+1 || (attempt.Status != "running" && attempt.Status != "failed" && attempt.Status != "passed") {
				return fmt.Errorf("batch %d attempt %d is invalid", index, attemptIndex)
			}
			if attempt.Commit != "" && (!validObjectIDString(attempt.Commit) || len(attempt.Commit) != objectIDLength) {
				return fmt.Errorf("batch %d attempt %d has an invalid commit", index, attemptIndex)
			}
			switch attempt.Status {
			case "running":
				if attempt.Failure != "" || len(attempt.ChangedFiles) != 0 || attempt.Commit != "" {
					return fmt.Errorf("batch %d attempt %d has contradictory running evidence", index, attemptIndex)
				}
			case "failed":
				if strings.TrimSpace(attempt.Failure) == "" || len(attempt.ChangedFiles) != 0 || attempt.Commit != "" {
					return fmt.Errorf("batch %d attempt %d has contradictory failure evidence", index, attemptIndex)
				}
			case "passed":
				if attempt.Failure != "" || len(attempt.ChangedFiles) == 0 || !validObjectIDString(attempt.Commit) {
					return fmt.Errorf("batch %d attempt %d has contradictory success evidence", index, attemptIndex)
				}
			}
			if !slices.IsSorted(attempt.ChangedFiles) || len(slices.Compact(slices.Clone(attempt.ChangedFiles))) != len(attempt.ChangedFiles) {
				return fmt.Errorf("batch %d attempt %d changed files are not canonical", index, attemptIndex)
			}
			for _, file := range attempt.ChangedFiles {
				parsed, err := finding.ParsePath(file)
				if err != nil || parsed.String() != file {
					return fmt.Errorf("batch %d attempt %d has an invalid changed file", index, attemptIndex)
				}
			}
		}
		if batch.Status == flywheel.BatchPending && len(batch.Attempts) != 0 {
			return fmt.Errorf("batch %d pending status has attempts", index)
		}
		if batch.Status == flywheel.BatchRunning && len(batch.Attempts) == 0 {
			return fmt.Errorf("batch %d running status lacks an attempt", index)
		}
		if batch.Status == flywheel.BatchRunning && batch.Attempts[len(batch.Attempts)-1].Status == "passed" {
			return fmt.Errorf("batch %d running status follows a passed attempt", index)
		}
		if len(batch.Attempts) == 2 && batch.Attempts[0].Status != "failed" {
			return fmt.Errorf("batch %d second attempt is unreachable", index)
		}
		if batch.Status == flywheel.BatchDone && (len(batch.Attempts) == 0 || batch.Attempts[len(batch.Attempts)-1].Status != "passed") {
			return fmt.Errorf("batch %d completed status lacks a passed attempt", index)
		}
		if batch.Status == flywheel.BatchStuck && (len(batch.Attempts) != 2 || batch.Attempts[1].Status != "failed") {
			return fmt.Errorf("batch %d stuck status lacks two failed attempts", index)
		}
	}
	return nil
}

func validateSuiteReport(result SuiteResult) error {
	if result.DurationMS < 0 {
		return errors.New("negative duration")
	}
	switch result.Status {
	case SuitePassed, SuiteFailed, SuiteMissing, SuiteErrored:
		return nil
	default:
		return errors.New("invalid status")
	}
}

func validateGateReport(gate GateReport, index int) error {
	if strings.TrimSpace(gate.Gate) == "" || strings.TrimSpace(gate.Language) == "" {
		return fmt.Errorf("gate %d name and language are required", index)
	}
	if gate.DurationMS < 0 {
		return fmt.Errorf("gate %d has negative duration", index)
	}
	if gate.Position < 0 {
		return fmt.Errorf("gate %d has negative position", index)
	}
	if gate.Blocking == nil {
		return fmt.Errorf("gate %d blocking severities are required", index)
	}
	seenBlocking := make(map[finding.Severity]struct{}, len(gate.Blocking))
	for _, severity := range gate.Blocking {
		if severity != finding.Error && severity != finding.Warning && severity != finding.Info {
			return fmt.Errorf("gate %d has invalid blocking severity %q", index, severity)
		}
		if _, exists := seenBlocking[severity]; exists {
			return fmt.Errorf("gate %d has duplicate blocking severity %q", index, severity)
		}
		seenBlocking[severity] = struct{}{}
	}
	switch gate.FixPolicy {
	case gatepkg.AutofixOnly, gatepkg.AutofixThenLLM, gatepkg.LLMFix, gatepkg.ReportOnly:
	default:
		return fmt.Errorf("gate %d has invalid fix policy %q", index, gate.FixPolicy)
	}
	if err := validateGateStatus(gate, index); err != nil {
		return err
	}
	for findingIndex, item := range gate.Findings {
		if err := finding.Validate(item); err != nil {
			return fmt.Errorf("gate %d finding %d: %w", index, findingIndex, err)
		}
		if item.Gate != gate.Gate || item.Language != gate.Language {
			return fmt.Errorf("gate %d finding %d ownership is inconsistent", index, findingIndex)
		}
	}
	if len(gate.Findings) > 0 {
		grouped, err := finding.Group(gate.Findings)
		if err != nil || !equalFindings(grouped, gate.Findings) {
			return fmt.Errorf("gate %d findings are not deterministically grouped", index)
		}
	}
	return nil
}

func validateGateStatus(gate GateReport, index int) error {
	switch gate.Status {
	case GatePassed:
		if len(gate.Findings) == 0 && gate.Error == "" {
			return nil
		}
	case GateFindings:
		if len(gate.Findings) > 0 && gate.Error == "" {
			return nil
		}
	case GateErrored:
		if len(gate.Findings) == 0 && strings.TrimSpace(gate.Error) != "" {
			return nil
		}
	default:
		return fmt.Errorf("gate %d has invalid status %q", index, gate.Status)
	}
	return fmt.Errorf("gate %d %s status is inconsistent", index, gate.Status)
}

func equalFindings(left, right []finding.Finding) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		leftItem, rightItem := left[index], right[index]
		leftOccurrences, rightOccurrences := leftItem.Occurrences, rightItem.Occurrences
		leftItem.Occurrences, rightItem.Occurrences = nil, nil
		if !reflect.DeepEqual(leftItem, rightItem) || !slices.Equal(leftOccurrences, rightOccurrences) {
			return false
		}
	}
	return true
}

func pruneRuns(runsRoot *os.Root, retain int) error {
	entries, err := fs.ReadDir(runsRoot.FS(), ".")
	if err != nil {
		return fmt.Errorf("read runs directory: %w", err)
	}
	valid := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validRunID(entry.Name()) {
			continue
		}
		valid = append(valid, entry.Name())
	}
	if len(valid) <= retain {
		return nil
	}
	sort.Strings(valid)
	for _, name := range valid[:len(valid)-retain] {
		info, err := runsRoot.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect old run %s: %w", name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := runsRoot.RemoveAll(name); err != nil {
			return fmt.Errorf("prune old run %s: %w", name, err)
		}
	}
	return nil
}

func validRunID(runID string) bool {
	if !runIDPattern.MatchString(runID) {
		return false
	}
	_, err := time.Parse(runTimestampLayout, runID[:26])
	return err == nil
}

func createRootTemp(root *os.Root, prefix string) (*os.File, string, error) {
	for range 10 {
		suffix := make([]byte, 8)
		if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
			return nil, "", err
		}
		name := "." + prefix + "-" + hex.EncodeToString(suffix) + ".tmp"
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("temporary file name collisions")
}

func publishNoReplace(root *os.Root, source, destination string) error {
	if err := root.Link(source, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrReportExists
		}
		return err
	}
	return nil
}

// WriteReportAudit atomically persists the validated pre-cleanup report. It is
// an immutable audit checkpoint, not a completed run visible through Latest.
func (run *RunLedger) WriteReportAudit(report Report) error {
	return run.writeReport(report, "report.pre-cleanup.json")
}

// WriteReport atomically persists the completed report.
func (run *RunLedger) WriteReport(report Report) error {
	return run.writeReport(report, "report.json")
}

func (run *RunLedger) writeReport(report Report, reportName string) error {
	if run == nil || run.marker != run {
		return ErrUninitialized
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || run.closing {
		return ErrClosed
	}
	var err error
	report, err = prepareReport(report, run.runID, run.repoID)
	if err != nil {
		return err
	}
	if _, err := run.runRoot.Lstat(reportName); err == nil {
		return ErrReportExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect report path: %w", err)
	}

	temporary, temporaryName, err := createRootTemp(run.runRoot, "report")
	if err != nil {
		return fmt.Errorf("create temporary report: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = run.runRoot.Remove(temporaryName)
		}
	}()
	if err := writeReportTemporary(temporary, report); err != nil {
		return err
	}
	if err := publishNoReplace(run.runRoot, temporaryName, reportName); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	if err := run.runRoot.Remove(temporaryName); err != nil {
		return fmt.Errorf("remove published report temporary file: %w", err)
	}
	removeTemporary = false
	if err := syncRootDirectory(run.runRoot); err != nil {
		return fmt.Errorf("sync run directory: %w", err)
	}
	return nil
}

func prepareReport(report Report, runID, repoID string) (Report, error) {
	if report.SchemaVersion != ReportSchemaVersion {
		return report, fmt.Errorf("unsupported report schema version %d", report.SchemaVersion)
	}
	if report.RunID == "" {
		report.RunID = runID
	} else if report.RunID != runID {
		return report, fmt.Errorf("report run ID %q does not match ledger run ID %q", report.RunID, runID)
	}
	if err := validateReport(report, runID); err != nil {
		return report, fmt.Errorf("validate report: %w", err)
	}
	if report.RepoID != repoID {
		return report, fmt.Errorf("report repository ID %q does not match ledger repository ID %q", report.RepoID, repoID)
	}
	return report, nil
}

func writeReportTemporary(temporary *os.File, report Report) error {
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set report permissions: %w", err)
	}
	encoder := json.NewEncoder(&boundedReportWriter{writer: temporary, remaining: reportOutputLimit})
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	return nil
}

type boundedReportWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *boundedReportWriter) Write(raw []byte) (int, error) {
	if len(raw) <= writer.remaining {
		written, err := writer.writer.Write(raw)
		writer.remaining -= written
		return written, err
	}
	written, err := writer.writer.Write(raw[:writer.remaining])
	writer.remaining -= written
	if err != nil {
		return written, err
	}
	return written, errors.New("encoded report exceeds size limit")
}

// WritePlan atomically persists the current action plan, replacing any prior version.
func (run *RunLedger) WritePlan(raw []byte) error {
	if run == nil || run.marker != run {
		return ErrUninitialized
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || run.closing {
		return ErrClosed
	}
	temporary, temporaryName, err := createRootTemp(run.runRoot, "plan")
	if err != nil {
		return fmt.Errorf("create temporary plan: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = run.runRoot.Remove(temporaryName)
		}
	}()
	if err := writeArtifactTemporary(temporary, raw, 0); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	if err := run.runRoot.Rename(temporaryName, "plan.json"); err != nil {
		return fmt.Errorf("publish plan: %w", err)
	}
	removeTemporary = false
	syncDirectory := syncRootDirectory
	if run.syncDirectory != nil {
		syncDirectory = run.syncDirectory
	}
	if err := syncDirectory(run.runRoot); err != nil {
		return &PlanWriteError{Published: true, Err: fmt.Errorf("sync run directory: %w", err)}
	}
	return nil
}

// WriteBrief persists one immutable agent brief.
func (run *RunLedger) WriteBrief(batchID string, attempt int, raw []byte) error {
	return run.writeAttemptArtifact(runBriefArtifact, batchID, attempt, raw)
}

// WriteAdapterJSONL persists one immutable, bounded adapter protocol log.
func (run *RunLedger) WriteAdapterJSONL(batchID string, attempt int, raw []byte) error {
	return run.writeAttemptArtifact(runAdapterArtifact, batchID, attempt, raw)
}

type runArtifactKind int

const (
	runBriefArtifact runArtifactKind = iota
	runAdapterArtifact
)

func (run *RunLedger) writeAttemptArtifact(kind runArtifactKind, batchID string, attempt int, raw []byte) error {
	if run == nil || run.marker != run {
		return ErrUninitialized
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || run.closing {
		return ErrClosed
	}
	if !validArtifactBatchID(batchID) {
		return fmt.Errorf("invalid batch ID %q", batchID)
	}
	if attempt <= 0 {
		return fmt.Errorf("attempt must be positive")
	}
	root := run.briefRoot
	extension := ".txt"
	limit := 0
	label := "brief"
	temporaryPrefix := "brief"
	if kind == runAdapterArtifact {
		root = run.adapterRoot
		extension = ".jsonl"
		limit = rawOutputLimit
		label = "adapter JSONL"
		temporaryPrefix = "adapter"
	}
	name := "attempt-" + attemptArtifactDigest(batchID, attempt) + extension
	if _, err := root.Lstat(name); err == nil {
		return fmt.Errorf("%s already exists", label)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s path: %w", label, err)
	}
	temporary, temporaryName, err := createRootTemp(root, temporaryPrefix)
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", label, err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = root.Remove(temporaryName)
		}
	}()
	if err := writeArtifactTemporary(temporary, raw, limit); err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := root.Link(temporaryName, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists", label)
		}
		return fmt.Errorf("publish %s: %w", label, err)
	}
	if err := root.Remove(temporaryName); err != nil {
		return fmt.Errorf("remove published %s temporary file: %w", label, err)
	}
	removeTemporary = false
	if err := syncRootDirectory(root); err != nil {
		return fmt.Errorf("sync %s directory: %w", label, err)
	}
	return nil
}

func validArtifactBatchID(batchID string) bool {
	return batchID != "" && batchID != "." && batchID != ".." &&
		filepath.Clean(batchID) == batchID && !filepath.IsAbs(batchID) &&
		!strings.ContainsAny(batchID, `/\:`)
}

func attemptArtifactDigest(batchID string, attempt int) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/attempt-artifact-identity/v1\x00"))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(batchID)))
	_, _ = hash.Write(encoded[:])
	_, _ = hash.Write([]byte(batchID))
	binary.BigEndian.PutUint64(encoded[:], uint64(attempt))
	_, _ = hash.Write(encoded[:])
	return hex.EncodeToString(hash.Sum(nil))
}

func writeArtifactTemporary(temporary *os.File, raw []byte, limit int) error {
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set artifact permissions: %w", err)
	}
	output := raw
	if limit > 0 && len(output) > limit {
		output = append(append(make([]byte, 0, limit), raw[:limit-len(rawTruncationMarker)]...), rawTruncationMarker...)
	}
	if _, err := temporary.Write(output); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return nil
}

type ledgerRawSink struct {
	run      *RunLedger
	gate     string
	language string
}

// RawSink validates and binds one gate identity before execution begins.
func (run *RunLedger) RawSink(gate, language string) (RawSink, error) {
	if run == nil || run.marker != run {
		return nil, ErrUninitialized
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || run.closing {
		return nil, ErrClosed
	}
	return &ledgerRawSink{run: run, gate: gate, language: language}, nil
}

func (sink *ledgerRawSink) WriteRaw(stream string, raw []byte) error {
	if sink == nil || sink.run == nil {
		return ErrUninitialized
	}
	return sink.run.writeRaw(sink.gate, sink.language, stream, raw)
}

// writeRaw atomically persists one captured tool stream for a bound sink.
func (run *RunLedger) writeRaw(gate, language, stream string, raw []byte) error {
	if run == nil || run.marker != run {
		return ErrUninitialized
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed || run.closing {
		return ErrClosed
	}
	if stream != "stdout" && stream != "stderr" {
		return fmt.Errorf("invalid raw output stream %q", stream)
	}
	temporary, temporaryName, err := createRootTemp(run.rawRoot, "raw")
	if err != nil {
		return fmt.Errorf("create temporary raw output: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = run.rawRoot.Remove(temporaryName)
		}
	}()
	if err := writeRawTemporary(temporary, raw); err != nil {
		return err
	}
	name := RawOutputName(gate, language, stream)
	if err := run.rawRoot.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("publish raw output: %w", err)
	}
	removeTemporary = false
	if err := syncRootDirectory(run.rawRoot); err != nil {
		return fmt.Errorf("sync raw output directory: %w", err)
	}
	return nil
}

// RawOutputName is the filename a gate's captured stream is persisted under
// inside a run's raw directory. Gate and language are folded into a bounded
// digest so any compiled gate name can be written without weakening ledger
// confinement, which leaves the name opaque: readers locate an artifact by
// deriving its name here, not by parsing one off disk.
func RawOutputName(gateName, language, stream string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/raw-output-identity/v1\x00"))
	for _, component := range []string{gateName, language} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(component)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(component))
	}
	return "gate-" + hex.EncodeToString(hash.Sum(nil)) + "." + stream
}

func writeRawTemporary(temporary *os.File, raw []byte) error {
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set raw output permissions: %w", err)
	}
	output := raw
	if len(output) > rawOutputLimit {
		output = append(append(make([]byte, 0, rawOutputLimit), raw[:rawOutputLimit-len(rawTruncationMarker)]...), rawTruncationMarker...)
	}
	if _, err := temporary.Write(output); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write raw output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync raw output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close raw output: %w", err)
	}
	return nil
}

// Close releases this run's repository lock.
func (run *RunLedger) Close() error {
	if run == nil || run.marker != run {
		return ErrUninitialized
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed {
		return nil
	}
	run.closing = true
	if err := run.lock.release(); err != nil {
		return err
	}
	var closeErr error
	for _, root := range []**os.Root{&run.adapterRoot, &run.briefRoot, &run.rawRoot, &run.runRoot, &run.runsRoot, &run.repoRoot} {
		if *root == nil {
			continue
		}
		if err := (*root).Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
			continue
		}
		*root = nil
	}
	if closeErr != nil {
		return closeErr
	}
	run.closed = true
	return nil
}
