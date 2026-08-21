//go:build !windows

package run

import (
	"errors"
	"os"
	"syscall"
)

func tryAdvisoryLock(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrLocked
	}
	return err
}

func unlockAdvisoryLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func syncRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func privateDirectoryMode(mode os.FileMode) bool {
	return mode.Perm() == 0o700
}

func privateFileMode(mode os.FileMode) bool {
	return mode.Perm() == 0o600
}
