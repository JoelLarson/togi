package gate

import (
	"errors"
	"sort"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/normalizer"
)

// CostClass is a gate's declared runtime tier.
type CostClass string

const (
	Instant CostClass = "instant"
	Fast    CostClass = "fast"
	Slow    CostClass = "slow"
	Glacial CostClass = "glacial"
)

// FixPolicy describes how a gate's findings are resolved.
type FixPolicy string

const (
	AutofixOnly    FixPolicy = "autofix-only"
	AutofixThenLLM FixPolicy = "autofix-then-llm"
	LLMFix         FixPolicy = "llm-fix"
	ReportOnly     FixPolicy = "report-only"
)

// Scope describes whether a gate judges a diff or a whole repository.
type Scope string

const (
	Diff Scope = "diff"
	Repo Scope = "repo"
)

// Location describes whether a gate finding identifies a point or structural entity.
type Location string

const (
	PointLocation  Location = "point"
	EntityLocation Location = "entity"
)

// Manifest is the language-independent definition of a gate.
type Manifest struct {
	Name        string
	Description string
	CostClass   CostClass
	FixPolicy   FixPolicy
	Scope       Scope
	Location    Location
	Blocking    []finding.Severity
	Timeout     time.Duration
}

// Version describes how to extract and constrain a tool version.
type Version struct {
	Command    []string
	Pattern    string
	Constraint string
}

// Binding defines how a gate runs for one language. A binding minted by
// Compile carries its compiled normalizer and an unforgeable validity
// witness; a hand-built literal reports Valid() == false and cannot run.
type Binding struct {
	Language         string
	Tool             string
	Command          []string
	SuccessExitCodes []int
	FindingExitCodes []int
	Normalizer       string
	RuleID           string
	Message          string
	Settings         map[string]any
	SeverityMap      map[string]finding.Severity
	Version          Version
	Aliases          map[string]string

	compiled normalizer.Normalizer
	valid    bool
}

// Valid reports whether this binding was minted by Compile.
func (b Binding) Valid() bool { return b.valid }

// Normalize converts raw tool output into findings via the binding's
// compiled normalizer.
func (b Binding) Normalize(ctx normalizer.Context, raw []byte) ([]finding.Finding, error) {
	if !b.valid || b.compiled == nil {
		return nil, errors.New("binding is not compiled")
	}
	return b.compiled.Normalize(ctx, raw)
}

// Gate combines a manifest with its language bindings.
type Gate struct {
	Manifest Manifest
	Bindings map[string]Binding

	valid bool
}

// Valid reports whether this gate was minted by Compile.
func (g Gate) Valid() bool { return g.valid }

// BindingLanguages returns a gate's binding languages in stable order.
func (g Gate) BindingLanguages() []string {
	languages := make([]string, 0, len(g.Bindings))
	for language := range g.Bindings {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	return languages
}

func (c CostClass) defaultTimeout() time.Duration {
	switch c {
	case Instant:
		return 10 * time.Second
	case Fast:
		return 60 * time.Second
	case Slow:
		return 10 * time.Minute
	case Glacial:
		return 60 * time.Minute
	default:
		return 0
	}
}
