package flywheel

import (
	"context"
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gitcmd/gitcmdtest"
)

func TestChangedGoPackagesSelectsActualPackages(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "alpha/alpha.go", "package alpha\n")
	writeValidationFile(t, root, "beta/survivor.go", "package beta\n")

	packages, full, err := ChangedGoPackages(root, []string{
		"notes.md", "beta/deleted.go", "alpha/second.go", "alpha/alpha.go",
	})
	if err != nil {
		t.Fatalf("ChangedGoPackages() error = %v", err)
	}
	if full || !slices.Equal(packages, []string{"./alpha", "./beta"}) {
		t.Fatalf("ChangedGoPackages() = (%v, %t), want local alpha and beta", packages, full)
	}

	packages, full, err = ChangedGoPackages(root, []string{"docs/readme.md"})
	if err != nil || full || packages != nil {
		t.Fatalf("non-Go ChangedGoPackages() = (%v, %t, %v), want nil, false, nil", packages, full, err)
	}

	packages, full, err = ChangedGoPackages(root, []string{"go.sum", "alpha/alpha.go"})
	if err != nil || !full || packages != nil {
		t.Fatalf("module ChangedGoPackages() = (%v, %t, %v), want nil, true, nil", packages, full, err)
	}
}

func TestChangedGoPackagesUsesCanonicalRootPackage(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "main.go", "package project\n")
	packages, full, err := ChangedGoPackages(root, []string{"main.go"})
	if err != nil || full || !slices.Equal(packages, []string{"."}) {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want root package", packages, full, err)
	}
}

func TestChangedGoPackagesExpandsGoChangesWhenModuleHasIgnoreDirective(t *testing.T) {
	for _, contents := range []string{
		"module example.test/project\n\ngo 1.25\nignore ./generated\n",
		"module example.test/project\n\ngo 1.25\nignore (\n\t./generated\n)\n",
		"module example.test/project\n\ngo 1.25\nignore \"./generated path\"\n",
	} {
		root := t.TempDir()
		writeValidationFile(t, root, "go.mod", contents)
		writeValidationFile(t, root, "pkg/value.go", "package pkg\n")
		packages, full, err := ChangedGoPackages(root, []string{"pkg/value.go"})
		if err != nil || !full || packages != nil {
			t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want full suite for module ignore", packages, full, err)
		}
	}
}

func TestValidationModuleConfinementRejectsEscapingLocalReplacement(t *testing.T) {
	for _, replacement := range []string{"../outside", "/tmp/outside", `C:\outside`} {
		root := t.TempDir()
		writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\nreplace example.test/dep => "+replacement+"\n")
		if err := ValidateModuleConfinement(root); err == nil {
			t.Fatalf("ValidateModuleConfinement(%q) succeeded", replacement)
		}
	}
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\nreplace example.test/dep => ./deps/dep\n")
	if err := ValidateModuleConfinement(root); err != nil {
		t.Fatalf("confined replacement rejected: %v", err)
	}
}

func TestChangedGoPackagesIgnoresDeletedPackageWithoutSurvivors(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	packages, full, err := ChangedGoPackages(root, []string{"removed/deleted.go", "removed/readme.md"})
	if err != nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want no existing package", packages, full, err)
	}
}

func TestChangedGoPackagesIgnoresGoToolIgnoredSurvivors(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/_ignored.go", "package pkg\n")
	writeValidationFile(t, root, "pkg/.ignored.go", "package pkg\n")
	packages, full, err := ChangedGoPackages(root, []string{"pkg/deleted.go"})
	if err != nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want no active package survivors", packages, full, err)
	}
}

func TestChangedGoPackagesUsesActiveGoBuildFiles(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "os/value_windows.go", "package os\n")
	writeValidationFile(t, root, "tagged/value.go", "//go:build togi_never_enabled\n\npackage tagged\n")
	writeValidationFile(t, root, "arch/value_386.go", "package arch\n")
	writeValidationFile(t, root, "deleted/value_windows.go", "package deleted\n")
	writeValidationFile(t, root, "deleted/value_tagged.go", "//go:build togi_never_enabled\n\npackage deleted\n")
	context := build.Default
	context.GOOS = "linux"
	context.GOARCH = "amd64"

	packages, full, err := changedGoPackages(root, []string{
		"os/value_windows.go",
		"tagged/value.go",
		"arch/value_386.go",
		"deleted/removed.go",
	}, context)
	if err != nil || full || packages != nil {
		t.Fatalf("changedGoPackages() = (%v, %t, %v), want inactive files excluded", packages, full, err)
	}
}

func TestChangedGoPackagesIncludesActiveCgoAndTestOnlyPackages(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "cgo/value.go", "package cgo\n\nimport \"C\"\n")
	writeValidationFile(t, root, "tested/value_test.go", "package tested\n")
	context := build.Default
	context.CgoEnabled = true

	packages, full, err := changedGoPackages(root, []string{"cgo/value.go", "tested/deleted.go"}, context)
	if err != nil || full || !slices.Equal(packages, []string{"./cgo", "./tested"}) {
		t.Fatalf("changedGoPackages() = (%v, %t, %v), want active cgo and test-only packages", packages, full, err)
	}
}

