package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathsHonorsXDGEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_CACHE_HOME", "/xdg/cache")

	got, err := ResolvePaths("/home/test")
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Config: "/xdg/config/togi", State: "/xdg/state/togi", Cache: "/xdg/cache/togi"}
	if got != want {
		t.Fatalf("Paths = %#v, want %#v", got, want)
	}
}

func TestResolvePathsUsesStandardFallbacks(t *testing.T) {
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
	}

	got, err := ResolvePaths("/home/test")
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Config: "/home/test/.config/togi", State: "/home/test/.local/state/togi", Cache: "/home/test/.cache/togi"}
	if got != want {
		t.Fatalf("Paths = %#v, want %#v", got, want)
	}
}

func TestResolvePathsIgnoresRelativeXDGValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "config")
	t.Setenv("XDG_STATE_HOME", "state")
	t.Setenv("XDG_CACHE_HOME", "cache")

	got, err := ResolvePaths("/home/test")
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Config: "/home/test/.config/togi", State: "/home/test/.local/state/togi", Cache: "/home/test/.cache/togi"}
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
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_CACHE_HOME", "/xdg/cache")

	if _, err := ResolvePaths(""); err != nil {
		t.Fatalf("ResolvePaths with unused empty home failed: %v", err)
	}
}

func TestResolvePathsCreatesNoDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))

	if _, err := ResolvePaths("/home/test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("ResolvePaths created %q or returned unexpected error: %v", root, err)
	}
}

func TestPathsDerivedDirectories(t *testing.T) {
	p := Paths{Config: "/xdg/config/togi", State: "/xdg/state/togi", Cache: "/xdg/cache/togi"}
	if got, want := p.RepoState("repo-abc"), "/xdg/state/togi/repo-abc"; got != want {
		t.Fatalf("RepoState = %q, want %q", got, want)
	}
	if got, want := p.GateOverrides(), "/xdg/config/togi/gates"; got != want {
		t.Fatalf("GateOverrides = %q, want %q", got, want)
	}
}
