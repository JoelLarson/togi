package runledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

type runHistoryFeature struct {
	world           *harness.World
	base            string
	beforeTree      []string
	beforeStatus    string
	linked          *harness.Repository
	linkedRun       harness.RunObservation
	firstClone      *harness.Repository
	secondClone     *harness.Repository
	firstEmpty      *harness.Repository
	secondEmpty     *harness.Repository
	firstCloneRun   harness.RunObservation
	firstEmptyRun   harness.RunObservation
	firstDone       chan error
	releasePath     string
	firstRun        harness.RunObservation
	secondRun       harness.RunObservation
	completedOutput string
}

func newRunHistoryFeature(factory harness.DriverFactory) *runHistoryFeature {
	return &runHistoryFeature{world: harness.NewWorld(factory, harness.NeedsGauntlet|harness.NeedsHistory)}
}

func (f *runHistoryFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.after)
	sc.Step(`^I run the gauntlet$`, f.runGauntlet)
	sc.Step(`^I inspect repository status$`, f.inspectStatus)
	sc.Step(`^a committed Go repository with one gate finding$`, f.oneGateFinding)
	sc.Step(`^report.json and both raw gate streams are persisted under XDG state$`, f.persistedOutsideRepository)
	sc.Step(`^the target repository tree and status are unchanged$`, f.targetUnchanged)
	sc.Step(`^a repository with primary and linked feature worktrees$`, f.primaryAndLinked)
	sc.Step(`^a completed run in the linked worktree$`, f.completedLinkedRun)
	sc.Step(`^I inspect status from the primary worktree$`, f.statusPrimary)
	sc.Step(`^status renders the linked worktree run$`, f.rendersLinkedRun)
	sc.Step(`^two shallow clones of the same remote using different URL forms$`, f.shallowClones)
	sc.Step(`^a completed run in the first clone$`, f.completedFirstCloneRun)
	sc.Step(`^I inspect status from the second clone$`, f.statusSecondClone)
	sc.Step(`^status renders the first clone's run$`, f.rendersFirstCloneRun)
	sc.Step(`^both clones persist under the same repository state directory$`, f.sharedCloneState)
	sc.Step(`^two empty repositories with no remotes$`, f.emptyRepositories)
	sc.Step(`^the repositories persist under different state directories$`, f.separateEmptyState)
	sc.Step(`^a committed Go repository with a gate paused after startup$`, f.pausedGate)
	sc.Step(`^I start another gauntlet run for the repository$`, f.startSecondRun)
	sc.Step(`^the second run is rejected as locked$`, f.secondLocked)
	sc.Step(`^the first run can complete after the gate resumes$`, f.firstCompletes)
	sc.Step(`^a committed Go repository with an abandoned unlocked lock file$`, f.abandonedLock)
	sc.Step(`^a completed report is persisted$`, f.reportPersisted)
	sc.Step(`^a committed Go repository with 20 completed runs$`, f.twentyCompletedRuns)
	sc.Step(`^I complete one more run$`, f.completeOneMoreRun)
	sc.Step(`^only the newest 20 run directories remain$`, f.twentyNewestRemain)
	sc.Step(`^a committed Go repository with two completed runs$`, f.twoCompletedRuns)
	sc.Step(`^status renders the newer completed run$`, f.rendersNewerRun)
	sc.Step(`^a committed Go repository with one completed run$`, f.oneCompletedRun)
	sc.Step(`^newer incomplete and corrupt run directories$`, f.newerInvalidRuns)
	sc.Step(`^status renders the completed run$`, f.rendersCompletedRun)
}

func (f *runHistoryFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.base, f.beforeStatus, f.completedOutput = "base", "", ""
	f.beforeTree, f.linked, f.firstClone, f.secondClone, f.firstEmpty, f.secondEmpty = nil, nil, nil, nil, nil, nil
	f.firstDone, f.releasePath = nil, ""
	f.firstRun, f.secondRun, f.linkedRun, f.firstCloneRun, f.firstEmptyRun = harness.RunObservation{}, harness.RunObservation{}, harness.RunObservation{}, harness.RunObservation{}, harness.RunObservation{}
	return f.world.Before(ctx, scenario)
}

func (f *runHistoryFeature) after(ctx context.Context, scenario *godog.Scenario, scenarioErr error) (context.Context, error) {
	if f.releasePath != "" {
		if err := os.Remove(f.releasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			scenarioErr = errors.Join(scenarioErr, err)
		}
	}
	if f.firstDone != nil {
		select {
		case err := <-f.firstDone:
			scenarioErr = errors.Join(scenarioErr, err)
		case <-time.After(5 * time.Second):
			scenarioErr = errors.Join(scenarioErr, errors.New("timed out waiting for paused gate"))
		}
	}
	return f.world.After(ctx, scenario, scenarioErr)
}

