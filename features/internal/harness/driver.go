package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/repoid"
	"github.com/joellarson/togi/internal/run"
)

// DriverFactory constructs the acceptance drivers requested by a scenario.
type DriverFactory interface {
	Name() string
	CapabilityTags() string
	NewGauntlet(*Environment) (GauntletDriver, error)
	NewHistory(*Environment) (HistoryDriver, error)
	NewWiki(*Environment) (WikiDriver, error)
}

type GauntletDriver interface {
	Run(context.Context, RunRequest) (RunObservation, error)
	Close() error
}

type HistoryDriver interface {
	Status(context.Context, StatusRequest) (CommandObservation, error)
	Close() error
}

type WikiDriver interface {
	Show(context.Context, string) (CommandObservation, error)
	Lint(context.Context) (CommandObservation, error)
	Eject(context.Context, string) (CommandObservation, error)
	Close() error
}

type RunRequest struct {
	Root          string
	Base          string
	GateNames     []string
	ReportOnly    bool
	Agent         string
	MaxIterations int
	MaxWallClock  time.Duration
	Verbose       bool
	NoColor       bool
}

// ReportOnly marks an existing acceptance request as non-mutating.
func ReportOnly(request RunRequest) RunRequest {
	request.ReportOnly = true
	return request
}

func normalizeRunRequest(request RunRequest) (RunRequest, error) {
	if request.ReportOnly {
		if request.MaxIterations != 0 || request.MaxWallClock != 0 {
			return RunRequest{}, errors.New("fix-mode rails cannot be used in report-only mode")
		}
		return request, nil
	}
	if request.MaxIterations == 0 {
		request.MaxIterations = run.DefaultMaxIterations
	}
	if request.MaxWallClock == 0 {
		request.MaxWallClock = run.DefaultMaxWallClock
	}
	return request, nil
}

type StatusRequest struct {
	Root    string
	NoColor bool
}

var ErrUnsupportedCapability = errors.New("acceptance driver does not support capability")

// Environment is the isolated process and storage environment for one scenario.
type Environment struct {
	TempRoot   string
	Home       string
	ConfigRoot string
	StateRoot  string
	CacheRoot  string
	BinRoot    string
	GOOS       string

	mu         sync.Mutex
	variables  map[string]string
	previous   map[string]environmentValue
	active     bool
	closed     bool
	path       string
	goModCache string
	goCache    string
	paths      config.Paths
	resolves   atomic.Int64
	clock      *scenarioClock
	random     *scenarioRandom
}

type environmentValue struct {
	value string
	ok    bool
}

func NewEnvironment() (*Environment, error) {
	goModCache, err := goEnvironmentPath("GOMODCACHE")
	if err != nil {
		return nil, err
	}
	goCache, err := goEnvironmentPath("GOCACHE")
	if err != nil {
		return nil, err
	}
	tempRoot, err := os.MkdirTemp("", "togi-acceptance-")
	if err != nil {
		return nil, fmt.Errorf("create scenario root: %w", err)
	}

	e := &Environment{
		TempRoot:   tempRoot,
		Home:       filepath.Join(tempRoot, "home"),
		ConfigRoot: filepath.Join(tempRoot, "config", "togi"),
		StateRoot:  filepath.Join(tempRoot, "state", "togi"),
		CacheRoot:  filepath.Join(tempRoot, "cache", "togi"),
		BinRoot:    filepath.Join(tempRoot, "bin"),
		GOOS:       runtime.GOOS,
		variables:  make(map[string]string),
		path:       os.Getenv("PATH"),
		goModCache: goModCache,
		goCache:    goCache,
		clock:      newScenarioClock(),
		random:     &scenarioRandom{},
	}
	paths, err := config.Resolve(config.Environment{Home: e.Home, Getenv: func(key string) string { return e.valuesLocked()[key] }})
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, fmt.Errorf("resolve scenario storage paths: %w", err)
	}
	e.paths = paths
	for _, path := range []string{e.Home, e.ConfigRoot, e.StateRoot, e.CacheRoot, e.BinRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			_ = os.RemoveAll(tempRoot)
			return nil, fmt.Errorf("create scenario directory %q: %w", path, err)
		}
	}
	return e, nil
}

