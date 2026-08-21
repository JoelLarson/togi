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
	message, err := template.New("message").
		Funcs(template.FuncMap{"index": rejectIndirectCaptureLookup}).
		Option("missingkey=error").
		Parse(ctx.Binding.Message)
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

func rejectIndirectCaptureLookup(any, ...any) (any, error) {
	return nil, errors.New("indirect capture lookup is not allowed")
}

func validateTemplateCaptureReferences(message *template.Template, captures map[string]struct{}) error {
	for _, defined := range message.Templates() {
		if defined.Tree == nil || defined.Tree.Root == nil {
			continue
		}
		if err := validateTemplateNode(defined.Tree.Root, captures, true); err != nil {
			return err
		}
	}
	return nil
}

func validateTemplateNode(node parse.Node, captures map[string]struct{}, dotIsCaptureMap bool) error {
	if node == nil {
		return nil
	}
	switch node := node.(type) {
	case *parse.ListNode:
		for _, child := range node.Nodes {
			if err := validateTemplateNode(child, captures, dotIsCaptureMap); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return validateTemplateNode(node.Pipe, captures, dotIsCaptureMap)
	case *parse.IfNode:
		return validateTemplateBranch(node.Pipe, node.List, node.ElseList, captures, dotIsCaptureMap, dotIsCaptureMap)
	case *parse.RangeNode:
		return validateTemplateBranch(node.Pipe, node.List, node.ElseList, captures, dotIsCaptureMap, false)
	case *parse.WithNode:
		return validateTemplateBranch(node.Pipe, node.List, node.ElseList, captures, dotIsCaptureMap, false)
	case *parse.TemplateNode:
		if node.Pipe != nil {
			if err := validateTemplateNode(node.Pipe, captures, dotIsCaptureMap); err != nil {
				return err
			}
		}
		if !dotIsCaptureMap || !templatePipePassesRootDot(node.Pipe) {
			return errors.New("regex message template must receive the root capture map")
		}
	case *parse.PipeNode:
		for _, command := range node.Cmds {
			if err := validateTemplateNode(command, captures, dotIsCaptureMap); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		for _, argument := range node.Args {
			if err := validateTemplateNode(argument, captures, dotIsCaptureMap); err != nil {
				return err
			}
		}
	case *parse.FieldNode:
		if !dotIsCaptureMap {
			return errors.New("regex message template references a capture after dot was rebound")
		}
		if len(node.Ident) != 1 {
			return errors.New("regex message template requires direct capture references")
		}
		if _, ok := captures[node.Ident[0]]; !ok {
			return fmt.Errorf("regex message template references unknown capture %q", node.Ident[0])
		}
	case *parse.ChainNode:
		if len(node.Field) != 0 {
			return errors.New("regex message template requires direct capture references")
		}
		return validateTemplateNode(node.Node, captures, dotIsCaptureMap)
	case *parse.VariableNode:
		if len(node.Ident) > 1 {
			return errors.New("regex message template requires direct capture references")
		}
	case *parse.IdentifierNode:
		if node.Ident == "index" {
			return errors.New("regex message template requires direct capture references")
		}
	}
	return nil
}

func validateTemplateBranch(pipe *parse.PipeNode, list, elseList *parse.ListNode, captures map[string]struct{}, currentDot, listDot bool) error {
	if pipe != nil {
		if err := validateTemplateNode(pipe, captures, currentDot); err != nil {
			return err
		}
	}
	if list != nil {
		if err := validateTemplateNode(list, captures, listDot); err != nil {
			return err
		}
	}
	if elseList != nil {
		if err := validateTemplateNode(elseList, captures, currentDot); err != nil {
			return err
		}
	}
	return nil
}

func templatePipePassesRootDot(pipe *parse.PipeNode) bool {
	if pipe == nil || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return false
	}
	_, ok := pipe.Cmds[0].Args[0].(*parse.DotNode)
	return ok
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
