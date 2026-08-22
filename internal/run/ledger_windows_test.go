//go:build windows

package run

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const errorSharingViolation syscall.Errno = 32

func TestWindowsRootHandlesDenyRepoStateReplacement(t *testing.T) {
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	movedState := filepath.Join(root, "moved-state")
	if err := os.Rename(repoState, movedState); !isWindowsSharingViolation(err) {
		_ = run.Close()
		t.Fatalf("rename with open ledger roots = %v, want sharing violation", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repoState, movedState); err != nil {
		t.Fatalf("rename after Close: %v", err)
	}
}

func TestWindowsLockHandleDeniesUnlink(t *testing.T) {
	repoState := t.TempDir()
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(repoState, "lock")
	if err := os.Remove(lockPath); !isWindowsSharingViolation(err) {
		_ = run.Close()
		t.Fatalf("remove with open lock = %v, want sharing violation", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove after Close: %v", err)
	}
}

func isWindowsSharingViolation(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
