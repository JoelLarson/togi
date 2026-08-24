package flywheel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
)

const integrityProduction = "package sample\nfunc calculateTotal(value int) int { return value + 1 }\n"

const integrityTests = `package sample
import "testing"
func TestTotal(t *testing.T) { // existing comment
	if got := calculateTotal(1); got != 2 { t.Fatalf("got %d", got) }
}
func BenchmarkTotal(b *testing.B) { for range b.N { calculateTotal(1) } }
func FuzzTotal(f *testing.F) { f.Add(1); f.Fuzz(func(t *testing.T, value int) { calculateTotal(value) }) }
func Example_calculateTotal() { println(calculateTotal(1)) }
`

func integrityRules(t *testing.T, original, attempted map[string]string) []string {
	t.Helper()
	result := CheckIntegrity(snapshot(original), snapshot(attempted))
	if result.Err != nil {
		t.Fatalf("CheckIntegrity() error = %v", result.Err)
	}
	rules := make([]string, len(result.Findings))
	for index, item := range result.Findings {
		rules[index] = item.RuleID
		if item.Gate != "integrity" || item.File == "" || item.Line < 1 || item.Fingerprint == "" || len(item.Snippet) > 240 {
			t.Fatalf("invalid integrity finding: %#v", item)
		}
	}
	return rules
}

func TestIntegrityDiscoveryMatrix(t *testing.T) {
	tests := []struct {
		name    string
		before  string
		after   string
		blocked bool
	}{
		{"deleted test", "func TestThing(t *testing.T) {}", "", true},
		{"renamed test", "func TestThing(t *testing.T) {}", "func TestOther(t *testing.T) {}", true},
		{"deleted benchmark", "func BenchmarkThing(b *testing.B) {}", "", true},
		{"renamed benchmark", "func BenchmarkThing(b *testing.B) {}", "func BenchmarkOther(b *testing.B) {}", true},
		{"deleted fuzz", "func FuzzThing(f *testing.F) {}", "", true},
		{"renamed fuzz", "func FuzzThing(f *testing.F) {}", "func FuzzOther(f *testing.F) {}", true},
		{"deleted example", "func ExampleThing() {}", "", true},
		{"renamed example", "func ExampleThing() {}", "func ExampleOther() {}", true},
		{"test result invalid", "func TestThing(t *testing.T) {}", "func TestThing(t *testing.T) int { return 1 }", true},
		{"benchmark parameter invalid", "func BenchmarkThing(b *testing.B) {}", "func BenchmarkThing() {}", true},
		{"fuzz parameter invalid", "func FuzzThing(f *testing.F) {}", "func FuzzThing(f testing.F) {}", true},
		{"example parameter invalid", "func ExampleThing() {}", "func ExampleThing(value int) {}", true},
		{"exact test name deleted", "func Test(t *testing.T) {}", "", true},
		{"new test", "", "func TestNew(t *testing.T) {}", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := "package sample\nimport \"testing\"\n" + test.before + "\n"
			after := "package sample\nimport \"testing\"\n" + test.after + "\n"
			rules := integrityRules(t, map[string]string{"sample.go": integrityProduction, "sample_test.go": before}, map[string]string{"sample.go": integrityProduction, "sample_test.go": after})
			blocked := false
			for _, rule := range rules {
				blocked = blocked || rule == "togi/test-discovery"
			}
			if blocked != test.blocked {
				t.Fatalf("rules = %v; discovery blocked = %v, want %v", rules, blocked, test.blocked)
			}
		})
	}
}

func TestIntegrityDiscoveryRejectsBuildSuffixExclusion(t *testing.T) {
	original := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n",
	}
	attempted := map[string]string{
		"sample.go":              integrityProduction,
		"sample_windows_test.go": original["sample_test.go"],
	}
	if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/test-discovery") {
		t.Fatalf("rules = %v, want togi/test-discovery", rules)
	}
}

func TestIntegrityDiscoveryRejectsIgnoredGoPaths(t *testing.T) {
	original := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n",
	}
	for name, attemptedPath := range map[string]string{
		"underscore file": "_sample_test.go",
		"dot file":        ".sample_test.go",
		"underscore dir":  "_ignored/sample_test.go",
		"dot dir":         ".ignored/sample_test.go",
	} {
		t.Run(name, func(t *testing.T) {
			attempted := map[string]string{"sample.go": integrityProduction, attemptedPath: original["sample_test.go"]}
			if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/test-discovery") {
				t.Fatalf("rules = %v, want togi/test-discovery", rules)
			}
		})
	}
}

func TestIntegrityDiscoveryRejectsWildcardExcludedPackages(t *testing.T) {
	before := map[string]string{
		"go.mod":               "module example.invalid/project\n\ngo 1.25\n",
		"oldpkg/value.go":      "package oldpkg\nfunc Value() {}\n",
		"oldpkg/value_test.go": "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { Value() }\n",
	}
	for name, prefix := range map[string]string{"vendor": "vendor/newpkg", "nested module": "nested/newpkg"} {
		t.Run(name, func(t *testing.T) {
			after := map[string]string{
				"go.mod":                  before["go.mod"],
				prefix + "/value.go":      "package newpkg\nfunc Value() {}\n",
				prefix + "/value_test.go": "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { Value() }\n",
			}
			if name == "nested module" {
				after["nested/go.mod"] = "module example.invalid/nested\n\ngo 1.25\n"
			}
			if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-discovery") {
				t.Fatalf("rules = %v, want wildcard-exclusion discovery finding", rules)
			}
		})
	}
}

func TestIntegrityDiscoveryRecognizesTestingImportAlias(t *testing.T) {
	original := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport testkit \"testing\"\nfunc TestThing(t *testkit.T) {}\n",
	}
	attempted := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport testkit \"testing\"\n",
	}
	if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/test-discovery") {
		t.Fatalf("rules = %v, want togi/test-discovery", rules)
	}
}

func TestIntegrityDiscoveryRejectsPackageAndBuildExclusion(t *testing.T) {
	before := map[string]string{"sample.go": integrityProduction, "sample_test.go": "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n"}
	for name, changed := range map[string]string{
		"package exclusion": "package other\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n",
		"build exclusion":   "//go:build never\n\npackage sample\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			rules := integrityRules(t, before, map[string]string{"sample.go": integrityProduction, "sample_test.go": changed})
			if !containsRule(rules, "togi/test-discovery") {
				t.Fatalf("rules = %v, want togi/test-discovery", rules)
			}
		})
	}
}

func TestIntegrityRejectsSkipInsertion(t *testing.T) {
	before := map[string]string{"sample.go": integrityProduction, "sample_test.go": "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n"}
	after := map[string]string{"sample.go": integrityProduction, "sample_test.go": "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T) { t.Skip(\"later\") }\n"}
	rules := integrityRules(t, before, after)
	if !containsRule(rules, "togi/new-suppression") {
		t.Fatalf("rules = %v, want togi/new-suppression", rules)
	}
}

func TestFixtureIntegrity(t *testing.T) {
	original := map[string]string{"sample.go": integrityProduction, "sample_test.go": integrityTests, "testdata/input.txt": "before\n"}
	for name, mutate := range map[string]func(map[string]string){
		"changed": func(files map[string]string) { files["testdata/input.txt"] = "after\n" },
		"deleted": func(files map[string]string) { delete(files, "testdata/input.txt") },
	} {
		t.Run(name, func(t *testing.T) {
			attempted := cloneStrings(original)
			mutate(attempted)
			if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/test-fixture") {
				t.Fatalf("rules = %v, want togi/test-fixture", rules)
			}
		})
	}
	attempted := cloneStrings(original)
	attempted["testdata/new.txt"] = "new\n"
	if rules := integrityRules(t, original, attempted); containsRule(rules, "togi/test-fixture") {
		t.Fatalf("new fixture rules = %v, want allowed", rules)
	}
}

