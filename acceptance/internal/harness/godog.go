package harness

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

type failureReporter interface {
	Errorf(string, ...interface{})
}

// FeatureOptions returns the deterministic, strict options used by every
// acceptance feature. It rejects a feature path that would execute nothing.
func FeatureOptions(t *testing.T, factory DriverFactory, path string) *godog.Options {
	t.Helper()
	options := buildFeatureOptions(factory, runtime.GOOS, path)
	count, err := eligibleFeatureCount(options)
	if err != nil {
		t.Fatalf("parse acceptance feature %q: %v", path, err)
	}
	if count == 0 {
		t.Fatalf("acceptance feature %q has no eligible scenarios", path)
	}
	options.TestingT = t
	return options
}

func buildFeatureOptions(factory DriverFactory, goos, path string) *godog.Options {
	return &godog.Options{
		Format:      "pretty",
		Paths:       []string{path},
		Strict:      true,
		Concurrency: 1,
		Randomize:   0,
		Tags:        combineTags(factory.CapabilityTags(), hostTags(goos)),
	}
}

func combineTags(expressions ...string) string {
	var nonEmpty []string
	for _, expression := range expressions {
		if expression != "" {
			nonEmpty = append(nonEmpty, expression)
		}
	}
	return strings.Join(nonEmpty, " and ")
}

func hostTags(goos string) string {
	if goos == "linux" {
		return "~@unsupported-host"
	}
	return "~@linux"
}

func eligibleFeatureCount(options *godog.Options) (int, error) {
	features, err := (godog.TestSuite{Options: options}).RetrieveFeatures()
	if err != nil {
		return 0, err
	}
	if len(features) == 0 {
		return 0, nil
	}
	if len(features) != 1 {
		return 0, fmt.Errorf("parsed %d features, want exactly 1", len(features))
	}
	return len(features[0].Pickles), nil
}

// RequireGodogSuccess reports a failed strict suite through the owning test.
func RequireGodogSuccess(t failureReporter, status int) {
	if status != 0 {
		t.Errorf("acceptance suite failed with status %d", status)
	}
}

// ForEachSelectedDriver runs a test body once per process-selected driver.
func ForEachSelectedDriver(t *testing.T, body func(*testing.T, DriverFactory)) {
	t.Helper()
	for _, factory := range selectedFactories() {
		factory := factory
		t.Run(factory.Name(), func(t *testing.T) { body(t, factory) })
	}
}

// RequireLinux skips runtime domains that intentionally only specify Linux.
func RequireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Phase 1/2 runtime acceptance specifications require Linux")
	}
}
