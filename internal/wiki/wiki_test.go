package wiki

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/gate"
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

// Every alias in a shipped binding should either resolve to a shipped page or
// be a deliberate gap. This pins the deliberate gaps so adding a page, or a
// new dangling alias, is a conscious edit rather than a silent drift.
func TestShippedAliasesResolveExceptKnownGaps(t *testing.T) {
	gates, err := (gate.Loader{}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	loader := Loader{}
	dangling := make([]string, 0)
	for page := range ReverseIndex(gates) {
		exists, err := loader.Exists(page)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			dangling = append(dangling, page)
		}
	}
	if !reflect.DeepEqual(dangling, []string{"lint-correctness"}) {
		t.Fatalf("dangling pages = %v, want only lint-correctness", dangling)
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

func TestLoadAllMergesShippedAndOverrideOnlyPages(t *testing.T) {
	dir := t.TempDir()
	writePage(t, dir, "aardvark", "# Aardvark\n")

	pages, err := (Loader{OverrideDir: dir}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pages))
	for _, page := range pages {
		names = append(names, page.Name)
	}
	if !reflect.DeepEqual(names, []string{"aardvark", "small-composable-functions"}) {
		t.Fatalf("names = %v", names)
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
		page, ok := Resolve(aliases, tc.ruleID)
		if page != tc.page || ok != tc.ok {
			t.Fatalf("Resolve(%q) = (%q, %v), want (%q, %v)", tc.ruleID, page, ok, tc.page, tc.ok)
		}
	}
}

func TestResolveHandlesNoAliases(t *testing.T) {
	if page, ok := Resolve(nil, "gocyclo/complexity"); ok {
		t.Fatalf("Resolve on nil aliases returned %q", page)
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
		fakeGate("complexity", "go", map[string]string{"gocyclo/complexity": "small-composable-functions"}),
		fakeGate("complexity-rs", "rust", map[string]string{"clippy/cognitive_complexity": "small-composable-functions"}),
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
		fakeGate("a", "go", map[string]string{"tool/rule": "page-one"}),
		fakeGate("b", "go", map[string]string{"tool/rule": "page-two"}),
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
		fakeGate("a", "go", map[string]string{"tool/rule": "page-one"}),
		fakeGate("b", "go", map[string]string{"tool/rule": "page-one"}),
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

func fakeGate(name, language string, aliases map[string]string) gate.Gate {
	return gate.Gate{
		Manifest: gate.Manifest{Name: name},
		Bindings: map[string]gate.Binding{language: {Language: language, Aliases: aliases}},
	}
}
