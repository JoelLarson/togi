//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package waiver

import (
	"fmt"
	"os"
)

// syncDirectory makes a publication itself durable, not just its bytes.
func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open waiver state directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync waiver state directory: %w", err)
	}
	return nil
}
