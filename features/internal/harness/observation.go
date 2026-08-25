package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/joellarson/togi/internal/run"
)

type Outcome struct {
	Code    int
	Message string
}

type exitSource interface {
	outcome() (Outcome, error)
}

type serviceExit struct{ err error }
type processExit struct{ code int }

// outcome maps a service error onto a process status the same way the CLI
// does, so the in-process driver cannot observe an exit code the subprocess
// would never produce.
func (e serviceExit) outcome() (Outcome, error) {
	if e.err == nil {
		return Outcome{}, nil
	}
	return Outcome{Code: run.ResolveExit(e.err), Message: e.err.Error()}, nil
}

func (e processExit) outcome() (Outcome, error) { return Outcome{Code: e.code}, nil }

type CommandObservation struct {
	stdout []byte
	stderr []byte
	source exitSource
}

type RunObservation struct {
	CommandObservation
	reportBytes []byte
	reportPath  string
	rawPaths    map[string]string
}

func newServiceRunObservation(stdout, stderr []byte, err error, reportBytes []byte, reportPath string) RunObservation {
	return newRunObservation(stdout, stderr, serviceExit{err: err}, reportBytes, reportPath, nil)
}

func newProcessRunObservation(stdout, stderr []byte, code int, reportBytes []byte, reportPath string) RunObservation {
	return newRunObservation(stdout, stderr, processExit{code: code}, reportBytes, reportPath, nil)
}

func newRunObservation(stdout, stderr []byte, source exitSource, reportBytes []byte, reportPath string, rawPaths map[string]string) RunObservation {
	return RunObservation{
		CommandObservation: CommandObservation{stdout: cloneBytes(stdout), stderr: cloneBytes(stderr), source: source},
		reportBytes:        cloneBytes(reportBytes),
		reportPath:         reportPath,
		rawPaths:           clonePaths(rawPaths),
	}
}

func (o CommandObservation) Stdout() string { return string(o.stdout) }
func (o CommandObservation) Stderr() string { return string(o.stderr) }
func (o CommandObservation) Outcome() (Outcome, error) {
	if o.source == nil {
		return Outcome{}, errors.New("observation has no outcome source")
	}
	return o.source.outcome()
}

func (o RunObservation) Report() (Report, error) {
	if len(o.reportBytes) == 0 {
		return Report{}, errors.New("run observation has no persisted report")
	}
	decoder := json.NewDecoder(bytes.NewReader(o.reportBytes))
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode persisted report: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Report{}, errors.New("decode persisted report: trailing JSON value")
		}
		return Report{}, fmt.Errorf("decode persisted report: trailing input: %w", err)
	}
	return report, nil
}

func (o RunObservation) ReportPath() string { return o.reportPath }

// ArtifactPath locates a named run artifact without reading its contents.
func (o RunObservation) ArtifactPath(name string) (string, bool) {
	if o.reportPath == "" || name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name {
		return "", false
	}
	path := filepath.Join(filepath.Dir(o.reportPath), name)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return path, true
}

// ArtifactCount counts direct entries in a run artifact directory.
func (o RunObservation) ArtifactCount(name string) (int, error) {
	path, ok := o.ArtifactPath(name)
	if !ok {
		return 0, fmt.Errorf("run artifact %q is unavailable", name)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, fmt.Errorf("read run artifact directory %q: %w", name, err)
	}
	return len(entries), nil
}

// RawPath locates one persisted gate stream. Raw filenames are opaque digests,
// so the artifact is found by deriving the name the ledger writes rather than
// by reading identity back out of the directory listing.
func (o RunObservation) RawPath(gate, language, stream string) (string, bool) {
	path, ok := o.rawPaths[run.RawOutputName(gate, language, stream)]
	return path, ok
}

func cloneBytes(value []byte) []byte { return append([]byte(nil), value...) }
func clonePaths(value map[string]string) map[string]string {
	if len(value) == 0 {
		return nil
	}
	copy := make(map[string]string, len(value))
	for key, path := range value {
		copy[key] = path
	}
	return copy
}

