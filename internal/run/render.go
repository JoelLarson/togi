package run

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/joellarson/togi/internal/finding"
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
	items := append([]finding.Finding(nil), report.Findings...)
	slices.SortFunc(items, compareFindings)
	for _, item := range items {
		severity := string(item.Severity)
		if opts.Color && !opts.NoColor {
			severity = colorSeverity(item.Severity, severity)
		}
		if _, err := fmt.Fprintf(output, "%s:%d: %s: %s (%s)\n", safeText(item.File), item.Line, severity, safeText(item.Message), safeText(item.RuleID)); err != nil {
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
	if _, err := fmt.Fprintf(output, "%d findings (%d %s, %d %s, %d info)\n", report.Counts.Occurrences, report.Counts.Errors, plural(report.Counts.Errors, "error"), report.Counts.Warnings, plural(report.Counts.Warnings, "warning"), report.Counts.Info); err != nil {
		return err
	}
	gates := append([]GateReport(nil), report.Gates...)
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
	verdict := string(report.Verdict)
	if opts.Color && !opts.NoColor {
		verdict = colorVerdict(report.Verdict, verdict)
	}
	_, err := fmt.Fprintf(output, "verdict: %s\n", verdict)
	return err
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
	if severity == finding.Error {
		code = "31"
	} else if severity == finding.Warning {
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
