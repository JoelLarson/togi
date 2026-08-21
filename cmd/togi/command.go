package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var version = "dev"

type streams struct {
	out io.Writer
	err io.Writer
}

func newRootCommand(s streams) *cobra.Command {
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

	return root
}
