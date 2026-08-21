package finding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Severity is a normalized finding severity.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
)

// Occurrence is an additional location for an otherwise identical finding.
type Occurrence struct {
	Line    int `json:"line"`
	EndLine int `json:"end_line,omitempty"`
}

// Finding is a normalized quality issue reported by a gate.
type Finding struct {
	Gate        string       `json:"gate"`
	Language    string       `json:"language"`
	RuleID      string       `json:"rule_id"`
	Severity    Severity     `json:"severity"`
	File        string       `json:"file"`
	Line        int          `json:"line"`
	EndLine     int          `json:"end_line,omitempty"`
	Snippet     string       `json:"snippet"`
	Occurrences []Occurrence `json:"occurrences,omitempty"`
	Message     string       `json:"message"`
	Fingerprint string       `json:"fingerprint"`
}

// Fingerprint returns the stable identity for a finding.
func Fingerprint(finding Finding) string {
	hash := sha256.New()
	for _, field := range []string{
		finding.Gate,
		finding.RuleID,
		normalizeFile(finding.File),
		normalizeSnippet(finding.Snippet),
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizeFile(file string) string {
	return filepath.ToSlash(filepath.Clean(file))
}

func normalizeSnippet(snippet string) string {
	return strings.Join(strings.Fields(snippet), " ")
}

// Validate verifies that a finding can enter the normalized finding boundary.
func Validate(finding Finding) error {
	switch {
	case strings.TrimSpace(finding.Gate) == "":
		return errors.New("gate is required")
	case strings.TrimSpace(finding.Language) == "":
		return errors.New("language is required")
	case strings.TrimSpace(finding.RuleID) == "":
		return errors.New("rule ID is required")
	case !validSeverity(finding.Severity):
		return fmt.Errorf("invalid severity %q", finding.Severity)
	case finding.File == "":
		return errors.New("file is required")
	case finding.Line <= 0:
		return errors.New("line must be positive")
	case finding.EndLine != 0 && finding.EndLine < finding.Line:
		return errors.New("end line precedes line")
	case strings.TrimSpace(finding.Snippet) == "":
		return errors.New("snippet is required")
	case strings.TrimSpace(finding.Message) == "":
		return errors.New("message is required")
	}

	for index, occurrence := range finding.Occurrences {
		if occurrence.Line <= 0 {
			return fmt.Errorf("occurrence %d line must be positive", index)
		}
		if occurrence.EndLine != 0 && occurrence.EndLine < occurrence.Line {
			return fmt.Errorf("occurrence %d end line precedes line", index)
		}
	}
	if finding.Fingerprint != "" && finding.Fingerprint != Fingerprint(finding) {
		return errors.New("fingerprint does not match finding identity")
	}
	return nil
}

func validSeverity(severity Severity) bool {
	return severity == Error || severity == Warning || severity == Info
}
