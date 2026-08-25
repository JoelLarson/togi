package waiver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/pelletier/go-toml/v2"
)

var (
	// ErrReasonRequired means an approval carried no explanation.
	ErrReasonRequired = errors.New("waiver reason is required")
	// ErrInvalidFingerprint means the approved identity is not a fingerprint.
	ErrInvalidFingerprint = errors.New("waiver fingerprint is invalid")
)

// FileName is the waiver record's name at a repository's state-directory root.
const FileName = "waivers.toml"

// reasonLimit bounds one explanation so a durable operator record stays
// readable and the file stays bounded like every other persisted artifact.
const reasonLimit = 4096

// Record is one operator's approval of a single fingerprint.
type Record struct {
	Fingerprint string    `toml:"fingerprint"`
	Reason      string    `toml:"reason"`
	ApprovedAt  time.Time `toml:"approved_at"`
}

type document struct {
	Waivers []Record `toml:"waiver"`
}

// Store persists the waivers of one repository below its external state
// directory. Publication replaces the file atomically; two operators waiving
// at the same instant is not a case it arbitrates.
type Store struct {
	Dir string
	Now func() time.Time
}

// Approve records an approval of fingerprint and reports whether this call
// created it. Approving an already approved fingerprint keeps the original
// reason and approval time rather than adding a second record.
func (s Store) Approve(fingerprint, reason string) (Record, bool, error) {
	record, err := newRecord(fingerprint, reason, s.now())
	if err != nil {
		return Record{}, false, err
	}
	if err := s.prepareDirectory(); err != nil {
		return Record{}, false, err
	}
	root, err := s.open()
	if err != nil {
		return Record{}, false, err
	}
	defer func() { _ = root.Close() }()

	existing, err := load(root)
	if err != nil {
		return Record{}, false, err
	}
	for _, candidate := range existing {
		if candidate.Fingerprint == record.Fingerprint {
			return candidate, false, nil
		}
	}
	if err := publish(root, document{Waivers: append(existing, record)}); err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// Load returns this repository's approvals in the order they were approved.
func (s Store) Load() ([]Record, error) {
	root, err := s.open()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return load(root)
}

func (s Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// directory names this store's state directory, which is the one thing a
// zero Store cannot supply.
func (s Store) directory() (string, error) {
	if strings.TrimSpace(s.Dir) == "" {
		return "", errors.New("waiver state directory is required")
	}
	return s.Dir, nil
}

func (s Store) open() (*os.Root, error) {
	dir, err := s.directory()
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open waiver state directory: %w", err)
	}
	return root, nil
}

func (s Store) prepareDirectory() error {
	dir, err := s.directory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create waiver state directory: %w", err)
	}
	return nil
}

func newRecord(fingerprint, reason string, approvedAt time.Time) (Record, error) {
	if !finding.ValidFingerprint(fingerprint) {
		return Record{}, fmt.Errorf("%w: %q", ErrInvalidFingerprint, fingerprint)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Record{}, ErrReasonRequired
	}
	if len(reason) > reasonLimit {
		return Record{}, fmt.Errorf("waiver reason exceeds %d bytes", reasonLimit)
	}
	if approvedAt.IsZero() {
		return Record{}, errors.New("waiver approval time is required")
	}
	return Record{Fingerprint: fingerprint, Reason: reason, ApprovedAt: approvedAt.UTC()}, nil
}

func load(root *os.Root) ([]Record, error) {
	file, err := root.Open(FileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", FileName, err)
	}
	defer func() { _ = file.Close() }()

	var decoded document
	if err := toml.NewDecoder(file).DisallowUnknownFields().Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode %s (unknown or malformed field): %w", FileName, err)
	}
	seen := make(map[string]struct{}, len(decoded.Waivers))
	for index, record := range decoded.Waivers {
		validated, err := newRecord(record.Fingerprint, record.Reason, record.ApprovedAt)
		if err != nil {
			return nil, fmt.Errorf("%s waiver %d: %w", FileName, index+1, err)
		}
		if _, duplicate := seen[validated.Fingerprint]; duplicate {
			return nil, fmt.Errorf("%s waiver %d: duplicate fingerprint %q", FileName, index+1, validated.Fingerprint)
		}
		seen[validated.Fingerprint] = struct{}{}
		decoded.Waivers[index] = validated
	}
	return decoded.Waivers, nil
}

// publish replaces the file in one step, so a reader never observes a partial
// record set. It repeats the run ledger's temp-then-rename shape rather than
// sharing it: `run` imports this package, and ADR-0012 has no room for a
// technical file-writing package to hold the shape for both.
func publish(root *os.Root, decoded document) error {
	encoded, err := toml.Marshal(decoded)
	if err != nil {
		return fmt.Errorf("encode %s: %w", FileName, err)
	}
	temporary, name, err := createTemp(root)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = root.Remove(name)
		}
	}()
	if err := write(temporary, encoded); err != nil {
		return err
	}
	if err := root.Rename(name, FileName); err != nil {
		return fmt.Errorf("publish %s: %w", FileName, err)
	}
	remove = false
	return syncDirectory(root)
}

func createTemp(root *os.Root) (*os.File, string, error) {
	for range 10 {
		suffix := make([]byte, 8)
		if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
			return nil, "", err
		}
		name := ".waivers-" + hex.EncodeToString(suffix) + ".tmp"
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", fmt.Errorf("create temporary %s: %w", FileName, err)
		}
	}
	return nil, "", errors.New("temporary waiver file name collisions")
}

func write(temporary *os.File, encoded []byte) error {
	defer func() { _ = temporary.Close() }()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set %s permissions: %w", FileName, err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write %s: %w", FileName, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", FileName, err)
	}
	return nil
}