func TestChangedGoPackagesRejectsMalformedBuildConstraint(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/value.go", "//go:build (\n\npackage pkg\n")
	packages, full, err := ChangedGoPackages(root, []string{"pkg/value.go"})
	if err == nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want malformed build constraint rejected", packages, full, err)
	}
}

func TestChangedGoPackagesExcludesCgoOnlyPackageWhenCgoDisabled(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "cgo/value.go", "package cgo\n\nimport \"C\"\n")
	context := build.Default
	context.CgoEnabled = false

	packages, full, err := changedGoPackages(root, []string{"cgo/value.go"}, context)
	if err != nil || full || packages != nil {
		t.Fatalf("changedGoPackages() = (%v, %t, %v), want cgo-only package excluded", packages, full, err)
	}
}

func TestChangedGoPackagesRejectsTestdataPaths(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/testdata/helper.go", "package testdata\n")
	packages, full, err := ChangedGoPackages(root, []string{"pkg/testdata/helper.go"})
	if err == nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want testdata path rejected", packages, full, err)
	}
}

func TestChangedGoPackagesRejectsChangedGoToolIgnoredBasenames(t *testing.T) {
	tests := []struct {
		name    string
		changed string
		files   map[string]string
	}{
		{name: "present underscore with sibling", changed: "pkg/_changed.go", files: map[string]string{"pkg/_changed.go": "package pkg\n", "pkg/live.go": "package pkg\n"}},
		{name: "present underscore without sibling", changed: "pkg/_changed.go", files: map[string]string{"pkg/_changed.go": "package pkg\n"}},
		{name: "present dot with sibling", changed: "pkg/.changed.go", files: map[string]string{"pkg/.changed.go": "package pkg\n", "pkg/live.go": "package pkg\n"}},
		{name: "present dot without sibling", changed: "pkg/.changed.go", files: map[string]string{"pkg/.changed.go": "package pkg\n"}},
		{name: "deleted underscore with sibling", changed: "pkg/_deleted.go", files: map[string]string{"pkg/live.go": "package pkg\n"}},
		{name: "deleted underscore without sibling", changed: "pkg/_deleted.go", files: map[string]string{"pkg/readme.md": "fixture\n"}},
		{name: "deleted dot with sibling", changed: "pkg/.deleted.go", files: map[string]string{"pkg/live.go": "package pkg\n"}},
		{name: "deleted dot without sibling", changed: "pkg/.deleted.go", files: map[string]string{"pkg/readme.md": "fixture\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
			for name, contents := range test.files {
				writeValidationFile(t, root, name, contents)
			}
			packages, full, err := ChangedGoPackages(root, []string{test.changed})
			if err == nil || packages != nil || full {
				t.Fatalf("ChangedGoPackages(%q) = (%v, %t, %v), want ignored Go basename rejected", test.changed, packages, full, err)
			}
		})
	}
}

func TestChangedGoPackagesRejectsAddedAndRenamedNestedModuleMarkers(t *testing.T) {
	tests := []struct {
		name    string
		changed []string
		present []string
	}{
		{name: "added go.mod", changed: []string{"added/go.mod"}, present: []string{"added/go.mod"}},
		{name: "added go.sum", changed: []string{"added/go.sum"}, present: []string{"added/go.sum"}},
		{name: "renamed go.mod", changed: []string{"old/go.mod", "new/go.mod"}, present: []string{"new/go.mod"}},
		{name: "renamed go.sum", changed: []string{"old/go.sum", "new/go.sum"}, present: []string{"new/go.sum"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
			for _, marker := range test.present {
				writeValidationFile(t, root, marker, "nested marker\n")
			}
			packages, full, err := ChangedGoPackages(root, test.changed)
			if err == nil || packages != nil || full {
				t.Fatalf("ChangedGoPackages(%v) = (%v, %t, %v), want nested marker rejected", test.changed, packages, full, err)
			}
		})
	}
}

func TestChangedGoPackagesRejectsDeletedNestedModuleMarkersByPath(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	for _, marker := range []string{"deleted/go.mod", "deleted/go.sum"} {
		packages, full, err := ChangedGoPackages(root, []string{marker})
		if err == nil || packages != nil || full {
			t.Fatalf("ChangedGoPackages(%q) = (%v, %t, %v), want deleted nested marker rejected", marker, packages, full, err)
		}
	}
}

func TestChangedGoPackagesRejectsNestedMarkerAlongsideRootMarker(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	packages, full, err := ChangedGoPackages(root, []string{"go.mod", "deleted/go.sum"})
	if err == nil || packages != nil || full {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want nested marker to fail before full expansion", packages, full, err)
	}
}

func TestChangedGoPackagesAllowsNonGoDotUnderscoreAndVendorFilenames(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	packages, full, err := ChangedGoPackages(root, []string{".golangci.yml", "_notes.md", "vendor"})
	if err != nil || full || packages != nil {
		t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want valid non-Go filenames ignored", packages, full, err)
	}
}