func TestIntegrityBehaviorMatrix(t *testing.T) {
	beforeBody := `func TestTotal(t *testing.T) { // existing comment
	if got := calculateTotal(1); got != 2 { t.Fatalf("got %d", got) }
}`
	changes := map[string]string{
		"literal":      strings.Replace(beforeBody, "calculateTotal(1)", "calculateTotal(3)", 1),
		"expectation":  strings.Replace(beforeBody, "got != 2", "got != 3", 1),
		"operator":     strings.Replace(beforeBody, "got != 2", "got == 2", 1),
		"callee":       strings.Replace(beforeBody, "calculateTotal(1)", "other(1)", 1),
		"argument":     strings.Replace(beforeBody, "calculateTotal(1)", "calculateTotal(1, 2)", 1),
		"assertion":    strings.Replace(beforeBody, "t.Fatalf", "t.Errorf", 1),
		"comment":      strings.Replace(beforeBody, "existing comment", "rewritten comment", 1),
		"control flow": strings.Replace(beforeBody, "if got := calculateTotal(1); got != 2", "if got := calculateTotal(1); false", 1),
		"statement order": `func TestTotal(t *testing.T) { // existing comment
	calculateTotal(1)
	if got := calculateTotal(1); got != 2 { t.Fatalf("got %d", got) }
}`,
		"case": strings.Replace(beforeBody, "calculateTotal(1)", "calculateTotal(2)", 1),
	}
	for name, changed := range changes {
		t.Run(name, func(t *testing.T) {
			before := "package sample\nimport \"testing\"\n" + beforeBody + "\n"
			after := "package sample\nimport \"testing\"\n" + changed + "\n"
			rules := integrityRules(t, map[string]string{"sample.go": integrityProduction, "sample_test.go": before}, map[string]string{"sample.go": integrityProduction, "sample_test.go": after})
			if !containsRule(rules, "togi/test-behavior") {
				t.Fatalf("rules = %v, want togi/test-behavior", rules)
			}
		})
	}
}

func TestIntegrityFormattingOnlyPasses(t *testing.T) {
	before := "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T){calculateTotal(1)}\n"
	after := "package sample\n\nimport (\n\t\"testing\"\n)\n\nfunc TestThing(t *testing.T) {\n\tcalculateTotal(1)\n}\n"
	rules := integrityRules(t, map[string]string{"sample.go": integrityProduction, "sample_test.go": before}, map[string]string{"sample.go": integrityProduction, "sample_test.go": after})
	if len(rules) != 0 {
		t.Fatalf("formatting-only rules = %v, want none", rules)
	}
}

func TestIntegrityImportTargetChangeIsBehavior(t *testing.T) {
	before := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport (\"testing\"; target \"example.invalid/original\")\nfunc TestThing(t *testing.T) { target.Check() }\n",
	}
	after := cloneStrings(before)
	after["sample_test.go"] = strings.Replace(before["sample_test.go"], "example.invalid/original", "example.invalid/replacement", 1)
	if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
		t.Fatalf("rules = %v, want togi/test-behavior", rules)
	}
}

func TestIntegrityProtectsTestHelpersAndSideEffectImports(t *testing.T) {
	before := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport \"testing\"\nfunc helper() int { return 2 }\nfunc TestThing(t *testing.T) { if helper() != 2 { t.Fatal(\"bad\") } }\n",
	}
	for name, changed := range map[string]string{
		"helper body":        "package sample\nimport \"testing\"\nfunc helper() int { return 3 }\nfunc TestThing(t *testing.T) { if helper() != 2 { t.Fatal(\"bad\") } }\n",
		"side effect import": "package sample\nimport (\"testing\"; _ \"example.invalid/sideeffect\")\nfunc helper() int { return 2 }\nfunc TestThing(t *testing.T) { if helper() != 2 { t.Fatal(\"bad\") } }\n",
		"package comment":    "// rewritten package note\npackage sample\nimport \"testing\"\nfunc helper() int { return 2 }\nfunc TestThing(t *testing.T) { if helper() != 2 { t.Fatal(\"bad\") } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			baseline := before
			if name == "package comment" {
				baseline = cloneStrings(before)
				baseline["sample_test.go"] = "// original package note\n" + baseline["sample_test.go"]
			}
			if rules := integrityRules(t, baseline, map[string]string{"sample.go": integrityProduction, "sample_test.go": changed}); !containsRule(rules, "togi/test-behavior") {
				t.Fatalf("rules = %v, want togi/test-behavior", rules)
			}
		})
	}
}

func TestIntegrityRejectsNewTestHarnessControl(t *testing.T) {
	before := map[string]string{
		"sample.go":      integrityProduction,
		"sample_test.go": "package sample\nimport \"testing\"\nfunc TestThing(t *testing.T) {}\n",
	}
	for name, declaration := range map[string]string{
		"TestMain": "func TestMain(m *testing.M) {}\n",
		"init":     "func init() {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			attempted := cloneStrings(before)
			attempted["sample_test.go"] += declaration
			if rules := integrityRules(t, before, attempted); !containsRule(rules, "togi/test-behavior") {
				t.Fatalf("rules = %v, want togi/test-behavior", rules)
			}
		})
	}
}

func TestIntegrityProtectsImportOnlyTestFile(t *testing.T) {
	before := map[string]string{
		"sample.go":     integrityProduction,
		"hooks_test.go": "package sample\nimport _ \"example.invalid/hook\"\n",
	}
	if rules := integrityRules(t, before, map[string]string{"sample.go": integrityProduction}); !containsRule(rules, "togi/test-behavior") {
		t.Fatalf("rules = %v, want togi/test-behavior", rules)
	}
}

