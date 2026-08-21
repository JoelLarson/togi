//go:build windows

package run

import (
	"errors"
	"syscall"
)

const errorInvalidParameter syscall.Errno = 87

func processIsAlive(pid int) (bool, error) {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err == nil {
		_ = syscall.CloseHandle(handle)
		return true, nil
	}
	if errors.Is(err, errorInvalidParameter) {
		return false, nil
	}
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		return true, nil
	}
	return false, err
}

func syncDirectory(string) error {
	return nil
}
