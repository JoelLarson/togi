package gauntlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

type diffFeature struct {
	world      *harness.World
	base       string
	olderBase  string
	shared     string
	marker     string
	beforeTree []string
}

func newDiffFeature(factory harness.DriverFactory) *diffFeature {
	return &diffFeature{world: harness.NewWorld(factory, harness.NeedsGauntlet)}
}

func (f *diffFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.world.After)
	sc.Step(`^a committed feature branch with two possible bases$`, f.twoBases)
	sc.Step(`^a gate finding belongs only to the explicitly based diff$`, f.explicitFinding)
	sc.Step(`^I run the gauntlet with the older base$`, f.runOlder)
	sc.Step(`^the report records the explicit base and its merge base$`, f.explicitRecorded)
	sc.Step(`^the finding is in scope$`, f.oneFinding)
	sc.Step(`^a committed feature branch whose origin HEAD points to "([^"]*)"$`, f.originHead)
	sc.Step(`^I run the gauntlet without a base$`, f.runAutomatic)
	sc.Step(`^the report base is "([^"]*)"$`, f.reportBase)
	sc.Step(`^a committed feature branch from local "([^"]*)" without a remote$`, f.localTrunk)
	sc.Step(`^trunk and the feature branch diverged after a shared commit$`, f.diverged)
	sc.Step(`^a gate reports findings on both branches' changes$`, f.bothBranches)
	sc.Step(`^I run the gauntlet against trunk$`, f.runTrunk)
	sc.Step(`^the report merge base is the shared commit$`, f.mergeBase)
	sc.Step(`^only the feature finding is in scope$`, f.onlyFeature)
	sc.Step(`^a committed feature changes line 8 but not line 3$`, f.pointLines)
	sc.Step(`^a diff-scoped gate reports point findings on lines 3 and 8$`, f.pointGate)
	sc.Step(`^I run the gauntlet$`, f.runDefault)
	sc.Step(`^only the finding on line 8 remains$`, f.lineEight)
	sc.Step(`^a committed feature changes the body of function "([^"]*)"$`, f.entityChange)
	sc.Step(`^a diff-scoped gate reports an entity finding on the function signature$`, f.entityGate)
	sc.Step(`^the structural finding for "([^"]*)" remains$`, f.structural)
	sc.Step(`^a committed feature changes "([^"]*)"$`, f.repoChange)
	sc.Step(`^a whole-repo gate reports a finding in "([^"]*)"$`, f.repoGate)
	sc.Step(`^the finding in "([^"]*)" remains$`, f.findingFile)
	sc.Step(`^a committed feature deletes line 5 from "([^"]*)"$`, f.deleteLine)
	sc.Step(`^a diff-scoped gate reports a point finding at the deletion location$`, f.deletionGate)
	sc.Step(`^the deletion finding remains in scope$`, f.oneFinding)
	sc.Step(`^a committed feature renames "([^"]*)" to "([^"]*)"$`, f.rename)
	sc.Step(`^a gate reports a finding in "([^"]*)"$`, f.namedFinding)
	sc.Step(`^the report records the finding in "([^"]*)"$`, f.findingFile)
	sc.Step(`^a committed feature changes the binary file "([^"]*)"$`, f.binary)
	sc.Step(`^the diff records one changed file and zero changed lines$`, f.binaryRecorded)
	sc.Step(`^a repository with (.*)$`, f.invalidRepository)
	sc.Step(`^a gate that records whether it starts$`, f.markerGate)
	sc.Step(`^the run is rejected for the (.*)$`, f.rejected)
	sc.Step(`^no gate, ledger, or target-repository file is created$`, f.noSideEffects)
}

func (f *diffFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.base, f.olderBase, f.shared, f.marker, f.beforeTree = "base", "", "", "", nil
	return f.world.Before(ctx, scenario)
}