func TestWitnessedRenameMatrix(t *testing.T) {
	t.Run("production declaration and matching test call", func(t *testing.T) {
		before := map[string]string{"sample.go": integrityProduction, "sample_test.go": integrityTests}
		after := map[string]string{
			"sample.go":      "package sample\nfunc totalFor(value int) int { return value + 1 }\n",
			"sample_test.go": renamedTestReferences(integrityTests),
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("witnessed rename rules = %v, want none", rules)
		}
	})
	t.Run("recursive production declaration and matching test call", func(t *testing.T) {
		before := map[string]string{
			"sample.go":      "package sample\nfunc Old(n int) int { if n == 0 { return 0 }; return Old(n-1) }\n",
			"sample_test.go": "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Old(2) != 0 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"sample.go":      "package sample\nfunc New(n int) int { if n == 0 { return 0 }; return New(n-1) }\n",
			"sample_test.go": strings.Replace(before["sample_test.go"], "Old(2)", "New(2)", 1),
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("recursive witness rules = %v, want none", rules)
		}
	})
	t.Run("ignored declaration cannot steal test binding", func(t *testing.T) {
		before := map[string]string{
			"_poison.go":     "package sample\nfunc Old() {}\n",
			"sample.go":      "package sample\nfunc Old() {}\n",
			"sample_test.go": "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { Old() }\n",
		}
		after := map[string]string{
			"_poison.go":     before["_poison.go"],
			"sample.go":      "package sample\nfunc New() {}\n",
			"sample_test.go": strings.Replace(before["sample_test.go"], "Old()", "New()", 1),
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("ignored binding rules = %v, want none", rules)
		}
	})
	t.Run("same-spelled dependency selector is not a witness", func(t *testing.T) {
		before := map[string]string{
			"sample.go":      "package sample\nimport \"example.invalid/dep\"\nfunc Old() { dep.Old() }\n",
			"sample_test.go": "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { Old() }\n",
		}
		after := map[string]string{
			"sample.go":      "package sample\nimport \"example.invalid/dep\"\nfunc New() { dep.New() }\n",
			"sample_test.go": strings.Replace(before["sample_test.go"], "Old()", "New()", 1),
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want no false production witness", rules)
		}
	})
	t.Run("test local same-spelling is not remapped", func(t *testing.T) {
		before := map[string]string{
			"sample.go":      "package sample\nfunc Old() {}\n",
			"sample_test.go": "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { Old := func() {}; Old() }\n",
		}
		after := map[string]string{
			"sample.go":      "package sample\nfunc New() {}\n",
			"sample_test.go": strings.ReplaceAll(before["sample_test.go"], "Old", "New"),
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want local binding behavior finding", rules)
		}
	})
	t.Run("local shadows authenticated import alias", func(t *testing.T) {
		before := map[string]string{
			"go.mod":         "module example.invalid/project\n\ngo 1.25\n",
			"dep/value.go":   "package dep\nfunc Old() {}\n",
			"sample_test.go": "package sample\nimport (\"testing\"; dep \"example.invalid/project/dep\")\nfunc TestValue(t *testing.T) { dep := struct{ Old func() }{}; dep.Old() }\n",
		}
		after := map[string]string{
			"go.mod":         before["go.mod"],
			"dep/value.go":   "package dep\nfunc New() {}\n",
			"sample_test.go": strings.ReplaceAll(before["sample_test.go"], "Old", "New"),
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want shadowed alias behavior finding", rules)
		}
	})
	t.Run("authenticated imported declaration rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go":           "package lib\nfunc Old() {}\n",
			"consumer/value_test.go": "package consumer\nimport (\"testing\"; \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { lib.Old() }\n",
		}
		after := map[string]string{
			"go.mod":                 before["go.mod"],
			"lib/value.go":           "package lib\nfunc New() {}\n",
			"consumer/value_test.go": strings.Replace(before["consumer/value_test.go"], "lib.Old", "lib.New", 1),
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("imported declaration witness rules = %v, want none", rules)
		}
	})
	t.Run("production package and matching external test import", func(t *testing.T) {
		before := map[string]string{
			"go.mod":            "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package oldpkg_test\nimport (\"testing\"; oldpkg \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { if oldpkg.Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":            before["go.mod"],
			"lib/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package newpkg_test\nimport (\"testing\"; newpkg \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { if newpkg.Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("package rename rules = %v, want none", rules)
		}
	})
	for _, test := range []struct{ name, beforeImport, afterImport, beforeQualifier, afterQualifier string }{
		{"explicit aliases", `legacy "example.invalid/project/lib"`, `modern "example.invalid/project/lib"`, "legacy", "modern"},
		{"default to explicit", `"example.invalid/project/lib"`, `modern "example.invalid/project/lib"`, "oldpkg", "modern"},
		{"explicit to default", `legacy "example.invalid/project/lib"`, `"example.invalid/project/lib"`, "legacy", "newpkg"},
	} {
		t.Run("package rename "+test.name, func(t *testing.T) {
			before := map[string]string{
				"go.mod":            "module example.invalid/project\n\ngo 1.25\n",
				"lib/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
				"lib/value_test.go": "package oldpkg_test\nimport (\"testing\"; " + test.beforeImport + ")\nfunc TestValue(t *testing.T) { if " + test.beforeQualifier + ".Value() != 1 { t.Fatal(\"bad\") } }\n",
			}
			after := map[string]string{
				"go.mod":            before["go.mod"],
				"lib/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
				"lib/value_test.go": "package newpkg_test\nimport (\"testing\"; " + test.afterImport + ")\nfunc TestValue(t *testing.T) { if " + test.afterQualifier + ".Value() != 1 { t.Fatal(\"bad\") } }\n",
			}
			if rules := integrityRules(t, before, after); len(rules) != 0 {
				t.Fatalf("alias repair rules = %v, want none", rules)
			}
		})
	}
	t.Run("package alias repair cannot switch to local shadow", func(t *testing.T) {
		before := map[string]string{
			"go.mod":       "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go": "package oldpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package oldpkg_test\nimport (\"testing\"; legacy \"example.invalid/project/lib\")\n" +
				"var keep = legacy.Value\ntype fake struct { Value func() int }\n" +
				"func TestValue(t *testing.T) { modern := fake{Value: func() int { return 1 }}; if legacy.Value() != 1 || modern.Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":       before["go.mod"],
			"lib/value.go": "package newpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package newpkg_test\nimport (\"testing\"; modern \"example.invalid/project/lib\")\n" +
				"var keep = modern.Value\ntype fake struct { Value func() int }\n" +
				"func TestValue(t *testing.T) { modern := fake{Value: func() int { return 1 }}; if modern.Value() != 1 || modern.Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want attempted shadow binding blocked", rules)
		}
	})
	t.Run("blank import cannot become named", func(t *testing.T) {
		before := map[string]string{
			"go.mod":            "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package oldpkg_test\nimport (\"testing\"; _ \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"go.mod":            before["go.mod"],
			"lib/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package newpkg_test\nimport (\"testing\"; modern \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { modern.Value() }\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want conservative blank-import finding", rules)
		}
	})
	t.Run("dot import remains dot during package rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":            "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package oldpkg_test\nimport (\"testing\"; . \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":            before["go.mod"],
			"lib/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package newpkg_test\nimport (\"testing\"; . \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("dot-import package rename rules = %v, want none", rules)
		}
	})
	t.Run("dot import cannot become named", func(t *testing.T) {
		before := map[string]string{
			"go.mod":            "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package oldpkg_test\nimport (\"testing\"; . \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { Value() }\n",
		}
		after := map[string]string{
			"go.mod":            before["go.mod"],
			"lib/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
			"lib/value_test.go": "package newpkg_test\nimport (\"testing\"; modern \"example.invalid/project/lib\")\nfunc TestValue(t *testing.T) { modern.Value() }\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want conservative dot-import finding", rules)
		}
	})
	t.Run("production package move and matching import path", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/value.go":        "package oldpkg\nfunc Value() int { return 1 }\n",
			"consumer/value_test.go": "package consumer_test\nimport (\"testing\"; oldpkg \"example.invalid/project/oldpkg\")\nfunc TestValue(t *testing.T) { if oldpkg.Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":                 before["go.mod"],
			"newpkg/value.go":        "package newpkg\nfunc Value() int { return 1 }\n",
			"consumer/value_test.go": "package consumer_test\nimport (\"testing\"; newpkg \"example.invalid/project/newpkg\")\nfunc TestValue(t *testing.T) { if newpkg.Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("package move rules = %v, want none", rules)
		}
	})
	t.Run("composed package directory and declaration rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/value.go":        "package oldpkg\nfunc Old() int { return 1 }\n",
			"consumer/value_test.go": "package consumer\nimport (\"testing\"; legacy \"example.invalid/project/oldpkg\")\nfunc TestValue(t *testing.T) { if legacy.Old() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":                 before["go.mod"],
			"newpkg/value.go":        "package newpkg\nfunc New() int { return 1 }\n",
			"consumer/value_test.go": "package consumer\nimport (\"testing\"; modern \"example.invalid/project/newpkg\")\nfunc TestValue(t *testing.T) { if modern.New() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("composed witness rules = %v, want none", rules)
		}
	})
	t.Run("composed multi-file package and declaration rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":               "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/first.go":      "package oldpkg\nfunc Old() int { return 1 }\n",
			"oldpkg/second.go":     "package oldpkg\nfunc Use() int { return Old() }\n",
			"oldpkg/value_test.go": "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Old() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":               before["go.mod"],
			"newpkg/first.go":      "package newpkg\nfunc New() int { return 1 }\n",
			"newpkg/second.go":     "package newpkg\nfunc Use() int { return New() }\n",
			"newpkg/value_test.go": "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { if New() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("multi-file composed witness rules = %v, want none", rules)
		}
	})
	for name, afterProduction := range map[string]map[string]string{
		"body change": {
			"newpkg/value.go": "package newpkg\nfunc New() int { return 2 }\n",
		},
		"ambiguous directory": {
			"newpkg/value.go": "package newpkg\nfunc New() int { return 1 }\n",
			"other/value.go":  "package other\nfunc New() int { return 1 }\n",
		},
	} {
		t.Run("composed rename rejects "+name, func(t *testing.T) {
			before := map[string]string{
				"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
				"oldpkg/value.go":        "package oldpkg\nfunc Old() int { return 1 }\n",
				"consumer/value_test.go": "package consumer\nimport (\"testing\"; legacy \"example.invalid/project/oldpkg\")\nfunc TestValue(t *testing.T) { legacy.Old() }\n",
			}
			after := map[string]string{"go.mod": before["go.mod"], "consumer/value_test.go": "package consumer\nimport (\"testing\"; modern \"example.invalid/project/newpkg\")\nfunc TestValue(t *testing.T) { modern.New() }\n"}
			for file, source := range afterProduction {
				after[file] = source
			}
			if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") && !containsRule(rules, "togi/test-discovery") {
				t.Fatalf("rules = %v, want composed rename blocked", rules)
			}
		})
	}
	t.Run("production package move carries co-located tests", func(t *testing.T) {
		before := map[string]string{
			"go.mod":               "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"oldpkg/value_test.go": "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":               before["go.mod"],
			"newpkg/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
			"newpkg/value_test.go": "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(\"bad\") } }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("co-located package move rules = %v, want none", rules)
		}
	})
	t.Run("partial production package move is not witnessed", func(t *testing.T) {
		before := map[string]string{
			"go.mod":               "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/doc.go":        "// Package values documents values.\npackage oldpkg\n",
			"oldpkg/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"oldpkg/value_test.go": "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"go.mod":               before["go.mod"],
			"newpkg/doc.go":        "// Package values documents values.\npackage newpkg\n",
			"oldpkg/value.go":      before["oldpkg/value.go"],
			"newpkg/value_test.go": "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-discovery") {
			t.Fatalf("rules = %v, want partial package move blocked", rules)
		}
	})
	t.Run("remaining build variant prevents complete move", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/value.go":         "package oldpkg\nfunc Value() int { return 1 }\n",
			"oldpkg/value_windows.go": "package oldpkg\nfunc WindowsOnly() {}\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { Value() }\n",
		}
		after := map[string]string{
			"go.mod":                  before["go.mod"],
			"newpkg/value.go":         "package newpkg\nfunc Value() int { return 1 }\n",
			"oldpkg/value_windows.go": before["oldpkg/value_windows.go"],
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { Value() }\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-discovery") {
			t.Fatalf("build-variant move rules = %v, want blocked", rules)
		}
	})
	t.Run("all build variants permit complete move", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/value_linux.go":   "package oldpkg\nfunc LinuxValue() int { return 1 }\n",
			"oldpkg/value_windows.go": "package oldpkg\nfunc WindowsValue() int { return 1 }\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"go.mod":                  before["go.mod"],
			"newpkg/value_linux.go":   strings.Replace(before["oldpkg/value_linux.go"], "package oldpkg", "package newpkg", 1),
			"newpkg/value_windows.go": strings.Replace(before["oldpkg/value_windows.go"], "package oldpkg", "package newpkg", 1),
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("complete build-variant move rules = %v, want none", rules)
		}
	})
	t.Run("retained old declaration in another variant blocks global rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"sample/value_linux.go":   "package sample\nfunc Old() int { return 1 }\n",
			"sample/value_windows.go": "package sample\nfunc Old() int { return 1 }\n",
			"consumer/value_test.go":  "package consumer\nimport (\"testing\"; \"example.invalid/project/sample\")\nfunc TestValue(t *testing.T) { sample.Old() }\n",
		}
		after := cloneStrings(before)
		after["sample/value_linux.go"] = "package sample\nfunc New() int { return 1 }\n"
		after["consumer/value_test.go"] = "package consumer\nimport (\"testing\"; \"example.invalid/project/sample\")\nfunc TestValue(t *testing.T) { sample.New() }\n"
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("partial variant rename rules = %v, want behavior blocked", rules)
		}
	})
	t.Run("all declaration variants permit consistent global rename", func(t *testing.T) {
		before := map[string]string{
			"value_linux.go":   "package sample\nfunc Old() int { return 1 }\n",
			"value_windows.go": "package sample\nfunc Old() int { return 1 }\n",
			"value_test.go":    "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { Old() }\n",
		}
		after := map[string]string{
			"value_linux.go":   "package sample\nfunc New() int { return 1 }\n",
			"value_windows.go": "package sample\nfunc New() int { return 1 }\n",
			"value_test.go":    "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { New() }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("complete variant rename rules = %v, want none", rules)
		}
	})
	t.Run("overlapping custom variants cannot publish global rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
			"sample/value_tag1.go":   "//go:build tag1\n\npackage sample\nfunc Old() int { return 1 }\n",
			"sample/value_tag2.go":   "//go:build tag2\n\npackage sample\nfunc Old(value int) int { return value }\n",
			"consumer/value_test.go": "package consumer\nimport (\"testing\"; \"example.invalid/project/sample\")\nfunc TestValue(t *testing.T) { sample.Old() }\n",
		}
		after := cloneStrings(before)
		after["sample/value_tag1.go"] = strings.Replace(before["sample/value_tag1.go"], "Old", "New", 1)
		after["sample/value_tag2.go"] = strings.Replace(before["sample/value_tag2.go"], "Old", "New", 1)
		after["consumer/value_test.go"] = strings.Replace(before["consumer/value_test.go"], "sample.Old", "sample.New", 1)
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("overlapping variant rules = %v, want behavior blocked", rules)
		}
	})
	t.Run("mixed variant targets cannot publish global rename", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"sample/value_linux.go":   "package sample\nfunc Old() int { return 1 }\n",
			"sample/value_windows.go": "package sample\nfunc Old() int { return 1 }\n",
			"consumer/value_test.go":  "package consumer\nimport (\"testing\"; \"example.invalid/project/sample\")\nfunc TestValue(t *testing.T) { sample.Old() }\n",
		}
		after := cloneStrings(before)
		after["sample/value_linux.go"] = "package sample\nfunc New() int { return 1 }\n"
		after["sample/value_windows.go"] = "package sample\nfunc Modern() int { return 1 }\n"
		after["consumer/value_test.go"] = strings.Replace(before["consumer/value_test.go"], "sample.Old", "sample.New", 1)
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("mixed variant rules = %v, want behavior blocked", rules)
		}
	})
	t.Run("inactive declaration rename composes with complete move", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/common.go":        "package oldpkg\nfunc Common() int { return 1 }\n",
			"oldpkg/value_windows.go": "package oldpkg\nfunc Old(value int) int { if value == 0 { return 0 }; return Old(value - 1) }\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { _ = Common() }\n",
		}
		after := map[string]string{
			"go.mod":                  before["go.mod"],
			"newpkg/common.go":        strings.Replace(before["oldpkg/common.go"], "package oldpkg", "package newpkg", 1),
			"newpkg/value_windows.go": strings.ReplaceAll(strings.Replace(before["oldpkg/value_windows.go"], "package oldpkg", "package newpkg", 1), "Old", "New"),
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { _ = Common() }\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("inactive composed move rules = %v, want none", rules)
		}
	})
	t.Run("leftover inactive variant prevents move", func(t *testing.T) {
		before := map[string]string{
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_windows.go": "package oldpkg\nfunc Old() {}\n",
			"oldpkg/keep_windows.go":  "package oldpkg\nfunc Keep() {}\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"newpkg/common.go":        "package newpkg\nfunc Common() {}\n",
			"newpkg/value_windows.go": "package newpkg\nfunc New() {}\n",
			"oldpkg/keep_windows.go":  before["oldpkg/keep_windows.go"],
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-discovery") {
			t.Fatalf("leftover inactive rules = %v, want blocked", rules)
		}
	})
	t.Run("ambiguous inactive variants prevent move", func(t *testing.T) {
		before := map[string]string{
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_windows.go": "package oldpkg\nfunc Old() {}\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"newpkg/common.go":        "package newpkg\nfunc Common() {}\n",
			"newpkg/value_windows.go": "package newpkg\nfunc New() {}\n",
			"newpkg/other_windows.go": "package newpkg\nfunc New() {}\n",
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-discovery") {
			t.Fatalf("ambiguous inactive rules = %v, want blocked", rules)
		}
	})
	t.Run("invalid inactive variant fails closed", func(t *testing.T) {
		before := map[string]string{
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_windows.go": "package oldpkg\nfunc Old() {}\n",
		}
		after := map[string]string{
			"newpkg/common.go":        "package newpkg\nfunc Common() {}\n",
			"newpkg/value_windows.go": "package newpkg\nfunc New(\n",
		}
		if result := CheckIntegrity(snapshot(before), snapshot(after)); result.Err == nil {
			t.Fatal("CheckIntegrity error = nil, want malformed inactive variant rejected")
		}
	})
	t.Run("inactive local dependency composes with move", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"dep/value_windows.go":    "package dep\nfunc Value(value int) int { return value }\n",
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_windows.go": "package oldpkg\nimport \"example.invalid/project/dep\"\nfunc Old(value int) int { if value == 0 { return dep.Value(value) }; return Old(value - 1) }\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"go.mod":                  before["go.mod"],
			"dep/value_windows.go":    before["dep/value_windows.go"],
			"newpkg/common.go":        "package newpkg\nfunc Common() {}\n",
			"newpkg/value_windows.go": strings.ReplaceAll(strings.Replace(before["oldpkg/value_windows.go"], "package oldpkg", "package newpkg", 1), "Old", "New"),
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("inactive dependency move rules = %v, want none", rules)
		}
	})
	t.Run("inactive disjunctive variant selects compatible dependency", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"dep/value_windows.go":    "package dep\nfunc Value(value int) int { return value }\n",
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_variant.go": "//go:build windows || plan9\n\npackage oldpkg\nimport \"example.invalid/project/dep\"\nfunc Old(value int) int { return dep.Value(value) }\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := cloneStrings(before)
		delete(after, "oldpkg/common.go")
		delete(after, "oldpkg/value_variant.go")
		delete(after, "oldpkg/value_test.go")
		after["newpkg/common.go"] = "package newpkg\nfunc Common() {}\n"
		after["newpkg/value_variant.go"] = strings.ReplaceAll(strings.Replace(before["oldpkg/value_variant.go"], "package oldpkg", "package newpkg", 1), "Old", "New")
		after["newpkg/value_test.go"] = "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n"
		if rules := integrityRules(t, before, after); containsRule(rules, "togi/test-behavior") || containsRule(rules, "togi/test-discovery") {
			t.Fatalf("inactive disjunctive rules = %v, want package move witnessed", rules)
		}
	})
	t.Run("inactive negated variant selects snapshot dependency platform", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"dep/value_windows.go":    "package dep\nfunc Value(value int) int { return value }\n",
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_variant.go": "//go:build !linux\n\npackage oldpkg\nimport \"example.invalid/project/dep\"\nfunc Old(value int) int { return dep.Value(value) }\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := cloneStrings(before)
		delete(after, "oldpkg/common.go")
		delete(after, "oldpkg/value_variant.go")
		delete(after, "oldpkg/value_test.go")
		after["newpkg/common.go"] = "package newpkg\nfunc Common() {}\n"
		after["newpkg/value_variant.go"] = strings.ReplaceAll(strings.Replace(before["oldpkg/value_variant.go"], "package oldpkg", "package newpkg", 1), "Old", "New")
		after["newpkg/value_test.go"] = "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n"
		if rules := integrityRules(t, before, after); containsRule(rules, "togi/test-behavior") || containsRule(rules, "togi/test-discovery") {
			t.Fatalf("inactive negated rules = %v, want package move witnessed", rules)
		}
	})
	t.Run("inactive filename wildcard does not consume variant budget", func(t *testing.T) {
		before := map[string]string{
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_windows.go": "//go:build tag1 || tag2 || tag3 || tag4\n\npackage oldpkg\nfunc Old() {}\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := map[string]string{
			"newpkg/common.go":        "package newpkg\nfunc Common() {}\n",
			"newpkg/value_windows.go": "//go:build tag1 || tag2 || tag3 || tag4\n\npackage newpkg\nfunc New() {}\n",
			"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		result := CheckIntegrity(snapshot(before), snapshot(after))
		if result.Err != nil {
			t.Fatalf("CheckIntegrity error = %v, want bounded variant analysis", result.Err)
		}
	})
	t.Run("inactive multi-hop dependency composes with move", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
			"leaf/value_windows.go":   "package leaf\nfunc Value(value int) int { return value }\n",
			"dep/value_windows.go":    "package dep\nimport \"example.invalid/project/leaf\"\nfunc Value(value int) int { return leaf.Value(value) }\n",
			"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
			"oldpkg/value_windows.go": "package oldpkg\nimport \"example.invalid/project/dep\"\nfunc Old(value int) int { return dep.Value(value) }\n",
			"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
		}
		after := cloneStrings(before)
		delete(after, "oldpkg/common.go")
		delete(after, "oldpkg/value_windows.go")
		delete(after, "oldpkg/value_test.go")
		after["newpkg/common.go"] = "package newpkg\nfunc Common() {}\n"
		after["newpkg/value_windows.go"] = strings.ReplaceAll(strings.Replace(before["oldpkg/value_windows.go"], "package oldpkg", "package newpkg", 1), "Old", "New")
		after["newpkg/value_test.go"] = "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n"
		if rules := integrityRules(t, before, after); len(rules) != 0 {
			t.Fatalf("inactive multi-hop rules = %v, want none", rules)
		}
	})
	for name, mutate := range map[string]func(map[string]string){
		"missing dependency": func(after map[string]string) { delete(after, "dep/value_windows.go") },
		"ambiguous dependency": func(after map[string]string) {
			after["dep/other_windows.go"] = "package dep\nfunc Value(value int) int { return value }\n"
		},
		"cyclic dependency": func(after map[string]string) {
			after["dep/value_windows.go"] = "package dep\nimport \"example.invalid/project/newpkg\"\nfunc Value(value int) int { newpkg.New(value); return value }\n"
		},
	} {
		t.Run("inactive move rejects "+name, func(t *testing.T) {
			before := map[string]string{
				"go.mod":                  "module example.invalid/project\n\ngo 1.25\n",
				"dep/value_windows.go":    "package dep\nfunc Value(value int) int { return value }\n",
				"oldpkg/common.go":        "package oldpkg\nfunc Common() {}\n",
				"oldpkg/value_windows.go": "package oldpkg\nimport \"example.invalid/project/dep\"\nfunc Old(value int) int { return dep.Value(value) }\n",
				"oldpkg/value_test.go":    "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
			}
			after := map[string]string{
				"go.mod":                  before["go.mod"],
				"dep/value_windows.go":    before["dep/value_windows.go"],
				"newpkg/common.go":        "package newpkg\nfunc Common() {}\n",
				"newpkg/value_windows.go": "package newpkg\nimport \"example.invalid/project/dep\"\nfunc New(value int) int { return dep.Value(value) }\n",
				"newpkg/value_test.go":    "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) {}\n",
			}
			mutate(after)
			result := CheckIntegrity(snapshot(before), snapshot(after))
			if result.Err == nil {
				rules := make([]string, len(result.Findings))
				for index, item := range result.Findings {
					rules[index] = item.RuleID
				}
				if !containsRule(rules, "togi/test-discovery") && !containsRule(rules, "togi/test-behavior") {
					t.Fatalf("rules = %v, want inactive dependency failure blocked", rules)
				}
			}
		})
	}
	t.Run("package move into ignored directory is not witnessed", func(t *testing.T) {
		before := map[string]string{
			"go.mod":               "module example.invalid/project\n\ngo 1.25\n",
			"oldpkg/value.go":      "package oldpkg\nfunc Value() int { return 1 }\n",
			"oldpkg/value_test.go": "package oldpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { Value() }\n",
		}
		after := map[string]string{
			"go.mod":                before["go.mod"],
			"_newpkg/value.go":      "package newpkg\nfunc Value() int { return 1 }\n",
			"_newpkg/value_test.go": "package newpkg\nimport \"testing\"\nfunc TestValue(t *testing.T) { Value() }\n",
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-discovery") {
			t.Fatalf("rules = %v, want ignored-directory discovery finding", rules)
		}
	})
	t.Run("third-party suffix cannot borrow local witness", func(t *testing.T) {
		before := map[string]string{
			"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
			"foo/value.go":           "package foo\nfunc Old() int { return 1 }\n",
			"consumer/value_test.go": "package consumer\nimport (\"testing\"; external \"third.party/foo\")\nfunc TestValue(t *testing.T) { if external.Old() != 2 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"go.mod":                 before["go.mod"],
			"foo/value.go":           "package foo\nfunc New() int { return 1 }\n",
			"consumer/value_test.go": strings.Replace(before["consumer/value_test.go"], "external.Old", "external.New", 1),
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want authenticated behavior finding", rules)
		}
	})
	t.Run("rename witness stays in its production package", func(t *testing.T) {
		before := map[string]string{
			"a/value.go":      "package a\nfunc Old() int { return 1 }\n",
			"b/value.go":      "package b\nfunc Old() int { return 2 }\nfunc New() int { return 3 }\n",
			"b/value_test.go": "package b\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Old() != 2 { t.Fatal(\"bad\") } }\n",
		}
		after := map[string]string{
			"a/value.go":      "package a\nfunc New() int { return 1 }\n",
			"b/value.go":      before["b/value.go"],
			"b/value_test.go": strings.Replace(before["b/value_test.go"], "Old()", "New()", 1),
		}
		if rules := integrityRules(t, before, after); !containsRule(rules, "togi/test-behavior") {
			t.Fatalf("rules = %v, want scoped behavior finding", rules)
		}
	})
	blocked := []struct{ name, production, tests string }{
		{"test-only identifier rename", integrityProduction, renamedTestReferences(integrityTests)},
		{"ambiguous production witness", "package sample\nfunc totalFor(value int) int { return value + 1 }\nfunc sumFor(value int) int { return value + 1 }\n", renamedTestReferences(integrityTests)},
		{"rename plus expected literal", "package sample\nfunc totalFor(value int) int { return value + 1 }\n", strings.ReplaceAll(renamedTestReferences(integrityTests), "got != 2", "got != 3")},
		{"test discovery rename", "package sample\nfunc totalFor(value int) int { return value + 1 }\n", strings.ReplaceAll(strings.ReplaceAll(integrityTests, "calculateTotal", "totalFor"), "TestTotal", "TestSum")},
	}
	for _, test := range blocked {
		t.Run(test.name, func(t *testing.T) {
			rules := integrityRules(t, map[string]string{"sample.go": integrityProduction, "sample_test.go": integrityTests}, map[string]string{"sample.go": test.production, "sample_test.go": test.tests})
			if !containsRule(rules, "togi/test-behavior") && !containsRule(rules, "togi/test-discovery") {
				t.Fatalf("rules = %v, want behavior or discovery finding", rules)
			}
		})
	}
}

