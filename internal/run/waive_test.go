package run

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/waiver"
)

const waiveFingerprint = "0f2ac1e1b8f7c8b56b6da5e0f9dc0f6e6c1a2b3c4d5e6f708192a3b4c5d6e7f8"

func TestWaiveRecordsAnApprovalOutsideTheRepository(t *testing.T) {
	root, paths := fixtureRepository(t)
	before := targetTree(t, root)
	out := new(bytes.Buffer)
	service := fixtureService(paths, out)
	service.Now = func() time.Time { return time.Date(2026, time.August, 25, 3, 17, 33, 0, time.UTC) }

	record, err := service.Waive(context.Background(), root, waiveFingerprint, "the deleted test covered a removed feature")
	if err != nil {
		t.Fatalf("Waive() = %v", err)
	}
	if record.Fingerprint != waiveFingerprint || record.Reason != "the deleted test covered a removed feature" {
		t.Fatalf("Waive() = %#v", record)
	}
	if !record.ApprovedAt.Equal(time.Date(2026, time.August, 25, 3, 17, 33, 0, time.UTC)) {
		t.Fatalf("approved at %s, want the service clock", record.ApprovedAt)
	}
	for _, want := range []string{waiveFingerprint, "the deleted test covered a removed feature", "2026-08-25T03:17:33Z"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
		}
	}

	loaded, err := waiver.Store{Dir: repoStateDir(t, service, root)}.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 || loaded[0] != record {
		t.Fatalf("persisted waivers = %#v, want %#v", loaded, record)
	}
	if got := targetTree(t, root); !slices.Equal(got, before) {
		t.Fatalf("waiving changed the target repository:\n got %v\nwant %v", got, before)
	}
}

func TestWaiveReportsAnAlreadyApprovedFingerprint(t *testing.T) {
	root, paths := fixtureRepository(t)
	out := new(bytes.Buffer)
	service := fixtureService(paths, out)
	first, err := service.Waive(context.Background(), root, waiveFingerprint, "the original judgement")
	if err != nil {
		t.Fatalf("first Waive() = %v", err)
	}

	out.Reset()
	again, err := service.Waive(context.Background(), root, waiveFingerprint, "a later judgement")
	if err != nil {
		t.Fatalf("second Waive() = %v", err)
	}
	if again != first {
		t.Fatalf("Waive() = %#v, want the original %#v", again, first)
	}
	if !strings.Contains(out.String(), "already waived") {
		t.Fatalf("stdout = %q, want it to report an existing approval", out.String())
	}
	loaded, err := waiver.Store{Dir: repoStateDir(t, service, root)}.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("persisted waivers = %#v, want one record", loaded)
	}
}

func TestWaiveRefusesAnUnexplainedOrUnidentifiedApproval(t *testing.T) {
	root, paths := fixtureRepository(t)
	service := fixtureService(paths, new(bytes.Buffer))

	if _, err := service.Waive(context.Background(), root, waiveFingerprint, "  "); !errors.Is(err, waiver.ErrReasonRequired) {
		t.Fatalf("Waive() = %v, want ErrReasonRequired", err)
	}
	if _, err := service.Waive(context.Background(), root, "not-a-fingerprint", "approved"); !errors.Is(err, waiver.ErrInvalidFingerprint) {
		t.Fatalf("Waive() = %v, want ErrInvalidFingerprint", err)
	}
	if _, err := os.Stat(filepath.Join(repoStateDir(t, service, root), waiver.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused approval wrote state: %v", err)
	}
}

func TestWaiveIsSharedBetweenLinkedWorktrees(t *testing.T) {
	root, paths := fixtureRepository(t)
	service := fixtureService(paths, new(bytes.Buffer))
	linked := filepath.Join(t.TempDir(), "linked")
	gitFixture(t, root, "worktree", "add", "-q", "-b", "linked", linked)

	if _, err := service.Waive(context.Background(), root, waiveFingerprint, "approved in the first checkout"); err != nil {
		t.Fatalf("Waive() = %v", err)
	}
	loaded, err := waiver.Store{Dir: repoStateDir(t, service, linked)}.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded) != 1 || loaded[0].Reason != "approved in the first checkout" {
		t.Fatalf("linked worktree waivers = %#v, want the approval made in the original checkout", loaded)
	}
}

func TestWaiveRequiresAnInitializedService(t *testing.T) {
	root, paths := fixtureRepository(t)
	if _, err := (Service{Stdout: new(bytes.Buffer)}).Waive(context.Background(), root, waiveFingerprint, "approved"); err == nil {
		t.Fatal("Waive() accepted unresolved storage paths")
	}
	if _, err := (Service{Paths: paths}).Waive(context.Background(), root, waiveFingerprint, "approved"); err == nil {
		t.Fatal("Waive() accepted a service with no output")
	}
	if _, err := fixtureService(paths, new(bytes.Buffer)).Waive(nil, root, waiveFingerprint, "approved"); err == nil { //nolint:staticcheck // a nil context is the boundary under test
		t.Fatal("Waive() accepted a nil context")
	}
	if _, err := fixtureService(paths, new(bytes.Buffer)).Waive(context.Background(), filepath.Join(t.TempDir(), "absent"), waiveFingerprint, "approved"); err == nil {
		t.Fatal("Waive() accepted a directory outside any repository")
	}
}

func TestWaiveIsUnsupportedOffLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the unsupported-platform boundary is asserted from Linux")
	}
	root, paths := fixtureRepository(t)
	service := fixtureService(paths, new(bytes.Buffer))
	service.GOOS = "darwin"
	if _, err := service.Waive(context.Background(), root, waiveFingerprint, "approved"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Waive() = %v, want ErrUnsupportedPlatform", err)
	}
}

func repoStateDir(t *testing.T, service Service, root string) string {
	t.Helper()
	id, err := service.resolveRepository(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return service.Paths.RepoState(id)
}
