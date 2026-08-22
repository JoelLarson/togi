package wiki

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/gate"
)

func newService(t *testing.T, overrideDir string) (Service, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return Service{
		Pages:  Loader{OverrideDir: overrideDir},
		Gates:  gate.Loader{},
		Stdout: &stdout,
		Stderr: &stderr,
	}, &stdout, &stderr
}

func TestShowPrintsBodyThenReverseIndex(t *testing.T) {
	service, stdout, _ := newService(t, t.TempDir())
	if err := service.Show("small-composable-functions"); err != nil {
		t.Fatal(err)
	}

	out := stdout.String()
	if !strings.HasPrefix(out, "# Small, composable functions\n") {
		t.Fatalf("output does not begin with the page body: %.60q", out)
	}
	if !strings.Contains(out, "Extract Function") {
		t.Fatal("output omits the techniques section")
	}
	if !strings.Contains(out, "page: small-composable-functions (shipped)") {
		t.Fatalf("output omits the provenance line:\n%s", out)
	}
	if !strings.Contains(out, "complexity/go\tgocyclo/complexity") {
		t.Fatalf("output omits the reverse index:\n%s", out)
	}
}

// ADR-0006 requires identical inputs to produce identical output, because
// briefs are deterministic concatenation rather than search.
func TestShowIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	first, firstOut, _ := newService(t, dir)
	second, secondOut, _ := newService(t, dir)
	if err := first.Show("small-composable-functions"); err != nil {
		t.Fatal(err)
	}
	if err := second.Show("small-composable-functions"); err != nil {
		t.Fatal(err)
	}
	if firstOut.String() != secondOut.String() {
		t.Fatal("two renders of one page differ")
	}
}

func TestShowUnknownPageFails(t *testing.T) {
	service, _, _ := newService(t, t.TempDir())
	if err := service.Show("no-such-page"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLintWarnsOnDanglingWithoutFailing(t *testing.T) {
	service, stdout, stderr := newService(t, t.TempDir())
	if err := service.Lint(); err != nil {
		t.Fatalf("lint failed on a dangling alias: %v", err)
	}
	if !strings.Contains(stderr.String(), `"lint-correctness", which has no page`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 dangling, 0 conflicting") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestLintFailsOnConflictingAliases(t *testing.T) {
	dir := t.TempDir()
	writeConflictingGates(t, dir)

	var stdout, stderr bytes.Buffer
	service := Service{
		Pages:  Loader{OverrideDir: t.TempDir()},
		Gates:  gate.Loader{OverrideDir: dir},
		Stdout: &stdout,
		Stderr: &stderr,
	}
	err := service.Lint()
	if !errors.Is(err, ErrConflictingAliases) {
		t.Fatalf("err = %v, want ErrConflictingAliases", err)
	}
	if !strings.Contains(stderr.String(), `"gocyclo/complexity" is aliased to`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestEjectWritesShippedBodyAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	service, _, _ := newService(t, dir)
	if err := service.Eject("small-composable-functions"); err != nil {
		t.Fatal(err)
	}

	filename := filepath.Join(dir, "small-composable-functions.md")
	written, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	shippedPage, err := loadShipped("small-composable-functions")
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != shippedPage.Body {
		t.Fatal("ejected file does not match the shipped page")
	}

	if err := service.Eject("small-composable-functions"); err == nil {
		t.Fatal("second eject overwrote an existing override")
	}
}

func TestEjectCreatesMissingWikiDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config", "togi", "wiki")
	service, _, _ := newService(t, dir)
	if err := service.Eject("small-composable-functions"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "small-composable-functions.md")); err != nil {
		t.Fatal(err)
	}
}

func TestEjectUnknownPageFails(t *testing.T) {
	service, _, _ := newService(t, t.TempDir())
	if err := service.Eject("no-such-page"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// writeConflictingGates lays down a gate override whose alias sends a rule ID
// already claimed by the shipped complexity gate to a different page.
func writeConflictingGates(t *testing.T, dir string) {
	t.Helper()
	gateDir := filepath.Join(dir, "lint", "go")
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `name = "lint"
description = "Static analysis and correctness checks"
cost_class = "fast"
fix_policy = "autofix-then-llm"
scope = "diff"
blocking = ["error", "warning"]
timeout = "60s"
`
	if err := os.WriteFile(filepath.Join(dir, "lint", "gate.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	binding := `language = "go"
tool = "golangci-lint"
command = ["golangci-lint", "run"]
success_exit_codes = [0]
finding_exit_codes = [1]
normalizer = "golangci-json"

[severity_map]
default = "warning"

[aliases]
"gocyclo/complexity" = "a-different-page"
`
	if err := os.WriteFile(filepath.Join(gateDir, "binding.toml"), []byte(binding), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShowSeparatesBodyWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "small-composable-functions", "# Mine\n\nNo trailing newline.")

	service, stdout, _ := newService(t, dir)
	if err := service.Show("small-composable-functions"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "No trailing newline.\n\n---\n") {
		t.Fatalf("body runs into the separator:\n%q", stdout.String())
	}
}
