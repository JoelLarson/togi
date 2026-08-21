package run

import (
	"time"

	"github.com/joellarson/togi/internal/finding"
)

// GateStatus describes the outcome of one gate execution.
type GateStatus string

const (
	GatePassed   GateStatus = "passed"
	GateFindings GateStatus = "findings"
	GateErrored  GateStatus = "errored"
)

// Verdict describes the overall outcome of a phase-one run.
type Verdict string

const (
	VerdictUnverified Verdict = "unverified"
	VerdictFindings   Verdict = "findings"
	VerdictErrored    Verdict = "errored"
)

// GateReport records one gate's normalized result.
type GateReport struct {
	Gate            string            `json:"gate"`
	Language        string            `json:"language"`
	Status          GateStatus        `json:"status"`
	Findings        []finding.Finding `json:"findings,omitempty"`
	DurationMS      int64             `json:"duration_ms"`
	ObservedVersion string            `json:"observed_version,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	Error           string            `json:"error,omitempty"`
}

// Counts summarizes findings and their occurrence sites.
type Counts struct {
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Info        int `json:"info"`
	Occurrences int `json:"occurrences"`
}

// Report is the machine-readable artifact for a completed run.
type Report struct {
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	RepoID        string            `json:"repo_id"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Verdict       Verdict           `json:"verdict"`
	Gates         []GateReport      `json:"gates"`
	Findings      []finding.Finding `json:"findings"`
	Counts        Counts            `json:"counts"`
}
