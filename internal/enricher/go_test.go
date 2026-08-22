package enricher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

func TestGoEnrichExtendsEntityLocationsWithoutAliasingInput(t *testing.T) {
	root := t.TempDir()
	writeGoSource(t, root, "example.go", declarationSource)

	in := []finding.Finding{{
		File: "example.go",
		Line: 4,
		Occurrences: []finding.Occurrence{
			{Line: 8},
			{Line: 12},
		},
	}}
	wantInput := cloneFindings(in)

	got, err := (Go{}).Enrich(context.Background(), goEntityContext(root), in)
	if err != nil {
		t.Fatalf("Go.Enrich() error = %v, want nil", err)
	}
	if got[0].EndLine != 5 {
		t.Fatalf("primary end line = %d, want 5", got[0].EndLine)
	}
	if got[0].Occurrences[0].EndLine != 9 {
		t.Fatalf("function occurrence end line = %d, want 9", got[0].Occurrences[0].EndLine)
	}
	if got[0].Occurrences[1].EndLine != 13 {
		t.Fatalf("method occurrence end line = %d, want 13", got[0].Occurrences[1].EndLine)
	}
	if !reflect.DeepEqual(in, wantInput) {
		t.Fatalf("Go.Enrich() mutated input: got %#v, want %#v", in, wantInput)
	}

	got[0].EndLine = 99
	got[0].Occurrences[0].EndLine = 99
	if !reflect.DeepEqual(in, wantInput) {
		t.Fatalf("Go.Enrich() returned aliased storage: got %#v, want %#v", in, wantInput)
	}
}

func TestGoEnrichPreservesExistingEndLines(t *testing.T) {
	root := t.TempDir()
	writeGoSource(t, root, "example.go", declarationSource)
	in := []finding.Finding{{
		File:    "example.go",
		Line:    4,
		EndLine: 4,
		Occurrences: []finding.Occurrence{
			{Line: 8, EndLine: 8},
			{Line: 12},
		},
	}}

	got, err := (Go{}).Enrich(context.Background(), goEntityContext(root), in)
	if err != nil {
		t.Fatalf("Go.Enrich() error = %v, want nil", err)
	}
	if got[0].EndLine != 4 {
		t.Fatalf("primary end line = %d, want preserved 4", got[0].EndLine)
	}
	if got[0].Occurrences[0].EndLine != 8 {
		t.Fatalf("occurrence end line = %d, want preserved 8", got[0].Occurrences[0].EndLine)
	}
	if got[0].Occurrences[1].EndLine != 13 {
		t.Fatalf("unbounded occurrence end line = %d, want 13", got[0].Occurrences[1].EndLine)
	}
}

func TestGoEnrichLeavesPackageAndOutsideDeclarationLocationsAsPoints(t *testing.T) {
	root := t.TempDir()
	writeGoSource(t, root, "example.go", declarationSource)
	in := []finding.Finding{{
		File: "example.go",
		Line: 1,
		Occurrences: []finding.Occurrence{
			{Line: 2},
			{Line: 14},
		},
	}}

	got, err := (Go{}).Enrich(context.Background(), goEntityContext(root), in)
	if err != nil {
		t.Fatalf("Go.Enrich() error = %v, want nil", err)
	}
	if got[0].EndLine != 0 || got[0].Occurrences[0].EndLine != 0 || got[0].Occurrences[1].EndLine != 0 {
		t.Fatalf("outside declaration locations = %#v, want points", got[0])
	}
}

func TestGoEnrichChoosesSmallestNestedDeclaration(t *testing.T) {
	root := t.TempDir()
	writeGoSource(t, root, "nested.go", nestedDeclarationSource)
	in := []finding.Finding{
		{File: "nested.go", Line: 5},
		{File: "nested.go", Line: 7},
		{File: "nested.go", Line: 9},
	}

	got, err := (Go{}).Enrich(context.Background(), goEntityContext(root), in)
	if err != nil {
		t.Fatalf("Go.Enrich() error = %v, want nil", err)
	}
	for index, want := range []int{6, 7, 10} {
		if got[index].EndLine != want {
			t.Fatalf("finding %d end line = %d, want %d", index, got[index].EndLine, want)
		}
	}
}

