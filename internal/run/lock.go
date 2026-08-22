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
	"strings"
	"sync"
	"time"

	"github.com/joellarson/togi/internal/runner"
)

var (
	// ErrLocked means another run owns the repository run ledger.
	ErrLocked = errors.New("run ledger is locked")
	// ErrInvalidLock means the persistent lock path is not a regular file.
	ErrInvalidLock = errors.New("invalid run ledger lock")
	// ErrUnsupportedPlatform means this platform is outside phase-one runtime support.
	ErrUnsupportedPlatform = runner.ErrUnsupportedPlatform
)

type lockRecord struct {
	PID   int       `json:"pid"`
	Start time.Time `json:"start"`
	Token string    `json:"token"`
}

type stateLock struct {
	file     *os.File
	claim    *processLockClaim
	unlocked bool
	closed   bool
}

// A same-process open/close can release process-associated fcntl locks, so the
// local claim must be won before any backend opens the persistent lock file.
type processLockClaim struct {
	key      string
	identity os.FileInfo
}

var processLockClaims = struct {
	sync.Mutex
	owners map[string]*processLockClaim
}{owners: make(map[string]*processLockClaim)}

func acquireStateLock(root *os.Root, now time.Time) (*stateLock, error) {
	const name = "lock"
	key, identity, err := processLockIdentity(root, name)
	if err != nil {
		return nil, err
	}
	claim, err := claimProcessLock(key, identity)
	if err != nil {
		return nil, err
	}
	lock := &stateLock{claim: claim, unlocked: true}
	cleanup := func(primary error) error {
		return errors.Join(primary, lock.release())
	}
	record, err := newLockRecord(now, rand.Reader)
	if err != nil {
		return nil, cleanup(err)
	}
	before, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, cleanup(err)
	}
	if err == nil && (!before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0) {
		return nil, cleanup(fmt.Errorf("%w: %s is not a regular file", ErrInvalidLock, name))
	}

	file, err := openLockFile(root, name)
	if err != nil {
		return nil, cleanup(err)
	}
	lock.file = file
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
	if err := secureAndWriteLock(root, name, file, record); err != nil {
		return nil, cleanup(err)
	}
	return lock, nil
}

func secureAndWriteLock(root *os.Root, name string, file *os.File, record lockRecord) error {
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("tighten lock permissions: %w", err)
	}
	lockInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect lock permissions: %w", err)
	}
	if !privateFileMode(lockInfo.Mode()) {
		return fmt.Errorf("lock permissions are %04o, want 0600", lockInfo.Mode().Perm())
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock record: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek lock record: %w", err)
	}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		return fmt.Errorf("encode lock record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync lock record: %w", err)
	}
	if err := validateOpenedLock(root, name, file); err != nil {
		return err
	}
	return nil
}

func processLockIdentity(root *os.Root, name string) (string, os.FileInfo, error) {
	identity, err := root.Lstat(".")
	if err != nil {
		return "", nil, err
	}
	path, err := filepath.Abs(root.Name())
	if err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	key := filepath.Clean(filepath.Join(path, name))
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key, identity, nil
}

func claimProcessLock(key string, identity os.FileInfo) (*processLockClaim, error) {
	processLockClaims.Lock()
	defer processLockClaims.Unlock()
	for _, owner := range processLockClaims.owners {
		if owner.key == key || os.SameFile(owner.identity, identity) {
			return nil, ErrLocked
		}
	}
	claim := &processLockClaim{key: key, identity: identity}
	processLockClaims.owners[key] = claim
	return claim, nil
}

func releaseProcessLock(claim *processLockClaim) {
	if claim == nil {
		return
	}
	processLockClaims.Lock()
	defer processLockClaims.Unlock()
	if processLockClaims.owners[claim.key] == claim {
		delete(processLockClaims.owners, claim.key)
	}
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
	if lock == nil {
		return nil
	}
	if lock.file != nil && !lock.unlocked {
		if err := unlockAdvisoryLock(lock.file); err != nil {
			return fmt.Errorf("unlock run ledger: %w", err)
		}
		lock.unlocked = true
	}
	if lock.file != nil && !lock.closed {
		if err := lock.file.Close(); err != nil {
			return fmt.Errorf("close run ledger lock: %w", err)
		}
		lock.closed = true
	}
	releaseProcessLock(lock.claim)
	lock.claim = nil
	return nil
}