func (f *runHistoryFeature) repository() (*harness.Repository, error) {
	repository, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repository"))
	if err != nil {
		return nil, err
	}
	for name, body := range map[string]string{
		"go.mod":     "module fixture\n\ngo 1.25\n",
		"feature.go": "package fixture\n\nfunc Feature() { value := 1; _ = value }\n",
	} {
		if err := repository.Write(name, body); err != nil {
			return nil, err
		}
	}
	if _, err := repository.Commit("base"); err != nil {
		return nil, err
	}
	if err := repository.Branch(f.base); err != nil {
		return nil, err
	}
	if err := f.world.UseRepository(repository); err != nil {
		return nil, err
	}
	return repository, nil
}

func (f *runHistoryFeature) installGate(behavior harness.ToolBehavior) error {
	if _, err := f.world.Environment().InstallTool("history-gate", behavior); err != nil {
		return err
	}
	return f.world.Environment().WriteGate(harness.GateDefinition{
		Name: "history", Description: "history fixture", Tool: "history-gate", Scope: "repo", Location: "point",
		Command: []string{"history-gate"}, SeverityMap: map[string]string{"default": "warning", "warning": "warning"},
	})
}

func (f *runHistoryFeature) oneGateFinding() error {
	repository, err := f.repository()
	if err != nil {
		return err
	}
	f.beforeTree, err = repository.Tree()
	if err != nil {
		return err
	}
	f.beforeStatus, err = repository.Git("status", "--porcelain=v1", "-z")
	if err != nil {
		return err
	}
	return f.installGate(harness.ToolBehavior{Stdout: lint("feature.go", 3), Stderr: []byte("HISTORY RAW STDERR\n")})
}

func (f *runHistoryFeature) persistedOutsideRepository(ctx context.Context) error {
	observation := f.world.LastRun()
	report, err := observation.Report()
	if err != nil {
		return err
	}
	state, err := f.world.Environment().RepoState(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	prefix := filepath.Join(state, "runs", report.RunID)
	if !within(prefix, observation.ReportPath()) || filepath.Base(observation.ReportPath()) != "report.json" {
		return fmt.Errorf("report path %q is not under %q", observation.ReportPath(), prefix)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		path, ok := observation.RawPath("history", "go", stream)
		if !ok || !within(filepath.Join(prefix, "raw"), path) {
			return fmt.Errorf("%s raw path = %q, ok=%t", stream, path, ok)
		}
	}
	return nil
}

func (f *runHistoryFeature) targetUnchanged() error {
	tree, err := f.world.Repository().Tree()
	if err != nil {
		return err
	}
	status, err := f.world.Repository().Git("status", "--porcelain=v1", "-z")
	if err != nil {
		return err
	}
	if strings.Join(tree, "\x00") != strings.Join(f.beforeTree, "\x00") || status != f.beforeStatus {
		return fmt.Errorf("target changed: tree=%v status=%q", tree, status)
	}
	return nil
}

func (f *runHistoryFeature) primaryAndLinked() error {
	primary, err := f.repository()
	if err != nil {
		return err
	}
	linked, err := primary.LinkedWorktree(filepath.Join(f.world.Environment().TempRoot, "linked"), "feature")
	if err != nil {
		return err
	}
	if err := linked.Write("feature.go", "package fixture\n\nfunc Feature() { value := 2; _ = value }\n"); err != nil {
		return err
	}
	if _, err := linked.Commit("feature"); err != nil {
		return err
	}
	f.linked = linked
	return f.installGate(harness.ToolBehavior{Stdout: lint("feature.go", 3)})
}

func (f *runHistoryFeature) completedLinkedRun(ctx context.Context) error {
	driver, err := f.world.Gauntlet()
	if err != nil {
		return err
	}
	f.linkedRun, err = driver.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.linked.Root, Base: f.base, NoColor: true}))
	return err
}

func (f *runHistoryFeature) runGauntlet(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: "base", NoColor: true}))
}

func (f *runHistoryFeature) inspectStatus(ctx context.Context) error {
	return f.world.Status(ctx, harness.StatusRequest{Root: f.world.Repository().Root, NoColor: true})
}

func (f *runHistoryFeature) statusPrimary(ctx context.Context) error {
	return f.inspectStatus(ctx)
}
func (f *runHistoryFeature) rendersLinkedRun() error {
	return sameRender(f.world.LastCommand(), f.linkedRun)
}

