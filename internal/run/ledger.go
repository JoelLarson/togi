package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joellarson/togi/internal/finding"
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
)

var errIncompleteRun = errors.New("incomplete run")

const defaultRunRetention = 20

const rawOutputLimit = 1 << 20

var rawTruncationMarker = []byte("\n[togi: output truncated]\n")

var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}$`)

var rawComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Ledger manages run artifacts below one repository's external state path.
type Ledger struct {
	RepoState string
	Keep      int
	Now       func() time.Time
	Random    io.Reader
}

// RunLedger is one active, exclusively locked run directory.
type RunLedger struct {
	Dir    string
	lock   *stateLock
	mu     sync.Mutex
	closed bool
}

// Start acquires the repository lock and creates a new run directory.
func (ledger Ledger) Start() (*RunLedger, error) {
	if ledger.RepoState == "" {
		return nil, errors.New("repository state path is required")
	}
	if err := os.MkdirAll(ledger.RepoState, 0o700); err != nil {
		return nil, fmt.Errorf("create repository state: %w", err)
	}
	runsDir := filepath.Join(ledger.RepoState, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create runs directory: %w", err)
	}
	now := time.Now
	if ledger.Now != nil {
		now = ledger.Now
	}
	startedAt := now().UTC()
	lock, err := acquireStateLock(filepath.Join(ledger.RepoState, "lock"), startedAt)
	if err != nil {
		return nil, err
	}
	keep := ledger.Keep
	if keep <= 0 {
		keep = defaultRunRetention
	}
	if err := pruneRuns(runsDir, keep-1); err != nil {
		_ = lock.release()
		return nil, err
	}
	random := ledger.Random
	if random == nil {
		random = rand.Reader
	}
	suffix := make([]byte, 2)
	if _, err := io.ReadFull(random, suffix); err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("generate run ID: %w", err)
	}
	runID := startedAt.Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix)
	runDir := filepath.Join(runsDir, runID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		_ = lock.release()
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrRunIDCollision, runID)
		}
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(runDir, "raw"), 0o700); err != nil {
		_ = os.Remove(runDir)
		_ = lock.release()
		return nil, fmt.Errorf("create raw directory: %w", err)
	}
	return &RunLedger{Dir: runDir, lock: lock}, nil
}

// Latest returns the newest complete, parseable report in the ledger.
func (ledger Ledger) Latest() (Report, error) {
	if ledger.RepoState == "" {
		return Report{}, errors.New("repository state path is required")
	}
	runsDir := filepath.Join(ledger.RepoState, "runs")
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, ErrNoCompleteRuns
	}
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
		report, err := readCompleteReport(filepath.Join(runsDir, runID), runID)
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

func readCompleteReport(runDir, runID string) (Report, error) {
	runInfo, err := os.Lstat(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, errIncompleteRun
	}
	if err != nil {
		return Report{}, err
	}
	if !runInfo.IsDir() || runInfo.Mode()&os.ModeSymlink != 0 {
		return Report{}, errIncompleteRun
	}
	reportPath := filepath.Join(runDir, "report.json")
	before, err := os.Lstat(reportPath)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, errIncompleteRun
	}
	if err != nil {
		return Report{}, err
	}
	if !before.Mode().IsRegular() {
		return Report{}, errIncompleteRun
	}
	file, err := os.Open(reportPath)
	if err != nil {
		return Report{}, err
	}
	defer file.Close()
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
	return report, nil
}

func validateReport(report Report, runID string) error {
	if report.SchemaVersion != 1 {
		return fmt.Errorf("unsupported report schema version %d", report.SchemaVersion)
	}
	if report.RunID != runID {
		return fmt.Errorf("report run ID %q does not match ledger run ID %q", report.RunID, runID)
	}
	if report.Verdict != "" && report.Verdict != VerdictUnverified && report.Verdict != VerdictFindings && report.Verdict != VerdictErrored {
		return fmt.Errorf("invalid verdict %q", report.Verdict)
	}
	if !report.StartedAt.IsZero() && !report.FinishedAt.IsZero() && report.FinishedAt.Before(report.StartedAt) {
		return errors.New("report finished before it started")
	}
	if report.Counts.Errors < 0 || report.Counts.Warnings < 0 || report.Counts.Info < 0 || report.Counts.Occurrences < 0 {
		return errors.New("report counts must not be negative")
	}
	for index, gate := range report.Gates {
		if gate.Status != "" && gate.Status != GatePassed && gate.Status != GateFindings && gate.Status != GateErrored {
			return fmt.Errorf("gate %d has invalid status %q", index, gate.Status)
		}
		if gate.DurationMS < 0 {
			return fmt.Errorf("gate %d has negative duration", index)
		}
		for findingIndex, item := range gate.Findings {
			if err := finding.Validate(item); err != nil {
				return fmt.Errorf("gate %d finding %d: %w", index, findingIndex, err)
			}
		}
	}
	for index, item := range report.Findings {
		if err := finding.Validate(item); err != nil {
			return fmt.Errorf("finding %d: %w", index, err)
		}
	}
	return nil
}

func pruneRuns(runsDir string, retain int) error {
	entries, err := os.ReadDir(runsDir)
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
		path := filepath.Join(runsDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect old run %s: %w", name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("prune old run %s: %w", name, err)
		}
	}
	return nil
}

func validRunID(runID string) bool {
	if !runIDPattern.MatchString(runID) {
		return false
	}
	_, err := time.Parse("20060102T150405Z", runID[:16])
	return err == nil
}

// WriteReport atomically persists the completed report.
func (run *RunLedger) WriteReport(report Report) error {
	if run == nil {
		return ErrClosed
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed {
		return ErrClosed
	}
	runID := filepath.Base(run.Dir)
	if report.SchemaVersion != 1 {
		return fmt.Errorf("unsupported report schema version %d", report.SchemaVersion)
	}
	if report.RunID == "" {
		report.RunID = runID
	} else if report.RunID != runID {
		return fmt.Errorf("report run ID %q does not match ledger run ID %q", report.RunID, runID)
	}
	if err := validateReport(report, runID); err != nil {
		return fmt.Errorf("validate report: %w", err)
	}
	reportPath := filepath.Join(run.Dir, "report.json")
	if _, err := os.Lstat(reportPath); err == nil {
		return ErrReportExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect report path: %w", err)
	}

	temporary, err := os.CreateTemp(run.Dir, ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary report: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set report permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
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
	if err := os.Rename(temporaryPath, reportPath); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	removeTemporary = false
	if err := syncDirectory(run.Dir); err != nil {
		return fmt.Errorf("sync run directory: %w", err)
	}
	return nil
}

// WriteRaw atomically persists one captured tool stream.
func (run *RunLedger) WriteRaw(gate, language, stream string, raw []byte) error {
	if run == nil {
		return ErrClosed
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed {
		return ErrClosed
	}
	if !safeRawComponent(gate) {
		return fmt.Errorf("invalid raw output gate %q", gate)
	}
	if !safeRawComponent(language) {
		return fmt.Errorf("invalid raw output language %q", language)
	}
	if stream != "stdout" && stream != "stderr" {
		return fmt.Errorf("invalid raw output stream %q", stream)
	}
	rawDir := filepath.Join(run.Dir, "raw")
	temporary, err := os.CreateTemp(rawDir, ".raw-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary raw output: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set raw output permissions: %w", err)
	}
	output := raw
	if len(output) > rawOutputLimit {
		output = make([]byte, 0, rawOutputLimit)
		output = append(output, raw[:rawOutputLimit-len(rawTruncationMarker)]...)
		output = append(output, rawTruncationMarker...)
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
	name := gate + "." + language + "." + stream
	if err := os.Rename(temporaryPath, filepath.Join(rawDir, name)); err != nil {
		return fmt.Errorf("publish raw output: %w", err)
	}
	removeTemporary = false
	if err := syncDirectory(rawDir); err != nil {
		return fmt.Errorf("sync raw output directory: %w", err)
	}
	return nil
}

func safeRawComponent(component string) bool {
	if len(component) > 64 || !rawComponentPattern.MatchString(component) {
		return false
	}
	upper := strings.ToUpper(component)
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" {
		return false
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) {
		return upper[3] < '1' || upper[3] > '9'
	}
	return true
}

// Close releases this run's repository lock.
func (run *RunLedger) Close() error {
	if run == nil {
		return nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed {
		return nil
	}
	run.closed = true
	return run.lock.release()
}
