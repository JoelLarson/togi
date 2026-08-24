package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

type gateCustomizationFeature struct {
	world          *harness.World
	beforeTree     []string
	invokedMarkers []string
}

func newGateCustomizationFeature(factory harness.DriverFactory) *gateCustomizationFeature {
	return &gateCustomizationFeature{world: harness.NewWorld(factory, harness.NeedsGauntlet)}
}

func (f *gateCustomizationFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.world.After)
	sc.Step(`^I run the gauntlet$`, f.runGauntlet)
	sc.Step(`^the target repository contains no gate definition$`, f.noGateDefinition)
	sc.Step(`^a committed Go repository with a changed function$`, f.changedRepository)
	sc.Step(`^the shipped Go gates report representative findings$`, f.shippedFindings)
	sc.Step(`^the shipped findings are normalized without repository configuration$`, f.shippedNormalized)
	sc.Step(`^a committed Go repository with an XDG override for the "([^"]*)" gate$`, f.override)
	sc.Step(`^the report contains the overridden lint behavior only$`, f.overriddenOnly)
	sc.Step(`^a committed Go repository with an additional "([^"]*)" gate in XDG config$`, f.additional)
	sc.Step(`^the report contains the shipped gates and the "([^"]*)" gate$`, f.includesAdditional)
	sc.Step(`^a committed Go repository with an invalid XDG gate definition$`, f.invalidDefinition)
	sc.Step(`^every available gate records whether it starts$`, f.recordStarts)
	sc.Step(`^the run is rejected for invalid gate data$`, f.invalidRejected)
	sc.Step(`^no gate, ledger, or target-repository file is created$`, f.noSideEffects)
}

func (f *gateCustomizationFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.beforeTree, f.invokedMarkers = nil, nil
	return f.world.Before(ctx, scenario)
}

func (f *gateCustomizationFeature) runGauntlet(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "base", NoColor: true}))
}

func (f *gateCustomizationFeature) noGateDefinition() error {
	tree, err := f.world.Repository().Tree()
	if err != nil {
		return err
	}
	for _, name := range tree {
		if filepath.Base(name) == "gate.toml" {
			return fmt.Errorf("target repository contains gate definition %q", name)
		}
	}
	return nil
}

func (f *gateCustomizationFeature) changedRepository() error {
	repository, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repository"))
	if err != nil {
		return err
	}
	for name, contents := range map[string]string{
		"go.mod":     "module fixture\n\ngo 1.25\n",
		"feature.go": "package fixture\n\nfunc Feature() error {\n return nil\n}\n\nfunc One() {}\nfunc Two() {}\nfunc Three() {}\nfunc Four() {}\nfunc Five() {}\nfunc Six() {}\nfunc Seven() {}\nfunc Eight() {}\nfunc Nine() {}\nfunc Ten() {}\n",
	} {
		if err := repository.Write(name, contents); err != nil {
			return err
		}
	}
	if _, err := repository.Commit("base"); err != nil {
		return err
	}
	if err := repository.Branch("base"); err != nil {
		return err
	}
	if err := repository.Write("feature.go", "package fixture\n\nfunc Feature() error {\n return nil\n}\n\nfunc One() {}\nfunc Two() {}\nfunc Three() {}\nfunc Four() {}\nfunc Five() {}\nfunc Six() {}\nfunc Seven() {}\nfunc Eight() {}\nfunc Nine() {}\nfunc Ten() {}\nfunc Changed() {}\n"); err != nil {
		return err
	}
	if _, err := repository.Commit("feature"); err != nil {
		return err
	}
	f.beforeTree, err = repository.Tree()
	if err != nil {
		return err
	}
	return f.world.UseRepository(repository)
}

func (f *gateCustomizationFeature) installShippedTools() error {
	if _, err := f.world.Environment().InstallTool("gocyclo", harness.ToolBehavior{Stdout: []byte("18 fixture Changed feature.go:17:1\n")}); err != nil {
		return err
	}
	_, err := f.world.Environment().InstallTool("golangci-lint", harness.ToolBehavior{Stdout: lint("feature.go", 17), VersionStdout: []byte("v2.12.2\n")})
	return err
}

func (f *gateCustomizationFeature) shippedFindings() error { return f.installShippedTools() }

func (f *gateCustomizationFeature) shippedNormalized() error {
	report, err := f.world.LastRun().Report()
	if err != nil {
		return err
	}
	if got, want := gateNames(report), []string{"complexity", "lint"}; !slices.Equal(got, want) {
		return fmt.Errorf("gates = %v, want %v", got, want)
	}
	if len(report.Findings) != 2 {
		return fmt.Errorf("findings = %#v, want one normalized finding from each shipped gate", report.Findings)
	}
	if !hasFinding(report, "complexity", "gocyclo/complexity", "cyclomatic complexity 18 in Changed") || !hasFinding(report, "lint", "golangci-lint/errcheck", "Error return value is not checked") {
		return fmt.Errorf("shipped findings = %#v", report.Findings)
	}
	return nil
}

func (f *gateCustomizationFeature) override(name string) error {
	if name != "lint" {
		return fmt.Errorf("unsupported shipped gate override %q", name)
	}
	if err := f.changedRepository(); err != nil {
		return err
	}
	if err := f.installShippedTools(); err != nil {
		return err
	}
	if _, err := f.world.Environment().InstallTool("lint-override", harness.ToolBehavior{Stdout: []byte("feature.go:17: override diagnostic\n")}); err != nil {
		return err
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{Name: "lint", Description: "operator lint override", Tool: "lint-override", Scope: "diff", Location: "point", Normalizer: `regex:^(?P<file>[^:]+):(?P<line>\d+): (?P<detail>.+)$`, RuleID: "operator/lint", Message: "operator lint: {{.detail}}", Command: []string{"lint-override"}, SeverityMap: map[string]string{"default": "warning"}})
}

