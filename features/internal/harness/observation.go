package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/joellarson/togi/internal/run"
	"github.com/joellarson/togi/internal/wiki"
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

func (e serviceExit) outcome() (Outcome, error) {
	if e.err == nil {
		return Outcome{}, nil
	}
	var exit *run.ExitError
	if errors.As(e.err, &exit) {
		return Outcome{Code: exit.Code, Message: e.err.Error()}, nil
	}
	if errors.Is(e.err, wiki.ErrConflictingAliases) {
		return Outcome{Code: 1, Message: e.err.Error()}, nil
	}
	return Outcome{Code: 70, Message: e.err.Error()}, nil
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
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	RepoID        string       `json:"repo_id"`
	Diff          DiffReport   `json:"diff"`
	StartedAt     time.Time    `json:"started_at"`
	FinishedAt    time.Time    `json:"finished_at"`
	Verdict       string       `json:"verdict"`
	Gates         []GateReport `json:"gates"`
	Findings      []Finding    `json:"findings"`
	Counts        Counts       `json:"counts"`
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