func TestChangedGoPackagesRejectsUnsafeOrExcludedPaths(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/pkg.go", "package pkg\n")
	writeValidationFile(t, root, "real/real.go", "package real\n")
	if err := os.Symlink(filepath.Join(root, "pkg", "pkg.go"), filepath.Join(root, "pkg", "internal-link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outsidePlaceholder(t), "outside.go"), filepath.Join(root, "pkg", "linked.go")); err != nil {
		t.Fatal(err)
	}
	writeValidationFile(t, root, "nested/go.mod", "module example.test/nested\n\ngo 1.25\n")
	writeValidationFile(t, root, "nested/nested.go", "package nested\n")
	outside := t.TempDir()
	writeValidationFile(t, outside, "escape.go", "package escape\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		"", " ", ".", "./pkg/pkg.go", "pkg//pkg.go", "pkg/../pkg/pkg.go",
		"../outside.go", "/tmp/outside.go", `C:\repo\file.go`, "C:/repo/file.go",
		".hidden/file.go", "_hidden/file.go", "vendor/lib/file.go", "nested/nested.go",
		".hidden/readme.md", "vendor/readme.md", "nested/readme.md", "nested/go.mod",
		"escape/escape.go", "escape/readme.md", "pkg/linked.go", "pkg/internal-link.go",
		"alias/real.go", "pkg/file.go\x00tail",
	}
	for _, changed := range paths {
		t.Run(strings.ReplaceAll(changed, "/", "_"), func(t *testing.T) {
			packages, full, err := ChangedGoPackages(root, []string{changed})
			if err == nil || packages != nil || full {
				t.Fatalf("ChangedGoPackages(%q) = (%v, %t, %v), want rejection", changed, packages, full, err)
			}
		})
	}
}

func TestChangedGoPackagesValidatesAllPathsBeforeExpandingModule(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	for _, companion := range []string{"../escape.go", ".hidden/escape.go", "vendor/lib.go"} {
		packages, full, err := ChangedGoPackages(root, []string{"go.mod", companion})
		if err == nil || packages != nil || full {
			t.Fatalf("ChangedGoPackages(%q) = (%v, %t, %v), want malformed companion path rejected", companion, packages, full, err)
		}
	}
}

func TestChangedGoPackagesRejectsInvalidRootAndBoundedInput(t *testing.T) {
	realRoot := t.TempDir()
	writeValidationFile(t, realRoot, "go.mod", "module example.test/project\n\ngo 1.25\n")
	linkParent := t.TempDir()
	linkRoot := filepath.Join(linkParent, "repo")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		root  string
		files []string
	}{
		{root: "", files: []string{"a.go"}},
		{root: linkRoot, files: []string{"a.go"}},
		{root: realRoot, files: make([]string, maxChangedPackageFiles+1)},
		{root: realRoot, files: []string{strings.Repeat("a", maxChangedPackagePathBytes+1) + ".go"}},
	} {
		packages, full, err := ChangedGoPackages(call.root, call.files)
		if err == nil || packages != nil || full {
			t.Fatalf("ChangedGoPackages() = (%v, %t, %v), want bounded rejection", packages, full, err)
		}
	}
}

func TestAttemptValidatorRejectsSemanticAndInfrastructureFailures(t *testing.T) {
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	replacement := validationFinding(t, "lint", "pkg/b.go", "lint/b", 1)
	original := TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}

	tests := []struct {
		name        string
		changed     []string
		integrity   TreeSnapshot
		gates       GateValidation
		suite       SuiteValidation
		wantKind    ValidationKind
		wantFailure string
	}{
		{name: "no-op", wantKind: ValidationSemanticFailure, wantFailure: "no worktree changes"},
		{name: "integrity error", changed: []string{"pkg/a.go"}, integrity: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("not go")}}, wantKind: ValidationInfrastructureFailure, wantFailure: "integrity"},
		{name: "gate error", changed: []string{"pkg/a.go"}, integrity: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, gates: GateValidation{Errored: []string{"lint"}}, wantKind: ValidationInfrastructureFailure, wantFailure: "gate"},
		{name: "persistent", changed: []string{"pkg/a.go"}, integrity: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, gates: GateValidation{Blocking: []finding.Finding{assigned}}, wantKind: ValidationSemanticFailure, wantFailure: "assigned"},
		{name: "replacement", changed: []string{"pkg/a.go"}, integrity: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, gates: GateValidation{Blocking: []finding.Finding{replacement}}, wantKind: ValidationSemanticFailure, wantFailure: "replacement"},
		{name: "suite failure", changed: []string{"pkg/a.go"}, integrity: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n// changed\n")}}, suite: SuiteValidation{Passed: false}, wantKind: ValidationSemanticFailure, wantFailure: "behavioral suite"},
		{name: "suite infrastructure", changed: []string{"pkg/a.go"}, integrity: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n// changed\n")}}, suite: SuiteValidation{InfrastructureError: "tool missing"}, wantKind: ValidationInfrastructureFailure, wantFailure: "behavioral suite"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
			if test.integrity.Files != nil {
				for file, contents := range test.integrity.Files {
					writeValidationFile(t, root, file, string(contents))
				}
			}
			gateCalls, suiteCalls := 0, 0
			validator := AttemptValidator{
				Original: original, Baseline: []finding.Finding{assigned},
				RunGates:    func(context.Context, string, Batch) GateValidation { gateCalls++; return test.gates },
				RunPackages: func(context.Context, string, []string, bool) SuiteValidation { suiteCalls++; return test.suite },
			}
			result := validator.Validate(context.Background(), root, test.changed, validationBatch(t, root, assigned))
			if result.Kind != test.wantKind || !strings.Contains(result.Failure, test.wantFailure) {
				t.Fatalf("Validate() = %#v, want %s containing %q", result, test.wantKind, test.wantFailure)
			}
			if test.name == "suite infrastructure" && (strings.Contains(result.Failure, "tool missing") || strings.Contains(result.Failure, root)) {
				t.Fatalf("Validate() leaked suite diagnostics: %q", result.Failure)
			}
			if test.name == "no-op" && (gateCalls != 0 || suiteCalls != 0) {
				t.Fatalf("callbacks after no-op = %d/%d", gateCalls, suiteCalls)
			}
		})
	}
}