func TestNewTestsMustCompileAndBeDiscoverable(t *testing.T) {
	original := map[string]string{"sample.go": "package sample\nfunc helper() int { return 1 }\n"}
	for name, source := range map[string]string{
		"generic":   "package sample\nimport \"testing\"\nfunc TestAdded[T any](t *testing.T) {}\n",
		"undefined": "package sample\nimport \"testing\"\nfunc TestAdded(t *testing.T) { missing() }\n",
		"bad call":  "package sample\nimport \"testing\"\nfunc TestAdded(t *testing.T) { helper(1) }\n",
		"duplicate": "package sample\nimport \"testing\"\nfunc TestAdded(t *testing.T) {}\nfunc TestAdded(t *testing.T) {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			attempted := cloneStrings(original)
			attempted["added_test.go"] = source
			result := CheckIntegrity(snapshot(original), snapshot(attempted))
			if result.Err == nil || !strings.Contains(result.Err.Error(), "invalid new test") {
				t.Fatalf("CheckIntegrity error = %v, want sanitized invalid-new-test error", result.Err)
			}
			if strings.Contains(result.Err.Error(), "missing") {
				t.Fatalf("CheckIntegrity error leaked source detail: %v", result.Err)
			}
		})
	}
	for name, constraints := range map[string]string{
		"malformed build constraint": "//go:build (\n\n",
		"multiple build constraints": "//go:build linux\n//go:build amd64\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			attempted := cloneStrings(original)
			attempted["added_test.go"] = constraints + "package sample\nimport \"testing\"\nfunc TestAdded(t *testing.T) {}\n"
			result := CheckIntegrity(snapshot(original), snapshot(attempted))
			if result.Err == nil || !strings.Contains(result.Err.Error(), "invalid new test") {
				t.Fatalf("CheckIntegrity error = %v, want invalid build constraint rejected", result.Err)
			}
		})
	}
	for name, additions := range map[string]map[string]string{
		"testdata fixture": {
			"testdata/case_test.go": "//go:build (\n\npackage fixture\nimport \"testing\"\nfunc TestCase(t *testing.T) {}\n",
		},
		"nested module": {
			"nested/go.mod":       "module example.invalid/nested\n\ngo 1.25\n",
			"nested/case_test.go": "//go:build (\n\npackage nested\nimport \"testing\"\nfunc TestCase(t *testing.T) {}\n",
		},
	} {
		t.Run("constraint preflight excludes "+name, func(t *testing.T) {
			attempted := cloneStrings(original)
			for file, source := range additions {
				attempted[file] = source
			}
			result := CheckIntegrity(snapshot(original), snapshot(attempted))
			if result.Err != nil && strings.Contains(result.Err.Error(), "invalid new test") {
				t.Fatalf("CheckIntegrity error = %v, excluded path entered test preflight", result.Err)
			}
		})
	}
	t.Run("opaque external import", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["added_test.go"] = "package sample\nimport (\"testing\"; \"example.invalid/external\")\nfunc TestAdded(t *testing.T) { external.Run() }\n"
		if result := CheckIntegrity(snapshot(original), snapshot(attempted)); result.Err != nil {
			t.Fatalf("CheckIntegrity error = %v, want opaque selector tolerated", result.Err)
		}
	})
	t.Run("cross-file local helper", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["helper_test.go"] = "package sample\nfunc testHelper() int { return helper() }\n"
		attempted["added_test.go"] = "package sample\nimport \"testing\"\nfunc TestAdded(t *testing.T) { _ = testHelper() }\n"
		if result := CheckIntegrity(snapshot(original), snapshot(attempted)); result.Err != nil {
			t.Fatalf("CheckIntegrity error = %v, want cross-file helper accepted", result.Err)
		}
	})
	for name, call := range map[string]struct {
		call      string
		wantError bool
	}{
		"valid cross-package call":   {"lib.Helper(1)", false},
		"invalid cross-package call": {"lib.Helper()", true},
	} {
		t.Run(name, func(t *testing.T) {
			baseline := map[string]string{
				"go.mod":       "module example.invalid/project\n\ngo 1.25\n",
				"lib/value.go": "package lib\nfunc Helper(value int) int { return value }\n",
			}
			attempted := cloneStrings(baseline)
			attempted["consumer/value_test.go"] = "package consumer\nimport (\"testing\"; \"example.invalid/project/lib\")\nfunc TestAdded(t *testing.T) { _ = " + call.call + " }\n"
			result := CheckIntegrity(snapshot(baseline), snapshot(attempted))
			if call.wantError != (result.Err != nil) {
				t.Fatalf("CheckIntegrity error = %v, wantError = %t", result.Err, call.wantError)
			}
		})
	}
	t.Run("mutually exclusive build variants", func(t *testing.T) {
		baseline := map[string]string{
			"value_linux.go":   "package sample\nfunc platform() int { return 1 }\n",
			"value_windows.go": "package sample\nfunc platform() int { return 2 }\n",
		}
		attempted := cloneStrings(baseline)
		attempted["value_test.go"] = "package sample\nimport \"testing\"\nfunc TestAdded(t *testing.T) { _ = platform() }\n"
		if result := CheckIntegrity(snapshot(baseline), snapshot(attempted)); result.Err != nil {
			t.Fatalf("CheckIntegrity error = %v, want active build package accepted", result.Err)
		}
	})
	t.Run("invalid local imported package", func(t *testing.T) {
		baseline := map[string]string{
			"go.mod":       "module example.invalid/project\n\ngo 1.25\n",
			"lib/value.go": "package lib\nfunc Helper() {}\nvar Broken Missing\n",
		}
		attempted := cloneStrings(baseline)
		attempted["consumer/value_test.go"] = "package consumer\nimport (\"testing\"; \"example.invalid/project/lib\")\nfunc TestAdded(t *testing.T) { lib.Helper() }\n"
		result := CheckIntegrity(snapshot(baseline), snapshot(attempted))
		if result.Err == nil || !strings.Contains(result.Err.Error(), "invalid new test") {
			t.Fatalf("CheckIntegrity error = %v, want invalid dependency rejected", result.Err)
		}
	})
}

