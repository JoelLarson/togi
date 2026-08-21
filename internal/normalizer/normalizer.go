package normalizer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

// Context supplies the gate metadata and repository used during normalization.
type Context struct {
	Gate    string
	Root    string
	Binding gate.Binding
}

// Func converts one tool's raw output into normalized findings.
type Func func(Context, []byte) ([]finding.Finding, error)

// Registry maps stable compiled normalizer names to their implementations.
type Registry map[string]Func

// NewRegistry returns all compiled normalizers.
func NewRegistry() Registry {
	return Registry{
		"golangci-json": normalizeGolangCI,
	}
}

// Normalize dispatches to a compiled normalizer or a data-defined regex.
func (r Registry) Normalize(name string, ctx Context, raw []byte) ([]finding.Finding, error) {
	if strings.HasPrefix(name, "regex:") {
		return normalizeRegex(strings.TrimPrefix(name, "regex:"), ctx, raw)
	}
	normalize, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("unknown normalizer %q", name)
	}
	return normalize(ctx, raw)
}

func mappedSeverity(binding gate.Binding, toolSeverity string) (finding.Severity, error) {
	if severity, ok := binding.SeverityMap[toolSeverity]; ok {
		return severity, nil
	}
	if severity, ok := binding.SeverityMap["default"]; ok {
		return severity, nil
	}
	return "", fmt.Errorf("no severity mapping for %q and no default", toolSeverity)
}

func makeFinding(ctx Context, ruleID, toolSeverity, file string, line int, message string) (finding.Finding, error) {
	severity, err := mappedSeverity(ctx.Binding, toolSeverity)
	if err != nil {
		return finding.Finding{}, err
	}
	normalizedFile, snippet, err := readSourceLine(ctx.Root, file, line)
	if err != nil {
		return finding.Finding{}, err
	}

	result := finding.Finding{
		Gate:     ctx.Gate,
		Language: ctx.Binding.Language,
		RuleID:   ruleID,
		Severity: severity,
		File:     normalizedFile,
		Line:     line,
		Snippet:  snippet,
		Message:  message,
	}
	result.Fingerprint = finding.Fingerprint(result)
	if err := finding.Validate(result); err != nil {
		return finding.Finding{}, fmt.Errorf("invalid normalized finding: %w", err)
	}
	return result, nil
}

func readSourceLine(root, reportedFile string, line int) (string, string, error) {
	if root == "" {
		return "", "", errors.New("repository root is required")
	}
	if reportedFile == "" {
		return "", "", errors.New("source file is required")
	}
	if line <= 0 {
		return "", "", fmt.Errorf("source line must be positive, got %d", line)
	}
	if filepath.IsAbs(reportedFile) {
		return "", "", fmt.Errorf("source path %q must be relative", reportedFile)
	}

	cleanFile := filepath.Clean(reportedFile)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository root: %w", err)
	}
	sourceRoot, err := os.OpenRoot(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("open repository root: %w", err)
	}
	defer sourceRoot.Close()

	snippet, err := lineFromRoot(sourceRoot, cleanFile, line)
	if err != nil {
		return "", "", fmt.Errorf("read %s:%d: %w", filepath.ToSlash(cleanFile), line, err)
	}
	return filepath.ToSlash(cleanFile), snippet, nil
}

func lineFromRoot(root *os.Root, path string, wanted int) (string, error) {
	file, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for current := 1; ; current++ {
		line, readErr := reader.ReadString('\n')
		if current == wanted {
			if len(line) == 0 && errors.Is(readErr, io.EOF) {
				return "", errors.New("line is past end of file")
			}
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			return line, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return "", errors.New("line is past end of file")
			}
			return "", readErr
		}
	}
}