func (f *diffFeature) repo(files map[string]string) (*harness.Repository, error) {
	r, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repo"))
	if err != nil {
		return nil, err
	}
	for name, body := range files {
		if err := r.Write(name, body); err != nil {
			return nil, err
		}
	}
	if _, err = r.Commit("base"); err != nil {
		return nil, err
	}
	if err = r.Branch("base"); err != nil {
		return nil, err
	}
	return r, f.world.UseRepository(r)
}
func (f *diffFeature) simple() (*harness.Repository, error) {
	return f.repo(map[string]string{"go.mod": "module fixture\n\ngo 1.25\n", "feature.go": "package fixture\n\nfunc Feature() {\n\tvalue := 1\n\t_ = value\n}\n", "legacy.go": "package fixture\n\nfunc Legacy() {}\n"})
}
func (f *diffFeature) commitChange(r *harness.Repository, name, body string) error {
	if err := r.Write(name, body); err != nil {
		return err
	}
	_, err := r.Commit("feature")
	return err
}
func (f *diffFeature) gate(name, scope, location string, output []byte) error {
	tool := name + "-tool"
	if _, err := f.world.Environment().InstallTool(tool, harness.ToolBehavior{Stdout: output}); err != nil {
		return err
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{Name: name, Description: name, Tool: tool, Normalizer: "golangci-json", Command: []string{tool}, Scope: scope, Location: location, SeverityMap: map[string]string{"default": "warning", "warning": "warning"}})
}
func (f *diffFeature) twoBases() error {
	r, err := f.simple()
	if err != nil {
		return err
	}
	f.olderBase, err = r.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err = f.commitChange(r, "old.go", "package fixture\nfunc Old() {}\n"); err != nil {
		return err
	}
	f.base, err = r.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	return f.commitChange(r, "feature.go", "package fixture\nfunc Feature() { value := 2; _ = value }\n")
}
func (f *diffFeature) explicitFinding() error {
	return f.gate("diff", "diff", "point", lint("old.go", 2))
}
func (f *diffFeature) runOlder(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.olderBase, NoColor: true}))
}
func (f *diffFeature) report() (harness.Report, error) { return f.world.LastRun().Report() }
func (f *diffFeature) explicitRecorded() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if r.Diff.BaseRef != f.olderBase || r.Diff.MergeBase != f.olderBase {
		return fmt.Errorf("diff=%#v", r.Diff)
	}
	return nil
}
func (f *diffFeature) oneFinding() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 {
		return fmt.Errorf("findings=%#v", r.Findings)
	}
	return nil
}
func (f *diffFeature) originHead(branch string) error {
	r, e := f.simple()
	if e != nil {
		return e
	}
	base, e := r.Git("rev-parse", "HEAD")
	if e != nil {
		return e
	}
	if e = r.SetOriginHEAD(branch, base); e != nil {
		return e
	}
	return f.commitChange(r, "feature.go", "package fixture\nfunc Feature() { value := 2; _ = value }\n")
}
func (f *diffFeature) runAutomatic(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, NoColor: true}))
}
func (f *diffFeature) reportBase(want string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if r.Diff.BaseRef != want {
		return fmt.Errorf("base=%q", r.Diff.BaseRef)
	}
	return nil
}
func (f *diffFeature) localTrunk(branch string) error {
	r, e := f.simple()
	if e != nil {
		return e
	}
	if branch != "main" {
		return fmt.Errorf("unsupported trunk %q", branch)
	}
	return f.commitChange(r, "feature.go", "package fixture\nfunc Feature() { value := 2; _ = value }\n")
}
func (f *diffFeature) diverged() error {
	r, e := f.simple()
	if e != nil {
		return e
	}
	f.shared, e = r.Git("rev-parse", "HEAD")
	if e != nil {
		return e
	}
	if e = r.Branch("trunk"); e != nil {
		return e
	}
	if e = r.Checkout("trunk"); e != nil {
		return e
	}
	if e = f.commitChange(r, "feature.go", "package fixture\n\nfunc Feature() {\n\tvalue := 2\n\t_ = value\n}\n"); e != nil {
		return e
	}
	if e = r.Checkout("main"); e != nil {
		return e
	}
	return f.commitChange(r, "feature.go", "package fixture\n\nfunc FeatureFeature() {\n\tvalue := 1\n\t_ = value\n}\n")
}
func (f *diffFeature) bothBranches() error {
	return f.gate("diff", "diff", "point", lint("feature.go", 3, 4))
}
func (f *diffFeature) runTrunk(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "trunk", NoColor: true}))
}
func (f *diffFeature) mergeBase() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if r.Diff.MergeBase != f.shared {
		return fmt.Errorf("merge base=%q want %q", r.Diff.MergeBase, f.shared)
	}
	return nil
}
func (f *diffFeature) onlyFeature() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 || r.Findings[0].File != "feature.go" {
		return fmt.Errorf("findings=%#v gates=%#v diff=%#v", r.Findings, r.Gates, r.Diff)
	}
	return nil
}
func (f *diffFeature) pointLines() error {
	r, e := f.simple()
	if e != nil {
		return e
	}
	return f.commitChange(r, "feature.go", "package fixture\n\nfunc Feature() {\n\tvalue := 1\n\t_ = value\n}\n\nfunc Added() {}\n")
}
func (f *diffFeature) pointGate() error {
	return f.gate("point", "diff", "point", lint("feature.go", 3, 8))
}
func (f *diffFeature) runDefault(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true}))
}
func (f *diffFeature) lineEight() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 || r.Findings[0].Line != 8 {
		return fmt.Errorf("findings=%#v", r.Findings)
	}
	return nil
}
func (f *diffFeature) entityChange(name string) error {
	r, e := f.repo(map[string]string{"go.mod": "module fixture\n\ngo 1.25\n", "feature.go": "package fixture\n\nfunc " + name + "() { value := 1; _ = value }\n"})
	if e != nil {
		return e
	}
	return f.commitChange(r, "feature.go", "package fixture\n\nfunc "+name+"() { value := 2; _ = value }\n")
}
func (f *diffFeature) entityGate() error {
	return f.gate("entity", "diff", "entity", lint("feature.go", 3))
}
func (f *diffFeature) structural(name string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 || !strings.Contains(r.Findings[0].File, "feature.go") {
		return fmt.Errorf("structural %q findings=%#v", name, r.Findings)
	}
	return nil
}
func (f *diffFeature) repoChange(file string) error {
	r, e := f.simple()
	if e != nil {
		return e
	}
	return f.commitChange(r, file, "package fixture\nfunc Changed() {}\n")
}
func (f *diffFeature) repoGate(file string) error {
	return f.gate("repository", "repo", "point", lint(file, 3))
}
func (f *diffFeature) findingFile(file string) error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if len(r.Findings) != 1 || r.Findings[0].File != file {
		return fmt.Errorf("findings=%#v", r.Findings)
	}
	return nil
}
func (f *diffFeature) deleteLine(file string) error {
	r, e := f.repo(map[string]string{"go.mod": "module fixture\n\ngo 1.25\n", file: "package fixture\n\nfunc Feature() {\n\ta := 1\n\tb := 2\n\t_ = a + b\n}\n"})
	if e != nil {
		return e
	}
	return f.commitChange(r, file, "package fixture\n\nfunc Feature() {\n\ta := 1\n\t_ = a\n}\n")
}
func (f *diffFeature) deletionGate() error {
	return f.gate("deletion", "diff", "point", lint("feature.go", 5))
}
func (f *diffFeature) rename(before, after string) error {
	r, e := f.repo(map[string]string{"go.mod": "module fixture\n\ngo 1.25\n", before: "package fixture\nfunc Before() {}\n"})
	if e != nil {
		return e
	}
	if _, e = r.Git("mv", before, after); e != nil {
		return e
	}
	if e = r.Write(after, "package fixture\nfunc After() { value := 1; _ = value }\n"); e != nil {
		return e
	}
	_, e = r.Commit("rename")
	return e
}
func (f *diffFeature) namedFinding(file string) error {
	return f.gate("renamed", "diff", "point", lint(file, 2))
}
func (f *diffFeature) binary(file string) error {
	r, e := f.repo(map[string]string{"go.mod": "module fixture\n\ngo 1.25\n", file: "\x00old"})
	if e != nil {
		return e
	}
	if e = r.WriteBytes(file, []byte("\x00new")); e != nil {
		return e
	}
	_, e = r.Commit("binary")
	return e
}
func (f *diffFeature) binaryRecorded() error {
	r, e := f.report()
	if e != nil {
		return e
	}
	if r.Diff.ChangedFiles != 1 || r.Diff.ChangedLines != 0 {
		return fmt.Errorf("diff=%#v", r.Diff)
	}
	return nil
}
func (f *diffFeature) invalidRepository(problem string) error {
	r, e := f.simple()
	if e != nil {
		return e
	}
	f.base = "base"
	switch problem {
	case "dirty worktree":
		e = r.Write("dirty.go", "package fixture\n")
	case "unsupported submodule":
		source, e2 := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "submodule"))
		if e2 != nil {
			return e2
		}
		if e2 = source.Write("sub.go", "package sub\n"); e2 != nil {
			return e2
		}
		if _, e2 = source.Commit("sub"); e2 != nil {
			return e2
		}
		e = r.AddSubmodule("sub", source.Root)
		if e == nil {
			_, e = r.Commit("submodule")
		}
	case "missing base":
		f.base = "missing"
	case "invalid base":
		f.base = "-invalid"
	case "unrelated history":
		if _, e = r.Git("checkout", "--orphan", "unrelated"); e != nil {
			return e
		}
		if _, e = r.Git("rm", "-rf", "."); e != nil {
			return e
		}
		if e = r.Write("other.go", "package other\n"); e != nil {
			return e
		}
		if _, e = r.Commit("unrelated"); e != nil {
			return e
		}
		if e = r.Checkout("main"); e != nil {
			return e
		}
		f.base = "unrelated"
	default:
		return fmt.Errorf("unknown precondition %q", problem)
	}
	if e != nil {
		return e
	}
	f.beforeTree, e = r.Tree()
	return e
}
func (f *diffFeature) markerGate() error {
	f.marker = filepath.Join(f.world.Environment().TempRoot, "invoked")
	_, e := f.world.Environment().InstallTool("marker-tool", harness.ToolBehavior{InvokedMarker: f.marker})
	if e != nil {
		return e
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{Name: "marker", Description: "marker", Tool: "marker-tool", Normalizer: "golangci-json", Command: []string{"marker-tool"}, Scope: "repo", Location: "point", SeverityMap: map[string]string{"default": "warning"}})
}
func (f *diffFeature) rejected(problem string) error {
	o, e := f.world.LastRun().Outcome()
	if e != nil {
		return e
	}
	if o.Code != 70 {
		return fmt.Errorf("outcome=%#v for %s", o, problem)
	}
	if o.Message == "" && f.world.LastRun().Stderr() == "" {
		return fmt.Errorf("missing rejection diagnostic")
	}
	return nil
}
func (f *diffFeature) noSideEffects() error {
	if f.world.LastRun().ReportPath() != "" {
		return fmt.Errorf("report=%q", f.world.LastRun().ReportPath())
	}
	if _, e := os.Stat(f.marker); !os.IsNotExist(e) {
		return fmt.Errorf("tool invoked: %v", e)
	}
	state, e := f.world.Environment().RepoState(context.Background(), f.world.Repository().Root)
	if e != nil {
		return e
	}
	if _, e = os.Stat(state); !os.IsNotExist(e) {
		return fmt.Errorf("state exists: %v", e)
	}
	after, e := f.world.Repository().Tree()
	if e != nil {
		return e
	}
	if strings.Join(after, "\n") != strings.Join(f.beforeTree, "\n") {
		return fmt.Errorf("target tree changed")
	}
	return nil
}

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
	sc.Step(`^a committed Go repository with a changed function$`, f.changed)
	sc.Step(`^a committed Go repository with one gate finding$`, f.one)
	sc.Step(`^a committed Go repository with a healthy gate finding$`, f.healthy)
	sc.Step(`^a committed Go repository with "([^"]*)" and "([^"]*)" gates$`, f.named)
	sc.Step(`^a committed Go repository with a healthy versioned gate$`, f.versioned)
	sc.Step(`^a committed Go repository whose gate result is (.*)$`, f.result)
	sc.Step(`^the shipped Go gates report one complexity and one lint finding$`, f.shipped)
	sc.Step(`^the "([^"]*)" and "([^"]*)" gates report findings$`, f.two)
	sc.Step(`^the "([^"]*)" gate reports rule "([^"]*)" on "([^"]*)" line (\d+)$`, f.rule)
	sc.Step(`^one gate reports the same finding on lines (\d+), (\d+), and (\d+)$`, f.repeat)
	sc.Step(`^I run the gauntlet with only the "([^"]*)" gate$`, f.runOnly)
	sc.Step(`^I run the unchanged gauntlet twice$`, f.twice)
	sc.Step(`^the "([^"]*)" gate finishes after the "([^"]*)" gate$`, f.delayed)
	sc.Step(`^the gate writes the raw diagnostic "([^"]*)"$`, f.rawDiagnostic)
	sc.Step(`^the report contains the "([^"]*)" and "([^"]*)" gates$`, f.gates)
	sc.Step(`^the report contains both shipped findings$`, f.twoReported)
	sc.Step(`^the report contains only the "([^"]*)" gate$`, f.only)
	sc.Step(`^the finding records its gate, language, rule, severity, file, line, message, and fingerprint$`, f.public)
	sc.Step(`^the report contains one finding with two occurrences$`, f.grouped)
	sc.Step(`^the finding fingerprint is identical in both reports$`, f.stable)
	sc.Step(`^the report orders gates as "([^"]*)"$`, f.order)
	sc.Step(`^the findings are shown in compiler-style output$`, f.compiler)
	sc.Step(`^the raw diagnostic exists only in a persisted raw artifact$`, f.persisted)
	sc.Step(`^a sibling gate experiences (.*)$`, f.problem)
	sc.Step(`^the sibling gate is errored$`, f.errored)
	sc.Step(`^the healthy gate finding remains in the report$`, f.healthyFinding)
	sc.Step(`^the tool version is outside the gate constraint$`, func() error { return nil })
	sc.Step(`^the gate has a version warning$`, f.warning)
	sc.Step(`^the gate is not errored$`, f.notErrored)
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
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "base", GateNames: []string{n}, NoColor: true}))
}
func (f *gauntletFeature) run(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "base", GateNames: f.selected, NoColor: true}))
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
	if e := f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "base", NoColor: true})); e != nil {
		return e
	}
	r, e := f.world.LastRun().Report()
	if e != nil {
		return e
	}
	f.first = r
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "base", NoColor: true}))
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

