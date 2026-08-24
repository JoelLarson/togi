package flywheel

import "github.com/joellarson/togi/internal/finding"

// TreeSnapshot is repository-relative file content captured at one tree state.
type TreeSnapshot struct {
	Files map[string][]byte
}

func integrityFinding(ruleID, file string, line int, snippet, message string) finding.Finding {
	item := finding.Finding{
		Gate:     "integrity",
		Language: "go",
		RuleID:   ruleID,
		Severity: finding.Error,
		File:     file,
		Line:     line,
		Snippet:  snippet,
		Message:  message,
	}
	grouped, err := finding.Group([]finding.Finding{item})
	if err != nil {
		panic("invalid integrity finding: " + err.Error())
	}
	return grouped[0]
}
