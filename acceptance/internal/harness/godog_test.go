package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/cucumber/godog"
)

func TestFeatureOptions(t *testing.T) {
	path := filepath.Join("testdata", "undefined.feature")
	for _, test := range []struct {
		name    string
		factory DriverFactory
		goos    string
		want    string
	}{
		{"service linux", newServiceFactory(), "linux", "~@unsupported-host"},
		{"cli linux", newCLIFactory("togi"), "linux", "~@simulated-platform && ~@unsupported-host"},
		{"service non-linux", newServiceFactory(), "darwin", "~@linux"},
		{"cli non-linux", newCLIFactory("togi"), "darwin", "~@simulated-platform && ~@linux"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := buildFeatureOptions(test.factory, test.goos, path)
			if options.Format != "pretty" || !options.Strict || options.Concurrency != 1 || options.Randomize != 0 {
				t.Fatalf("suite options = %#v, want deterministic strict options", options)
			}
			if len(options.Paths) != 1 || options.Paths[0] != path {
				t.Fatalf("Paths = %v, want [%q]", options.Paths, path)
			}
			if options.Tags != test.want {
				t.Fatalf("Tags = %q, want %q", options.Tags, test.want)
			}
			if options.Concurrency > 1 {
				t.Fatalf("Concurrency = %d, want at most one", options.Concurrency)
			}
		})
	}
}

func TestDriverTags(t *testing.T) {
	if got := newServiceFactory().CapabilityTags(); got != "" {
		t.Fatalf("service capability tags = %q, want empty", got)
	}
	if got := newCLIFactory("togi").CapabilityTags(); got != "~@simulated-platform" {
		t.Fatalf("CLI capability tags = %q", got)
	}
	if got := hostTags("linux"); got != "~@unsupported-host" {
		t.Fatalf("linux host tags = %q", got)
	}
	if got := hostTags("darwin"); got != "~@linux" {
		t.Fatalf("non-linux host tags = %q", got)
	}
}

func TestFeatureOptionsRejectEmptySelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filtered.feature")
	if err := os.WriteFile(path, []byte("@unsupported-host\nFeature: filtered\n  Scenario: filtered\n    Given a step\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := buildFeatureOptions(newServiceFactory(), "linux", path)
	count, err := eligibleFeatureCount(options)
	if err != nil {
		t.Fatalf("eligibleFeatureCount(): %v", err)
	}
	if count != 0 {
		t.Fatalf("eligibleFeatureCount() = %d, want 0", count)
	}
}

func TestFeatureOptionsAddsTestingTAfterValidation(t *testing.T) {
	options := FeatureOptions(t, newServiceFactory(), filepath.Join("testdata", "undefined.feature"))
	if options.TestingT != t {
		t.Fatal("FeatureOptions() did not attach the owning test")
	}
}

func TestFeatureOptionsFatalForEmptySelection(t *testing.T) {
	if os.Getenv("TOGI_TEST_FEATURE_OPTIONS_FATAL") == "1" {
		FeatureOptions(t, newServiceFactory(), os.Getenv("TOGI_TEST_FEATURE_PATH"))
		return
	}
	path := filepath.Join(t.TempDir(), "filtered.feature")
	if err := os.WriteFile(path, []byte("@unsupported-host\nFeature: filtered\n  Scenario: filtered\n    Given a step\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := helperTestCommand(t, "TestFeatureOptionsFatalForEmptySelection")
	command.Env = append(os.Environ(), "TOGI_TEST_FEATURE_OPTIONS_FATAL=1", "TOGI_TEST_FEATURE_PATH="+path)
	if err := command.Run(); err == nil {
		t.Fatal("FeatureOptions() subprocess succeeded, want fatal failure")
	}
}

func TestStrictSuitesFail(t *testing.T) {
	for _, test := range []struct {
		name        string
		feature     string
		initializer func(*godog.ScenarioContext)
	}{
		{"undefined", "undefined.feature", func(*godog.ScenarioContext) {}},
		{"pending", "pending.feature", func(context *godog.ScenarioContext) {
			context.Step(`a pending step`, func() error { return godog.ErrPending })
		}},
		{"ambiguous", "ambiguous.feature", func(context *godog.ScenarioContext) {
			context.Step(`an ambiguous step`, func() error { return nil })
			context.Step(`an ambiguous (.*)`, func(string) error { return nil })
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := buildFeatureOptions(newServiceFactory(), runtime.GOOS, filepath.Join("testdata", test.feature))
			if status := (godog.TestSuite{Name: test.name, Options: options, ScenarioInitializer: test.initializer}).Run(); status == 0 {
				t.Fatal("strict suite passed, want failure")
			}
		})
	}
}

type fakeTestingT struct{ failed bool }

func (t *fakeTestingT) Errorf(string, ...interface{}) { t.failed = true }

func TestRequireGodogSuccess(t *testing.T) {
	fake := &fakeTestingT{}
	RequireGodogSuccess(fake, 1)
	if !fake.failed {
		t.Fatal("RequireGodogSuccess() did not report a failure")
	}
}

func TestForEachSelectedDriverUsesSelectionOrder(t *testing.T) {
	original := selectedFactories()
	t.Cleanup(func() { setSelectedFactories(original) })
	setSelectedFactories([]DriverFactory{newServiceFactory(), newCLIFactory("togi")})

	var names []string
	ForEachSelectedDriver(t, func(_ *testing.T, factory DriverFactory) {
		names = append(names, factory.Name())
	})
	if !slices.Equal(names, []string{"service", "cli"}) {
		t.Fatalf("selected driver order = %v, want [service cli]", names)
	}
}

func helperTestCommand(t *testing.T, name string) *exec.Cmd {
	t.Helper()
	return exec.Command(os.Args[0], "-test.run=^"+name+"$")
}
