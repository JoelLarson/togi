//go:build !linux

package harness

import "os"

func secureMkdirAll(string, string, os.FileMode) error            { return ErrUnsupportedCapability }
func secureWrite(string, string, []byte, os.FileMode) error       { return ErrUnsupportedCapability }
func secureRemove(string, string) error                           { return ErrUnsupportedCapability }
func secureAtomicWrite(string, string, []byte, os.FileMode) error { return ErrUnsupportedCapability }
func withWorkspaceMutation(string, func() error) error            { return ErrUnsupportedCapability }
