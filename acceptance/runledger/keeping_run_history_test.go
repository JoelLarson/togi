package runledger

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/acceptance/internal/harness"
)

func TestKeepingRunHistory(t *testing.T) {
	harness.RequireLinux(t)
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "keeping_run_history.feature")
		status := godog.TestSuite{
			Name:                "keeping run history",
			Options:             options,
			ScenarioInitializer: newRunHistoryFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
