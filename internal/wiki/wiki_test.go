package wiki

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/gate/gatetest"
)

func TestLoadReadsShippedPage(t *testing.T) {
	page, err := (Loader{}).Load("small-composable-functions")
	if err != nil {
		t.Fatal(err)
	}
	if page.Name != "small-composable-functions" {
		t.Fatalf("name = %q", page.Name)
	}
	if page.Title != "Small, composable functions" {
		t.Fatalf("title = %q", page.Title)
	}
	if page.Origin != Shipped {
		t.Fatalf("origin = %q", page.Origin)
	}
	if !strings.HasPrefix(page.Body, "# Small, composable functions\n") {
		t.Fatalf("body does not open with its heading: %.40q", page.Body)
	}
}

// Enumerating the embedded pages rather than naming them means a newly
// shipped page is covered the moment it is added, instead of silently going
// untested until someone remembers to extend a list.
func TestEveryShippedPageLoads(t *testing.T) {
	names := shippedPageNames(t)
	if len(names) < 4 {
		t.Fatalf("shipped pages = %v, want at least the four aliased pages", names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			page, err := (Loader{}).Load(name)
			if err != nil {
				t.Fatal(err)
			}
			if page.Name != name {
				t.Fatalf("name = %q, want %q", page.Name, name)
			}
			if page.Origin != Shipped {
				t.Fatalf("origin = %q, want %q", page.Origin, Shipped)
			}
			if page.Title == "" {
				t.Fatal("title is empty")
			}
			if !strings.HasPrefix(page.Body, "# "+page.Title+"\n") {
				t.Fatalf("body does not open with its heading: %.40q", page.Body)
			}
		})
	}
}

// A brief concatenates a page body verbatim (ADR-0006), so a page missing its
// techniques or constraints hands the fix agent prose with nothing actionable
// in it. The loader deliberately does not interpret markdown beyond the title,
// so this pins the house structure of our own shipped pages rather than
// asserting a format contract that the loader enforces.
func TestShippedPagesCarryTechniquesAndConstraints(t *testing.T) {
	for _, name := range shippedPageNames(t) {
		t.Run(name, func(t *testing.T) {
			page, err := (Loader{}).Load(name)
			if err != nil {
				t.Fatal(err)
			}
			for _, section := range []string{"## Why this matters", "## Techniques", "## Constraints"} {
				if !strings.Contains(page.Body, section) {
					t.Fatalf("page %q has no %q section", name, section)
				}
			}
		})
	}
}

func shippedPageNames(t *testing.T) []string {
	t.Helper()
	entries, err := shipped.ReadDir(shippedRoot)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".md"))
	}
	sort.Strings(names)
	return names
}

// Every alias in a shipped binding must resolve to a shipped page. There are
// deliberately no gaps: an alias pointing at a page nobody wrote gives the fix
// agent a principle-page reference it cannot read, so a new dangling alias has
// to be a conscious edit here rather than silent drift.
func TestShippedAliasesResolveWithoutGaps(t *testing.T) {
	gates, err := (gate.Loader{}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	loader := Loader{}
	dangling := make([]string, 0)
	for page := range ReverseIndex(gates) {
		_, err := loader.Load(page)
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if errors.Is(err, ErrNotFound) {
			dangling = append(dangling, page)
		}
	}
	if !reflect.DeepEqual(dangling, []string{}) {
		t.Fatalf("dangling pages = %v, want none", dangling)
	}
}

func TestLoadPrefersOverride(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "small-composable-functions", "# Mine\n\nLocal wording.\n")

	page, err := (Loader{OverrideDir: dir}).Load("small-composable-functions")
	if err != nil {
		t.Fatal(err)
	}
	if page.Origin != Override {
		t.Fatalf("origin = %q, want override", page.Origin)
	}
	if page.Title != "Mine" {
		t.Fatalf("title = %q", page.Title)
	}
}

func TestLoadFallsBackToShippedWhenOverrideDirLacksPage(t *testing.T) {
	page, err := (Loader{OverrideDir: t.TempDir()}).Load("small-composable-functions")
	if err != nil {
		t.Fatal(err)
	}
	if page.Origin != Shipped {
		t.Fatalf("origin = %q, want shipped", page.Origin)
	}
}

func TestLoadMissingPageReportsNotFound(t *testing.T) {
	_, err := (Loader{}).Load("no-such-page")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLoadRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "page.md"} {
		if _, err := (Loader{}).Load(name); err == nil {
			t.Fatalf("Load(%q) succeeded", name)
		}
	}
}

