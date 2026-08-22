package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
)

var fixedTime = time.Date(2026, time.August, 21, 15, 12, 30, 123456789, time.UTC)

const ledgerTestRepoID = "dddddddddddddddddddddddddddddddddddddddd"

func testLedger(repoState string) Ledger {
	return Ledger{RepoID: ledgerTestRepoID, RepoState: repoState, RunsDir: filepath.Join(repoState, "runs")}
}

func TestLedgerCreatesSortableRunAndAtomicReport(t *testing.T) {
	repoState := filepath.Join(t.TempDir(), "external", "repo-state")
	ledger := Ledger{RepoID: ledgerTestRepoID,
		RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"),
		Now:    func() time.Time { return fixedTime },
		Random: bytes.NewReader([]byte{0xa3, 0xf1}),
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

	if got, want := filepath.Base(run.Dir), "20260821T151230.123456789Z-a3f1"; got != want {
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
		if !privateDirectoryMode(info.Mode()) {
			t.Errorf("mode for %s is not private: %04o", path, info.Mode().Perm())
		}
	}

	report := completeReportFixture(filepath.Base(run.Dir))
	if err := run.WriteReport(report); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(run.Dir, "report.json")
	info, err := os.Stat(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !privateFileMode(info.Mode()) {
		t.Errorf("report mode is not private: %04o", info.Mode().Perm())
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

func TestLedgerUsesExplicitRunsDirectory(t *testing.T) {
	repoState := filepath.Join(t.TempDir(), "repo-state")
	runsDir := filepath.Join(repoState, "custom-runs")
	run, err := (Ledger{RepoID: ledgerTestRepoID, RepoState: repoState, RunsDir: runsDir}).Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if filepath.Dir(run.Dir) != runsDir {
		t.Fatalf("run directory = %q, want child of %q", run.Dir, runsDir)
	}
	if _, err := os.Stat(filepath.Join(repoState, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ledger created implicit runs directory: %v", err)
	}
}

func TestLedgerStartRejectsSymlinkedRepoState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement safety is covered by sharing-violation tests")
	}
	root := t.TempDir()
	externalState := filepath.Join(root, "external-state")
	runsDir := filepath.Join(externalState, "runs")
	oldRun := filepath.Join(runsDir, "20260821T120000.000000000Z-0000")
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

	run, err := (Ledger{RepoID: ledgerTestRepoID,
		RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"),
		Keep:   1,
		Now:    func() time.Time { return fixedTime },
		Random: bytes.NewReader([]byte{0xa3, 0xf1}),
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement safety is covered by sharing-violation tests")
	}
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	if err := os.Mkdir(repoState, 0o700); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	oldRun := filepath.Join(externalRuns, "20260821T120000.000000000Z-0000")
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

	run, err := (Ledger{RepoID: ledgerTestRepoID,
		RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"),
		Keep:   1,
		Now:    func() time.Time { return fixedTime },
		Random: bytes.NewReader([]byte{0xa3, 0xf1}),
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

	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	for _, path := range []string{repoState, runsDir, run.Dir, filepath.Join(run.Dir, "raw")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !privateDirectoryMode(info.Mode()) {
			t.Errorf("mode for %s is not private: %04o", path, info.Mode().Perm())
		}
	}
}

func TestLedgerRejectsConcurrentStart(t *testing.T) {
	repoState := t.TempDir()
	first, err := (Ledger{RepoID: ledgerTestRepoID, RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"), Now: func() time.Time { return fixedTime }}).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	lockPath := filepath.Join(repoState, "lock")
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	second, err := (Ledger{RepoID: ledgerTestRepoID,
		RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"),
		Now: func() time.Time { return fixedTime.Add(time.Hour) },
	}).Start()
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("concurrent Start error = %v, want ErrLocked", err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("losing contender changed lock record:\nbefore: %s\nafter: %s", before, after)
	}
}

func TestProcessLockClaimRequiresMatchingOwner(t *testing.T) {
	repoState := t.TempDir()
	identity, err := os.Stat(repoState)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(repoState, "lock")
	owner, err := claimProcessLock(key, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseProcessLock(owner) })

	if _, err := claimProcessLock(key, identity); !errors.Is(err, ErrLocked) {
		t.Fatalf("second claim error = %v, want ErrLocked", err)
	}
	if _, err := claimProcessLock(key+".renamed", identity); !errors.Is(err, ErrLocked) {
		t.Fatalf("same identity under another path = %v, want ErrLocked", err)
	}
	releaseProcessLock(&processLockClaim{key: key, identity: identity})
	if _, err := claimProcessLock(key, identity); !errors.Is(err, ErrLocked) {
		t.Fatalf("claim after foreign release = %v, want ErrLocked", err)
	}

	releaseProcessLock(owner)
	next, err := claimProcessLock(key, identity)
	if err != nil {
		t.Fatalf("claim after owner release: %v", err)
	}
	releaseProcessLock(next)
}

func TestProcessLockClaimConcurrentContentionHasOneOwner(t *testing.T) {
	repoState := t.TempDir()
	identity, err := os.Stat(repoState)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(repoState, "lock")
	const attempts = 20
	start := make(chan struct{})
	results := make(chan *processLockClaim, attempts)
	var group sync.WaitGroup
	for range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			claim, _ := claimProcessLock(key, identity)
			results <- claim
		}()
	}
	close(start)
	group.Wait()
	close(results)

	var owner *processLockClaim
	winners := 0
	for claim := range results {
		if claim != nil {
			winners++
			owner = claim
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners = %d, want 1", winners)
	}
	releaseProcessLock(owner)
}

func TestLedgerAcquiresUnlockedPersistentLockRegardlessOfRecord(t *testing.T) {
	repoState := t.TempDir()
	lockPath := filepath.Join(repoState, "lock")
	stale := fmt.Sprintf(`{"pid":%d,"start":"2026-08-20T12:00:00Z","token":"stale-owner"}`+"\n", os.Getpid())
	if err := os.WriteFile(lockPath, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{RepoID: ledgerTestRepoID,
		RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"),
		Now:    func() time.Time { return fixedTime },
		Random: bytes.NewReader([]byte{0xa3, 0xf1}),
	}

	run, err := ledger.Start()
	if err != nil {
		t.Fatalf("Start with unlocked stale record: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("persistent lock file missing after Close: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("lock mode = %v, want regular file", info.Mode())
	}
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, []byte("stale-owner")) {
		t.Fatalf("stale lock record was not replaced: %s", contents)
	}
}

func TestLedgerTightensPersistentLockPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows privacy is enforced by inherited ACLs")
	}
	repoState := t.TempDir()
	lockPath := filepath.Join(repoState, "lock")
	if err := os.WriteFile(lockPath, []byte("stale record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath, 0o644); err != nil {
		t.Fatal(err)
	}

	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("lock permissions = %04o, want %04o", got, want)
	}
}

func TestAdvisoryLockRemainsAnchoredAfterRepoStateReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows denies replacement while the root handle is open")
	}
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	if err := os.Mkdir(repoState, 0o700); err != nil {
		t.Fatal(err)
	}
	repoRoot, err := os.OpenRoot(repoState)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repoRoot.Close() }()
	movedState := filepath.Join(root, "moved-state")
	if err := os.Rename(repoState, movedState); err != nil {
		t.Fatal(err)
	}
	externalState := filepath.Join(root, "external-state")
	if err := os.Mkdir(externalState, 0o700); err != nil {
		t.Fatal(err)
	}
	externalLock := filepath.Join(externalState, "lock")
	if err := os.WriteFile(externalLock, []byte("external sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalState, repoState); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	lock, err := acquireStateLock(repoRoot, fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(movedState, "lock")); err != nil {
		t.Fatalf("anchored lock missing: %v", err)
	}
	contents, err := os.ReadFile(externalLock)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "external sentinel" {
		t.Fatalf("external lock changed: %q", contents)
	}
}

func TestEnsureRunsDirectoryRemainsAnchoredAfterRepoStateReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows denies replacement while the root handle is open")
	}
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	if err := os.Mkdir(repoState, 0o700); err != nil {
		t.Fatal(err)
	}
	repoBoundary, err := existingDirectory(repoState)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := openBoundaryRoot(repoBoundary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repoRoot.Close() }()

	movedState := filepath.Join(root, "moved-state")
	if err := os.Rename(repoState, movedState); err != nil {
		t.Fatal(err)
	}
	externalState := filepath.Join(root, "external-state")
	if err := os.Mkdir(externalState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalState, repoState); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ensureChildDirectoryAt(repoRoot, "runs", filepath.Join(repoState, "runs")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(movedState, "runs")); err != nil {
		t.Fatalf("anchored runs directory missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(externalState, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement tree was modified: %v", err)
	}
}

func TestLedgerOverwritesStaleLockRecord(t *testing.T) {
	repoState := t.TempDir()
	lockPath := filepath.Join(repoState, "lock")
	stale := []byte(`{"pid":2147483647,"start":"2026-08-20T12:00:00Z","token":"stale-owner"}` + "\n")
	if err := os.WriteFile(lockPath, stale, 0o600); err != nil {
		t.Fatal(err)
	}

	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatalf("Start with stale lock: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("persistent lock missing after Close: %v", err)
	}
	if bytes.Contains(contents, []byte("stale-owner")) {
		t.Fatalf("stale lock record remains: %s", contents)
	}
}

func TestLedgerLockRecordIdentifiesOwner(t *testing.T) {
	repoState := t.TempDir()
	run, err := (Ledger{RepoID: ledgerTestRepoID, RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"), Now: func() time.Time { return fixedTime }}).Start()
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
			run, err := (testLedger(repoState)).Start()
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows denies lock unlink while its handle is open")
	}
	repoState := t.TempDir()
	run, err := (testLedger(repoState)).Start()
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

	if err := run.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
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
	_, err := (Ledger{RepoID: ledgerTestRepoID, RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"), Random: bytes.NewReader(nil)}).Start()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Start error = %v, want EOF", err)
	}
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatalf("lock remained held after failed Start: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(repoState, "lock")); err != nil {
		t.Fatalf("persistent lock missing after failed Start recovery: %v", err)
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
			run, err := (testLedger(repoState)).Start()
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

func TestLedgerAdvisoryLockReleasesOnProcessExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repoState := t.TempDir()
	command := exec.Command(executable, "-test.run=^TestLedgerLockCrashHelper$")
	command.Env = append(os.Environ(),
		"TOGI_LOCK_CRASH_HELPER=1",
		"TOGI_LOCK_CRASH_STATE="+repoState,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v\n%s", err, output)
	}

	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatalf("Start after lock owner process exited: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerLockCrashHelper(t *testing.T) {
	if os.Getenv("TOGI_LOCK_CRASH_HELPER") != "1" {
		return
	}
	if _, err := (Ledger{RepoID: ledgerTestRepoID, RepoState: os.Getenv("TOGI_LOCK_CRASH_STATE"), RunsDir: filepath.Join(os.Getenv("TOGI_LOCK_CRASH_STATE"), "runs")}).Start(); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestLedgerPrunesBeforeCreatingRun(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := createPrunableRuns(t, runsDir)
	unexpected := filepath.Join(runsDir, "operator-notes")
	if err := os.Mkdir(unexpected, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repoState, "must-not-remove")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRun := createRunSymlink(t, runsDir, target)

	ledger := Ledger{RepoID: ledgerTestRepoID, RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"), Keep: 0, Now: func() time.Time { return fixedTime }, Random: bytes.NewReader([]byte{0xa3, 0xf1})}
	run, err := ledger.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	assertPrunedRuns(t, runsDir, existing, unexpected, linkedRun, target)
}

func createPrunableRuns(t *testing.T, runsDir string) []string {
	t.Helper()
	var existing []string
	for index := range 25 {
		id := fmt.Sprintf("20260821T14%02d00.000000000Z-%04x", index, index)
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
	return existing
}

func createRunSymlink(t *testing.T, runsDir, target string) string {
	t.Helper()
	linkedRun := ""
	if runtime.GOOS != "windows" {
		linkedRun = filepath.Join(runsDir, "20260821T130000.000000000Z-dead")
		if err := os.Symlink(target, linkedRun); err != nil {
			t.Logf("symlink pruning coverage unavailable: %v", err)
			linkedRun = ""
		}
	}
	return linkedRun
}

func assertPrunedRuns(t *testing.T, runsDir string, existing []string, unexpected, linkedRun, target string) {
	t.Helper()
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

func TestPruneRemainsAnchoredAfterRunsDirectoryReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows denies replacement while the root handle is open")
	}
	root := t.TempDir()
	runsPath := filepath.Join(root, "runs")
	originalOldRun := filepath.Join(runsPath, "20260821T110000.000000000Z-0000")
	if err := os.MkdirAll(originalOldRun, 0o700); err != nil {
		t.Fatal(err)
	}
	runsRoot, err := os.OpenRoot(runsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runsRoot.Close() }()
	originalPath := filepath.Join(root, "original-runs")
	if err := os.Rename(runsPath, originalPath); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	externalOldRun := filepath.Join(externalRuns, "20260821T120000.000000000Z-0000")
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

	if err := pruneRuns(runsRoot, 0); err != nil {
		t.Fatalf("pruneRuns through retained root: %v", err)
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("external sentinel was touched: %v", err)
	}
	if string(contents) != "keep" {
		t.Fatalf("external sentinel = %q, want keep", contents)
	}
	if _, err := os.Stat(filepath.Join(originalPath, filepath.Base(originalOldRun))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original old run remains after pruning: %v", err)
	}
}

func TestLedgerReportsRunIDCollisionAndReleasesLock(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runID := "20260821T151230.123456789Z-a3f1"
	if err := os.Mkdir(filepath.Join(runsDir, runID), 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{RepoID: ledgerTestRepoID,
		RepoState: repoState, RunsDir: filepath.Join(repoState, "runs"),
		Now:    func() time.Time { return fixedTime },
		Random: bytes.NewReader([]byte{0xa3, 0xf1}),
	}

	run, err := ledger.Start()
	if run != nil {
		_ = run.Close()
	}
	if !errors.Is(err, ErrRunIDCollision) {
		t.Fatalf("Start error = %v, want ErrRunIDCollision", err)
	}
	if _, err := os.Lstat(filepath.Join(repoState, "lock")); err != nil {
		t.Fatalf("persistent lock missing after collision: %v", err)
	}
	next, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatalf("lock remained held after collision: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRawPreservesBytes(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
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
	if !privateFileMode(info.Mode()) {
		t.Errorf("raw output mode is not private: %04o", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(run.Dir, "raw", ".raw-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("raw output left temporary files: %v", matches)
	}
}

func TestRunLedgerWritesRemainAnchoredAfterRepoStateReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows denies replacement while ledger handles are open")
	}
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	runID := filepath.Base(run.Dir)
	movedState := filepath.Join(root, "moved-state")
	if err := os.Rename(repoState, movedState); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	externalState := filepath.Join(root, "external-state")
	externalRun := filepath.Join(externalState, "runs", runID)
	if err := os.MkdirAll(filepath.Join(externalRun, "raw"), 0o700); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	if err := os.Symlink(externalState, repoState); err != nil {
		_ = run.Close()
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := run.WriteRaw("gate", "go", "stdout", []byte("anchored")); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	if err := run.WriteReport(completeReportFixture(runID)); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	anchoredRun := filepath.Join(movedState, "runs", runID)
	if got, err := os.ReadFile(filepath.Join(anchoredRun, "raw", "gate.go.stdout")); err != nil {
		t.Fatalf("read anchored raw output: %v", err)
	} else if string(got) != "anchored" {
		t.Fatalf("anchored raw output = %q", got)
	}
	if _, err := os.Stat(filepath.Join(anchoredRun, "report.json")); err != nil {
		t.Fatalf("anchored report missing: %v", err)
	}
	for _, path := range []string{
		filepath.Join(externalRun, "raw", "gate.go.stdout"),
		filepath.Join(externalRun, "report.json"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact escaped to replacement tree %s: %v", path, err)
		}
	}
}

func TestWriteRawCapsOutputIncludingMarker(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
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
	run, err := (testLedger(t.TempDir())).Start()
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
	run, err := (testLedger(t.TempDir())).Start()
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

func TestRunLedgerRejectsUninitializedValue(t *testing.T) {
	var run RunLedger
	if err := run.WriteRaw("gate", "go", "stdout", nil); !errors.Is(err, ErrUninitialized) {
		t.Errorf("WriteRaw on zero value = %v, want ErrUninitialized", err)
	}
	if err := run.WriteReport(Report{SchemaVersion: 1}); !errors.Is(err, ErrUninitialized) {
		t.Errorf("WriteReport on zero value = %v, want ErrUninitialized", err)
	}
	if err := run.Close(); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Close on zero value = %v, want ErrUninitialized", err)
	}
}

func TestRunLedgerRejectsCopiedValue(t *testing.T) {
	repoState := t.TempDir()
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	copied := RunLedger{
		Dir:      run.Dir,
		runID:    run.runID,
		lock:     run.lock,
		repoRoot: run.repoRoot,
		runsRoot: run.runsRoot,
		runRoot:  run.runRoot,
		rawRoot:  run.rawRoot,
		marker:   run,
	}

	if err := copied.WriteRaw("gate", "go", "stdout", nil); !errors.Is(err, ErrUninitialized) {
		t.Errorf("WriteRaw on copied value = %v, want ErrUninitialized", err)
	}
	if err := copied.WriteReport(Report{SchemaVersion: 1}); !errors.Is(err, ErrUninitialized) {
		t.Errorf("WriteReport on copied value = %v, want ErrUninitialized", err)
	}
	if err := copied.Close(); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Close on copied value = %v, want ErrUninitialized", err)
	}
	if next, err := (testLedger(repoState)).Start(); !errors.Is(err, ErrLocked) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("Start after copied Close = %v, want ErrLocked", err)
	}
}

func TestZeroRunLedgerCannotReleaseProcessClaim(t *testing.T) {
	repoState := t.TempDir()
	run, err := (testLedger(repoState)).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	var zero RunLedger
	if err := zero.Close(); !errors.Is(err, ErrUninitialized) {
		t.Fatalf("zero Close = %v, want ErrUninitialized", err)
	}
	if next, err := (testLedger(repoState)).Start(); !errors.Is(err, ErrLocked) {
		if next != nil {
			_ = next.Close()
		}
		t.Fatalf("Start after zero Close = %v, want ErrLocked", err)
	}
}

func TestLatestReturnsNewestParseableCompleteReport(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wanted := Report{
		SchemaVersion: 2,
		RunID:         "20260821T120200.000000000Z-0002",
		RepoID:        strings.Repeat("d", 40),
		Diff:          completeReportFixture("").Diff,
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
	writeReportFixture(t, runsDir, Report{SchemaVersion: 1, RunID: "20260821T120000.000000000Z-0000"})
	malformedDir := filepath.Join(runsDir, "20260821T120100.000000000Z-0001")
	if err := os.Mkdir(malformedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformedDir, "report.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeReportFixture(t, runsDir, wanted)
	if err := os.Mkdir(filepath.Join(runsDir, "20260821T120300.000000000Z-0003"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkReportDir := filepath.Join(runsDir, "20260821T120400.000000000Z-0004")
	if err := os.Mkdir(symlinkReportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideReport, err := json.Marshal(Report{SchemaVersion: 1, RunID: "20260821T120400.000000000Z-0004"})
	if err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(repoState, "outside-report.json")
	if err := os.WriteFile(outsidePath, outsideReport, 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outsidePath, filepath.Join(symlinkReportDir, "report.json")); err != nil {
			t.Logf("report symlink coverage unavailable: %v", err)
		}
		outsideRun := filepath.Join(repoState, "outside-run")
		if err := os.Mkdir(outsideRun, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideRun, filepath.Join(runsDir, "20260821T120500.000000000Z-0005")); err != nil {
			t.Logf("run symlink coverage unavailable: %v", err)
		}
	}

	got, err := (testLedger(repoState)).Latest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("Latest() = %#v, want %#v", got, wanted)
	}
}

func TestLatestSortsSameSecondRunsByNanoseconds(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	older := Report{SchemaVersion: 2, RunID: "20260821T120000.100000000Z-ffff"}
	newer := Report{SchemaVersion: 2, RunID: "20260821T120000.900000000Z-0000"}
	writeReportFixture(t, runsDir, newer)
	writeReportFixture(t, runsDir, older)

	got, err := (testLedger(repoState)).Latest()
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != newer.RunID {
		t.Fatalf("Latest run ID = %q, want %q", got.RunID, newer.RunID)
	}
}

func TestLatestReadRemainsAnchoredAfterRunsDirectoryReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows denies replacement while the root handle is open")
	}
	root := t.TempDir()
	runsPath := filepath.Join(root, "runs")
	if err := os.Mkdir(runsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	wanted := Report{SchemaVersion: 2, RunID: "20260821T120000.100000000Z-0000"}
	writeReportFixture(t, runsPath, wanted)
	runsRoot, err := os.OpenRoot(runsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runsRoot.Close() }()
	originalPath := filepath.Join(root, "original-runs")
	if err := os.Rename(runsPath, originalPath); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	if err := os.Mkdir(externalRuns, 0o700); err != nil {
		t.Fatal(err)
	}
	writeReportFixture(t, externalRuns, completeReportFixture("20260821T120000.900000000Z-ffff"))
	if err := os.Symlink(externalRuns, runsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := latestFromRunsRoot(runsRoot, ledgerTestRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != wanted.RunID {
		t.Fatalf("anchored Latest run ID = %q, want %q", got.RunID, wanted.RunID)
	}
}

func TestLatestRejectsSymlinkedRepoState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement safety is covered by sharing-violation tests")
	}
	root := t.TempDir()
	externalState := filepath.Join(root, "external-state")
	runsDir := filepath.Join(externalState, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := completeReportFixture("20260821T120000.000000000Z-0000")
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

	if _, err := (testLedger(repoState)).Latest(); err == nil {
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement safety is covered by sharing-violation tests")
	}
	root := t.TempDir()
	repoState := filepath.Join(root, "repo-state")
	if err := os.Mkdir(repoState, 0o700); err != nil {
		t.Fatal(err)
	}
	externalRuns := filepath.Join(root, "external-runs")
	if err := os.Mkdir(externalRuns, 0o700); err != nil {
		t.Fatal(err)
	}
	report := completeReportFixture("20260821T120000.000000000Z-0000")
	writeReportFixture(t, externalRuns, report)
	reportPath := filepath.Join(externalRuns, report.RunID, "report.json")
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalRuns, filepath.Join(repoState, "runs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := (testLedger(repoState)).Latest(); err == nil {
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

	if _, err := (testLedger(repoState)).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
	for _, path := range []string{repoState, runsDir} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !privateDirectoryMode(info.Mode()) {
			t.Errorf("mode for %s is not private: %04o", path, info.Mode().Perm())
		}
	}
}

func TestLatestReturnsSentinelWithoutCompleteRuns(t *testing.T) {
	repoState := t.TempDir()
	if _, err := (testLedger(repoState)).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
}

func TestWriteReportFillsRunIDAndRejectsDuplicate(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	first := completeReportFixture("")
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

	if err := run.WriteReport(completeReportFixture("")); !errors.Is(err, ErrReportExists) {
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

func TestWriteReportRejectsDifferentRepositoryIdentity(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	report := completeReportFixture("")
	report.RepoID = strings.Repeat("e", 40)
	if err := run.WriteReport(report); err == nil || !strings.Contains(err.Error(), "repository ID") {
		t.Fatalf("WriteReport error = %v, want repository identity mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched report artifact exists: %v", err)
	}
}

func TestLatestSkipsReportForDifferentRepositoryIdentity(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := completeReportFixture("20260821T120000.000000000Z-0000")
	report.RepoID = strings.Repeat("e", 40)
	writeReportFixture(t, runsDir, report)
	if _, err := (testLedger(repoState)).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
}

func TestPublishNoReplaceAllowsExactlyOneConcurrentWinner(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	first := []byte("first complete report\n")
	second := []byte("second complete report\n")
	if err := root.WriteFile("first.tmp", first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("second.tmp", second, 0o600); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, source := range []string{"first.tmp", "second.tmp"} {
		go func() {
			<-start
			results <- publishNoReplace(root, source, "report.json")
		}()
	}
	close(start)
	firstErr := <-results
	secondErr := <-results
	close(results)
	winners := 0
	losers := 0
	for _, err := range []error{firstErr, secondErr} {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrReportExists):
			losers++
		default:
			t.Fatalf("publish error = %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("publish results: winners=%d losers=%d", winners, losers)
	}
	contents, err := root.ReadFile("report.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, first) && !bytes.Equal(contents, second) {
		t.Fatalf("published partial or unexpected report: %q", contents)
	}
}

func TestWriteReportRejectsInvalidReportsWithoutArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		report func(runID string) Report
	}{
		{name: "schema", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.SchemaVersion = 1
			return report
		}},
		{name: "future schema", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.SchemaVersion = 3
			return report
		}},
		{name: "run ID", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.RunID = "wrong"
			return report
		}},
		{name: "verdict", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.Verdict = "unknown"
			return report
		}},
		{name: "timestamps", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.StartedAt = fixedTime
			report.FinishedAt = fixedTime.Add(-time.Second)
			return report
		}},
		{name: "counts", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.Counts.Errors = -1
			return report
		}},
		{name: "gate status", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.Gates[0].Status = "unknown"
			return report
		}},
		{name: "gate duration", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.Gates[0].DurationMS = -1
			return report
		}},
		{name: "finding", report: func(runID string) Report {
			report := completeReportFixture(runID)
			report.Findings = []finding.Finding{{Gate: "lint"}}
			return report
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, err := (testLedger(t.TempDir())).Start()
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

func TestValidateReportRejectsMissingAndMalformedDiffMetadata(t *testing.T) {
	valid := completeReportFixture("20260821T120000.000000000Z-0000")
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "missing base ref", mutate: func(report *Report) { report.Diff.BaseRef = "" }},
		{name: "blank base ref", mutate: func(report *Report) { report.Diff.BaseRef = " \t" }},
		{name: "control base ref", mutate: func(report *Report) { report.Diff.BaseRef = "main\nbranch" }},
		{name: "unicode control base ref", mutate: func(report *Report) { report.Diff.BaseRef = "main\u0085branch" }},
		{name: "missing base commit", mutate: func(report *Report) { report.Diff.BaseCommit = "" }},
		{name: "malformed base commit", mutate: func(report *Report) { report.Diff.BaseCommit = strings.Repeat("a", 39) }},
		{name: "base commit trailing newline", mutate: func(report *Report) { report.Diff.BaseCommit += "\n" }},
		{name: "base commit surrounding whitespace", mutate: func(report *Report) { report.Diff.BaseCommit = " " + report.Diff.BaseCommit + " " }},
		{name: "missing merge base", mutate: func(report *Report) { report.Diff.MergeBase = "" }},
		{name: "malformed merge base", mutate: func(report *Report) { report.Diff.MergeBase = strings.ToUpper(report.Diff.MergeBase) }},
		{name: "merge base trailing newline", mutate: func(report *Report) { report.Diff.MergeBase += "\n" }},
		{name: "merge base surrounding whitespace", mutate: func(report *Report) { report.Diff.MergeBase = " " + report.Diff.MergeBase + " " }},
		{name: "missing head", mutate: func(report *Report) { report.Diff.Head = "" }},
		{name: "malformed head", mutate: func(report *Report) { report.Diff.Head = strings.Repeat("g", 40) }},
		{name: "head trailing newline", mutate: func(report *Report) { report.Diff.Head += "\n" }},
		{name: "head surrounding whitespace", mutate: func(report *Report) { report.Diff.Head = " " + report.Diff.Head + " " }},
		{name: "mixed object ID lengths", mutate: func(report *Report) { report.Diff.Head = strings.Repeat("c", 64) }},
		{name: "negative file count", mutate: func(report *Report) { report.Diff.ChangedFiles = -1 }},
		{name: "negative line count", mutate: func(report *Report) { report.Diff.ChangedLines = -1 }},
		{name: "zero files with lines", mutate: func(report *Report) { report.Diff.ChangedFiles, report.Diff.ChangedLines = 0, 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := valid
			test.mutate(&report)
			if err := validateReport(report, report.RunID); err == nil {
				t.Fatal("validateReport accepted invalid diff metadata")
			}
		})
	}
}

func TestValidateReportRequiresFullRepositoryObjectID(t *testing.T) {
	for _, repositoryID := range []string{
		"",
		"repo-id",
		"repo\x00id",
		strings.ToUpper(strings.Repeat("d", 40)),
	} {
		report := completeReportFixture("20260821T120000.000000000Z-0000")
		report.RepoID = repositoryID
		if err := validateReport(report, report.RunID); err == nil {
			t.Fatalf("validateReport accepted repository ID %q", repositoryID)
		}
	}
}

func TestValidateDiffReportRejectsObjectIDWhitespaceAtMatchingLengths(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*DiffReport)
	}{
		{name: "trailing newline", mutate: func(diff *DiffReport) {
			diff.BaseCommit += "\n"
			diff.MergeBase += "\n"
			diff.Head += "\n"
		}},
		{name: "surrounding whitespace", mutate: func(diff *DiffReport) {
			diff.BaseCommit = " " + diff.BaseCommit + " "
			diff.MergeBase = " " + diff.MergeBase + " "
			diff.Head = " " + diff.Head + " "
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := completeReportFixture("20260821T120000.000000000Z-0000")
			test.mutate(&report.Diff)
			if err := validateReport(report, report.RunID); err == nil {
				t.Fatal("validateReport accepted object ID whitespace")
			}
		})
	}
}

func TestValidObjectIDStringRejectsDelimitedObjectIDs(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	for _, test := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "SHA-1", value: sha1, want: true},
		{name: "SHA-256", value: sha256, want: true},
		{name: "base commit trailing newline", value: sha1 + "\n"},
		{name: "merge base trailing newline", value: sha1 + "\n"},
		{name: "head trailing newline", value: sha1 + "\n"},
		{name: "base commit surrounding whitespace", value: " " + sha1 + " "},
		{name: "merge base surrounding whitespace", value: " " + sha1 + " "},
		{name: "head surrounding whitespace", value: " " + sha1 + " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validObjectIDString(test.value); got != test.want {
				t.Fatalf("validObjectIDString(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}

func TestValidateReportAcceptsZeroAndBinaryDiffCounts(t *testing.T) {
	for _, diff := range []DiffReport{
		{BaseRef: "origin/main", BaseCommit: strings.Repeat("a", 40), MergeBase: strings.Repeat("b", 40), Head: strings.Repeat("c", 40)},
		{BaseRef: "origin/main", BaseCommit: strings.Repeat("a", 40), MergeBase: strings.Repeat("b", 40), Head: strings.Repeat("c", 40), ChangedFiles: 1},
	} {
		report := completeReportFixture("20260821T120000.000000000Z-0000")
		report.Diff = diff
		if err := validateReport(report, report.RunID); err != nil {
			t.Fatalf("validateReport(%#v): %v", diff, err)
		}
	}
}

func TestLatestSkipsSchemaOneReports(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	older := completeReportFixture("20260821T120000.000000000Z-0000")
	newer := completeReportFixture("20260821T120100.000000000Z-0001")
	newer.SchemaVersion = 1
	writeReportFixture(t, runsDir, older)
	writeReportFixture(t, runsDir, newer)

	got, err := (testLedger(repoState)).Latest()
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != older.RunID {
		t.Fatalf("Latest run ID = %q, want %q", got.RunID, older.RunID)
	}

	onlyLegacyState := t.TempDir()
	onlyLegacyRuns := filepath.Join(onlyLegacyState, "runs")
	if err := os.Mkdir(onlyLegacyRuns, 0o700); err != nil {
		t.Fatal(err)
	}
	writeReportFixture(t, onlyLegacyRuns, newer)
	if _, err := (testLedger(onlyLegacyState)).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
}

func TestWriteReportRejectsTamperedCompleteReports(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*Report)
	}{
		{name: "repo ID", tamper: func(report *Report) { report.RepoID = "" }},
		{name: "started", tamper: func(report *Report) { report.StartedAt = time.Time{} }},
		{name: "finished", tamper: func(report *Report) { report.FinishedAt = time.Time{} }},
		{name: "counts", tamper: func(report *Report) { report.Counts.Errors = 1 }},
		{name: "verdict", tamper: func(report *Report) { report.Verdict = VerdictFindings }},
		{name: "gate name", tamper: func(report *Report) { report.Gates[0].Gate = "" }},
		{name: "gate language", tamper: func(report *Report) { report.Gates[0].Language = "" }},
		{name: "passed error", tamper: func(report *Report) { report.Gates[0].Error = "failed" }},
		{name: "findings without findings", tamper: func(report *Report) { report.Gates[0].Status = GateFindings }},
		{name: "errored without error", tamper: func(report *Report) { report.Gates[0].Status = GateErrored; report.Verdict = VerdictErrored }},
		{name: "duplicate gate", tamper: func(report *Report) { report.Gates = append(report.Gates, report.Gates[0]) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run, err := (testLedger(t.TempDir())).Start()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			report := completeReportFixture(filepath.Base(run.Dir))
			test.tamper(&report)
			if err := run.WriteReport(report); err == nil {
				t.Fatal("WriteReport accepted tampered report")
			}
		})
	}
}

func TestLatestSkipsSemanticallyInvalidNewestReport(t *testing.T) {
	repoState := t.TempDir()
	runsDir := filepath.Join(repoState, "runs")
	if err := os.Mkdir(runsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	older := completeReportFixture("20260821T120000.000000000Z-0000")
	newer := completeReportFixture("20260821T120100.000000000Z-0001")
	if err := validateReport(older, older.RunID); err != nil {
		t.Fatalf("valid fixture: %v", err)
	}
	newer.Counts.Warnings = 1
	writeReportFixture(t, runsDir, older)
	writeReportFixture(t, runsDir, newer)
	got, err := (testLedger(repoState)).Latest()
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != older.RunID {
		t.Fatalf("Latest run ID = %q, want %q", got.RunID, older.RunID)
	}

	onlyInvalidState := t.TempDir()
	onlyInvalidRuns := filepath.Join(onlyInvalidState, "runs")
	if err := os.Mkdir(onlyInvalidRuns, 0o700); err != nil {
		t.Fatal(err)
	}
	writeReportFixture(t, onlyInvalidRuns, newer)
	if _, err := (testLedger(onlyInvalidState)).Latest(); !errors.Is(err, ErrNoCompleteRuns) {
		t.Fatalf("Latest error = %v, want ErrNoCompleteRuns", err)
	}
}

func TestWriteReportCleansTemporaryFileAfterEncodingError(t *testing.T) {
	run, err := (testLedger(t.TempDir())).Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	report := completeReportFixture("")
	report.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	report.FinishedAt = report.StartedAt.Add(time.Second)
	if err := run.WriteReport(report); err == nil {
		t.Fatal("WriteReport succeeded with a non-JSON timestamp")
	} else if !strings.Contains(err.Error(), "encode report") {
		t.Fatalf("WriteReport error = %v, want encoding failure", err)
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
	report = completeReportDefaults(report)
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

func completeReportDefaults(report Report) Report {
	defaults := completeReportFixture(report.RunID)
	if report.SchemaVersion == 0 {
		report.SchemaVersion = defaults.SchemaVersion
	}
	if report.RepoID == "" {
		report.RepoID = defaults.RepoID
	}
	if report.StartedAt.IsZero() {
		report.StartedAt = defaults.StartedAt
	}
	if report.FinishedAt.IsZero() {
		report.FinishedAt = defaults.FinishedAt
	}
	if report.Verdict == "" {
		report.Verdict = defaults.Verdict
	}
	if report.Gates == nil {
		report.Gates = defaults.Gates
	}
	if report.Findings == nil {
		report.Findings = defaults.Findings
	}
	if report.Diff == (DiffReport{}) {
		report.Diff = defaults.Diff
	}
	return report
}

func completeReportFixture(runID string) Report {
	started := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	return Report{
		SchemaVersion: 2,
		RunID:         runID,
		RepoID:        strings.Repeat("d", 40),
		Diff: DiffReport{
			BaseRef:    "origin/main",
			BaseCommit: strings.Repeat("a", 40),
			MergeBase:  strings.Repeat("b", 40),
			Head:       strings.Repeat("c", 40),
		},
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Verdict:    VerdictUnverified,
		Gates:      []GateReport{{Gate: "lint", Language: "go", Status: GatePassed}},
		Findings:   []finding.Finding{},
		Counts:     Counts{},
	}
}