type Report struct {
	SchemaVersion int            `json:"schema_version"`
	RunID         string         `json:"run_id"`
	RepoID        string         `json:"repo_id"`
	Diff          DiffReport     `json:"diff"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    time.Time      `json:"finished_at"`
	Verdict       string         `json:"verdict"`
	Gates         []GateReport   `json:"gates"`
	Findings      []Finding      `json:"findings"`
	Counts        Counts         `json:"counts"`
	Waivers       []WaiverRecord `json:"waivers"`
	Fix           *FixReport     `json:"fix,omitempty"`
}
type WaiverRecord struct {
	Fingerprint string    `json:"fingerprint"`
	Reason      string    `json:"reason"`
	ApprovedAt  time.Time `json:"approved_at"`
}

type FixReport struct {
	OriginalHead  string        `json:"original_head"`
	FeatureBranch string        `json:"feature_branch"`
	Agent         AgentReport   `json:"agent"`
	Baseline      SuiteResult   `json:"baseline"`
	Final         *SuiteResult  `json:"final,omitempty"`
	Rails         RailsReport   `json:"rails"`
	Batches       []BatchReport `json:"batches"`
	Integrity     []Finding     `json:"integrity"`
	Landing       LandingReport `json:"landing"`
}

type AgentReport struct {
	Name  string      `json:"name"`
	Usage *AgentUsage `json:"usage,omitempty"`
}
type AgentUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}
type RailsReport struct {
	MaxIterations  int   `json:"max_iterations"`
	Iterations     int   `json:"iterations"`
	MaxWallClockMS int64 `json:"max_wall_clock_ms"`
	ElapsedMS      int64 `json:"elapsed_ms"`
}
type SuiteResult struct {
	Command    []string `json:"command"`
	Packages   []string `json:"packages,omitempty"`
	Status     string   `json:"status"`
	DurationMS int64    `json:"duration_ms"`
	Diagnostic string   `json:"diagnostic,omitempty"`
}
type BatchReport struct {
	ID          string          `json:"id"`
	PrimaryFile string          `json:"primary_file"`
	Findings    []Finding       `json:"findings"`
	Status      string          `json:"status"`
	Attempts    []AttemptReport `json:"attempts"`
}
type AttemptReport struct {
	Number       int      `json:"number"`
	Status       string   `json:"status"`
	Failure      string   `json:"failure,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Commit       string   `json:"commit,omitempty"`
}
type LandingReport struct {
	Status          string `json:"status"`
	Commit          string `json:"commit,omitempty"`
	PreservedBranch string `json:"preserved_branch,omitempty"`
	Error           string `json:"error,omitempty"`
}

type DiffReport struct {
	BaseRef      string `json:"base_ref"`
	BaseCommit   string `json:"base_commit"`
	MergeBase    string `json:"merge_base"`
	Head         string `json:"head"`
	ChangedFiles int    `json:"changed_files"`
	ChangedLines int    `json:"changed_lines"`
}
type GateReport struct {
	Gate            string    `json:"gate"`
	Language        string    `json:"language"`
	Blocking        []string  `json:"blocking"`
	FixPolicy       string    `json:"fix_policy"`
	Position        int       `json:"position"`
	Status          string    `json:"status"`
	Findings        []Finding `json:"findings,omitempty"`
	DurationMS      int64     `json:"duration_ms"`
	ObservedVersion string    `json:"observed_version,omitempty"`
	Warnings        []string  `json:"warnings,omitempty"`
	Error           string    `json:"error,omitempty"`
}
type Finding struct {
	Gate        string       `json:"gate"`
	Language    string       `json:"language"`
	RuleID      string       `json:"rule_id"`
	Severity    string       `json:"severity"`
	File        string       `json:"file"`
	Line        int          `json:"line"`
	EndLine     int          `json:"end_line,omitempty"`
	Snippet     string       `json:"snippet"`
	Occurrences []Occurrence `json:"occurrences,omitempty"`
	Message     string       `json:"message"`
	Fingerprint string       `json:"fingerprint"`
}
type Occurrence struct {
	Line    int `json:"line"`
	EndLine int `json:"end_line,omitempty"`
}
type Counts struct {
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Info        int `json:"info"`
	Occurrences int `json:"occurrences"`
}
