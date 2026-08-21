package normalizer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

const (
	// maxSnippetBytes bounds source retained in a finding, excluding its line terminator.
	maxSnippetBytes   = 64 * 1024
	rawOutputGuidance = "inspect persisted raw output"
)

var (
	errLinePastEOF = errors.New("source line is past end of file")
	errLineTooLong = errors.New("source line exceeds 64 KiB")
)

// Context supplies the gate metadata and repository used during normalization.
type Context struct {
	Gate    string
	Root    string
	Binding gate.Binding
}

// Func converts one tool's raw output into normalized findings.
type Func func(Context, []byte) ([]finding.Finding, error)

// Registry dispatches immutable compiled normalizers by stable name.
type Registry struct {
	funcs map[string]Func
}

// NewRegistry returns all compiled normalizers.
func NewRegistry() Registry {
	return Registry{funcs: map[string]Func{
		"golangci-json": normalizeGolangCI,
	}}
}

// Normalize dispatches to a compiled normalizer or a data-defined regex.
func (r Registry) Normalize(name string, ctx Context, raw []byte) ([]finding.Finding, error) {
	if strings.HasPrefix(name, "regex:") {
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("regex output is not valid UTF-8; %s", rawOutputGuidance)
		}
		return normalizeRegex(strings.TrimPrefix(name, "regex:"), ctx, raw)
	}
	normalize, ok := r.funcs[name]
	if !ok {
		return nil, fmt.Errorf("unknown normalizer %q", name)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("normalizer output is not valid UTF-8; %s", rawOutputGuidance)
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
	return "", errors.New("tool severity has no mapping and no default")
}

func makeFinding(ctx Context, sources *sourceSession, ruleID, toolSeverity, file string, line int, message string) (finding.Finding, error) {
	severity, err := mappedSeverity(ctx.Binding, toolSeverity)
	if err != nil {
		return finding.Finding{}, err
	}
	normalizedFile, snippet, err := sources.readLine(file, line)
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
		return finding.Finding{}, errors.New("normalized finding is invalid")
	}
	return result, nil
}

type sourceSession struct {
	root          *os.Root
	canonicalRoot string
}

func openSourceSession(rootPath string) (*sourceSession, error) {
	if rootPath == "" {
		return nil, errors.New("repository root is required")
	}
	absoluteRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, errors.New("repository root cannot be resolved")
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, errors.New("repository root cannot be resolved")
	}
	root, err := os.OpenRoot(canonicalRoot)
	if err != nil {
		return nil, errors.New("repository root cannot be opened")
	}
	return &sourceSession{root: root, canonicalRoot: canonicalRoot}, nil
}

func (s *sourceSession) close() error {
	if s == nil || s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}

func (s *sourceSession) readLine(reportedFile string, line int) (string, string, error) {
	if s == nil || s.root == nil {
		return "", "", errors.New("source session is closed")
	}
	if reportedFile == "" {
		return "", "", errors.New("source file is required")
	}
	if line <= 0 {
		return "", "", errors.New("source line is invalid")
	}

	cleanFile := filepath.Clean(reportedFile)
	if filepath.IsAbs(cleanFile) {
		relative, err := filepath.Rel(s.canonicalRoot, cleanFile)
		if err != nil || pathEscapesRoot(relative) {
			return "", "", errors.New("source path is outside repository root")
		}
		cleanFile = relative
	} else if pathEscapesRoot(cleanFile) {
		return "", "", errors.New("source path is outside repository root")
	}

	file, err := openRegularSource(s.root, cleanFile)
	if err != nil {
		return "", "", fmt.Errorf("source file cannot be opened; %s", rawOutputGuidance)
	}
	defer file.Close()
	snippet, err := lineFromReader(file, line)
	if err != nil {
		return "", "", fmt.Errorf("source line cannot be read; %s", rawOutputGuidance)
	}
	return filepath.ToSlash(cleanFile), snippet, nil
}

func pathEscapesRoot(path string) bool {
	return path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) || filepath.IsAbs(path)
}

func openRegularSource(root *os.Root, path string) (*os.File, error) {
	before, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	file, err := openSourceFile(root, path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("source changed to a non-regular file")
	}
	return file, nil
}

func lineFromReader(input io.Reader, wanted int) (string, error) {
	reader := bufio.NewReaderSize(input, maxSnippetBytes+2)
	for current := 1; ; current++ {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			return "", errLineTooLong
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return "", errLinePastEOF
		}
		line = bytesWithoutLineTerminator(line)
		if len(line) > maxSnippetBytes {
			return "", errLineTooLong
		}
		if current == wanted {
			return string(line), nil
		}
		if errors.Is(readErr, io.EOF) {
			return "", errLinePastEOF
		}
	}
}

func bytesWithoutLineTerminator(line []byte) []byte {
	line = bytesTrimSuffix(line, '\n')
	return bytesTrimSuffix(line, '\r')
}

func bytesTrimSuffix(value []byte, suffix byte) []byte {
	if len(value) > 0 && value[len(value)-1] == suffix {
		return value[:len(value)-1]
	}
	return value
}
