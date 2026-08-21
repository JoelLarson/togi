//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package run

import (
	"errors"
	"os"
	"os/exec"
)

type processTree struct{}

func prepareProcessTree(_ *exec.Cmd) (*processTree, error) { return &processTree{}, nil }

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
