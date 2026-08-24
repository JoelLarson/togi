package gauntlet

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

func TestFixingAFeatureDiff(t *testing.T) {
	harness.RequireLinux(t)
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "fixing_a_feature_diff.feature")
		status := godog.TestSuite{
			Name: "fixing a feature diff", Options: options,
			ScenarioInitializer: newFixFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
