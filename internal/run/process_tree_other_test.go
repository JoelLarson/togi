//go:build !linux

package run

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareProcessTreeRejectsUnsupportedPlatformBeforeStart(t *testing.T) {
	cmd := exec.Command("must-not-start")
	tree, err := prepareProcessTree(cmd)
	if tree != nil || err == nil {
		t.Fatalf("prepareProcessTree() = (%#v, %v), want nil tree and error", tree, err)
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("error = %v, want ErrUnsupportedPlatform", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) || cmd.Process != nil {
		t.Fatalf("error/process = %q / %#v", err, cmd.Process)
	}
}
