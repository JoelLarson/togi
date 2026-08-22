package harness

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"
)

// BindReports supplies shared report actions and assertions.
func (w *World) BindReports(sc *godog.ScenarioContext) {
	sc.Step(`^I run the gauntlet$`, func(ctx context.Context) error {
		if w.repository == nil {
			return fmt.Errorf("scenario repository is required")
		}
		return w.Run(ctx, RunRequest{Root: w.repository.Root, Base: "base", NoColor: true})
	})
	sc.Step(`^the report verdict is (.*)$`, func(want string) error {
		report, err := w.lastRun.Report()
		if err != nil {
			return err
		}
		if report.Verdict != want {
			return fmt.Errorf("report verdict = %q, want %q; report: %#v", report.Verdict, want, report)
		}
		return nil
	})
	sc.Step(`^the application outcome is (\d+)$`, func(want int) error {
		outcome, err := w.lastRun.Outcome()
		if err != nil {
			return err
		}
		if outcome.Code != want {
			return fmt.Errorf("application outcome = %d, want %d", outcome.Code, want)
		}
		return nil
	})
}

// BindHistory supplies the common status action used by history specifications.
func (w *World) BindHistory(sc *godog.ScenarioContext) {
	sc.Step(`^I inspect repository status$`, func(ctx context.Context) error {
		if w.repository == nil {
			return fmt.Errorf("scenario repository is required")
		}
		return w.Status(ctx, StatusRequest{Root: w.repository.Root, NoColor: true})
	})
}
