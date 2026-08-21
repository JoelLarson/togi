package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Paths names togi's external configuration, durable state, and cache roots.
type Paths struct {
	Config string
	State  string
	Cache  string
}

// ResolvePaths computes togi's XDG storage paths without creating directories.
func ResolvePaths(home string) (Paths, error) {
	config, configFallback := xdgPath("XDG_CONFIG_HOME", ".config", home)
	state, stateFallback := xdgPath("XDG_STATE_HOME", filepath.Join(".local", "state"), home)
	cache, cacheFallback := xdgPath("XDG_CACHE_HOME", ".cache", home)
	if (configFallback || stateFallback || cacheFallback) && !filepath.IsAbs(home) {
		return Paths{}, fmt.Errorf("home directory %q must be an absolute path when an XDG fallback is needed", home)
	}

	return Paths{
		Config: filepath.Join(config, "togi"),
		State:  filepath.Join(state, "togi"),
		Cache:  filepath.Join(cache, "togi"),
	}, nil
}

func xdgPath(environment, fallback, home string) (string, bool) {
	value := os.Getenv(environment)
	if value != "" && filepath.IsAbs(value) {
		return value, false
	}
	return filepath.Join(home, fallback), true
}

// RepoState returns the durable state directory for a repository identity.
func (p Paths) RepoState(directory string) string {
	return filepath.Join(p.State, directory)
}

// GateOverrides returns the directory containing user-supplied gate definitions.
func (p Paths) GateOverrides() string {
	return filepath.Join(p.Config, "gates")
}