func TestAttemptValidatorPassesDeepCopiesAndExpandsChangedPackages(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n// changed\n")
	writeValidationFile(t, root, "other/b.go", "package other\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	batch := validationBatch(t, root, assigned)
	baseline := []finding.Finding{assigned}
	var gotPackages []string
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}},
		Baseline: baseline,
		RunGates: func(_ context.Context, _ string, got Batch) GateValidation {
			got.Findings[0].Message = "mutated"
			return GateValidation{}
		},
		RunPackages: func(_ context.Context, _ string, packages []string, full bool) SuiteValidation {
			if full {
				t.Fatal("local changes requested full suite")
			}
			gotPackages = slices.Clone(packages)
			packages[0] = "mutated"
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"pkg/a.go", "other/b.go", "pkg/a.go"}, batch)
	if result.Kind != ValidationPassed || !slices.Equal(gotPackages, []string{"./other", "./pkg"}) {
		t.Fatalf("Validate() = %#v, packages = %v", result, gotPackages)
	}
	if !slices.Equal(result.ChangedFiles, []string{"other/b.go", "pkg/a.go"}) {
		t.Fatalf("Validate() changed files = %v, want sorted unique paths", result.ChangedFiles)
	}
	if batch.Findings[0].Message == "mutated" || baseline[0].Message == "mutated" {
		t.Fatal("validator callback aliased caller input")
	}
	if !result.Proof.present() {
		t.Fatal("passed validation omitted the prepared proof")
	}
	result.Proof.changed[0] = "mutated.go"
	if batch.proof.changed[0] == "mutated.go" {
		t.Fatal("validation result proof aliases the batch proof")
	}
}

func TestAttemptValidatorExecutesCallbacksAgainstPreparedTree(t *testing.T) {
	repo, head := workspaceRepository(t)
	writeValidationFile(t, repo, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, repo, "pkg/value.go", "package pkg\n\nconst Value = \"old\"\n")
	gitcmdtest.Git(t, repo, "add", "--", "go.mod", "pkg/value.go")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "add package")
	head = gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "validation-root")
	original, err := SnapshotAttempt(workspace.Path())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "pkg/value.go", "package pkg\n\nconst Value = \"fixed\"\n")
	proof := mustPrepareBatchProof(t, workspace)
	assigned := validationFinding(t, "lint", "pkg/value.go", "lint/value", 1)
	batch := validationBatch(t, workspace.Path(), assigned)
	batch.proof = proof
	seen := make([]string, 0, 2)
	validator := AttemptValidator{
		Original: original, Baseline: []finding.Finding{assigned},
		RunGates: func(_ context.Context, root string, _ Batch) GateValidation {
			seen = append(seen, root)
			contents, readErr := os.ReadFile(filepath.Join(root, "pkg/value.go"))
			if readErr != nil || !strings.Contains(string(contents), "fixed") {
				t.Fatalf("gate validation bytes = %q, %v", contents, readErr)
			}
			return GateValidation{}
		},
		RunPackages: func(_ context.Context, root string, _ []string, _ bool) SuiteValidation {
			seen = append(seen, root)
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), workspace.Path(), []string{"pkg/value.go"}, batch)
	if result.Kind != ValidationPassed || len(seen) != 2 || seen[0] != proof.ValidationRoot() || seen[1] != proof.ValidationRoot() {
		t.Fatalf("Validate() = %#v, callback roots = %v", result, seen)
	}
}

