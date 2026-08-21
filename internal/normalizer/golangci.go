package normalizer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/joellarson/togi/internal/finding"
)

type golangCIEnvelope struct {
	Issues *[]golangCIIssue `json:"Issues"`
}

type golangCIIssue struct {
	FromLinter string `json:"FromLinter"`
	Text       string `json:"Text"`
	Severity   string `json:"Severity"`
	Pos        struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
		Column   int    `json:"Column"`
	} `json:"Pos"`
}

func normalizeGolangCI(ctx Context, raw []byte) ([]finding.Finding, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var envelope golangCIEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode golangci-lint JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode golangci-lint JSON: multiple values")
		}
		return nil, fmt.Errorf("decode golangci-lint JSON trailing data: %w", err)
	}
	if envelope.Issues == nil {
		return nil, errors.New("decode golangci-lint JSON: Issues array is required")
	}

	results := make([]finding.Finding, 0, len(*envelope.Issues))
	for index, issue := range *envelope.Issues {
		if strings.TrimSpace(issue.FromLinter) == "" {
			return nil, fmt.Errorf("golangci-lint issue %d: FromLinter is required", index)
		}
		if strings.TrimSpace(issue.Text) == "" {
			return nil, fmt.Errorf("golangci-lint issue %d: Text is required", index)
		}
		if strings.TrimSpace(issue.Pos.Filename) == "" {
			return nil, fmt.Errorf("golangci-lint issue %d: Pos.Filename is required", index)
		}
		if issue.Pos.Line <= 0 {
			return nil, fmt.Errorf("golangci-lint issue %d: Pos.Line must be positive", index)
		}

		result, err := makeFinding(
			ctx,
			"golangci-lint/"+issue.FromLinter,
			issue.Severity,
			issue.Pos.Filename,
			issue.Pos.Line,
			issue.Text,
		)
		if err != nil {
			return nil, fmt.Errorf("golangci-lint issue %d: %w", index, err)
		}
		results = append(results, result)
	}
	return results, nil
}
