package run

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/flywheel"
	"github.com/joellarson/togi/internal/waiver"
)

// RenderOptions controls human report presentation.
type RenderOptions struct {
	Color   bool
	NoColor bool
}

// Render writes a deterministic, compiler-style report without raw tool output.
func Render(output io.Writer, report Report, opts RenderOptions) error {
	if output == nil {
		return fmt.Errorf("report output is required")
	}
	display := report
	if report.Fix != nil && len(report.Fix.Integrity) != 0 {
		display.Findings = append(cloneReportFindings(report.Findings), cloneReportFindings(report.Fix.Integrity)...)
		display.Counts = countFindings(display.Findings)
	}
	if err := renderFindings(output, display, opts); err != nil {
		return err
	}
	if err := renderGates(output, report.Gates); err != nil {
		return err
	}
	if report.Fix != nil {
		if err := renderFix(output, *report.Fix); err != nil {
			return err
		}
	}
	if err := renderWaivers(output, report.Waivers); err != nil {
		return err
	}
	verdict := string(report.Verdict)
	if opts.Color && !opts.NoColor {
		verdict = colorVerdict(report.Verdict, verdict)
	}
	_, err := fmt.Fprintf(output, "verdict: %s\n", verdict)
	return err
}

func renderFix(output io.Writer, fix FixReport) error {
	if _, err := fmt.Fprintf(output, "baseline suite: %s (%dms)\n", fix.Baseline.Status, fix.Baseline.DurationMS); err != nil {
		return err
	}
	if fix.Final != nil {
		if _, err := fmt.Fprintf(output, "final suite: %s (%dms)\n", fix.Final.Status, fix.Final.DurationMS); err != nil {
			return err
		}
	}
	completed, attempts := 0, 0
	for _, batch := range fix.Batches {
		if batch.Status == flywheel.BatchDone {
			completed++
		}
		attempts += len(batch.Attempts)
	}
	if _, err := fmt.Fprintf(output, "batches: %d/%d complete (%d attempts)\n", completed, len(fix.Batches), attempts); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "rails: %d/%d iterations, %d/%dms wall clock\n", fix.Rails.Iterations, fix.Rails.MaxIterations, fix.Rails.ElapsedMS, fix.Rails.MaxWallClockMS); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "landing: %s", safeText(fix.Landing.Status)); err != nil {
		return err
	}
	if fix.Landing.Commit != "" {
		if _, err := fmt.Fprintf(output, " (%s)", safeText(fix.Landing.Commit)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(output); err != nil {
		return err
	}
	if fix.Landing.PreservedBranch != "" {
		if _, err := fmt.Fprintf(output, "preserved branch: %s\n", safeText(fix.Landing.PreservedBranch)); err != nil {
			return err
		}
	}
	return nil
}

func renderWaivers(output io.Writer, records []waiver.Record) error {
	if len(records) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(output, "waivers:"); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(output, "  %s: %s\n", safeText(record.Fingerprint), safeText(record.Reason)); err != nil {
			return err
		}
	}
	return nil
}

func renderFindings(output io.Writer, report Report, opts RenderOptions) error {
	items := append([]finding.Finding(nil), report.Findings...)
	slices.SortFunc(items, compareFindings)
	for _, item := range items {
		severity := string(item.Severity)
		if opts.Color && !opts.NoColor {
			severity = colorSeverity(item.Severity, severity)
		}
		fingerprint := ""
		if item.Fingerprint != "" {
			fingerprint = " [fingerprint: " + safeText(item.Fingerprint) + "]"
		}
		if _, err := fmt.Fprintf(output, "%s:%d: %s: %s (%s)%s\n", safeText(item.File), item.Line, severity, safeText(item.Message), safeText(item.RuleID), fingerprint); err != nil {
			return err
		}
		if len(item.Occurrences) > 0 {
			occurrences := append([]finding.Occurrence(nil), item.Occurrences...)
			slices.SortFunc(occurrences, func(left, right finding.Occurrence) int {
				if left.Line != right.Line {
					return left.Line - right.Line
				}
				return left.EndLine - right.EndLine
			})
			lines := make([]string, len(occurrences))
			for index, occurrence := range occurrences {
				lines[index] = fmt.Sprint(occurrence.Line)
			}
			if _, err := fmt.Fprintf(output, "    +%d more at lines %s\n", len(lines), strings.Join(lines, ", ")); err != nil {
				return err
			}
		}
	}
	if len(items) > 0 {
		if _, err := fmt.Fprintln(output); err != nil {
			return err
		}
	}
	// Counts are occurrence-weighted; the finding total is the number of
	// distinct findings, which is what the stalemate rail tracks.
	total := fmt.Sprintf("%d %s", len(items), plural(len(items), "finding"))
	if report.Counts.Occurrences != len(items) {
		total += fmt.Sprintf(" across %d %s", report.Counts.Occurrences, plural(report.Counts.Occurrences, "occurrence"))
	}
	if _, err := fmt.Fprintf(output, "%s (%d %s, %d %s, %d info)\n", total, report.Counts.Errors, plural(report.Counts.Errors, "error"), report.Counts.Warnings, plural(report.Counts.Warnings, "warning"), report.Counts.Info); err != nil {
		return err
	}
	return nil
}

func renderGates(output io.Writer, reports []GateReport) error {
	gates := append([]GateReport(nil), reports...)
	slices.SortFunc(gates, func(left, right GateReport) int {
		if left.Gate != right.Gate {
			return strings.Compare(left.Gate, right.Gate)
		}
		return strings.Compare(left.Language, right.Language)
	})
	for _, gate := range gates {
		if _, err := fmt.Fprintf(output, "%s: %s", safeText(gate.Gate), gate.Status); err != nil {
			return err
		}
		if gate.Status == GateErrored && gate.Error != "" {
			if _, err := fmt.Fprintf(output, ": %s", safeText(gate.Error)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(output, " (%dms)", gate.DurationMS); err != nil {
			return err
		}
		if gate.ObservedVersion != "" {
			if _, err := fmt.Fprintf(output, " version %s", safeText(gate.ObservedVersion)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(output); err != nil {
			return err
		}
		for _, warning := range gate.Warnings {
			if _, err := fmt.Fprintf(output, "    warning: %s\n", safeText(warning)); err != nil {
				return err
			}
		}
	}
	return nil
}

func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

func plural(count int, singular string) string {
	if count == 1 {
		return singular
	}
	return singular + "s"
}

func compareFindings(left, right finding.Finding) int {
	if left.File != right.File {
		return strings.Compare(left.File, right.File)
	}
	if left.Line != right.Line {
		return left.Line - right.Line
	}
	if left.Gate != right.Gate {
		return strings.Compare(left.Gate, right.Gate)
	}
	if left.RuleID != right.RuleID {
		return strings.Compare(left.RuleID, right.RuleID)
	}
	return strings.Compare(left.Fingerprint, right.Fingerprint)
}

func colorSeverity(severity finding.Severity, text string) string {
	code := "36"
	switch severity {
	case finding.Error:
		code = "31"
	case finding.Warning:
		code = "33"
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func colorVerdict(verdict Verdict, text string) string {
	code := "33"
	if verdict == VerdictErrored {
		code = "31"
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