func TestAttemptValidatorRejectsValidationSnapshotMutation(t *testing.T) {
	repo, head := workspaceRepository(t)
	writeValidationFile(t, repo, "pkg/value.go", "package pkg\n")
	gitcmdtest.Git(t, repo, "add", "--", "pkg/value.go")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "add package")
	head = gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "validation-mutation")
	original, err := SnapshotAttempt(workspace.Path())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "pkg/value.go", "package pkg\n// fixed\n")
	proof := mustPrepareBatchProof(t, workspace)
	assigned := validationFinding(t, "lint", "pkg/value.go", "lint/value", 1)
	batch := validationBatch(t, workspace.Path(), assigned)
	batch.proof = proof
	validator := AttemptValidator{
		Original: original, Baseline: []finding.Finding{assigned},
		RunGates: func(_ context.Context, root string, _ Batch) GateValidation {
			file := filepath.Join(root, "pkg/value.go")
			if err := os.Chmod(file, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("package pkg\n// tampered\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return GateValidation{}
		},
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), workspace.Path(), []string{"pkg/value.go"}, batch)
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "prepared batch") {
		t.Fatalf("Validate() = %#v, want validation snapshot drift failure", result)
	}
	if err := workspace.ResetAttempt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(proof.ValidationRoot()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation snapshot survived reset: %v", err)
	}
}

func TestAttemptValidatorPackageExecutionCannotBeRedirectedByAdapterSymlinkSwap(t *testing.T) {
	repo, head := workspaceRepository(t)
	writeValidationFile(t, repo, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, repo, "pkg/value.go", "package pkg\n")
	gitcmdtest.Git(t, repo, "add", "--", "go.mod", "pkg/value.go")
	gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "add package")
	head = gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
	workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "validation-symlink")
	original, err := SnapshotAttempt(workspace.Path())
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, workspace.Path(), "pkg/value.go", "package pkg\n// staged safe bytes\n")
	proof := mustPrepareBatchProof(t, workspace)
	assigned := validationFinding(t, "lint", "pkg/value.go", "lint/value", 1)
	batch := validationBatch(t, workspace.Path(), assigned)
	batch.proof = proof
	external := t.TempDir()
	writeValidationFile(t, external, "value.go", "package pkg\n// redirected bytes\n")
	validator := AttemptValidator{
		Original: original, Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { return GateValidation{} },
		RunPackages: func(_ context.Context, root string, _ []string, _ bool) SuiteValidation {
			if err := os.Rename(filepath.Join(workspace.Path(), "pkg"), filepath.Join(workspace.Path(), "pkg-original")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, filepath.Join(workspace.Path(), "pkg")); err != nil {
				t.Fatal(err)
			}
			contents, readErr := os.ReadFile(filepath.Join(root, "pkg/value.go"))
			if readErr != nil || !strings.Contains(string(contents), "staged safe bytes") || strings.Contains(string(contents), "redirected") {
				t.Fatalf("suite root followed adapter swap: %q, %v", contents, readErr)
			}
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), workspace.Path(), []string{"pkg/value.go"}, batch)
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "prepared batch") {
		t.Fatalf("Validate() = %#v, want adapter drift rejection", result)
	}
	if err := workspace.ResetAttempt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptValidatorPassesWhenOnlyUnassignedBaselineBlockersRemain(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n// changed\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	unassigned := validationFinding(t, "complexity", "pkg/b.go", "complexity/b", 2)
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}},
		Baseline: []finding.Finding{assigned, unassigned},
		RunGates: func(context.Context, string, Batch) GateValidation {
			return GateValidation{Blocking: []finding.Finding{unassigned}}
		},
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"pkg/a.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationPassed || len(result.Findings) != 0 {
		t.Fatalf("Validate() = %#v, want engine-compatible passed result", result)
	}
}

func TestAttemptValidatorRequestsFullSuiteForModuleEdit(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	assigned := validationFinding(t, "lint", "go.mod", "lint/module", 1)
	full := false
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"go.mod": []byte("module example.test/project\n")}},
		Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { return GateValidation{} },
		RunPackages: func(_ context.Context, _ string, packages []string, requested bool) SuiteValidation {
			full = requested
			if packages != nil {
				t.Fatalf("full packages = %v", packages)
			}
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"go.mod"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationPassed || !full {
		t.Fatalf("Validate() = %#v, full = %t", result, full)
	}
}

func TestAttemptValidatorRequestsFullSuiteForDeletedRootModuleMarker(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	assigned := validationFinding(t, "lint", "go.sum", "lint/module", 1)
	suiteCalls := 0
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"go.mod": []byte("module example.test/project\n"), "go.sum": []byte("old sum\n")}},
		Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { return GateValidation{} },
		RunPackages: func(_ context.Context, _ string, packages []string, full bool) SuiteValidation {
			suiteCalls++
			if !full || packages != nil {
				t.Fatalf("RunPackages(%v, %t), want full suite", packages, full)
			}
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"go.sum"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationPassed || suiteCalls != 1 {
		t.Fatalf("Validate() = %#v, suite calls = %d", result, suiteCalls)
	}
}

