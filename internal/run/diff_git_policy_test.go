//go:build linux

package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestResolveDiffRejectsGitConversionFiltersBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*testing.T, string, string, string)
	}{
		{name: "clean", configure: func(t *testing.T, repo, command, _ string) {
			gitDiffTest(t, repo, "config", "filter.marker.clean", command)
		}},
		{name: "process", configure: func(t *testing.T, repo, command, _ string) {
			gitDiffTest(t, repo, "config", "filter.marker.process", command)
		}},
		{name: "included clean", configure: func(t *testing.T, repo, command, include string) {
			contents := "[filter \"marker\"]\n\tclean = " + command + "\n"
			if err := os.WriteFile(include, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			gitDiffTest(t, repo, "config", "include.path", include)
		}},
		{name: "worktree clean", configure: func(t *testing.T, repo, command, _ string) {
			gitDiffTest(t, repo, "config", "extensions.worktreeConfig", "true")
			gitDiffTest(t, repo, "config", "--worktree", "filter.marker.clean", command)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, ".gitattributes", "*.go filter=marker\n")
			writeDiffTestFile(t, repo, "file.go", "base\n")
			base := commitDiffTestRepo(t, repo, "base")
			writeDiffTestFile(t, repo, "file.go", "feature\n")
			commitDiffTestRepo(t, repo, "feature")
			marker := filepath.Join(t.TempDir(), "executed")
			const secret = "conversion-filter-secret"
			command := helperBinary(t) + " mark " + marker + " " + secret
			test.configure(t, repo, command, filepath.Join(t.TempDir(), "included-config"))

			_, err := resolveDiff(context.Background(), repo, base)
			if err == nil || !strings.Contains(err.Error(), "Git conversion filters") {
				t.Fatalf("resolveDiff() error = %v, want conversion-filter diagnostic", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), command) {
				t.Fatalf("resolveDiff() exposed conversion command: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("conversion filter executed: %v", err)
			}
		})
	}
}

func TestResolveDiffAllowsSmudgeOnlyGitFilter(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, ".gitattributes", "*.go filter=marker\n")
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	commitDiffTestRepo(t, repo, "feature")
	marker := filepath.Join(t.TempDir(), "executed")
	gitDiffTest(t, repo, "config", "filter.marker.smudge", helperBinary(t)+" mark "+marker)

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("smudge-only filter executed: %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 1, End: 1}}}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() lines = %#v, want %#v", got.Lines, want)
	}
}

func TestResolveDiffDoesNotClassifyRequiredOnlyAsConversionCommand(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, ".gitattributes", "*.go filter=marker\n")
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	commitDiffTestRepo(t, repo, "feature")
	gitDiffTest(t, repo, "config", "filter.marker.required", "true")

	_, err := resolveDiff(context.Background(), repo, base)
	if err == nil {
		t.Fatal("resolveDiff() error = nil, want Git status failure for missing required driver")
	}
	if strings.Contains(err.Error(), "Git conversion filters") {
		t.Fatalf("required-only field classified as a conversion command: %v", err)
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

func TestResolveDiffRejectsRepositoryLocalAttributes(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	commitDiffTestRepo(t, repo, "feature")
	attributes := localAttributesPathForTest(t, repo)
	if err := os.MkdirAll(filepath.Dir(attributes), 0o755); err != nil {
		t.Fatal(err)
	}
	const contents = "*.go binary\n"
	if err := os.WriteFile(attributes, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resolveDiff(context.Background(), repo, base)
	if err == nil || !strings.Contains(err.Error(), "local Git attributes") {
		t.Fatalf("resolveDiff() error = %v, want local-attributes diagnostic", err)
	}
	if strings.Contains(err.Error(), "*.go") || strings.Contains(err.Error(), "binary") {
		t.Fatalf("resolveDiff() exposed local attributes contents: %v", err)
	}
}

func TestResolveDiffAllowsEmptyRepositoryLocalAttributes(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	writeDiffTestFile(t, repo, "file.go", "feature\n")
	commitDiffTestRepo(t, repo, "feature")
	attributes := localAttributesPathForTest(t, repo)
	if err := os.MkdirAll(filepath.Dir(attributes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attributes, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveDiff(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("resolveDiff() error = %v", err)
	}
	want := finding.ChangedLines{"file.go": {{Start: 1, End: 1}}}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("resolveDiff() lines = %#v, want %#v", got.Lines, want)
	}
}

func TestResolveDiffRejectsUnsafeRepositoryLocalAttributesPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(*testing.T, string)
	}{
		{name: "symlink", create: func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "attributes")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
		{name: "nonregular", create: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newDiffTestRepo(t)
			writeDiffTestFile(t, repo, "file.go", "base\n")
			base := commitDiffTestRepo(t, repo, "base")
			attributes := localAttributesPathForTest(t, repo)
			if err := os.MkdirAll(filepath.Dir(attributes), 0o755); err != nil {
				t.Fatal(err)
			}
			test.create(t, attributes)

			_, err := resolveDiff(context.Background(), repo, base)
			if err == nil || !strings.Contains(err.Error(), "local Git attributes") {
				t.Fatalf("resolveDiff() error = %v, want unsafe local-attributes diagnostic", err)
			}
		})
	}
}

func TestResolveDiffFindsRepositoryLocalAttributesFromLinkedWorktree(t *testing.T) {
	repo := newDiffTestRepo(t)
	writeDiffTestFile(t, repo, "file.go", "base\n")
	base := commitDiffTestRepo(t, repo, "base")
	linked := filepath.Join(t.TempDir(), "linked")
	gitDiffTest(t, repo, "worktree", "add", "-b", "feature", linked, "main")
	writeDiffTestFile(t, linked, "file.go", "feature\n")
	commitDiffTestRepo(t, linked, "feature")
	attributes := localAttributesPathForTest(t, linked)
	if err := os.MkdirAll(filepath.Dir(attributes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attributes, []byte("*.go binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := resolveDiff(context.Background(), linked, base)
	if err == nil || !strings.Contains(err.Error(), "local Git attributes") {
		t.Fatalf("resolveDiff(linked worktree) error = %v, want local-attributes diagnostic", err)
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

func localAttributesPathForTest(t *testing.T, repo string) string {
	t.Helper()
	path := gitDiffTest(t, repo, "rev-parse", "--git-path", "info/attributes")
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	return filepath.Clean(path)
}
