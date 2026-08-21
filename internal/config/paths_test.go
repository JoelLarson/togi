package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathsHonorsXDGEnvironment(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	got, err := ResolvePaths("")
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Config: filepath.Join(configHome, "togi"), State: filepath.Join(stateHome, "togi"), Cache: filepath.Join(cacheHome, "togi")}
	if got != want {
		t.Fatalf("Paths = %#v, want %#v", got, want)
	}
}

func TestResolvePathsUsesStandardFallbacks(t *testing.T) {
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
	}

	home := t.TempDir()
	got, err := ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Config: filepath.Join(home, ".config", "togi"), State: filepath.Join(home, ".local", "state", "togi"), Cache: filepath.Join(home, ".cache", "togi")}
	if got != want {
		t.Fatalf("Paths = %#v, want %#v", got, want)
	}
}

func TestResolvePathsIgnoresRelativeXDGValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "config")
	t.Setenv("XDG_STATE_HOME", "state")
	t.Setenv("XDG_CACHE_HOME", "cache")

	home := t.TempDir()
	got, err := ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Config: filepath.Join(home, ".config", "togi"), State: filepath.Join(home, ".local", "state", "togi"), Cache: filepath.Join(home, ".cache", "togi")}
	if got != want {
		t.Fatalf("Paths = %#v, want %#v", got, want)
	}
}

func TestResolvePathsRejectsInvalidHomeWhenFallbackNeeded(t *testing.T) {
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
	}
	for _, home := range []string{"", "relative/home"} {
		_, err := ResolvePaths(home)
		if err == nil {
			t.Fatalf("ResolvePaths(%q) succeeded, want invalid home error", home)
		}
		if !strings.Contains(err.Error(), "home") {
			t.Fatalf("ResolvePaths(%q) error = %q, want home context", home, err)
		}
	}
}

func TestResolvePathsDoesNotNeedHomeWhenAllXDGValuesAreAbsolute(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))

	if _, err := ResolvePaths(""); err != nil {
		t.Fatalf("ResolvePaths with unused empty home failed: %v", err)
	}
}

func TestResolvePathsCreatesNoDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))

	if _, err := ResolvePaths(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("ResolvePaths created %q or returned unexpected error: %v", root, err)
	}
}

func TestPathsDerivedDirectories(t *testing.T) {
	root := t.TempDir()
	p := Paths{Config: filepath.Join(root, "config", "togi"), State: filepath.Join(root, "state", "togi"), Cache: filepath.Join(root, "cache", "togi")}
	if got, want := p.RepoState("repo-abc"), filepath.Join(root, "state", "togi", "repo-abc"); got != want {
		t.Fatalf("RepoState = %q, want %q", got, want)
	}
	if got, want := p.GateOverrides(), filepath.Join(root, "config", "togi", "gates"); got != want {
		t.Fatalf("GateOverrides = %q, want %q", got, want)
	}
}