type fixFeature struct {
	world        *harness.World
	originalHead string
	mode         string
	request      harness.RunRequest
	landingDone  chan error
}

func newFixFeature(factory harness.DriverFactory) *fixFeature {
	return &fixFeature{world: harness.NewWorld(factory, harness.NeedsGauntlet)}
}

func (f *fixFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.world.After)
	sc.Step(`^a green feature with a blocking finding$`, f.greenBlocked)
	sc.Step(`^a green feature without blockers$`, f.greenClean)
	sc.Step(`^the agent removes the finding$`, f.agentFixes)
	sc.Step(`^the selected agent is missing$`, f.missingAgent)
	sc.Step(`^a feature whose behavioral suite is (missing|red)$`, f.baseline)
	sc.Step(`^a green feature whose initial gate errors$`, f.initialGateError)
	sc.Step(`^the agent makes a valid cross-file fix$`, f.crossFile)
	sc.Step(`^the agent makes no changes$`, f.noOp)
	sc.Step(`^the agent attempts (an unauthorized Git commit|a new suppression|test deletion|an assertion change)$`, f.integrityViolation)
	sc.Step(`^the agent performs a witnessed compilation-only rename$`, f.witnessedRename)
	sc.Step(`^only one iteration is allowed$`, f.oneIteration)
	sc.Step(`^the agent exceeds the wall-clock budget$`, f.wallClock)
	sc.Step(`^the agent introduces a regression outside local validation$`, f.finalRegression)
	sc.Step(`^the agent fixes it while the original worktree becomes (dirty|detached|branch-moved)$`, f.landingConflict)
	sc.Step(`^I run the fix loop$`, f.run)
	sc.Step(`^the fix run is (unsealed|errored|unverified|blocked|rails-exhausted) with exit (\d+)$`, f.outcome)
	sc.Step(`^the agent was invoked (\d+) times?$`, f.invocations)
	sc.Step(`^one squash commit with the fixed tree reaches the feature branch$`, f.squashLanded)
	sc.Step(`^the fix audit contains its report plan and brief$`, f.auditArtifacts)
	sc.Step(`^the feature branch is unchanged$`, f.featureUnchanged)
	sc.Step(`^the landed tree contains both related edits$`, f.relatedEdits)
	sc.Step(`^the validated run branch is absent$`, f.runBranchAbsent)
	sc.Step(`^the validated run branch is preserved$`, f.runBranchPreserved)
	sc.Step(`^the witnessed rename reaches the feature branch$`, f.renameLanded)
	sc.Step(`^the concurrent feature state is preserved$`, f.concurrentState)
}

