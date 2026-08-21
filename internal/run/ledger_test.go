package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
)

var fixedTime = time.Date(2026, time.August, 21, 15, 12, 30, 0, time.UTC)

func TestLedgerCreatesSortableRunAndAtomicReport(t *testing.T) {
	repoState := filepath.Join(t.TempDir(), "external", "repo-state")
	ledger := Ledger{
		RepoState: repoState,
		Now:       func() time.Time { return fixedTime },
		Random:    bytes.NewReader([]byte{0xa3, 0xf1}),
	}

	run, err := ledger.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := run.Close(); err != nil {
			t.Errorf("close run: %v", err)
		}
	})

	if got, want := filepath.Base(run.Dir), "20260821T151230Z-a3f1"; got != want {
		t.Fatalf("run ID = %q, want %q", got, want)
	}
	for _, path := range []string{
		repoState,
		filepath.Join(repoState, "runs"),
		run.Dir,
		filepath.Join(run.Dir, "raw"),
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode for %s = %04o, want 0700", path, got)
		}
	}

	report := Report{SchemaVersion: 1, RunID: filepath.Base(run.Dir)}
	if err := run.WriteReport(report); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(run.Dir, "report.json")
	info, err := os.Stat(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("report mode = %04o, want 0600", got)
	}
	contents, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(contents, []byte("\n")) {
		t.Error("report JSON does not end with a newline")
	}
	if matches, err := filepath.Glob(filepath.Join(run.Dir, ".report-*.tmp")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("atomic report left temporary files: %v", matches)
	}
}

