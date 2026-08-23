package harness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlueLivesInTestFiles keeps every step pattern where Gherkin tooling can
// find it. The Cucumber language server's default Go glue is
// features/**/*_test.go, and Zed cannot override it: the extension wraps
// lsp.cucumber.settings as {"cucumber": {...}} while the server reads "glue"
// off the top level, so the setting is discarded and the defaults are used.
// A step registered outside a _test.go file therefore still runs under godog
// but reads as undefined in the editor.
func TestGlueLivesInTestFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	fileSet := token.NewFileSet()
	var stray []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && selector.Sel.Name == "Step" {
				stray = append(stray, fileSet.Position(call.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(stray) > 0 {
		t.Fatalf("step definitions registered outside a _test.go file at %s; move the registration into the specification package's steps_test.go, because the Cucumber language server only reads glue from features/**/*_test.go", strings.Join(stray, ", "))
	}
}