func goEnvironmentPath(key string) (string, error) {
	output, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return "", fmt.Errorf("resolve Go %s: %w", key, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (e *Environment) resolveRepo(ctx context.Context, root string) (repoid.ID, error) {
	e.resolves.Add(1)
	return repoid.Resolve(ctx, root)
}

func (e *Environment) Setenv(key, value string) error {
	if key == "" || strings.ContainsAny(key, "=\x00") || strings.Contains(value, "\x00") {
		return fmt.Errorf("invalid environment variable %q", key)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active || e.closed {
		return errors.New("scenario environment is no longer configurable")
	}
	e.variables[key] = value
	return nil
}

// RestrictPath retains only named ambient tools beside scenario-owned tools.
func (e *Environment) RestrictPath(names ...string) error {
	if runtime.GOOS == "windows" {
		return ErrUnsupportedCapability
	}
	targets := make(map[string]string, len(names))
	for _, name := range names {
		if err := validateFixtureName("tool", name); err != nil {
			return err
		}
		path, err := exec.LookPath(name)
		if err != nil {
			return fmt.Errorf("retain tool %q: %w", name, err)
		}
		targets[name] = path
	}
	for name, target := range targets {
		link := filepath.Join(e.BinRoot, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect retained tool %q: %w", name, err)
		}
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("retain tool %q: %w", name, err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.active || e.closed {
		return errors.New("scenario environment is not active")
	}
	e.path = ""
	if err := os.Setenv("PATH", e.BinRoot); err != nil {
		return fmt.Errorf("restrict scenario PATH: %w", err)
	}
	return nil
}

func (e *Environment) Activate() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("scenario environment is closed")
	}
	if e.active {
		return errors.New("scenario environment is already active")
	}

	values := e.valuesLocked()
	e.previous = make(map[string]environmentValue, len(values))
	for key, value := range values {
		previous, ok := os.LookupEnv(key)
		e.previous[key] = environmentValue{value: previous, ok: ok}
		if err := os.Setenv(key, value); err != nil {
			e.restoreLocked()
			return fmt.Errorf("activate scenario environment: %w", err)
		}
	}
	e.active = true
	return nil
}

func (e *Environment) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	var restoreErr error
	if e.active {
		restoreErr = e.restoreLocked()
		e.active = false
	}
	e.closed = true
	removeErr := os.RemoveAll(e.TempRoot)
	return errors.Join(restoreErr, removeErr)
}

func (e *Environment) Environ() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	values := e.valuesLocked()
	result := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, overridden := values[key]; !overridden {
			result = append(result, entry)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (e *Environment) RepoState(ctx context.Context, root string) (string, error) {
	id, err := e.resolveRepo(ctx, root)
	if err != nil {
		return "", err
	}
	return e.paths.RepoState(id), nil
}

// Paths reports the storage roots togi resolves from this scenario's
// environment. Both drivers read external state through it, so the in-process
// service and the CLI subprocess observe one layout.
func (e *Environment) Paths() config.Paths { return e.paths }

type scenarioClock struct {
	mu   sync.Mutex
	next time.Time
}

func newScenarioClock() *scenarioClock {
	return &scenarioClock{next: time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)}
}

func (c *scenarioClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.next
	c.next = c.next.Add(time.Nanosecond)
	return now
}

type scenarioRandom struct{ next atomic.Uint64 }

func (r *scenarioRandom) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte(r.next.Add(1))
	}
	return len(buffer), nil
}

func (e *Environment) RepoResolutions() int64 {
	return e.resolves.Load()
}

func (e *Environment) valuesLocked() map[string]string {
	pathValue := e.BinRoot
	if e.path != "" {
		pathValue += string(os.PathListSeparator) + e.path
	}
	values := map[string]string{
		"HOME":            e.Home,
		"XDG_CONFIG_HOME": filepath.Dir(e.ConfigRoot),
		"XDG_STATE_HOME":  filepath.Dir(e.StateRoot),
		"XDG_CACHE_HOME":  filepath.Dir(e.CacheRoot),
		"PATH":            pathValue,
		"LANG":            "C",
		"LC_ALL":          "C",
		"GOMODCACHE":      e.goModCache,
		"GOCACHE":         e.goCache,
	}
	for key, value := range e.variables {
		values[key] = value
	}
	return values
}

func (e *Environment) restoreLocked() error {
	var result error
	for key, previous := range e.previous {
		var err error
		if previous.ok {
			err = os.Setenv(key, previous.value)
		} else {
			err = os.Unsetenv(key)
		}
		result = errors.Join(result, err)
	}
	e.previous = nil
	return result
}

// World holds scenario-local driver state. Bindings are introduced with the
// domain feature suites, after their fixture APIs exist.
type Capabilities uint8

