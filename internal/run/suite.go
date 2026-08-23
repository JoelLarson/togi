package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/runner"
)

type SuiteStatus string

const (
	SuitePassed  SuiteStatus = "passed"
	SuiteFailed  SuiteStatus = "failed"
	SuiteMissing SuiteStatus = "missing"
	SuiteErrored SuiteStatus = "errored"
)

const suiteDiagnosticLimit = 64 << 10

const suiteListOutputLimit = 1 << 20

var (
	suiteTruncationMarker = []byte("\n[togi: output truncated]\n")
	ErrSuiteCanceled      = errors.New("behavioral suite canceled")
)

type SuiteResult struct {
	Command    []string    `json:"command"`
	Packages   []string    `json:"packages,omitempty"`
	Status     SuiteStatus `json:"status"`
	DurationMS int64       `json:"duration_ms"`
	Diagnostic string      `json:"diagnostic,omitempty"`
}

// GoSuite discovers and executes the repository's Go behavioral suite.
type GoSuite struct {
	executable       string
	now              func() time.Time
	runCommand       commandRunner
	inspectFile      suiteFileInspector
	discoverPackages func(context.Context, string) ([]string, error)
}

type suiteFileInspector func(context.Context, string, string) (bool, error)

func NewGoSuite(executable string) *GoSuite {
	return &GoSuite{executable: executable}
}

