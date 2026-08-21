package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
)

func TestLoadAllReadsEmbeddedGoBindings(t *testing.T) {
	gates, err := (Loader{}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}

	if names := gateNames(gates); !slices.Equal(names, []string{"complexity", "lint"}) {
		t.Fatalf("names = %v", names)
	}
	for _, loaded := range gates {
		binding, ok := loaded.Bindings["go"]
		if !ok {
			t.Fatalf("gate %q has no Go binding", loaded.Manifest.Name)
		}
		if binding.Normalizer == "" {
			t.Fatalf("gate %q has empty normalizer", loaded.Manifest.Name)
		}
	}

	complexity := gates[0]
	if got := complexity.Bindings["go"].Command; !slices.Equal(got, []string{"gocyclo", "-over", "{{.threshold}}", "."}) {
		t.Fatalf("complexity command = %q", got)
	}
	lint := gates[1]
	if got := lint.Bindings["go"].Version.Constraint; got != ">=2.12.2" {
		t.Fatalf("lint version constraint = %q", got)
	}
}

func TestConfigDirectoryOverridesEmbeddedGateWholesale(t *testing.T) {
	overrides := t.TempDir()
	binding := strings.Replace(validBinding(""), `tool = "tool"`, `tool = "fake-lint"`, 1)
	writeGateFixture(t, overrides, "lint", validManifest("lint", `timeout = "7s"`), binding)

	got, err := (Loader{OverrideDir: overrides}).Load("lint")
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.Timeout != 7*time.Second {
		t.Fatalf("timeout = %v", got.Manifest.Timeout)
	}
	if got.Bindings["go"].Tool != "fake-lint" {
		t.Fatalf("tool = %q", got.Bindings["go"].Tool)
	}
}

func TestOverrideDoesNotFallBackToEmbeddedFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			name: "missing manifest",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "lint", "go", "binding.toml"), validBinding(""))
			},
			want: "gate.toml",
		},
		{
			name: "missing bindings",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "lint", "gate.toml"), validManifest("lint", ""))
			},
			want: "binding",
		},
		{
			name: "malformed binding",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "lint", "gate.toml"), validManifest("lint", ""))
				writeFile(t, filepath.Join(root, "lint", "go", "binding.toml"), "language = [")
			},
			want: "binding.toml",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			_, err := (Loader{OverrideDir: root}).Load("lint")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestStrictTOMLRejectsUnknownFieldsWithFileContext(t *testing.T) {
	tests := []struct {
		name    string
		gate    string
		binding string
		file    string
	}{
		{name: "manifest", gate: validManifest("custom", `mystery = true`), binding: validBinding(""), file: "gate.toml"},
		{name: "binding", gate: validManifest("custom", ""), binding: validBinding(`mystery = true`), file: "binding.toml"},
		{name: "version", gate: validManifest("custom", ""), binding: validBinding("[version]\ncommand = [\"tool\", \"version\"]\npattern = \"(.*)\"\nconstraint = \">=1.0.0\"\nmystery = true"), file: "binding.toml"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeGateFixture(t, root, "custom", test.gate, test.binding)
			_, err := (Loader{OverrideDir: root}).Load("custom")
			if err == nil || !strings.Contains(err.Error(), test.file) || !strings.Contains(strings.ToLower(err.Error()), "unknown") {
				t.Fatalf("error = %v, want unknown field with %s context", err, test.file)
			}
		})
	}
}