func TestGoEnrichPointLocationDoesNotRequireSource(t *testing.T) {
	in := []finding.Finding{{File: "../unsafe.go", Line: 4, Occurrences: []finding.Occurrence{{Line: 8}}}}
	want := cloneFindings(in)

	got, err := (Go{}).Enrich(context.Background(), Context{
		Root:     filepath.Join(t.TempDir(), "does-not-exist"),
		Language: "go",
		Location: gate.PointLocation,
	}, in)
	if err != nil {
		t.Fatalf("Go.Enrich() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Go.Enrich() = %#v, want %#v", got, want)
	}
	got[0].Occurrences[0].EndLine = 9
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("Go.Enrich() returned aliased point finding: got %#v, want %#v", in, want)
	}
}

func TestGoEnrichRejectsUnsupportedEntityContexts(t *testing.T) {
	root := t.TempDir()
	in := []finding.Finding{{File: "example.go", Line: 1}}

	for _, test := range []struct {
		name string
		ctx  Context
	}{
		{name: "language", ctx: Context{Root: root, Language: "rust", Location: gate.EntityLocation}},
		{name: "relative root", ctx: Context{Root: "relative", Language: "go", Location: gate.EntityLocation}},
		{name: "unknown location", ctx: Context{Root: root, Language: "go", Location: gate.Location("unknown")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (Go{}).Enrich(context.Background(), test.ctx, in)
			if err == nil {
				t.Fatal("Go.Enrich() error = nil, want non-nil")
			}
		})
	}
}

func TestGoEnrichFailsForUnreadableOrInvalidGoSource(t *testing.T) {
	root := t.TempDir()
	writeGoSource(t, root, "invalid.go", "package sample\nfunc broken() { one topSecret }\n")
	if err := os.Mkdir(filepath.Join(root, "source-dir"), 0o700); err != nil {
		t.Fatalf("Mkdir(source-dir): %v", err)
	}

	for _, test := range []struct {
		name string
		file string
	}{
		{name: "missing", file: "missing.go"},
		{name: "invalid", file: "invalid.go"},
		{name: "read failure", file: "source-dir"},
		{name: "parent traversal", file: "../outside.go"},
		{name: "absolute path", file: filepath.Join(root, "invalid.go")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (Go{}).Enrich(context.Background(), goEntityContext(root), []finding.Finding{{File: test.file, Line: 1}})
			if err == nil {
				t.Fatal("Go.Enrich() error = nil, want non-nil")
			}
			if strings.Contains(err.Error(), "topSecret") {
				t.Fatalf("Go.Enrich() error exposed source content: %v", err)
			}
			if test.name == "read failure" && !strings.Contains(err.Error(), "read finding source") {
				t.Fatalf("Go.Enrich() error = %v, want read failure", err)
			}
		})
	}
}

func TestGoEnrichRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	escape := t.TempDir()
	writeGoSource(t, escape, "outside.go", declarationSource)
	link := filepath.Join(root, "outside")
	if err := os.Symlink(escape, link); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatalf("Symlink(%q, %q): %v", escape, link, err)
	}

	_, err := (Go{}).Enrich(context.Background(), goEntityContext(root), []finding.Finding{{File: "outside/outside.go", Line: 4}})
	if err == nil {
		t.Fatal("Go.Enrich() error = nil, want symlink escape rejection")
	}
}

func TestGoEnrichHonorsCanceledContextBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (Go{}).Enrich(ctx, goEntityContext(filepath.Join(t.TempDir(), "does-not-exist")), []finding.Finding{{File: "example.go", Line: 1}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Go.Enrich() error = %v, want context.Canceled", err)
	}
}

func TestGoEnrichPreservesNilAndEmptyInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		in   []finding.Finding
	}{
		{name: "nil", in: nil},
		{name: "empty", in: []finding.Finding{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := (Go{}).Enrich(context.Background(), Context{Location: gate.EntityLocation}, test.in)
			if err != nil {
				t.Fatalf("Go.Enrich() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, test.in) {
				t.Fatalf("Go.Enrich() = %#v, want %#v", got, test.in)
			}
			if (got == nil) != (test.in == nil) {
				t.Fatalf("Go.Enrich() nilness = %t, want %t", got == nil, test.in == nil)
			}
		})
	}
}

func goEntityContext(root string) Context {
	return Context{Root: root, Language: "go", Location: gate.EntityLocation}
}

func writeGoSource(t *testing.T, root, name, source string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

const declarationSource = `package sample

type Record struct {
	Value int
}

func Work() {
	println("work")
}

func (Record) Method() {
	println("method")
}
`

const nestedDeclarationSource = `package sample

func Work() {
	var localValue = struct {
		Value int
	}{}
	const localConst = 1
	type localType struct {
		Value int
	}
	println(localValue, localConst, localType{})
}
`