func TestAttemptValidatorFailsClosedForDeletedNestedModuleMarker(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	assigned := validationFinding(t, "lint", "nested/go.mod", "lint/module", 1)
	gateCalls, suiteCalls := 0, 0
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"go.mod": []byte("module example.test/project\n"), "nested/go.mod": []byte("module example.test/nested\n")}},
		Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { gateCalls++; return GateValidation{} },
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			suiteCalls++
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"nested/go.mod"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "nested") || gateCalls != 0 || suiteCalls != 0 {
		t.Fatalf("Validate() = %#v, callbacks = %d/%d, want fail-closed nested marker", result, gateCalls, suiteCalls)
	}
}

func TestAttemptValidatorRejectsIntegrityFindingsBeforeCallbacks(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\nfunc A() {}\n")
	original := TreeSnapshot{Files: map[string][]byte{
		"pkg/a.go":      []byte("package pkg\nfunc A() {}\n"),
		"pkg/a_test.go": []byte("package pkg\nimport \"testing\"\nfunc TestA(t *testing.T) { A() }\n"),
	}}
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	gateCalls, suiteCalls := 0, 0
	validator := AttemptValidator{
		Original: original, Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { gateCalls++; return GateValidation{} },
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			suiteCalls++
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"pkg/a_test.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationSemanticFailure || len(result.Findings) == 0 || !strings.Contains(result.Failure, "integrity") {
		t.Fatalf("Validate() = %#v, want integrity semantic failure", result)
	}
	if gateCalls != 0 || suiteCalls != 0 {
		t.Fatalf("callbacks after integrity failure = %d/%d", gateCalls, suiteCalls)
	}
}

func TestAttemptValidatorAllowsWaivedIntegrityFindings(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\nfunc A() {}\n")
	original := TreeSnapshot{Files: map[string][]byte{
		"pkg/a.go":      []byte("package pkg\nfunc A() {}\n"),
		"pkg/a_test.go": []byte("package pkg\nimport \"testing\"\nfunc TestA(t *testing.T) { A() }\n"),
	}}
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	withoutWaiver := AttemptValidator{
		Original: original, Baseline: []finding.Finding{assigned},
		RunGates:    func(context.Context, string, Batch) GateValidation { return GateValidation{} },
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation { return SuiteValidation{Passed: true} },
	}
	blocked := withoutWaiver.Validate(context.Background(), root, []string{"pkg/a_test.go"}, validationBatch(t, root, assigned))
	if len(blocked.Findings) == 0 {
		t.Fatalf("unwaived validation = %#v, want integrity finding", blocked)
	}
	partial := withoutWaiver
	partial.WaivedFingerprints = map[string]struct{}{blocked.Findings[0].Fingerprint: {}}
	stillBlocked := partial.Validate(context.Background(), root, []string{"pkg/a_test.go"}, validationBatch(t, root, assigned))
	if stillBlocked.Kind != ValidationSemanticFailure || len(stillBlocked.Findings) == 0 {
		t.Fatalf("partially waived validation = %#v, want remaining integrity finding", stillBlocked)
	}
	waived := withoutWaiver
	waived.WaivedFingerprints = make(map[string]struct{}, len(blocked.Findings))
	for _, item := range blocked.Findings {
		waived.WaivedFingerprints[item.Fingerprint] = struct{}{}
	}
	result := waived.Validate(context.Background(), root, []string{"pkg/a_test.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationPassed {
		t.Fatalf("waived validation = %#v, want passed", result)
	}
}

func TestWaivedIntegrityFindingsMatchOnlyUnchangedIdentity(t *testing.T) {
	original := validationFinding(t, "integrity", "pkg/a_test.go", "togi/test-behavior", 3)
	waived := map[string]struct{}{original.Fingerprint: {}}

	moved := original
	moved.Line = 30
	moved.Fingerprint = finding.Fingerprint(moved)
	if got := unwaivedIntegrityFindings([]finding.Finding{moved}, waived); len(got) != 0 {
		t.Fatalf("moved finding = %#v, want waived", got)
	}

	changed := original
	changed.Snippet = "func TestA(t *testing.T) { t.Skip(\"approved\") }"
	changed.Fingerprint = finding.Fingerprint(changed)
	if got := unwaivedIntegrityFindings([]finding.Finding{changed}, waived); len(got) != 1 || got[0].Fingerprint != changed.Fingerprint {
		t.Fatalf("changed snippet = %#v, want unwaived finding", got)
	}
}

// Filtering in place would leave the caller holding a rewritten slice, which
// is a silent corruption rather than a visible failure. Pin the contract.
func TestUnwaivedIntegrityFindingsLeavesItsInputIntact(t *testing.T) {
	approved := validationFinding(t, "integrity", "pkg/a_test.go", "togi/test-behavior", 3)
	blocking := validationFinding(t, "integrity", "pkg/b_test.go", "togi/test-behavior", 4)
	findings := []finding.Finding{approved, blocking}

	result := unwaivedIntegrityFindings(findings, map[string]struct{}{approved.Fingerprint: {}})
	if len(result) != 1 || result[0].Fingerprint != blocking.Fingerprint {
		t.Fatalf("filtered = %#v, want only the unapproved finding", result)
	}
	if len(findings) != 2 || findings[0].Fingerprint != approved.Fingerprint || findings[1].Fingerprint != blocking.Fingerprint {
		t.Fatalf("filtering rewrote its input: %#v", findings)
	}
}

func TestAttemptValidatorStopsOnCancellationAndPreservesCause(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n// changed\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	cause := "wall-clock rail exhausted: " + strings.Repeat("界", maxBriefDiagnosticFieldBytes)
	gateCalls, suiteCalls := 0, 0
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { gateCalls++; return GateValidation{} },
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			suiteCalls++
			return SuiteValidation{Passed: true}
		},
	}
	ctx, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(assertError(cause))
	result := validator.Validate(ctx, root, []string{"pkg/a.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "wall-clock rail exhausted") || len(result.Failure) > maxBriefDiagnosticFieldBytes || !utf8.ValidString(result.Failure) || gateCalls != 0 || suiteCalls != 0 {
		t.Fatalf("Validate() = %#v, callbacks = %d/%d", result, gateCalls, suiteCalls)
	}
}