const (
	NeedsGauntlet Capabilities = 1 << iota
	NeedsHistory
	NeedsWiki
)

type World struct {
	factory      DriverFactory
	capabilities Capabilities
	environment  *Environment
	repository   *Repository
	gauntlet     GauntletDriver
	history      HistoryDriver
	wiki         WikiDriver
	lastRun      RunObservation
	lastCommand  CommandObservation
}

func NewWorld(factory DriverFactory, capabilities Capabilities) *World {
	return &World{factory: factory, capabilities: capabilities}
}
func (w *World) Before(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
	if w.factory == nil {
		return ctx, errors.New("scenario driver factory is required")
	}
	environment, err := NewEnvironment()
	if err != nil {
		return ctx, err
	}
	if err := environment.Activate(); err != nil {
		_ = environment.Close()
		return ctx, err
	}
	w.environment = environment
	w.repository = nil
	w.lastRun = RunObservation{}
	w.lastCommand = CommandObservation{}

	if w.capabilities&NeedsGauntlet != 0 {
		w.gauntlet, err = w.factory.NewGauntlet(environment)
	}
	if err == nil && w.capabilities&NeedsHistory != 0 {
		w.history, err = w.factory.NewHistory(environment)
	}
	if err == nil && w.capabilities&NeedsWiki != 0 {
		w.wiki, err = w.factory.NewWiki(environment)
	}
	if err != nil {
		_, cleanupErr := w.After(ctx, nil, err)
		return ctx, errors.Join(err, cleanupErr)
	}
	return ctx, nil
}
func (w *World) After(ctx context.Context, _ *godog.Scenario, scenarioErr error) (context.Context, error) {
	var result error
	if w.wiki != nil {
		result = errors.Join(result, w.wiki.Close())
		w.wiki = nil
	}
	if w.history != nil {
		result = errors.Join(result, w.history.Close())
		w.history = nil
	}
	if w.gauntlet != nil {
		result = errors.Join(result, w.gauntlet.Close())
		w.gauntlet = nil
	}
	if w.environment != nil {
		result = errors.Join(result, w.environment.Close())
		w.environment = nil
	}
	if scenarioErr != nil {
		result = errors.Join(scenarioErr, result)
	}
	return ctx, result
}
func (w *World) Environment() *Environment { return w.environment }
func (w *World) Repository() *Repository   { return w.repository }
func (w *World) UseRepository(repository *Repository) error {
	if repository == nil || repository.Root == "" {
		return errors.New("scenario repository is required")
	}
	w.repository = repository
	return nil
}
func (w *World) Gauntlet() (GauntletDriver, error) {
	if w.gauntlet == nil {
		return nil, ErrUnsupportedCapability
	}
	return w.gauntlet, nil
}
func (w *World) History() (HistoryDriver, error) {
	if w.history == nil {
		return nil, ErrUnsupportedCapability
	}
	return w.history, nil
}
func (w *World) Wiki() (WikiDriver, error) {
	if w.wiki == nil {
		return nil, ErrUnsupportedCapability
	}
	return w.wiki, nil
}
func (w *World) Run(ctx context.Context, request RunRequest) error {
	driver, err := w.Gauntlet()
	if err != nil {
		return err
	}
	observation, err := driver.Run(ctx, request)
	if err != nil {
		return err
	}
	w.lastRun = observation
	return nil
}
func (w *World) Status(ctx context.Context, request StatusRequest) error {
	driver, err := w.History()
	if err != nil {
		return err
	}
	observation, err := driver.Status(ctx, request)
	if err != nil {
		return err
	}
	w.lastCommand = observation
	return nil
}
func (w *World) Show(ctx context.Context, name string) error {
	driver, err := w.Wiki()
	if err != nil {
		return err
	}
	observation, err := driver.Show(ctx, name)
	if err != nil {
		return err
	}
	w.lastCommand = observation
	return nil
}
func (w *World) Lint(ctx context.Context) error {
	driver, err := w.Wiki()
	if err != nil {
		return err
	}
	observation, err := driver.Lint(ctx)
	if err != nil {
		return err
	}
	w.lastCommand = observation
	return nil
}
func (w *World) Eject(ctx context.Context, name string) error {
	driver, err := w.Wiki()
	if err != nil {
		return err
	}
	observation, err := driver.Eject(ctx, name)
	if err != nil {
		return err
	}
	w.lastCommand = observation
	return nil
}
func (w *World) LastRun() RunObservation         { return w.lastRun }
func (w *World) LastCommand() CommandObservation { return w.lastCommand }