func TestManifestValidation(t *testing.T) {
	tests := []struct {
		name     string
		gateName string
		manifest string
		want     string
	}{
		{name: "name required", gateName: "custom", manifest: validManifest("", ""), want: "name"},
		{name: "description required", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `description = "description"`, `description = ""`, 1), want: "description"},
		{name: "name matches directory", gateName: "custom", manifest: validManifest("other", ""), want: "directory"},
		{name: "invalid cost class", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `cost_class = "fast"`, `cost_class = "medium"`, 1), want: "cost class"},
		{name: "invalid fix policy", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `fix_policy = "llm-fix"`, `fix_policy = "magic"`, 1), want: "fix policy"},
		{name: "invalid scope", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `scope = "diff"`, `scope = "branch"`, 1), want: "scope"},
		{name: "blocking required", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `blocking = ["error", "warning"]`, `blocking = []`, 1), want: "blocking"},
		{name: "invalid blocking severity", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `blocking = ["error", "warning"]`, `blocking = ["critical"]`, 1), want: "severity"},
		{name: "duplicate blocking severity", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `blocking = ["error", "warning"]`, `blocking = ["error", "error"]`, 1), want: "duplicate"},
		{name: "invalid timeout", gateName: "custom", manifest: validManifest("custom", `timeout = "soon"`), want: "timeout"},
		{name: "nonpositive timeout", gateName: "custom", manifest: validManifest("custom", `timeout = "0s"`), want: "positive"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeGateFixture(t, root, test.gateName, test.manifest, validBinding(""))
			_, err := (Loader{OverrideDir: root}).Load(test.gateName)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestCostClassDeterminesDefaultTimeout(t *testing.T) {
	tests := []struct {
		cost CostClass
		want time.Duration
	}{
		{Instant, 10 * time.Second},
		{Fast, 60 * time.Second},
		{Slow, 10 * time.Minute},
		{Glacial, 60 * time.Minute},
	}

	for _, test := range tests {
		t.Run(string(test.cost), func(t *testing.T) {
			root := t.TempDir()
			manifest := strings.Replace(validManifest("custom", ""), `cost_class = "fast"`, fmt.Sprintf(`cost_class = %q`, test.cost), 1)
			writeGateFixture(t, root, "custom", manifest, validBinding(""))
			got, err := (Loader{OverrideDir: root}).Load("custom")
			if err != nil {
				t.Fatal(err)
			}
			if got.Manifest.Timeout != test.want {
				t.Fatalf("timeout = %v, want %v", got.Manifest.Timeout, test.want)
			}
		})
	}
}

func TestBindingValidation(t *testing.T) {
	tests := []struct {
		name    string
		binding string
		want    string
	}{
		{name: "language required", binding: strings.Replace(validBinding(""), `language = "go"`, `language = ""`, 1), want: "language"},
		{name: "language matches directory", binding: strings.Replace(validBinding(""), `language = "go"`, `language = "rust"`, 1), want: "directory"},
		{name: "tool required", binding: strings.Replace(validBinding(""), `tool = "tool"`, `tool = ""`, 1), want: "tool"},
		{name: "command required", binding: strings.Replace(validBinding(""), `command = ["tool", "check"]`, `command = []`, 1), want: "command"},
		{name: "empty command argument", binding: strings.Replace(validBinding(""), `command = ["tool", "check"]`, `command = ["tool", ""]`, 1), want: "command"},
		{name: "success exits required", binding: strings.Replace(validBinding(""), `success_exit_codes = [0, 1]`, `success_exit_codes = []`, 1), want: "exit"},
		{name: "negative exit", binding: strings.Replace(validBinding(""), `success_exit_codes = [0, 1]`, `success_exit_codes = [-1]`, 1), want: "exit"},
		{name: "exit above byte range", binding: strings.Replace(validBinding(""), `success_exit_codes = [0, 1]`, `success_exit_codes = [256]`, 1), want: "exit"},
		{name: "duplicate exit", binding: strings.Replace(validBinding(""), `success_exit_codes = [0, 1]`, `success_exit_codes = [0, 0]`, 1), want: "duplicate"},
		{name: "normalizer required", binding: strings.Replace(validBinding(""), `normalizer = "fixture"`, `normalizer = ""`, 1), want: "normalizer"},
		{name: "severity map required", binding: strings.Replace(validBinding(""), "\n[severity_map]\ndefault = \"warning\"\n", "", 1), want: "severity map"},
		{name: "invalid mapped severity", binding: strings.Replace(validBinding(""), `default = "warning"`, `default = "critical"`, 1), want: "severity"},
		{name: "regex rule ID required", binding: strings.Replace(validBinding(""), `normalizer = "fixture"`, `normalizer = "regex:^(.*)$"`, 1), want: "rule"},
		{name: "regex message required", binding: strings.Replace(validBinding("rule_id = \"tool/rule\""), `normalizer = "fixture"`, `normalizer = "regex:^(.*)$"`, 1), want: "message"},
		{name: "incomplete version block", binding: validBinding("[version]\ncommand = [\"tool\", \"version\"]"), want: "version"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeGateFixture(t, root, "custom", validManifest("custom", ""), test.binding)
			_, err := (Loader{OverrideDir: root}).Load("custom")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoaderRejectsAbsentAndUnsafeGateNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../lint", "lint/go", `lint\\go`, "/lint"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			_, err := (Loader{OverrideDir: t.TempDir()}).Load(name)
			if err == nil {
				t.Fatalf("Load(%q) succeeded", name)
			}
		})
	}

	_, err := (Loader{OverrideDir: t.TempDir()}).Load("absent")
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("absent error = %v", err)
	}
}