func TestAttemptValidatorDoesNotStartSuiteAfterGateCancellation(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n// changed\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	suiteCalls := 0
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation {
			cancel(assertError("iteration rail exhausted"))
			return GateValidation{}
		},
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			suiteCalls++
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(ctx, root, []string{"pkg/a.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "iteration rail exhausted") || suiteCalls != 0 {
		t.Fatalf("Validate() = %#v, suite calls = %d", result, suiteCalls)
	}
}

func TestAttemptValidatorBoundsFailuresAndRejectsMalformedEvidence(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n// changed\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	malformed := assigned
	malformed.Fingerprint = "wrong"
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation {
			return GateValidation{Blocking: []finding.Finding{malformed}}
		},
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation { return SuiteValidation{Passed: true} },
	}
	result := validator.Validate(context.Background(), root, []string{"pkg/a.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationInfrastructureFailure || len(result.Failure) > maxBriefDiagnosticFieldBytes || strings.Contains(result.Failure, root) {
		t.Fatalf("Validate() = %#v, want bounded opaque infrastructure failure", result)
	}
}

func TestAttemptValidatorRejectsOversizedFindingEvidenceBeforeSuite(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "go.mod", "module example.test/project\n\ngo 1.25\n")
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n// changed\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	oversized := assigned
	oversized.Occurrences = make([]finding.Occurrence, maxValidationOccurrences+1)
	for index := range oversized.Occurrences {
		oversized.Occurrences[index] = finding.Occurrence{Line: index + 2}
	}
	suiteCalls := 0
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{"pkg/a.go": []byte("package pkg\n")}}, Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation {
			return GateValidation{Blocking: []finding.Finding{oversized}}
		},
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			suiteCalls++
			return SuiteValidation{Passed: true}
		},
	}
	result := validator.Validate(context.Background(), root, []string{"pkg/a.go"}, validationBatch(t, root, assigned))
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "resource") || suiteCalls != 0 {
		t.Fatalf("Validate() = %#v, suite calls = %d, want bounded infrastructure rejection", result, suiteCalls)
	}
}

func TestAttemptValidatorFailsClosedWhenGateCallbackInvalidatesPreparedBatch(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	valid := true
	batch := validationBatch(t, root, assigned)
	batch.proof.verify = func(context.Context, BatchProof) error {
		if !valid {
			return errors.New("prepared worktree changed")
		}
		return nil
	}
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{}},
		Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation {
			valid = false
			return GateValidation{}
		},
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			t.Fatal("suite must not run after proof invalidation")
			return SuiteValidation{}
		},
	}

	result := validator.Validate(context.Background(), root, []string{"pkg/a.go"}, batch)
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "prepared batch") {
		t.Fatalf("result = %#v, want infrastructure proof failure", result)
	}
	if result.Proof.present() {
		t.Fatal("failed validation retained a batch proof")
	}
}

func TestAttemptValidatorFailsClosedWhenSuiteCallbackInvalidatesPreparedBatch(t *testing.T) {
	root := t.TempDir()
	writeValidationFile(t, root, "pkg/a.go", "package pkg\n")
	assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
	valid := true
	batch := validationBatch(t, root, assigned)
	batch.proof.verify = func(context.Context, BatchProof) error {
		if !valid {
			return errors.New("prepared worktree changed")
		}
		return nil
	}
	validator := AttemptValidator{
		Original: TreeSnapshot{Files: map[string][]byte{}},
		Baseline: []finding.Finding{assigned},
		RunGates: func(context.Context, string, Batch) GateValidation { return GateValidation{} },
		RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
			valid = false
			return SuiteValidation{Passed: true}
		},
	}

	result := validator.Validate(context.Background(), root, []string{"pkg/a.go"}, batch)
	if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "prepared batch") || result.Proof.present() {
		t.Fatalf("result = %#v, want infrastructure proof failure", result)
	}
}

