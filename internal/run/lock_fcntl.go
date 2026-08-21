//go:build aix || (solaris && !illumos)

package run

import (
	"errors"
	"io"
	"os"
	"syscall"
)

func tryAdvisoryLock(file *os.File) error {
	lock := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: int16(io.SeekStart),
		Len:    1,
	}
	err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock)
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) {
		return ErrLocked
	}
	return err
}

func unlockAdvisoryLock(file *os.File) error {
	lock := syscall.Flock_t{
		Type:   syscall.F_UNLCK,
		Whence: int16(io.SeekStart),
		Len:    1,
	}
	return syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock)
}

func ensureLockPlatform() error { return nil }
