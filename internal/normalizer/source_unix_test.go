//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package normalizer

import (
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSourceLookupRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := NewRegistry().Normalize(
			`regex:^(?P<file>.+):(?P<line>\d+)$`,
			regexContext(root),
			[]byte(fmt.Sprintf("%s:1\n", filepath.Base(path))),
		)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("error = nil, want non-regular source error")
		}
	case <-time.After(time.Second):
		t.Fatal("normalizer blocked opening a FIFO")
	}
}
