package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

var (
	// ErrLocked means another process owns the repository run ledger.
	ErrLocked = errors.New("run ledger is locked")
	// ErrInvalidLock means the persistent lock path is not a regular file.
	ErrInvalidLock = errors.New("invalid run ledger lock")
	// ErrUnsupportedPlatform means this platform has no reliable stdlib advisory lock.
	ErrUnsupportedPlatform = errors.New("run ledger locking is unsupported on this platform")
)

type lockRecord struct {
	PID   int       `json:"pid"`
	Start time.Time `json:"start"`
	Token string    `json:"token"`
}

type stateLock struct {
	file     *os.File
	unlocked bool
	closed   bool
}

func acquireStateLock(root *os.Root, now time.Time) (*stateLock, error) {
	const name = "lock"
	record, err := newLockRecord(now, rand.Reader)
	if err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && (!before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalidLock, name)
	}

	file, err := openLockFile(root, name)
	if err != nil {
		return nil, err
	}
	lock := &stateLock{file: file, unlocked: true}
	cleanup := func(primary error) error {
		return errors.Join(primary, lock.release())
	}
	if err := validateOpenedLock(root, name, file); err != nil {
		return nil, cleanup(err)
	}
	if err := tryAdvisoryLock(file); err != nil {
		return nil, cleanup(err)
	}
	lock.unlocked = false
	if err := validateOpenedLock(root, name, file); err != nil {
		return nil, cleanup(err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, cleanup(fmt.Errorf("tighten lock permissions: %w", err))
	}
	lockInfo, err := file.Stat()
	if err != nil {
		return nil, cleanup(fmt.Errorf("inspect lock permissions: %w", err))
	}
	if !privateFileMode(lockInfo.Mode()) {
		return nil, cleanup(fmt.Errorf("lock permissions are %04o, want 0600", lockInfo.Mode().Perm()))
	}
	if err := file.Truncate(0); err != nil {
		return nil, cleanup(fmt.Errorf("truncate lock record: %w", err))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, cleanup(fmt.Errorf("seek lock record: %w", err))
	}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		return nil, cleanup(fmt.Errorf("encode lock record: %w", err))
	}
	if err := file.Sync(); err != nil {
		return nil, cleanup(fmt.Errorf("sync lock record: %w", err))
	}
	if err := validateOpenedLock(root, name, file); err != nil {
		return nil, cleanup(err)
	}
	return lock, nil
}

func validateOpenedLock(root *os.Root, name string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect persistent lock: %w", err)
	}
	current, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return fmt.Errorf("%w: %s changed while opening", ErrInvalidLock, name)
	}
	return nil
}

func newLockRecord(now time.Time, random io.Reader) (lockRecord, error) {
	tokenBytes := make([]byte, 16)
	if _, err := io.ReadFull(random, tokenBytes); err != nil {
		return lockRecord{}, fmt.Errorf("generate lock token: %w", err)
	}
	return lockRecord{
		PID:   os.Getpid(),
		Start: now.UTC(),
		Token: hex.EncodeToString(tokenBytes),
	}, nil
}

func (lock *stateLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	if !lock.unlocked {
		if err := unlockAdvisoryLock(lock.file); err != nil {
			return fmt.Errorf("unlock run ledger: %w", err)
		}
		lock.unlocked = true
	}
	if !lock.closed {
		if err := lock.file.Close(); err != nil {
			return fmt.Errorf("close run ledger lock: %w", err)
		}
		lock.closed = true
	}
	return nil
}