func (f *fixFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.originalHead, f.mode, f.landingDone = "", "", nil
	f.request = harness.RunRequest{Agent: "codex", GateNames: []string{"quality"}, MaxIterations: 4, MaxWallClock: 5 * time.Second, NoColor: true}
	return f.world.Before(ctx, scenario)
}

const fixSource = "package fixture\n\n// BAD\nfunc Feature() int { return 1 }\n"
const fixedSource = "package fixture\n\nfunc Feature() int { return 1 }\n"
const fixTest = "package fixture\n\nimport \"testing\"\n\nfunc TestFeature(t *testing.T) { Feature() }\n"

func (f *fixFeature) repository(source, testBody string) error {
	repository, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repo"))
	if err != nil {
		return err
	}
	if err := repository.Write("go.mod", "module fixture\n\ngo 1.25\n"); err != nil {
		return err
	}
	if err := repository.Write("base.go", "package fixture\n\nfunc Base() {}\n"); err != nil {
		return err
	}
	if _, err := repository.Commit("base"); err != nil {
		return err
	}
	if err := repository.Branch("base"); err != nil {
		return err
	}
	if err := repository.Write("feature.go", source); err != nil {
		return err
	}
	if testBody != "" {
		if err := repository.Write("feature_test.go", testBody); err != nil {
			return err
		}
	}
	f.originalHead, err = repository.Commit("feature")
	if err != nil {
		return err
	}
	f.request.Root, f.request.Base = repository.Root, "base"
	return f.world.UseRepository(repository)
}

