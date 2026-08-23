package gitcmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestHermeticEnvContainsExactlyOneOwnedValue(t *testing.T) {
	owned := map[string]string{
		"LC_ALL":                 "C",
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_GLOBAL":      os.DevNull,
		"GIT_ATTR_NOSYSTEM":      "1",
		"GIT_NO_LAZY_FETCH":      "1",
		"GIT_OPTIONAL_LOCKS":     "0",
	}
	for name := range owned {
		t.Setenv(name, "ambient")
		t.Setenv(strings.ToLower(name), "ambient-lower")
	}
	before := slices.Clone(os.Environ())
	environment := Env(Hermetic)
	if !slices.Equal(os.Environ(), before) {
		t.Fatal("Env mutated the ambient process environment")
	}
	for name, value := range owned {
		count := 0
		for _, entry := range environment {
			entryName, entryValue, _ := strings.Cut(entry, "=")
			if strings.EqualFold(entryName, name) {
				count++
				if entryName != name || entryValue != value {
					t.Fatalf("owned environment %s has unexpected entry %q", name, entry)
				}
			}
		}
		if count != 1 {
			t.Fatalf("owned environment %s appears %d times, want exactly once", name, count)
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

func TestOutputEnvSuppliesExplicitCommitIdentity(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")

	extra := map[string]string{
		"GIT_AUTHOR_NAME":     "Togi Author",
		"GIT_AUTHOR_EMAIL":    "author@example.invalid",
		"GIT_COMMITTER_NAME":  "Togi Committer",
		"GIT_COMMITTER_EMAIL": "committer@example.invalid",
	}
	args := []string{"commit", "--allow-empty", "-qm", "identity"}
	wantExtra := mapsClone(extra)
	wantArgs := slices.Clone(args)
	if _, err := OutputEnv(context.Background(), repo, Hermetic, 1<<20, extra, args...); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(extra, wantExtra) || !slices.Equal(args, wantArgs) {
		t.Fatalf("OutputEnv mutated inputs: extra=%v args=%v", extra, args)
	}
	got := strings.TrimSpace(string(mustOutput(t, repo, "show", "-s", "--format=%an <%ae>|%cn <%ce>")))
	if want := "Togi Author <author@example.invalid>|Togi Committer <committer@example.invalid>"; got != want {
		t.Fatalf("commit identity = %q, want %q", got, want)
	}
}

func TestOutputEnvStripsAmbientGitVariablesBeforeApplyingExplicitValues(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing"))

	got, err := OutputEnv(context.Background(), repo, Hermetic, 1<<20, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != repo {
		t.Fatalf("top level = %q, want %q", strings.TrimSpace(string(got)), repo)
	}
}

func TestOutputEnvRejectsInvalidEnvironment(t *testing.T) {
	tests := []map[string]string{
		{"BAD-NAME": "value"},
		{"BAD\x00NAME": "value"},
		{"GOOD_NAME": "bad\x00value"},
		{"GIT_CONFIG_NOSYSTEM": "0"},
		{"GIT_DIR": t.TempDir()},
		{"GIT_CONFIG_COUNT": "0"},
	}
	for _, extra := range tests {
		if _, err := OutputEnv(context.Background(), t.TempDir(), Hermetic, 1<<20, extra, "version"); err == nil {
			t.Fatalf("OutputEnv(%q) succeeded, want validation error", extra)
		}
	}
}

func TestOutputEnvRejectsCaseFoldedIdentityCollisionsAndMixedCase(t *testing.T) {
	tests := []map[string]string{
		{"GIT_AUTHOR_NAME": "one", "git_author_name": "two"},
		{"Git_Author_Email": "author@example.invalid"},
		{"GIT_COMMITTER_NAME": "one", "git_committer_name": "two"},
	}
	for _, extra := range tests {
		before := mapsClone(extra)
		if _, err := OutputEnv(context.Background(), t.TempDir(), Hermetic, 1<<20, extra, "version"); err == nil {
			t.Fatalf("OutputEnv(%q) succeeded, want case-fold validation error", extra)
		}
		if !reflect.DeepEqual(extra, before) {
			t.Fatalf("OutputEnv mutated input: got %q, want %q", extra, before)
		}
	}
}

func TestOutputWithConfigPreservesEqualsInKey(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	key := "includeIf.hasconfig:remote.*.url:https://example.com/**?x=*.path"
	output, err := OutputWithConfig(context.Background(), repo, Hermetic, 1<<20, key, "/tmp/probe",
		"config", "--get", key)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(output)); got != "/tmp/probe" {
		t.Fatalf("config value = %q, want exact injected value", got)
	}
}

func mapsClone(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func mustOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := Output(context.Background(), dir, Hermetic, 1<<20, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
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
