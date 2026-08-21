package main

import (
	"fmt"
	"io"
	"os"
)

func run(args []string, out, errOut io.Writer) int {
	cmd := newRootCommand(streams{out: out, err: errOut})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