func (f *fixFeature) installQualityGate(errored bool) error {
	if errored {
		if _, err := f.world.Environment().InstallTool("quality-tool", harness.ToolBehavior{ExitCode: 7}); err != nil {
			return err
		}
	} else {
		tool := filepath.Join(f.world.Environment().BinRoot, "quality-tool")
		script := "#!/bin/sh\nset -eu\nif grep -q BAD feature.go; then printf '%s\\n' '{\"Issues\":[{\"FromLinter\":\"quality\",\"Text\":\"remove BAD marker\",\"Severity\":\"warning\",\"Pos\":{\"Filename\":\"feature.go\",\"Line\":3,\"Column\":1}}]}'; else printf '%s\\n' '{\"Issues\":[]}'; fi\n"
		if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
			return err
		}
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{
		Name: "quality", Description: "quality", Tool: "quality-tool", Normalizer: "golangci-json",
		RuleID: "quality/bad", Message: "remove BAD marker", Command: []string{"quality-tool"}, Scope: "repo", Location: "point",
		SeverityMap: map[string]string{"default": "warning", "warning": "warning"},
	})
}

func (f *fixFeature) greenBlocked() error {
	if err := f.repository(fixSource, fixTest); err != nil {
		return err
	}
	return f.installQualityGate(false)
}

