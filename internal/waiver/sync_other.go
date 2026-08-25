//go:build !(aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package waiver

import "os"

// syncDirectory is a no-op where a directory handle cannot be synced.
// Orchestration returns unsupported on those platforms long before a waiver
// is recorded, so the publication above is all such a build can offer.
func syncDirectory(*os.Root) error { return nil }
