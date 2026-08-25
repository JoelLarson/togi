package waiver

import (
	"testing"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
)

func TestApprovingAFinding(t *testing.T) {
	harness.RequireLinux(t)
	harness.ForEachSelectedDriver(t, func(t *testing.T, factory harness.DriverFactory) {
		options := harness.FeatureOptions(t, factory, "approving_a_finding.feature")
		status := godog.TestSuite{
			Name:                "approving a finding",
			Options:             options,
			ScenarioInitializer: newApprovalFeature(factory).initialize,
		}.Run()
		harness.RequireGodogSuccess(t, status)
	})
}
