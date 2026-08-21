package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	runpkg "github.com/joellarson/togi/internal/run"
)

func run(args []string, out, errOut io.Writer) int {
	return executeCommand(args, errOut, newRootCommand(streams{out: out, err: errOut}))
}

func runWithService(args []string, out, errOut io.Writer, service commandService) int {
	return executeCommand(args, errOut, newRootCommandWithService(streams{out: out, err: errOut}, service))
}

func executeCommand(args []string, errOut io.Writer, cmd interface {
	SetArgs([]string)
	Execute() error
}) int {
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		var exitErr *runpkg.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.Code
		}
		return 70
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