func TestWitnessWorkBudget(t *testing.T) {
	source := func(prefix string, count int) string {
		var result strings.Builder
		result.WriteString("package sample\n")
		for index := 0; index < count; index++ {
			fmt.Fprintf(&result, "func %s%04d() {}\n", prefix, index)
		}
		return result.String()
	}
	for _, test := range []struct {
		name      string
		count     int
		wantError bool
	}{
		{"near", (maxIntegrityWitnessWork - 2) / 2, false},
		{"over", (maxIntegrityWitnessWork-2)/2 + 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := CheckIntegrity(
				snapshot(map[string]string{"sample.go": source("Old", test.count)}),
				snapshot(map[string]string{"sample.go": source("New", test.count)}),
			)
			if test.wantError != (result.Err != nil) {
				t.Fatalf("CheckIntegrity error = %v, wantError = %t", result.Err, test.wantError)
			}
			if result.Err != nil && !strings.Contains(result.Err.Error(), "resource limit exceeded") {
				t.Fatalf("CheckIntegrity error = %v, want resource error", result.Err)
			}
		})
	}
}

func TestInactiveVariantDependencyWorkBudget(t *testing.T) {
	files := map[string]string{
		"go.mod":                 "module example.invalid/project\n\ngo 1.25\n",
		"owner/common.go":        "package owner\nfunc Common() {}\n",
		"owner/value_windows.go": "package owner\nimport \"example.invalid/project/dep\"\nfunc Value() { dep.Use() }\n",
	}
	for index := 0; index <= maxIntegrityWitnessWork; index++ {
		files[fmt.Sprintf("dep/value%04d.go", index)] = "package dep\n"
	}
	result := CheckIntegrity(snapshot(files), snapshot(files))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "inspect original test snapshot: resource limit exceeded") {
		t.Fatalf("CheckIntegrity error = %v, want inactive dependency resource error", result.Err)
	}
}

