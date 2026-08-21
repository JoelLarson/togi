//go:build linux

package run

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type processTree struct{}

func prepareProcessTree(cmd *exec.Cmd) (*processTree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processTree{}, nil
}

func (*processTree) afterStart(process *os.Process) error {
	if process == nil || process.Pid <= 0 {
		return errors.New("started process is unavailable")
	}
	if process.Pid == syscall.Getpgrp() {
		return errors.New("tool process group matches togi process group")
	}
	return nil
}

func (*processTree) terminate(process *os.Process) error {
	if process == nil || process.Pid <= 0 {
		return os.ErrProcessDone
	}
	groupID := process.Pid
	if groupID == syscall.Getpgrp() {
		return errors.New("refusing to signal togi process group")
	}
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("terminate tool process group: %w", err)
	}
	return nil
}

func (tree *processTree) close(process *os.Process) error {
	err := tree.terminate(process)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
