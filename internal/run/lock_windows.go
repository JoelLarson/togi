//go:build windows

package run

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx   = kernel32.NewProc("LockFileEx")
	unlockFileEx = kernel32.NewProc("UnlockFileEx")
)

func tryAdvisoryLock(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, callErr := lockFileEx.Call(
		file.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return nil
	}
	if errors.Is(callErr, errorLockViolation) {
		return ErrLocked
	}
	return callErr
}

func unlockAdvisoryLock(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, callErr := unlockFileEx.Call(
		file.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return nil
	}
	return callErr
}

func syncRootDirectory(*os.Root) error {
	return nil
}

func privateDirectoryMode(os.FileMode) bool {
	// Windows' stdlib Chmod can only change the read-only attribute. A nil
	// Chmod error is the strongest portable verification available here.
	return true
}

func privateFileMode(os.FileMode) bool {
	// File privacy follows the parent directory's inherited Windows ACL.
	return true
}