func TestWitnessedRenameCountsDeclarationInstances(t *testing.T) {
	before := map[string]string{
		"old.go":         "package sample\nfunc Old(value int) int { return value + 1 }\n",
		"sample_test.go": "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { New(1) }\n",
	}
	after := map[string]string{
		"new_linux.go":   "//go:build linux\n\npackage sample\nfunc New(value int) int { return value + 1 }\n",
		"new_windows.go": "//go:build windows\n\npackage sample\nfunc New(value int) int { return value + 1 }\n",
		"sample_test.go": before["sample_test.go"],
	}
	originalTests := cloneStrings(before)
	originalTests["sample_test.go"] = strings.Replace(before["sample_test.go"], "New(1)", "Old(1)", 1)
	if rules := integrityRules(t, originalTests, after); !containsRule(rules, "togi/test-behavior") {
		t.Fatalf("rules = %v, want ambiguous declaration-instance finding", rules)
	}
}

func TestIntegritySnapshotOriginalAndAttempt(t *testing.T) {
	repo := t.TempDir()
	gitcmdtest.Git(t, repo, "init", "-q")
	for name, contents := range map[string]string{
		"sample.go":          "package sample\nfunc Value() int { return 1 }\n",
		"sample_test.go":     "package sample\n",
		"testdata/input.txt": "fixture\n",
		"ignored.txt":        "ignore\n",
	} {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitcmdtest.Git(t, repo, "add", ".")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	head := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	original, err := SnapshotOriginal(context.Background(), repo, head)
	if err != nil {
		t.Fatalf("SnapshotOriginal() error = %v", err)
	}
	attempted, err := SnapshotAttempt(repo)
	if err != nil {
		t.Fatalf("SnapshotAttempt() error = %v", err)
	}
	for _, got := range []TreeSnapshot{original, attempted} {
		if len(got.Files) != 3 || string(got.Files["testdata/input.txt"]) != "fixture\n" {
			t.Fatalf("snapshot files = %v, want relevant tracked content", mapKeys(got.Files))
		}
	}
}

func TestIntegritySnapshotAttemptRejectsSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "attempt")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotAttempt(linkRoot); err == nil {
		t.Fatal("SnapshotAttempt accepted a symlink root")
	}
}