func TestLoadRejectsSymlinkedOverride(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(target, []byte("# Elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "small-composable-functions.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := (Loader{OverrideDir: dir}).Load("small-composable-functions")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
}

func TestLoadRejectsPageWithoutHeading(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "headless", "No heading here.\n")

	_, err := (Loader{OverrideDir: dir}).Load("headless")
	if err == nil || !strings.Contains(err.Error(), "heading") {
		t.Fatalf("err = %v, want a heading complaint", err)
	}
}

func TestResolvePrefersExactThenLongestGlob(t *testing.T) {
	aliases := map[string]string{
		"golangci-lint/*":        "lint-correctness",
		"golangci-lint/errcheck": "unchecked-errors",
		"golangci-lint/gosec/*":  "security",
		"gocyclo/complexity":     "small-composable-functions",
	}
	cases := []struct {
		ruleID string
		page   string
		ok     bool
	}{
		{"golangci-lint/errcheck", "unchecked-errors", true},
		{"golangci-lint/ineffassign", "lint-correctness", true},
		{"golangci-lint/gosec/G401", "security", true},
		{"gocyclo/complexity", "small-composable-functions", true},
		{"clippy/needless_range_loop", "", false},
	}
	for _, tc := range cases {
		page, ok := resolve(aliases, tc.ruleID)
		if page != tc.page || ok != tc.ok {
			t.Fatalf("resolve(%q) = (%q, %v), want (%q, %v)", tc.ruleID, page, ok, tc.page, tc.ok)
		}
	}
}

func TestResolveHandlesNoAliases(t *testing.T) {
	if page, ok := resolve(nil, "gocyclo/complexity"); ok {
		t.Fatalf("resolve on nil aliases returned %q", page)
	}
}

func TestReverseIndexReportsShippedAliases(t *testing.T) {
	gates, err := (gate.Loader{}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	index := ReverseIndex(gates)
	want := []Ref{{Gate: "complexity", Language: "go", RuleID: "gocyclo/complexity"}}
	if got := index["small-composable-functions"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestReverseIndexIsManyToOne(t *testing.T) {
	gates := []gate.Gate{
		fakeGate(t, "complexity", "go", map[string]string{"gocyclo/complexity": "small-composable-functions"}),
		fakeGate(t, "complexity-rs", "rust", map[string]string{"clippy/cognitive_complexity": "small-composable-functions"}),
	}
	refs := ReverseIndex(gates)["small-composable-functions"]
	if len(refs) != 2 {
		t.Fatalf("refs = %v, want two aliases onto one page", refs)
	}
	if refs[0].Gate != "complexity" || refs[1].Gate != "complexity-rs" {
		t.Fatalf("refs are not gate-ordered: %v", refs)
	}
}

func TestConflictsDetectsOneRuleIDOnTwoPages(t *testing.T) {
	gates := []gate.Gate{
		fakeGate(t, "a", "go", map[string]string{"tool/rule": "page-one"}),
		fakeGate(t, "b", "go", map[string]string{"tool/rule": "page-two"}),
	}
	conflicts := Conflicts(gates)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v, want one", conflicts)
	}
	if !reflect.DeepEqual(conflicts[0].Pages, []string{"page-one", "page-two"}) {
		t.Fatalf("pages = %v", conflicts[0].Pages)
	}
}

func TestConflictsIgnoresAgreeingAliases(t *testing.T) {
	gates := []gate.Gate{
		fakeGate(t, "a", "go", map[string]string{"tool/rule": "page-one"}),
		fakeGate(t, "b", "go", map[string]string{"tool/rule": "page-one"}),
	}
	if conflicts := Conflicts(gates); len(conflicts) != 0 {
		t.Fatalf("conflicts = %v, want none", conflicts)
	}
}

func TestShippedGatesHaveNoConflicts(t *testing.T) {
	gates, err := (gate.Loader{}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if conflicts := Conflicts(gates); len(conflicts) != 0 {
		t.Fatalf("shipped gates conflict: %v", conflicts)
	}
}

func writePage(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+pageSuffix), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeGate(t *testing.T, name, language string, aliases map[string]string) gate.Gate {
	t.Helper()
	return gatetest.Compile(t, name, gatetest.Language(language), gatetest.Aliases(aliases))
}