func (f *runHistoryFeature) shallowClones() error {
	source, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "origin.git"))
	if err != nil {
		return err
	}
	for name, body := range map[string]string{
		"go.mod":     "module fixture\n\ngo 1.25\n",
		"feature.go": "package fixture\n\nfunc Feature() { value := 1; _ = value }\n",
	} {
		if err := source.Write(name, body); err != nil {
			return err
		}
	}
	if _, err := source.Commit("base"); err != nil {
		return err
	}
	if err := source.Write("feature.go", "package fixture\n\nfunc Feature() { value := 2; _ = value }\n"); err != nil {
		return err
	}
	if _, err := source.Commit("feature"); err != nil {
		return err
	}
	firstRoot := filepath.Join(f.world.Environment().TempRoot, "clone-one")
	secondRoot := filepath.Join(f.world.Environment().TempRoot, "clone-two")
	if _, err := source.Git("clone", "--depth", "1", "file://"+source.Root, firstRoot); err != nil {
		return err
	}
	if _, err := source.Git("clone", "--depth", "1", "file://"+source.Root+"/", secondRoot); err != nil {
		return err
	}
	f.firstClone = &harness.Repository{Root: firstRoot}
	f.secondClone = &harness.Repository{Root: secondRoot}
	if err := f.world.UseRepository(f.firstClone); err != nil {
		return err
	}
	return f.installGate(harness.ToolBehavior{Stdout: lint("feature.go", 3)})
}

func (f *runHistoryFeature) completedFirstCloneRun(ctx context.Context) error {
	driver, err := f.world.Gauntlet()
	if err != nil {
		return err
	}
	f.firstCloneRun, err = driver.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.firstClone.Root, NoColor: true}))
	return err
}

func (f *runHistoryFeature) statusSecondClone(ctx context.Context) error {
	return f.world.Status(ctx, harness.StatusRequest{Root: f.secondClone.Root, NoColor: true})
}

func (f *runHistoryFeature) rendersFirstCloneRun() error {
	return sameRender(f.world.LastCommand(), f.firstCloneRun)
}

func (f *runHistoryFeature) sharedCloneState(ctx context.Context) error {
	first, err := f.world.Environment().RepoState(ctx, f.firstClone.Root)
	if err != nil {
		return err
	}
	second, err := f.world.Environment().RepoState(ctx, f.secondClone.Root)
	if err != nil {
		return err
	}
	if first != second {
		return fmt.Errorf("clone state directories = %q and %q", first, second)
	}
	return nil
}

func (f *runHistoryFeature) emptyRepositories() error {
	first, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "empty-one"))
	if err != nil {
		return err
	}
	second, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "empty-two"))
	if err != nil {
		return err
	}
	f.firstEmpty, f.secondEmpty = first, second
	return f.world.UseRepository(first)
}

func (f *runHistoryFeature) separateEmptyState(ctx context.Context) error {
	first, err := f.world.Environment().RepoState(ctx, f.firstEmpty.Root)
	if err != nil {
		return err
	}
	second, err := f.world.Environment().RepoState(ctx, f.secondEmpty.Root)
	if err != nil {
		return err
	}
	if first == second {
		return fmt.Errorf("empty repositories shared state directory %q", first)
	}
	return nil
}

func (f *runHistoryFeature) pausedGate() error {
	if _, err := f.repository(); err != nil {
		return err
	}
	f.releasePath = filepath.Join(f.world.Environment().TempRoot, "release")
	started := filepath.Join(f.world.Environment().TempRoot, "started")
	if err := os.WriteFile(f.releasePath, []byte("wait"), 0o600); err != nil {
		return err
	}
	if err := f.installGate(harness.ToolBehavior{Stdout: lint("feature.go", 3), WaitFor: f.releasePath, StartedMarker: started}); err != nil {
		return err
	}
	driver, err := f.world.Gauntlet()
	if err != nil {
		return err
	}
	f.firstDone = make(chan error, 1)
	go func() {
		observation, runErr := driver.Run(context.Background(), harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true}))
		f.firstRun = observation
		f.firstDone <- runErr
	}()
	return waitForPath(started)
}

func (f *runHistoryFeature) startSecondRun(ctx context.Context) error {
	driver, err := f.world.Gauntlet()
	if err != nil {
		return err
	}
	f.secondRun, err = driver.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true}))
	return err
}

func (f *runHistoryFeature) secondLocked() error {
	outcome, err := f.secondRun.Outcome()
	if err != nil {
		return err
	}
	diagnostic := outcome.Message + f.secondRun.Stdout() + f.secondRun.Stderr()
	if !strings.Contains(strings.ToLower(diagnostic), "locked") {
		return fmt.Errorf("second outcome = %#v, want locked diagnostic", outcome)
	}
	state, err := f.world.Environment().RepoState(context.Background(), f.world.Repository().Root)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(state, "runs"))
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return fmt.Errorf("run directories = %d, want 1", len(entries))
	}
	return nil
}