func TestIntegritySnapshotAttemptRejectsConcurrentMutation(t *testing.T) {
	for _, test := range []struct {
		name  string
		stage string
		path  string
		run   func(t *testing.T, root string)
	}{
		{"entry add", "after-inventory", "", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "added_test.go"), []byte("package sample\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"entry delete", "before-post-inventory", "", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "sample_test.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"entry rename", "before-post-inventory", "", func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "sample_test.go"), filepath.Join(root, "renamed_test.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"same-size overwrite", "after-first-read", "sample_test.go", func(t *testing.T, root string) {
			file := filepath.Join(root, "sample_test.go")
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for index := range contents {
				if contents[index] == 's' {
					contents[index] = 'x'
					break
				}
			}
			if err := os.WriteFile(file, contents, info.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(integrityProduction), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "sample_test.go"), []byte(integrityTests), 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			snapshotAttemptHook = func(stage, file string) error {
				if !called && stage == test.stage && (test.path == "" || file == test.path) {
					called = true
					test.run(t, root)
				}
				return nil
			}
			t.Cleanup(func() { snapshotAttemptHook = nil })
			if _, err := SnapshotAttempt(root); err == nil {
				t.Fatal("SnapshotAttempt accepted concurrent mutation")
			}
			if !called {
				t.Fatalf("snapshot hook %q was not called", test.stage)
			}
		})
	}
}

