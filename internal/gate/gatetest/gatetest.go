// Package gatetest provides valid gate fixtures for tests outside package gate.
package gatetest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/pelletier/go-toml/v2"
)

// Option changes one part of a gate fixture.
type Option func(*fixture)

type fixture struct {
	manifest gate.Manifest
	binding  gate.Binding
}

// Compile returns a valid in-memory gate minted by gate.Compile.
func Compile(t testing.TB, name string, options ...Option) gate.Gate {
	t.Helper()
	value := newFixture(name, options)
	compiled, err := gate.Compile(value.manifest, map[string]gate.Binding{
		value.binding.Language: value.binding,
	})
	if err != nil {
		t.Fatalf("compile gate fixture %q: %v", name, err)
	}
	return compiled
}

// Write writes a valid gate manifest and language binding under overrideDir.
func Write(t testing.TB, overrideDir, name string, options ...Option) {
	t.Helper()
	value := newFixture(name, options)
	if _, err := gate.Compile(value.manifest, map[string]gate.Binding{value.binding.Language: value.binding}); err != nil {
		t.Fatalf("compile gate fixture %q before writing: %v", name, err)
	}

	manifest := manifestWire{
		Name: value.manifest.Name, Description: value.manifest.Description,
		CostClass: string(value.manifest.CostClass), FixPolicy: string(value.manifest.FixPolicy),
		Scope: string(value.manifest.Scope), Location: string(value.manifest.Location),
		Blocking: value.manifest.Blocking, Timeout: value.manifest.Timeout.String(),
	}
	binding := bindingWire{
		Language: value.binding.Language, Tool: value.binding.Tool, Command: value.binding.Command,
		SuccessExitCodes: value.binding.SuccessExitCodes, FindingExitCodes: value.binding.FindingExitCodes,
		Normalizer: value.binding.Normalizer, RuleID: value.binding.RuleID, Message: value.binding.Message,
		Settings: value.binding.Settings, SeverityMap: value.binding.SeverityMap,
		Version: value.binding.Version, Aliases: value.binding.Aliases,
	}
	writeTOML(t, filepath.Join(overrideDir, name, "gate.toml"), manifest)
	writeTOML(t, filepath.Join(overrideDir, name, value.binding.Language, "binding.toml"), binding)
}

func newFixture(name string, options []Option) fixture {
	value := fixture{
		manifest: gate.Manifest{
			Name: name, Description: name + " fixture", CostClass: gate.Fast,
			FixPolicy: gate.ReportOnly, Scope: gate.Repo, Location: gate.PointLocation,
			Blocking: []finding.Severity{finding.Error, finding.Warning}, Timeout: 5 * time.Second,
		},
		binding: gate.Binding{
			Language: "go", Tool: "fixture", Command: []string{"fixture"},
			SuccessExitCodes: []int{0}, FindingExitCodes: []int{1},
			Normalizer:  "golangci-json",
			SeverityMap: map[string]finding.Severity{"warning": finding.Warning, "default": finding.Warning},
			Aliases:     map[string]string{},
		},
	}
	for _, option := range options {
		option(&value)
	}
	return value
}

// Scope sets the manifest scope.
func Scope(scope gate.Scope) Option {
	return func(value *fixture) { value.manifest.Scope = scope }
}

// Location sets the manifest location semantics.
func Location(location gate.Location) Option {
	return func(value *fixture) { value.manifest.Location = location }
}

// Blocking sets the severities that block, including an explicitly empty set.
func Blocking(severities ...finding.Severity) Option {
	return func(value *fixture) { value.manifest.Blocking = append([]finding.Severity{}, severities...) }
}

// Command sets the binding command.
func Command(command ...string) Option {
	return func(value *fixture) { value.binding.Command = append([]string(nil), command...) }
}

// Aliases sets the binding's rule ID to principle page mappings.
func Aliases(aliases map[string]string) Option {
	return func(value *fixture) {
		value.binding.Aliases = make(map[string]string, len(aliases))
		for ruleID, page := range aliases {
			value.binding.Aliases[ruleID] = page
		}
	}
}

// Language sets the binding language.
func Language(language string) Option {
	return func(value *fixture) { value.binding.Language = language }
}

// Version sets the binding's version probe and constraint.
func Version(version gate.Version) Option {
	return func(value *fixture) {
		value.binding.Version = gate.Version{
			Command: append([]string(nil), version.Command...), Pattern: version.Pattern, Constraint: version.Constraint,
		}
	}
}

// Settings sets the binding's command template values.
func Settings(settings map[string]any) Option {
	return func(value *fixture) {
		value.binding.Settings = make(map[string]any, len(settings))
		for key, item := range settings {
			value.binding.Settings[key] = item
		}
	}
}

type manifestWire struct {
	Name        string             `toml:"name"`
	Description string             `toml:"description"`
	CostClass   string             `toml:"cost_class"`
	FixPolicy   string             `toml:"fix_policy"`
	Scope       string             `toml:"scope"`
	Location    string             `toml:"location"`
	Blocking    []finding.Severity `toml:"blocking"`
	Timeout     string             `toml:"timeout"`
}

type bindingWire struct {
	Language         string                      `toml:"language"`
	Tool             string                      `toml:"tool"`
	Command          []string                    `toml:"command"`
	SuccessExitCodes []int                       `toml:"success_exit_codes"`
	FindingExitCodes []int                       `toml:"finding_exit_codes"`
	Normalizer       string                      `toml:"normalizer"`
	RuleID           string                      `toml:"rule_id,omitempty"`
	Message          string                      `toml:"message,omitempty"`
	Settings         map[string]any              `toml:"settings,omitempty"`
	SeverityMap      map[string]finding.Severity `toml:"severity_map"`
	Version          gate.Version                `toml:"version,omitempty"`
	Aliases          map[string]string           `toml:"aliases,omitempty"`
}

func writeTOML(t testing.TB, filename string, value any) {
	t.Helper()
	encoded, err := toml.Marshal(value)
	if err != nil {
		t.Fatalf("encode gate fixture %q: %v", filename, err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("create gate fixture directory %q: %v", filepath.Dir(filename), err)
	}
	if err := os.WriteFile(filename, encoded, 0o644); err != nil {
		t.Fatalf("write gate fixture %q: %v", filename, err)
	}
}
