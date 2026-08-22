package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/joellarson/togi/internal/config"
	runpkg "github.com/joellarson/togi/internal/run"
)

func run(args []string, out, errOut io.Writer, environment config.Environment) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return executeCommand(ctx, args, errOut, newRootCommand(streams{out: out, err: errOut}, environment))
}

func runWithService(args []string, out, errOut io.Writer, service commandService) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return executeCommand(ctx, args, errOut, newRootCommandWithService(streams{out: out, err: errOut}, service))
}

func executeCommand(ctx context.Context, args []string, errOut io.Writer, cmd interface {
	SetContext(context.Context)
	SetArgs([]string)
	Execute() error
}) int {
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		var exitErr *runpkg.ExitError
		if errors.As(err, &exitErr) && exitErr != nil && validPublishedExitCode(exitErr.Code) {
			return exitErr.Code
		}
		return 70
	}
	return 0
}

func validPublishedExitCode(code int) bool {
	return code >= 1 && code <= 5
}

func mainEnvironment(getenv func(string) string) config.Environment {
	if getenv == nil {
		return config.Environment{}
	}
	values := make(map[string]string, 3)
	needsHome := false
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		values[key] = getenv(key)
		needsHome = needsHome || !filepath.IsAbs(values[key])
	}
	home := ""
	if needsHome {
		home = getenv("HOME")
	}
	return config.Environment{Home: home, Getenv: func(key string) string { return values[key] }}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, mainEnvironment(os.Getenv)))
}
