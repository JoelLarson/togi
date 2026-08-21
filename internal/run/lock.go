package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

var (
	// ErrLocked means another live process owns the repository run ledger.
	ErrLocked = errors.New("run ledger is locked")
	// ErrInvalidLock means the lock path cannot be handled without risking
	// removal of an unrelated filesystem entry.
	ErrInvalidLock = errors.New("invalid run ledger lock")
	// ErrLockOwnershipLost means the lock changed before its owner released it.
	ErrLockOwnershipLost = errors.New("run ledger lock ownership lost")
)

type lockRecord struct {
	PID   int       `json:"pid"`
	Start time.Time `json:"start"`
	Token string    `json:"token"`
}

type stateLock struct {
	path   string
	record lockRecord
}

func acquireStateLock(path string, now time.Time) (*stateLock, error) {
	record, err := newLockRecord(now, rand.Reader)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		lock, err := createStateLock(path, record)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}

		existing, err := readLockRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if errors.Is(err, ErrInvalidLock) && attempt < 3 {
				runtime.Gosched()
				time.Sleep(time.Millisecond)
				continue
			}
			return nil, err
		}
		alive, err := processIsAlive(existing.PID)
		if err != nil {
			return nil, fmt.Errorf("check lock process %d: %w", existing.PID, err)
		}
		if alive {
			return nil, fmt.Errorf("%w: pid %d", ErrLocked, existing.PID)
		}
		removed, err := removeMatchingLock(path, existing)
		if err != nil {
			return nil, err
		}
		if !removed {
			continue
		}
	}
	return nil, fmt.Errorf("%w: lock changed repeatedly", ErrLocked)
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

func createStateLock(path string, record lockRecord) (*stateLock, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	created, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect created lock: %w", err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			current, err := os.Lstat(path)
			if err == nil && current.Mode().IsRegular() && os.SameFile(created, current) {
				_ = os.Remove(path)
			}
		}
	}()
	if err := json.NewEncoder(file).Encode(record); err != nil {
		return nil, fmt.Errorf("encode lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync lock: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close lock: %w", err)
	}
	keep = true
	return &stateLock{path: path, record: record}, nil
}

func readLockRecord(path string) (lockRecord, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return lockRecord{}, err
	}
	if !before.Mode().IsRegular() {
		return lockRecord{}, fmt.Errorf("%w: %s is not a regular file", ErrInvalidLock, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return lockRecord{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return lockRecord{}, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return lockRecord{}, fmt.Errorf("%w: %s changed while opening", ErrInvalidLock, path)
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var record lockRecord
	if err := decoder.Decode(&record); err != nil {
		return lockRecord{}, fmt.Errorf("%w: decode %s: %v", ErrInvalidLock, path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return lockRecord{}, fmt.Errorf("%w: trailing data in %s", ErrInvalidLock, path)
	}
	if record.PID <= 0 || record.Start.IsZero() || record.Token == "" {
		return lockRecord{}, fmt.Errorf("%w: incomplete record in %s", ErrInvalidLock, path)
	}
	return record, nil
}

func removeMatchingLock(path string, expected lockRecord) (bool, error) {
	actual, err := readLockRecord(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if actual.PID != expected.PID || !actual.Start.Equal(expected.Start) || actual.Token != expected.Token {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (lock *stateLock) release() error {
	if lock == nil || lock.path == "" {
		return nil
	}
	removed, err := removeMatchingLock(lock.path, lock.record)
	if err != nil {
		return err
	}
	if !removed {
		if _, err := os.Lstat(lock.path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return ErrLockOwnershipLost
	}
	if err := syncDirectory(filepath.Dir(lock.path)); err != nil {
		return fmt.Errorf("sync lock directory: %w", err)
	}
	return nil
}
