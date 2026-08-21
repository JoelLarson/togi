//go:build windows

package run

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

func openLockFile(root *os.Root, name string) (*os.File, error) {
	anchored, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := anchored.Close(); err != nil {
		return nil, err
	}
	path := filepath.Join(root.Name(), name)
	pathUTF16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathUTF16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = syscall.CloseHandle(handle)
		return nil, errors.New("wrap Windows lock file handle")
	}
	return file, nil
}

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

func ensureLockPlatform() error { return nil }

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
