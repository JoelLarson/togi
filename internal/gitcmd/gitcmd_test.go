package gitcmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestEnvStripsMixedCaseGitVariablesForBothPolicies(t *testing.T) {
	t.Setenv("git_dir", "/tmp/other-repository")
	t.Setenv("Git_Config_Count", "1")

	for _, iso := range []Isolation{Hermetic, HonourGlobal} {
		for _, entry := range Env(iso) {
			name, _, _ := strings.Cut(entry, "=")
			if strings.EqualFold(name, "GIT_DIR") || strings.EqualFold(name, "GIT_CONFIG_COUNT") {
				t.Fatalf("Env(%#v) includes %q", iso, entry)
			}
		}
	}
}

func TestEnvHardeningPerPolicy(t *testing.T) {
	hermeticOnly := []string{
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
	hermetic := Env(Hermetic)
	honour := Env(HonourGlobal)
	for _, env := range [][]string{hermetic, honour} {
		if !slices.Contains(env, "GIT_NO_REPLACE_OBJECTS=1") {
			t.Fatal("replace objects are not disabled")
		}
	}
	for _, entry := range hermeticOnly {
		if !slices.Contains(hermetic, entry) {
			t.Fatalf("Hermetic env is missing %q", entry)
		}
		if slices.Contains(honour, entry) {
			t.Fatalf("HonourGlobal env includes hermetic entry %q", entry)
		}
	}
}

func TestArgsPrependsHermeticOverridesOnly(t *testing.T) {
	want := []string{"-c", "core.fsmonitor=false", "-c", "core.attributesFile=" + os.DevNull, "status"}
	if got := Args(Hermetic, "status"); !slices.Equal(got, want) {
		t.Fatalf("Args(Hermetic) = %v, want %v", got, want)
	}
	if got := Args(HonourGlobal, "status"); !slices.Equal(got, []string{"status"}) {
		t.Fatalf("Args(HonourGlobal) = %v, want bare args", got)
	}
}

func TestOutputSeparatesStdoutFromErrorDiagnostics(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "-c", "user.name=Togi", "-c", "user.email=togi@example.invalid", "commit", "--allow-empty", "-qm", "initial")

	stdout, err := Output(context.Background(), repo, Hermetic, 1<<20, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stdout)) == "" {
		t.Fatal("stdout is empty")
	}

	_, err = Output(context.Background(), repo, Hermetic, 1<<20, "rev-parse", "--show-toplevel", "--verify", "missing-ref")
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %v, want *CommandError", err)
	}
	if strings.Contains(cmdErr.Error(), repo) {
		t.Fatalf("error includes stdout repository path: %q", cmdErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want wrapped *exec.ExitError", err)
	}
}

func TestOutputReportsTruncationAsErrOutputLimit(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	_, err := Output(context.Background(), repo, Hermetic, 4, "rev-parse", "--git-dir")
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("error = %v, want ErrOutputLimit", err)
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %v, want *CommandError", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = Env(Hermetic)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
