package wiki

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/gate/gatetest"
)

type gateSource struct {
	gates []gate.Gate
	err   error
}

func (source gateSource) LoadAll() ([]gate.Gate, error) { return source.gates, source.err }

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

func TestForLoadsPageForExactAlias(t *testing.T) {
	service := Service{
		Pages: Loader{},
		Gates: gateSource{gates: []gate.Gate{gatetest.Compile(t, "complexity",
			gatetest.Aliases(map[string]string{"gocyclo/complexity": "small-composable-functions"}),
		)}},
	}

	page, found, err := service.For(finding.Finding{Gate: "complexity", Language: "go", RuleID: "gocyclo/complexity"})
	if err != nil {
		t.Fatal(err)
	}
	if !found || page.Name != "small-composable-functions" {
		t.Fatalf("page = %#v, found = %v", page, found)
	}
}

func TestForAliasPrecedence(t *testing.T) {
	aliases := map[string]string{
		"golangci-lint/*":          "broad",
		"golangci-lint/gosec/*":    "small-composable-functions",
		"golangci-lint/gosec/G401": "exact",
	}
	service := Service{Pages: Loader{}, Gates: gateSource{gates: []gate.Gate{
		gatetest.Compile(t, "lint", gatetest.Aliases(aliases)),
	}}}

	tests := []struct {
		name   string
		ruleID string
		found  bool
	}{
		{name: "longest glob", ruleID: "golangci-lint/gosec/G402", found: true},
		{name: "exact beats glob", ruleID: "golangci-lint/gosec/G401", found: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, found, err := service.For(finding.Finding{Gate: "lint", Language: "go", RuleID: test.ruleID})
			if err != nil {
				t.Fatal(err)
			}
			if found != test.found {
				t.Fatalf("found = %v, want %v (page %#v)", found, test.found, page)
			}
			if found && page.Name != "small-composable-functions" {
				t.Fatalf("page = %q", page.Name)
			}
		})
	}
}

func TestForReturnsNotFoundForMissingMapping(t *testing.T) {
	known := gatetest.Compile(t, "known", gatetest.Aliases(map[string]string{
		"tool/dangling": "no-such-page",
		"tool/mapped":   "small-composable-functions",
	}))
	tests := []struct {
		name    string
		finding finding.Finding
	}{
		{name: "unknown gate", finding: finding.Finding{Gate: "unknown", Language: "go", RuleID: "tool/mapped"}},
		{name: "unknown language", finding: finding.Finding{Gate: "known", Language: "rust", RuleID: "tool/mapped"}},
		{name: "missing alias", finding: finding.Finding{Gate: "known", Language: "go", RuleID: "tool/missing"}},
		{name: "dangling page", finding: finding.Finding{Gate: "known", Language: "go", RuleID: "tool/dangling"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, found, err := (Service{Pages: Loader{}, Gates: gateSource{gates: []gate.Gate{known}}}).For(test.finding)
			if err != nil {
				t.Fatal(err)
			}
			if found || page != (Page{}) {
				t.Fatalf("page = %#v, found = %v", page, found)
			}
		})
	}
}

func TestForPropagatesGateSourceError(t *testing.T) {
	want := errors.New("gate source failed")
	_, found, err := (Service{Pages: Loader{}, Gates: gateSource{err: want}}).For(finding.Finding{})
	if !errors.Is(err, want) || found {
		t.Fatalf("err = %v, found = %v", err, found)
	}
}

func TestForPropagatesPageLoadError(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "broken", "not a heading\n")
	service := Service{
		Pages: Loader{OverrideDir: dir},
		Gates: gateSource{gates: []gate.Gate{gatetest.Compile(t, "lint",
			gatetest.Aliases(map[string]string{"tool/rule": "broken"}),
		)}},
	}

	_, found, err := service.For(finding.Finding{Gate: "lint", Language: "go", RuleID: "tool/rule"})
	if err == nil || !strings.Contains(err.Error(), "heading") || found {
		t.Fatalf("err = %v, found = %v", err, found)
	}
}

func TestForPropagatesPageIOError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("# Target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	service := Service{
		Pages: Loader{OverrideDir: dir},
		Gates: gateSource{gates: []gate.Gate{gatetest.Compile(t, "lint",
			gatetest.Aliases(map[string]string{"tool/rule": "linked"}),
		)}},
	}

	_, found, err := service.For(finding.Finding{Gate: "lint", Language: "go", RuleID: "tool/rule"})
	if err == nil || !strings.Contains(err.Error(), "symlink") || found {
		t.Fatalf("err = %v, found = %v", err, found)
	}
}

func TestForRejectsDuplicateLoadedGateIdentity(t *testing.T) {
	first := gatetest.Compile(t, "lint", gatetest.Aliases(map[string]string{"tool/rule": "small-composable-functions"}))
	second := gatetest.Compile(t, "lint", gatetest.Aliases(map[string]string{"tool/rule": "other"}))

	_, found, err := (Service{Pages: Loader{}, Gates: gateSource{gates: []gate.Gate{first, second}}}).For(
		finding.Finding{Gate: "lint", Language: "go", RuleID: "tool/rule"},
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate gate") || found {
		t.Fatalf("err = %v, found = %v", err, found)
	}
}

func TestForRejectsInvalidLoadedGate(t *testing.T) {
	invalid := gate.Gate{Manifest: gate.Manifest{Name: "lint"}}
	_, found, err := (Service{Pages: Loader{}, Gates: gateSource{gates: []gate.Gate{invalid}}}).For(
		finding.Finding{Gate: "lint", Language: "go", RuleID: "tool/rule"},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid gate") || found {
		t.Fatalf("err = %v, found = %v", err, found)
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
	var stdout, stderr bytes.Buffer
	service := Service{
		Pages: Loader{OverrideDir: t.TempDir()},
		Gates: gateSource{gates: []gate.Gate{
			gatetest.Compile(t, "complexity", gatetest.Aliases(map[string]string{"gocyclo/complexity": "small-composable-functions"})),
			gatetest.Compile(t, "lint", gatetest.Aliases(map[string]string{"gocyclo/complexity": "a-different-page"})),
		}},
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
