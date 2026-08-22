package gauntlet

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/acceptance/internal/harness"
)

func TestJudgingAFeatureDiff(t *testing.T) {
	harness.RequireLinux(t)
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "judging_a_feature_diff.feature")
		status := godog.TestSuite{
			Name: "judging a feature diff", Options: options,
			ScenarioInitializer: newDiffFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
