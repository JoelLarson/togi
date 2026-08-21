package normalizer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	for _, name := range []string{
		"golangci-json",
		`regex:^(?P<file>[^:]+):(?P<line>\d+)$`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NewRegistry().Normalize(name, Context{}, nil)
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
	}{
		{name: "malformed JSON", raw: `{"Issues": [`},
		{name: "missing Issues", raw: `{}`},
		{name: "null Issues", raw: `{"Issues": null}`},
		{name: "missing linter", raw: issueJSON("", "message", "warning", "source.go", 1)},
		{name: "missing text", raw: issueJSON("errcheck", "", "warning", "source.go", 1)},
		{name: "missing filename", raw: issueJSON("errcheck", "message", "warning", "", 1)},
		{name: "invalid line", raw: issueJSON("errcheck", "message", "warning", "source.go", 0)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry().Normalize("golangci-json", Context{}, []byte(test.raw))
			if err == nil {
				t.Fatal("error = nil, want malformed output error")
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
		{name: "absolute path", file: outside, line: 1},
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

func TestLineFromRootStaysAnchoredWhenRootPathIsReplaced(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "repo")
	outsidePath := filepath.Join(parent, "outside")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsidePath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(rootPath, "source.go"), "inside\n")
	writeTestFile(t, filepath.Join(outsidePath, "source.go"), "outside\n")

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(rootPath, filepath.Join(parent, "moved-repo")); err != nil {
		t.Skipf("cannot rename an open repository root: %v", err)
	}
	if err := os.Symlink(outsidePath, rootPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := lineFromRoot(root, "source.go", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "inside" {
		t.Fatalf("snippet = %q, want original rooted file", got)
	}
}

func TestSourceLookupHandlesCRLFLongLinesAndNativePaths(t *testing.T) {
	root := t.TempDir()
	longLine := strings.Repeat("x", 128*1024)
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
