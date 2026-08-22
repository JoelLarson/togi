package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/enricher"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/repoid"
	"github.com/joellarson/togi/internal/run"
	"github.com/joellarson/togi/internal/wiki"
)

type serviceFactory struct{}

func newServiceFactory() serviceFactory { return serviceFactory{} }

func (serviceFactory) Name() string           { return "service" }
func (serviceFactory) CapabilityTags() string { return "" }
func (serviceFactory) NewGauntlet(env *Environment) (GauntletDriver, error) {
	if env == nil {
		return nil, errors.New("scenario environment is required")
	}
	return serviceGauntlet{environment: env}, nil
}
func (serviceFactory) NewHistory(env *Environment) (HistoryDriver, error) {
	if env == nil {
		return nil, errors.New("scenario environment is required")
	}
	return serviceHistory{environment: env}, nil
}
func (serviceFactory) NewWiki(env *Environment) (WikiDriver, error) {
	if env == nil {
		return nil, errors.New("scenario environment is required")
	}
	return serviceWiki{environment: env}, nil
}

type serviceGauntlet struct{ environment *Environment }

func (d serviceGauntlet) Run(ctx context.Context, request RunRequest) (RunObservation, error) {
	before, err := snapshotRuns(ctx, d.environment, request.Root)
	if err != nil {
		return RunObservation{}, err
	}
	var stdout, stderr bytes.Buffer
	_, serviceErr := d.service(&stdout, &stderr).Run(ctx, run.Options{
		Root: request.Root, Base: request.Base, GateNames: request.GateNames,
		ReportOnly: true, Verbose: request.Verbose, NoColor: request.NoColor,
	})
	after, err := snapshotRuns(ctx, d.environment, request.Root)
	if err != nil {
		return RunObservation{}, err
	}
	created := newRuns(before, after)
	if len(created) == 0 {
		return newRunObservation(stdout.Bytes(), stderr.Bytes(), serviceExit{err: serviceErr}, nil, "", nil), nil
	}
	if len(created) != 1 {
		return RunObservation{}, fmt.Errorf("service run created %d run directories", len(created))
	}
	reportPath := filepath.Join(d.environment.StateRoot, after.directory, "runs", created[0], "report.json")
	report, err := os.ReadFile(reportPath)
	if err != nil {
		return RunObservation{}, fmt.Errorf("read persisted report %q: %w", reportPath, err)
	}
	rawPaths, err := persistedRawPaths(filepath.Dir(reportPath))
	if err != nil {
		return RunObservation{}, err
	}
	return newRunObservation(stdout.Bytes(), stderr.Bytes(), serviceExit{err: serviceErr}, report, reportPath, rawPaths), nil
}

func (d serviceGauntlet) Close() error { return nil }

func (d serviceGauntlet) service(stdout, stderr *bytes.Buffer) run.Service {
	env := d.environment
	return run.Service{
		Paths:    config.Paths{Config: env.ConfigRoot, State: env.StateRoot, Cache: env.CacheRoot},
		Loader:   gate.Loader{OverrideDir: filepath.Join(env.ConfigRoot, "gates")},
		Executor: run.Executor{Enrichers: enricher.NewRegistry(), Now: env.clock.Now},
		Stdout:   stdout, VerboseOut: stderr, Now: env.clock.Now, Random: env.random,
		GOOS: env.GOOS, ResolveRepo: env.resolveRepo,
	}
}

type serviceHistory struct{ environment *Environment }

func (d serviceHistory) Status(ctx context.Context, request StatusRequest) (CommandObservation, error) {
	var stdout, stderr bytes.Buffer
	_, err := (serviceGauntlet{environment: d.environment}).service(&stdout, &stderr).Status(ctx, request.Root, request.NoColor)
	return CommandObservation{stdout: cloneBytes(stdout.Bytes()), stderr: cloneBytes(stderr.Bytes()), source: serviceExit{err: err}}, nil
}
func (serviceHistory) Close() error { return nil }

type serviceWiki struct{ environment *Environment }

func (d serviceWiki) Show(_ context.Context, name string) (CommandObservation, error) {
	return d.invoke(func(service wiki.Service) error { return service.Show(name) })
}
func (d serviceWiki) Lint(_ context.Context) (CommandObservation, error) {
	return d.invoke(func(service wiki.Service) error { return service.Lint() })
}
func (d serviceWiki) Eject(_ context.Context, name string) (CommandObservation, error) {
	return d.invoke(func(service wiki.Service) error { return service.Eject(name) })
}
func (serviceWiki) Close() error { return nil }
func (d serviceWiki) invoke(action func(wiki.Service) error) (CommandObservation, error) {
	var stdout, stderr bytes.Buffer
	env := d.environment
	err := action(wiki.Service{
		Pages:  wiki.Loader{OverrideDir: filepath.Join(env.ConfigRoot, "wiki")},
		Gates:  gate.Loader{OverrideDir: filepath.Join(env.ConfigRoot, "gates")},
		Stdout: &stdout, Stderr: &stderr,
	})
	return CommandObservation{stdout: cloneBytes(stdout.Bytes()), stderr: cloneBytes(stderr.Bytes()), source: serviceExit{err: err}}, nil
}

type runSnapshot struct {
	directory string
	runs      map[string]struct{}
}

func snapshotRuns(ctx context.Context, environment *Environment, root string) (runSnapshot, error) {
	if root == "" {
		root = "."
	}
	repository, err := repoid.Resolve(ctx, root)
	if err != nil {
		return runSnapshot{}, fmt.Errorf("resolve repository for run snapshot: %w", err)
	}
	directory := repository.Directory
	runsRoot := filepath.Join(environment.StateRoot, directory, "runs")
	entries, err := os.ReadDir(runsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return runSnapshot{directory: directory, runs: make(map[string]struct{})}, nil
	}
	if err != nil {
		return runSnapshot{}, fmt.Errorf("read run directory snapshot: %w", err)
	}
	runs := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			runs[entry.Name()] = struct{}{}
		}
	}
	return runSnapshot{directory: directory, runs: runs}, nil
}

func newRuns(before, after runSnapshot) []string {
	if before.directory != after.directory {
		return nil
	}
	created := make([]string, 0)
	for name := range after.runs {
		if _, exists := before.runs[name]; !exists {
			created = append(created, name)
		}
	}
	sort.Strings(created)
	return created
}

func persistedRawPaths(runDirectory string) (map[string]string, error) {
	rawDirectory := filepath.Join(runDirectory, "raw")
	entries, err := os.ReadDir(rawDirectory)
	if err != nil {
		return nil, fmt.Errorf("read persisted raw outputs: %w", err)
	}
	paths := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		parts := strings.Split(entry.Name(), ".")
		if len(parts) != 3 || (parts[2] != "stdout" && parts[2] != "stderr") {
			continue
		}
		paths[rawPathKey(parts[0], parts[1], parts[2])] = filepath.Join(rawDirectory, entry.Name())
	}
	return paths, nil
}
