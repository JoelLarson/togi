package run

import (
	"context"
	"encoding/json"
	"errors"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/flywheel"
	"github.com/joellarson/togi/internal/runner"
)

func TestDiscoverGoPackagesFindsRunnableTargets(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "root_test.go", `package root
import "testing"
func TestRoot(t *testing.T) {}
func BenchmarkRoot(b *testing.B) {}
`)
	writeSuiteFile(t, root, "alpha/alpha_test.go", `package alpha
import check "testing"
func TestAlpha(t *check.T) {}
`)
	writeSuiteFile(t, root, "alpha/second_test.go", `package alpha
func ExampleAlpha() {
	// Output: alpha
}
`)
	writeSuiteFile(t, root, "examples/example_test.go", `package examples
type Thing struct{}
func ExampleThing() {
	// Output: thing
}
`)
	writeSuiteFile(t, root, "fuzz/fuzz_test.go", `package fuzz
import . "testing"
func FuzzRoundTrip(f *F) {}
`)
	writeSuiteFile(t, root, "explicit/explicit_test.go", `package explicit
import "testing"
func TestExplicitResultList(t *testing.T) () {}
`)

	packages, err := discoverGoPackages(t, root,
		testGoListPackage{Dir: root, TestGoFiles: []string{"root_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "alpha"), TestGoFiles: []string{"alpha_test.go", "second_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "examples"), XTestGoFiles: []string{"example_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "explicit"), TestGoFiles: []string{"explicit_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "fuzz"), TestGoFiles: []string{"fuzz_test.go"}},
	)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []string{".", "./alpha", "./examples", "./explicit", "./fuzz"}
	if !slices.Equal(packages, want) {
		t.Fatalf("packages = %v, want %v", packages, want)
	}
}