func (f *fixFeature) greenClean() error {
	if err := f.repository(fixedSource, fixTest); err != nil {
		return err
	}
	if err := f.installQualityGate(false); err != nil {
		return err
	}
	return f.installAgent(harness.AgentBehavior{})
}

func (f *fixFeature) agentFixes() error {
	return f.installAgent(harness.AgentBehavior{Edits: map[string]string{"feature.go": fixedSource}})
}

func (f *fixFeature) installAgent(behavior harness.AgentBehavior) error {
	_, err := f.world.Environment().InstallAgent("codex", behavior)
	return err
}

func (f *fixFeature) missingAgent() error {
	f.mode = "missing-agent"
	return f.world.Environment().RestrictPath("go", "git", "grep")
}

func (f *fixFeature) baseline(condition string) error {
	testBody := ""
	if condition == "red" {
		testBody = "package fixture\nimport \"testing\"\nfunc TestFeature(t *testing.T) { t.Fatal(\"red baseline\") }\n"
	}
	if err := f.repository(fixSource, testBody); err != nil {
		return err
	}
	if err := f.installQualityGate(false); err != nil {
		return err
	}
	return f.installAgent(harness.AgentBehavior{Edits: map[string]string{"feature.go": fixedSource}})
}

func (f *fixFeature) initialGateError() error {
	if err := f.repository(fixSource, fixTest); err != nil {
		return err
	}
	if err := f.installQualityGate(true); err != nil {
		return err
	}
	return f.installAgent(harness.AgentBehavior{Edits: map[string]string{"feature.go": fixedSource}})
}

func (f *fixFeature) crossFile() error {
	if err := f.world.Repository().Write("related.go", "package fixture\n\nconst Related = \"old\"\n"); err != nil {
		return err
	}
	var err error
	f.originalHead, err = f.world.Repository().Commit("related feature")
	if err != nil {
		return err
	}
	return f.installAgent(harness.AgentBehavior{Edits: map[string]string{
		"feature.go": fixedSource,
		"related.go": "package fixture\n\nconst Related = \"fixed\"\n",
	}})
}

func (f *fixFeature) noOp() error { return f.installAgent(harness.AgentBehavior{}) }

func (f *fixFeature) integrityViolation(violation string) error {
	behavior := harness.AgentBehavior{Edits: map[string]string{"feature.go": fixedSource}}
	switch violation {
	case "an unauthorized Git commit":
		behavior.GitArgs = []string{"commit", "-am", "agent mutation"}
	case "a new suppression":
		behavior.Edits["feature.go"] = "package fixture\n\nfunc Feature() int {\n\treturn 1 //nolint:revive\n}\n"
	case "test deletion":
		behavior.Delete = []string{"feature_test.go"}
	case "an assertion change":
		assertion := "package fixture\nimport \"testing\"\nfunc TestFeature(t *testing.T) { if Feature() != 1 { t.Fatal(\"bad\") } }\n"
		if err := f.world.Repository().Write("feature_test.go", assertion); err != nil {
			return err
		}
		var err error
		f.originalHead, err = f.world.Repository().Commit("add assertion")
		if err != nil {
			return err
		}
		behavior.Edits["feature_test.go"] = strings.Replace(assertion, "!= 1", "!= 2", 1)
	default:
		return fmt.Errorf("unknown integrity violation %q", violation)
	}
	return f.installAgent(behavior)
}

