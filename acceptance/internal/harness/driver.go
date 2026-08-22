package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/internal/repoid"
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
	Root      string
	Base      string
	GateNames []string
	Verbose   bool
	NoColor   bool
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

	mu        sync.Mutex
	variables map[string]string
	previous  map[string]environmentValue
	active    bool
	closed    bool
	path      string
	resolves  atomic.Int64
	clock     *scenarioClock
	random    *scenarioRandom
}

type environmentValue struct {
	value string
	ok    bool
}

func NewEnvironment() (*Environment, error) {
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
		clock:      newScenarioClock(),
		random:     &scenarioRandom{},
	}
	for _, path := range []string{e.Home, e.ConfigRoot, e.StateRoot, e.CacheRoot, e.BinRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			_ = os.RemoveAll(tempRoot)
			return nil, fmt.Errorf("create scenario directory %q: %w", path, err)
		}
	}
	return e, nil
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
	return filepath.Join(e.StateRoot, id.Directory), nil
}

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
	values := map[string]string{
		"HOME":            e.Home,
		"XDG_CONFIG_HOME": filepath.Dir(e.ConfigRoot),
		"XDG_STATE_HOME":  filepath.Dir(e.StateRoot),
		"XDG_CACHE_HOME":  filepath.Dir(e.CacheRoot),
		"PATH":            e.BinRoot + string(os.PathListSeparator) + e.path,
		"LANG":            "C",
		"LC_ALL":          "C",
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
