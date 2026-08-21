package normalizer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

func TestGoldenNormalizers(t *testing.T) {
	tests := []struct {
		name     string
		gate     string
		rawCount int
	}{
		{name: "golangci", gate: "lint", rawCount: 2},
		{name: "gocyclo", gate: "complexity", rawCount: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, raw, want := goldenFixture(t, test.name, test.gate)
			got, err := NewRegistry().Normalize(ctx.Binding.Normalizer, ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != test.rawCount {
				t.Fatalf("len(ungrouped findings) = %d, want %d", len(got), test.rawCount)
			}
			for _, normalized := range got {
				if len(normalized.Occurrences) != 0 {
					t.Fatalf("normalizer grouped occurrences: %#v", normalized)
				}
			}
			grouped, err := finding.Group(got)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.MarshalIndent(grouped, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			encoded = append(encoded, '\n')
			if string(encoded) != string(want) {
				t.Fatalf("normalized findings:\n%s\nwant:\n%s", encoded, want)
			}
		})
	}
}

func TestNormalizeEmptyOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		normalizer string
		ctx        Context
	}{
		{name: "golangci", normalizer: "golangci-json"},
		{
			name:       "regex",
			normalizer: `regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
			ctx:        regexContext(""),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewRegistry().Normalize(test.normalizer, test.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("findings = %#v, want none", got)
			}
		})
	}
}

func TestGolangCIRejectsMalformedOutput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "malformed JSON", raw: `{"Issues": [`, want: "invalid JSON"},
		{name: "missing Issues", raw: `{}`, want: "Issues array"},
		{name: "null Issues", raw: `{"Issues": null}`, want: "Issues array"},
		{name: "missing linter", raw: issueJSON("", "message", "warning", "source.go", 1), want: "FromLinter"},
		{name: "missing text", raw: issueJSON("errcheck", "", "warning", "source.go", 1), want: "Text"},
		{name: "missing filename", raw: issueJSON("errcheck", "message", "warning", "", 1), want: "Pos.Filename"},
		{name: "invalid line", raw: issueJSON("errcheck", "message", "warning", "source.go", 0), want: "Pos.Line"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry().Normalize("golangci-json", Context{}, []byte(test.raw))
			if err == nil {
				t.Fatal("error = nil, want malformed output error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want error class %q", err, test.want)
			}
		})
	}
}

func TestGolangCIAllowsExplicitEmptyIssues(t *testing.T) {
	got, err := NewRegistry().Normalize("golangci-json", Context{}, []byte(`{"Issues": []}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("findings = %#v, want none", got)
	}
}

func TestNormalizeRejectsUnknownNormalizer(t *testing.T) {
	_, err := NewRegistry().Normalize("unknown", Context{}, nil)
	if err == nil {
		t.Fatal("error = nil, want unknown normalizer error")
	}
}

func TestRegistryIsOpaqueAndZeroValueIsSafe(t *testing.T) {
	if kind := reflect.TypeOf(NewRegistry()).Kind(); kind != reflect.Struct {
		t.Fatalf("Registry kind = %v, want opaque struct", kind)
	}
	var registry Registry
	if _, err := registry.Normalize("unknown", Context{}, nil); err == nil {
		t.Fatal("zero Registry returned nil error for unknown normalizer")
	}
}

func TestRegistrySupportsConcurrentNormalization(t *testing.T) {
	registry := NewRegistry()
	const workers = 32
	var wait sync.WaitGroup
	errors := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			findings, err := registry.Normalize("golangci-json", Context{}, []byte(`{"Issues":[]}`))
			if err != nil {
				errors <- err
				return
			}
			if len(findings) != 0 {
				errors <- fmt.Errorf("got %d findings, want zero", len(findings))
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestRegexDefinitionValidation(t *testing.T) {
	tests := []struct {
		name       string
		normalizer string
	}{
		{name: "malformed", normalizer: `regex:[`},
		{name: "missing file capture", normalizer: `regex:^(?P<line>\d+)$`},
		{name: "missing line capture", normalizer: `regex:^(?P<file>.+)$`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry().Normalize(test.normalizer, Context{}, nil)
			if err == nil {
				t.Fatal("error = nil, want regex definition error")
			}
		})
	}
}

func TestRegexRejectsDuplicateNamedCaptures(t *testing.T) {
	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>a)(?P<file>b)(?P<line>\d+)$`,
		Context{},
		nil,
	)
	if err == nil {
		t.Fatal("error = nil, want duplicate named capture error")
	}
}

func TestRegexRejectsUnmatchedNonemptyLines(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "line one\nline two\n")
	ctx := regexContext(root)
	const normalizer = `regex:^(?P<file>[^:]+):(?P<line>\d+)$`

	for _, raw := range []string{
		"garbage\n",
		"source.go:1\ngarbage\nsource.go:2\n",
		"source.go:1\n \nsource.go:2\n",
	} {
		got, err := NewRegistry().Normalize(normalizer, ctx, []byte(raw))
		if err == nil {
			t.Fatalf("Normalize(%q) error = nil, want unmatched line error", raw)
		}
		if got != nil {
			t.Fatalf("Normalize(%q) findings = %#v, want nil on error", raw, got)
		}
	}
}

func TestRegexValidatesRawBeforeOpeningSourceRoot(t *testing.T) {
	ctx := regexContext("")
	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
		ctx,
		[]byte("unmatched\n"),
	)
	if err == nil || !strings.Contains(err.Error(), "persisted raw output") {
		t.Fatalf("error = %q, want raw parse error before source-root error", err)
	}
}

func TestRegexRejectsInvalidNumericLine(t *testing.T) {
	ctx := regexContext(t.TempDir())
	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>[^:]+):(?P<line>[^:]+)$`,
		ctx,
		[]byte("source.go:not-a-number\n"),
	)
	if err == nil {
		t.Fatal("error = nil, want invalid line error")
	}
}

func TestRegexMessageTemplateValidation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "line one\n")

	for _, message := range []string{"{{.missing}}", "{{"} {
		ctx := regexContext(root)
		ctx.Binding.Message = message
		_, err := NewRegistry().Normalize(
			`regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
			ctx,
			[]byte("source.go:1\n"),
		)
		if err == nil {
			t.Fatalf("message %q: error = nil, want template error", message)
		}
	}
}

func TestRegexPreflightsBindingWithEmptyOutput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Context)
	}{
		{name: "template key", mutate: func(ctx *Context) { ctx.Binding.Message = "{{.missing}}" }},
		{name: "indirect template key", mutate: func(ctx *Context) { ctx.Binding.Message = `{{index . "missing"}}` }},
		{name: "template key in untaken branch", mutate: func(ctx *Context) {
			ctx.Binding.Message = `{{if eq .file "capture"}}ok{{else}}{{.missing}}{{end}}`
		}},
		{name: "indirect key in untaken branch", mutate: func(ctx *Context) {
			ctx.Binding.Message = `{{if eq .file "capture"}}ok{{else}}{{index . "missing"}}{{end}}`
		}},
		{name: "capture in rebound dot branch", mutate: func(ctx *Context) {
			ctx.Binding.Message = `{{if eq .file "capture"}}ok{{else}}{{with .file}}{{.line}}{{end}}{{end}}`
		}},
		{name: "template invocation without root dot", mutate: func(ctx *Context) {
			ctx.Binding.Message = `{{define "nested"}}{{.file}}{{end}}{{template "nested"}}`
		}},
		{name: "range over captures", mutate: func(ctx *Context) {
			ctx.Binding.Message = `{{range $name, $value := .}}{{$name}}={{$value}}{{end}}`
		}},
		{name: "qualified rule", mutate: func(ctx *Context) { ctx.Binding.RuleID = "complexity" }},
		{name: "default severity", mutate: func(ctx *Context) { ctx.Binding.SeverityMap = nil }},
		{name: "canonical severity", mutate: func(ctx *Context) {
			ctx.Binding.SeverityMap["default"] = finding.Severity("critical")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := regexContext("")
			test.mutate(&ctx)
			_, err := NewRegistry().Normalize(
				`regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
				ctx,
				nil,
			)
			if err == nil {
				t.Fatal("error = nil, want regex binding preflight error")
			}
		})
	}
}