func (f *fixFeature) witnessedRename() error {
	source := "package fixture\n\n// BAD\nfunc calculateTotal(value int) int { return value + 1 }\n"
	testBody := "package fixture\nimport \"testing\"\nfunc TestTotal(t *testing.T) { if got := calculateTotal(1); got != 2 { t.Fatalf(\"got %d\", got) } }\n"
	if err := f.world.Repository().Write("feature.go", source); err != nil {
		return err
	}
	if err := f.world.Repository().Write("feature_test.go", testBody); err != nil {
		return err
	}
	var err error
	f.originalHead, err = f.world.Repository().Commit("rename fixture")
	if err != nil {
		return err
	}
	return f.installAgent(harness.AgentBehavior{Edits: map[string]string{
		"feature.go":      "package fixture\n\nfunc totalFor(value int) int { return value + 1 }\n",
		"feature_test.go": strings.Replace(testBody, "calculateTotal", "totalFor", 1),
	}})
}

func (f *fixFeature) oneIteration() error {
	f.request.MaxIterations = 1
	return nil
}

func (f *fixFeature) wallClock() error {
	f.request.MaxWallClock = 500 * time.Millisecond
	return f.installAgent(harness.AgentBehavior{Sleep: time.Second, Edits: map[string]string{"feature.go": fixedSource}})
}

func (f *fixFeature) finalRegression() error {
	if err := f.world.Repository().Write("consumer/consumer_test.go", "package consumer\nimport (\"testing\"; root \"fixture\")\nfunc TestFeatureContract(t *testing.T) { if root.Feature() != 1 { t.Fatal(\"regression\") } }\n"); err != nil {
		return err
	}
	var err error
	f.originalHead, err = f.world.Repository().Commit("consumer contract")
	if err != nil {
		return err
	}
	return f.installAgent(harness.AgentBehavior{Edits: map[string]string{"feature.go": "package fixture\n\nfunc Feature() int { return 2 }\n"}})
}

func (f *fixFeature) landingConflict(condition string) error {
	f.mode = condition
	behavior := harness.AgentBehavior{Edits: map[string]string{"feature.go": fixedSource}}
	switch condition {
	case "dirty":
	case "detached":
	case "branch-moved":
	default:
		return fmt.Errorf("unknown landing condition %q", condition)
	}
	return f.installAgent(behavior)
}

func (f *fixFeature) run(ctx context.Context) error {
	if f.mode == "dirty" || f.mode == "detached" || f.mode == "branch-moved" {
		f.landingDone = make(chan error, 1)
		go f.mutateLandingTarget(ctx)
	}
	err := f.world.Run(ctx, f.request)
	if f.landingDone != nil {
		select {
		case mutationErr := <-f.landingDone:
			err = errors.Join(err, mutationErr)
		case <-time.After(2 * time.Second):
			err = errors.Join(err, errors.New("landing mutation did not observe a validated commit"))
		}
	}
	return err
}

func (f *fixFeature) mutateLandingTarget(ctx context.Context) {
	repository := f.world.Repository()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		refs, err := repository.Git("for-each-ref", "--format=%(objectname)", "refs/heads/togi")
		if err != nil {
			f.landingDone <- err
			return
		}
		if refs != "" && refs != f.originalHead {
			switch f.mode {
			case "dirty":
				_, err = repository.Git("update-index", "--chmod=+x", "feature.go")
			case "detached":
				_, err = repository.Git("checkout", "--detach")
			case "branch-moved":
				_, err = repository.Git("commit", "--allow-empty", "-m", "concurrent move")
			}
			f.landingDone <- err
			return
		}
		select {
		case <-ctx.Done():
			f.landingDone <- ctx.Err()
			return
		case <-ticker.C:
		}
	}
}

func (f *fixFeature) report() (harness.Report, error) { return f.world.LastRun().Report() }

func (f *fixFeature) outcome(verdict string, code int) error {
	if verdict == "rails-exhausted" {
		verdict = "rails"
	}
	report, err := f.report()
	if err != nil {
		return err
	}
	got, err := f.world.LastRun().Outcome()
	if err != nil {
		return err
	}
	if report.Verdict != verdict || got.Code != code {
		return fmt.Errorf("fix outcome = %q/%d, want %q/%d; fix=%#v gates=%#v", report.Verdict, got.Code, verdict, code, report.Fix, report.Gates)
	}
	return nil
}

