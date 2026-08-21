package enricher

import (
	"context"

	"github.com/joellarson/togi/internal/finding"
)

// Context describes the repository context available to an enricher.
type Context struct {
	Root     string
	Language string
}

// Enricher adds context to normalized findings.
type Enricher interface {
	Enrich(context.Context, Context, []finding.Finding) ([]finding.Finding, error)
}

// Noop leaves normalized findings unchanged until enrichment is implemented.
type Noop struct{}

// Enrich implements Enricher without inspecting context or findings.
func (Noop) Enrich(_ context.Context, _ Context, in []finding.Finding) ([]finding.Finding, error) {
	return in, nil
}
