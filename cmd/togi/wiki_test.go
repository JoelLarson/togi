package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	runpkg "github.com/joellarson/togi/internal/run"
	"github.com/joellarson/togi/internal/wiki"
)

type fakeWikiService struct {
	shown    string
	ejected  string
	linted   bool
	lintErr  error
	showErr  error
	ejectErr error
}

func (service *fakeWikiService) Show(name string) error {
	service.shown = name
	return service.showErr
}

func (service *fakeWikiService) Lint() error {
	service.linted = true
	return service.lintErr
}

func (service *fakeWikiService) Eject(name string) error {
	service.ejected = name
	return service.ejectErr
}

func executeWiki(t *testing.T, service wikiService, args ...string) error {
	t.Helper()
	cmd := newRootCommandWithServices(streams{out: io.Discard, err: io.Discard}, &fakeService{}, service)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestWikiShowPassesPageName(t *testing.T) {
	service := &fakeWikiService{}
	if err := executeWiki(t, service, "wiki", "show", "small-composable-functions"); err != nil {
		t.Fatal(err)
	}
	if service.shown != "small-composable-functions" {
		t.Fatalf("shown = %q", service.shown)
	}
}

func TestWikiEjectPassesPageName(t *testing.T) {
	service := &fakeWikiService{}
	if err := executeWiki(t, service, "wiki", "eject", "small-composable-functions"); err != nil {
		t.Fatal(err)
	}
	if service.ejected != "small-composable-functions" {
		t.Fatalf("ejected = %q", service.ejected)
	}
}

func TestWikiShowAndEjectRequireExactlyOneArgument(t *testing.T) {
	for _, args := range [][]string{
		{"wiki", "show"},
		{"wiki", "show", "a", "b"},
		{"wiki", "eject"},
		{"wiki", "lint", "extra"},
	} {
		if err := executeWiki(t, &fakeWikiService{}, args...); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}

// A dangling alias is legal (ADR-0006), so lint must exit zero for it.
func TestWikiLintSucceedsWithoutConflicts(t *testing.T) {
	service := &fakeWikiService{}
	if err := executeWiki(t, service, "wiki", "lint"); err != nil {
		t.Fatal(err)
	}
	if !service.linted {
		t.Fatal("lint was not invoked")
	}
}

func TestWikiLintConflictsExitWithFindingsStatus(t *testing.T) {
	service := &fakeWikiService{lintErr: wiki.ErrConflictingAliases}
	err := executeWiki(t, service, "wiki", "lint")

	var exitErr *runpkg.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want an ExitError", err)
	}
	if exitErr.Code != 1 {
		t.Fatalf("exit code = %d, want 1", exitErr.Code)
	}
}

// A loader failure must surface as a plain error, not as a typed exit status.
func TestWikiLintPropagatesOtherErrors(t *testing.T) {
	service := &fakeWikiService{lintErr: errors.New("gate directory unreadable")}
	err := executeWiki(t, service, "wiki", "lint")

	var exitErr *runpkg.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want an untyped error", err)
	}
	if err == nil || err.Error() != "gate directory unreadable" {
		t.Fatalf("err = %v", err)
	}
}

// A bare group command prints help and succeeds, matching bare `togi`.
func TestWikiWithoutSubcommandPrintsHelp(t *testing.T) {
	var stdout bytes.Buffer
	service := &fakeWikiService{}
	cmd := newRootCommandWithServices(streams{out: &stdout, err: io.Discard}, &fakeService{}, service)
	cmd.SetArgs([]string{"wiki"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"show", "lint", "eject"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help omits %q:\n%s", want, stdout.String())
		}
	}
	if service.linted || service.shown != "" || service.ejected != "" {
		t.Fatal("bare wiki command invoked a subcommand")
	}
}
