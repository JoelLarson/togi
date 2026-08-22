package finding

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string // "" means rejection
	}{
		{name: "plain file", input: "main.go", want: "main.go"},
		{name: "nested", input: "pkg/util/main.go", want: "pkg/util/main.go"},
		{name: "cleans dot segments", input: "pkg/./main.go", want: "pkg/main.go"},
		{name: "cleans duplicate separators", input: "pkg//main.go", want: "pkg/main.go"},
		{name: "internal traversal that stays inside", input: "pkg/../main.go", want: ""},
		{name: "empty", input: "", want: ""},
		{name: "blank", input: "   ", want: ""},
		{name: "posix absolute", input: "/etc/passwd", want: ""},
		{name: "windows drive absolute", input: "c:/windows/system32", want: ""},
		{name: "windows backslash absolute", input: `\etc\passwd`, want: ""},
		{name: "leading traversal", input: "../outside.go", want: ""},
		{name: "backslash traversal", input: `..\outside.go`, want: ""},
		{name: "lone traversal", input: "..", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParsePath(test.input)
			if test.want == "" {
				if err == nil {
					t.Fatalf("ParsePath(%q) = %q, want rejection", test.input, got)
				}
				if !got.IsZero() {
					t.Fatalf("rejected path is non-zero: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePath(%q) error = %v", test.input, err)
			}
			if got.String() != test.want {
				t.Fatalf("ParsePath(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNewPathRewritesAbsolutePathsInsideRoot(t *testing.T) {
	root := t.TempDir()
	got, err := NewPath(root, filepath.Join(root, "pkg", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "pkg/main.go" {
		t.Fatalf("NewPath = %q, want pkg/main.go", got)
	}
}

func TestNewPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, input := range []string{
		filepath.Join(root, "..", "outside.go"),
		filepath.Dir(root),
		"../outside.go",
		"",
	} {
		if got, err := NewPath(root, input); err == nil {
			t.Fatalf("NewPath(%q) = %q, want rejection", input, got)
		} else if !strings.Contains(err.Error(), "repository") {
			t.Fatalf("NewPath(%q) error = %v", input, err)
		}
	}
}
