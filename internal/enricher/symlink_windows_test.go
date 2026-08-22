//go:build windows

package enricher

import (
	"errors"
	"io/fs"
	"syscall"
)

func symlinkUnavailable(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.Errno(1314))
}
