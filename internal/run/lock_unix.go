//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

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

func ensureLockPlatform() error { return nil }
