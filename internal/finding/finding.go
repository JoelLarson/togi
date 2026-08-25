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

// ValidFingerprint reports whether value has the shape Fingerprint produces.
// It is the syntactic check a persisted or operator-supplied identity can be
// held to; matching a fingerprint to a live finding is a separate question.
func ValidFingerprint(value string) bool {
	if len(value) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func normalizeFile(file string) string {
	return filepath.ToSlash(filepath.Clean(file))
}

func normalizeSnippet(snippet string) string {
	return strings.Join(strings.Fields(snippet), " ")
}

// Validate verifies that a finding can enter the normalized finding boundary.
func Validate(finding Finding) error {
	if err := validateIdentity(finding); err != nil {
		return err
	}
	if err := validateLocation(finding.Line, finding.EndLine); err != nil {
		return err
	}
	for index, occurrence := range finding.Occurrences {
		if err := validateLocation(occurrence.Line, occurrence.EndLine); err != nil {
			return fmt.Errorf("occurrence %d %w", index, err)
		}
	}
	if finding.Fingerprint != "" && finding.Fingerprint != Fingerprint(finding) {
		return errors.New("fingerprint does not match finding identity")
	}
	return nil
}

func validateIdentity(finding Finding) error {
	switch {
	case strings.TrimSpace(finding.Gate) == "":
		return errors.New("gate is required")
	case strings.TrimSpace(finding.Language) == "":
		return errors.New("language is required")
	case strings.TrimSpace(finding.RuleID) == "":
		return errors.New("rule ID is required")
	case !toolQualifiedRuleID(finding.RuleID):
		return errors.New("rule ID must be tool-qualified")
	case !validSeverity(finding.Severity):
		return fmt.Errorf("invalid severity %q", finding.Severity)
	case finding.File == "":
		return errors.New("file is required")
	case strings.TrimSpace(finding.Snippet) == "":
		return errors.New("snippet is required")
	case strings.TrimSpace(finding.Message) == "":
		return errors.New("message is required")
	}
	if _, err := ParsePath(finding.File); err != nil {
		return err
	}
	return nil
}

func validateLocation(line, endLine int) error {
	if line <= 0 {
		return errors.New("line must be positive")
	}
	if endLine != 0 && endLine < line {
		return errors.New("end line precedes line")
	}
	return nil
}

func validSeverity(severity Severity) bool {
	return severity == Error || severity == Warning || severity == Info
}

func toolQualifiedRuleID(ruleID string) bool {
	slash := strings.IndexByte(ruleID, '/')
	return slash > 0 && slash < len(ruleID)-1
}
