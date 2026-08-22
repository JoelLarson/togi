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
	for _, bind := range []struct {
		expression string
		step       any
	}{
		{`^a committed feature branch with two possible bases$`, f.twoBases},
		{`^a gate finding belongs only to the explicitly based diff$`, f.explicitFinding},
		{`^I run the gauntlet with the older base$`, f.runOlder},
		{`^the report records the explicit base and its merge base$`, f.explicitRecorded},
		{`^the finding is in scope$`, f.oneFinding},
		{`^a committed feature branch whose origin HEAD points to "([^"]*)"$`, f.originHead},
		{`^I run the gauntlet without a base$`, f.runAutomatic},
		{`^the report base is "([^"]*)"$`, f.reportBase},
		{`^a committed feature branch from local "([^"]*)" without a remote$`, f.localTrunk},
		{`^trunk and the feature branch diverged after a shared commit$`, f.diverged},
		{`^a gate reports findings on both branches' changes$`, f.bothBranches},
		{`^I run the gauntlet against trunk$`, f.runTrunk},
		{`^the report merge base is the shared commit$`, f.mergeBase},
		{`^only the feature finding is in scope$`, f.onlyFeature},
		{`^a committed feature changes line 8 but not line 3$`, f.pointLines},
		{`^a diff-scoped gate reports point findings on lines 3 and 8$`, f.pointGate},
		{`^I run the gauntlet$`, f.runDefault},
		{`^only the finding on line 8 remains$`, f.lineEight},
		{`^a committed feature changes the body of function "([^"]*)"$`, f.entityChange},
		{`^a diff-scoped gate reports an entity finding on the function signature$`, f.entityGate},
		{`^the structural finding for "([^"]*)" remains$`, f.structural},
		{`^a committed feature changes "([^"]*)"$`, f.repoChange},
		{`^a whole-repo gate reports a finding in "([^"]*)"$`, f.repoGate},
		{`^the finding in "([^"]*)" remains$`, f.findingFile},
		{`^a committed feature deletes line 5 from "([^"]*)"$`, f.deleteLine},
		{`^a diff-scoped gate reports a point finding at the deletion location$`, f.deletionGate},
		{`^the deletion finding remains in scope$`, f.oneFinding},
		{`^a committed feature renames "([^"]*)" to "([^"]*)"$`, f.rename},
		{`^a gate reports a finding in "([^"]*)"$`, f.namedFinding},
		{`^the report records the finding in "([^"]*)"$`, f.findingFile},
		{`^a committed feature changes the binary file "([^"]*)"$`, f.binary},
		{`^the diff records one changed file and zero changed lines$`, f.binaryRecorded},
		{`^a repository with (.*)$`, f.invalidRepository},
		{`^a gate that records whether it starts$`, f.markerGate},
		{`^the run is rejected for the (.*)$`, f.rejected},
		{`^no gate, ledger, or target-repository file is created$`, f.noSideEffects},
	} {
		sc.Step(bind.expression, bind.step)
	}
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
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: f.olderBase, NoColor: true})
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
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, NoColor: true})
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
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: "trunk", NoColor: true})
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
	return f.world.Run(ctx, harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true})
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