func TestDiscoverGoPackagesCountsOnlyRunnableExamples(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "no-output/example_test.go", `package nooutput
func ExampleNoOutput() {}
`)
	writeSuiteFile(t, root, "empty-output/example_test.go", `package emptyoutput
func ExampleEmpty() {
	// Output:
}
`)
	writeSuiteFile(t, root, "nonempty-output/example_test.go", `package nonemptyoutput
func ExampleNonempty() {
	// Output: value
}
`)
	writeSuiteFile(t, root, "nil-body/example_test.go", `package nilbody
func ExampleNilBody()
`)

	packages, err := discoverGoPackages(t, root,
		testGoListPackage{Dir: filepath.Join(root, "no-output"), TestGoFiles: []string{"example_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "empty-output"), TestGoFiles: []string{"example_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "nonempty-output"), TestGoFiles: []string{"example_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "nil-body"), TestGoFiles: []string{"example_test.go"}},
	)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []string{"./empty-output", "./nonempty-output"}
	if !slices.Equal(packages, want) {
		t.Fatalf("packages = %v, want %v", packages, want)
	}
}

func TestDiscoverGoPackagesAcceptsCmdGoPointerShapes(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "bare/bare_test.go", `package bare
type T struct{}
func TestBare(t *T) {}
`)
	writeSuiteFile(t, root, "selector/selector_test.go", `package selector
func FuzzAlternate(f *alternate.F) {}
`)

	packages, err := discoverGoPackages(t, root,
		testGoListPackage{Dir: filepath.Join(root, "bare"), TestGoFiles: []string{"bare_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "selector"), TestGoFiles: []string{"selector_test.go"}},
	)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []string{"./bare", "./selector"}
	if !slices.Equal(packages, want) {
		t.Fatalf("packages = %v, want %v", packages, want)
	}
}

func TestDiscoverGoPackagesRejectsNonBehavioralTargets(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "bench/bench_test.go", `package bench
import "testing"
func BenchmarkOnly(b *testing.B) {}
`)
	writeSuiteFile(t, root, "malformed/malformed_test.go", `package malformed
import "testing"
func TestNoArgument() {}
func TestValueArgument(t testing.T) {}
func TestWrongArgument(f *testing.F) {}
func TestResult(t *testing.T) int { return 0 }
func TestGeneric[T any](t *testing.T) {}
func Testlower(t *testing.T) {}
func FuzzNoArgument() {}
func FuzzWrongArgument(t *testing.T) {}
func FuzzResult(f *testing.F) int { return 0 }
func FuzzGeneric[T any](f *testing.F) {}
func Fuzzlower(f *testing.F) {}
func ExampleArgument(value int) {}
func ExampleResult() int { return 0 }
func ExampleGeneric[T any]() {
	// Output: generic
}
`)
	writeSuiteFile(t, root, "empty/empty_test.go", "package empty\n")
	writeSuiteFile(t, root, "vendor/hidden/hidden_test.go", `package hidden
import "testing"
func TestHidden(t *testing.T) {}
`)
	writeSuiteFile(t, root, "testdata/hidden_test.go", `package testdata
import "testing"
func TestHidden(t *testing.T) {}
`)
	writeSuiteFile(t, root, "nested/testdata/hidden_test.go", `package testdata
func ExampleHidden() {}
`)

	packages, err := discoverGoPackages(t, root,
		testGoListPackage{Dir: filepath.Join(root, "bench"), TestGoFiles: []string{"bench_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "malformed"), TestGoFiles: []string{"malformed_test.go"}},
		testGoListPackage{Dir: filepath.Join(root, "empty"), TestGoFiles: []string{"empty_test.go"}},
	)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(packages) != 0 {
		t.Fatalf("packages = %v, want none", packages)
	}
}

func TestDiscoverGoPackagesReportsMalformedGo(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "broken_test.go", "package broken\nfunc TestBroken(\n")

	_, err := discoverGoPackages(t, root, testGoListPackage{Dir: root, TestGoFiles: []string{"broken_test.go"}})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("Discover() error = %v, want parse error", err)
	}
}

func TestGoSuiteFullRunDiscoversTargetsAndPasses(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "behavior_test.go", `package behavior
import "testing"
func TestBehavior(t *testing.T) {}
`)
	suite := fakeGoSuiteWithPackages(t, "pass", testGoListPackage{Dir: root, TestGoFiles: []string{"behavior_test.go"}})

	result, err := suite.Run(context.Background(), root, []string{"./ignored"}, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuitePassed || !slices.Equal(result.Command, []string{suite.executable, "test", "./..."}) {
		t.Fatalf("result = %#v", result)
	}
	if result.Packages != nil {
		t.Fatalf("full-run packages = %v, want nil", result.Packages)
	}
}

func TestGoSuiteFullRunUsesBoundedSelectedListFieldsThenTests(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "behavior_test.go", `package behavior
import "testing"
func TestBehavior(t *testing.T) {}
`)
	listed, err := json.Marshal(testGoListPackage{Dir: root, TestGoFiles: []string{"behavior_test.go"}})
	if err != nil {
		t.Fatalf("encode listed package: %v", err)
	}
	wantList := []string{"fake-go", "list", "-e", "-json=Dir,TestGoFiles,XTestGoFiles", "./..."}
	broadList := []string{"fake-go", "list", "-json", "./..."}
	wantTest := []string{"fake-go", "test", "./..."}
	var commands [][]string
	suite := NewGoSuite("fake-go")
	suite.runCommand = func(_ context.Context, _ string, command, _ []string) runner.Result {
		commands = append(commands, slices.Clone(command))
		if len(commands) == 1 && slices.Equal(command, broadList) {
			return suiteRunnerResult(strings.Repeat("x", suiteListOutputLimit+1), "", nil)
		}
		if len(commands) == 1 {
			if !slices.Equal(command, wantList) {
				t.Fatalf("list command = %v, want %v", command, wantList)
			}
			return suiteRunnerResult(string(listed)+"\n", "", nil)
		}
		return suiteRunnerResult("", "", nil)
	}

	result, err := suite.Run(context.Background(), root, nil, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuitePassed || len(commands) != 2 || !slices.Equal(commands[0], wantList) || !slices.Equal(commands[1], wantTest) {
		t.Fatalf("result/commands = %#v/%v", result, commands)
	}
}

func TestGoSuitePinsIdenticalBuildEnvironmentForListAndTest(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "behavior_test.go", "package behavior\nimport \"testing\"\nfunc TestBehavior(t *testing.T) {}\n")
	listed, err := json.Marshal(testGoListPackage{Dir: root, TestGoFiles: []string{"behavior_test.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFLAGS", "-tags=custom")
	t.Setenv("goflags", "-tags=hostile")
	t.Setenv("GoOs", "plan9")
	t.Setenv("GOWORK", filepath.Join(root, "hostile.work"))
	t.Setenv("GOAMD64", "v3")
	t.Setenv("goarm", "5")
	var environments [][]string
	suite := NewGoSuite("fake-go")
	suite.runCommand = func(_ context.Context, _ string, command, environment []string) runner.Result {
		environments = append(environments, slices.Clone(environment))
		if len(command) > 1 && command[1] == "list" {
			return suiteRunnerResult(string(listed)+"\n", "", nil)
		}
		return suiteRunnerResult("", "", nil)
	}

	result, err := suite.Run(context.Background(), root, nil, true)
	if err != nil || result.Status != SuitePassed || len(environments) != 2 || !slices.Equal(environments[0], environments[1]) {
		t.Fatalf("Run() = (%#v, %v), environments = %v", result, err, environments)
	}
	assertSuiteBuildEnvironment(t, environments[0], map[string]string{
		"GOOS": build.Default.GOOS, "GOARCH": build.Default.GOARCH,
		"CGO_ENABLED": map[bool]string{false: "0", true: "1"}[build.Default.CgoEnabled],
		"GOFLAGS":     "", "GOENV": "off", "GOWORK": "off", "GOEXPERIMENT": "none", "GOTOOLCHAIN": "local", "GO111MODULE": "on",
	})
	if !environmentContainsExact(environments[0], "PATH="+os.Getenv("PATH")) {
		t.Fatal("suite environment dropped PATH")
	}
	assertSuiteArchitectureEnvironment(t, environments[0])
}

func TestGoSuiteBuildEnvironmentStripsDuplicateMixedCaseOwnedKeys(t *testing.T) {
	inherited := []string{
		"PATH=/tools", "HOME=/home/test", "GOCACHE=/cache", "TMPDIR=/tmp/owned",
		"GOFLAGS=-tags=custom", "goflags=-tags=other", "GoOs=plan9", "GOARCH=386",
		"cgo_enabled=1", "GOENV=hostile", "goenv=other", "GOWORK=/tmp/go.work", "goexperiment=hostile", "gotoolchain=auto", "go111module=off", "GO111MODULE=auto",
		"GOAMD64=v3", "goamd64=v4", "GOARM=5", "goarm=6", "GO386=softfloat",
	}
	environment := goSuiteBuildEnvironment(inherited)
	assertSuiteBuildEnvironment(t, environment, map[string]string{
		"GOOS": build.Default.GOOS, "GOARCH": build.Default.GOARCH,
		"CGO_ENABLED": map[bool]string{false: "0", true: "1"}[build.Default.CgoEnabled],
		"GOFLAGS":     "", "GOENV": "off", "GOWORK": "off", "GOEXPERIMENT": "none", "GOTOOLCHAIN": "local", "GO111MODULE": "on",
	})
	for _, preserved := range []string{"PATH=/tools", "HOME=/home/test", "GOCACHE=/cache", "TMPDIR=/tmp/owned"} {
		if !environmentContainsExact(environment, preserved) {
			t.Fatalf("suite environment dropped %q: %v", preserved, environment)
		}
	}
	assertSuiteArchitectureEnvironment(t, environment)
}

func TestGoSuiteRejectsModuleReplacementOutsideValidationRootBeforeCommands(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\nreplace example.test/dep => ../outside\n")
	commands := 0
	suite := NewGoSuite("fake-go")
	suite.runCommand = func(context.Context, string, []string, []string) runner.Result {
		commands++
		return suiteRunnerResult("", "", nil)
	}
	result, err := suite.Run(context.Background(), root, []string{"."}, false)
	if err != nil || result.Status != SuiteErrored || commands != 0 || strings.Contains(result.Diagnostic, root) {
		t.Fatalf("Run() = (%#v, %v), commands = %d", result, err, commands)
	}
}

func TestGoSuiteAndChangedPackagesIgnoreInheritedCustomBuildTags(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeSuiteFile(t, root, "custom/value.go", "//go:build custom\n\npackage custom\n")
	t.Setenv("GOFLAGS", "-tags=custom")

	packages, full, err := flywheel.ChangedGoPackages(root, []string{"custom/value.go"})
	if err != nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want inherited custom tag excluded", packages, full, err)
	}
	buildContext := flywheel.ValidationGoBuildContext()
	assertSuiteBuildEnvironment(t, goSuiteBuildEnvironment(os.Environ()), map[string]string{
		"GOOS": buildContext.GOOS, "GOARCH": buildContext.GOARCH,
		"CGO_ENABLED": map[bool]string{false: "0", true: "1"}[buildContext.CgoEnabled],
		"GOFLAGS":     "", "GOENV": "off", "GOWORK": "off", "GOEXPERIMENT": "none", "GOTOOLCHAIN": "local", "GO111MODULE": "on",
	})
}

func TestGoSuiteAndChangedPackagesPinArchitectureFeatureTags(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeSuiteFile(t, root, "feature/value.go", "//go:build amd64.v3\n\npackage feature\n")
	t.Setenv("GOAMD64", "v3")
	t.Setenv("GOARM", "5")

	packages, full, err := flywheel.ChangedGoPackages(root, []string{"feature/value.go"})
	if err != nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want pinned architecture tags", packages, full, err)
	}
	buildContext := flywheel.ValidationGoBuildContext()
	if buildContext.GOARCH == "amd64" && (!slices.Contains(buildContext.ToolTags, "amd64.v1") || slices.Contains(buildContext.ToolTags, "amd64.v3")) {
		t.Fatalf("ToolTags = %v, want pinned cumulative amd64.v1", buildContext.ToolTags)
	}
	assertSuiteArchitectureEnvironment(t, goSuiteBuildEnvironment(os.Environ()))
}

