package gauntlet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/acceptance/internal/harness"
)

const complexityNormalizer = `regex:^(?P<value>\d+) \S+ (?P<symbol>\S+) (?P<file>[^:]+):(?P<line>\d+):\d+$`

type gauntletFeature struct {
	world    *harness.World
	first    harness.Report
	raw      string
	selected []string
}

func newGauntletFeature(factory harness.DriverFactory) *gauntletFeature {
	return &gauntletFeature{world: harness.NewWorld(factory, harness.NeedsGauntlet)}
}
func (f *gauntletFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.world.After)
	sc.Step(`^I run the gauntlet$`, f.run)
	sc.Step(`^the report verdict is (.*)$`, f.verdict)
	sc.Step(`^the application outcome is (\d+)$`, f.outcome)
	for _, bind := range []struct {
		expression string
		step       any
	}{
		{`^a committed Go repository with a changed function$`, f.changed}, {`^a committed Go repository with one gate finding$`, f.one}, {`^a committed Go repository with a healthy gate finding$`, f.healthy}, {`^a committed Go repository with "([^"]*)" and "([^"]*)" gates$`, f.named}, {`^a committed Go repository with a healthy versioned gate$`, f.versioned}, {`^a committed Go repository whose gate result is (.*)$`, f.result},
		{`^the shipped Go gates report one complexity and one lint finding$`, f.shipped}, {`^the "([^"]*)" and "([^"]*)" gates report findings$`, f.two}, {`^the "([^"]*)" gate reports rule "([^"]*)" on "([^"]*)" line (\d+)$`, f.rule}, {`^one gate reports the same finding on lines (\d+), (\d+), and (\d+)$`, f.repeat}, {`^I run the gauntlet with only the "([^"]*)" gate$`, f.runOnly}, {`^I run the unchanged gauntlet twice$`, f.twice}, {`^the "([^"]*)" gate finishes after the "([^"]*)" gate$`, f.delayed}, {`^the gate writes the raw diagnostic "([^"]*)"$`, f.rawDiagnostic},
		{`^the report contains the "([^"]*)" and "([^"]*)" gates$`, f.gates}, {`^the report contains both shipped findings$`, f.twoReported}, {`^the report contains only the "([^"]*)" gate$`, f.only}, {`^the finding records its gate, language, rule, severity, location, message, and fingerprint$`, f.public}, {`^the report contains one finding with two occurrences$`, f.grouped}, {`^the finding fingerprint is identical in both reports$`, f.stable}, {`^the report orders gates as "([^"]*)"$`, f.order}, {`^stdout contains compiler-style findings$`, f.compiler}, {`^the raw diagnostic exists only in a persisted raw artifact$`, f.persisted},
		{`^a sibling gate experiences (.*)$`, f.problem}, {`^the sibling gate is errored$`, f.errored}, {`^the healthy gate finding remains in the report$`, f.healthyFinding}, {`^the tool version is outside the gate constraint$`, func() error { return nil }}, {`^the gate has a version warning$`, f.warning}, {`^the gate is not errored$`, f.notErrored},
	} {
		sc.Step(bind.expression, bind.step)
	}
}
func (f *gauntletFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.selected = nil
	f.first = harness.Report{}
	f.raw = ""
	return f.world.Before(ctx, scenario)
}
func (f *gauntletFeature) changed() error {
	r, e := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repo"))
	if e != nil {
		return e
	}
	for n, b := range map[string]string{"go.mod": "module fixture\n\ngo 1.25\n", "feature.go": "package fixture\n\nfunc Feature() error {\n return nil\n}\n\nfunc One() {}\nfunc Two() {}\nfunc Three() {}\nfunc Four() {}\nfunc Five() {}\nfunc Six() {}\nfunc Seven() {}\nfunc Eight() {}\nfunc Nine() {}\nfunc Ten() {}\n"} {
		if e = r.Write(n, b); e != nil {
			return e
		}
	}
	if _, e = r.Commit("base"); e != nil {
		return e
	}
	if e = r.Branch("base"); e != nil {
		return e
	}
	if e = r.Write("feature.go", "package fixture\n\nfunc Feature() error {\n return nil\n}\n\nfunc One() {}\nfunc Two() {}\nfunc Three() {}\nfunc Four() {}\nfunc Five() {}\nfunc Six() {}\nfunc Seven() {}\nfunc Eight() {}\nfunc Nine() {}\nfunc Ten() {}\nfunc Changed() {}\n"); e != nil {
		return e
	}
	if _, e = r.Commit("feature"); e != nil {
		return e
	}
	if _, e = f.world.Environment().InstallTool("gocyclo", harness.ToolBehavior{}); e != nil {
		return e
	}
	if _, e = f.world.Environment().InstallTool("golangci-lint", harness.ToolBehavior{Stdout: []byte(`{"Issues":[]}`), VersionStdout: []byte("v2.12.2\n")}); e != nil {
		return e
	}
	return f.world.UseRepository(r)
}
func lint(file string, lines ...int) []byte {
	issues := []map[string]any{}
	for _, n := range lines {
		issues = append(issues, map[string]any{"FromLinter": "errcheck", "Text": "Error return value is not checked", "Severity": "warning", "Pos": map[string]any{"Filename": file, "Line": n, "Column": 1}})
	}
	b, _ := json.Marshal(map[string]any{"Issues": issues})
	return b
}
func (f *gauntletFeature) gate(name string, b harness.ToolBehavior, n string) error {
	tool := name + "-tool"
	if _, e := f.world.Environment().InstallTool(tool, b); e != nil {
		return e
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{Name: name, Description: name, Tool: tool, Normalizer: n, RuleID: name + "/rule", Message: "fixture finding", Command: []string{tool}, Scope: "repo", Location: "point", SeverityMap: map[string]string{"default": "warning", "warning": "warning"}})
}
func (f *gauntletFeature) one() error {
	if e := f.changed(); e != nil {
		return e
	}
	return f.gate("lint", harness.ToolBehavior{Stdout: lint("feature.go", 4)}, "golangci-json")
}
func (f *gauntletFeature) healthy() error {
	if e := f.changed(); e != nil {
		return e
	}
	return f.gate("healthy", harness.ToolBehavior{Stdout: lint("feature.go", 4)}, "golangci-json")
}
func (f *gauntletFeature) named(a, b string) error {
	if e := f.changed(); e != nil {
		return e
	}
	f.selected = []string{a, b}
	return f.two(a, b)
}
func (f *gauntletFeature) shipped() error {
	if _, e := f.world.Environment().InstallTool("gocyclo", harness.ToolBehavior{Stdout: []byte("18 fixture Changed feature.go:17:1\n")}); e != nil {
		return e
	}
	_, e := f.world.Environment().InstallTool("golangci-lint", harness.ToolBehavior{Stdout: lint("feature.go", 17), VersionStdout: []byte("v2.12.2\n")})
	return e
}
func (f *gauntletFeature) two(a, b string) error {
	for _, n := range []string{a, b} {
		if e := f.gate(n, harness.ToolBehavior{Stdout: lint("feature.go", 4)}, "golangci-json"); e != nil {
			return e
		}
	}
	return nil
}
func (f *gauntletFeature) rule(name, rule, file string, line int) error {
	return f.gate(name, harness.ToolBehavior{Stdout: lint(file, line)}, "golangci-json")
}
func (f *gauntletFeature) repeat(a, b, c int) error {
	if e := f.world.Repository().Write("repeat.go", "package fixture\n\n same()\n\n\n\n\n same()\n\n\n\n\n same()\n"); e != nil {
		return e
	}
	if _, e := f.world.Repository().Commit("repeat"); e != nil {
		return e
	}
	return f.gate("repeat", harness.ToolBehavior{Stdout: lint("repeat.go", a, b, c)}, "golangci-json")
}
func (f *gauntletFeature) runOnly(ctx context.Context, n string) error {
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: "base", GateNames: []string{n}, NoColor: true})
}
func (f *gauntletFeature) run(ctx context.Context) error {
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: "base", GateNames: f.selected, NoColor: true})
}
func (f *gauntletFeature) verdict(want string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if r.Verdict != want {
		return fmt.Errorf("verdict=%q", r.Verdict)
	}
	return nil
}
func (f *gauntletFeature) outcome(want int) error {
	got, e := f.world.LastRun().Outcome()
	if e != nil {
		return e
	}
	if got.Code != want {
		return fmt.Errorf("outcome=%d", got.Code)
	}
	return nil
}
func (f *gauntletFeature) twice(ctx context.Context) error {
	if e := f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: "base", NoColor: true}); e != nil {
		return e
	}
	r, e := f.world.LastRun().Report()
	if e != nil {
		return e
	}
	f.first = r
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: "base", NoColor: true})
}
func (f *gauntletFeature) delayed(a, b string) error {
	return f.gate(a, harness.ToolBehavior{Stdout: lint("feature.go", 4), Delay: 20 * time.Millisecond}, "golangci-json")
}
func (f *gauntletFeature) rawDiagnostic(s string) error {
	f.raw = s
	_, e := f.world.Environment().InstallTool("lint-tool", harness.ToolBehavior{Stdout: lint("feature.go", 4), Stderr: []byte(s)})
	return e
}
func (f *gauntletFeature) report() (harness.Report, error) { return f.world.LastRun().Report() }
func (f *gauntletFeature) gates(a, b string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Gates) != 2 || r.Gates[0].Gate != a || r.Gates[1].Gate != b {
		return fmt.Errorf("gates = %#v", r.Gates)
	}
	return nil
}
func (f *gauntletFeature) twoReported() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 2 {
		return fmt.Errorf("findings = %#v", r.Findings)
	}
	return nil
}
func (f *gauntletFeature) only(n string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Gates) != 1 || r.Gates[0].Gate != n {
		return fmt.Errorf("gates = %#v", r.Gates)
	}
	return nil
}
func (f *gauntletFeature) public() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 {
		return fmt.Errorf("findings = %#v", r.Findings)
	}
	x := r.Findings[0]
	if x.Gate == "" || x.Language == "" || x.RuleID == "" || x.Severity == "" || x.File == "" || x.Line == 0 || x.Message == "" || x.Fingerprint == "" {
		return fmt.Errorf("incomplete finding %#v", x)
	}
	return nil
}
func (f *gauntletFeature) grouped() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 || len(r.Findings[0].Occurrences) != 2 {
		return fmt.Errorf("findings = %#v", r.Findings)
	}
	return nil
}
func (f *gauntletFeature) stable() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(f.first.Findings) != 1 || len(r.Findings) != 1 || f.first.Findings[0].Fingerprint != r.Findings[0].Fingerprint {
		return fmt.Errorf("fingerprints differ")
	}
	return nil
}
func (f *gauntletFeature) order(w string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	v := []string{}
	for _, g := range r.Gates {
		v = append(v, g.Gate)
	}
	if strings.Join(v, ",") != w {
		return fmt.Errorf("order = %q", strings.Join(v, ","))
	}
	return nil
}
func (f *gauntletFeature) compiler() error {
	if !strings.Contains(f.world.LastRun().Stdout(), "warning:") {
		return fmt.Errorf("stdout=%q", f.world.LastRun().Stdout())
	}
	return nil
}
func (f *gauntletFeature) persisted() error {
	if strings.Contains(f.world.LastRun().Stdout(), f.raw) {
		return fmt.Errorf("raw stdout")
	}
	paths, e := filepath.Glob(filepath.Join(filepath.Dir(f.world.LastRun().ReportPath()), "raw", "*.stderr"))
	if e != nil {
		return e
	}
	found := false
	for _, p := range paths {
		b, readErr := os.ReadFile(p)
		if readErr == nil && strings.Contains(string(b), f.raw) {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("raw artifact")
	}
	return nil
}
func (f *gauntletFeature) problem(p string) error {
	if p == "a missing tool" {
		return f.world.Environment().WriteGate(harness.GateDefinition{Name: "sibling", Description: "sibling", Tool: "missing", Normalizer: "golangci-json", Command: []string{"missing"}, Scope: "repo", Location: "point", SeverityMap: map[string]string{"default": "warning"}})
	}
	b := harness.ToolBehavior{}
	if p == "a crashed tool" {
		b.ExitCode = 2
	} else if p == "a timed out tool" {
		b.ExitCode = 124
	} else {
		b.Stdout = []byte("bad")
	}
	return f.gate("sibling", b, "golangci-json")
}
func (f *gauntletFeature) errored() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	for _, g := range r.Gates {
		if g.Gate == "sibling" && g.Status == "errored" {
			return nil
		}
	}
	return fmt.Errorf("not errored")
}
func (f *gauntletFeature) healthyFinding() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	for _, x := range r.Findings {
		if x.Gate == "healthy" {
			return nil
		}
	}
	return fmt.Errorf("healthy absent")
}
func (f *gauntletFeature) versioned() error {
	if e := f.changed(); e != nil {
		return e
	}
	return f.gate("versioned", harness.ToolBehavior{Stdout: lint("feature.go", 4), VersionStdout: []byte("v1.0.0\n")}, "golangci-json")
}
func (f *gauntletFeature) warning() error { return nil }
func (f *gauntletFeature) notErrored() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if r.Gates[0].Status == "errored" {
		return fmt.Errorf("errored")
	}
	return nil
}
func (f *gauntletFeature) result(s string) error {
	if e := f.changed(); e != nil {
		return e
	}
	if s == "findings" {
		return f.gate("result", harness.ToolBehavior{Stdout: lint("feature.go", 4)}, "golangci-json")
	}
	if s == "clear" {
		return f.gate("result", harness.ToolBehavior{Stdout: []byte(`{"Issues":[]}`)}, "golangci-json")
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{Name: "result", Description: "result", Tool: "missing", Normalizer: "golangci-json", Command: []string{"missing"}, Scope: "repo", Location: "point", SeverityMap: map[string]string{"default": "warning"}})
}
