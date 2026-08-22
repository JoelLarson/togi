package enricher

import (
	"context"
	"reflect"
	"sort"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

// Context describes the repository context available to an enricher.
type Context struct {
	Root     string
	Location gate.Location
}

// Enricher adds context to normalized findings.
type Enricher interface {
	Enrich(context.Context, Context, []finding.Finding) ([]finding.Finding, error)
}

// Registry resolves the enricher for a binding's language. Language dispatch
// lives here — not inside an adapter — so adding a language is additive and
// a missing enricher is a load-time answer, never an errored gate mid-run.
type Registry map[string]Enricher

// NewRegistry returns every supported language's enricher.
func NewRegistry() Registry {
	return Registry{"go": Go{}}
}

// For returns the enricher for a language.
func (r Registry) For(language string) (Enricher, bool) {
	enricher, ok := r[language]
	if !ok || enricher == nil {
		return nil, false
	}
	value := reflect.ValueOf(enricher)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return nil, false
	}
	return enricher, ok
}

// Languages returns the supported languages in unspecified order.
func (r Registry) Languages() []string {
	languages := make([]string, 0, len(r))
	for language := range r {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	return languages
}

// Noop leaves normalized findings unchanged until enrichment is implemented.
type Noop struct{}

// Enrich implements Enricher without inspecting context or findings.
func (Noop) Enrich(_ context.Context, _ Context, in []finding.Finding) ([]finding.Finding, error) {
	return in, nil
}