type listedGoPackage struct {
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

// Discover asks the configured Go executable for the active package universe,
// then inspects only test files selected by that tool invocation.
func (s *GoSuite) Discover(ctx context.Context, root string) ([]string, error) {
	if ctx == nil {
		return nil, errors.New("behavioral suite context is required")
	}
	if s == nil {
		return nil, errors.New("Go behavioral suite is required")
	}
	if strings.TrimSpace(s.executable) == "" {
		return nil, errors.New("behavioral suite executable is required")
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("behavioral suite root is required")
	}
	if ctx.Err() != nil {
		return nil, suiteCancellationError(ctx, nil, nil)
	}
	run := s.runCommand
	if run == nil {
		run = runGoListCommand
	}
	process := run(ctx, root, []string{s.executable, "list", "-e", "-json=Dir,TestGoFiles,XTestGoFiles", "./..."})
	if ctx.Err() != nil {
		return nil, suiteCancellationError(ctx, nil, nil)
	}
	if process.CleanupErr != nil {
		return finishGoDiscovery(ctx, nil, errors.New("clean up Go package discovery"))
	}
	if bufferTruncated(process.Stdout) || bufferTruncated(process.Stderr) {
		return finishGoDiscovery(ctx, nil, errors.New("Go package discovery output exceeded its capture limit"))
	}
	if process.RunErr != nil {
		var exitErr *exec.ExitError
		if errors.As(process.RunErr, &exitErr) {
			return finishGoDiscovery(ctx, nil, errors.New("Go package discovery failed"))
		}
		return finishGoDiscovery(ctx, nil, errors.New("start Go package discovery"))
	}
	inspectFile := s.inspectFile
	if inspectFile == nil {
		inspectFile = inspectGoTestFile
	}
	packages, err := discoverListedGoPackages(ctx, root, process.Stdout.Bytes(), inspectFile)
	return finishGoDiscovery(ctx, packages, err)
}

func finishGoDiscovery(ctx context.Context, packages []string, resultErr error) ([]string, error) {
	if ctx.Err() != nil {
		return nil, suiteCancellationError(ctx, nil, nil)
	}
	return packages, resultErr
}

func discoverListedGoPackages(ctx context.Context, root string, raw []byte, inspectFile suiteFileInspector) ([]string, error) {
	if err := postListCancellation(ctx); err != nil {
		return nil, err
	}
	rootAbs, err := filepath.Abs(root)
	if cancelErr := postListCancellation(ctx); cancelErr != nil {
		return nil, cancelErr
	}
	if err != nil {
		return nil, errors.New("resolve behavioral suite root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if cancelErr := postListCancellation(ctx); cancelErr != nil {
		return nil, cancelErr
	}
	if err != nil {
		return nil, errors.New("resolve behavioral suite root")
	}
	packages := make(map[string]struct{})
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		if err := postListCancellation(ctx); err != nil {
			return nil, err
		}
		var listed listedGoPackage
		decodeErr := decoder.Decode(&listed)
		if err := postListCancellation(ctx); err != nil {
			return nil, err
		}
		if decodeErr != nil {
			if errors.Is(decodeErr, io.EOF) {
				break
			}
			return nil, errors.New("decode Go package discovery: malformed JSON stream")
		}
		pkg, runnable, err := inspectListedGoPackage(ctx, resolvedRoot, listed, inspectFile)
		if cancelErr := postListCancellation(ctx); cancelErr != nil {
			return nil, cancelErr
		}
		if err != nil {
			return nil, err
		}
		if runnable {
			packages[pkg] = struct{}{}
		}
	}
	result := make([]string, 0, len(packages))
	for pkg := range packages {
		result = append(result, pkg)
	}
	slices.Sort(result)
	if err := postListCancellation(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func inspectListedGoPackage(ctx context.Context, root string, listed listedGoPackage, inspectFile suiteFileInspector) (string, bool, error) {
	if err := postListCancellation(ctx); err != nil {
		return "", false, err
	}
	if listed.Dir == "" || !filepath.IsAbs(listed.Dir) {
		return "", false, errors.New("Go package discovery returned an invalid package directory")
	}
	dir, err := filepath.EvalSymlinks(listed.Dir)
	if cancelErr := postListCancellation(ctx); cancelErr != nil {
		return "", false, cancelErr
	}
	if err != nil || !pathWithinRoot(root, dir) {
		return "", false, errors.New("Go package discovery returned a package outside the repository")
	}
	relativeDir, err := filepath.Rel(root, dir)
	if cancelErr := postListCancellation(ctx); cancelErr != nil {
		return "", false, cancelErr
	}
	if err != nil || filepath.IsAbs(relativeDir) {
		return "", false, errors.New("resolve discovered Go package")
	}
	pkg := "."
	if relativeDir != "." {
		pkg = "./" + filepath.ToSlash(relativeDir)
	}
	files := append(slices.Clone(listed.TestGoFiles), listed.XTestGoFiles...)
	runnablePackage := false
	for _, name := range files {
		if err := postListCancellation(ctx); err != nil {
			return "", false, err
		}
		if name == "" || filepath.IsAbs(name) || filepath.Base(name) != name || strings.Contains(name, `\`) || !strings.HasSuffix(name, "_test.go") {
			return "", false, errors.New("Go package discovery returned an invalid test file")
		}
		path, err := filepath.EvalSymlinks(filepath.Join(dir, name))
		if cancelErr := postListCancellation(ctx); cancelErr != nil {
			return "", false, cancelErr
		}
		if err != nil || !pathWithinRoot(root, path) {
			return "", false, errors.New("Go package discovery returned a test file outside the repository")
		}
		info, err := os.Stat(path)
		if cancelErr := postListCancellation(ctx); cancelErr != nil {
			return "", false, cancelErr
		}
		if err != nil || !info.Mode().IsRegular() {
			return "", false, errors.New("Go package discovery returned a non-regular test file")
		}
		displayPath, err := filepath.Rel(root, path)
		if cancelErr := postListCancellation(ctx); cancelErr != nil {
			return "", false, cancelErr
		}
		if err != nil {
			return "", false, errors.New("resolve discovered Go test file")
		}
		runnable, err := inspectFile(ctx, path, filepath.ToSlash(displayPath))
		if cancelErr := postListCancellation(ctx); cancelErr != nil {
			return "", false, cancelErr
		}
		if err != nil {
			return "", false, err
		}
		if runnable {
			runnablePackage = true
		}
	}
	if err := postListCancellation(ctx); err != nil {
		return "", false, err
	}
	return pkg, runnablePackage, nil
}

func inspectGoTestFile(ctx context.Context, path, displayPath string) (bool, error) {
	if err := postListCancellation(ctx); err != nil {
		return false, err
	}
	source, err := os.ReadFile(path)
	if cancelErr := postListCancellation(ctx); cancelErr != nil {
		return false, cancelErr
	}
	if err != nil {
		return false, errors.New("read discovered Go test file")
	}
	file, err := parser.ParseFile(token.NewFileSet(), displayPath, source, parser.ParseComments)
	if cancelErr := postListCancellation(ctx); cancelErr != nil {
		return false, cancelErr
	}
	if err != nil {
		return false, errors.New("parse discovered Go test file: malformed Go source")
	}
	genericExamples := make(map[string]struct{})
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		switch {
		case validTargetName(fn.Name.Name, "Test") && validTestingSignature(fn.Type, "T"):
			return true, nil
		case validTargetName(fn.Name.Name, "Fuzz") && validTestingSignature(fn.Type, "F"):
			return true, nil
		case validTargetName(fn.Name.Name, "Example") && !noTypeParameters(fn.Type):
			genericExamples[fn.Name.Name] = struct{}{}
		}
	}
	for _, example := range doc.Examples(file) {
		if _, generic := genericExamples["Example"+example.Name]; generic {
			continue
		}
		if example.Output != "" || example.EmptyOutput {
			return true, nil
		}
	}
	if err := postListCancellation(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func postListCancellation(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return suiteCancellationError(ctx, nil, nil)
	}
	return nil
}

func validTargetName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := name[len(prefix):]
	if suffix == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	return !unicode.IsLower(r)
}

func validTestingSignature(fn *ast.FuncType, target string) bool {
	if fn == nil || !noResults(fn) || !noTypeParameters(fn) || fn.Params == nil || len(fn.Params.List) != 1 {
		return false
	}
	parameter := fn.Params.List[0]
	if len(parameter.Names) > 1 {
		return false
	}
	pointer, ok := parameter.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if identifier, ok := pointer.X.(*ast.Ident); ok {
		return identifier.Name == target
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == target
}

func noResults(fn *ast.FuncType) bool {
	return fn.Results == nil || len(fn.Results.List) == 0
}

func noTypeParameters(fn *ast.FuncType) bool {
	return fn.TypeParams == nil || len(fn.TypeParams.List) == 0
}

// Run executes either the full repository suite or a local package subset.
func (s *GoSuite) Run(ctx context.Context, root string, packages []string, full bool) (result SuiteResult, resultErr error) {
	now := time.Now
	if s != nil && s.now != nil {
		now = s.now
	}
	started := now()
	defer func() {
		duration := now().Sub(started)
		if duration < 0 {
			duration = 0
		}
		result.DurationMS = duration.Milliseconds()
	}()

	if ctx == nil {
		return invalidSuiteResult(result, "behavioral suite context is required")
	}
	if s == nil {
		return invalidSuiteResult(result, "Go behavioral suite is required")
	}
	if strings.TrimSpace(s.executable) == "" {
		return invalidSuiteResult(result, "behavioral suite executable is required")
	}
	if strings.TrimSpace(root) == "" {
		return invalidSuiteResult(result, "behavioral suite root is required")
	}
	if err := ctx.Err(); err != nil {
		result.Status = SuiteErrored
		result.Diagnostic, resultErr = suiteCancellation(ctx, nil, nil)
		return result, resultErr
	}
	if full {
		discover := s.discoverPackages
		if discover == nil {
			discover = s.Discover
		}
		discovered, err := discover(ctx, root)
		if ctx.Err() != nil {
			result.Status = SuiteErrored
			result.Diagnostic, resultErr = suiteCancellation(ctx, nil, nil)
			return result, resultErr
		}
		if err != nil {
			result.Status = SuiteErrored
			result.Diagnostic = boundSuiteDiagnostic(err.Error())
			if errors.Is(err, ErrSuiteCanceled) {
				return result, err
			}
			return result, nil
		}
		if len(discovered) == 0 {
			result.Status = SuiteMissing
			return result, nil
		}
		result.Command = []string{s.executable, "test", "./..."}
	} else {
		normalized, err := normalizeSuitePackages(root, packages)
		if err != nil {
			return invalidSuiteResult(result, err.Error())
		}
		result.Packages = normalized
		if len(result.Packages) == 0 {
			result.Packages = nil
			result.Status = SuiteMissing
			return result, nil
		}
		result.Command = append([]string{s.executable, "test"}, result.Packages...)
	}
	if ctx.Err() != nil {
		result.Command = nil
		result.Status = SuiteErrored
		result.Diagnostic, resultErr = suiteCancellation(ctx, nil, nil)
		return result, resultErr
	}

	run := s.runCommand
	if run == nil {
		run = runSuiteCommand
	}
	process := run(ctx, root, result.Command)
	result.Diagnostic = combineSuiteDiagnostic(process.Stdout, process.Stderr)
	if err := ctx.Err(); err != nil {
		result.Status = SuiteErrored
		result.Diagnostic, resultErr = suiteCancellation(ctx, process.RunErr, process.CleanupErr)
		return result, resultErr
	}
	if process.CleanupErr != nil {
		result.Status = SuiteErrored
		result.Diagnostic = suiteErrorDiagnostic("clean up behavioral suite", process.CleanupErr, result.Diagnostic)
		return result, nil
	}
	if process.RunErr == nil {
		result.Status = SuitePassed
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(process.RunErr, &exitErr) {
		result.Status = SuiteFailed
		return result, nil
	}
	result.Status = SuiteErrored
	result.Diagnostic = suiteErrorDiagnostic("start behavioral suite", process.RunErr, result.Diagnostic)
	return result, nil
}

func runSuiteCommand(ctx context.Context, root string, command []string) runner.Result {
	return runner.Run(ctx, root, command, runner.Options{
		StdoutLimit:      suiteDiagnosticLimit,
		StderrLimit:      suiteDiagnosticLimit,
		TruncationMarker: suiteTruncationMarker,
	})
}

func runGoListCommand(ctx context.Context, root string, command []string) runner.Result {
	return runner.Run(ctx, root, command, runner.Options{
		StdoutLimit:      suiteListOutputLimit,
		StderrLimit:      suiteDiagnosticLimit,
		TruncationMarker: suiteTruncationMarker,
	})
}

func bufferTruncated(buffer *runner.Buffer) bool {
	return buffer != nil && buffer.Truncated()
}

func normalizeSuitePackages(root string, packages []string) ([]string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.New("resolve behavioral suite root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, errors.New("resolve behavioral suite root")
	}
	unique := make(map[string]struct{}, len(packages))
	for _, pkg := range packages {
		canonical, err := canonicalSuitePackage(rootAbs, resolvedRoot, pkg)
		if err != nil {
			return nil, err
		}
		unique[canonical] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for pkg := range unique {
		result = append(result, pkg)
	}
	slices.Sort(result)
	return result, nil
}

func canonicalSuitePackage(rootAbs, resolvedRoot, pkg string) (string, error) {
	if pkg == "" || strings.TrimSpace(pkg) != pkg || strings.ContainsRune(pkg, 0) || strings.Contains(pkg, `\`) {
		return "", errors.New("invalid local behavioral suite package")
	}
	if pathpkg.IsAbs(pkg) || strings.HasPrefix(pkg, "//") || windowsVolumePath(pkg) {
		return "", errors.New("local behavioral suite package must be repository-relative")
	}
	cleaned := pathpkg.Clean(pkg)
	if windowsVolumePath(cleaned) {
		return "", errors.New("local behavioral suite package must be repository-relative")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "...") {
		return "", errors.New("local behavioral suite package escapes the repository")
	}
	if cleaned == "." {
		return ".", nil
	}
	resolved, err := resolveNearestExisting(filepath.Join(rootAbs, filepath.FromSlash(cleaned)))
	if err != nil {
		return "", errors.New("resolve local behavioral suite package")
	}
	if !pathWithinRoot(resolvedRoot, resolved) {
		return "", errors.New("local behavioral suite package escapes the repository")
	}
	return "./" + cleaned, nil
}

func windowsVolumePath(path string) bool {
	return len(path) >= 2 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':'
}

func resolveNearestExisting(path string) (string, error) {
	for {
		_, err := os.Lstat(path)
		if err == nil {
			return filepath.EvalSymlinks(path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}

func invalidSuiteResult(result SuiteResult, diagnostic string) (SuiteResult, error) {
	result.Status = SuiteErrored
	result.Diagnostic = diagnostic
	return result, errors.New(diagnostic)
}

func combineSuiteDiagnostic(stdout, stderr *runner.Buffer) string {
	parts := make([]string, 0, 2)
	if stdout != nil && len(stdout.Bytes()) != 0 {
		parts = append(parts, strings.TrimSpace(string(stdout.Bytes())))
	}
	if stderr != nil && len(stderr.Bytes()) != 0 {
		parts = append(parts, strings.TrimSpace(string(stderr.Bytes())))
	}
	return boundSuiteDiagnostic(strings.Join(parts, "\n"))
}

func suiteErrorDiagnostic(prefix string, err error, output string) string {
	diagnostic := prefix + ": " + err.Error()
	if output != "" {
		diagnostic += "\n" + output
	}
	return boundSuiteDiagnostic(diagnostic)
}

func suiteCancellation(ctx context.Context, runErr, cleanupErr error) (string, error) {
	err := suiteCancellationError(ctx, runErr, cleanupErr)
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	diagnostic := boundSuiteDiagnostic("behavioral suite canceled: " + cause.Error())
	return diagnostic, err
}

func suiteCancellationError(ctx context.Context, runErr, cleanupErr error) error {
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return errors.Join(ErrSuiteCanceled, ctx.Err(), cause, runErr, cleanupErr)
}

func boundSuiteDiagnostic(diagnostic string) string {
	buffer := runner.NewBuffer(suiteDiagnosticLimit, suiteTruncationMarker)
	_, _ = buffer.Write([]byte(diagnostic))
	return string(buffer.Bytes())
}
