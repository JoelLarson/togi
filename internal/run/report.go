package run

import (
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

// ReportSchemaVersion is the only persisted report schema accepted pre-1.0.
const ReportSchemaVersion = 3

// GateStatus describes the outcome of one gate execution.
type GateStatus string

const (
	GatePassed   GateStatus = "passed"
	GateFindings GateStatus = "findings"
	GateErrored  GateStatus = "errored"
)

// Verdict describes the overall outcome of a run.
type Verdict string

const (
	VerdictUnverified Verdict = "unverified"
	VerdictFindings   Verdict = "findings"
	VerdictBlocked    Verdict = "blocked"
	VerdictRails      Verdict = "rails"
	VerdictErrored    Verdict = "errored"
	VerdictUnsealed   Verdict = "unsealed"
)

// GateReport records one gate's normalized result.
type GateReport struct {
	Gate            string             `json:"gate"`
	Language        string             `json:"language"`
	Blocking        []finding.Severity `json:"blocking"`
	FixPolicy       gate.FixPolicy     `json:"fix_policy"`
	Position        int                `json:"position"`
	Status          GateStatus         `json:"status"`
	Findings        []finding.Finding  `json:"findings,omitempty"`
	DurationMS      int64              `json:"duration_ms"`
	ObservedVersion string             `json:"observed_version,omitempty"`
	Warnings        []string           `json:"warnings,omitempty"`
	Error           string             `json:"error,omitempty"`
}

// NewGateReport captures the compiled gate and binding identity used by one request.
func NewGateReport(g gate.Gate, binding gate.Binding, position int) GateReport {
	blocking := make([]finding.Severity, len(g.Manifest.Blocking))
	copy(blocking, g.Manifest.Blocking)
	return GateReport{
		Gate:      g.Manifest.Name,
		Language:  binding.Language,
		Blocking:  blocking,
		FixPolicy: g.Manifest.FixPolicy,
		Position:  position,
	}
}

func cloneGateReports(reports []GateReport) []GateReport {
	if reports == nil {
		return nil
	}
	cloned := make([]GateReport, len(reports))
	for index, report := range reports {
		cloned[index] = report
		cloned[index].Blocking = cloneReportSlice(report.Blocking)
		cloned[index].Warnings = cloneReportSlice(report.Warnings)
		cloned[index].Findings = cloneReportFindings(report.Findings)
	}
	return cloned
}

func cloneReportFindings(findings []finding.Finding) []finding.Finding {
	if findings == nil {
		return nil
	}
	cloned := make([]finding.Finding, len(findings))
	for index, item := range findings {
		cloned[index] = item
		cloned[index].Occurrences = cloneReportSlice(item.Occurrences)
	}
	return cloned
}

func cloneReportSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	cloned := make([]T, len(values))
	copy(cloned, values)
	return cloned
}

// RunRef locates a completed report in external state at runtime.
type RunRef struct {
	ID  string
	Dir string
}

// Counts summarizes findings and their occurrence sites.
type Counts struct {
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Info        int `json:"info"`
	Occurrences int `json:"occurrences"`
}

// DiffReport records the public diff scope metadata for a completed run.
type DiffReport struct {
	BaseRef      string `json:"base_ref"`
	BaseCommit   string `json:"base_commit"`
	MergeBase    string `json:"merge_base"`
	Head         string `json:"head"`
	ChangedFiles int    `json:"changed_files"`
	ChangedLines int    `json:"changed_lines"`
}

// Report is the machine-readable artifact for a completed run.
type Report struct {
	Ref           RunRef            `json:"-"`
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	RepoID        string            `json:"repo_id"`
	Diff          DiffReport        `json:"diff"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Verdict       Verdict           `json:"verdict"`
	Gates         []GateReport      `json:"gates"`
	Findings      []finding.Finding `json:"findings"`
	Counts        Counts            `json:"counts"`
}