func TestRegexRejectsLeadingAndInteriorBlankLines(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "line one\nline two\n")
	for _, raw := range []string{
		"\nsource.go:1\n",
		"source.go:1\n\nsource.go:2\n",
		"\r\nsource.go:1\r\n",
	} {
		_, err := NewRegistry().Normalize(
			`regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
			regexContext(root),
			[]byte(raw),
		)
		if err == nil {
			t.Fatalf("Normalize(%q) error = nil, want blank-line error", raw)
		}
	}
}

func TestNormalizersRejectInvalidUTF8(t *testing.T) {
	for _, test := range []struct {
		name       string
		normalizer string
		ctx        Context
	}{
		{name: "golangci", normalizer: "golangci-json"},
		{name: "regex", normalizer: `regex:^(?P<file>.+):(?P<line>\d+)$`, ctx: regexContext("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry().Normalize(test.normalizer, test.ctx, []byte{0xff})
			if err == nil {
				t.Fatal("error = nil, want invalid UTF-8 error")
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error = %q, want UTF-8 validation error", err)
			}
		})
	}
}

func TestNormalizerErrorsDoNotExposeRawOutput(t *testing.T) {
	const secret = "RAW_SECRET_SENTINEL"
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "source\n")
	tests := []struct {
		name       string
		normalizer string
		ctx        Context
		raw        string
		wantRawRef bool
	}{
		{
			name:       "regex unmatched line",
			normalizer: `regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
			ctx:        regexContext(root),
			raw:        secret + "\n",
			wantRawRef: true,
		},
		{
			name:       "regex invalid capture",
			normalizer: `regex:^(?P<file>[^:]+):(?P<line>.+)$`,
			ctx:        regexContext(root),
			raw:        "source.go:" + secret + "\n",
			wantRawRef: true,
		},
		{
			name:       "regex source path",
			normalizer: `regex:^(?P<file>.+):(?P<line>\d+)$`,
			ctx:        regexContext(root),
			raw:        secret + ".go:1\n",
		},
		{
			name:       "golangci severity",
			normalizer: "golangci-json",
			ctx: Context{Gate: "lint", Root: root, Binding: gate.Binding{
				Language:    "go",
				SeverityMap: map[string]finding.Severity{"warning": finding.Warning},
			}},
			raw: issueJSON("errcheck", "message", secret, "source.go", 1),
		},
		{
			name:       "golangci multiple values",
			normalizer: "golangci-json",
			raw:        `{"Issues":[]} "` + secret + `"`,
			wantRawRef: true,
		},
		{
			name:       "golangci source path",
			normalizer: "golangci-json",
			ctx: Context{Gate: "lint", Root: root, Binding: gate.Binding{
				Language:    "go",
				SeverityMap: map[string]finding.Severity{"default": finding.Warning},
			}},
			raw: issueJSON("errcheck", "message", "warning", secret+".go", 1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry().Normalize(test.normalizer, test.ctx, []byte(test.raw))
			if err == nil {
				t.Fatal("error = nil, want normalization error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error exposes raw output: %q", err)
			}
			if test.wantRawRef && !strings.Contains(err.Error(), "persisted raw output") {
				t.Fatalf("error = %q, want persisted raw output guidance", err)
			}
		})
	}
}

