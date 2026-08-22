package platform

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/acceptance/internal/harness"
)

func TestSupportingPlatforms(t *testing.T) {
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "supporting_platforms.feature")
		status := godog.TestSuite{
			Name:                "supporting platforms",
			Options:             options,
			ScenarioInitializer: newPlatformFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
