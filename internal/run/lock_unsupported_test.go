//go:build plan9 || js || wasip1

package run

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLedgerStartRejectsUnsupportedLockPlatformBeforeCreatingState(t *testing.T) {
	repoState := filepath.Join(t.TempDir(), "repo-state")
	run, err := (Ledger{RepoState: repoState}).Start()
	if run != nil {
		_ = run.Close()
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Start error = %v, want ErrUnsupportedPlatform", err)
	}
	if _, err := os.Lstat(repoState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported Start created repository state: %v", err)
	}
}
