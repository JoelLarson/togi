package normalizer

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/joellarson/togi/internal/finding"
)

func normalizeRegex(pattern string, ctx Context, raw []byte) ([]finding.Finding, error) {
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex normalizer: %w", err)
	}
	seenCaptures := make(map[string]struct{})
	for _, name := range compiled.SubexpNames()[1:] {
		if name == "" {
			continue
		}
		if _, exists := seenCaptures[name]; exists {
			return nil, fmt.Errorf("regex normalizer has duplicate named capture %q", name)
		}
		seenCaptures[name] = struct{}{}
	}
	fileIndex := compiled.SubexpIndex("file")
	if fileIndex < 0 {
		return nil, fmt.Errorf("regex normalizer requires named capture %q", "file")
	}
	lineIndex := compiled.SubexpIndex("line")
	if lineIndex < 0 {
		return nil, fmt.Errorf("regex normalizer requires named capture %q", "line")
	}
	message, err := template.New("message").Option("missingkey=error").Parse(ctx.Binding.Message)
	if err != nil {
		return nil, fmt.Errorf("parse regex message template: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	lines := strings.Split(string(raw), "\n")
	results := make([]finding.Finding, 0, len(lines))
	for index, rawLine := range lines {
		lineText := strings.TrimSuffix(rawLine, "\r")
		if lineText == "" {
			continue
		}
		matches := compiled.FindStringSubmatch(lineText)
		if matches == nil || matches[0] != lineText {
			return nil, fmt.Errorf("regex output line %d did not match: %q", index+1, lineText)
		}

		captures := make(map[string]string)
		for captureIndex, name := range compiled.SubexpNames() {
			if captureIndex > 0 && name != "" {
				captures[name] = matches[captureIndex]
			}
		}
		lineNumber, err := strconv.Atoi(matches[lineIndex])
		if err != nil || lineNumber <= 0 {
			return nil, fmt.Errorf("regex output line %d has invalid line %q", index+1, matches[lineIndex])
		}
		var rendered bytes.Buffer
		if err := message.Execute(&rendered, captures); err != nil {
			return nil, fmt.Errorf("render regex output line %d message: %w", index+1, err)
		}

		result, err := makeFinding(
			ctx,
			ctx.Binding.RuleID,
			"default",
			matches[fileIndex],
			lineNumber,
			rendered.String(),
		)
		if err != nil {
			return nil, fmt.Errorf("regex output line %d: %w", index+1, err)
		}
		results = append(results, result)
	}
	return results, nil
}
