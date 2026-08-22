package gauntlet

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/acceptance/internal/harness"
)

func TestRunningTheGauntlet(t *testing.T) {
	harness.RequireLinux(t)
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "running_the_gauntlet.feature")
		status := godog.TestSuite{
			Name:                "running the gauntlet",
			Options:             options,
			ScenarioInitializer: newGauntletFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
