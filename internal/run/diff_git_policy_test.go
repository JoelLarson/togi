//go:build linux

package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/finding"
)

func TestResolveDiffNeutralizesExecutableGitConfiguration(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*testing.T, string, string)
	}{
		{name: "fsmonitor", configure: func(t *testing.T, repo, marker string) {
			gitDiffTest(t, repo, "config", "core.fsmonitor", helperBinary(t)+" mark "+marker)
		}},
		{name: "textconv", configure: func(t *testing.T, repo, marker string) {
			writeDiffTestFile(t, repo, ".gitattributes", "*.go diff=marker\n")
			gitDiffTest(t, repo, "add", ".gitattributes")
			gitDiffTest(t, repo, "commit", "--amend", "--no-edit")
			gitDiffTest(t, repo, "config", "diff.marker.textconv", helperBinary(t)+" mark "+marker)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, "file.go", "base\n")
			base := commitDiffTestRepo(t, repo, "base")
			marker := filepath.Join(t.TempDir(), "executed")
			test.configure(t, repo, marker)
			if test.name == "textconv" {
				base = gitDiffTest(t, repo, "rev-parse", "HEAD")
			}
			writeDiffTestFile(t, repo, "file.go", "feature\n")
			commitDiffTestRepo(t, repo, "feature")
			if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			got, err := resolveDiff(context.Background(), repo, base)
			if err != nil {
				t.Fatalf("resolveDiff() error = %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("repository-configured command executed: %v", err)
			}
			want := finding.ChangedLines{"file.go": {{Start: 1, End: 1}}}
			if !reflect.DeepEqual(got.Lines, want) {
				t.Fatalf("resolveDiff() lines = %#v, want %#v", got.Lines, want)
			}
		})
	}
}

func TestResolveDiffNeutralizesLocalAttributesFile(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	attributes := filepath.Join(t.TempDir(), "attributes")
	if err := os.WriteFile(attributes, []byte("*.go binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDiffTest(t, repo, "config", "core.attributesFile", attributes)
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	commitDiffTestRepo(t, repo, "feature")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 1, End: 1}}}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() lines = %#v, want %#v", got.Lines, want)
	}
}

func TestResolveDiffHonorsCommittedAttributes(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, ".gitattributes", "*.dat binary\n")
	writeDiffTestFile(t, repo, "data.dat", "base text\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "data.dat", "feature text\n")
	commitDiffTestRepo(t, repo, "feature")

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"data.dat": {}}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() lines = %#v, want committed binary attribute result %#v", got.Lines, want)
	}
}

func TestDeterministicDiffOptions(t *testing.T) {
	want := []string{
		"--no-ext-diff",
		"--no-textconv",
		"--diff-algorithm=myers",
		"--no-indent-heuristic",
		"--inter-hunk-context=0",
		"--find-renames=50%",
		"-l0",
	}
	if got := deterministicDiffOptions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("deterministicDiffOptions() = %#v, want %#v", got, want)
	}
}

func TestResolveDiffPinsDiffConfiguration(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "one\ntwo\nthree\nfour\nfive\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "one\nTWO\nthree\nFOUR\nfive\n")
	commitDiffTestRepo(t, repo, "feature")
	for key, value := range map[string]string{
		"diff.algorithm":        "histogram",
		"diff.indentHeuristic":  "true",
		"diff.interHunkContext": "99",
		"diff.renameLimit":      "1",
		"diff.renames":          "false",
	} {
		gitDiffTest(t, repo, "config", key, value)
	}

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 2, End: 2}, {Start: 4, End: 4}}}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() lines = %#v, want %#v", got.Lines, want)
	}
}

func TestBoundedCommandCancellationTerminatesDescendants(t *testing.T) {
	root := t.TempDir()
	started := filepath.Join(root, "descendant-started")
	survived := filepath.Join(root, "descendant-survived")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	helper := helperBinary(t)
	go func() {
		_, err := boundedCommandOutput(ctx, root, helper, []string{"spawn-survivor", started, survived}, diffGitEnvironment(), 1024)
		result <- err
	}()

	waitForDiffTestFile(t, started, time.Second)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("boundedCommandOutput() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bounded command did not return promptly after cancellation")
	}
	time.Sleep(450 * time.Millisecond)
	if _, err := os.Stat(survived); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived cancellation: %v", err)
	}
}

func waitForDiffTestFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}
