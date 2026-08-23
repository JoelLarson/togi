package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/joellarson/togi/internal/repoid"
)

// Environment supplies the process values needed to resolve XDG paths.
type Environment struct {
	Home   string
	Getenv func(string) string
}

// Paths names togi's external configuration, durable state, and cache roots.
type Paths struct {
	config string
	state  string
	cache  string
}

// Resolve computes togi's XDG storage paths without creating directories.
func Resolve(environment Environment) (Paths, error) {
	if environment.Getenv == nil {
		return Paths{}, errors.New("environment lookup is required")
	}
	config, configFallback := xdgPath(environment.Getenv, "XDG_CONFIG_HOME", ".config", environment.Home)
	state, stateFallback := xdgPath(environment.Getenv, "XDG_STATE_HOME", filepath.Join(".local", "state"), environment.Home)
	cache, cacheFallback := xdgPath(environment.Getenv, "XDG_CACHE_HOME", ".cache", environment.Home)
	if (configFallback || stateFallback || cacheFallback) && !filepath.IsAbs(environment.Home) {
		return Paths{}, fmt.Errorf("home directory %q must be an absolute path when an XDG fallback is needed", environment.Home)
	}

	return Paths{
		config: filepath.Join(config, "togi"),
		state:  filepath.Join(state, "togi"),
		cache:  filepath.Join(cache, "togi"),
	}, nil
}

func xdgPath(getenv func(string) string, environment, fallback, home string) (string, bool) {
	value := getenv(environment)
	if value != "" && filepath.IsAbs(value) {
		return value, false
	}
	return filepath.Join(home, fallback), true
}

// IsZero reports whether paths have not been resolved.
func (p Paths) IsZero() bool {
	return p.config == "" || p.state == "" || p.cache == ""
}

// RepoState returns the durable state directory for a repository identity.
func (p Paths) RepoState(id repoid.ID) string {
	if p.IsZero() || id.IsZero() {
		return ""
	}
	return filepath.Join(p.state, id.Dir())
}

// RunsDir returns the directory containing all run ledgers for a repository.
func (p Paths) RunsDir(id repoid.ID) string {
	if p.IsZero() || id.IsZero() {
		return ""
	}
	return filepath.Join(p.RepoState(id), "runs")
}

// RunDir returns one run ledger directory.
func (p Paths) RunDir(id repoid.ID, runID string) string {
	if p.IsZero() || id.IsZero() || !validPathComponent(runID) {
		return ""
	}
	return filepath.Join(p.RunsDir(id), runID)
}

// WorktreesDir returns the cache directory containing togi-owned worktrees for a repository.
func (p Paths) WorktreesDir(id repoid.ID) string {
	if p.IsZero() || id.IsZero() {
		return ""
	}
	return filepath.Join(p.cache, id.Key(), "worktrees")
}

// WorktreeDir returns the cache directory for one run's togi-owned worktree.
func (p Paths) WorktreeDir(id repoid.ID, runID string) string {
	if p.IsZero() || id.IsZero() || !validPathComponent(runID) {
		return ""
	}
	return filepath.Join(p.WorktreesDir(id), runID)
}

func validPathComponent(value string) bool {
	return value != "" && value != "." && value != ".." &&
		filepath.Clean(value) == value && !filepath.IsAbs(value) &&
		!strings.ContainsAny(value, `/\:`)
}

// GateOverrides returns the directory containing user-supplied gate definitions.
func (p Paths) GateOverrides() string {
	if p.IsZero() {
		return ""
	}
	return filepath.Join(p.config, "gates")
}

// Wiki returns the directory containing user-supplied principle pages.
func (p Paths) Wiki() string {
	if p.IsZero() {
		return ""
	}
	return filepath.Join(p.config, "wiki")
}