func (f *gateCustomizationFeature) overriddenOnly() error {
	report, err := f.world.LastRun().Report()
	if err != nil {
		return err
	}
	var lintFindings []harness.Finding
	for _, candidate := range report.Findings {
		if candidate.Gate == "lint" {
			lintFindings = append(lintFindings, candidate)
		}
	}
	if len(lintFindings) != 1 || lintFindings[0].RuleID != "operator/lint" || lintFindings[0].Message != "operator lint: override diagnostic" {
		return fmt.Errorf("overridden lint findings = %#v", lintFindings)
	}
	for _, candidate := range lintFindings {
		if strings.HasPrefix(candidate.RuleID, "golangci-lint/") {
			return fmt.Errorf("shipped lint finding leaked through override: %#v", candidate)
		}
	}
	return nil
}

func (f *gateCustomizationFeature) additional(name string) error {
	if name != "architecture" {
		return fmt.Errorf("unsupported additional gate %q", name)
	}
	if err := f.changedRepository(); err != nil {
		return err
	}
	if err := f.installShippedTools(); err != nil {
		return err
	}
	if _, err := f.world.Environment().InstallTool("architecture-tool", harness.ToolBehavior{Stdout: lint("feature.go", 17)}); err != nil {
		return err
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{Name: "architecture", Description: "architecture fixture", Tool: "architecture-tool", Scope: "repo", Location: "point", Normalizer: "golangci-json", Command: []string{"architecture-tool"}, SeverityMap: map[string]string{"default": "warning", "warning": "warning"}})
}

func (f *gateCustomizationFeature) includesAdditional(name string) error {
	report, err := f.world.LastRun().Report()
	if err != nil {
		return err
	}
	if got, want := gateNames(report), []string{"architecture", "complexity", "lint"}; !slices.Equal(got, want) {
		return fmt.Errorf("gates = %v, want %v", got, want)
	}
	if !hasFinding(report, name, "golangci-lint/errcheck", "Error return value is not checked") {
		return fmt.Errorf("additional gate %q is absent from findings %#v", name, report.Findings)
	}
	return nil
}

func (f *gateCustomizationFeature) invalidDefinition() error {
	if err := f.changedRepository(); err != nil {
		return err
	}
	return f.world.Environment().WriteInvalidGate("invalid", "name = \"invalid\"\ndescription = \"invalid fixture\"\ncost_class = \"fast\"\nfix_policy = \"report-only\"\nscope = \"repo\"\nunknown_field = \"rejected\"\n")
}

func (f *gateCustomizationFeature) recordStarts() error {
	for _, tool := range []string{"gocyclo", "golangci-lint"} {
		marker := filepath.Join(f.world.Environment().TempRoot, tool+".invoked")
		behavior := harness.ToolBehavior{InvokedMarker: marker}
		if tool == "golangci-lint" {
			behavior.VersionStdout = []byte("v2.12.2\n")
		}
		if _, err := f.world.Environment().InstallTool(tool, behavior); err != nil {
			return err
		}
		f.invokedMarkers = append(f.invokedMarkers, marker)
	}
	return nil
}

func (f *gateCustomizationFeature) invalidRejected() error {
	outcome, err := f.world.LastRun().Outcome()
	if err != nil {
		return err
	}
	if outcome.Code != 70 {
		return fmt.Errorf("outcome = %#v, want invalid gate data rejection", outcome)
	}
	if !strings.Contains(f.world.LastRun().Stderr()+outcome.Message, "unknown") {
		return fmt.Errorf("missing invalid gate diagnostic: stderr=%q outcome=%#v", f.world.LastRun().Stderr(), outcome)
	}
	return nil
}

func (f *gateCustomizationFeature) noSideEffects() error {
	if path := f.world.LastRun().ReportPath(); path != "" {
		return fmt.Errorf("report path = %q, want none", path)
	}
	for _, marker := range f.invokedMarkers {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			return fmt.Errorf("gate started at %q: %v", marker, err)
		}
	}
	state, err := f.world.Environment().RepoState(context.Background(), f.world.Repository().Root)
	if err != nil {
		return err
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		return fmt.Errorf("ledger state exists: %v", err)
	}
	after, err := f.world.Repository().Tree()
	if err != nil {
		return err
	}
	if !slices.Equal(after, f.beforeTree) {
		return fmt.Errorf("target repository changed: before=%v after=%v", f.beforeTree, after)
	}
	return nil
}

func gateNames(report harness.Report) []string {
	names := make([]string, len(report.Gates))
	for index, gate := range report.Gates {
		names[index] = gate.Gate
	}
	return names
}
func hasFinding(report harness.Report, gate, ruleID, message string) bool {
	for _, candidate := range report.Findings {
		if candidate.Gate == gate && candidate.RuleID == ruleID && candidate.Message == message {
			return true
		}
	}
	return false
}
func lint(file string, lines ...int) []byte {
	issues := make([]string, 0, len(lines))
	for _, line := range lines {
		issues = append(issues, fmt.Sprintf(`{"FromLinter":"errcheck","Text":"Error return value is not checked","Severity":"warning","Pos":{"Filename":%q,"Line":%d,"Column":1}}`, file, line))
	}
	return []byte(`{"Issues":[` + strings.Join(issues, ",") + `]}`)
}