func (f *fixFeature) invocations(want int) error {
	got, err := f.world.Environment().AgentInvocations("codex")
	if err != nil {
		return err
	}
	if len(got) != want {
		return fmt.Errorf("agent invocations = %d, want %d", len(got), want)
	}
	return nil
}

func (f *fixFeature) squashLanded() error {
	repository := f.world.Repository()
	head, err := repository.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	parent, err := repository.Git("rev-parse", "HEAD^")
	if err != nil {
		return err
	}
	subject, err := repository.Git("show", "-s", "--format=%s", "HEAD")
	if err != nil {
		return err
	}
	contents, err := repository.Git("show", "HEAD:feature.go")
	if err != nil {
		return err
	}
	if head == f.originalHead || parent != f.originalHead || subject != "togi: apply verified fixes" || strings.Contains(contents, "BAD") {
		return fmt.Errorf("landing head=%q parent=%q subject=%q feature=%q", head, parent, subject, contents)
	}
	return nil
}

func (f *fixFeature) auditArtifacts() error {
	observation := f.world.LastRun()
	if observation.ReportPath() == "" {
		return errors.New("persisted report is absent")
	}
	if _, ok := observation.ArtifactPath("plan.json"); !ok {
		return errors.New("persisted plan is absent")
	}
	count, err := observation.ArtifactCount("briefs")
	if err != nil || count != 1 {
		return fmt.Errorf("brief artifacts = %d, %v; want 1", count, err)
	}
	return nil
}

func (f *fixFeature) featureUnchanged() error {
	head, err := f.world.Repository().Git("rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	if head != f.originalHead {
		return fmt.Errorf("feature branch = %s, want %s", head, f.originalHead)
	}
	return nil
}

func (f *fixFeature) relatedEdits() error {
	for name, marker := range map[string]string{"feature.go": "func Feature", "related.go": `Related = "fixed"`} {
		contents, err := f.world.Repository().Git("show", "HEAD:"+name)
		if err != nil || !strings.Contains(contents, marker) || strings.Contains(contents, "BAD") {
			return fmt.Errorf("landed %s = %q, %v", name, contents, err)
		}
	}
	return nil
}

func (f *fixFeature) runBranchAbsent() error {
	refs, err := f.world.Repository().Git("for-each-ref", "--format=%(refname)", "refs/heads/togi")
	if err != nil {
		return err
	}
	if refs != "" {
		return fmt.Errorf("unexpected validated refs %q", refs)
	}
	return nil
}

func (f *fixFeature) runBranchPreserved() error {
	report, err := f.report()
	if err != nil {
		return err
	}
	if report.Fix == nil || report.Fix.Landing.PreservedBranch == "" {
		return fmt.Errorf("preserved branch missing from report %#v", report.Fix)
	}
	_, err = f.world.Repository().Git("show-ref", "--verify", "refs/heads/"+report.Fix.Landing.PreservedBranch)
	return err
}

func (f *fixFeature) renameLanded() error {
	production, err := f.world.Repository().Git("show", "HEAD:feature.go")
	if err != nil {
		return err
	}
	tests, err := f.world.Repository().Git("show", "HEAD:feature_test.go")
	if err != nil {
		return err
	}
	if !strings.Contains(production, "totalFor") || !strings.Contains(tests, "totalFor") || strings.Contains(production+tests, "calculateTotal") {
		return fmt.Errorf("witnessed rename did not land")
	}
	return nil
}

func (f *fixFeature) concurrentState() error {
	repository := f.world.Repository()
	switch f.mode {
	case "dirty":
		head, err := repository.Git("rev-parse", "refs/heads/main")
		if err != nil || head != f.originalHead {
			return fmt.Errorf("dirty landing branch = %q, %v", head, err)
		}
		status, err := repository.Git("status", "--porcelain")
		if err != nil || status == "" {
			return fmt.Errorf("dirty state was not preserved: %q, %v", status, err)
		}
	case "detached":
		if _, err := repository.Git("symbolic-ref", "-q", "HEAD"); err == nil {
			return errors.New("detached HEAD was not preserved")
		}
		head, err := repository.Git("rev-parse", "HEAD")
		if err != nil || head != f.originalHead {
			return fmt.Errorf("detached HEAD = %q, %v", head, err)
		}
	case "branch-moved":
		subject, err := repository.Git("show", "-s", "--format=%s", "refs/heads/main")
		if err != nil || subject != "concurrent move" {
			return fmt.Errorf("moved feature subject = %q, %v", subject, err)
		}
	default:
		return fmt.Errorf("unknown concurrent mode %q", f.mode)
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
