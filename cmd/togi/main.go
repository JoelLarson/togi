package main

import (
	"os"
)

func main() {
	if err := newRootCommand(streams{out: os.Stdout, err: os.Stderr}).Execute(); err != nil {
		os.Exit(1)
	}
}
