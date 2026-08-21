package normalizer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/joellarson/togi/internal/finding"
)

func normalizeRegex(pattern string, ctx Context, raw []byte) ([]finding.Finding, error) {
	compiled, message, err := preflightRegex(pattern, ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	records, err := parseRegexOutput(compiled, message, raw)
	if err != nil {
		return nil, err
	}
	sources, err := openSourceSession(ctx.Root)
	if err != nil {
		return nil, err
	}
	defer sources.close()

	results := make([]finding.Finding, 0, len(records))
	for _, record := range records {
		result, err := makeFinding(
			ctx,
			sources,
			ctx.Binding.RuleID,
			"default",
			record.file,
			record.line,
			record.message,
		)
		if err != nil {
			return nil, fmt.Errorf("regex output line %d: %w", record.outputLine, err)
		}
		results = append(results, result)
	}
	return results, nil
}

type regexRecord struct {
	outputLine int
	file       string
	line       int
	message    string
}

func parseRegexOutput(compiled *regexp.Regexp, message *template.Template, raw []byte) ([]regexRecord, error) {
	lines := strings.Split(string(raw), "\n")
	records := make([]regexRecord, 0, len(lines))
	for index, rawLine := range lines {
		lineText := strings.TrimSuffix(rawLine, "\r")
		if lineText == "" {
			if index == len(lines)-1 && rawLine == "" {
				continue
			}
			return nil, fmt.Errorf("regex output line %d is blank; %s", index+1, rawOutputGuidance)
		}
		matches := compiled.FindStringSubmatch(lineText)
		if matches == nil || matches[0] != lineText {
			return nil, fmt.Errorf("regex output line %d does not match; %s", index+1, rawOutputGuidance)
		}

		captures := captureValues(compiled, matches)
		lineIndex := compiled.SubexpIndex("line")
		lineNumber, err := strconv.Atoi(matches[lineIndex])
		if err != nil || lineNumber <= 0 {
			return nil, fmt.Errorf("regex output line %d capture %q is not a positive integer; %s", index+1, "line", rawOutputGuidance)
		}
		var rendered bytes.Buffer
		if err := message.Execute(&rendered, captures); err != nil {
			return nil, fmt.Errorf("render regex output line %d message: %w", index+1, err)
		}

		records = append(records, regexRecord{
			outputLine: index + 1,
			file:       matches[compiled.SubexpIndex("file")],
			line:       lineNumber,
			message:    rendered.String(),
		})
	}
	return records, nil
}

func preflightRegex(pattern string, ctx Context) (*regexp.Regexp, *template.Template, error) {
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("compile regex normalizer: %w", err)
	}
	seenCaptures := make(map[string]struct{})
	for _, name := range compiled.SubexpNames()[1:] {
		if name == "" {
			continue
		}
		if _, exists := seenCaptures[name]; exists {
			return nil, nil, fmt.Errorf("regex normalizer has duplicate named capture %q", name)
		}
		seenCaptures[name] = struct{}{}
	}
	if compiled.SubexpIndex("file") < 0 {
		return nil, nil, fmt.Errorf("regex normalizer requires named capture %q", "file")
	}
	if compiled.SubexpIndex("line") < 0 {
		return nil, nil, fmt.Errorf("regex normalizer requires named capture %q", "line")
	}
	message, err := template.New("message").Option("missingkey=error").Parse(ctx.Binding.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("parse regex message template: %w", err)
	}
	if err := validateTemplateCaptureReferences(message, seenCaptures); err != nil {
		return nil, nil, err
	}
	defaultSeverity, ok := ctx.Binding.SeverityMap["default"]
	if !ok {
		return nil, nil, errors.New("regex binding requires a default severity")
	}
	probe := finding.Finding{
		Gate: "preflight", Language: "preflight", RuleID: ctx.Binding.RuleID,
		Severity: defaultSeverity, File: "source", Line: 1,
		Snippet: "source", Message: "source",
	}
	if err := finding.Validate(probe); err != nil {
		return nil, nil, errors.New("regex binding rule ID or default severity is invalid")
	}
	values := make(map[string]string)
	for _, name := range compiled.SubexpNames()[1:] {
		if name != "" {
			values[name] = "capture"
		}
	}
	if err := message.Execute(io.Discard, values); err != nil {
		return nil, nil, fmt.Errorf("validate regex message template: %w", err)
	}
	return compiled, message, nil
}

func validateTemplateCaptureReferences(message *template.Template, captures map[string]struct{}) error {
	if len(message.Templates()) != 1 || message.Tree == nil || message.Tree.Root == nil {
		return errors.New("regex message template supports only static text and direct capture references")
	}
	return validateTemplateList(message.Tree.Root, captures)
}

func validateTemplateList(list *parse.ListNode, captures map[string]struct{}) error {
	for _, node := range list.Nodes {
		switch node := node.(type) {
		case *parse.TextNode:
			continue
		case *parse.ActionNode:
			if err := validateDirectCaptureAction(node, captures); err != nil {
				return err
			}
		default:
			return errors.New("regex message template supports only static text and direct capture references")
		}
	}
	return nil
}

func validateDirectCaptureAction(action *parse.ActionNode, captures map[string]struct{}) error {
	pipe := action.Pipe
	if pipe == nil || len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return errors.New("regex message template supports only direct capture references")
	}
	field, ok := pipe.Cmds[0].Args[0].(*parse.FieldNode)
	if !ok || len(field.Ident) != 1 {
		return errors.New("regex message template supports only direct capture references")
	}
	if _, ok := captures[field.Ident[0]]; !ok {
		return fmt.Errorf("regex message template references unknown capture %q", field.Ident[0])
	}
	return nil
}

func captureValues(compiled *regexp.Regexp, matches []string) map[string]string {
	values := make(map[string]string)
	for index, name := range compiled.SubexpNames() {
		if index > 0 && name != "" {
			values[name] = matches[index]
		}
	}
	return values
}
