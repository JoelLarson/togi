package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/enricher"
	runpkg "github.com/joellarson/togi/internal/run"
)

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(streams{out: &stdout, err: &stderr}, commandEnvironment(t))
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version command: %v", err)
	}
	if got, want := stdout.String(), "togi dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

type fakeService struct {
	ctx           context.Context
	runOptions    runpkg.Options
	runErr        error
	statusRoot    string
	statusNoColor bool
	statusErr     error
}

func (service *fakeService) Run(ctx context.Context, opts runpkg.Options) (runpkg.Report, error) {
	service.ctx = ctx
	service.runOptions = opts
	return runpkg.Report{}, service.runErr
}

func (service *fakeService) Status(_ context.Context, root string, noColor bool) (runpkg.Report, error) {
	service.statusRoot, service.statusNoColor = root, noColor
	return runpkg.Report{}, service.statusErr
}

func TestRunCommandPassesFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	service := &fakeService{runErr: &runpkg.ExitError{Code: 4, Err: errors.New("errored")}}
	cmd := newRootCommandWithService(streams{out: &stdout, err: &stderr}, service)
	cmd.SetArgs([]string{"run", "--base", "refs/heads/main", "--report-only", "--gate", "lint", "--gate", "complexity", "--verbose", "--no-color"})
	err := cmd.Execute()
	var exitErr *runpkg.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 {
		t.Fatalf("Execute error = %v", err)
	}
	want := runpkg.Options{Root: ".", Base: "refs/heads/main", GateNames: []string{"lint", "complexity"}, ReportOnly: true, Verbose: true, NoColor: true}
	if got := service.runOptions; !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
}

func TestRunCommandRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"run", "extra"}} {
		service := &fakeService{}
		cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
}

func TestDefaultServiceUsesGoEnricher(t *testing.T) {
	service, _, err := defaultServices(streams{out: io.Discard, err: io.Discard}, commandEnvironment(t))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := service.Executor.Enrichers.For("go")
	if !ok {
		t.Fatal("Go enricher is not registered")
	}
	if _, ok := got.(enricher.Go); !ok {
		t.Fatalf("enricher = %T, want enricher.Go", got)
	}
}

func TestStatusCommandReadsWithoutRunVerdict(t *testing.T) {
	service := &fakeService{}
	cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
	cmd.SetArgs([]string{"status", "--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if service.statusRoot != "." || !service.statusNoColor {
		t.Fatalf("status args = %q, %v", service.statusRoot, service.statusNoColor)
	}
	if service.runOptions.Root != "" {
		t.Fatal("status invoked run")
	}
}

func TestExecuteCommandPassesCanceledContextToService(t *testing.T) {
	service := &fakeService{}
	cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := executeCommand(ctx, []string{"run"}, io.Discard, cmd); got != 0 {
		t.Fatalf("status = %d", got)
	}
	if service.ctx == nil || !errors.Is(service.ctx.Err(), context.Canceled) {
		t.Fatalf("service context = %v", service.ctx)
	}
}

func TestMainRunMapsTypedAndInternalErrors(t *testing.T) {
	var typedNil *runpkg.ExitError
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "typed", err: &runpkg.ExitError{Code: 5, Err: errors.New("unverified")}, want: 5},
		{name: "blocked", err: &runpkg.ExitError{Code: 2, Err: errors.New("blocked")}, want: 2},
		{name: "typed nil", err: typedNil, want: 70},
		{name: "zero", err: &runpkg.ExitError{Code: 0}, want: 70},
		{name: "invalid", err: &runpkg.ExitError{Code: 6}, want: 70},
		{name: "internal", err: errors.New("broken"), want: 70},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			service := &fakeService{runErr: tc.err}
			got := runWithService([]string{"run"}, io.Discard, &stderr, service)
			if got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
			if strings.Count(stderr.String(), tc.err.Error()) != 1 {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(streams{out: &stdout, err: &stderr}, commandEnvironment(t))
	cmd.SetArgs([]string{"version", "extra"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected version command to reject positional arguments")
	}
}

func TestRunReportsCommandErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if got := run([]string{"unknown"}, &stdout, &stderr, commandEnvironment(t)); got == 0 {
		t.Fatal("run status = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q, want unknown-command diagnostic", stderr.String())
	}
}

func TestRootCommandResolvesStorageOnce(t *testing.T) {
	lookups := make(map[string]int)
	root := t.TempDir()
	environment := config.Environment{Getenv: func(key string) string {
		lookups[key]++
		return map[string]string{
			"XDG_CONFIG_HOME": root + "/config",
			"XDG_STATE_HOME":  root + "/state",
			"XDG_CACHE_HOME":  root + "/cache",
		}[key]
	}}
	_ = newRootCommand(streams{out: io.Discard, err: io.Discard}, environment)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if lookups[key] != 1 {
			t.Fatalf("%s lookups = %d, want 1", key, lookups[key])
		}
	}
}

func commandEnvironment(t *testing.T) config.Environment {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"XDG_CONFIG_HOME": root + "/config",
		"XDG_STATE_HOME":  root + "/state",
		"XDG_CACHE_HOME":  root + "/cache",
	}
	return config.Environment{Getenv: func(key string) string { return values[key] }}
}
