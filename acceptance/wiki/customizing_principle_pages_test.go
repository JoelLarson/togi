package wiki

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/acceptance/internal/harness"
)

func TestCustomizingPrinciplePages(t *testing.T) {
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "customizing_principle_pages.feature")
		status := godog.TestSuite{
			Name:                "customizing principle pages",
			Options:             options,
			ScenarioInitializer: newPrinciplePagesFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
