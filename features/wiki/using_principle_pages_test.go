package wiki

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

func TestUsingPrinciplePages(t *testing.T) {
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "using_principle_pages.feature")
		status := godog.TestSuite{
			Name:                "using principle pages",
			Options:             options,
			ScenarioInitializer: newPrinciplePagesFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