func (f *runHistoryFeature) firstCompletes() error {
	if err := os.Remove(f.releasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f.releasePath = ""
	select {
	case err := <-f.firstDone:
		f.firstDone = nil
		if err != nil {
			return err
		}
		_, reportErr := f.firstRun.Report()
		return reportErr
	case <-time.After(5 * time.Second):
		return errors.New("timed out waiting for first run")
	}
}

func (f *runHistoryFeature) abandonedLock(ctx context.Context) error {
	if _, err := f.repository(); err != nil {
		return err
	}
	if err := f.installGate(harness.ToolBehavior{Stdout: lint("feature.go", 3)}); err != nil {
		return err
	}
	state, err := f.world.Environment().RepoState(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(state, "lock"), []byte(`{"pid":1,"start":"2000-01-01T00:00:00Z","token":"old"}`), 0o600)
}

func (f *runHistoryFeature) reportPersisted() error {
	if _, err := f.world.LastRun().Report(); err != nil {
		return err
	}
	if f.world.LastRun().ReportPath() == "" {
		return errors.New("completed run has no persisted report path")
	}
	return nil
}

func (f *runHistoryFeature) twentyCompletedRuns(ctx context.Context) error {
	if err := f.oneGateFinding(); err != nil {
		return err
	}
	for range 20 {
		if err := f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true})); err != nil {
			return err
		}
	}
	return nil
}
func (f *runHistoryFeature) completeOneMoreRun(ctx context.Context) error {
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true}))
}

func (f *runHistoryFeature) twentyNewestRemain(ctx context.Context) error {
	state, err := f.world.Environment().RepoState(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(state, "runs"))
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) != 20 {
		return fmt.Errorf("run directories = %d, want 20: %v", len(names), names)
	}
	report, err := f.world.LastRun().Report()
	if err != nil {
		return err
	}
	if names[len(names)-1] != report.RunID {
		return fmt.Errorf("newest run = %q, want %q", names[len(names)-1], report.RunID)
	}
	return nil
}

func (f *runHistoryFeature) twoCompletedRuns(ctx context.Context) error {
	if err := f.oneGateFinding(); err != nil {
		return err
	}
	if err := f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true})); err != nil {
		return err
	}
	return f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true}))
}
func (f *runHistoryFeature) rendersNewerRun() error {
	return sameRender(f.world.LastCommand(), f.world.LastRun())
}

func (f *runHistoryFeature) oneCompletedRun(ctx context.Context) error {
	if err := f.oneGateFinding(); err != nil {
		return err
	}
	if err := f.world.Run(ctx, harness.ReportOnly(harness.RunRequest{Root: f.world.Repository().Root, Base: f.base, NoColor: true})); err != nil {
		return err
	}
	f.completedOutput = f.world.LastRun().Stdout()
	return nil
}

func (f *runHistoryFeature) newerInvalidRuns(ctx context.Context) error {
	state, err := f.world.Environment().RepoState(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	runs := filepath.Join(state, "runs")
	for name, report := range map[string][]byte{
		"99991231T235958.000000000Z-fffe": nil,
		"99991231T235959.000000000Z-ffff": []byte("{not JSON"),
	} {
		directory := filepath.Join(runs, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			return err
		}
		if report != nil {
			if err := os.WriteFile(filepath.Join(directory, "report.json"), report, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}
func (f *runHistoryFeature) rendersCompletedRun() error {
	if f.world.LastCommand().Stdout() != f.completedOutput {
		return fmt.Errorf("status output = %q, want %q", f.world.LastCommand().Stdout(), f.completedOutput)
	}
	return nil
}

func sameRender(command harness.CommandObservation, run harness.RunObservation) error {
	if command.Stdout() != run.Stdout() {
		return fmt.Errorf("status output = %q, want %q", command.Stdout(), run.Stdout())
	}
	return nil
}
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func waitForPath(path string) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %q", path)
		case <-ticker.C:
		}
	}
}
func lint(file string, lines ...int) []byte {
	issues := make([]string, 0, len(lines))
	for _, line := range lines {
		issues = append(issues, fmt.Sprintf(`{"FromLinter":"history","Text":"history finding","Severity":"warning","Pos":{"Filename":%q,"Line":%d}}`, file, line))
	}
	return []byte(`{"Issues":[` + strings.Join(issues, ",") + `]}`)
}
