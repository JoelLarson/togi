package flywheel

import (
	"errors"
	"fmt"
	"go/build/constraint"
	"strings"
	"testing"
)

func snapshot(contents map[string]string) TreeSnapshot {
	files := make(map[string][]byte, len(contents))
	for file, content := range contents {
		files[file] = []byte(content)
	}
	return TreeSnapshot{Files: files}
}

func suppressionCheck(t *testing.T, original, attempted map[string]string, want int) {
	t.Helper()
	got, err := checkSuppressionIntegrity(snapshot(original), snapshot(attempted))
	if err != nil {
		t.Fatalf("checkSuppressionIntegrity() error = %v", err)
	}
	if len(got) != want {
		t.Fatalf("checkSuppressionIntegrity() = %#v; want %d findings", got, want)
	}
	for _, item := range got {
		if item.Gate != "integrity" || item.RuleID == "" || item.File == "" || item.Line < 1 || len(item.Snippet) > 240 {
			t.Fatalf("invalid bounded finding: %#v", item)
		}
	}
}

func TestSuppressionIntegrityDirectiveMatrix(t *testing.T) {
	stable := []struct {
		name      string
		original  string
		attempted string
	}{
		{
			"unchanged with line shift",
			"package check\nfunc run() {\nwork() //nolint:errcheck\n}\n",
			"package check\n\nfunc run() {\nwork() //nolint:errcheck\n}\n",
		},
		{
			"nolint reason changes",
			"package check\nfunc run() { work() //nolint:errcheck // old reason\n}\n",
			"package check\nfunc run() { work() //nolint:errcheck // new reason\n}\n",
		},
		{
			"lint ignore reason changes",
			"package check\nfunc run() { work() //lint:ignore U1000 old reason\n}\n",
			"package check\nfunc run() { work() //lint:ignore U1000 new reason\n}\n",
		},
		{
			"nosec reason changes",
			"package check\nfunc run() { work() //#nosec G204 -- old reason\n}\n",
			"package check\nfunc run() { work() //#nosec G204 -- new reason\n}\n",
		},
		{
			"nolint narrows",
			"package check\nfunc run() { work() //nolint\n}\n",
			"package check\nfunc run() { work() //nolint:errcheck\n}\n",
		},
		{
			"nosec narrows",
			"package check\nfunc run() { work() //#nosec\n}\n",
			"package check\nfunc run() { work() //#nosec G204\n}\n",
		},
		{
			"removal",
			"package check\nfunc run() { work() //nolint\n}\n",
			"package check\nfunc run() { work()\n}\n",
		},
		{
			"nolint prefix word is ordinary comment",
			"package check\nfunc run() { work() }\n",
			"package check\nfunc run() { work() //nolinter\n}\n",
		},
		{
			"lint ignore prefix word is ordinary comment",
			"package check\nfunc run() { work() }\n",
			"package check\nfunc run() { work() //lint:ignored U1000\n}\n",
		},
		{
			"nosec prefix word is ordinary comment",
			"package check\nfunc run() { work() }\n",
			"package check\nfunc run() { work() //#nosecurity G204\n}\n",
		},
	}
	for _, test := range stable {
		t.Run(test.name, func(t *testing.T) {
			suppressionCheck(t, map[string]string{"check.go": test.original}, map[string]string{"check.go": test.attempted}, 0)
		})
	}

	violations := []struct {
		name      string
		original  string
		attempted string
		want      int
	}{
		{
			"addition",
			"package check\nfunc run() { work() }\n",
			"package check\nfunc run() { work() //nolint:errcheck\n}\n",
			1,
		},
		{
			"broadening",
			"package check\nfunc run() { work() //nolint:errcheck\n}\n",
			"package check\nfunc run() { work() //nolint\n}\n",
			1,
		},
		{
			"scope superset",
			"package check\nfunc run() { work() //nolint:errcheck\n}\n",
			"package check\nfunc run() { work() //nolint:errcheck,govet\n}\n",
			1,
		},
		{
			"movement",
			"package check\nfunc run() { first() //nolint\nsecond()\n}\n",
			"package check\nfunc run() { first()\nsecond() //nolint\n}\n",
			1,
		},
		{
			"duplication",
			"package check\nfunc run() { first() //nolint\nsecond()\n}\n",
			"package check\nfunc run() { first() //nolint\nsecond() //nolint\n}\n",
			1,
		},
		{
			"declaration movement",
			"package check\nfunc left() { work() //nolint\n}\nfunc right() { work() }\n",
			"package check\nfunc left() { work() }\nfunc right() { work() //nolint\n}\n",
			1,
		},
		{
			"duplicate declaration deletion cannot mask movement",
			"package check\nfunc init() { markerA(); work() //nolint\n}\nfunc init() { markerB(); work() }\n",
			"package check\nfunc init() { markerB(); work() //nolint\n}\n",
			1,
		},
	}
	for _, test := range violations {
		t.Run(test.name, func(t *testing.T) {
			suppressionCheck(t, map[string]string{"check.go": test.original}, map[string]string{"check.go": test.attempted}, test.want)
		})
	}

	t.Run("multiline occurrences remain distinct", func(t *testing.T) {
		original := "package check\nfunc run() { work() /*\n * #nosec G204\n */ }\n"
		attempted := "package check\nfunc run() { work() /*\n * #nosec G204\n * #nosec G204\n */ }\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("mixed duplicate scopes match deterministically", func(t *testing.T) {
		original := "package check\nfunc run() { work() /*\n * nolint\n * nolint:errcheck\n */ }\n"
		attempted := "package check\nfunc run() { work() /*\n * nolint:errcheck\n * nolint\n */ }\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
	t.Run("target reorder cannot preserve applicability", func(t *testing.T) {
		original := "package check\nfunc run() { first() //nolint\nsecond()\n}\n"
		attempted := "package check\nfunc run() { second() //nolint\nfirst()\n}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("duplicate target occurrence survives ordinal masking", func(t *testing.T) {
		original := "package check\nfunc run() {\nprefix()\nwork() //nolint\nwork()\n}\n"
		attempted := "package check\nfunc run() {\nwork()\nwork() //nolint\n}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("control header change cannot preserve applicability", func(t *testing.T) {
		original := "package check\nfunc run(left bool) { if left { work() //nolint\n} }\n"
		attempted := "package check\nfunc run(left bool) { if !left { work() //nolint\n} }\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("subset matching uses maximum cardinality", func(t *testing.T) {
		original := []suppressionSite{
			{identity: "same", scope: ruleScope("a,b"), line: 1, snippet: "first"},
			{identity: "same", scope: ruleScope("a,c"), line: 2, snippet: "second"},
		}
		attempted := []suppressionSite{
			{identity: "same", scope: ruleScope("a"), line: 1, snippet: "first"},
			{identity: "same", scope: ruleScope("b"), line: 2, snippet: "second"},
		}
		got, err := compareSuppressionSites("check.go", original, attempted)
		if err != nil || len(got) != 0 {
			t.Fatalf("compareSuppressionSites() = %#v, %v; want complete matching", got, err)
		}
	})
}

func TestSuppressionIntegrityTestingProvenance(t *testing.T) {
	positives := map[string]map[string]string{
		"direct method": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run(t *testing.T) { t.Skip(\"later\") }\n",
		},
		"skip now": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run(t *testing.T) { t.SkipNow() }\n",
		},
		"promoted testing method": {
			"check_test.go": "package check\nimport \"testing\"\ntype suite struct { *testing.T }\nfunc run(s suite) { s.Skip(\"later\") }\n",
		},
		"explicit embedded selector": {
			"check_test.go": "package check\nimport \"testing\"\ntype suite struct { *testing.T }\nfunc run(s suite) { s.T.Skip(\"later\") }\n",
		},
		"method expression": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run(t *testing.T) { (*testing.T).Skip(t, \"later\") }\n",
		},
		"testing interface expression": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run(t testing.TB) { testing.TB.Skip(t, \"later\") }\n",
		},
		"local interface redeclaration": {
			"check_test.go": "package check\nimport \"testing\"\ntype TB interface { testing.TB; Skip(...any); SkipNow() }\nfunc run(t TB) { t.Skip(\"later\") }\n",
		},
		"function local alias": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run(t *testing.T) { type TestT = testing.T; var alias *TestT = t; alias.Skip(\"later\") }\n",
		},
		"generic type parameter": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run[T interface { testing.TB }](t T) { t.Skip(\"later\") }\n",
		},
		"nested callback": {
			"check_test.go": "package check\nimport \"testing\"\nfunc run(t *testing.T) { t.Run(\"case\", func(t *testing.T) { t.Skip(\"later\") }) }\n",
		},
		"cross file alias": {
			"types.go":      "package check\nimport \"testing\"\ntype TestT = testing.T\n",
			"check_test.go": "package check\nfunc run(t *TestT) { (*TestT).Skip(t, \"later\") }\n",
		},
		"inactive platform declaration cannot mask testing alias": {
			"a_windows.go":  "//go:build windows\n\npackage check\ntype TestT struct{}\nfunc (*TestT) Skip(...any) {}\n",
			"z_linux.go":    "//go:build linux\n\npackage check\nimport \"testing\"\ntype TestT = testing.T\n",
			"check_test.go": "package check\nfunc run(t *TestT) { t.Skip(\"later\") }\n",
		},
		"inactive platform file is still protected": {
			"check_test.go": "//go:build windows\n\npackage check\nimport \"testing\"\nfunc run(t *testing.T) { t.Skip(\"later\") }\n",
		},
		"inactive platform package alias is protected": {
			"types_windows.go": "//go:build windows\n\npackage check\nimport \"testing\"\ntype TestT = testing.T\n",
			"check_test.go":    "//go:build windows\n\npackage check\nfunc run(t *TestT) { t.Skip(\"later\") }\n",
		},
		"compatible architecture suffixes share provenance": {
			"types_arm64.go":           "package check\nimport \"testing\"\ntype TestT = testing.T\n",
			"feature_special_arm64.go": "package check\nfunc run(t *TestT) { t.Skip(\"later\") }\n",
		},
		"compatible build expressions share provenance": {
			"types_windows_amd64.go": "//go:build windows && amd64\n\npackage check\nimport \"testing\"\ntype TestT = testing.T\n",
			"check_test.go":          "//go:build windows\n\npackage check\nfunc run(t *TestT) { t.Skip(\"later\") }\n",
		},
		"testing provenance is unioned across platforms": {
			"types_linux.go":   "//go:build linux\n\npackage check\ntype TestT struct{}\nfunc (*TestT) Skip(...any) {}\n",
			"types_windows.go": "//go:build windows\n\npackage check\nimport \"testing\"\ntype TestT = testing.T\n",
			"check_test.go":    "package check\nfunc run(t *TestT) { t.Skip(\"later\") }\n",
		},
		"future release file is forcibly checked": {
			"check_test.go": "//go:build go1.26\n\npackage check\nimport \"testing\"\nfunc run(t *testing.T) { t.Skip(\"later\") }\n",
		},
		"unsatisfiable custom constraint is forcibly checked": {
			"check_test.go": "//go:build feature && !feature\n\npackage check\nimport \"testing\"\nfunc run(t *testing.T) { t.Skip(\"later\") }\n",
		},
		"forced file wins over active conflicting sibling": {
			"types_linux.go": "//go:build linux\n\npackage check\ntype TestT struct{}\nfunc (*TestT) Skip(...any) {}\n",
			"check_test.go":  "//go:build go1.26\n\npackage check\nimport \"testing\"\ntype TestT = testing.T\nfunc run(t *TestT) { t.Skip(\"later\") }\n",
		},
	}
	for name, attempted := range positives {
		t.Run(name, func(t *testing.T) {
			original := make(map[string]string, len(attempted))
			for file, source := range attempted {
				if statement := skipStatement(source); statement != "" {
					original[file] = strings.Replace(source, statement, "work()", 1)
				} else {
					original[file] = source
				}
			}
			suppressionCheck(t, original, attempted, 1)
		})
	}

	t.Run("unchanged call tolerates line shift", func(t *testing.T) {
		original := "package check\nimport \"testing\"\nfunc run(t *testing.T) { t.Skip(\"later\") }\n"
		attempted := "package check\n\nimport \"testing\"\n\nfunc run(t *testing.T) {\n t.Skip(\"later\")\n}\n"
		suppressionCheck(t, map[string]string{"check_test.go": original}, map[string]string{"check_test.go": attempted}, 0)
	})

	t.Run("call movement across branches", func(t *testing.T) {
		original := "package check\nimport \"testing\"\nfunc run(t *testing.T, left bool) { if left { t.Skip(\"later\") } else { work() } }\n"
		attempted := "package check\nimport \"testing\"\nfunc run(t *testing.T, left bool) { if left { work() } else { t.Skip(\"later\") } }\n"
		suppressionCheck(t, map[string]string{"check_test.go": original}, map[string]string{"check_test.go": attempted}, 1)
	})

	owners := []struct {
		name      string
		original  string
		attempted string
	}{
		{
			"callback callee",
			"func run(t *testing.T) { left(func() { t.Skip(\"later\") }) }",
			"func run(t *testing.T) { right(func() { t.Skip(\"later\") }) }",
		},
		{
			"callback non literal argument",
			"func run(t *testing.T) { t.Run(\"left\", func(t *testing.T) { t.Skip(\"later\") }) }",
			"func run(t *testing.T) { t.Run(\"right\", func(t *testing.T) { t.Skip(\"later\") }) }",
		},
		{
			"assignment owner",
			"func run(t *testing.T) { left := func() { t.Skip(\"later\") }; _ = left }",
			"func run(t *testing.T) { right := func() { t.Skip(\"later\") }; _ = right }",
		},
		{
			"keyed composite owner",
			"func run(t *testing.T) { _ = map[string]func(){\"left\": func() { t.Skip(\"later\") }} }",
			"func run(t *testing.T) { _ = map[string]func(){\"right\": func() { t.Skip(\"later\") }} }",
		},
	}
	for _, test := range owners {
		t.Run(test.name, func(t *testing.T) {
			original := "package check\nimport \"testing\"\n" + test.original + "\n"
			attempted := "package check\nimport \"testing\"\n" + test.attempted + "\n"
			suppressionCheck(t, map[string]string{"check_test.go": original}, map[string]string{"check_test.go": attempted}, 1)
		})
	}

	t.Run("duplicate callback occurrence survives ordinal masking", func(t *testing.T) {
		original := `package check
import "testing"
func run(t *testing.T) {
	prefix()
	t.Run("same", func(t *testing.T) { t.Skip("later") })
	t.Run("same", func(t *testing.T) { work() })
}
`
		attempted := `package check
import "testing"
func run(t *testing.T) {
	t.Run("same", func(t *testing.T) { work() })
	t.Run("same", func(t *testing.T) { t.Skip("later") })
}
`
		suppressionCheck(t, map[string]string{"check_test.go": original}, map[string]string{"check_test.go": attempted}, 1)
	})
}

func skipStatement(source string) string {
	for _, statement := range []string{
		"t.Skip(\"later\")", "t.SkipNow()", "s.Skip(\"later\")", "s.T.Skip(\"later\")",
		"(*testing.T).Skip(t, \"later\")", "testing.TB.Skip(t, \"later\")", "alias.Skip(\"later\")",
		"(*TestT).Skip(t, \"later\")", "w.Skip(\"not testing\")", "s.Skip(\"not testing\")",
		"s.Skip(\"field\")", "s.Skip(\"custom\")", "t.Skip(\"unknown\")", "testing.Skip(\"local\")",
		"t.Skip(\"local\")",
	} {
		if strings.Contains(source, statement) {
			return statement
		}
	}
	if strings.Contains(source, "t.Run") {
		return "t.Skip(\"later\")"
	}
	return ""
}

func TestSuppressionIntegrityRejectsUnprovenTestingNames(t *testing.T) {
	negatives := map[string]string{
		"custom method": `package check
type worker struct{}
func (worker) Skip(...any) {}
func run(w worker) { w.Skip("not testing") }
`,
		"function field": `package check
type suite struct { Skip func(...any) }
func run(s suite) { s.Skip("not testing") }
`,
		"field shadows promoted method": `package check
import "testing"
type suite struct { *testing.T; Skip func(...any) }
func run(s suite) { s.Skip("field") }
`,
		"custom method shadows promoted method": `package check
import "testing"
type suite struct { *testing.T }
func (suite) Skip(...any) {}
func run(s suite) { s.Skip("custom") }
`,
		"opaque external type": `package check
import fake "example.com/testing"
func run(t *fake.T) { t.Skip("unknown") }
`,
		"testing import shadowed": `package check
import "testing"
type local struct{}
func (local) Skip(...any) {}
func run(t *testing.T) { testing := local{}; testing.Skip("local") }
`,
		"different local interface signature": `package check
import "testing"
type TB interface { testing.TB; Skip(string) }
func run(t TB) { t.Skip("local") }
`,
		"unrelated structural interface": `package check
type TB interface { Skip(...any); SkipNow() }
func run(t TB) { t.Skip("local") }
`,
	}
	for name, attempted := range negatives {
		t.Run(name, func(t *testing.T) {
			original := strings.Replace(attempted, skipStatement(attempted), "work()", 1)
			suppressionCheck(t, map[string]string{"check_test.go": original}, map[string]string{"check_test.go": attempted}, 0)
		})
	}

	t.Run("snapshot packages do not resolve cross directory imports", func(t *testing.T) {
		original := map[string]string{
			"other/types.go": "package other\nimport \"testing\"\ntype T = testing.T\n",
			"check_test.go":  "package check\nimport other \"repo/other\"\nfunc run(t *other.T) { work() }\n",
		}
		attempted := map[string]string{
			"other/types.go": "package other\nimport \"testing\"\ntype T = testing.T\n",
			"check_test.go":  "package check\nimport other \"repo/other\"\nfunc run(t *other.T) { t.Skip(\"unknown\") }\n",
		}
		suppressionCheck(t, original, attempted, 0)
	})
}

func TestSuppressionIntegrityBuildConstraints(t *testing.T) {
	t.Run("build prefix words are ordinary comments", func(t *testing.T) {
		original := "package check\nfunc run() {}\n"
		for name, header := range map[string]string{
			"legacy":         "// +builder linux\n\n",
			"compact legacy": "//+builder linux\n\n",
			"modern":         "//go:builder linux\n\n",
		} {
			t.Run(name, func(t *testing.T) {
				suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": header + original}, 0)
			})
		}
	})
	t.Run("compact legacy addition", func(t *testing.T) {
		original := "package check\nfunc run() {}\n"
		attempted := "//+build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("compact legacy movement to effective header", func(t *testing.T) {
		original := "package check\n//+build linux\nfunc run() {}\n"
		attempted := "//+build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("compact legacy broadening", func(t *testing.T) {
		original := "//+build linux\n\npackage check\nfunc run() {}\n"
		attempted := "//+build linux darwin\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("removing one legacy line broadens aggregate", func(t *testing.T) {
		original := "// +build linux\n//+build amd64\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("adding one legacy line narrows aggregate", func(t *testing.T) {
		original := "// +build linux\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux\n//+build amd64\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
	t.Run("removing redundant legacy line is equivalent", func(t *testing.T) {
		original := "// +build linux\n//+build linux\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
	t.Run("adding duplicate legacy line fails", func(t *testing.T) {
		original := "// +build linux\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux\n//+build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("idempotent legacy expression is duplicate", func(t *testing.T) {
		original := "// +build linux\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux\n// +build linux,linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("reordered legacy expression is duplicate", func(t *testing.T) {
		original := "// +build linux,amd64\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux,amd64\n//+build amd64,linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("equivalent baseline legacy duplicate still matches", func(t *testing.T) {
		original := "// +build linux\n// +build linux,linux\n\npackage check\nfunc run() {}\n"
		attempted := "// +build linux\n//+build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
	t.Run("adding duplicate modern line fails once", func(t *testing.T) {
		original := "//go:build linux\n\npackage check\nfunc run() {}\n"
		attempted := "//go:build linux\n//go:build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("parenthesized modern expression is duplicate", func(t *testing.T) {
		original := "//go:build linux\n\npackage check\nfunc run() {}\n"
		attempted := "//go:build linux\n//go:build (linux)\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("removing duplicate modern line is allowed", func(t *testing.T) {
		original := "//go:build linux\n//go:build linux\n\npackage check\nfunc run() {}\n"
		attempted := "//go:build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
	t.Run("unchanged compact paired header", func(t *testing.T) {
		source := "//go:build linux\n//+build linux\n\npackage check\nfunc run() {}\n"
		shifted := "//go:build linux\n//+build linux\n\npackage check\n\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": source}, map[string]string{"check.go": shifted}, 0)
	})
	t.Run("unchanged paired header", func(t *testing.T) {
		source := "//go:build linux\n// +build linux\n\npackage check\nfunc run() {}\n"
		shifted := "//go:build linux\n// +build linux\n\npackage check\n\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": source}, map[string]string{"check.go": shifted}, 0)
	})
	t.Run("new effective header", func(t *testing.T) {
		original := "package check\nfunc run() {}\n"
		attempted := "//go:build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("inert interior moved to header", func(t *testing.T) {
		original := "package check\n// +build linux\nfunc run() {}\n"
		attempted := "// +build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("header removal is allowed", func(t *testing.T) {
		original := "//go:build linux\n\npackage check\nfunc run() {}\n"
		attempted := "package check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
	t.Run("constraint broadening fails", func(t *testing.T) {
		original := "//go:build linux\n\npackage check\nfunc run() {}\n"
		attempted := "//go:build linux || darwin\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 1)
	})
	t.Run("constraint narrowing passes", func(t *testing.T) {
		original := "//go:build linux || darwin\n\npackage check\nfunc run() {}\n"
		attempted := "//go:build linux\n\npackage check\nfunc run() {}\n"
		suppressionCheck(t, map[string]string{"check.go": original}, map[string]string{"check.go": attempted}, 0)
	})
}

func TestSuppressionIntegrityProtectedConfig(t *testing.T) {
	for _, name := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"} {
		t.Run(name, func(t *testing.T) {
			suppressionCheck(t, map[string]string{name: "old\n"}, map[string]string{name: "new\n"}, 1)
		})
	}
}

func TestSuppressionIntegrityMalformedAndBounded(t *testing.T) {
	t.Run("malformed changed Go is infrastructure error", func(t *testing.T) {
		_, err := checkSuppressionIntegrity(
			snapshot(map[string]string{"check.go": "package check\nfunc run() {}\n"}),
			snapshot(map[string]string{"check.go": "package check\nfunc run( {\n"}),
		)
		if err == nil || !strings.Contains(err.Error(), "malformed Go syntax") {
			t.Fatalf("error = %v; want bounded malformed syntax error", err)
		}
	})

	t.Run("malformed unchanged sibling does not hide changed file", func(t *testing.T) {
		original := map[string]string{
			"broken.go": "package check\nfunc broken( {\n",
			"check.go":  "package check\nfunc run() { work() }\n",
		}
		attempted := map[string]string{
			"broken.go": "package check\nfunc broken( {\n",
			"check.go":  "package check\nfunc run() { work() //nolint\n}\n",
		}
		suppressionCheck(t, original, attempted, 1)
	})

	t.Run("path union checked before allocation", func(t *testing.T) {
		files := make(map[string][]byte, maxSuppressionPaths+1)
		for index := 0; index <= maxSuppressionPaths; index++ {
			files[fmt.Sprintf("file-%05d.txt", index)] = []byte("same")
		}
		_, err := boundedChangedPaths(TreeSnapshot{Files: files}, TreeSnapshot{})
		if !errors.Is(err, errSuppressionResourceLimit) {
			t.Fatalf("error = %v; want resource limit", err)
		}
	})

	t.Run("candidate aggregate is bounded", func(t *testing.T) {
		source := "package check\nfunc run(t T) {\n" + strings.Repeat("t.Skip()\n", maxSuppressionCandidates+1) + "}\n"
		_, err := loadSuppressionSnapshot(snapshot(map[string]string{"check.go": source}), map[string]bool{"check.go": true})
		if !errors.Is(err, errSuppressionResourceLimit) {
			t.Fatalf("error = %v; want resource limit", err)
		}
	})

	t.Run("analysis budget is shared across snapshots", func(t *testing.T) {
		tree := snapshot(map[string]string{"check.go": "package check\nfunc run() { work() //nolint\n}\n"})
		probe := maxSuppressionAggregateWork
		if _, err := loadSuppressionSnapshotBounded(tree, map[string]bool{"check.go": true}, &probe); err != nil {
			t.Fatal(err)
		}
		consumed := maxSuppressionAggregateWork - probe
		budget := consumed + consumed/2
		if _, err := loadSuppressionSnapshotBounded(tree, map[string]bool{"check.go": true}, &budget); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSuppressionSnapshotBounded(tree, map[string]bool{"check.go": true}, &budget); !errors.Is(err, errSuppressionResourceLimit) {
			t.Fatalf("second snapshot error = %v; want shared resource limit", err)
		}
	})

	t.Run("ordinary comments use linear extraction work", func(t *testing.T) {
		source := "package check\nfunc run() {\n" + strings.Repeat("work() // ordinary documentation\n", 500) + "}\n"
		tree, err := loadSuppressionSnapshot(snapshot(map[string]string{"check.go": source}), map[string]bool{"check.go": true})
		if err != nil {
			t.Fatal(err)
		}
		parsed := tree.files["check.go"]
		budget := parsed.nodes*3 + len(parsed.file.Comments)*2
		if _, err := snapshotSitesBounded(tree, "check.go", &budget); err != nil {
			t.Fatalf("snapshotSitesBounded() error = %v; want near-linear ordinary-comment scan", err)
		}
	})

	t.Run("candidate ownership traversal is budgeted", func(t *testing.T) {
		source := "package check\nfunc run() {\n" + strings.Repeat("work() //nolint:errcheck\n", 40) + "}\n"
		tree, err := loadSuppressionSnapshot(snapshot(map[string]string{"check.go": source}), map[string]bool{"check.go": true})
		if err != nil {
			t.Fatal(err)
		}
		parsed := tree.files["check.go"]
		budget := parsed.nodes*3 + len(parsed.file.Comments)*2 + parsed.nodes/2
		if _, err := snapshotSitesBounded(tree, "check.go", &budget); !errors.Is(err, errSuppressionResourceLimit) {
			t.Fatalf("snapshotSitesBounded() error = %v; want resource limit", err)
		}
	})

	t.Run("matching work is bounded", func(t *testing.T) {
		original := make([]suppressionSite, 1_001)
		attempted := make([]suppressionSite, 1_001)
		for index := range original {
			original[index] = suppressionSite{identity: "original" + fmt.Sprint(index), line: 1, snippet: "baseline"}
			attempted[index] = suppressionSite{identity: "attempted" + fmt.Sprint(index), line: 1, snippet: "attempt"}
		}
		_, err := compareSuppressionSites("check.go", original, attempted)
		if !errors.Is(err, errSuppressionResourceLimit) {
			t.Fatalf("error = %v; want resource limit", err)
		}
	})

	t.Run("constraint evaluation nodes are budgeted", func(t *testing.T) {
		terms := make([]string, 12)
		for index := range terms {
			terms[index] = fmt.Sprintf("(tag%d || !tag%d)", index, index)
		}
		expression, err := constraint.Parse("//go:build " + strings.Join(terms, " && "))
		if err != nil {
			t.Fatal(err)
		}
		work, budget := 0, 1<<len(terms)
		if _, err := constraintImplies(expression, expression, &work, &budget); !errors.Is(err, errSuppressionResourceLimit) {
			t.Fatalf("constraintImplies() error = %v; want node-visit resource limit", err)
		}
	})

	t.Run("constraint evaluation accepts exact near limit", func(t *testing.T) {
		expression, err := constraint.Parse("//go:build a && b")
		if err != nil {
			t.Fatal(err)
		}
		work, budget := 0, 24 // Four assignments, two three-node expression walks each.
		implies, err := constraintImplies(expression, expression, &work, &budget)
		if err != nil || !implies {
			t.Fatalf("constraintImplies() = %v, %v; want true at exact budget", implies, err)
		}
	})

	t.Run("diagnostics are deterministic", func(t *testing.T) {
		original := snapshot(map[string]string{"check.go": "package check\nfunc run() { work() }\n"})
		attempted := snapshot(map[string]string{"check.go": "package check\nfunc run() { work() //nolint\n}\n"})
		first, err := checkSuppressionIntegrity(original, attempted)
		if err != nil {
			t.Fatal(err)
		}
		for range 50 {
			next, nextErr := checkSuppressionIntegrity(original, attempted)
			if nextErr != nil || fmt.Sprint(next) != fmt.Sprint(first) {
				t.Fatalf("nondeterministic result: %#v, %v; want %#v", next, nextErr, first)
			}
		}
	})
}
