package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/joellarson/togi/internal/config"
	"github.com/joellarson/togi/internal/enricher"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/normalizer"
	runpkg "github.com/joellarson/togi/internal/run"
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

type failingService struct{ err error }

func (service failingService) Run(context.Context, runpkg.Options) (runpkg.Report, error) {
	return runpkg.Report{}, service.err
}

func (service failingService) Status(context.Context, string, bool) (runpkg.Report, error) {
	return runpkg.Report{}, service.err
}

func newRootCommand(s streams) *cobra.Command {
	service, err := defaultService(s)
	if err != nil {
		return newRootCommandWithService(s, failingService{err: err})
	}
	return newRootCommandWithService(s, service)
}

func defaultService(s streams) (runpkg.Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return runpkg.Service{}, fmt.Errorf("resolve home directory: %w", err)
	}
	paths, err := config.ResolvePaths(home)
	if err != nil {
		return runpkg.Service{}, fmt.Errorf("resolve storage paths: %w", err)
	}
	return runpkg.Service{
		Paths:      paths,
		Loader:     gate.Loader{OverrideDir: paths.GateOverrides()},
		Executor:   runpkg.Executor{Registry: normalizer.NewRegistry(), Enricher: enricher.Go{}},
		Stdout:     s.out,
		VerboseOut: s.err,
	}, nil
}

func newRootCommandWithService(s streams, service commandService) *cobra.Command {
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
