package enricher

import (
	"context"
	"reflect"
	"testing"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

func TestNoopEnrichPreservesFindings(t *testing.T) {
	in := []finding.Finding{{
		Gate:     "lint",
		Language: "go",
		RuleID:   "golangci-lint/errcheck",
		Severity: finding.Error,
		File:     "internal/example.go",
		Line:     12,
		EndLine:  14,
		Snippet:  "value, err := call()",
		Occurrences: []finding.Occurrence{{
			Line:    28,
			EndLine: 30,
		}},
		Message: "unchecked error",
	}}
	in[0].Fingerprint = finding.Fingerprint(in[0])
	if err := finding.Validate(in[0]); err != nil {
		t.Fatalf("test finding is invalid: %v", err)
	}
	wantInput := cloneFindings(in)

	got, err := (Noop{}).Enrich(context.Background(), Context{Root: "/repo", Location: gate.PointLocation}, in)
	if err != nil {
		t.Fatalf("Noop.Enrich() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("Noop.Enrich() = %#v, want %#v", got, in)
	}
	if !reflect.DeepEqual(in, wantInput) {
		t.Fatalf("Noop.Enrich() mutated input: got %#v, want %#v", in, wantInput)
	}
	if len(got) > 0 && &got[0] != &in[0] {
		t.Fatal("Noop.Enrich() allocated a replacement slice")
	}
}

func TestRegistryProvidesSupportedLanguages(t *testing.T) {
	registry := NewRegistry()
	got, ok := registry.For("go")
	if !ok {
		t.Fatal("For(go) did not find an enricher")
	}
	if _, ok := got.(Go); !ok {
		t.Fatalf("For(go) = %T, want Go", got)
	}
	if _, ok := registry.For("rust"); ok {
		t.Fatal("For(rust) unexpectedly found an enricher")
	}
	if want := []string{"go"}; !reflect.DeepEqual(registry.Languages(), want) {
		t.Fatalf("Languages() = %v, want %v", registry.Languages(), want)
	}
}

func cloneFindings(in []finding.Finding) []finding.Finding {
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

func TestNoopEnrichPreservesNilAndEmptyInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		in   []finding.Finding
	}{
		{name: "nil", in: nil},
		{name: "empty", in: make([]finding.Finding, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := (Noop{}).Enrich(context.Background(), Context{}, test.in)
			if err != nil {
				t.Fatalf("Noop.Enrich() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, test.in) {
				t.Fatalf("Noop.Enrich() = %#v, want %#v", got, test.in)
			}
			if (got == nil) != (test.in == nil) {
				t.Fatalf("Noop.Enrich() nilness = %t, want %t", got == nil, test.in == nil)
			}
		})
	}
}

func TestNoopEnrichIgnoresCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := (Noop{}).Enrich(ctx, Context{}, nil)
	if err != nil {
		t.Fatalf("Noop.Enrich() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Noop.Enrich() = %#v, want nil", got)
	}
}
