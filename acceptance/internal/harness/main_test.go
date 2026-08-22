package harness

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMainLifecycleDoesNotBuildService(t *testing.T) {
	events := []string{}
	code := mainLifecycle("service", func() int {
		events = append(events, "run")
		return 0
	}, lifecycleDeps{
		build: func(string, string) error {
			t.Fatal("service selection built the CLI")
			return nil
		},
	})
	if code != 0 {
		t.Fatalf("mainLifecycle() = %d, want 0", code)
	}
	if !reflect.DeepEqual(events, []string{"run"}) {
		t.Fatalf("events = %v, want run only", events)
	}
}

func TestMainLifecycleSelectsRequestedFactories(t *testing.T) {
	code := mainLifecycle("all", func() int {
		factories := selectedFactories()
		if len(factories) != 2 || factories[0].Name() != "service" || factories[1].Name() != "cli" {
			t.Fatalf("selected factories = %v", driverNames(factories))
		}
		return 0
	}, lifecycleDeps{
		moduleRoot: func() (string, error) { return "/module", nil },
		makeTemp:   func(string, string) (string, error) { return "/temporary", nil },
		build:      func(string, string) error { return nil },
		removeAll:  func(string) error { return nil },
	})
	if code != 0 {
		t.Fatalf("mainLifecycle() = %d, want 0", code)
	}
}

func driverNames(factories []DriverFactory) []string {
	names := make([]string, len(factories))
	for index, factory := range factories {
		names[index] = factory.Name()
	}
	return names
}

func TestMainLifecycleBuildsCLIAndCleansTemporaryDirectory(t *testing.T) {
	events := []string{}
	code := mainLifecycle("cli", func() int {
		events = append(events, "run")
		return 0
	}, lifecycleDeps{
		moduleRoot: func() (string, error) { return "/module", nil },
		makeTemp:   func(string, string) (string, error) { return "/temporary", nil },
		build: func(root, binary string) error {
			events = append(events, "build:"+root+":"+binary)
			return nil
		},
		removeAll: func(path string) error {
			events = append(events, "remove:"+path)
			return nil
		},
	})
	if code != 0 {
		t.Fatalf("mainLifecycle() = %d, want 0", code)
	}
	want := []string{"build:/module:" + filepath.Join("/temporary", "togi"), "run", "remove:/temporary"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestMainLifecycleReturnsBuildFailure(t *testing.T) {
	code := mainLifecycle("cli", func() int {
		t.Fatal("run called after build failure")
		return 0
	}, lifecycleDeps{
		moduleRoot: func() (string, error) { return "/module", nil },
		makeTemp:   func(string, string) (string, error) { return "/temporary", nil },
		build:      func(string, string) error { return errors.New("build failed") },
		removeAll:  func(string) error { return nil },
	})
	if code != 1 {
		t.Fatalf("mainLifecycle() = %d, want 1", code)
	}
}

func TestMainLifecycleCleanupFailureChangesSuccessfulResult(t *testing.T) {
	code := mainLifecycle("cli", func() int { return 0 }, lifecycleDeps{
		moduleRoot: func() (string, error) { return "/module", nil },
		makeTemp:   func(string, string) (string, error) { return "/temporary", nil },
		build:      func(string, string) error { return nil },
		removeAll:  func(string) error { return errors.New("cleanup failed") },
	})
	if code != 1 {
		t.Fatalf("mainLifecycle() = %d, want 1", code)
	}
}

func TestMainLifecycleCleanupPreservesTestFailure(t *testing.T) {
	code := mainLifecycle("cli", func() int { return 7 }, lifecycleDeps{
		moduleRoot: func() (string, error) { return "/module", nil },
		makeTemp:   func(string, string) (string, error) { return "/temporary", nil },
		build:      func(string, string) error { return nil },
		removeAll:  func(string) error { return errors.New("cleanup failed") },
	})
	if code != 7 {
		t.Fatalf("mainLifecycle() = %d, want 7", code)
	}
}
