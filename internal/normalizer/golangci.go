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

type golangciNormalizer struct {
	config Config
}

func (n golangciNormalizer) Normalize(ctx Context, raw []byte) ([]finding.Finding, error) {
	if err := validUTF8Raw("normalizer", raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var envelope golangCIEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("golangci-lint output is invalid JSON; %s", rawOutputGuidance)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("golangci-lint output contains multiple JSON values; %s", rawOutputGuidance)
		}
		return nil, fmt.Errorf("golangci-lint output has invalid trailing data; %s", rawOutputGuidance)
	}
	if envelope.Issues == nil {
		return nil, errors.New("decode golangci-lint JSON: Issues array is required")
	}

	if len(*envelope.Issues) == 0 {
		return nil, nil
	}
	for index, issue := range *envelope.Issues {
		if err := validateGolangCIIssue(index, issue); err != nil {
			return nil, err
		}
	}
	sources, err := openSourceSession(ctx.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sources.close() }()

	results := make([]finding.Finding, 0, len(*envelope.Issues))
	for index, issue := range *envelope.Issues {
		result, err := makeFinding(
			ctx,
			n.config,
			sources,
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

func validateGolangCIIssue(index int, issue golangCIIssue) error {
	if strings.TrimSpace(issue.FromLinter) == "" {
		return fmt.Errorf("golangci-lint issue %d: FromLinter is required", index)
	}
	if strings.TrimSpace(issue.Text) == "" {
		return fmt.Errorf("golangci-lint issue %d: Text is required", index)
	}
	if strings.TrimSpace(issue.Pos.Filename) == "" {
		return fmt.Errorf("golangci-lint issue %d: Pos.Filename is required", index)
	}
	if issue.Pos.Line <= 0 {
		return fmt.Errorf("golangci-lint issue %d: Pos.Line must be positive", index)
	}
	return nil
}
