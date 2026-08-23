package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	suite.runCommand = func(_ context.Context, _ string, command []string) runner.Result {
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
		suite.runCommand = func(context.Context, string, []string) runner.Result {
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
			suite.runCommand = func(context.Context, string, []string) runner.Result {
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
			suite.runCommand = func(context.Context, string, []string) runner.Result {
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
	suite.runCommand = func(context.Context, string, []string) runner.Result {
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