func TestLedgerStartRejectsSymlinkedRepoState(t *testing.T) {
	root := t.TempDir()
	externalState := filepath.Join(root, "external-state")
	runsDir := filepath.Join(externalState, "runs")
	oldRun := filepath.Join(runsDir, "20260821T120000Z-0000")
	if err := os.MkdirAll(oldRun, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(oldRun, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoState := filepath.Join(root, "repo-state")
	if err := os.Symlink(externalState, repoState); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	run, err := (Ledger{
		RepoState: repoState,
		Keep:      1,
		Now:       func() time.Time { return fixedTime },
		Random:    bytes.NewReader([]byte{0xa3, 0xf1}),
	}).Start()
	if run != nil {
		_ = run.Close()
	}
	if err == nil {
		t.Fatal("Start succeeded through a symlinked repository state directory")
	}
	contents, readErr := os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("external sentinel was touched: %v", readErr)
	}
	if string(contents) != "keep" {
		t.Fatalf("external sentinel = %q, want keep", contents)
	}
}

func TestLedgerStartRejectsSymlinkedRunsDirectory(t *testing.T) {
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	if err := os.Mkdir(repoState, 0o700); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	oldRun := filepath.Join(externalRuns, "20260821T120000Z-0000")
	if err := os.MkdirAll(oldRun, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(oldRun, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalRuns, filepath.Join(repoState, "runs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	run, err := (Ledger{
		RepoState: repoState,
		Keep:      1,
		Now:       func() time.Time { return fixedTime },
		Random:    bytes.NewReader([]byte{0xa3, 0xf1}),
	}).Start()
	if run != nil {
		_ = run.Close()
	}
	if err == nil {
		t.Fatal("Start succeeded through a symlinked runs directory")
	}
	contents, readErr := os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("external sentinel was touched: %v", readErr)
	}
	if string(contents) != "keep" {
		t.Fatalf("external sentinel = %q, want keep", contents)
	}
}

func TestLedgerStartTightensExistingDirectoryPermissions(t *testing.T) {
	repoState := filepath.Join(t.TempDir(), "repo-state")
	runsDir := filepath.Join(repoState, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repoState, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	run, err := (Ledger{RepoState: repoState}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	for _, path := range []string{repoState, runsDir, run.Dir, filepath.Join(run.Dir, "raw")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode for %s = %04o, want 0700", path, got)
		}
	}
}

func TestLedgerRejectsConcurrentStart(t *testing.T) {
	repoState := t.TempDir()
	first, err := (Ledger{RepoState: repoState}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := (Ledger{RepoState: repoState}).Start()
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("concurrent Start error = %v, want ErrLocked", err)
	}
}

func TestLedgerRecoversStaleLock(t *testing.T) {
	repoState := t.TempDir()
	lockPath := filepath.Join(repoState, "lock")
	stale := []byte(`{"pid":2147483647,"start":"2026-08-20T12:00:00Z","token":"stale-owner"}` + "\n")
	if err := os.WriteFile(lockPath, stale, 0o600); err != nil {
		t.Fatal(err)
	}

	run, err := (Ledger{RepoState: repoState}).Start()
	if err != nil {
		t.Fatalf("Start with stale lock: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains after Close: %v", err)
	}
}

func TestLedgerLockRecordIdentifiesOwner(t *testing.T) {
	repoState := t.TempDir()
	run, err := (Ledger{RepoState: repoState, Now: func() time.Time { return fixedTime }}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	contents, err := os.ReadFile(filepath.Join(repoState, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		PID   int       `json:"pid"`
		Start time.Time `json:"start"`
		Token string    `json:"token"`
	}
	if err := json.Unmarshal(contents, &record); err != nil {
		t.Fatal(err)
	}
	if record.PID != os.Getpid() || !record.Start.Equal(fixedTime) || len(record.Token) != 32 {
		t.Fatalf("lock record = %+v", record)
	}
}

func TestLedgerRejectsUnsafeLockEntries(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "unrelated")
				if err := os.WriteFile(target, []byte("must remain"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repoState := t.TempDir()
			lockPath := filepath.Join(repoState, "lock")
			test.setup(t, lockPath)
			run, err := (Ledger{RepoState: repoState}).Start()
			if run != nil {
				_ = run.Close()
			}
			if !errors.Is(err, ErrInvalidLock) {
				t.Fatalf("Start error = %v, want ErrInvalidLock", err)
			}
			if _, err := os.Lstat(lockPath); err != nil {
				t.Fatalf("unsafe lock entry was removed: %v", err)
			}
		})
	}
}

func TestClosePreservesReplacementLock(t *testing.T) {
	repoState := t.TempDir()
	run, err := (Ledger{RepoState: repoState}).Start()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(repoState, "lock")
	replacement := []byte(`{"pid":1,"start":"2026-08-21T15:12:30Z","token":"replacement"}` + "\n")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run.Close(); !errors.Is(err, ErrLockOwnershipLost) {
		t.Fatalf("Close error = %v, want ErrLockOwnershipLost", err)
	}
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement lock changed: %q", got)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestLedgerReleasesLockWhenStartFails(t *testing.T) {
	repoState := t.TempDir()
	_, err := (Ledger{RepoState: repoState, Random: bytes.NewReader(nil)}).Start()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Start error = %v, want EOF", err)
	}
	if _, err := os.Lstat(filepath.Join(repoState, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains after failed Start: %v", err)
	}
}

func TestLedgerConcurrentStartRaceHasOneOwner(t *testing.T) {
	repoState := t.TempDir()
	const attempts = 20
	type result struct {
		run *RunLedger
		err error
	}
	start := make(chan struct{})
	results := make(chan result, attempts)
	var group sync.WaitGroup
	for range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			run, err := (Ledger{RepoState: repoState}).Start()
			results <- result{run: run, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	owners := 0
	var owner *RunLedger
	for result := range results {
		if result.err == nil {
			owners++
			owner = result.run
			continue
		}
		if !errors.Is(result.err, ErrLocked) {
			t.Errorf("losing Start error = %v, want ErrLocked", result.err)
		}
	}
	if owners != 1 {
		t.Fatalf("owners = %d, want 1", owners)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerPrunesBeforeCreatingRun(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var existing []string
	for index := range 25 {
		id := fmt.Sprintf("20260821T14%02d00Z-%04x", index, index)
		existing = append(existing, id)
		dir := filepath.Join(runsDir, id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if index%2 == 0 {
			if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	unexpected := filepath.Join(runsDir, "operator-notes")
	if err := os.Mkdir(unexpected, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repoState, "must-not-remove")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRun := filepath.Join(runsDir, "20260821T130000Z-dead")
	if err := os.Symlink(target, linkedRun); err != nil {
		t.Logf("symlink pruning coverage unavailable: %v", err)
		linkedRun = ""
	}

	ledger := Ledger{
		RepoState: repoState,
		Keep:      0,
		Now:       func() time.Time { return fixedTime },
		Random:    bytes.NewReader([]byte{0xa3, 0xf1}),
	}
	run, err := ledger.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	for _, id := range existing[:6] {
		if _, err := os.Stat(filepath.Join(runsDir, id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("old run %s remains: %v", id, err)
		}
	}
	for _, id := range existing[6:] {
		if _, err := os.Stat(filepath.Join(runsDir, id)); err != nil {
			t.Errorf("retained run %s: %v", id, err)
		}
	}
	if _, err := os.Stat(unexpected); err != nil {
		t.Errorf("unexpected entry was not preserved: %v", err)
	}
	if linkedRun != "" {
		if _, err := os.Lstat(linkedRun); err != nil {
			t.Errorf("run-shaped symlink was not preserved: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("symlink target was touched: %v", err)
		}
	}
}

func TestPruneRejectsReplacedRunsDirectory(t *testing.T) {
	root := t.TempDir()
	runsPath := filepath.Join(root, "runs")
	if err := os.Mkdir(runsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(runsPath)
	if err != nil {
		t.Fatal(err)
	}
	originalPath := filepath.Join(root, "original-runs")
	if err := os.Rename(runsPath, originalPath); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	externalOldRun := filepath.Join(externalRuns, "20260821T120000Z-0000")
	if err := os.MkdirAll(externalOldRun, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(externalOldRun, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalRuns, runsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err = pruneRuns(directoryBoundary{path: runsPath, identity: info}, 0)
	if err == nil {
		t.Fatal("pruneRuns succeeded after the runs directory was replaced")
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("external sentinel was touched: %v", err)
	}
	if string(contents) != "keep" {
		t.Fatalf("external sentinel = %q, want keep", contents)
	}
}

func TestLedgerReportsRunIDCollisionAndReleasesLock(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runID := "20260821T151230Z-a3f1"
	if err := os.Mkdir(filepath.Join(runsDir, runID), 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{
		RepoState: repoState,
		Now:       func() time.Time { return fixedTime },
		Random:    bytes.NewReader([]byte{0xa3, 0xf1}),
	}

	run, err := ledger.Start()
	if run != nil {
		_ = run.Close()
	}
	if !errors.Is(err, ErrRunIDCollision) {
		t.Fatalf("Start error = %v, want ErrRunIDCollision", err)
	}
	if _, err := os.Lstat(filepath.Join(repoState, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains after collision: %v", err)
	}
}

func TestWriteRawPreservesBytes(t *testing.T) {
	run, err := (Ledger{RepoState: t.TempDir()}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	raw := []byte{'o', 'u', 't', 0, '\n', 0xff}

	if err := run.WriteRaw("golangci-lint", "go", "stdout", raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(run.Dir, "raw", "golangci-lint.go.stdout")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("raw output = %v, want %v", got, raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("raw output mode = %04o, want 0600", got)
	}
	matches, err := filepath.Glob(filepath.Join(run.Dir, "raw", ".raw-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("raw output left temporary files: %v", matches)
	}
}

func TestWriteRawCapsOutputIncludingMarker(t *testing.T) {
	run, err := (Ledger{RepoState: t.TempDir()}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	const limit = 1 << 20
	marker := []byte("\n[togi: output truncated]\n")
	raw := bytes.Repeat([]byte("x"), limit+1)

	if err := run.WriteRaw("gocyclo", "go", "stderr", raw); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(run.Dir, "raw", "gocyclo.go.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != limit {
		t.Fatalf("capped output length = %d, want %d", len(got), limit)
	}
	if !bytes.HasSuffix(got, marker) {
		t.Fatalf("capped output does not end in marker: %q", got[len(got)-len(marker):])
	}
	if !bytes.Equal(got[:limit-len(marker)], raw[:limit-len(marker)]) {
		t.Error("capped output prefix changed")
	}

	exact := bytes.Repeat([]byte("y"), limit)
	if err := run.WriteRaw("gocyclo", "go", "stdout", exact); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(run.Dir, "raw", "gocyclo.go.stdout"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, exact) {
		t.Error("output at the cap was changed")
	}
}

func TestWriteRawRejectsUnsafeNames(t *testing.T) {
	run, err := (Ledger{RepoState: t.TempDir()}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	for _, test := range []struct {
		gate     string
		language string
		stream   string
	}{
		{gate: "", language: "go", stream: "stdout"},
		{gate: "../escape", language: "go", stream: "stdout"},
		{gate: "gate/name", language: "go", stream: "stdout"},
		{gate: `gate\name`, language: "go", stream: "stdout"},
		{gate: "gate.name", language: "go", stream: "stdout"},
		{gate: "gate name", language: "go", stream: "stdout"},
		{gate: "CON", language: "go", stream: "stdout"},
		{gate: "gate", language: "../go", stream: "stdout"},
		{gate: "gate", language: "go", stream: "output"},
	} {
		if err := run.WriteRaw(test.gate, test.language, test.stream, []byte("unsafe")); err == nil {
			t.Errorf("WriteRaw(%q, %q, %q) succeeded", test.gate, test.language, test.stream)
		}
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "escape.go.stdout")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("traversal output exists outside raw directory: %v", err)
	}
}

func TestRunLedgerRejectsWritesAfterClose(t *testing.T) {
	run, err := (Ledger{RepoState: t.TempDir()}).Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run.WriteRaw("gate", "go", "stdout", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteRaw after Close = %v, want ErrClosed", err)
	}
	if err := run.WriteReport(Report{SchemaVersion: 1}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteReport after Close = %v, want ErrClosed", err)
	}
}

func TestLatestReturnsNewestParseableCompleteReport(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wanted := Report{
		SchemaVersion: 1,
		RunID:         "20260821T120200Z-0002",
		RepoID:        "repo-id",
		StartedAt:     time.Date(2026, time.August, 21, 12, 2, 0, 0, time.UTC),
		FinishedAt:    time.Date(2026, time.August, 21, 12, 2, 5, 0, time.UTC),
		Verdict:       VerdictErrored,
		Gates: []GateReport{{
			Gate:       "lint",
			Language:   "go",
			Status:     GateErrored,
			DurationMS: 5000,
			Error:      "tool missing",
		}},
		Findings: []finding.Finding{},
		Counts:   Counts{},
	}
	writeReportFixture(t, runsDir, Report{SchemaVersion: 1, RunID: "20260821T120000Z-0000"})
	malformedDir := filepath.Join(runsDir, "20260821T120100Z-0001")
	if err := os.Mkdir(malformedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformedDir, "report.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeReportFixture(t, runsDir, wanted)
	if err := os.Mkdir(filepath.Join(runsDir, "20260821T120300Z-0003"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkReportDir := filepath.Join(runsDir, "20260821T120400Z-0004")
	if err := os.Mkdir(symlinkReportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideReport, err := json.Marshal(Report{SchemaVersion: 1, RunID: "20260821T120400Z-0004"})
	if err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(repoState, "outside-report.json")
	if err := os.WriteFile(outsidePath, outsideReport, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(symlinkReportDir, "report.json")); err != nil {
		t.Logf("report symlink coverage unavailable: %v", err)
	}
	outsideRun := filepath.Join(repoState, "outside-run")
	if err := os.Mkdir(outsideRun, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideRun, filepath.Join(runsDir, "20260821T120500Z-0005")); err != nil {
		t.Logf("run symlink coverage unavailable: %v", err)
	}

	got, err := (Ledger{RepoState: repoState}).Latest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("Latest() = %#v, want %#v", got, wanted)
	}
}

func TestLatestRejectsSymlinkedRepoState(t *testing.T) {
	root := t.TempDir()
	externalState := filepath.Join(root, "external-state")
	runsDir := filepath.Join(externalState, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := Report{SchemaVersion: 1, RunID: "20260821T120000Z-0000"}
	writeReportFixture(t, runsDir, report)
	reportPath := filepath.Join(runsDir, report.RunID, "report.json")
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	repoState := filepath.Join(root, "repo-state")
	if err := os.Symlink(externalState, repoState); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := (Ledger{RepoState: repoState}).Latest(); err == nil {
		t.Fatal("Latest succeeded through a symlinked repository state directory")
	}
	after, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("external report was touched: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("external report changed")
	}
}

func TestLatestRejectsSymlinkedRunsDirectory(t *testing.T) {
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	if err := os.Mkdir(repoState, 0o700); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	if err := os.Mkdir(externalRuns, 0o700); err != nil {
		t.Fatal(err)
	}
	report := Report{SchemaVersion: 1, RunID: "20260821T120000Z-0000"}
	writeReportFixture(t, externalRuns, report)
	reportPath := filepath.Join(externalRuns, report.RunID, "report.json")
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalRuns, filepath.Join(repoState, "runs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := (Ledger{RepoState: repoState}).Latest(); err == nil {
		t.Fatal("Latest succeeded through a symlinked runs directory")
	}
	after, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("external report was touched: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("external report changed")
	}
}

func TestLatestTightensExistingDirectoryPermissions(t *testing.T) {
	repoState := filepath.Join(t.TempDir(), "repo-state")
	runsDir := filepath.Join(repoState, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repoState, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := (Ledger{RepoState: repoState}).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
	for _, path := range []string{repoState, runsDir} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode for %s = %04o, want 0700", path, got)
		}
	}
}

func TestLatestReturnsSentinelWithoutCompleteRuns(t *testing.T) {
	repoState := t.TempDir()
	if _, err := (Ledger{RepoState: repoState}).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
}

func TestWriteReportFillsRunIDAndRejectsDuplicate(t *testing.T) {
	run, err := (Ledger{RepoState: t.TempDir()}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	first := Report{SchemaVersion: 1, Verdict: VerdictUnverified}
	if err := run.WriteReport(first); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(run.Dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Report
	if err := json.Unmarshal(contents, &persisted); err != nil {
		t.Fatal(err)
	}
	if got, want := persisted.RunID, filepath.Base(run.Dir); got != want {
		t.Fatalf("persisted run ID = %q, want %q", got, want)
	}

	if err := run.WriteReport(Report{SchemaVersion: 1, Verdict: VerdictErrored}); !errors.Is(err, ErrReportExists) {
		t.Fatalf("duplicate WriteReport error = %v, want ErrReportExists", err)
	}
	after, err := os.ReadFile(filepath.Join(run.Dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, contents) {
		t.Error("duplicate WriteReport changed the completed report")
	}
}

func TestWriteReportRejectsInvalidReportsWithoutArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		report func(runID string) Report
	}{
		{name: "schema", report: func(string) Report { return Report{SchemaVersion: 2} }},
		{name: "run ID", report: func(string) Report { return Report{SchemaVersion: 1, RunID: "wrong"} }},
		{name: "verdict", report: func(string) Report { return Report{SchemaVersion: 1, Verdict: "unknown"} }},
		{name: "timestamps", report: func(string) Report {
			return Report{SchemaVersion: 1, StartedAt: fixedTime, FinishedAt: fixedTime.Add(-time.Second)}
		}},
		{name: "counts", report: func(string) Report {
			return Report{SchemaVersion: 1, Counts: Counts{Errors: -1}}
		}},
		{name: "gate status", report: func(string) Report {
			return Report{SchemaVersion: 1, Gates: []GateReport{{Status: "unknown"}}}
		}},
		{name: "gate duration", report: func(string) Report {
			return Report{SchemaVersion: 1, Gates: []GateReport{{DurationMS: -1}}}
		}},
		{name: "finding", report: func(string) Report {
			return Report{SchemaVersion: 1, Findings: []finding.Finding{{Gate: "lint"}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, err := (Ledger{RepoState: t.TempDir()}).Start()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = run.Close() })
			if err := run.WriteReport(test.report(filepath.Base(run.Dir))); err == nil {
				t.Fatal("WriteReport succeeded")
			}
			if _, err := os.Stat(filepath.Join(run.Dir, "report.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid report artifact exists: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(run.Dir, ".report-*.tmp"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("invalid report left temporary files: %v", matches)
			}
		})
	}
}

func TestWriteReportCleansTemporaryFileAfterEncodingError(t *testing.T) {
	run, err := (Ledger{RepoState: t.TempDir()}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	report := Report{SchemaVersion: 1, StartedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := run.WriteReport(report); err == nil {
		t.Fatal("WriteReport succeeded with a non-JSON timestamp")
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial report exists: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(run.Dir, ".report-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("encoding error left temporary files: %v", matches)
	}
}

func writeReportFixture(t *testing.T, runsDir string, report Report) {
	t.Helper()
	runDir := filepath.Join(runsDir, report.RunID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "report.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
