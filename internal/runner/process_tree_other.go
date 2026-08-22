//go:build !linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

type processTree struct{}

func prepareProcessTree(_ *exec.Cmd) (*processTree, error) {
	return nil, fmt.Errorf("%w: process execution on %s", ErrUnsupportedPlatform, runtime.GOOS)
}

func (*processTree) afterStart(process *os.Process) error {
	if process == nil {
		return errors.New("started process is unavailable")
	}
	return nil
}

func (*processTree) terminate(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}

func (tree *processTree) close(process *os.Process) error {
	err := tree.terminate(process)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
