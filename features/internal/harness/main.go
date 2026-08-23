package harness

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

var acceptanceDriver = flag.String(
	"acceptance.driver",
	"service",
	"acceptance driver: service, cli, or all",
)

type testingM interface{ Run() int }

// Main initializes the requested acceptance drivers, runs the package suite,
// and tears down process-scoped build artifacts. Domain packages call it from
// their TestMain wrappers.
func Main(m testingM) int {
	flag.Parse()
	return mainLifecycle(*acceptanceDriver, m.Run, defaultLifecycleDeps())
}

type lifecycleDeps struct {
	moduleRoot func() (string, error)
	makeTemp   func(string, string) (string, error)
	build      func(string, string) error
	removeAll  func(string) error
	stderr     io.Writer
}

func defaultLifecycleDeps() lifecycleDeps {
	return lifecycleDeps{
		moduleRoot: func() (string, error) {
			workingDirectory, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("get package working directory: %w", err)
			}
			return findModuleRoot(workingDirectory)
		},
		makeTemp:  os.MkdirTemp,
		build:     buildCLI,
		removeAll: os.RemoveAll,
		stderr:    os.Stderr,
	}
}

func mainLifecycle(selection string, run func() int, deps lifecycleDeps) int {
	names, err := selectDriverNames(selection)
	if err != nil {
		lifecycleError(deps.stderr, err)
		return 1
	}

	selected := make([]DriverFactory, 0, len(names))
	var temporary string
	if includesDriver(names, "cli") {
		root, rootErr := deps.moduleRoot()
		if rootErr != nil {
			lifecycleError(deps.stderr, rootErr)
			return 1
		}
		temporary, err = deps.makeTemp("", "togi-acceptance-cli-")
		if err != nil {
			lifecycleError(deps.stderr, fmt.Errorf("create CLI build directory: %w", err))
			return 1
		}
		binary := filepath.Join(temporary, "togi")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		if err := deps.build(root, binary); err != nil {
			lifecycleError(deps.stderr, fmt.Errorf("build acceptance CLI: %w", err))
			if removeErr := deps.removeAll(temporary); removeErr != nil {
				lifecycleError(deps.stderr, fmt.Errorf("remove CLI build directory: %w", removeErr))
			}
			return 1
		}
		for _, name := range names {
			if name == "cli" {
				selected = append(selected, newCLIFactory(binary))
			} else {
				selected = append(selected, newServiceFactory())
			}
		}
	} else {
		selected = append(selected, newServiceFactory())
	}
	setSelectedFactories(selected)
	result := run()
	if temporary != "" {
		if err := deps.removeAll(temporary); err != nil {
			lifecycleError(deps.stderr, fmt.Errorf("remove CLI build directory: %w", err))
			if result == 0 {
				return 1
			}
		}
	}
	return result
}

func lifecycleError(stderr io.Writer, err error) {
	if stderr != nil {
		_, _ = fmt.Fprintln(stderr, err)
	}
}

func buildCLI(root, binary string) error {
	command := exec.Command("go", "build", "-o", binary, "./cmd/togi")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %w: %s", err, output)
	}
	return nil
}

func findModuleRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(filepath.Join(current, "go.mod"))
		switch {
		case statErr == nil && !info.IsDir():
			return current, nil
		case statErr == nil:
			return "", errors.New("go.mod is not a file")
		case !errors.Is(statErr, os.ErrNotExist):
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("go.mod not found")
		}
		current = parent
	}
}
