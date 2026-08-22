package enricher

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

// Go enriches entity findings with their enclosing Go declaration range.
type Go struct{}

// Enrich implements Enricher for Go source files.
func (Go) Enrich(ctx context.Context, enrichment Context, in []finding.Finding) ([]finding.Finding, error) {
	out := clone(in)
	if len(out) == 0 {
		return out, nil
	}
	if enrichment.Location == gate.PointLocation {
		return out, nil
	}
	if enrichment.Location != gate.EntityLocation {
		return nil, fmt.Errorf("unsupported finding location %q", enrichment.Location)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("enrich Go findings: %w", err)
	}
	if enrichment.Language != "go" {
		return nil, fmt.Errorf("cannot enrich %q entity findings as Go", enrichment.Language)
	}
	if !filepath.IsAbs(enrichment.Root) {
		return nil, fmt.Errorf("repository root must be an absolute path")
	}

	root, err := os.OpenRoot(enrichment.Root)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()

	files := make(map[string]parsedFile)
	for index := range out {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("enrich Go findings: %w", err)
		}
		if out[index].EndLine != 0 && allOccurrencesBounded(out[index].Occurrences) {
			continue
		}

		path, err := repositoryPath(out[index].File)
		if err != nil {
			return nil, err
		}
		parsed, ok := files[path]
		if !ok {
			parsed, err = parseGoFile(ctx, root, path)
			if err != nil {
				return nil, err
			}
			files[path] = parsed
		}

		if out[index].EndLine == 0 {
			out[index].EndLine = enclosingDeclarationEndLine(parsed, out[index].Line)
		}
		for occurrence := range out[index].Occurrences {
			if out[index].Occurrences[occurrence].EndLine == 0 {
				out[index].Occurrences[occurrence].EndLine = enclosingDeclarationEndLine(parsed, out[index].Occurrences[occurrence].Line)
			}
		}
	}

	return out, nil
}

type parsedFile struct {
	file *ast.File
	fset *token.FileSet
}

func clone(in []finding.Finding) []finding.Finding {
	if in == nil {
		return nil
	}
	out := make([]finding.Finding, len(in))
	for index, item := range in {
		out[index] = item
		out[index].Occurrences = append([]finding.Occurrence(nil), item.Occurrences...)
	}
	return out
}

func allOccurrencesBounded(occurrences []finding.Occurrence) bool {
	for _, occurrence := range occurrences {
		if occurrence.EndLine == 0 {
			return false
		}
	}
	return true
}

func repositoryPath(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("finding file must be a repository-relative path")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("finding file escapes repository root")
	}
	return cleaned, nil
}

func parseGoFile(ctx context.Context, root *os.Root, path string) (parsedFile, error) {
	if err := ctx.Err(); err != nil {
		return parsedFile{}, fmt.Errorf("enrich Go findings: %w", err)
	}

	file, err := root.Open(path)
	if err != nil {
		return parsedFile{}, fmt.Errorf("open finding source %q: %w", path, err)
	}
	defer file.Close()
	source, err := io.ReadAll(file)
	if err != nil {
		return parsedFile{}, fmt.Errorf("read finding source %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return parsedFile{}, fmt.Errorf("enrich Go findings: %w", err)
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		return parsedFile{}, fmt.Errorf("parse Go source %q", path)
	}
	return parsedFile{file: parsed, fset: fset}, nil
}

func enclosingDeclarationEndLine(parsed parsedFile, line int) int {
	shortestRange := 0
	startLine := 0
	endLine := 0
	ast.Inspect(parsed.file, func(node ast.Node) bool {
		declaration, ok := node.(ast.Decl)
		if !ok {
			return true
		}
		start := parsed.fset.Position(declaration.Pos()).Line
		end := parsed.fset.Position(declaration.End()).Line
		if line < start || line > end {
			return true
		}
		declarationRange := end - start
		isShorter := endLine == 0 || declarationRange < shortestRange
		isEarlierEqualRange := declarationRange == shortestRange && (start < startLine || (start == startLine && end < endLine))
		if isShorter || isEarlierEqualRange {
			shortestRange = declarationRange
			startLine = start
			endLine = end
		}
		return true
	})
	return endLine
}
