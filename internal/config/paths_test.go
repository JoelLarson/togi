package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/repoid"
)

func testEnvironment(home string, values map[string]string) Environment {
	return Environment{Home: home, Getenv: func(key string) string { return values[key] }}
}

func TestResolveHonorsXDGEnvironment(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	got, err := Resolve(testEnvironment("", map[string]string{
		"XDG_CONFIG_HOME": configHome,
		"XDG_STATE_HOME":  stateHome,
		"XDG_CACHE_HOME":  cacheHome,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.GateOverrides() != filepath.Join(configHome, "togi", "gates") || got.Wiki() != filepath.Join(configHome, "togi", "wiki") {
		t.Fatalf("config paths = gates %q wiki %q", got.GateOverrides(), got.Wiki())
	}
}

func TestResolveUsesStandardFallbacks(t *testing.T) {
	home := t.TempDir()
	got, err := Resolve(testEnvironment(home, nil))
	if err != nil {
		t.Fatal(err)
	}
	id, err := repoid.New(strings.Repeat("a", 40), home)
	if err != nil {
		t.Fatal(err)
	}
	if got.GateOverrides() != filepath.Join(home, ".config", "togi", "gates") || got.Wiki() != filepath.Join(home, ".config", "togi", "wiki") || got.RepoState(id) != filepath.Join(home, ".local", "state", "togi", id.Key()) {
		t.Fatalf("fallback paths are incorrect")
	}
}

func TestResolveIgnoresRelativeXDGValues(t *testing.T) {
	home := t.TempDir()
	got, err := Resolve(testEnvironment(home, map[string]string{
		"XDG_CONFIG_HOME": "config",
		"XDG_STATE_HOME":  "state",
		"XDG_CACHE_HOME":  "cache",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.GateOverrides() != filepath.Join(home, ".config", "togi", "gates") {
		t.Fatalf("GateOverrides = %q", got.GateOverrides())
	}
}

func TestResolveRejectsInvalidHomeWhenFallbackNeeded(t *testing.T) {
	for _, home := range []string{"", "relative/home"} {
		_, err := Resolve(testEnvironment(home, nil))
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded, want invalid home error", home)
		}
		if !strings.Contains(err.Error(), "home") {
			t.Fatalf("Resolve(%q) error = %q, want home context", home, err)
		}
	}
}

func TestResolveDoesNotNeedHomeWhenAllXDGValuesAreAbsolute(t *testing.T) {
	root := t.TempDir()
	if _, err := Resolve(testEnvironment("", map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
	})); err != nil {
		t.Fatalf("Resolve with unused empty home failed: %v", err)
	}
}

func TestResolveCreatesNoDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := Resolve(testEnvironment("", map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("Resolve created %q or returned unexpected error: %v", root, err)
	}
}

func TestPathsDerivedDirectories(t *testing.T) {
	root := t.TempDir()
	p, err := Resolve(testEnvironment("", map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_CACHE_HOME": filepath.Join(root, "cache")}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := repoid.New(strings.Repeat("a", 40), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.RepoState(id), filepath.Join(root, "state", "togi", id.Key()); got != want {
		t.Fatalf("RepoState = %q, want %q", got, want)
	}
	if got, want := p.RunsDir(id), filepath.Join(root, "state", "togi", id.Key(), "runs"); got != want {
		t.Fatalf("RunsDir = %q, want %q", got, want)
	}
	if got, want := p.RunDir(id, "run-id"), filepath.Join(root, "state", "togi", id.Key(), "runs", "run-id"); got != want {
		t.Fatalf("RunDir = %q, want %q", got, want)
	}
	if got, want := p.GateOverrides(), filepath.Join(root, "config", "togi", "gates"); got != want {
		t.Fatalf("GateOverrides = %q, want %q", got, want)
	}
	if got, want := p.Wiki(), filepath.Join(root, "config", "togi", "wiki"); got != want {
		t.Fatalf("Wiki = %q, want %q", got, want)
	}
}

func TestResolveRejectsNilEnvironmentLookup(t *testing.T) {
	if _, err := Resolve(Environment{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("Resolve error = %v, want environment lookup error", err)
	}
}

func TestPathsZeroValueIsUnusable(t *testing.T) {
	var paths Paths
	if !paths.IsZero() {
		t.Fatal("zero Paths is not zero")
	}
	id, err := repoid.New(strings.Repeat("a", 40), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"gates": paths.GateOverrides(), "wiki": paths.Wiki(), "state": paths.RepoState(id), "runs": paths.RunsDir(id), "run": paths.RunDir(id, "run-id")} {
		if got != "" {
			t.Fatalf("zero Paths %s = %q, want empty", name, got)
		}
	}
}
