//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package run

import "os"

func openLockFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
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