func TestLoadAllIncludesSortedOverrideOnlyGatesAndBindings(t *testing.T) {
	root := t.TempDir()
	writeGateFixture(t, root, "zeta", validManifest("zeta", ""), validBinding(""))
	writeFile(t, filepath.Join(root, "zeta", "rust", "binding.toml"), strings.Replace(validBinding(""), `language = "go"`, `language = "rust"`, 1))
	writeGateFixture(t, root, "alpha", validManifest("alpha", ""), validBinding(""))

	gates, err := (Loader{OverrideDir: root}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := gateNames(gates); !slices.Equal(got, []string{"alpha", "complexity", "lint", "zeta"}) {
		t.Fatalf("names = %v", got)
	}
	if got := gates[3].BindingLanguages(); !slices.Equal(got, []string{"go", "rust"}) {
		t.Fatalf("binding languages = %v", got)
	}
}

func TestRenderCommandRendersArgumentsIndependentlyWithoutMutation(t *testing.T) {
	binding := Binding{
		Command:  []string{"tool", "{{.threshold}}", "literal with spaces", "{{.enabled}}"},
		Settings: map[string]any{"threshold": int64(15), "enabled": true},
	}
	wantBinding := cloneBinding(binding)

	got, err := binding.RenderCommand()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tool", "15", "literal with spaces", "true"}
	if !slices.Equal(got, want) {
		t.Fatalf("command = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(binding, wantBinding) {
		t.Fatalf("binding mutated:\n got: %#v\nwant: %#v", binding, wantBinding)
	}
	got[0] = "changed"
	if binding.Command[0] != "tool" {
		t.Fatal("rendered command aliases input command")
	}
}

func TestRenderCommandRejectsInvalidAndMissingTemplates(t *testing.T) {
	for _, command := range [][]string{{"{{"}, {"{{.missing}}"}} {
		_, err := (Binding{Command: command, Settings: map[string]any{"present": 1}}).RenderCommand()
		if err == nil {
			t.Fatalf("RenderCommand(%q) succeeded", command)
		}
	}
}

func TestVersionObserve(t *testing.T) {
	tests := []struct {
		name        string
		version     Version
		raw         string
		wantVersion string
		wantMatches bool
		wantError   string
	}{
		{name: "extracts and matches minimum", version: Version{Pattern: `v?(\d+\.\d+\.\d+)`, Constraint: ">=2.12.2"}, raw: "golangci-lint has version v2.13.0 built with go", wantVersion: "2.13.0", wantMatches: true},
		{name: "exact minimum matches", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=2.12.2"}, raw: "2.12.2", wantVersion: "2.12.2", wantMatches: true},
		{name: "below minimum mismatches", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=2.12.2"}, raw: "2.12.1", wantVersion: "2.12.1", wantMatches: false},
		{name: "optional version block", version: Version{}, raw: "anything", wantMatches: true},
		{name: "malformed regex", version: Version{Pattern: `(`, Constraint: ">=1.0.0"}, raw: "1.0.0", wantError: "pattern"},
		{name: "missing match", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0"}, raw: "unknown", wantError: "extract"},
		{name: "pattern needs capture", version: Version{Pattern: `\d+\.\d+\.\d+`, Constraint: ">=1.0.0"}, raw: "1.0.0", wantError: "capture"},
		{name: "malformed observed", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0"}, raw: "1.0", wantError: "observed"},
		{name: "observed leading zero", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0"}, raw: "01.0.0", wantError: "observed"},
		{name: "malformed constraint version", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=1.0"}, raw: "1.0.0", wantError: "constraint"},
		{name: "constraint leading zero", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=01.0.0"}, raw: "1.0.0", wantError: "constraint"},
		{name: "unsupported constraint", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: "^1.0.0"}, raw: "1.0.0", wantError: "constraint"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, matches, err := test.version.Observe(test.raw)
			if test.wantError != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.wantError) {
					t.Fatalf("error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.wantVersion || matches != test.wantMatches {
				t.Fatalf("Observe() = (%q, %v), want (%q, %v)", got, matches, test.wantVersion, test.wantMatches)
			}
		})
	}
}

func TestDefaultGateDetails(t *testing.T) {
	lint, err := (Loader{}).Load("lint")
	if err != nil {
		t.Fatal(err)
	}
	if lint.Manifest.CostClass != Fast || lint.Manifest.FixPolicy != AutofixThenLLM || lint.Manifest.Scope != Diff {
		t.Fatalf("lint manifest = %#v", lint.Manifest)
	}
	if !slices.Equal(lint.Manifest.Blocking, []finding.Severity{finding.Error, finding.Warning}) {
		t.Fatalf("lint blocking = %v", lint.Manifest.Blocking)
	}
	if got := lint.Bindings["go"].SeverityMap; got["error"] != finding.Error || got["warning"] != finding.Warning || got["default"] != finding.Warning {
		t.Fatalf("lint severity map = %#v", got)
	}
	if got := lint.Bindings["go"].Aliases["golangci-lint/*"]; got == "" {
		t.Fatal("lint wildcard alias missing")
	}

	complexity, err := (Loader{}).Load("complexity")
	if err != nil {
		t.Fatal(err)
	}
	if complexity.Manifest.FixPolicy != LLMFix || complexity.Bindings["go"].RuleID != "gocyclo/complexity" {
		t.Fatalf("complexity = %#v", complexity)
	}
	if !reflect.DeepEqual(complexity.Bindings["go"].Version, Version{}) {
		t.Fatalf("complexity version = %#v, want empty", complexity.Bindings["go"].Version)
	}
}

func gateNames(gates []Gate) []string {
	names := make([]string, len(gates))
	for i, gate := range gates {
		names[i] = gate.Manifest.Name
	}
	return names
}

func validManifest(name, extra string) string {
	return fmt.Sprintf(`name = %q
description = "description"
cost_class = "fast"
fix_policy = "llm-fix"
scope = "diff"
blocking = ["error", "warning"]
%s
`, name, extra)
}

func validBinding(extra string) string {
	return fmt.Sprintf(`language = "go"
tool = "tool"
command = ["tool", "check"]
success_exit_codes = [0, 1]
normalizer = "fixture"
%s

[severity_map]
default = "warning"
`, extra)
}

func writeGateFixture(t *testing.T, root, name, manifest, binding string) {
	t.Helper()
	writeFile(t, filepath.Join(root, name, "gate.toml"), manifest)
	writeFile(t, filepath.Join(root, name, "go", "binding.toml"), binding)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cloneBinding(binding Binding) Binding {
	clone := binding
	clone.Command = slices.Clone(binding.Command)
	clone.Settings = make(map[string]any, len(binding.Settings))
	for key, value := range binding.Settings {
		clone.Settings[key] = value
	}
	return clone
}
