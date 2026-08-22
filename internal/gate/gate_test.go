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
	"github.com/joellarson/togi/internal/normalizer"
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
	if complexity.Manifest.Location != EntityLocation {
		t.Fatalf("complexity location = %q, want %q", complexity.Manifest.Location, EntityLocation)
	}
	if got := complexity.Bindings["go"].Command; !slices.Equal(got, []string{"gocyclo", "-over", "{{.threshold}}", "."}) {
		t.Fatalf("complexity command = %q", got)
	}
	lint := gates[1]
	if lint.Manifest.Location != PointLocation {
		t.Fatalf("lint location = %q, want %q", lint.Manifest.Location, PointLocation)
	}
	if got := lint.Bindings["go"].Version.Constraint; got != ">=2.12.2 <3.0.0" {
		t.Fatalf("lint version constraint = %q", got)
	}
}

func TestManifestLocation(t *testing.T) {
	tests := []struct {
		name     string
		location string
		want     Location
		wantErr  string
	}{
		{name: "omitted", want: PointLocation},
		{name: "point", location: `location = "point"`, want: PointLocation},
		{name: "entity", location: `location = "entity"`, want: EntityLocation},
		{name: "explicit empty", location: `location = ""`, wantErr: "invalid location"},
		{name: "invalid", location: `location = "file"`, wantErr: "invalid location"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeGateFixture(t, root, "custom", validManifest("custom", test.location), validBinding(""))
			got, err := (Loader{OverrideDir: root}).Load("custom")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Manifest.Location != test.want {
				t.Fatalf("location = %q, want %q", got.Manifest.Location, test.want)
			}
		})
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
		{name: "invalid blocking severity", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `blocking = ["error", "warning"]`, `blocking = ["critical"]`, 1), want: "severity"},
		{name: "duplicate blocking severity", gateName: "custom", manifest: strings.Replace(validManifest("custom", ""), `blocking = ["error", "warning"]`, `blocking = ["error", "error"]`, 1), want: "duplicate"},
		{name: "empty timeout", gateName: "custom", manifest: validManifest("custom", `timeout = ""`), want: "timeout"},
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