func TestFixtureGoSyntaxIsComparedAsBytes(t *testing.T) {
	original := map[string]string{
		"sample.go":                 integrityProduction,
		"sample_test.go":            integrityTests,
		"testdata/invalid/input.go": "this is deliberately not Go",
	}
	if rules := integrityRules(t, original, cloneStrings(original)); len(rules) != 0 {
		t.Fatalf("unchanged malformed Go fixture rules = %v, want none", rules)
	}
	attempted := cloneStrings(original)
	attempted["testdata/invalid/input.go"] = "still deliberately not Go"
	if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/test-fixture") {
		t.Fatalf("changed malformed Go fixture rules = %v, want togi/test-fixture", rules)
	}
}

func TestFixtureRejectsSuppressionInNewContent(t *testing.T) {
	original := map[string]string{"sample.go": integrityProduction, "sample_test.go": integrityTests}
	attempted := cloneStrings(original)
	attempted["testdata/new.go"] = "package fixture //nolint\n"
	if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/new-suppression") {
		t.Fatalf("rules = %v, want togi/new-suppression", rules)
	}
}

func TestFixtureSuppressionUsesStructuralScanner(t *testing.T) {
	original := map[string]string{"sample.go": integrityProduction, "sample_test.go": integrityTests}
	t.Run("safe literal", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["testdata/safe.go"] = "package fixture\nconst text = \"//nolint is example data\"\n"
		if rules := integrityRules(t, original, attempted); containsRule(rules, "togi/new-suppression") {
			t.Fatalf("safe literal rules = %v, want allowed", rules)
		}
	})
	t.Run("testing method expression", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["testdata/skip.go"] = "package fixture\nimport \"testing\"\nfunc hide(t *testing.T) { (*testing.T).Skip(t, \"later\") }\n"
		if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/new-suppression") {
			t.Fatalf("method expression rules = %v, want blocked", rules)
		}
	})
	t.Run("existing fixture type context", func(t *testing.T) {
		baseline := cloneStrings(original)
		baseline["testdata/helper.go"] = "package fixture\nimport \"testing\"\ntype T = testing.T\n"
		attempted := cloneStrings(baseline)
		attempted["testdata/new.go"] = "package fixture\nfunc hide(t *T) { t.Skip(\"later\") }\n"
		if rules := integrityRules(t, baseline, attempted); !containsRule(rules, "togi/new-suppression") {
			t.Fatalf("contextual fixture rules = %v, want blocked", rules)
		}
	})
	for name, source := range map[string]string{
		"plain malformed":           "package fixture\nfunc broken(\n",
		"suppression and malformed": "package fixture\nimport \"testing\"\nfunc hide(t *testing.T) { t.Skip(\"sensitive reason\")\n",
	} {
		t.Run("new Go fixture "+name, func(t *testing.T) {
			attempted := cloneStrings(original)
			attempted["testdata/broken.go"] = source
			result := CheckIntegrity(snapshot(original), snapshot(attempted))
			if result.Err == nil || !strings.Contains(result.Err.Error(), "malformed Go syntax") {
				t.Fatalf("CheckIntegrity error = %v, want deterministic malformed Go error", result.Err)
			}
			if strings.Contains(result.Err.Error(), "sensitive reason") {
				t.Fatalf("CheckIntegrity error leaked source: %v", result.Err)
			}
		})
	}
	t.Run("over-limit existing Go context fails closed", func(t *testing.T) {
		baseline := cloneStrings(original)
		baseline["testdata/helper.go"] = "package fixture\nimport \"testing\"\ntype T = " + strings.Repeat("(", maxSuppressionDepth+1) + "testing.T" + strings.Repeat(")", maxSuppressionDepth+1) + "\n"
		attempted := cloneStrings(baseline)
		attempted["testdata/new.go"] = "package fixture\nfunc hide(t *T) { t.Skip(\"later\") }\n"
		result := CheckIntegrity(snapshot(baseline), snapshot(attempted))
		if result.Err == nil || !strings.Contains(result.Err.Error(), "resource limit exceeded") {
			t.Fatalf("CheckIntegrity error = %v, want context resource failure", result.Err)
		}
	})
	t.Run("near-limit existing Go context retains provenance", func(t *testing.T) {
		depth := maxSuppressionDepth - 16
		baseline := cloneStrings(original)
		baseline["testdata/helper.go"] = "package fixture\nimport \"testing\"\ntype T = " + strings.Repeat("(", depth) + "testing.T" + strings.Repeat(")", depth) + "\n"
		attempted := cloneStrings(baseline)
		attempted["testdata/new.go"] = "package fixture\nfunc hide(t *T) { t.Skip(\"later\") }\n"
		result := CheckIntegrity(snapshot(baseline), snapshot(attempted))
		if result.Err != nil {
			t.Fatalf("CheckIntegrity error = %v, want near-limit context analyzed", result.Err)
		}
		for _, item := range result.Findings {
			if item.RuleID == "togi/new-suppression" {
				return
			}
		}
		t.Fatalf("findings = %#v, want contextual suppression finding", result.Findings)
	})
	for name, source := range map[string]string{
		"skip with whitespace": "package fixture\nimport \"testing\"\nfunc hide(t *testing.T) { t . Skip /* reason */ (\"later\") }\n",
		"directive":            "package fixture\nfunc hide() {} // nolint\n",
		"build constraint":     "//go:build never\n\npackage fixture\n",
	} {
		t.Run("source txt "+name, func(t *testing.T) {
			attempted := cloneStrings(original)
			attempted["testdata/source.txt"] = source
			if rules := integrityRules(t, original, attempted); !containsRule(rules, "togi/new-suppression") {
				t.Fatalf("source-shaped fixture rules = %v, want blocked", rules)
			}
		})
	}
	t.Run("finding uses real fixture path", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["testdata/source.txt"] = "package fixture\nimport \"testing\"\nfunc hide(t *testing.T) { t.Skip(\"later\") }\n"
		result := CheckIntegrity(snapshot(original), snapshot(attempted))
		if result.Err != nil {
			t.Fatalf("CheckIntegrity error: %v", result.Err)
		}
		for _, item := range result.Findings {
			if item.RuleID == "togi/new-suppression" && item.File == "testdata/source.txt" {
				return
			}
		}
		t.Fatalf("findings = %#v, want suppression at real fixture path", result.Findings)
	})
	t.Run("safe golden text", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["testdata/output.golden"] = "expected //nolint text, not Go source\n"
		if rules := integrityRules(t, original, attempted); containsRule(rules, "togi/new-suppression") {
			t.Fatalf("safe golden rules = %v, want allowed", rules)
		}
	})
	t.Run("parseability probe enforces nesting limit", func(t *testing.T) {
		attempted := cloneStrings(original)
		attempted["testdata/deep.txt"] = "package fixture\nvar _ = " + strings.Repeat("(", maxSuppressionDepth+1) + "1\n"
		result := CheckIntegrity(snapshot(original), snapshot(attempted))
		if result.Err == nil {
			t.Fatal("CheckIntegrity error = nil, want bounded fixture probe failure")
		}
	})
	t.Run("non-Go baseline fixtures do not consume Go probe limit", func(t *testing.T) {
		baseline := cloneStrings(original)
		for index := 0; index < maxSuppressionGoFiles+1; index++ {
			baseline[fmt.Sprintf("testdata/output-%04d.txt", index)] = "expected output\n"
		}
		result := CheckIntegrity(snapshot(baseline), snapshot(baseline))
		if result.Err != nil {
			t.Fatalf("CheckIntegrity error = %v, want unchanged non-Go fixtures allowed", result.Err)
		}
	})
}

func containsRule(rules []string, want string) bool {
	for _, rule := range rules {
		if rule == want {
			return true
		}
	}
	return false
}

func renamedTestReferences(source string) string {
	return strings.ReplaceAll(strings.ReplaceAll(source, "calculateTotal", "totalFor"), "Example_totalFor", "Example_calculateTotal")
}

func cloneStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
