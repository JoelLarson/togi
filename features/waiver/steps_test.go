package waiver

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
	internalwaiver "github.com/joellarson/togi/internal/waiver"
)

// reportedFingerprint stands in for an identity a blocked report named. This
// specification covers recording an approval; matching one to a live finding
// belongs to the run that honors it.
const reportedFingerprint = "0f2ac1e1b8f7c8b56b6da5e0f9dc0f6e6c1a2b3c4d5e6f708192a3b4c5d6e7f8"

type approvalFeature struct {
	world       *harness.World
	second      *harness.Repository
	beforeFiles []string
	firstReason string
	lastReason  string
}

func newApprovalFeature(factory harness.DriverFactory) *approvalFeature {
	return &approvalFeature{world: harness.NewWorld(factory, harness.NeedsWaiver)}
}

func (f *approvalFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.world.After)
	sc.Step(`^a repository and a reported finding fingerprint$`, f.repository)
	sc.Step(`^that fingerprint was already waived for "([^"]*)"$`, f.alreadyWaived)
	sc.Step(`^a second checkout of that repository$`, f.secondCheckout)
	sc.Step(`^I waive that fingerprint for "([^"]*)"$`, f.waive)
	sc.Step(`^I waive that fingerprint for no stated reason$`, f.waiveWithoutReason)
	sc.Step(`^I waive that fingerprint from the second checkout for "([^"]*)"$`, f.waiveFromSecondCheckout)
	sc.Step(`^the waiver is recorded with that reason and the time it was approved$`, f.recordedWithReasonAndTime)
	sc.Step(`^the approval is refused and no waiver is recorded$`, f.refused)
	sc.Step(`^one waiver is recorded, keeping the first reason$`, f.keepsFirstReason)
	sc.Step(`^the waiver is stored outside the repository, whose files are unchanged$`, f.storedOutsideRepository)
	sc.Step(`^the waiver is visible from the first checkout$`, f.visibleFromFirstCheckout)
}

func (f *approvalFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.second, f.beforeFiles = nil, nil
	f.firstReason, f.lastReason = "", ""
	return f.world.Before(ctx, scenario)
}

func (f *approvalFeature) repository() error {
	repository, err := harness.NewRepository(filepath.Join(f.world.Environment().TempRoot, "repo"))
	if err != nil {
		return err
	}
	if err := repository.Write("feature.go", "package fixture\n\nfunc Feature() {}\n"); err != nil {
		return err
	}
	if _, err := repository.Commit("feature"); err != nil {
		return err
	}
	if err := f.world.UseRepository(repository); err != nil {
		return err
	}
	f.beforeFiles, err = workingFiles(repository.Root)
	return err
}

func (f *approvalFeature) alreadyWaived(ctx context.Context, reason string) error {
	if err := f.waive(ctx, reason); err != nil {
		return err
	}
	f.firstReason = reason
	outcome, err := f.world.LastCommand().Outcome()
	if err != nil {
		return err
	}
	if outcome.Code != 0 {
		return fmt.Errorf("preparing an existing waiver failed with outcome %#v", outcome)
	}
	return nil
}

func (f *approvalFeature) secondCheckout() error {
	second, err := f.world.Repository().LinkedWorktree(filepath.Join(f.world.Environment().TempRoot, "second"), "review")
	if err != nil {
		return err
	}
	f.second = second
	return nil
}

func (f *approvalFeature) waive(ctx context.Context, reason string) error {
	f.lastReason = reason
	return f.world.Waive(ctx, harness.WaiveRequest{
		Root:        f.world.Repository().Root,
		Fingerprint: reportedFingerprint,
		Reason:      reason,
	})
}

func (f *approvalFeature) waiveWithoutReason(ctx context.Context) error {
	return f.waive(ctx, "")
}

func (f *approvalFeature) waiveFromSecondCheckout(ctx context.Context, reason string) error {
	if f.second == nil {
		return fmt.Errorf("no second checkout was created")
	}
	f.lastReason = reason
	return f.world.Waive(ctx, harness.WaiveRequest{
		Root:        f.second.Root,
		Fingerprint: reportedFingerprint,
		Reason:      reason,
	})
}

func (f *approvalFeature) recordedWithReasonAndTime(ctx context.Context) error {
	records, err := f.recorded(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("recorded waivers = %#v, want exactly one", records)
	}
	record := records[0]
	if record.Fingerprint != reportedFingerprint {
		return fmt.Errorf("recorded fingerprint = %q, want the reported one", record.Fingerprint)
	}
	if record.Reason != f.lastReason {
		return fmt.Errorf("recorded reason = %q, want %q", record.Reason, f.lastReason)
	}
	if record.ApprovedAt.IsZero() {
		return fmt.Errorf("recorded waiver has no approval time")
	}
	if !strings.Contains(f.world.LastCommand().Stdout(), record.Reason) {
		return fmt.Errorf("the approval was not confirmed: %q", f.world.LastCommand().Stdout())
	}
	return nil
}

func (f *approvalFeature) refused(ctx context.Context) error {
	outcome, err := f.world.LastCommand().Outcome()
	if err != nil {
		return err
	}
	if outcome.Code == 0 {
		return fmt.Errorf("an unexplained approval succeeded")
	}
	records, err := f.recorded(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return fmt.Errorf("a refused approval recorded %#v", records)
	}
	return nil
}

func (f *approvalFeature) keepsFirstReason(ctx context.Context) error {
	records, err := f.recorded(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("recorded waivers = %#v, want exactly one", records)
	}
	if records[0].Reason != f.firstReason {
		return fmt.Errorf("recorded reason = %q, want the first judgement %q", records[0].Reason, f.firstReason)
	}
	return nil
}

func (f *approvalFeature) storedOutsideRepository(ctx context.Context) error {
	root := f.world.Repository().Root
	records, err := f.recorded(ctx, root)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("recorded waivers = %#v, want exactly one", records)
	}
	state, err := f.world.Environment().RepoState(ctx, root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, state)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(relative, "..") {
		return fmt.Errorf("waiver state %q is inside the repository", state)
	}
	after, err := workingFiles(root)
	if err != nil {
		return err
	}
	if strings.Join(after, "\n") != strings.Join(f.beforeFiles, "\n") {
		return fmt.Errorf("the repository changed: %v, want %v", after, f.beforeFiles)
	}
	return nil
}

func (f *approvalFeature) visibleFromFirstCheckout(ctx context.Context) error {
	records, err := f.recorded(ctx, f.world.Repository().Root)
	if err != nil {
		return err
	}
	if len(records) != 1 || records[0].Reason != f.lastReason {
		return fmt.Errorf("waivers seen from the first checkout = %#v, want the reason %q", records, f.lastReason)
	}
	return nil
}

func (f *approvalFeature) recorded(ctx context.Context, root string) ([]internalwaiver.Record, error) {
	state, err := f.world.Environment().RepoState(ctx, root)
	if err != nil {
		return nil, err
	}
	return internalwaiver.Store{Dir: state}.Load()
}

// workingFiles lists the repository's own files, excluding Git's own state.
func workingFiles(root string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if relative == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if relative == ".git" {
			return nil
		}
		names = append(names, relative)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list repository files: %w", err)
	}
	return names, nil
}
