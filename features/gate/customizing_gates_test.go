package gate

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

func TestCustomizingGates(t *testing.T) {
	harness.RequireLinux(t)
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "customizing_gates.feature")
		status := godog.TestSuite{
			Name:                "customizing gates",
			Options:             options,
			ScenarioInitializer: newGateCustomizationFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
