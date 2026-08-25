package waiver_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/waiver"
)

const (
	fingerprintA = "0f2ac1e1b8f7c8b56b6da5e0f9dc0f6e6c1a2b3c4d5e6f708192a3b4c5d6e7f8"
	fingerprintB = "1a2b3c4d5e6f708192a3b4c5d6e7f80f2ac1e1b8f7c8b56b6da5e0f9dc0f6e6c"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newStore(t *testing.T) waiver.Store {
	t.Helper()
	return waiver.Store{
		Dir: filepath.Join(t.TempDir(), "repo-state"),
		Now: fixedClock(time.Date(2026, time.August, 25, 3, 17, 33, 0, time.UTC)),
	}
}

func TestApproveRecordsReasonAndApprovalTime(t *testing.T) {
	store := newStore(t)

	record, created, err := store.Approve(fingerprintA, "the deleted test covered a removed feature")
	if err != nil {
		t.Fatalf("Approve() = %v", err)
	}
	if !created {
		t.Fatal("Approve() reported an existing record for a first approval")
	}
	want := waiver.Record{
		Fingerprint: fingerprintA,
		Reason:      "the deleted test covered a removed feature",
		ApprovedAt:  time.Date(2026, time.August, 25, 3, 17, 33, 0, time.UTC),
	}
	if record != want {
		t.Fatalf("Approve() = %#v, want %#v", record, want)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 || loaded[0] != want {
		t.Fatalf("Load() = %#v, want one %#v", loaded, want)
	}
}

func TestApproveWritesTheDocumentedFilename(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Approve(fingerprintA, "approved"); err != nil {
		t.Fatalf("Approve() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "waivers.toml")); err != nil {
		t.Fatalf("persisted waiver file: %v", err)
	}
}

func TestApproveKeepsOneRecordPerFingerprint(t *testing.T) {
	store := newStore(t)
	first, _, err := store.Approve(fingerprintA, "the original judgement")
	if err != nil {
		t.Fatalf("first Approve() = %v", err)
	}

	store.Now = fixedClock(time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC))
	again, created, err := store.Approve(fingerprintA, "a second, later judgement")
	if err != nil {
		t.Fatalf("second Approve() = %v", err)
	}
	if created {
		t.Fatal("Approve() reported a repeated approval as newly created")
	}
	if again != first {
		t.Fatalf("Approve() = %#v, want the original %#v", again, first)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 || loaded[0] != first {
		t.Fatalf("Load() = %#v, want only the original record", loaded)
	}
}

func TestApprovePreservesEarlierApprovalsInOrder(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Approve(fingerprintA, "first"); err != nil {
		t.Fatalf("Approve(A) = %v", err)
	}
	store.Now = fixedClock(time.Date(2026, time.August, 26, 8, 0, 0, 0, time.UTC))
	if _, _, err := store.Approve(fingerprintB, "second"); err != nil {
		t.Fatalf("Approve(B) = %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 2 || loaded[0].Fingerprint != fingerprintA || loaded[1].Fingerprint != fingerprintB {
		t.Fatalf("Load() = %#v, want both approvals in approval order", loaded)
	}
	if loaded[0].Reason != "first" || loaded[1].Reason != "second" {
		t.Fatalf("Load() reasons = %q and %q", loaded[0].Reason, loaded[1].Reason)
	}
}

func TestApproveRequiresAReason(t *testing.T) {
	for _, reason := range []string{"", "   ", "\t\n"} {
		store := newStore(t)
		if _, _, err := store.Approve(fingerprintA, reason); !errors.Is(err, waiver.ErrReasonRequired) {
			t.Fatalf("Approve(%q) = %v, want ErrReasonRequired", reason, err)
		}
		if _, err := os.Stat(filepath.Join(store.Dir, "waivers.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused approval wrote state: %v", err)
		}
	}
}

func TestApproveBoundsTheReason(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Approve(fingerprintA, strings.Repeat("x", 4097)); err == nil {
		t.Fatal("Approve() accepted an unbounded reason")
	}
	if _, _, err := store.Approve(fingerprintA, strings.Repeat("x", 4096)); err != nil {
		t.Fatalf("Approve() rejected a reason at the bound: %v", err)
	}
}

func TestApproveRequiresAFingerprint(t *testing.T) {
	for _, fingerprint := range []string{
		"",
		"not-a-fingerprint",
		strings.ToUpper(fingerprintA),
		fingerprintA[:63],
		fingerprintA + "0",
	} {
		store := newStore(t)
		if _, _, err := store.Approve(fingerprint, "approved"); !errors.Is(err, waiver.ErrInvalidFingerprint) {
			t.Fatalf("Approve(%q) = %v, want ErrInvalidFingerprint", fingerprint, err)
		}
	}
}

func TestApproveRequiresADirectory(t *testing.T) {
	if _, _, err := (waiver.Store{}).Approve(fingerprintA, "approved"); err == nil {
		t.Fatal("Approve() accepted a store with no state directory")
	}
	if _, err := (waiver.Store{}).Load(); err == nil {
		t.Fatal("Load() accepted a store with no state directory")
	}
}

func TestApproveKeepsWaiverStatePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	store := newStore(t)
	if _, _, err := store.Approve(fingerprintA, "approved"); err != nil {
		t.Fatalf("Approve() = %v", err)
	}
	directory, err := os.Stat(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode = %04o, want 0700", directory.Mode().Perm())
	}
	file, err := os.Stat(filepath.Join(store.Dir, "waivers.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode().Perm() != 0o600 {
		t.Fatalf("waiver file mode = %04o, want 0600", file.Mode().Perm())
	}
}

func TestApproveLeavesNoTemporaryFiles(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Approve(fingerprintA, "approved"); err != nil {
		t.Fatalf("Approve() = %v", err)
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "waivers.toml" {
		t.Fatalf("state directory = %v, want only waivers.toml", entries)
	}
}

func TestLoadWithoutAnApprovalIsEmpty(t *testing.T) {
	store := newStore(t)
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("Load() = %#v, want no records", loaded)
	}
}

func TestLoadRejectsAnUnreadableFile(t *testing.T) {
	approvedAt := "approved_at = 2026-08-25T03:17:33Z"
	for name, body := range map[string]string{
		"malformed TOML":        "[[waiver]\n",
		"unknown field":         "[[waiver]]\nfingerprint = \"" + fingerprintA + "\"\nreason = \"r\"\n" + approvedAt + "\nnote = \"extra\"\n",
		"invalid fingerprint":   "[[waiver]]\nfingerprint = \"nope\"\nreason = \"r\"\n" + approvedAt + "\n",
		"missing reason":        "[[waiver]]\nfingerprint = \"" + fingerprintA + "\"\nreason = \"\"\n" + approvedAt + "\n",
		"missing approval time": "[[waiver]]\nfingerprint = \"" + fingerprintA + "\"\nreason = \"r\"\n",
		"duplicate fingerprint": "[[waiver]]\nfingerprint = \"" + fingerprintA + "\"\nreason = \"r\"\n" + approvedAt + "\n" +
			"[[waiver]]\nfingerprint = \"" + fingerprintA + "\"\nreason = \"r\"\n" + approvedAt + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			if err := os.MkdirAll(store.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.Dir, "waivers.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(); err == nil {
				t.Fatalf("Load() accepted %s", name)
			}
			if _, _, err := store.Approve(fingerprintB, "approved"); err == nil {
				t.Fatalf("Approve() overwrote %s", name)
			}
		})
	}
}

func TestStoreRefusesAWaiverFileOutsideItsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixtures require POSIX")
	}
	store := newStore(t)
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(outside, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.Dir, "waivers.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() followed a symlink out of the state directory")
	}
	if _, _, err := store.Approve(fingerprintA, "approved"); err == nil {
		t.Fatal("Approve() followed a symlink out of the state directory")
	}
}

func TestApproveIsDurableAcrossStores(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Approve(fingerprintA, "approved once"); err != nil {
		t.Fatalf("Approve() = %v", err)
	}
	loaded, err := (waiver.Store{Dir: store.Dir}).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 || loaded[0].Reason != "approved once" {
		t.Fatalf("Load() = %#v, want the persisted approval", loaded)
	}
}

// A waiver is keyed by exactly what a gate's findings produce, so the
// identity a report prints is the identity an approval accepts.
func TestApproveAcceptsAProducedFingerprint(t *testing.T) {
	store := newStore(t)
	produced := finding.Fingerprint(finding.Finding{
		Gate:    "lint",
		RuleID:  "golangci-lint/errcheck",
		File:    "internal/run/run.go",
		Snippet: "value, _ := compute()",
	})

	record, created, err := store.Approve(produced, "the error is checked by the caller")
	if err != nil {
		t.Fatalf("Approve(%q) = %v", produced, err)
	}
	if !created || record.Fingerprint != produced {
		t.Fatalf("Approve() = %#v, created = %v", record, created)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 || loaded[0].Fingerprint != produced {
		t.Fatalf("Load() = %#v, want the produced fingerprint", loaded)
	}
}
