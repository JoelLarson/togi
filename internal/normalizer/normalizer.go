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
)

const (
	// maxSnippetBytes bounds source retained in a finding, excluding its line terminator.
	maxSnippetBytes   = 64 * 1024
	rawOutputGuidance = "inspect persisted raw output"
)

var (
	errLinePastEOF = errors.New("source line is past end of file")
	errLineTooLong = errors.New("source line exceeds 64 KiB")
	errInvalidUTF8 = errors.New("source line is not valid UTF-8")
)

// Context supplies the gate metadata and repository used during normalization.
type Context struct {
	Gate string
	Root string
}

// Config is the binding-derived data a normalizer is compiled against.
type Config struct {
	Language    string
	RuleID      string
	Message     string
	SeverityMap map[string]finding.Severity
}

// Normalizer is one tool's parser, compiled against its binding's Config.
type Normalizer interface {
	Normalize(ctx Context, raw []byte) ([]finding.Finding, error)
}

// Parse resolves and compiles a normalizer name against its binding config.
// This is the single home of the normalizer-name grammar: a name that Parse
// accepts is guaranteed runnable, so grammar mistakes surface at gate load
// time rather than as errored gates mid-run.
func Parse(name string, cfg Config) (Normalizer, error) {
	if strings.HasPrefix(name, "regex:") {
		return parseRegex(strings.TrimPrefix(name, "regex:"), cfg)
	}
	switch name {
	case "golangci-json":
		return golangciNormalizer{config: cfg}, nil
	case "":
		return nil, errors.New("normalizer name is required")
	default:
		return nil, fmt.Errorf("unknown normalizer %q", name)
	}
}

func validUTF8Raw(kind string, raw []byte) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("%s output is not valid UTF-8; %s", kind, rawOutputGuidance)
	}
	return nil
}

func mappedSeverity(cfg Config, toolSeverity string) (finding.Severity, error) {
	if severity, ok := cfg.SeverityMap[toolSeverity]; ok {
		return severity, nil
	}
	if severity, ok := cfg.SeverityMap["default"]; ok {
		return severity, nil
	}
	return "", errors.New("tool severity has no mapping and no default")
}

func makeFinding(ctx Context, cfg Config, sources *sourceSession, ruleID, toolSeverity, file string, line int, message string) (finding.Finding, error) {
	severity, err := mappedSeverity(cfg, toolSeverity)
	if err != nil {
		return finding.Finding{}, err
	}
	normalizedFile, snippet, err := sources.readLine(file, line)
	if err != nil {
		return finding.Finding{}, err
	}

	result := finding.Finding{
		Gate:     ctx.Gate,
		Language: cfg.Language,
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

	path, err := finding.NewPath(s.canonicalRoot, reportedFile)
	if err != nil {
		return "", "", errors.New("source path is outside repository root")
	}

	file, err := openRegularSource(s.root, filepath.FromSlash(path.String()))
	if err != nil {
		return "", "", fmt.Errorf("source file cannot be opened; %s", rawOutputGuidance)
	}
	defer func() { _ = file.Close() }()
	snippet, err := lineFromReader(file, line)
	if err != nil {
		if errors.Is(err, errInvalidUTF8) {
			return "", "", fmt.Errorf("source line is not valid UTF-8; %s", rawOutputGuidance)
		}
		return "", "", fmt.Errorf("source line cannot be read; %s", rawOutputGuidance)
	}
	return path.String(), snippet, nil
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
		_ = file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() {
		_ = file.Close()
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
			if !utf8.Valid(line) {
				return "", errInvalidUTF8
			}
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
