package finding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	return strings.ReplaceAll(file, "\\", "/")
}

func normalizeSnippet(snippet string) string {
	return strings.Join(strings.Fields(snippet), " ")
}