func TestSeverityMappingUsesExactKeyThenDefault(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "line one\nline two\n")
	ctx := Context{
		Gate: "lint",
		Root: root,
		Binding: gate.Binding{
			Language: "go",
			SeverityMap: map[string]finding.Severity{
				"warning": finding.Info,
				"default": finding.Warning,
			},
		},
	}
	raw := `{"Issues":[` +
		`{"FromLinter":"one","Text":"first","Severity":"warning","Pos":{"Filename":"source.go","Line":1}},` +
		`{"FromLinter":"two","Text":"second","Severity":"style","Pos":{"Filename":"source.go","Line":2}}` +
		`]}`
	got, err := NewRegistry().Normalize("golangci-json", ctx, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Severity != finding.Info || got[1].Severity != finding.Warning {
		t.Fatalf("severities = %#v, want exact info then default warning", got)
	}
}

func TestNormalizeRejectsMissingSeverityMappingAndDefault(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "line one\n")
	ctx := Context{
		Gate: "lint",
		Root: root,
		Binding: gate.Binding{
			Language:    "go",
			SeverityMap: map[string]finding.Severity{"error": finding.Error},
		},
	}
	_, err := NewRegistry().Normalize(
		"golangci-json",
		ctx,
		[]byte(issueJSON("errcheck", "message", "warning", "source.go", 1)),
	)
	if err == nil {
		t.Fatal("golangci error = nil, want missing severity mapping error")
	}

	ctx = regexContext(root)
	ctx.Binding.SeverityMap = nil
	_, err = NewRegistry().Normalize(
		`regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
		ctx,
		[]byte("source.go:1\n"),
	)
	if err == nil {
		t.Fatal("regex error = nil, want missing default severity error")
	}
}

func TestSourceLookupRejectsUnsafeOrInvalidLocations(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.go")
	writeTestFile(t, outside, "outside\n")
	writeTestFile(t, filepath.Join(root, "source.go"), "inside\n")

	tests := []struct {
		name string
		file string
		line int
	}{
		{name: "missing source", file: "missing.go", line: 1},
		{name: "line past EOF", file: "source.go", line: 2},
		{name: "path traversal", file: "../outside.go", line: 1},
		{name: "absolute outside path", file: outside, line: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := regexContext(root)
			raw := fmt.Sprintf("%s:%d\n", test.file, test.line)
			_, err := NewRegistry().Normalize(
				`regex:^(?P<file>.+):(?P<line>\d+)$`,
				ctx,
				[]byte(raw),
			)
			if err == nil {
				t.Fatal("error = nil, want source location error")
			}
		})
	}
}

func TestSourceLookupAcceptsAbsolutePathInsideRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "nested", "source.go")
	if err := os.Mkdir(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, inside, "inside\n")

	got, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte(fmt.Sprintf("%s:1\n", inside)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "nested/source.go" || got[0].Snippet != "inside" {
		t.Fatalf("findings = %#v, want root-relative absolute source", got)
	}
}

func TestSourceLookupRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.go")
	writeTestFile(t, outside, "outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte("escape.go:1\n"),
	)
	if err == nil {
		t.Fatal("error = nil, want symlink escape error")
	}
}

func TestSourceSessionRejectsNonRegularFile(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	session, err := openSourceSession(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	file, err := openRegularSource(session.root, "directory")
	if err == nil {
		file.Close()
		t.Fatal("non-regular source opened successfully")
	}
}

func TestSourceSessionStaysAnchoredAndCloses(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "repo")
	outsidePath := filepath.Join(parent, "outside")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsidePath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(rootPath, "first.go"), "original first\n")
	writeTestFile(t, filepath.Join(rootPath, "second.go"), "original second\n")
	writeTestFile(t, filepath.Join(outsidePath, "second.go"), "outside second\n")

	session, err := openSourceSession(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	firstFile, firstSnippet, err := session.readLine("first.go", 1)
	if err != nil {
		t.Fatal(err)
	}
	if firstFile != "first.go" || firstSnippet != "original first" {
		t.Fatalf("first read = (%q, %q)", firstFile, firstSnippet)
	}
	if err := os.Rename(rootPath, filepath.Join(parent, "moved-repo")); err != nil {
		session.close()
		t.Skipf("cannot rename an open repository root: %v", err)
	}
	if err := os.Symlink(outsidePath, rootPath); err != nil {
		session.close()
		t.Skipf("symlinks unavailable: %v", err)
	}

	secondFile, secondSnippet, err := session.readLine("second.go", 1)
	if err != nil {
		session.close()
		t.Fatal(err)
	}
	if secondFile != "second.go" || secondSnippet != "original second" {
		session.close()
		t.Fatalf("second read = (%q, %q), want retained repository", secondFile, secondSnippet)
	}
	if err := session.close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.readLine("second.go", 1); err == nil {
		t.Fatal("read after close succeeded")
	}
}

func TestSourceLineRejectsPartialBytesOnReadError(t *testing.T) {
	_, err := lineFromReader(errorAfterDataReader{}, 1)
	if err == nil {
		t.Fatal("error = nil, want non-EOF read error")
	}
}

func TestSourceLookupHandlesCRLFLongLinesAndNativePaths(t *testing.T) {
	root := t.TempDir()
	longLine := strings.Repeat("x", 32*1024)
	writeTestFile(t, filepath.Join(root, "source.go"), "first\r\n"+longLine+"\r\n")
	separator := string(filepath.Separator)
	toolPath := "nested" + separator + ".." + separator + "source.go"
	raw := fmt.Sprintf("%s:1\r\n%s:2\r\n", toolPath, toolPath)

	got, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte(raw),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(findings) = %d, want 2", len(got))
	}
	if got[0].Snippet != "first" || got[1].Snippet != longLine {
		t.Fatalf("snippets were not preserved: first=%q long-len=%d", got[0].Snippet, len(got[1].Snippet))
	}
	wantFile := filepath.ToSlash(filepath.Clean(toolPath))
	if got[0].File != wantFile || got[1].File != wantFile {
		t.Fatalf("files = %q, %q; want %q", got[0].File, got[1].File, wantFile)
	}
}

func TestSourceLookupRejectsOverlongLine(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), strings.Repeat("x", 64*1024+1)+"\n")
	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte("source.go:1\n"),
	)
	if err == nil {
		t.Fatal("error = nil, want overlong source line error")
	}
}

func TestSourceLookupAcceptsMaximumLengthLine(t *testing.T) {
	root := t.TempDir()
	want := strings.Repeat("x", maxSnippetBytes)
	writeTestFile(t, filepath.Join(root, "source.go"), want+"\r\n")
	got, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte("source.go:1\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Snippet != want {
		t.Fatalf("snippet length = %d, want %d", len(got[0].Snippet), len(want))
	}
}

func TestSourceLookupRejectsInvalidUTF8WithoutExposingSource(t *testing.T) {
	const sourceName = "RAW_SECRET_SOURCE.go"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, sourceName), []byte{0xff, '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte(sourceName+":1\n"),
	)
	if err == nil {
		t.Fatal("error = nil, want invalid source UTF-8 error")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error = %q, want UTF-8 classification", err)
	}
	if strings.Contains(err.Error(), sourceName) || strings.Contains(err.Error(), "\\xff") {
		t.Fatalf("error exposes source data or path: %q", err)
	}
}

func TestSourceLookupIgnoresInvalidUTF8BeforeRequestedLine(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "source.go"),
		[]byte{0xff, '\n', 'v', 'a', 'l', 'i', 'd', '\n'},
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	got, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		regexContext(root),
		[]byte("source.go:2\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Snippet != "valid" {
		t.Fatalf("findings = %#v, want valid requested snippet", got)
	}
}

func TestNormalizeValidatesEveryFindingAndSetsFingerprint(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), "line one\n")
	ctx := regexContext(root)
	ctx.Gate = ""
	_, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		ctx,
		[]byte("source.go:1\n"),
	)
	if err == nil {
		t.Fatal("error = nil, want finding validation error")
	}

	ctx.Gate = "complexity"
	got, err := NewRegistry().Normalize(
		`regex:^(?P<file>.+):(?P<line>\d+)$`,
		ctx,
		[]byte("source.go:1\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Fingerprint == "" || got[0].Fingerprint != finding.Fingerprint(got[0]) {
		t.Fatalf("findings = %#v, want one canonical fingerprint", got)
	}
}

func goldenFixture(t *testing.T, name, gateName string) (Context, []byte, []byte) {
	t.Helper()
	loaded, err := (gate.Loader{}).Load(gateName)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := loaded.Bindings["go"]
	if !ok {
		t.Fatalf("gate %q has no Go binding", gateName)
	}

	fixtureDir := filepath.Join("testdata", name)
	raw := readTestFile(t, filepath.Join(fixtureDir, "output.raw"))
	want := readTestFile(t, filepath.Join(fixtureDir, "want.json"))
	source := readTestFile(t, filepath.Join(fixtureDir, "source.go"))
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.go"), string(source))

	return Context{Gate: loaded.Manifest.Name, Root: root, Binding: binding}, raw, want
}

func regexContext(root string) Context {
	return Context{
		Gate: "complexity",
		Root: root,
		Binding: gate.Binding{
			Language:    "go",
			RuleID:      "gocyclo/complexity",
			Message:     "finding at {{.file}}:{{.line}}",
			SeverityMap: map[string]finding.Severity{"default": finding.Warning},
		},
	}
}

func issueJSON(linter, message, severity, file string, line int) string {
	return fmt.Sprintf(
		`{"Issues":[{"FromLinter":%q,"Text":%q,"Severity":%q,"Pos":{"Filename":%q,"Line":%d}}]}`,
		linter,
		message,
		severity,
		file,
		line,
	)
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

type errorAfterDataReader struct{}

func (errorAfterDataReader) Read(buffer []byte) (int, error) {
	return copy(buffer, "partial source bytes"), errors.New("injected read failure")
}