func TestAttemptValidatorProofFailureDominatesCallbackCancellation(t *testing.T) {
	for _, phase := range []string{"gate", "suite"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			writeValidationFile(t, root, "pkg/a.go", "package pkg\n")
			assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
			valid := true
			batch := validationBatch(t, root, assigned)
			batch.proof.verify = func(context.Context, BatchProof) error {
				if !valid {
					return errors.New("prepared worktree changed")
				}
				return nil
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			mutateAndCancel := func() {
				valid = false
				cancel(assertError("callback canceled"))
			}
			validator := AttemptValidator{
				Original: TreeSnapshot{Files: map[string][]byte{}}, Baseline: []finding.Finding{assigned},
				RunGates: func(context.Context, string, Batch) GateValidation {
					if phase == "gate" {
						mutateAndCancel()
					}
					return GateValidation{}
				},
				RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
					if phase == "suite" {
						mutateAndCancel()
					}
					return SuiteValidation{Passed: true}
				},
			}

			result := validator.Validate(ctx, root, []string{"pkg/a.go"}, batch)
			if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "prepared batch") || strings.Contains(result.Failure, "callback canceled") {
				t.Fatalf("result = %#v, want proof failure precedence", result)
			}
		})
	}
}

func TestAttemptValidatorRejectsIgnoredEntryCreatedByCallbacks(t *testing.T) {
	for _, phase := range []string{"gate", "suite"} {
		for _, artifact := range []string{"file", "empty-directory"} {
			t.Run(phase+"/"+artifact, func(t *testing.T) {
				repo, _ := workspaceRepositoryWithIgnoredGenerated(t)
				writeValidationFile(t, repo, "go.mod", "module example.test/project\n\ngo 1.25\n")
				writeValidationFile(t, repo, "pkg/a.go", "package pkg\n")
				gitcmdtest.Git(t, repo, "add", "--", "go.mod", "pkg/a.go")
				gitcmdtest.Git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "add Go package")
				head := gitcmdtest.Git(t, repo, "rev-parse", "HEAD")
				workspace := createTestWorkspace(t, repo, head, filepath.Join(t.TempDir(), "workspace"), "ignored-"+phase+"-"+artifact)
				original, err := SnapshotAttempt(workspace.Path())
				if err != nil {
					t.Fatal(err)
				}
				writeWorkspaceFile(t, workspace.Path(), "pkg/a.go", "package pkg\n// fixed\n")
				proof := mustPrepareBatchProof(t, workspace)
				assigned := validationFinding(t, "lint", "pkg/a.go", "lint/a", 1)
				batch := validationBatch(t, workspace.Path(), assigned)
				batch.proof = proof
				createIgnored := func() {
					directory := filepath.Join(workspace.Path(), "generated")
					if artifact == "empty-directory" {
						directory = filepath.Join(directory, "nested")
					}
					if err := os.MkdirAll(directory, 0o755); err != nil {
						t.Fatal(err)
					}
					if artifact == "file" {
						writeWorkspaceFile(t, workspace.Path(), "generated/output.txt", "ignored\n")
					}
				}
				validator := AttemptValidator{
					Original: original, Baseline: []finding.Finding{assigned},
					RunGates: func(context.Context, string, Batch) GateValidation {
						if phase == "gate" {
							createIgnored()
						}
						return GateValidation{}
					},
					RunPackages: func(context.Context, string, []string, bool) SuiteValidation {
						if phase == "suite" {
							createIgnored()
						}
						return SuiteValidation{Passed: true}
					},
				}

				result := validator.Validate(context.Background(), workspace.Path(), []string{"pkg/a.go"}, batch)
				if result.Kind != ValidationInfrastructureFailure || !strings.Contains(result.Failure, "prepared batch") {
					t.Fatalf("result = %#v, want ignored-entry proof failure", result)
				}
				if err := workspace.ResetAttempt(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(workspace.Path(), "generated")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("ignored callback output survived reset: %v", err)
				}
			})
		}
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }

func validationFinding(t *testing.T, gateName, file, rule string, occurrences int) finding.Finding {
	t.Helper()
	raw := make([]finding.Finding, occurrences)
	for index := range raw {
		raw[index] = finding.Finding{Gate: gateName, Language: "go", RuleID: rule, Severity: finding.Error, File: file, Line: index + 1, Snippet: rule, Message: rule}
	}
	grouped, err := finding.Group(raw)
	if err != nil {
		t.Fatal(err)
	}
	return grouped[0]
}

func validationBatch(t *testing.T, root string, item finding.Finding) Batch {
	t.Helper()
	plan, err := NewPlan([]finding.Finding{item})
	if err != nil {
		t.Fatal(err)
	}
	batch := plan.Batches[0]
	batch.proof = BatchProof{
		owner:      &struct{}{},
		tree:       strings.Repeat("a", 40),
		changed:    []string{item.File},
		verify:     func(context.Context, BatchProof) error { return nil },
		validation: &validationSnapshot{private: &privateTempDir{path: root}},
	}
	return batch
}

func writeValidationFile(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func outsidePlaceholder(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeValidationFile(t, root, "outside.go", "package outside\n")
	return root
}
