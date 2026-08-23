package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/enricher"
	"github.com/joellarson/togi/internal/gate"
	runpkg "github.com/joellarson/togi/internal/run"
	"github.com/joellarson/togi/internal/wiki"
	"github.com/spf13/cobra"
)

var version = "dev"

type streams struct {
	out io.Writer
	err io.Writer
}

type commandService interface {
	Run(context.Context, runpkg.Options) (runpkg.Report, error)
	Status(context.Context, string, bool) (runpkg.Report, error)
}

type wikiService interface {
	Show(string) error
	Lint() error
	Eject(string) error
}

type failingService struct{ err error }

func (service failingService) Run(context.Context, runpkg.Options) (runpkg.Report, error) {
	return runpkg.Report{}, service.err
}

func (service failingService) Status(context.Context, string, bool) (runpkg.Report, error) {
	return runpkg.Report{}, service.err
}

func (service failingService) Show(string) error  { return service.err }
func (service failingService) Lint() error        { return service.err }
func (service failingService) Eject(string) error { return service.err }

func newRootCommand(s streams, environment config.Environment) *cobra.Command {
	service, pages, err := defaultServices(s, environment)
	if err != nil {
		failure := failingService{err: err}
		return newRootCommandWithServices(s, failure, failure)
	}
	return newRootCommandWithServices(s, service, pages)
}

func defaultServices(s streams, environment config.Environment) (runpkg.Service, wikiService, error) {
	paths, err := config.Resolve(environment)
	if err != nil {
		return runpkg.Service{}, nil, fmt.Errorf("resolve storage paths: %w", err)
	}
	pages := wiki.Service{
		Pages:  wiki.Loader{OverrideDir: paths.Wiki()},
		Gates:  gate.Loader{OverrideDir: paths.GateOverrides()},
		Stdout: s.out,
		Stderr: s.err,
	}
	service := runpkg.Service{
		Paths:      paths,
		Loader:     gate.Loader{OverrideDir: paths.GateOverrides()},
		Executor:   runpkg.Executor{Enrichers: enricher.NewRegistry()},
		Stdout:     s.out,
		VerboseOut: s.err,
	}
	return service, pages, nil
}

// newRootCommandWithService is the run/status test seam. Its wiki service is
// inert so a test can never reach the real config directory through it; wiki
// tests use newRootCommandWithServices directly.
func newRootCommandWithService(s streams, service commandService) *cobra.Command {
	return newRootCommandWithServices(s, service, failingService{err: errors.New("wiki service unavailable")})
}

func newRootCommandWithServices(s streams, service commandService, pages wikiService) *cobra.Command {
	root := &cobra.Command{
		Use:           "togi",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(s.out)
	root.SetErr(s.err)

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the togi version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "togi %s\n", version)
			return err
		},
	})
	root.AddCommand(newRunCommand(service))
	root.AddCommand(newStatusCommand(service))
	root.AddCommand(newWikiCommand(pages))

	return root
}

type runFlags struct {
	reportOnly bool
	base       string
	gates      []string
	verbose    bool
	noColor    bool
}

func newRunCommand(service commandService) *cobra.Command {
	flags := runFlags{}
	command := &cobra.Command{
		Use:   "run",
		Short: "Run the configured quality gates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := service.Run(cmd.Context(), runpkg.Options{
				Root:       ".",
				Base:       flags.base,
				GateNames:  append([]string(nil), flags.gates...),
				ReportOnly: flags.reportOnly,
				Verbose:    flags.verbose,
				NoColor:    flags.noColor,
			})
			return err
		},
	}
	command.Flags().BoolVar(&flags.reportOnly, "report-only", false, "report findings without fixing")
	command.Flags().StringVar(&flags.base, "base", "", "base ref for diff scoping")
	command.Flags().StringArrayVar(&flags.gates, "gate", nil, "run only the named gate")
	command.Flags().BoolVar(&flags.verbose, "verbose", false, "print gate execution details")
	command.Flags().BoolVar(&flags.noColor, "no-color", false, "disable colored output")
	return command
}

func newStatusCommand(service commandService) *cobra.Command {
	var noColor bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Show the latest completed run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := service.Status(cmd.Context(), ".", noColor)
			return err
		},
	}
	command.Flags().BoolVar(&noColor, "no-color", false, "disable colored output")
	return command
}

func newWikiCommand(pages wikiService) *cobra.Command {
	command := &cobra.Command{
		Use:   "wiki",
		Short: "Inspect and edit the principle pages",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(&cobra.Command{
		Use:   "show <page>",
		Short: "Print a principle page and the aliases that reach it",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return pages.Show(args[0]) },
	})
	command.AddCommand(&cobra.Command{
		Use:   "lint",
		Short: "Check every alias target for dangling and conflicting pages",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return pages.Lint() },
	})
	command.AddCommand(&cobra.Command{
		Use:   "eject <page>",
		Short: "Copy a shipped page into the config directory for editing",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return pages.Eject(args[0]) },
	})
	return command
}
