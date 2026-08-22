package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type cliFactory struct{ binary string }

func newCLIFactory(binary string) cliFactory { return cliFactory{binary: binary} }

func (cliFactory) Name() string           { return "cli" }
func (cliFactory) CapabilityTags() string { return "~" + simulatedPlatformTag }
func (f cliFactory) NewGauntlet(env *Environment) (GauntletDriver, error) {
	if err := f.validate(env); err != nil {
		return nil, err
	}
	return cliGauntlet{factory: f, environment: env}, nil
}
func (f cliFactory) NewHistory(env *Environment) (HistoryDriver, error) {
	if err := f.validate(env); err != nil {
		return nil, err
	}
	return cliHistory{factory: f, environment: env}, nil
}
func (f cliFactory) NewWiki(env *Environment) (WikiDriver, error) {
	if err := f.validate(env); err != nil {
		return nil, err
	}
	return cliWiki{factory: f, environment: env}, nil
}
func (f cliFactory) validate(env *Environment) error {
	if env == nil {
		return errors.New("scenario environment is required")
	}
	if f.binary == "" {
		return errors.New("compiled acceptance CLI is required")
	}
	return nil
}

type cliGauntlet struct {
	factory     cliFactory
	environment *Environment
}

func (d cliGauntlet) Run(ctx context.Context, request RunRequest) (RunObservation, error) {
	before, err := snapshotRuns(ctx, d.environment, request.Root)
	if err != nil {
		return RunObservation{}, err
	}
	arguments := []string{"run", "--report-only", "--no-color"}
	if request.Base != "" {
		arguments = append(arguments, "--base", request.Base)
	}
	for _, name := range request.GateNames {
		arguments = append(arguments, "--gate", name)
	}
	if request.Verbose {
		arguments = append(arguments, "--verbose")
	}
	command, err := d.invoke(ctx, request.Root, arguments...)
	if err != nil {
		return RunObservation{}, err
	}
	after, err := snapshotRuns(ctx, d.environment, request.Root)
	if err != nil {
		return RunObservation{}, err
	}
	created := newRuns(before, after)
	if len(created) == 0 {
		return newRunObservation(command.stdout, command.stderr, processExit{code: command.code}, nil, "", nil), nil
	}
	if len(created) != 1 {
		return RunObservation{}, fmt.Errorf("CLI run created %d run directories", len(created))
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
	return newRunObservation(command.stdout, command.stderr, processExit{code: command.code}, report, reportPath, rawPaths), nil
}
func (cliGauntlet) Close() error { return nil }

type cliHistory struct {
	factory     cliFactory
	environment *Environment
}

func (d cliHistory) Status(ctx context.Context, request StatusRequest) (CommandObservation, error) {
	command, err := (cliGauntlet{factory: d.factory, environment: d.environment}).invoke(ctx, request.Root, "status", "--no-color")
	if err != nil {
		return CommandObservation{}, err
	}
	return newProcessCommandObservation(command), nil
}
func (cliHistory) Close() error { return nil }

type cliWiki struct {
	factory     cliFactory
	environment *Environment
}

func (d cliWiki) Show(ctx context.Context, name string) (CommandObservation, error) {
	return d.invoke(ctx, "wiki", "show", name)
}
func (d cliWiki) Lint(ctx context.Context) (CommandObservation, error) {
	return d.invoke(ctx, "wiki", "lint")
}
func (d cliWiki) Eject(ctx context.Context, name string) (CommandObservation, error) {
	return d.invoke(ctx, "wiki", "eject", name)
}
func (cliWiki) Close() error { return nil }
func (d cliWiki) invoke(ctx context.Context, arguments ...string) (CommandObservation, error) {
	command, err := (cliGauntlet{factory: d.factory, environment: d.environment}).invoke(ctx, d.environment.TempRoot, arguments...)
	if err != nil {
		return CommandObservation{}, err
	}
	return newProcessCommandObservation(command), nil
}

type processCommand struct {
	stdout []byte
	stderr []byte
	code   int
}

func newProcessCommandObservation(command processCommand) CommandObservation {
	return CommandObservation{
		stdout: cloneBytes(command.stdout),
		stderr: cloneBytes(command.stderr),
		source: processExit{code: command.code},
	}
}

func (d cliGauntlet) invoke(ctx context.Context, directory string, arguments ...string) (processCommand, error) {
	command := exec.CommandContext(ctx, d.factory.binary, arguments...)
	command.Dir = directory
	command.Env = d.environment.Environ()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := processCommand{stdout: cloneBytes(stdout.Bytes()), stderr: cloneBytes(stderr.Bytes())}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.code = exit.ExitCode()
		return result, nil
	}
	return processCommand{}, fmt.Errorf("run compiled acceptance CLI: %w", err)
}
