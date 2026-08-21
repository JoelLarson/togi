package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	runpkg "github.com/joellarson/togi/internal/run"
)

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(streams{out: &stdout, err: &stderr})
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version command: %v", err)
	}
	if got, want := stdout.String(), "togi dev\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

type fakeService struct {
	runOptions    runpkg.Options
	runErr        error
	statusRoot    string
	statusNoColor bool
	statusErr     error
}

func (service *fakeService) Run(_ context.Context, opts runpkg.Options) (runpkg.Report, error) {
	service.runOptions = opts
	return runpkg.Report{}, service.runErr
}

func (service *fakeService) Status(_ context.Context, root string, noColor bool) (runpkg.Report, error) {
	service.statusRoot, service.statusNoColor = root, noColor
	return runpkg.Report{}, service.statusErr
}

func TestRunCommandPassesPhaseOneFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	service := &fakeService{runErr: &runpkg.ExitError{Code: 4, Err: errors.New("errored")}}
	cmd := newRootCommandWithService(streams{out: &stdout, err: &stderr}, service)
	cmd.SetArgs([]string{"run", "--report-only", "--gate", "lint", "--gate", "complexity", "--verbose", "--no-color"})
	err := cmd.Execute()
	var exitErr *runpkg.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 {
		t.Fatalf("Execute error = %v", err)
	}
	want := runpkg.Options{Root: ".", GateNames: []string{"lint", "complexity"}, ReportOnly: true, Verbose: true, NoColor: true}
	if got := service.runOptions; !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
}

func TestRunCommandRejectsBaseAndArguments(t *testing.T) {
	for _, args := range [][]string{{"run", "--base", "main"}, {"run", "extra"}} {
		service := &fakeService{}
		cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
		if args[1] == "--base" && !strings.Contains(err.Error(), "phase 1") {
			t.Fatalf("base error = %v", err)
		}
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

func TestMainRunMapsTypedAndInternalErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "typed", err: &runpkg.ExitError{Code: 5, Err: errors.New("unverified")}, want: 5},
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
	cmd := newRootCommand(streams{out: &stdout, err: &stderr})
	cmd.SetArgs([]string{"version", "extra"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected version command to reject positional arguments")
	}
}

func TestRunReportsCommandErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if got := run([]string{"unknown"}, &stdout, &stderr); got == 0 {
		t.Fatal("run status = 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q, want unknown-command diagnostic", stderr.String())
	}
}