func TestGoSuiteAndChangedPackagesPinExperimentTagsAtProcessStart(t *testing.T) {
	const helper = "TOGI_EXPERIMENT_CONTEXT_HELPER"
	if os.Getenv(helper) == "1" {
		if slices.Contains(flywheel.ValidationGoBuildContext().ToolTags, "goexperiment.arenas") {
			t.Fatal("ValidationGoBuildContext retained process-start experiment tag")
		}
		assertSuiteBuildEnvironment(t, goSuiteBuildEnvironment(os.Environ()), map[string]string{"GOEXPERIMENT": "none"})
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestGoSuiteAndChangedPackagesPinExperimentTagsAtProcessStart$")
	command.Env = make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, "GOEXPERIMENT") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, helper+"=1", "GOEXPERIMENT=arenas")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("experiment helper failed: %v\n%s", err, output)
	}
}

func TestGoSuiteFullRunClassifiesSemanticTestFailure(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "conflict_test.go", `package conflict_test
import "testing"
func TestConflict(t *testing.T) {}
`)
	suite := fakeGoSuiteWithPackages(t, "fail", testGoListPackage{Dir: root, TestGoFiles: []string{"conflict_test.go"}})

	result, err := suite.Run(context.Background(), root, nil, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteFailed || result.Diagnostic != "suite stdout\nsuite stderr" {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteFullRunIsMissingWithoutTargets(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "only_benchmarks_test.go", `package behavior
import "testing"
func BenchmarkOnly(b *testing.B) {}
`)
	suite := fakeGoSuiteWithPackages(t, "must-not-run", testGoListPackage{Dir: root, TestGoFiles: []string{"only_benchmarks_test.go"}})

	result, err := suite.Run(context.Background(), root, nil, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteMissing || result.Command != nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteFullRunIgnoresFilesOutsideGoListUniverse(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "opposite GOOS and GOARCH suffix",
			path: "apparent_" + oppositeGOOS() + "_" + oppositeGOARCH() + "_test.go",
			body: "package apparent\nimport \"testing\"\nfunc TestApparent(t *testing.T) {}\n",
		},
		{
			name: "false build constraint",
			path: "apparent_test.go",
			body: "//go:build togi_never_enabled\n\npackage apparent\nimport \"testing\"\nfunc TestApparent(t *testing.T) {}\n",
		},
		{name: "ignored malformed test", path: "broken_windows_test.go", body: "package broken\nfunc TestBroken(\n"},
		{
			name: "hidden directory", path: ".hidden/apparent_test.go",
			body: "package apparent\nimport \"testing\"\nfunc TestApparent(t *testing.T) {}\n",
		},
		{
			name: "underscore directory", path: "_hidden/apparent_test.go",
			body: "package apparent\nimport \"testing\"\nfunc TestApparent(t *testing.T) {}\n",
		},
		{
			name: "nested module", path: "nested/apparent_test.go",
			body: "package apparent\nimport \"testing\"\nfunc TestApparent(t *testing.T) {}\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeSuiteFile(t, root, test.path, test.body)
			if test.name == "nested module" {
				writeSuiteFile(t, root, "nested/go.mod", "module example.test/nested\n\ngo 1.25\n")
			}
			suite := fakeGoSuiteWithPackages(t, "must-not-run", testGoListPackage{Dir: root})
			result, err := suite.Run(context.Background(), root, nil, true)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Status != SuiteMissing || result.Command != nil {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestGoSuiteFullRunClassifiesGoListFailures(t *testing.T) {
	for _, mode := range []string{"list-malformed", "list-truncated", "list-fail"} {
		t.Run(mode, func(t *testing.T) {
			result, err := fakeGoSuite(t, mode).Run(context.Background(), t.TempDir(), nil, true)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Status != SuiteErrored || result.Diagnostic == "" || result.Command != nil {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestGoSuiteGoListDiagnosticsDoNotExposeRepositoryRoots(t *testing.T) {
	resolvedRoot := t.TempDir()
	lexicalRoot := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(resolvedRoot, lexicalRoot); err != nil {
		t.Fatalf("create lexical root symlink: %v", err)
	}
	stdout := `{"Dir":` + strconv.Quote(lexicalRoot) + `,"TestGoFiles":["secret_test.go"]}`
	stderr := "list failed under " + resolvedRoot + " via " + lexicalRoot
	suite := fakeGoSuiteWithPrivateListFailure(t, stdout, stderr)

	_, discoverErr := suite.Discover(context.Background(), lexicalRoot)
	if discoverErr == nil {
		t.Fatal("Discover() error = nil")
	}
	result, runErr := suite.Run(context.Background(), lexicalRoot, nil, true)
	if runErr != nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if result.Status != SuiteErrored {
		t.Fatalf("result = %#v", result)
	}
	for _, secret := range []string{lexicalRoot, resolvedRoot} {
		if strings.Contains(discoverErr.Error(), secret) || strings.Contains(result.Diagnostic, secret) {
			t.Fatalf("list diagnostics exposed root %q: error=%q diagnostic=%q", secret, discoverErr, result.Diagnostic)
		}
	}
}

func TestGoSuiteListedNonregularTestPathDoesNotExposeRepositoryRoots(t *testing.T) {
	resolvedRoot := t.TempDir()
	lexicalRoot := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(resolvedRoot, lexicalRoot); err != nil {
		t.Fatalf("create lexical root symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(resolvedRoot, "leak_test.go"), 0o700); err != nil {
		t.Fatalf("create nonregular test path: %v", err)
	}
	suite := fakeGoSuiteWithPackages(t, "must-not-run", testGoListPackage{
		Dir: resolvedRoot, TestGoFiles: []string{"leak_test.go"},
	})

	_, discoverErr := suite.Discover(context.Background(), lexicalRoot)
	if discoverErr == nil {
		t.Fatal("Discover() error = nil")
	}
	result, runErr := suite.Run(context.Background(), lexicalRoot, nil, true)
	if runErr != nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if result.Status != SuiteErrored {
		t.Fatalf("result = %#v", result)
	}
	for _, secret := range []string{lexicalRoot, resolvedRoot} {
		if strings.Contains(discoverErr.Error(), secret) || strings.Contains(result.Diagnostic, secret) {
			t.Fatalf("nonregular path exposed root %q: error=%q diagnostic=%q", secret, discoverErr, result.Diagnostic)
		}
	}
}

func TestGoSuiteFullRunClassifiesGoListStartAndCleanupFailures(t *testing.T) {
	root := t.TempDir()
	t.Run("start", func(t *testing.T) {
		result, err := NewGoSuite(filepath.Join(t.TempDir(), "missing-go")).Run(context.Background(), root, nil, true)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if result.Status != SuiteErrored || !strings.Contains(result.Diagnostic, "start Go package discovery") {
			t.Fatalf("result = %#v", result)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		suite := NewGoSuite("fake-go")
		suite.runCommand = func(context.Context, string, []string, []string) runner.Result {
			return runner.Result{
				Stdout:     runner.NewBuffer(suiteListOutputLimit, suiteTruncationMarker),
				Stderr:     runner.NewBuffer(suiteDiagnosticLimit, suiteTruncationMarker),
				CleanupErr: errors.New("injected list cleanup failure"),
			}
		}
		result, err := suite.Run(context.Background(), root, nil, true)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if result.Status != SuiteErrored || !strings.Contains(result.Diagnostic, "clean up Go package discovery") {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestGoSuiteFullRunRejectsEscapingGoListPackageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeSuiteFile(t, outside, "outside_test.go", `package outside
import "testing"
func TestOutside(t *testing.T) {}
`)
	suite := fakeGoSuiteWithPackages(t, "must-not-run", testGoListPackage{
		Dir: outside, TestGoFiles: []string{"outside_test.go"},
	})

	result, err := suite.Run(context.Background(), root, nil, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteErrored || result.Command != nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteFullRunValidatesEveryListedTestFile(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "valid_test.go", `package valid
import "testing"
func TestValid(t *testing.T) {}
`)
	suite := fakeGoSuiteWithPackages(t, "must-not-run", testGoListPackage{
		Dir: root, TestGoFiles: []string{"valid_test.go", "../escaping_test.go"},
	})

	result, err := suite.Run(context.Background(), root, nil, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteErrored || result.Command != nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteFullRunPreservesGoListCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := fakeGoSuite(t, "list-sleep").Run(ctx, t.TempDir(), nil, true)
	if result.Status != SuiteErrored || !errors.Is(err, ErrSuiteCanceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() result/error = %#v/%v", result, err)
	}
}

func TestGoSuitePostListCancellationPrecedesInspectionOutcome(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "discover", true: "full run"}[full], func(t *testing.T) {
			root := t.TempDir()
			writeSuiteFile(t, root, "behavior_test.go", "package behavior\n")
			listed, err := json.Marshal(testGoListPackage{Dir: root, TestGoFiles: []string{"behavior_test.go"}})
			if err != nil {
				t.Fatalf("encode listed package: %v", err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			cause := errors.New("wall-clock rail exhausted during discovery")
			calls := 0
			suite := NewGoSuite("fake-go")
			suite.runCommand = func(context.Context, string, []string, []string) runner.Result {
				calls++
				return suiteRunnerResult(string(listed)+"\n", "", nil)
			}
			suite.inspectFile = func(context.Context, string, string) (bool, error) {
				cancel(cause)
				return false, errors.New("competing filesystem failure")
			}

			if !full {
				_, err = suite.Discover(ctx, root)
			} else {
				var result SuiteResult
				result, err = suite.Run(ctx, root, nil, true)
				if result.Status != SuiteErrored || result.Command != nil {
					t.Fatalf("result = %#v", result)
				}
			}
			if !errors.Is(err, ErrSuiteCanceled) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want suite, context, and custom cancellation causes", err)
			}
			if strings.Contains(err.Error(), "competing filesystem failure") {
				t.Fatalf("inspection error won over cancellation: %v", err)
			}
			if calls != 1 {
				t.Fatalf("process calls = %d, want list only", calls)
			}
		})
	}
}

func TestDiscoverListedGoPackagesCancellationPrecedesTargetlessAndMalformedOutcomes(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "targetless", raw: ""},
		{name: "malformed", raw: "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cause := errors.New("wall-clock rail exhausted")
			cancel(cause)
			_, err := discoverListedGoPackages(ctx, t.TempDir(), []byte(test.raw), func(context.Context, string, string) (bool, error) {
				t.Fatal("file inspection unexpectedly ran")
				return false, nil
			})
			if !errors.Is(err, ErrSuiteCanceled) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want suite, context, and custom cancellation causes", err)
			}
		})
	}
}

func TestGoSuiteRunRechecksCancellationAfterDiscovery(t *testing.T) {
	for _, test := range []struct {
		name     string
		packages []string
		err      error
	}{
		{name: "missing"},
		{name: "error", err: errors.New("competing discovery failure")},
		{name: "runnable", packages: []string{"."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cause := errors.New("wall-clock rail exhausted after discovery")
			suite := NewGoSuite("fake-go")
			suite.discoverPackages = func(context.Context, string) ([]string, error) {
				cancel(cause)
				return test.packages, test.err
			}
			runnerCalled := false
			suite.runCommand = func(context.Context, string, []string, []string) runner.Result {
				runnerCalled = true
				return suiteRunnerResult("", "", nil)
			}

			result, err := suite.Run(ctx, t.TempDir(), nil, true)
			if result.Status != SuiteErrored || result.Command != nil {
				t.Fatalf("result = %#v", result)
			}
			if !errors.Is(err, ErrSuiteCanceled) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("error = %v, want suite, context, and custom cancellation causes", err)
			}
			if runnerCalled {
				t.Fatal("command runner called after discovery cancellation")
			}
		})
	}
}

func TestGoSuiteLocalRunSortsAndDeduplicatesPackages(t *testing.T) {
	suite := fakeGoSuite(t, "pass")
	result, err := suite.Run(context.Background(), t.TempDir(), []string{"z", "./z", "./a/../z", ".", "./", "a/..", "./a"}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantPackages := []string{".", "./a", "./z"}
	wantCommand := append([]string{suite.executable, "test"}, wantPackages...)
	if result.Status != SuitePassed || !slices.Equal(result.Packages, wantPackages) || !slices.Equal(result.Command, wantCommand) {
		t.Fatalf("result = %#v, want packages %v and command %v", result, wantPackages, wantCommand)
	}
}

func TestGoSuiteLocalRunRejectsUnsafePackages(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("create escaping symlink: %v", err)
	}
	for _, test := range []struct {
		name string
		pkg  string
	}{
		{name: "blank", pkg: ""},
		{name: "whitespace", pkg: "  "},
		{name: "absolute", pkg: filepath.Join(root, "pkg")},
		{name: "parent", pkg: "../outside"},
		{name: "parent only", pkg: ".."},
		{name: "windows drive backslash", pkg: `C:\repo\pkg`},
		{name: "windows drive slash", pkg: "C:/repo/pkg"},
		{name: "cleaned windows drive", pkg: "./C:/repo/pkg"},
		{name: "double slash cleaned windows drive", pkg: ".//C:/repo/pkg"},
		{name: "windows UNC", pkg: `\\server\share`},
		{name: "slash UNC", pkg: "//server/share"},
		{name: "wildcard segment", pkg: ".../pkg"},
		{name: "wildcard suffix", pkg: "pkg.../child"},
		{name: "wildcard prefix", pkg: "pkg/...child"},
		{name: "symlink escape", pkg: "./escape/pkg"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := fakeGoSuite(t, "must-not-run").Run(context.Background(), root, []string{test.pkg}, false)
			if err == nil {
				t.Fatalf("Run() result/error = %#v/%v, want programmer-input error", result, err)
			}
			if result.Status != SuiteErrored || result.Command != nil {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestGoSuiteLocalRunRejectsPackagesOutsideActiveModuleUniverse(t *testing.T) {
	root := t.TempDir()
	writeSuiteFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeSuiteFile(t, root, ".hidden/hidden.go", "package hidden\n")
	writeSuiteFile(t, root, "_hidden/hidden.go", "package hidden\n")
	writeSuiteFile(t, root, "vendor/lib/lib.go", "package lib\n")
	writeSuiteFile(t, root, "nested/go.mod", "module example.test/nested\n\ngo 1.25\n")
	writeSuiteFile(t, root, "nested/nested.go", "package nested\n")
	for _, pkg := range []string{"./.hidden", "./_hidden", "./vendor/lib", "./nested"} {
		t.Run(pkg, func(t *testing.T) {
			result, err := fakeGoSuite(t, "must-not-run").Run(context.Background(), root, []string{pkg}, false)
			if err == nil || result.Status != SuiteErrored || result.Command != nil {
				t.Fatalf("Run(%q) = (%#v, %v), want programmer-input rejection", pkg, result, err)
			}
		})
	}
}

func TestGoSuiteLocalRunTreatsNoPackagesAsMissing(t *testing.T) {
	result, err := fakeGoSuite(t, "must-not-run").Run(context.Background(), t.TempDir(), nil, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteMissing || result.Command != nil || result.Packages != nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteRejectsInvalidProgrammerInputs(t *testing.T) {
	root := t.TempDir()
	validSuite := fakeGoSuite(t, "must-not-run")
	for _, test := range []struct {
		name  string
		suite *GoSuite
		ctx   context.Context
		root  string
	}{
		{name: "nil context", suite: validSuite, root: root},
		{name: "nil receiver", ctx: context.Background(), root: root},
		{name: "blank executable", suite: NewGoSuite("  "), ctx: context.Background(), root: root},
		{name: "blank root", suite: validSuite, ctx: context.Background(), root: "  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.suite.Run(test.ctx, test.root, []string{"."}, false)
			if err == nil {
				t.Fatalf("Run() result/error = %#v/%v, want programmer-input error", result, err)
			}
			if result.Status != SuiteErrored || result.Command != nil {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestGoSuiteClassifiesTestFailure(t *testing.T) {
	result, err := fakeGoSuite(t, "fail").Run(context.Background(), t.TempDir(), []string{"."}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteFailed || result.Diagnostic != "suite stdout\nsuite stderr" {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteClassifiesMissingExecutableAsErrored(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-go")
	result, err := NewGoSuite(missing).Run(context.Background(), t.TempDir(), []string{"."}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteErrored || !strings.Contains(result.Diagnostic, "start behavioral suite") {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuitePreservesCancellationSentinelAndCause(t *testing.T) {
	suite := fakeGoSuite(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	result, err := suite.Run(ctx, t.TempDir(), []string{"."}, false)
	if result.Status != SuiteErrored {
		t.Fatalf("result = %#v", result)
	}
	if !errors.Is(err, ErrSuiteCanceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want suite cancellation and deadline sentinels", err)
	}
}

func TestGoSuitePreservesCustomCancellationCause(t *testing.T) {
	cause := errors.New("wall-clock rail exhausted")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	result, err := fakeGoSuite(t, "must-not-run").Run(ctx, t.TempDir(), []string{"."}, false)
	if result.Status != SuiteErrored {
		t.Fatalf("result = %#v", result)
	}
	if !errors.Is(err, ErrSuiteCanceled) || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("Run() error = %v, want suite, context, and custom cancellation causes", err)
	}
}

func TestGoSuiteBoundsCombinedDiagnostic(t *testing.T) {
	result, err := fakeGoSuite(t, "large-failure").Run(context.Background(), t.TempDir(), []string{"."}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteFailed {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Diagnostic) > suiteDiagnosticLimit || !strings.HasSuffix(result.Diagnostic, string(suiteTruncationMarker)) {
		t.Fatalf("diagnostic length/suffix = %d/%q", len(result.Diagnostic), result.Diagnostic[max(0, len(result.Diagnostic)-len(suiteTruncationMarker)):])
	}
}

func TestGoSuiteCleanupFailureIsErrored(t *testing.T) {
	suite := fakeGoSuite(t, "pass")
	suite.runCommand = func(context.Context, string, []string, []string) runner.Result {
		return runner.Result{
			Stdout:     runner.NewBuffer(suiteDiagnosticLimit, suiteTruncationMarker),
			Stderr:     runner.NewBuffer(suiteDiagnosticLimit, suiteTruncationMarker),
			CleanupErr: errors.New("injected cleanup failure"),
		}
	}

	result, err := suite.Run(context.Background(), t.TempDir(), []string{"."}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != SuiteErrored || !strings.Contains(result.Diagnostic, "clean up behavioral suite") {
		t.Fatalf("result = %#v", result)
	}
}

func TestGoSuiteRecordsNonnegativeDuration(t *testing.T) {
	suite := fakeGoSuite(t, "pass")
	times := []time.Time{time.Unix(20, 0), time.Unix(19, 0)}
	suite.now = func() time.Time {
		now := times[0]
		times = times[1:]
		return now
	}

	result, err := suite.Run(context.Background(), t.TempDir(), []string{"."}, false)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.DurationMS != 0 {
		t.Fatalf("duration = %d, want 0", result.DurationMS)
	}
}

type testGoListPackage struct {
	Dir          string   `json:"Dir"`
	TestGoFiles  []string `json:"TestGoFiles,omitempty"`
	XTestGoFiles []string `json:"XTestGoFiles,omitempty"`
}

func discoverGoPackages(t *testing.T, root string, packages ...testGoListPackage) ([]string, error) {
	t.Helper()
	return fakeGoSuiteWithPackages(t, "must-not-run", packages...).Discover(context.Background(), root)
}

func fakeGoSuite(t *testing.T, mode string) *GoSuite {
	t.Helper()
	return fakeGoSuiteWithPackages(t, mode)
}

func fakeGoSuiteWithPackages(t *testing.T, mode string, packages ...testGoListPackage) *GoSuite {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "fake-go")
	var listOutput strings.Builder
	for _, pkg := range packages {
		encoded, err := json.Marshal(pkg)
		if err != nil {
			t.Fatalf("encode fake go list package: %v", err)
		}
		listOutput.Write(encoded)
		listOutput.WriteByte('\n')
	}
	listBody := "printf %s " + shellQuote(listOutput.String()) + "\nexit 0\n"
	switch mode {
	case "list-malformed":
		listBody = "printf '{'\nexit 0\n"
	case "list-truncated":
		listBody = "head -c 2097152 /dev/zero | tr '\\000' x\nexit 0\n"
	case "list-fail":
		listBody = "printf 'go list failed\\n' >&2\nexit 1\n"
	case "list-sleep":
		listBody = "sleep 2\nexit 0\n"
	}
	body := "if [ \"$1\" = list ]; then\n" +
		"[ \"$#\" -eq 4 ] && [ \"$2\" = -e ] && [ \"$3\" = -json=Dir,TestGoFiles,XTestGoFiles ] && [ \"$4\" = ./... ] || exit 97\n" +
		listBody +
		"fi\n" +
		"[ \"$1\" = test ] || exit 96\n"
	switch mode {
	case "pass":
		body += "exit 0\n"
	case "fail":
		body += "printf 'suite stdout\\n'\nprintf 'suite stderr\\n' >&2\nexit 1\n"
	case "large-failure":
		body += "head -c 131072 /dev/zero | tr '\\000' o\nhead -c 131072 /dev/zero | tr '\\000' e >&2\nexit 1\n"
	case "sleep":
		body += "sleep 2\n"
	case "must-not-run", "list-malformed", "list-truncated", "list-fail", "list-sleep":
		body += "printf 'suite command unexpectedly ran\\n' >&2\nexit 99\n"
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	return NewGoSuite(executable)
}

func fakeGoSuiteWithPrivateListFailure(t *testing.T, stdout, stderr string) *GoSuite {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "fake-go")
	body := "[ \"$1\" = list ] || exit 96\n" +
		"[ \"$#\" -eq 4 ] && [ \"$2\" = -e ] && [ \"$3\" = -json=Dir,TestGoFiles,XTestGoFiles ] && [ \"$4\" = ./... ] || exit 97\n" +
		"printf %s " + shellQuote(stdout) + "\n" +
		"printf %s " + shellQuote(stderr) + " >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write private fake go: %v", err)
	}
	return NewGoSuite(executable)
}

func suiteRunnerResult(stdout, stderr string, runErr error) runner.Result {
	result := runner.Result{
		Stdout: runner.NewBuffer(suiteListOutputLimit, suiteTruncationMarker),
		Stderr: runner.NewBuffer(suiteDiagnosticLimit, suiteTruncationMarker),
		RunErr: runErr,
	}
	_, _ = result.Stdout.Write([]byte(stdout))
	_, _ = result.Stderr.Write([]byte(stderr))
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func oppositeGOOS() string {
	if runtime.GOOS == "windows" {
		return "linux"
	}
	return "windows"
}

func oppositeGOARCH() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

func assertSuiteBuildEnvironment(t *testing.T, environment []string, expected map[string]string) {
	t.Helper()
	counts := make(map[string]int)
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if _, owned := expected[upper]; owned {
			counts[upper]++
			values[upper] = value
		}
	}
	for key, want := range expected {
		if counts[key] != 1 || values[key] != want {
			t.Fatalf("environment %s = %q (%d entries), want %q exactly once: %v", key, values[key], counts[key], want, environment)
		}
	}
}

func assertSuiteArchitectureEnvironment(t *testing.T, environment []string) {
	t.Helper()
	defaults := map[string]string{
		"amd64": "GOAMD64=v1", "386": "GO386=sse2", "arm": "GOARM=7", "arm64": "GOARM64=v8.0",
		"mips": "GOMIPS=hardfloat", "mipsle": "GOMIPS=hardfloat", "mips64": "GOMIPS64=hardfloat", "mips64le": "GOMIPS64=hardfloat",
		"ppc64": "GOPPC64=power8", "ppc64le": "GOPPC64=power8", "riscv64": "GORISCV64=rva20u64", "wasm": "GOWASM=",
	}
	want := defaults[flywheel.ValidationGoBuildContext().GOARCH]
	keys := []string{"GOAMD64", "GO386", "GOARM", "GOARM64", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM"}
	counts := make(map[string]int)
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if ok && slices.Contains(keys, strings.ToUpper(key)) {
			counts[strings.ToUpper(key)]++
			if want == "" || entry != want {
				t.Fatalf("unexpected architecture environment %q for %s: %v", entry, flywheel.ValidationGoBuildContext().GOARCH, environment)
			}
		}
	}
	if want != "" {
		key, _, _ := strings.Cut(want, "=")
		if counts[key] != 1 {
			t.Fatalf("architecture environment %s count = %d, want exactly one: %v", key, counts[key], environment)
		}
	}
}

func environmentContainsExact(environment []string, want string) bool {
	return slices.Contains(environment, want)
}

func writeSuiteFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
