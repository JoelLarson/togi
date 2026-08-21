//go:build plan9 || js || wasip1

package run

import "os"

func ensureLockPlatform() error { return ErrUnsupportedPlatform }

func openLockFile(*os.Root, string) (*os.File, error) { return nil, ErrUnsupportedPlatform }

func tryAdvisoryLock(*os.File) error { return ErrUnsupportedPlatform }

func unlockAdvisoryLock(*os.File) error { return ErrUnsupportedPlatform }

func syncRootDirectory(*os.Root) error { return ErrUnsupportedPlatform }

func privateDirectoryMode(os.FileMode) bool { return true }

func privateFileMode(os.FileMode) bool { return true }