func TestBlockingDefaultsOnlyWhenOmitted(t *testing.T) {
	tests := []struct {
		name     string
		blocking string
		want     []finding.Severity
	}{
		{name: "omitted defaults", blocking: "", want: []finding.Severity{finding.Error, finding.Warning}},
		{name: "explicit empty is advisory", blocking: `blocking = []`, want: []finding.Severity{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest := strings.Replace(validManifest("custom", ""), `blocking = ["error", "warning"]`, test.blocking, 1)
			writeGateFixture(t, root, "custom", manifest, validBinding(""))
			got, err := (Loader{OverrideDir: root}).Load("custom")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Manifest.Blocking, test.want) {
				t.Fatalf("blocking = %v, want %v", got.Manifest.Blocking, test.want)
			}
			if test.name == "explicit empty is advisory" && got.Manifest.Blocking == nil {
				t.Fatal("explicit empty blocking was not preserved")
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
		{name: "explicit empty success exits", binding: strings.Replace(validBinding(""), `success_exit_codes = [0]`, `success_exit_codes = []`, 1), want: "success exit"},
		{name: "negative success exit", binding: strings.Replace(validBinding(""), `success_exit_codes = [0]`, `success_exit_codes = [-1]`, 1), want: "success exit"},
		{name: "duplicate success exit", binding: strings.Replace(validBinding(""), `success_exit_codes = [0]`, `success_exit_codes = [0, 0]`, 1), want: "duplicate"},
		{name: "negative finding exit", binding: strings.Replace(validBinding(""), `finding_exit_codes = [1]`, `finding_exit_codes = [-1]`, 1), want: "finding exit"},
		{name: "duplicate finding exit", binding: strings.Replace(validBinding(""), `finding_exit_codes = [1]`, `finding_exit_codes = [1, 1]`, 1), want: "duplicate"},
		{name: "overlapping exits", binding: strings.Replace(validBinding(""), `finding_exit_codes = [1]`, `finding_exit_codes = [0]`, 1), want: "both"},
		{name: "normalizer required", binding: strings.Replace(validBinding(""), `normalizer = "golangci-json"`, `normalizer = ""`, 1), want: "normalizer"},
		{name: "unknown normalizer", binding: strings.Replace(validBinding(""), `normalizer = "golangci-json"`, `normalizer = "unknown"`, 1), want: "unknown normalizer"},
		{name: "bad regex", binding: strings.Replace(validBinding("rule_id = \"tool/rule\"\nmessage = \"message\""), `normalizer = "golangci-json"`, `normalizer = 'regex:('`, 1), want: "compile regex"},
		{name: "severity map required", binding: strings.Replace(validBinding(""), "\n[severity_map]\ndefault = \"warning\"\n", "", 1), want: "severity map"},
		{name: "invalid mapped severity", binding: strings.Replace(validBinding(""), `default = "warning"`, `default = "critical"`, 1), want: "severity"},
		{name: "regex rule ID required", binding: strings.Replace(validBinding(""), `normalizer = "golangci-json"`, `normalizer = "regex:^(?P<file>.+):(?P<line>\\d+)$"`, 1), want: "rule"},
		{name: "regex message required", binding: strings.Replace(validBinding("rule_id = \"tool/rule\""), `normalizer = "golangci-json"`, `normalizer = "regex:^(?P<file>.+):(?P<line>\\d+)$"`, 1), want: "message"},
		{name: "regex default severity required", binding: strings.Replace(strings.Replace(validBinding("rule_id = \"tool/rule\"\nmessage = \"message\""), `normalizer = "golangci-json"`, `normalizer = "regex:^(?P<file>.+):(?P<line>\\d+)$"`, 1), `default = "warning"`, `warning = "warning"`, 1), want: "default severity"},
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

func TestCompileMintsValidityWitnesses(t *testing.T) {
	manifest := Manifest{
		Name: "fixture", Description: "fixture", CostClass: Fast, FixPolicy: ReportOnly,
		Scope: Repo, Location: PointLocation, Blocking: []finding.Severity{}, Timeout: time.Second,
	}
	binding := Binding{
		Language: "go", Tool: "fixture", Command: []string{"fixture"}, SuccessExitCodes: []int{0},
		Normalizer: "golangci-json", SeverityMap: map[string]finding.Severity{"default": finding.Warning},
	}
	if (Gate{Manifest: manifest, Bindings: map[string]Binding{"go": binding}}).Valid() || binding.Valid() {
		t.Fatal("hand-built gate or binding reported valid")
	}
	if _, err := binding.Normalize(normalizer.Context{}, nil); err == nil {
		t.Fatal("hand-built binding normalized output")
	}

	compiled, err := Compile(manifest, map[string]Binding{"go": binding})
	if err != nil {
		t.Fatal(err)
	}
	compiledBinding := compiled.Bindings["go"]
	if !compiled.Valid() || !compiledBinding.Valid() {
		t.Fatalf("compiled validity = %v/%v, want true/true", compiled.Valid(), compiledBinding.Valid())
	}
	if _, err := compiledBinding.Normalize(normalizer.Context{}, nil); err != nil {
		t.Fatalf("compiled binding Normalize() error = %v", err)
	}
}

func TestExitCodeDefaultsAndOptionalFindingExits(t *testing.T) {
	tests := []struct {
		name    string
		binding string
		clean   []int
		finding []int
	}{
		{
			name:    "omitted success defaults to zero",
			binding: strings.Replace(validBinding(""), "success_exit_codes = [0]\n", "", 1),
			clean:   []int{0},
			finding: []int{1},
		},
		{
			name:    "omitted finding exits stay empty",
			binding: strings.Replace(validBinding(""), "finding_exit_codes = [1]\n", "", 1),
			clean:   []int{0},
			finding: []int{},
		},
		{
			name:    "explicit empty finding exits stay empty",
			binding: strings.Replace(validBinding(""), "finding_exit_codes = [1]", "finding_exit_codes = []", 1),
			clean:   []int{0},
			finding: []int{},
		},
		{
			name:    "exit codes are not byte limited",
			binding: strings.Replace(validBinding(""), "finding_exit_codes = [1]", "finding_exit_codes = [256]", 1),
			clean:   []int{0},
			finding: []int{256},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeGateFixture(t, root, "custom", validManifest("custom", ""), test.binding)
			got, err := (Loader{OverrideDir: root}).Load("custom")
			if err != nil {
				t.Fatal(err)
			}
			binding := got.Bindings["go"]
			if !slices.Equal(binding.SuccessExitCodes, test.clean) {
				t.Fatalf("success exits = %v, want %v", binding.SuccessExitCodes, test.clean)
			}
			if !slices.Equal(binding.FindingExitCodes, test.finding) {
				t.Fatalf("finding exits = %v, want %v", binding.FindingExitCodes, test.finding)
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

func TestOverrideRejectsSymlinkedGateDirectories(t *testing.T) {
	tests := []struct {
		name   string
		target func(*testing.T, string) string
	}{
		{
			name: "same root",
			target: func(t *testing.T, root string) string {
				writeGateFixture(t, root, "zreal", validManifest("zreal", ""), validBinding(""))
				return "zreal"
			},
		},
		{
			name: "outside root",
			target: func(t *testing.T, _ string) string {
				outside := t.TempDir()
				writeGateFixture(t, outside, "gate", validManifest("linked", ""), validBinding(""))
				return filepath.Join(outside, "gate")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := test.target(t, root)
			if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
				t.Skipf("create symlink: %v", err)
			}

			loader := Loader{OverrideDir: root}
			if _, err := loader.Load("linked"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
				t.Fatalf("Load error = %v, want symlink error", err)
			}
			if _, err := loader.LoadAll(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
				t.Fatalf("LoadAll error = %v, want symlink error", err)
			}
		})
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
		{name: "extracts and matches bounded major", version: Version{Pattern: `v?([0-9][0-9A-Za-z.+-]*)`, Constraint: ">=2.12.2 <3.0.0"}, raw: "golangci-lint has version v2.13.0", wantVersion: "2.13.0", wantMatches: true},
		{name: "exact minimum matches", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=2.12.2"}, raw: "2.12.2", wantVersion: "2.12.2", wantMatches: true},
		{name: "below minimum mismatches", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=2.12.2"}, raw: "2.12.1", wantVersion: "2.12.1", wantMatches: false},
		{name: "release candidate is below final lower bound", version: Version{Pattern: `(\S+)`, Constraint: ">=2.12.2 <3.0.0"}, raw: "2.12.2-rc.1", wantVersion: "2.12.2-rc.1", wantMatches: false},
		{name: "next major is rejected", version: Version{Pattern: `(\S+)`, Constraint: ">=2.12.2 <3.0.0"}, raw: "3.0.0", wantVersion: "3.0.0", wantMatches: false},
		{name: "build metadata is ignored", version: Version{Pattern: `(\S+)`, Constraint: ">=2.12.2+minimum <3.0.0"}, raw: "2.12.2+observed", wantVersion: "2.12.2+observed", wantMatches: true},
		{name: "numeric prerelease identifiers compare numerically", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0-rc.2 <1.0.0"}, raw: "1.0.0-rc.10", wantVersion: "1.0.0-rc.10", wantMatches: true},
		{name: "numeric prerelease is below lexical", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0-alpha <1.0.0"}, raw: "1.0.0-1", wantVersion: "1.0.0-1", wantMatches: false},
		{name: "optional version block", version: Version{}, raw: "anything", wantMatches: true},
		{name: "malformed regex", version: Version{Pattern: `(`, Constraint: ">=1.0.0"}, raw: "1.0.0", wantError: "pattern"},
		{name: "missing match", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0"}, raw: "unknown", wantError: "extract"},
		{name: "pattern needs capture", version: Version{Pattern: `\d+\.\d+\.\d+`, Constraint: ">=1.0.0"}, raw: "1.0.0", wantError: "capture"},
		{name: "malformed observed", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0"}, raw: "1.0", wantError: "observed"},
		{name: "observed leading zero", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0"}, raw: "01.0.0", wantError: "observed"},
		{name: "prerelease leading zero", version: Version{Pattern: `(\S+)`, Constraint: ">=1.0.0-1"}, raw: "1.0.0-01", wantError: "observed"},
		{name: "malformed constraint version", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=1.0"}, raw: "1.0.0", wantError: "constraint"},
		{name: "constraint leading zero", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=01.0.0"}, raw: "1.0.0", wantError: "constraint"},
		{name: "unsupported constraint", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: "^1.0.0"}, raw: "1.0.0", wantError: "constraint"},
		{name: "unsupported comparison", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: "<=2.0.0"}, raw: "1.0.0", wantError: "constraint"},
		{name: "malformed AND constraint", version: Version{Pattern: `(\d+\.\d+\.\d+)`, Constraint: ">=1.0.0 <"}, raw: "1.0.0", wantError: "constraint"},
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
	assertDefaultLint(t, lint)

	complexity, err := (Loader{}).Load("complexity")
	if err != nil {
		t.Fatal(err)
	}
	assertDefaultComplexity(t, complexity)
}

func assertDefaultLint(t *testing.T, lint Gate) {
	t.Helper()
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
	if binding := lint.Bindings["go"]; !slices.Equal(binding.SuccessExitCodes, []int{0}) || !slices.Equal(binding.FindingExitCodes, []int{1}) {
		t.Fatalf("lint exits = clean %v, finding %v", binding.SuccessExitCodes, binding.FindingExitCodes)
	}
	observed, matches, err := lint.Bindings["go"].Version.Observe("golangci-lint has version v2.12.2-rc.1+build.7")
	if err != nil || observed != "2.12.2-rc.1+build.7" || matches {
		t.Fatalf("lint version observation = (%q, %v, %v)", observed, matches, err)
	}

}

func assertDefaultComplexity(t *testing.T, complexity Gate) {
	t.Helper()
	if complexity.Manifest.FixPolicy != LLMFix || complexity.Bindings["go"].RuleID != "gocyclo/complexity" {
		t.Fatalf("complexity = %#v", complexity)
	}
	if !reflect.DeepEqual(complexity.Bindings["go"].Version, Version{}) {
		t.Fatalf("complexity version = %#v, want empty", complexity.Bindings["go"].Version)
	}
	if binding := complexity.Bindings["go"]; !slices.Equal(binding.SuccessExitCodes, []int{0}) || !slices.Equal(binding.FindingExitCodes, []int{1}) {
		t.Fatalf("complexity exits = clean %v, finding %v", binding.SuccessExitCodes, binding.FindingExitCodes)
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
success_exit_codes = [0]
finding_exit_codes = [1]
normalizer = "golangci-json"
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
