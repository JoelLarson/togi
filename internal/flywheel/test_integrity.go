package flywheel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gitcmd"
)

const (
	maxIntegrityFiles         = 8_192
	maxIntegrityFileBytes     = 2 << 20
	maxIntegritySnapshotBytes = 32 << 20
	maxIntegrityTokens        = 500_000
	maxIntegrityFindings      = 10_000
	maxIntegrityGitListBytes  = 4 << 20
	maxIntegrityWalkEntries   = 65_536
	maxIntegrityWalkPathBytes = 4 << 20
	maxIntegrityWalkDepth     = 64
	maxIntegrityWitnessWork   = 2_048
)

var errIntegrityResourceLimit = errors.New("integrity resource limit exceeded")

var snapshotAttemptHook func(stage, file string) error

// IntegrityResult reports deterministic integrity findings separately from
// failures that prevented the attempted tree from being judged.
type IntegrityResult struct {
	Findings []finding.Finding
	Err      error
}

type integrityFile struct {
	path           string
	source         []byte
	parsed         *ast.File
	fset           *token.FileSet
	packageName    string
	build          string
	imports        []integrityImport
	comments       []string
	info           *types.Info
	typeErrors     []types.Error
	typeValid      bool
	witnessObjects map[types.Object]string
	eligible       bool
	active         bool
}

type testDeclaration struct {
	key     string
	file    *integrityFile
	decl    *ast.FuncDecl
	kind    string
	name    string
	line    int
	tokens  []integrityToken
	imports []integrityImport
}

type integrityImport struct {
	alias string
	path  string
}

type behaviorDeclaration struct {
	key     string
	file    *integrityFile
	line    int
	name    string
	tokens  []integrityToken
	newTest bool
	node    ast.Decl
}

type identifierWitnesses struct {
	byDirectory map[string]map[string]string
	byObject    map[types.Object]string
}

type packageWitnesses struct {
	names       map[string]string
	directories map[string]string
	packages    map[string]packageWitness
	module      string
	imports     map[string]string
}

type packageWitness struct {
	oldName      string
	newName      string
	newDirectory string
}

type productionDeclaration struct {
	directory   string
	packageName string
	name        string
	shape       string
	object      types.Object
	file        *integrityFile
	variant     string
}

type integrityToken struct {
	token  token.Token
	text   string
	offset int
}

// CheckIntegrity compares the trusted original tree with one agent attempt.
func CheckIntegrity(original, attempted TreeSnapshot) IntegrityResult {
	if invalidNewTestBuildConstraints(original, attempted) {
		return IntegrityResult{Err: errors.New("inspect attempted tests: invalid new test")}
	}
	suppressions, err := checkSuppressionIntegrity(withoutTestdata(original), withoutTestdata(attempted))
	if err != nil {
		return IntegrityResult{Err: err}
	}
	tests, err := checkTestIntegrity(original, attempted)
	if err != nil {
		return IntegrityResult{Err: err}
	}
	all := append(suppressions, tests...)
	if len(all) > maxIntegrityFindings {
		return IntegrityResult{Err: errors.New("inspect integrity snapshot: resource limit exceeded")}
	}
	grouped, err := finding.Group(all)
	if err != nil {
		return IntegrityResult{Err: fmt.Errorf("group integrity findings: %w", err)}
	}
	return IntegrityResult{Findings: grouped}
}

func invalidNewTestBuildConstraints(original, attempted TreeSnapshot) bool {
	for _, file := range sortedSnapshotFiles(attempted) {
		if !strings.HasSuffix(file, "_test.go") || isTestdataPath(file) || !eligibleSnapshotGoPath(file, attempted) || bytes.Equal(original.Files[file], attempted.Files[file]) {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, attempted.Files[file], parser.ParseComments|parser.AllErrors)
		if err != nil || parsed == nil {
			continue
		}
		testLike := false
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && testLikeName(function.Name.Name) {
				testLike = true
				break
			}
		}
		if !testLike {
			continue
		}
		goBuild := 0
		for _, group := range parsed.Comments {
			if group.End() > parsed.Package {
				continue
			}
			for _, comment := range group.List {
				line := strings.TrimSpace(comment.Text)
				if strings.HasPrefix(line, "//go:build") {
					goBuild++
				}
				if strings.HasPrefix(line, "//go:build") || strings.HasPrefix(line, "// +build") {
					if _, err := constraint.Parse(line); err != nil {
						return true
					}
				}
			}
		}
		if goBuild > 1 {
			return true
		}
	}
	return false
}

func withoutTestdata(snapshot TreeSnapshot) TreeSnapshot {
	files := make(map[string][]byte, len(snapshot.Files))
	for file, contents := range snapshot.Files {
		if !isTestdataPath(file) {
			files[file] = contents
		}
	}
	return TreeSnapshot{Files: files}
}

func checkTestIntegrity(original, attempted TreeSnapshot) ([]finding.Finding, error) {
	changedPaths, err := boundedChangedPaths(original, attempted)
	if err != nil {
		return nil, errors.New("inspect test snapshot: resource limit exceeded")
	}
	changed := make(map[string]bool, len(changedPaths))
	for _, file := range changedPaths {
		changed[file] = true
	}
	before, err := parseIntegrityFiles(original, changed)
	if err != nil {
		return nil, fmt.Errorf("inspect original test snapshot: %w", publicIntegrityError(err))
	}
	after, err := parseIntegrityFiles(attempted, changed)
	if err != nil {
		return nil, fmt.Errorf("inspect attempted test snapshot: %w", publicIntegrityError(err))
	}
	witnessWork := maxIntegrityWitnessWork
	packageRenames, err := witnessedPackageRenames(before, after, modulePath(original), modulePath(attempted), &witnessWork)
	if err != nil {
		return nil, fmt.Errorf("inspect production package witnesses: %w", publicIntegrityError(err))
	}
	if err := validateNewTests(before, after, packageRenames); err != nil {
		return nil, errors.New("inspect attempted tests: invalid new test")
	}
	identifierRenames, err := witnessedIdentifierRenames(before, after, packageRenames, &witnessWork)
	if err != nil {
		return nil, errors.New("inspect test witnesses: resource limit exceeded")
	}

	beforeTests := discoverTests(before)
	afterTests := discoverTests(after)
	findings := make([]finding.Finding, 0)
	testKeys := make([]string, 0, len(beforeTests))
	for key := range beforeTests {
		testKeys = append(testKeys, key)
	}
	sort.Strings(testKeys)
	for _, key := range testKeys {
		baselines := beforeTests[key]
		candidates := afterTests[remapIntegrityKey(key, packageRenames.directories)]
		for _, baseline := range baselines {
			if len(baselines) != 1 || len(candidates) != 1 || !sameTestEnvironment(baseline, candidates[0], packageRenames) {
				findings = append(findings, testFinding("togi/test-discovery", baseline, "test declaration is no longer discoverable with its original identity"))
				continue
			}
		}
	}
	beforeDeclarations := testBehaviorDeclarations(before)
	afterDeclarations := testBehaviorDeclarations(after)
	declarationKeys := make([]string, 0, len(beforeDeclarations))
	for key := range beforeDeclarations {
		declarationKeys = append(declarationKeys, key)
	}
	sort.Strings(declarationKeys)
	baselineKeys := make(map[string]bool, len(declarationKeys))
	for _, key := range declarationKeys {
		baselineKeys[remapIntegrityKey(key, packageRenames.directories)] = true
		baselines := beforeDeclarations[key]
		candidates := afterDeclarations[remapIntegrityKey(key, packageRenames.directories)]
		for _, baseline := range baselines {
			changed := len(baselines) != 1 || len(candidates) != 1
			if !changed {
				current := candidates[0]
				changed = !sameBehaviorEnvironment(baseline, current, packageRenames) ||
					!equalIntegrityTokens(remapBoundTestTokens(baseline, current, identifierRenames, packageRenames), current.tokens)
			}
			if changed {
				findings = append(findings, behaviorFinding(baseline))
			}
		}
	}
	afterKeys := make([]string, 0, len(afterDeclarations))
	for key := range afterDeclarations {
		afterKeys = append(afterKeys, key)
	}
	sort.Strings(afterKeys)
	for _, key := range afterKeys {
		if baselineKeys[key] {
			continue
		}
		for _, declaration := range afterDeclarations[key] {
			if !declaration.newTest {
				findings = append(findings, behaviorFinding(declaration))
			}
		}
	}
	for _, filePath := range sortedIntegrityFiles(before) {
		baseline := before[filePath]
		if !eligibleGoTestPath(baseline) {
			continue
		}
		current := after[remapIntegrityFilePath(filePath, packageRenames.directories)]
		if current == nil || !importsPreserved(baseline.imports, current.imports, packageRenames) || !commentsPreserved(baseline.comments, current.comments) {
			findings = append(findings, integrityFinding("togi/test-behavior", filePath, 1, path.Base(filePath), "existing test behavior changed during a fix attempt"))
		}
	}

	fixturePaths := make([]string, 0)
	for file := range original.Files {
		if isTestdataPath(file) {
			fixturePaths = append(fixturePaths, file)
		}
	}
	sort.Strings(fixturePaths)
	for _, file := range fixturePaths {
		current, exists := attempted.Files[file]
		if exists && bytes.Equal(original.Files[file], current) {
			continue
		}
		findings = append(findings, integrityFinding("togi/test-fixture", file, 1, path.Base(file), "existing test fixture changed during a fix attempt"))
	}
	fixtureSuppressions, err := checkNewFixtureSuppressions(original, attempted)
	if err != nil {
		return nil, fmt.Errorf("inspect new test fixtures: %w", publicSuppressionError(err))
	}
	findings = append(findings, fixtureSuppressions...)
	return findings, nil
}

func remapIntegrityFilePath(file string, directories map[string]string) string {
	directory := path.Dir(file)
	if renamed, ok := directories[directory]; ok {
		return path.Join(renamed, path.Base(file))
	}
	return file
}

func checkNewFixtureSuppressions(original, attempted TreeSnapshot) ([]finding.Finding, error) {
	baseline := TreeSnapshot{Files: make(map[string][]byte)}
	withAdded := TreeSnapshot{Files: make(map[string][]byte)}
	realPaths := make(map[string]string)
	probe := fixtureGoProbe{}
	added := 0
	for _, file := range sortedSnapshotFiles(attempted) {
		contents := attempted.Files[file]
		if _, existed := original.Files[file]; existed || !isTestdataPath(file) {
			continue
		}
		valid, err := probe.valid(file, contents)
		if err != nil {
			return nil, err
		}
		if valid {
			synthetic := syntheticFixtureGoPath(file)
			withAdded.Files[synthetic] = contents
			realPaths[synthetic] = file
			added++
		}
	}
	if added == 0 {
		return nil, nil
	}
	for _, file := range sortedSnapshotFiles(original) {
		contents := original.Files[file]
		if !isTestdataPath(file) {
			continue
		}
		valid, err := probe.valid(file, contents)
		if err != nil {
			return nil, err
		}
		if valid {
			synthetic := syntheticFixtureGoPath(file)
			baseline.Files[synthetic] = contents
			withAdded.Files[synthetic] = contents
			realPaths[synthetic] = file
		}
	}
	findings, err := checkSuppressionIntegrity(baseline, withAdded)
	if err != nil {
		return nil, err
	}
	mapped := make([]finding.Finding, 0, len(findings))
	for _, item := range findings {
		real := realPaths[item.File]
		if real == "" {
			return nil, errors.New("fixture suppression path was not mapped")
		}
		mapped = append(mapped, integrityFinding(item.RuleID, real, item.Line, item.Snippet, item.Message))
		for _, occurrence := range item.Occurrences {
			mapped = append(mapped, integrityFinding(item.RuleID, real, occurrence.Line, item.Snippet, item.Message))
		}
	}
	return mapped, nil
}

type fixtureGoProbe struct {
	files int
	bytes int
}

func (probe *fixtureGoProbe) valid(file string, source []byte) (bool, error) {
	if len(source) > maxSuppressionFileBytes {
		return false, errSuppressionResourceLimit
	}
	if !sourceNestingWithinLimit(file, source) {
		return false, errSuppressionResourceLimit
	}
	_, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, parser.ParseComments|parser.AllErrors)
	if err != nil {
		if path.Ext(file) == ".go" {
			return false, err
		}
		return false, nil
	}
	probe.files++
	if probe.files > maxSuppressionGoFiles || probe.bytes > maxSuppressionGoBytes-len(source) {
		return false, errSuppressionResourceLimit
	}
	probe.bytes += len(source)
	return true, nil
}

func syntheticFixtureGoPath(file string) string {
	return file + ".togi.go"
}

func sortedSnapshotFiles(snapshot TreeSnapshot) []string {
	files := make([]string, 0, len(snapshot.Files))
	for file := range snapshot.Files {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

func remapIntegrityKey(key string, directories map[string]string) string {
	directory, remainder, found := strings.Cut(key, "\x00")
	if !found {
		return key
	}
	if renamed, ok := directories[directory]; ok {
		directory = renamed
	}
	return directory + "\x00" + remainder
}

func parseIntegrityFiles(snapshot TreeSnapshot, changed map[string]bool) (map[string]*integrityFile, error) {
	paths := make([]string, 0, len(snapshot.Files))
	for file := range snapshot.Files {
		if path.Ext(file) == ".go" && !isTestdataPath(file) {
			paths = append(paths, file)
		}
	}
	sort.Strings(paths)
	result := make(map[string]*integrityFile, len(paths))
	fset := token.NewFileSet()
	totalTokens := 0
	for _, file := range paths {
		source := snapshot.Files[file]
		parsed, err := parser.ParseFile(fset, file, source, parser.ParseComments|parser.AllErrors)
		if err != nil {
			if !strings.HasSuffix(file, "_test.go") && !changed[file] {
				continue
			}
			return nil, err
		}
		tokens, err := scanIntegrityTokens(file, source, nil)
		if err != nil {
			return nil, err
		}
		totalTokens += len(tokens)
		if totalTokens > maxIntegrityTokens {
			return nil, errIntegrityResourceLimit
		}
		buildDirectives := integrityBuildDirectives(parsed)
		eligible := eligibleSnapshotGoPath(file, snapshot)
		active := eligible && integrityBuildActive(file, buildDirectives)
		result[file] = &integrityFile{
			path: file, source: source, parsed: parsed, fset: fset,
			packageName: parsed.Name.Name, build: buildDirectives, imports: integrityImports(parsed), comments: integrityComments(parsed),
			eligible: eligible, active: active, typeValid: eligible && !active,
		}
	}
	if err := typecheckIntegrityFiles(fset, result, modulePath(snapshot)); err != nil {
		return nil, err
	}
	return result, nil
}

func typecheckIntegrityFiles(fset *token.FileSet, files map[string]*integrityFile, module string) error {
	groups := make(map[string][]*integrityFile)
	for _, filePath := range sortedIntegrityFiles(files) {
		file := files[filePath]
		if !file.eligible || !file.active {
			continue
		}
		key := path.Dir(file.path) + "\x00" + file.packageName
		groups[key] = append(groups[key], file)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	imports := integritySnapshotImporter(files, module, fset)
	for _, key := range keys {
		group := groups[key]
		typeErrors := make([]types.Error, 0)
		info := &types.Info{
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}
		parsed := make([]*ast.File, 0, len(group))
		for _, file := range group {
			parsed = append(parsed, file.parsed)
			file.info = info
			file.typeValid = true
		}
		configuration := types.Config{
			Importer:  imports,
			GoVersion: "go1.25", DisableUnusedImportCheck: true, Error: func(err error) {
				if typed, ok := err.(types.Error); ok {
					typeErrors = append(typeErrors, typed)
				}
			},
		}
		_, _ = configuration.Check(key, fset, parsed, info)
		for _, file := range group {
			file.typeErrors = append([]types.Error(nil), typeErrors...)
			for _, typeError := range typeErrors {
				if !opaqueSelectorTypeError(file, typeError, module) {
					file.typeValid = false
					break
				}
			}
		}
	}
	return typecheckInactiveIntegrityFiles(fset, files, module)
}

func typecheckInactiveIntegrityFiles(fset *token.FileSet, files map[string]*integrityFile, module string) error {
	labels := declarationPlaceholderLabels(files)
	groups := make(map[string][]*integrityFile)
	owned := make(map[string][]*integrityFile)
	for _, filePath := range sortedIntegrityFiles(files) {
		file := files[filePath]
		if !file.eligible || strings.HasSuffix(filePath, "_test.go") {
			continue
		}
		packageKey := path.Dir(filePath) + "\x00" + file.packageName
		variant := integrityVariantKey(file)
		owned[packageKey] = append(owned[packageKey], file)
		if variant != "\x00" && !file.active {
			groups[packageKey+"\x00"+variant] = append(groups[packageKey+"\x00"+variant], file)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	work := maxIntegrityWitnessWork
	packages := indexVariantIntegrityPackages(files, module)
	for _, key := range keys {
		targets := groups[key]
		directory, remainder, _ := strings.Cut(key, "\x00")
		packageName, _, _ := strings.Cut(remainder, "\x00")
		packageKey := directory + "\x00" + packageName
		candidates := owned[packageKey]
		contexts, err := forcedIntegrityVariants(targets[0], packages)
		if err != nil {
			return err
		}
		var info *types.Info
		var selected []*integrityFile
		valid := false
		for _, forced := range contexts {
			info, selected, valid, err = checkInactiveIntegrityVariant(fset, key, candidates, packages, module, forced, &work)
			if err != nil {
				return err
			}
			if valid {
				break
			}
		}
		for _, target := range targets {
			target.typeValid = valid
			target.info = info
			target.witnessObjects = make(map[types.Object]string)
			if valid {
				for _, file := range selected {
					for _, declaration := range file.parsed.Decls {
						identifier := declarationIdentifier(declaration)
						if identifier != nil {
							if object := info.Defs[identifier]; object != nil {
								target.witnessObjects[object] = labels[identifier]
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func checkInactiveIntegrityVariant(fset *token.FileSet, key string, candidates []*integrityFile, packages variantIntegrityPackageIndex, module string, forced forcedIntegrityContext, work *int) (*types.Info, []*integrityFile, bool, error) {
	if err := spendWitnessWork(work, len(candidates)); err != nil {
		return nil, nil, false, err
	}
	selected := make([]*integrityFile, 0, len(candidates))
	for _, candidate := range candidates {
		variant := integrityVariantKey(candidate)
		if variant == "\x00" || variant == forced.key || integrityBuildMatchesForced(candidate.path, candidate.build, forced) {
			selected = append(selected, candidate)
		}
	}
	sort.Slice(selected, func(left, right int) bool { return selected[left].path < selected[right].path })
	parsed := make([]*ast.File, 0, len(selected))
	for _, file := range selected {
		parsed = append(parsed, file.parsed)
	}
	info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
	typeErrors := make([]types.Error, 0)
	variantImports := newVariantIntegrityImporter(packages, module, fset, forced, work)
	configuration := types.Config{Importer: variantImports, GoVersion: "go1.25", DisableUnusedImportCheck: true, Error: func(err error) {
		if typed, ok := err.(types.Error); ok {
			typeErrors = append(typeErrors, typed)
		}
	}}
	_, _ = configuration.Check(key, fset, parsed, info)
	if variantImports.err != nil {
		return nil, nil, false, variantImports.err
	}
	originalInfo := make(map[*integrityFile]*types.Info, len(selected))
	for _, file := range selected {
		originalInfo[file] = file.info
		file.info = info
	}
	defer func() {
		for _, file := range selected {
			file.info = originalInfo[file]
		}
	}()
	for _, typeError := range typeErrors {
		opaque := false
		for _, file := range selected {
			if opaqueSelectorTypeError(file, typeError, module) {
				opaque = true
				break
			}
		}
		if !opaque {
			return info, selected, false, nil
		}
	}
	return info, selected, true, nil
}

func integrityVariantKey(file *integrityFile) string {
	return buildNameConstraint(file.path) + "\x00" + file.build
}

func integritySnapshotImporter(files map[string]*integrityFile, module string, fset *token.FileSet) types.Importer {
	result := &integrityImporter{
		module: module, standard: importer.Default(), fset: fset, files: files,
		localDirectories: make(map[string]string), packages: make(map[string]*types.Package), invalid: make(map[string]bool), visiting: make(map[string]bool),
	}
	if module == "" {
		return result
	}
	for _, filePath := range sortedIntegrityFiles(files) {
		file := files[filePath]
		if strings.HasSuffix(file.path, "_test.go") || !file.eligible || !file.active {
			continue
		}
		importPath := localImportPath(module, path.Dir(file.path))
		result.localDirectories[importPath] = path.Dir(file.path)
	}
	return result
}

func opaqueSelectorTypeError(file *integrityFile, typeError types.Error, module string) bool {
	position := file.fset.Position(typeError.Pos)
	if position.Filename != file.path {
		return false
	}
	opaque := false
	ast.Inspect(file.parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok || file.fset.Position(selector.Sel.Pos()).Offset != position.Offset {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		pkgName, imported := file.info.Uses[qualifier].(*types.PkgName)
		if ok && imported && pkgName.Imported().Path() != "testing" &&
			(module == "" || (pkgName.Imported().Path() != module && !strings.HasPrefix(pkgName.Imported().Path(), module+"/"))) {
			opaque = true
		}
		return false
	})
	return opaque
}

func validateNewTests(before, after map[string]*integrityFile, packageRenames packageWitnesses) error {
	baseline := make(map[string]int)
	for _, filePath := range sortedIntegrityFiles(before) {
		file := before[filePath]
		if !eligibleGoTestPath(file) {
			continue
		}
		directory := path.Dir(filePath)
		if renamed, ok := packageRenames.directories[directory]; ok {
			directory = renamed
		}
		for _, declaration := range file.parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && testLikeName(function.Name.Name) {
				baseline[directory+"\x00"+function.Name.Name]++
			}
		}
	}
	for _, filePath := range sortedIntegrityFiles(after) {
		file := after[filePath]
		if !eligibleGoTestPath(file) {
			continue
		}
		for _, declaration := range file.parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !testLikeName(function.Name.Name) {
				continue
			}
			key := path.Dir(filePath) + "\x00" + function.Name.Name
			if baseline[key] > 0 {
				baseline[key]--
				continue
			}
			if _, valid := typedDiscoveryKind(function, file); !valid || !file.typeValid {
				return errors.New("invalid new test")
			}
		}
	}
	return nil
}

func testLikeName(name string) bool {
	if name == "TestMain" {
		return false
	}
	return discoveryName(name, "Test") || discoveryName(name, "Benchmark") || discoveryName(name, "Fuzz") || exampleName(name)
}

func typedDiscoveryKind(function *ast.FuncDecl, file *integrityFile) (string, bool) {
	kind, syntaxValid := discoveryKind(function)
	if !syntaxValid || file.info == nil {
		return "", false
	}
	object, ok := file.info.Defs[function.Name].(*types.Func)
	if !ok {
		return "", false
	}
	signature, ok := object.Type().(*types.Signature)
	if !ok || signature.TypeParams().Len() != 0 || signature.Results().Len() != 0 {
		return "", false
	}
	if kind == "example" {
		return kind, signature.Params().Len() == 0
	}
	if signature.Params().Len() != 1 {
		return "", false
	}
	pointer, ok := signature.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return "", false
	}
	named, ok := pointer.Elem().(*types.Named)
	return kind, ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "testing" &&
		named.Obj().Name() == map[string]string{"test": "T", "benchmark": "B", "fuzz": "F"}[kind]
}

type integrityImporter struct {
	module           string
	standard         types.Importer
	fset             *token.FileSet
	files            map[string]*integrityFile
	localDirectories map[string]string
	packages         map[string]*types.Package
	invalid          map[string]bool
	visiting         map[string]bool
}

type forcedIntegrityContext struct {
	key    string
	goos   string
	goarch string
	tags   map[string]bool
}

type variantIntegrityImporter struct {
	module     string
	standard   types.Importer
	fset       *token.FileSet
	forced     forcedIntegrityContext
	work       *int
	candidates map[string][]*integrityFile
	packages   map[string]*types.Package
	invalid    map[string]bool
	visiting   map[string]bool
	err        error
}

type variantIntegrityPackageIndex map[string][]*integrityFile

func (value *integrityImporter) Import(importPath string) (*types.Package, error) {
	if existing := value.packages[importPath]; existing != nil {
		if value.invalid[importPath] {
			return existing, errors.New("invalid local package")
		}
		return existing, nil
	}
	if directory, local := value.localDirectories[importPath]; local {
		if value.visiting[importPath] {
			return nil, errors.New("local import cycle")
		}
		value.visiting[importPath] = true
		defer delete(value.visiting, importPath)
		parsed := make([]*ast.File, 0)
		for _, filePath := range sortedIntegrityFiles(value.files) {
			file := value.files[filePath]
			if path.Dir(filePath) == directory && !strings.HasSuffix(filePath, "_test.go") && file.eligible && file.active {
				parsed = append(parsed, file.parsed)
			}
		}
		if len(parsed) == 0 || value.fset == nil {
			return nil, errors.New("invalid local package")
		}
		info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
		typeErrors := make([]types.Error, 0)
		configuration := types.Config{Importer: value, GoVersion: "go1.25", DisableUnusedImportCheck: true, Error: func(err error) {
			if typed, ok := err.(types.Error); ok {
				typeErrors = append(typeErrors, typed)
			}
		}}
		pkg, _ := configuration.Check(importPath, value.fset, parsed, info)
		if pkg == nil {
			return nil, errors.New("invalid local package")
		}
		value.packages[importPath] = pkg
		for _, filePath := range sortedIntegrityFiles(value.files) {
			file := value.files[filePath]
			if path.Dir(filePath) == directory && !strings.HasSuffix(filePath, "_test.go") && file.eligible && file.active {
				file.info = info
			}
		}
		for _, typeError := range typeErrors {
			opaque := false
			for _, filePath := range sortedIntegrityFiles(value.files) {
				file := value.files[filePath]
				if path.Dir(filePath) == directory && file.active && opaqueSelectorTypeError(file, typeError, value.module) {
					opaque = true
					break
				}
			}
			if !opaque {
				value.invalid[importPath] = true
				return pkg, errors.New("invalid local package")
			}
		}
		return pkg, nil
	}
	first, _, _ := strings.Cut(importPath, "/")
	if !strings.Contains(first, ".") {
		if standard, err := value.standard.Import(importPath); err == nil {
			return standard, nil
		}
	}
	pkg := types.NewPackage(importPath, opaquePackageName(importPath))
	pkg.MarkComplete()
	value.packages[importPath] = pkg
	return pkg, nil
}

func indexVariantIntegrityPackages(files map[string]*integrityFile, module string) variantIntegrityPackageIndex {
	result := make(variantIntegrityPackageIndex)
	if module == "" {
		return result
	}
	for _, filePath := range sortedIntegrityFiles(files) {
		file := files[filePath]
		if strings.HasSuffix(file.path, "_test.go") || !file.eligible {
			continue
		}
		importPath := localImportPath(module, path.Dir(file.path))
		result[importPath] = append(result[importPath], file)
	}
	return result
}

func newVariantIntegrityImporter(packages variantIntegrityPackageIndex, module string, fset *token.FileSet, forced forcedIntegrityContext, work *int) *variantIntegrityImporter {
	result := &variantIntegrityImporter{
		module: module, standard: importer.Default(), fset: fset, forced: forced, work: work, candidates: packages,
		packages: make(map[string]*types.Package), invalid: make(map[string]bool), visiting: make(map[string]bool),
	}
	return result
}

func (value *variantIntegrityImporter) Import(importPath string) (*types.Package, error) {
	if existing := value.packages[importPath]; existing != nil {
		if value.invalid[importPath] {
			return existing, errors.New("invalid local package")
		}
		return existing, nil
	}
	candidates, local := value.candidates[importPath]
	withinModule := value.module != "" && (importPath == value.module || strings.HasPrefix(importPath, value.module+"/"))
	if local {
		if value.visiting[importPath] {
			return nil, errors.New("local import cycle")
		}
		value.visiting[importPath] = true
		defer delete(value.visiting, importPath)
		if err := spendWitnessWork(value.work, len(candidates)); err != nil {
			value.err = err
			return nil, err
		}
		selected := make([]*integrityFile, 0, len(candidates))
		for _, file := range candidates {
			variant := integrityVariantKey(file)
			if variant == "\x00" || variant == value.forced.key || integrityBuildMatchesForced(file.path, file.build, value.forced) {
				selected = append(selected, file)
			}
		}
		if len(selected) == 0 || value.fset == nil {
			return nil, errors.New("invalid local package")
		}
		parsed := make([]*ast.File, 0, len(selected))
		info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
		for _, file := range selected {
			parsed = append(parsed, file.parsed)
		}
		typeErrors := make([]types.Error, 0)
		configuration := types.Config{Importer: value, GoVersion: "go1.25", DisableUnusedImportCheck: true, Error: func(err error) {
			if typed, ok := err.(types.Error); ok {
				typeErrors = append(typeErrors, typed)
			}
		}}
		pkg, _ := configuration.Check(importPath, value.fset, parsed, info)
		if pkg == nil {
			return nil, errors.New("invalid local package")
		}
		value.packages[importPath] = pkg
		originalInfo := make(map[*integrityFile]*types.Info, len(selected))
		for _, file := range selected {
			originalInfo[file] = file.info
			file.info = info
		}
		defer func() {
			for _, file := range selected {
				file.info = originalInfo[file]
			}
		}()
		for _, typeError := range typeErrors {
			opaque := false
			for _, file := range selected {
				if opaqueSelectorTypeError(file, typeError, value.module) {
					opaque = true
					break
				}
			}
			if !opaque {
				value.invalid[importPath] = true
				return pkg, errors.New("invalid local package")
			}
		}
		return pkg, nil
	}
	if withinModule {
		return nil, errors.New("invalid local package")
	}
	first, _, _ := strings.Cut(importPath, "/")
	if !strings.Contains(first, ".") {
		if standard, err := value.standard.Import(importPath); err == nil {
			return standard, nil
		}
	}
	pkg := types.NewPackage(importPath, opaquePackageName(importPath))
	pkg.MarkComplete()
	value.packages[importPath] = pkg
	return pkg, nil
}

func discoverTests(files map[string]*integrityFile) map[string][]testDeclaration {
	result := make(map[string][]testDeclaration)
	paths := sortedIntegrityFiles(files)
	for _, file := range paths {
		entry := files[file]
		if !eligibleGoTestPath(entry) {
			continue
		}
		for _, decl := range entry.parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Recv != nil {
				continue
			}
			kind, valid := discoveryKind(function)
			if !valid {
				continue
			}
			start := function.Pos()
			if function.Doc != nil {
				start = function.Doc.Pos()
			}
			tokens, _ := scanIntegrityTokens(file, entry.source, &tokenRange{start: start, end: function.End(), fset: entry.fset})
			key := path.Dir(file) + "\x00" + kind + "\x00" + function.Name.Name
			result[key] = append(result[key], testDeclaration{
				key: key, file: entry, decl: function, kind: kind, name: function.Name.Name,
				line: entry.fset.Position(function.Pos()).Line, tokens: tokens, imports: entry.imports,
			})
		}
	}
	return result
}

func testBehaviorDeclarations(files map[string]*integrityFile) map[string][]behaviorDeclaration {
	result := make(map[string][]behaviorDeclaration)
	for _, file := range sortedIntegrityFiles(files) {
		entry := files[file]
		if !eligibleGoTestPath(entry) {
			continue
		}
		for _, declaration := range entry.parsed.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok && general.Tok == token.IMPORT {
				continue
			}
			name := testDeclarationIdentity(declaration, entry)
			if name == "" {
				continue
			}
			start := declaration.Pos()
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Doc != nil {
				start = function.Doc.Pos()
			}
			tokens, _ := scanIntegrityTokens(file, entry.source, &tokenRange{start: start, end: declaration.End(), fset: entry.fset})
			key := path.Dir(file) + "\x00" + name
			_, discoverable := discoveryKindForDeclaration(declaration)
			result[key] = append(result[key], behaviorDeclaration{
				key: key, file: entry, line: entry.fset.Position(declaration.Pos()).Line, name: name, tokens: tokens, newTest: discoverable, node: declaration,
			})
		}
	}
	return result
}

func eligibleGoTestPath(file *integrityFile) bool {
	return strings.HasSuffix(file.path, "_test.go") && file.eligible && file.active
}

func integrityBuildActive(file, directives string) bool {
	return integrityBuildMatches(file, directives, build.Default.GOOS, build.Default.GOARCH)
}

func integrityBuildMatches(file, directives, goos, goarch string) bool {
	return integrityBuildMatchesTags(file, directives, goos, goarch, nil)
}

func integrityBuildMatchesForced(file, directives string, forced forcedIntegrityContext) bool {
	return integrityBuildMatchesTags(file, directives, forced.goos, forced.goarch, forced.tags)
}

func integrityBuildMatchesTags(file, directives, goos, goarch string, tags map[string]bool) bool {
	nameConstraint := buildNameConstraint(file)
	switch {
	case strings.HasSuffix(nameConstraint, "/*") && !strings.HasPrefix(nameConstraint, goos+"/"):
		return false
	case strings.HasPrefix(nameConstraint, "*/") && !strings.HasSuffix(nameConstraint, "/"+goarch):
		return false
	case strings.Contains(nameConstraint, "/") && !strings.HasSuffix(nameConstraint, "/*") && !strings.HasPrefix(nameConstraint, "*/") &&
		nameConstraint != goos+"/"+goarch:
		return false
	}
	tagMatches := func(tag string) bool {
		if enabled, ok := tags[tag]; ok {
			return enabled
		}
		return integrityBuildTagFor(goos, goarch, tag)
	}
	lines := strings.Split(directives, "\n")
	hasGoBuild := false
	for _, line := range lines {
		if strings.HasPrefix(line, "//go:build") {
			hasGoBuild = true
			expression, err := constraint.Parse(line)
			if err != nil || !expression.Eval(tagMatches) {
				return false
			}
		}
	}
	if hasGoBuild {
		return true
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "// +build") {
			expression, err := constraint.Parse(line)
			if err != nil || !expression.Eval(tagMatches) {
				return false
			}
		}
	}
	return true
}

func integrityBuildTag(tag string) bool {
	return integrityBuildTagFor(build.Default.GOOS, build.Default.GOARCH, tag)
}

func integrityBuildTagFor(goos, goarch, tag string) bool {
	if tag == goos || tag == goarch || tag == build.Default.Compiler || tag == "unix" && integrityUnixOS(goos) {
		return true
	}
	if tag == "cgo" {
		return build.Default.CgoEnabled
	}
	for _, tags := range [][]string{build.Default.BuildTags, build.Default.ToolTags, build.Default.ReleaseTags} {
		for _, configured := range tags {
			if tag == configured {
				return true
			}
		}
	}
	return false
}

func forcedIntegrityVariants(file *integrityFile, packages variantIntegrityPackageIndex) ([]forcedIntegrityContext, error) {
	osTags := make(map[string]bool)
	archTags := make(map[string]bool)
	customTags := make(map[string]bool)
	knownOS := make(map[string]bool)
	knownArch := make(map[string]bool)
	for _, value := range strings.Fields(suppressionKnownOS) {
		knownOS[value] = true
	}
	for _, value := range strings.Fields(suppressionKnownArch) {
		knownArch[value] = true
	}
	collect := func(tag string) {
		if tag == "" || tag == "*" {
			return
		}
		if knownOS[tag] {
			osTags[tag] = true
		} else if knownArch[tag] {
			archTags[tag] = true
		} else if tag == "cgo" || !integrityFixedBuildTag(tag) {
			customTags[tag] = true
		}
	}
	collectFile := func(candidate *integrityFile) {
		left, right, paired := strings.Cut(buildNameConstraint(candidate.path), "/")
		if paired {
			collect(left)
			collect(right)
		}
		for _, line := range strings.Split(candidate.build, "\n") {
			if line == "" {
				continue
			}
			expression, err := constraint.Parse(line)
			if err == nil {
				collectIntegrityConstraintTags(expression, collect)
			}
		}
	}
	collectFile(file)
	for _, candidates := range packages {
		for _, candidate := range candidates {
			collectFile(candidate)
		}
	}
	custom := make([]string, 0, len(customTags))
	for tag := range customTags {
		custom = append(custom, tag)
	}
	sort.Strings(custom)
	if len(custom) > 5 {
		return nil, errIntegrityResourceLimit
	}
	operatingSystems := integrityBuildCandidates(build.Default.GOOS, suppressionKnownOS, osTags)
	architectures := integrityBuildCandidates(build.Default.GOARCH, suppressionKnownArch, archTags)
	result := make([]forcedIntegrityContext, 0)
	for _, goos := range operatingSystems {
		for _, goarch := range architectures {
			for assignment := 0; assignment < 1<<len(custom); assignment++ {
				tags := make(map[string]bool, len(custom))
				for index, tag := range custom {
					tags[tag] = assignment&(1<<index) != 0
				}
				forced := forcedIntegrityContext{key: integrityVariantKey(file), goos: goos, goarch: goarch, tags: tags}
				if !integrityBuildMatchesForced(file.path, file.build, forced) {
					continue
				}
				result = append(result, forced)
				if len(result) > maxSuppressionBuildVariants {
					return nil, errIntegrityResourceLimit
				}
			}
		}
	}
	if len(result) == 0 {
		result = append(result, forcedIntegrityContext{key: integrityVariantKey(file), goos: build.Default.GOOS, goarch: build.Default.GOARCH})
	}
	return result, nil
}

func integrityFixedBuildTag(tag string) bool {
	if tag == build.Default.Compiler || tag == "unix" {
		return true
	}
	for _, tags := range [][]string{build.Default.BuildTags, build.Default.ToolTags, build.Default.ReleaseTags} {
		for _, configured := range tags {
			if tag == configured {
				return true
			}
		}
	}
	return false
}

func collectIntegrityConstraintTags(expression constraint.Expr, collect func(string)) {
	switch value := expression.(type) {
	case *constraint.TagExpr:
		collect(value.Tag)
	case *constraint.NotExpr:
		collectIntegrityConstraintTags(value.X, collect)
	case *constraint.AndExpr:
		collectIntegrityConstraintTags(value.X, collect)
		collectIntegrityConstraintTags(value.Y, collect)
	case *constraint.OrExpr:
		collectIntegrityConstraintTags(value.X, collect)
		collectIntegrityConstraintTags(value.Y, collect)
	}
}

func integrityBuildCandidates(current, known string, referenced map[string]bool) []string {
	result := []string{current}
	seen := map[string]bool{current: true}
	for _, value := range strings.Fields(known) {
		if !seen[value] && (referenced[value] || len(result) == 1) {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result
}

func integrityUnixOS(goos string) bool {
	switch goos {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
		return true
	default:
		return false
	}
}

func eligibleGoSourcePath(file string) bool {
	components := strings.Split(file, "/")
	for index, component := range components {
		if component == "" || component == "." || strings.HasPrefix(component, ".") || strings.HasPrefix(component, "_") {
			return false
		}
		if index < len(components)-1 && component == "vendor" {
			return false
		}
	}
	return true
}

func eligibleSnapshotGoPath(file string, snapshot TreeSnapshot) bool {
	if !eligibleGoSourcePath(file) {
		return false
	}
	for directory := path.Dir(file); directory != "."; directory = path.Dir(directory) {
		if _, nestedModule := snapshot.Files[path.Join(directory, "go.mod")]; nestedModule {
			return false
		}
	}
	return true
}

func discoveryKindForDeclaration(declaration ast.Decl) (string, bool) {
	function, ok := declaration.(*ast.FuncDecl)
	if !ok || function.Recv != nil {
		return "", false
	}
	return discoveryKind(function)
}

func testDeclarationIdentity(declaration ast.Decl, file *integrityFile) string {
	switch value := declaration.(type) {
	case *ast.FuncDecl:
		receiver := ""
		if value.Recv != nil {
			tokens, _ := scanIntegrityTokens(file.path, file.source, &tokenRange{start: value.Recv.Pos(), end: value.Recv.End(), fset: file.fset})
			receiver = tokensString(tokens)
		}
		return "func\x00" + value.Name.Name + "\x00" + receiver
	case *ast.GenDecl:
		names := make([]string, 0)
		for _, spec := range value.Specs {
			switch item := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, item.Name.Name)
			case *ast.ValueSpec:
				for _, name := range item.Names {
					names = append(names, name.Name)
				}
			}
		}
		if len(names) > 0 {
			return value.Tok.String() + "\x00" + strings.Join(names, "\x00")
		}
	}
	return ""
}

func discoveryKind(function *ast.FuncDecl) (string, bool) {
	if function.Type.TypeParams != nil && len(function.Type.TypeParams.List) > 0 {
		return "", false
	}
	name := function.Name.Name
	switch {
	case discoveryName(name, "Test") && testingParameter(function.Type, "T"):
		return "test", true
	case discoveryName(name, "Benchmark") && testingParameter(function.Type, "B"):
		return "benchmark", true
	case discoveryName(name, "Fuzz") && testingParameter(function.Type, "F"):
		return "fuzz", true
	case strings.HasPrefix(name, "Example") && exampleName(name) && emptySignature(function.Type):
		return "example", true
	default:
		return "", false
	}
}

func discoveryName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

func exampleName(name string) bool {
	return discoveryName(name, "Example")
}

func testingParameter(function *ast.FuncType, testingType string) bool {
	if !emptyResults(function) || function.Params == nil || len(function.Params.List) != 1 {
		return false
	}
	field := function.Params.List[0]
	if len(field.Names) > 1 {
		return false
	}
	pointer, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if identifier, ok := pointer.X.(*ast.Ident); ok {
		return identifier.Name == testingType
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == testingType
}

func emptySignature(function *ast.FuncType) bool {
	return (function.Params == nil || len(function.Params.List) == 0) && emptyResults(function)
}

func emptyResults(function *ast.FuncType) bool {
	return function.Results == nil || len(function.Results.List) == 0
}

func sameTestEnvironment(before, after testDeclaration, packageRenames packageWitnesses) bool {
	if before.file.build != after.file.build || buildNameConstraint(before.file.path) != buildNameConstraint(after.file.path) {
		return false
	}
	wantPackage := remapPackageInDirectory(before.file.packageName, path.Dir(before.file.path), packageRenames)
	return wantPackage == after.file.packageName
}

func sameBehaviorEnvironment(before, after behaviorDeclaration, packageRenames packageWitnesses) bool {
	if before.file.build != after.file.build || buildNameConstraint(before.file.path) != buildNameConstraint(after.file.path) {
		return false
	}
	return remapPackageInDirectory(before.file.packageName, path.Dir(before.file.path), packageRenames) == after.file.packageName
}

func remapPackageInDirectory(name, directory string, witnesses packageWitnesses) string {
	witness, exists := witnesses.packages[directory]
	if !exists {
		return name
	}
	suffix := ""
	base := name
	if strings.HasSuffix(name, "_test") {
		base = strings.TrimSuffix(name, "_test")
		suffix = "_test"
	}
	if base == witness.oldName {
		return witness.newName + suffix
	}
	return name
}

func integrityImports(file *ast.File) []integrityImport {
	result := make([]integrityImport, 0, len(file.Imports))
	for _, imported := range file.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			importPath = imported.Path.Value
		}
		alias := ""
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		result = append(result, integrityImport{alias: alias, path: importPath})
	}
	return result
}

func integrityComments(file *ast.File) []string {
	result := make([]string, 0)
	for _, group := range file.Comments {
		for _, comment := range group.List {
			result = append(result, comment.Text)
		}
	}
	return result
}

func commentsPreserved(before, after []string) bool {
	counts := make(map[string]int, len(after))
	for _, comment := range after {
		counts[comment]++
	}
	for _, comment := range before {
		if counts[comment] == 0 {
			return false
		}
		counts[comment]--
	}
	return true
}

func importsPreserved(before, after []integrityImport, packageRenames packageWitnesses) bool {
	remaining := append([]integrityImport(nil), after...)
	for _, original := range before {
		directory := localImportDirectory(original.path, packageRenames)
		expectedPath := remapImportPath(original.path, packageRenames)
		matched := -1
		for index, candidate := range remaining {
			if candidate.path != expectedPath {
				continue
			}
			witnessed := directory != "" && packageRenames.packages[directory].newDirectory != ""
			if witnessed {
				if importAliasKind(original.alias) != importAliasKind(candidate.alias) {
					continue
				}
			} else if candidate.alias != original.alias {
				continue
			}
			matched = index
			break
		}
		if matched < 0 {
			return false
		}
		remaining = append(remaining[:matched], remaining[matched+1:]...)
	}
	for _, addition := range remaining {
		if addition.alias == "_" || addition.alias == "." {
			return false
		}
	}
	return true
}

func importAliasKind(alias string) string {
	if alias == "_" || alias == "." {
		return alias
	}
	return "named"
}

func behaviorFinding(declaration behaviorDeclaration) finding.Finding {
	return integrityFinding("togi/test-behavior", declaration.file.path, declaration.line, declaration.name,
		"existing test behavior changed during a fix attempt")
}

func remapImportPath(importPath string, witnesses packageWitnesses) string {
	directory := localImportDirectory(importPath, witnesses)
	if directory == "" {
		return importPath
	}
	newDirectory := directory
	if renamed, ok := witnesses.directories[directory]; ok {
		newDirectory = renamed
	}
	return localImportPath(witnesses.module, newDirectory)
}

func localImportDirectory(importPath string, witnesses packageWitnesses) string {
	return witnesses.imports[importPath]
}

func localImportPath(module, directory string) string {
	if directory == "." {
		return module
	}
	return strings.TrimSuffix(module, "/") + "/" + directory
}

func buildNameConstraint(file string) string {
	name := strings.TrimSuffix(path.Base(file), ".go")
	name = strings.TrimSuffix(name, "_test")
	parts := strings.Split(name, "_")
	if len(parts) < 2 {
		return ""
	}
	knownOS := make(map[string]bool)
	knownArch := make(map[string]bool)
	for _, value := range strings.Fields(suppressionKnownOS) {
		knownOS[value] = true
	}
	for _, value := range strings.Fields(suppressionKnownArch) {
		knownArch[value] = true
	}
	last := parts[len(parts)-1]
	if knownArch[last] && len(parts) >= 3 && knownOS[parts[len(parts)-2]] {
		return parts[len(parts)-2] + "/" + last
	}
	if knownOS[last] {
		return last + "/*"
	}
	if knownArch[last] {
		return "*/" + last
	}
	return ""
}

func testFinding(rule string, declaration testDeclaration, message string) finding.Finding {
	snippet := declaration.kind + " " + declaration.name
	return integrityFinding(rule, declaration.file.path, declaration.line, snippet, message)
}

func witnessedPackageRenames(before, after map[string]*integrityFile, beforeModule, afterModule string, work *int) (packageWitnesses, error) {
	type shapeGroup struct {
		before []string
		after  []string
	}
	groups := make(map[string]*shapeGroup)
	beforePackages := make(map[string][]string)
	afterPackages := make(map[string][]string)
	beforeDirectories := make(map[string]int)
	afterDirectories := make(map[string]int)
	beforePlaceholders := packageDeclarationPlaceholders(before)
	afterPlaceholders := packageDeclarationPlaceholders(after)
	for _, side := range []struct {
		files        map[string]*integrityFile
		placeholders map[types.Object]string
		before       bool
	}{{before, beforePlaceholders, true}, {after, afterPlaceholders, false}} {
		for _, filePath := range sortedIntegrityFiles(side.files) {
			file := side.files[filePath]
			if strings.HasSuffix(filePath, "_test.go") || !file.eligible {
				continue
			}
			if err := spendWitnessWork(work, 1); err != nil {
				return packageWitnesses{}, err
			}
			key := path.Dir(filePath) + "\x00" + file.packageName
			if side.before {
				beforeDirectories[path.Dir(filePath)]++
			} else {
				afterDirectories[path.Dir(filePath)]++
			}
			if !file.typeValid {
				continue
			}
			tokens, err := scanIntegrityTokens(filePath, file.source, nil)
			if err != nil {
				return packageWitnesses{}, err
			}
			shape := tokensString(normalizePackageFile(tokens, file, side.placeholders))
			group := groups[shape]
			if group == nil {
				group = &shapeGroup{}
				groups[shape] = group
			}
			if side.before {
				group.before = append(group.before, key)
				beforePackages[key] = append(beforePackages[key], shape)
			} else {
				group.after = append(group.after, key)
				afterPackages[key] = append(afterPackages[key], shape)
			}
		}
	}
	forward := make(map[string]map[string]struct{})
	reverse := make(map[string]map[string]struct{})
	for _, group := range groups {
		oldOnly, newOnly := unmatchedStrings(group.before, group.after)
		if len(oldOnly) != 1 || len(newOnly) != 1 {
			continue
		}
		oldKey := oldOnly[0]
		newKey := newOnly[0]
		addRenameCandidate(forward, oldKey, newKey)
		addRenameCandidate(reverse, newKey, oldKey)
	}
	result := packageWitnesses{names: make(map[string]string), directories: make(map[string]string), packages: make(map[string]packageWitness), imports: make(map[string]string)}
	if beforeModule != "" && beforeModule == afterModule {
		result.module = beforeModule
		for _, file := range before {
			if file.eligible && !strings.HasSuffix(file.path, "_test.go") {
				directory := path.Dir(file.path)
				result.imports[localImportPath(result.module, directory)] = directory
			}
		}
	}
	nameForward := make(map[string]map[string]struct{})
	nameReverse := make(map[string]map[string]struct{})
	directoryForward := make(map[string]map[string]struct{})
	directoryReverse := make(map[string]map[string]struct{})
	packageCandidates := make(map[string]map[string]struct{})
	for oldKey, candidates := range forward {
		if len(candidates) != 1 {
			continue
		}
		for newKey := range candidates {
			if len(reverse[newKey]) == 1 {
				oldDirectory, oldName, _ := strings.Cut(oldKey, "\x00")
				newDirectory, newName, _ := strings.Cut(newKey, "\x00")
				if !completePackageWitness(oldKey, newKey, oldDirectory, newDirectory, beforePackages, afterPackages, beforeDirectories, afterDirectories) {
					continue
				}
				if oldName != newName {
					addRenameCandidate(nameForward, oldName, newName)
					addRenameCandidate(nameReverse, newName, oldName)
				}
				if oldDirectory != newDirectory {
					addRenameCandidate(directoryForward, oldDirectory, newDirectory)
					addRenameCandidate(directoryReverse, newDirectory, oldDirectory)
				}
				addRenameCandidate(packageCandidates, oldDirectory, oldName+"\x00"+newDirectory+"\x00"+newName)
			}
		}
	}
	result.names = uniqueRenames(nameForward, nameReverse)
	result.directories = uniqueRenames(directoryForward, directoryReverse)
	for oldDirectory, candidates := range packageCandidates {
		if len(candidates) != 1 {
			continue
		}
		encoded := onlySetValue(candidates)
		oldName, remainder, _ := strings.Cut(encoded, "\x00")
		newDirectory, newName, _ := strings.Cut(remainder, "\x00")
		result.packages[oldDirectory] = packageWitness{oldName: oldName, newName: newName, newDirectory: newDirectory}
	}
	return result, nil
}

func completePackageWitness(oldKey, newKey, oldDirectory, newDirectory string, before, after map[string][]string, beforeDirectories, afterDirectories map[string]int) bool {
	if len(after[oldKey]) != 0 || len(before[newKey]) != 0 || beforeDirectories[oldDirectory] != len(before[oldKey]) || afterDirectories[newDirectory] != len(after[newKey]) {
		return false
	}
	oldShapes := append([]string(nil), before[oldKey]...)
	newShapes := append([]string(nil), after[newKey]...)
	sort.Strings(oldShapes)
	sort.Strings(newShapes)
	if len(oldShapes) == 0 || len(oldShapes) != len(newShapes) {
		return false
	}
	for index := range oldShapes {
		if oldShapes[index] != newShapes[index] {
			return false
		}
	}
	return true
}

func spendWitnessWork(remaining *int, amount int) error {
	if remaining == nil || amount < 0 || *remaining < amount {
		return errIntegrityResourceLimit
	}
	*remaining -= amount
	return nil
}

func unmatchedStrings(before, after []string) ([]string, []string) {
	byValue := make(map[string][]int, len(after))
	for index, candidate := range after {
		byValue[candidate] = append(byValue[candidate], index)
	}
	usedByValue := make(map[string]int, len(byValue))
	used := make([]bool, len(after))
	oldOnly := make([]string, 0)
	for _, baseline := range before {
		indexes := byValue[baseline]
		position := usedByValue[baseline]
		if position < len(indexes) {
			used[indexes[position]] = true
			usedByValue[baseline]++
		} else {
			oldOnly = append(oldOnly, baseline)
		}
	}
	newOnly := make([]string, 0)
	for index, candidate := range after {
		if !used[index] {
			newOnly = append(newOnly, candidate)
		}
	}
	return oldOnly, newOnly
}

func onlySetValue(values map[string]struct{}) string {
	for value := range values {
		return value
	}
	return ""
}

func uniqueRenames(forward, reverse map[string]map[string]struct{}) map[string]string {
	result := make(map[string]string)
	for oldName, candidates := range forward {
		if len(candidates) != 1 {
			continue
		}
		for newName := range candidates {
			if len(reverse[newName]) == 1 {
				result[oldName] = newName
			}
		}
	}
	return result
}

func witnessedIdentifierRenames(before, after map[string]*integrityFile, packageRenames packageWitnesses, work *int) (identifierWitnesses, error) {
	oldDecls, err := productionDeclarations(before, packageRenames, true, work)
	if err != nil {
		return identifierWitnesses{}, err
	}
	newDecls, err := productionDeclarations(after, packageWitnesses{}, false, work)
	if err != nil {
		return identifierWitnesses{}, err
	}
	type shapeGroup struct{ before, after []productionDeclaration }
	groups := make(map[string]*shapeGroup)
	beforeCounts := make(map[string]map[string]int)
	afterCounts := make(map[string]map[string]int)
	for _, side := range []struct {
		declarations []productionDeclaration
		before       bool
	}{{oldDecls, true}, {newDecls, false}} {
		for _, declaration := range side.declarations {
			scope := declaration.directory + "\x00" + declaration.packageName
			counts := afterCounts
			if side.before {
				counts = beforeCounts
			}
			if counts[scope] == nil {
				counts[scope] = make(map[string]int)
			}
			counts[scope][declaration.name]++
			key := scope + "\x00" + declaration.variant + "\x00" + declaration.shape
			group := groups[key]
			if group == nil {
				group = &shapeGroup{}
				groups[key] = group
			}
			if side.before {
				group.before = append(group.before, declaration)
			} else {
				group.after = append(group.after, declaration)
			}
		}
	}
	forward := make(map[string]map[string]map[string]struct{})
	reverse := make(map[string]map[string]map[string]struct{})
	type objectCandidate struct {
		object           types.Object
		oldName, newName string
		scope            string
	}
	objects := make(map[string][]objectCandidate)
	authorized := make(map[string]bool)
	type renameInstances struct {
		old []productionDeclaration
		new []productionDeclaration
	}
	instances := make(map[string]map[string]map[string]*renameInstances)
	for _, group := range groups {
		oldOnly, newOnly := unmatchedDeclarationInstances(group.before, group.after)
		if len(oldOnly) != 1 || len(newOnly) != 1 || oldOnly[0].object == nil {
			continue
		}
		directory := oldOnly[0].directory
		scope := directory + "\x00" + oldOnly[0].packageName
		oldName, newName := oldOnly[0].name, newOnly[0].name
		if instances[scope] == nil {
			instances[scope] = make(map[string]map[string]*renameInstances)
		}
		if instances[scope][oldName] == nil {
			instances[scope][oldName] = make(map[string]*renameInstances)
		}
		pair := instances[scope][oldName][newName]
		if pair == nil {
			pair = &renameInstances{}
			instances[scope][oldName][newName] = pair
		}
		pair.old = append(pair.old, oldOnly[0])
		pair.new = append(pair.new, newOnly[0])
		objects[directory] = append(objects[directory], objectCandidate{object: oldOnly[0].object, oldName: oldName, newName: newName, scope: scope})
	}
	for scope, byOld := range instances {
		directory, _, _ := strings.Cut(scope, "\x00")
		for oldName, byNew := range byOld {
			for newName, pair := range byNew {
				count := len(pair.old)
				if count == 0 || count != beforeCounts[scope][oldName] || afterCounts[scope][oldName] != 0 || count != afterCounts[scope][newName] {
					continue
				}
				if count > 1 {
					exclusive, err := declarationVariantsExclusive(pair.old, work)
					if err != nil {
						return identifierWitnesses{}, err
					}
					if !exclusive {
						continue
					}
				}
				if forward[directory] == nil {
					forward[directory] = make(map[string]map[string]struct{})
					reverse[directory] = make(map[string]map[string]struct{})
				}
				addRenameCandidate(forward[directory], oldName, newName)
				addRenameCandidate(reverse[directory], newName, oldName)
				authorized[scope+"\x00"+oldName+"\x00"+newName] = true
			}
		}
	}
	result := identifierWitnesses{byDirectory: make(map[string]map[string]string), byObject: make(map[types.Object]string)}
	for directory := range forward {
		result.byDirectory[directory] = uniqueRenames(forward[directory], reverse[directory])
		for _, candidate := range objects[directory] {
			if result.byDirectory[directory][candidate.oldName] == candidate.newName && authorized[candidate.scope+"\x00"+candidate.oldName+"\x00"+candidate.newName] {
				result.byObject[candidate.object] = candidate.newName
			}
		}
	}
	return result, nil
}

func declarationVariantsExclusive(declarations []productionDeclaration, work *int) (bool, error) {
	for left := 0; left < len(declarations); left++ {
		for right := left + 1; right < len(declarations); right++ {
			if err := spendWitnessWork(work, 1); err != nil {
				return false, err
			}
			if !buildNamesDefinitelyExclusive(declarations[left].file.path, declarations[right].file.path) {
				return false, nil
			}
		}
	}
	return true, nil
}

func buildNamesDefinitelyExclusive(left, right string) bool {
	leftOS, leftArch, _ := strings.Cut(buildNameConstraint(left), "/")
	rightOS, rightArch, _ := strings.Cut(buildNameConstraint(right), "/")
	if leftOS != "" && leftOS != "*" && rightOS != "" && rightOS != "*" && leftOS != rightOS {
		return true
	}
	return leftArch != "" && leftArch != "*" && rightArch != "" && rightArch != "*" && leftArch != rightArch
}

func unmatchedDeclarationInstances(before, after []productionDeclaration) ([]productionDeclaration, []productionDeclaration) {
	byName := make(map[string][]int, len(after))
	for index, candidate := range after {
		byName[candidate.name] = append(byName[candidate.name], index)
	}
	usedByName := make(map[string]int, len(byName))
	used := make([]bool, len(after))
	oldOnly := make([]productionDeclaration, 0)
	for _, baseline := range before {
		indexes := byName[baseline.name]
		position := usedByName[baseline.name]
		if position < len(indexes) {
			used[indexes[position]] = true
			usedByName[baseline.name]++
		} else {
			oldOnly = append(oldOnly, baseline)
		}
	}
	newOnly := make([]productionDeclaration, 0)
	for index, candidate := range after {
		if !used[index] {
			newOnly = append(newOnly, candidate)
		}
	}
	return oldOnly, newOnly
}

func productionDeclarations(files map[string]*integrityFile, packageRenames packageWitnesses, remap bool, work *int) ([]productionDeclaration, error) {
	result := make([]productionDeclaration, 0)
	for _, file := range sortedIntegrityFiles(files) {
		entry := files[file]
		if strings.HasSuffix(file, "_test.go") || !entry.eligible || !entry.typeValid {
			continue
		}
		packageName := entry.packageName
		directory := path.Dir(file)
		if remap {
			packageName = remapPackageInDirectory(packageName, directory, packageRenames)
			if renamed, ok := packageRenames.directories[directory]; ok {
				directory = renamed
			}
		}
		for _, declaration := range entry.parsed.Decls {
			identifier := declarationIdentifier(declaration)
			if identifier == nil {
				continue
			}
			if err := spendWitnessWork(work, 1); err != nil {
				return nil, err
			}
			tokens, _ := scanIntegrityTokens(file, entry.source, &tokenRange{start: declaration.Pos(), end: declaration.End(), fset: entry.fset})
			var object types.Object
			if entry.info != nil {
				object = entry.info.Defs[identifier]
			}
			shape := tokensString(normalizeBoundDeclaration(tokens, declaration, identifier, object, entry))
			result = append(result, productionDeclaration{
				directory: directory, packageName: packageName, name: identifier.Name, shape: shape, object: object,
				file: entry, variant: integrityVariantKey(entry),
			})
		}
	}
	return result, nil
}

func declarationIdentifier(declaration ast.Decl) *ast.Ident {
	switch value := declaration.(type) {
	case *ast.FuncDecl:
		return value.Name
	case *ast.GenDecl:
		if len(value.Specs) != 1 {
			return nil
		}
		switch spec := value.Specs[0].(type) {
		case *ast.TypeSpec:
			return spec.Name
		case *ast.ValueSpec:
			if len(spec.Names) == 1 {
				return spec.Names[0]
			}
		}
	}
	return nil
}

func normalizeBoundDeclaration(tokens []integrityToken, declaration ast.Decl, identifier *ast.Ident, object types.Object, file *integrityFile) []integrityToken {
	result := append([]integrityToken(nil), tokens...)
	replacements := map[int]bool{file.fset.Position(identifier.Pos()).Offset: true}
	if object != nil {
		ast.Inspect(declaration, func(node ast.Node) bool {
			used, ok := node.(*ast.Ident)
			if ok && file.info.Uses[used] == object {
				replacements[file.fset.Position(used.Pos()).Offset] = true
			}
			return true
		})
	}
	for index := range result {
		if result[index].token == token.IDENT && replacements[result[index].offset] {
			result[index].text = "$declaration"
		}
	}
	return result
}

func normalizePackageClause(tokens []integrityToken) []integrityToken {
	result := append([]integrityToken(nil), tokens...)
	for index := 0; index+1 < len(result); index++ {
		if result[index].token == token.PACKAGE && result[index+1].token == token.IDENT {
			result[index+1].text = "$package"
			break
		}
	}
	return result
}

func packageDeclarationPlaceholders(files map[string]*integrityFile) map[types.Object]string {
	result := make(map[types.Object]string)
	labels := declarationPlaceholderLabels(files)
	for _, filePath := range sortedIntegrityFiles(files) {
		file := files[filePath]
		if strings.HasSuffix(filePath, "_test.go") || !file.eligible {
			continue
		}
		if file.info != nil {
			for _, declaration := range file.parsed.Decls {
				identifier := declarationIdentifier(declaration)
				if identifier == nil {
					continue
				}
				if object := file.info.Defs[identifier]; object != nil {
					result[object] = labels[identifier]
				}
			}
		}
		for object, placeholder := range file.witnessObjects {
			result[object] = placeholder
		}
	}
	return result
}

func declarationPlaceholderLabels(files map[string]*integrityFile) map[*ast.Ident]string {
	result := make(map[*ast.Ident]string)
	ordinals := make(map[string]int)
	for _, filePath := range sortedIntegrityFiles(files) {
		file := files[filePath]
		if strings.HasSuffix(filePath, "_test.go") || !file.eligible {
			continue
		}
		key := path.Dir(filePath) + "\x00" + file.packageName
		for _, declaration := range file.parsed.Decls {
			identifier := declarationIdentifier(declaration)
			if identifier == nil {
				continue
			}
			result[identifier] = fmt.Sprintf("$package-declaration-%d", ordinals[key])
			ordinals[key]++
		}
	}
	return result
}

func normalizePackageFile(tokens []integrityToken, file *integrityFile, placeholders map[types.Object]string) []integrityToken {
	result := normalizePackageClause(tokens)
	if file.info == nil {
		return result
	}
	replacements := make(map[int]string)
	ast.Inspect(file.parsed, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		object := file.info.Defs[identifier]
		if object == nil {
			object = file.info.Uses[identifier]
		}
		if placeholder := placeholders[object]; placeholder != "" {
			replacements[file.fset.Position(identifier.Pos()).Offset] = placeholder
		}
		return true
	})
	for index := range result {
		if replacement := replacements[result[index].offset]; replacement != "" && result[index].token == token.IDENT {
			result[index].text = replacement
		}
	}
	return result
}

func remapBoundTestTokens(declaration, current behaviorDeclaration, identifiers identifierWitnesses, packages packageWitnesses) []integrityToken {
	replacements := make(map[int]string)
	ast.Inspect(declaration.node, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			qualifier, ok := selector.X.(*ast.Ident)
			pkgName, packageUse := declaration.file.info.Uses[qualifier].(*types.PkgName)
			selected := declaration.file.info.Uses[selector.Sel]
			if ok && packageUse && selected != nil && selected.Pkg() != nil && selected.Pkg().Path() == pkgName.Imported().Path() {
				directory := localImportDirectory(pkgName.Imported().Path(), packages)
				if directory != "" {
					tokenIndex := integrityTokenIndex(declaration.tokens, declaration.file.fset.Position(qualifier.Pos()).Offset)
					currentSelector, currentPkgName, currentSelected, currentBound := boundSelectorAtTokenIndex(current, tokenIndex)
					expectedPath := remapImportPath(pkgName.Imported().Path(), packages)
					if !currentBound || currentPkgName.Imported().Path() != expectedPath || currentSelected.Pkg() == nil || currentSelected.Pkg().Path() != expectedPath {
						return true
					}
					witnessDirectory := directory
					witness, packageRenamed := packages.packages[directory]
					if packageRenamed {
						witnessDirectory = witness.newDirectory
					}
					if renamed, exists := identifiers.byDirectory[witnessDirectory][selector.Sel.Name]; exists {
						replacements[declaration.file.fset.Position(selector.Sel.Pos()).Offset] = renamed
					}
					if packageRenamed {
						replacements[declaration.file.fset.Position(qualifier.Pos()).Offset] = currentSelector.X.(*ast.Ident).Name
					}
				}
			}
		}
		identifier, ok := node.(*ast.Ident)
		if ok {
			if renamed, exists := identifiers.byObject[declaration.file.info.Uses[identifier]]; exists {
				replacements[declaration.file.fset.Position(identifier.Pos()).Offset] = renamed
			}
		}
		return true
	})
	result := append([]integrityToken(nil), declaration.tokens...)
	for index := range result {
		if renamed, exists := replacements[result[index].offset]; exists && result[index].token == token.IDENT {
			result[index].text = renamed
		}
	}
	return result
}

func integrityTokenIndex(tokens []integrityToken, offset int) int {
	for index := range tokens {
		if tokens[index].offset == offset {
			return index
		}
	}
	return -1
}

func boundSelectorAtTokenIndex(declaration behaviorDeclaration, tokenIndex int) (*ast.SelectorExpr, *types.PkgName, types.Object, bool) {
	if tokenIndex < 0 || tokenIndex >= len(declaration.tokens) {
		return nil, nil, nil, false
	}
	wantOffset := declaration.tokens[tokenIndex].offset
	var matched *ast.SelectorExpr
	ast.Inspect(declaration.node, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if ok && declaration.file.fset.Position(qualifier.Pos()).Offset == wantOffset {
			matched = selector
			return false
		}
		return true
	})
	if matched == nil {
		return nil, nil, nil, false
	}
	qualifier := matched.X.(*ast.Ident)
	pkgName, ok := declaration.file.info.Uses[qualifier].(*types.PkgName)
	selected := declaration.file.info.Uses[matched.Sel]
	return matched, pkgName, selected, ok && selected != nil
}

func remapPackage(name string, renames map[string]string) string {
	suffix := ""
	base := name
	if strings.HasSuffix(name, "_test") {
		base = strings.TrimSuffix(name, "_test")
		suffix = "_test"
	}
	if renamed, ok := renames[base]; ok {
		return renamed + suffix
	}
	return name
}

func addRenameCandidate(index map[string]map[string]struct{}, from, to string) {
	if index[from] == nil {
		index[from] = make(map[string]struct{})
	}
	index[from][to] = struct{}{}
}

type tokenRange struct {
	start, end token.Pos
	fset       *token.FileSet
}

func scanIntegrityTokens(filename string, source []byte, within *tokenRange) ([]integrityToken, error) {
	start, end := 0, len(source)
	if within != nil {
		start = within.fset.Position(within.start).Offset
		end = within.fset.Position(within.end).Offset
		if start < 0 || end < start || end > len(source) {
			return nil, errIntegrityResourceLimit
		}
	}
	fset := token.NewFileSet()
	file := fset.AddFile(filename, -1, end-start)
	var lexer scanner.Scanner
	var scanErr error
	lexer.Init(file, source[start:end], func(_ token.Position, message string) {
		if scanErr == nil {
			scanErr = errors.New(message)
		}
	}, scanner.ScanComments)
	result := make([]integrityToken, 0)
	for {
		position, tok, literal := lexer.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON {
			continue
		}
		if literal == "" {
			literal = tok.String()
		}
		result = append(result, integrityToken{token: tok, text: literal, offset: start + file.Offset(position)})
		if len(result) > maxIntegrityTokens {
			return nil, errIntegrityResourceLimit
		}
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return result, nil
}

func equalIntegrityTokens(left, right []integrityToken) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].token != right[index].token || left[index].text != right[index].text {
			return false
		}
	}
	return true
}

func tokensString(tokens []integrityToken) string {
	var output strings.Builder
	for _, item := range tokens {
		output.WriteString(item.token.String())
		output.WriteByte(0)
		output.WriteString(item.text)
		output.WriteByte(0)
	}
	return output.String()
}

func integrityBuildDirectives(file *ast.File) string {
	var directives []string
	for _, group := range file.Comments {
		if group.End() > file.Package {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(comment.Text)
			if strings.HasPrefix(text, "//go:build") || strings.HasPrefix(text, "// +build") {
				directives = append(directives, text)
			}
		}
	}
	return strings.Join(directives, "\n")
}

func sortedIntegrityFiles(files map[string]*integrityFile) []string {
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	return paths
}

func publicIntegrityError(err error) error {
	if errors.Is(err, errIntegrityResourceLimit) {
		return errors.New("resource limit exceeded")
	}
	return errors.New("malformed Go syntax")
}

func isTestdataPath(file string) bool {
	for _, component := range strings.Split(file, "/") {
		if component == "testdata" {
			return true
		}
	}
	return false
}

func modulePath(snapshot TreeSnapshot) string {
	contents, exists := snapshot.Files["go.mod"]
	if !exists || len(contents) > maxIntegrityFileBytes {
		return ""
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		value := fields[1]
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		if value != "" && !strings.ContainsAny(value, "\x00\\") {
			return strings.TrimSuffix(value, "/")
		}
		return ""
	}
	return ""
}

func relevantIntegrityPath(file string) bool {
	if path.Base(file) == "go.mod" || path.Ext(file) == ".go" || isTestdataPath(file) {
		return true
	}
	_, protected := protectedGolangCIFiles[path.Base(file)]
	return protected
}

// SnapshotOriginal captures integrity-relevant files from the trusted commit.
func SnapshotOriginal(ctx context.Context, repository, originalHead string) (TreeSnapshot, error) {
	if ctx == nil {
		return TreeSnapshot{}, errors.New("snapshot context is required")
	}
	if !isHexObjectID(originalHead) {
		return TreeSnapshot{}, errors.New("original HEAD must be a hexadecimal object ID")
	}
	listed, err := gitcmd.Output(ctx, repository, gitcmd.Hermetic, maxIntegrityGitListBytes, "ls-tree", "-rz", "--name-only", originalHead)
	if err != nil {
		return TreeSnapshot{}, fmt.Errorf("list original integrity tree: %w", err)
	}
	paths, err := parseNULPaths(listed)
	if err != nil {
		return TreeSnapshot{}, err
	}
	result := TreeSnapshot{Files: make(map[string][]byte)}
	total := 0
	for _, file := range paths {
		if !relevantIntegrityPath(file) {
			continue
		}
		contents, err := gitcmd.Output(ctx, repository, gitcmd.Hermetic, maxIntegrityFileBytes+1, "show", originalHead+":"+file)
		if err != nil {
			return TreeSnapshot{}, fmt.Errorf("read original integrity file %q: %w", file, err)
		}
		if len(contents) > maxIntegrityFileBytes || total > maxIntegritySnapshotBytes-len(contents) {
			return TreeSnapshot{}, errors.New("original integrity snapshot: resource limit exceeded")
		}
		total += len(contents)
		result.Files[file] = contents
	}
	return result, nil
}

// SnapshotAttempt captures integrity-relevant files from the attempted tree.
func SnapshotAttempt(rootPath string) (TreeSnapshot, error) {
	rootInfo, err := os.Lstat(rootPath)
	if err != nil {
		return TreeSnapshot{}, errors.New("inspect attempted tree root: unavailable")
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return TreeSnapshot{}, errors.New("attempted tree root must be a directory, not a symbolic link")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return TreeSnapshot{}, errors.New("open attempted tree: unavailable")
	}
	defer root.Close()
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedInfo) {
		return TreeSnapshot{}, errors.New("attempted tree root changed while being opened")
	}
	before, err := inventoryAttempt(root)
	if err != nil {
		return TreeSnapshot{}, publicAttemptSnapshotError(err)
	}
	if err := runSnapshotAttemptHook("after-inventory", ""); err != nil {
		return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
	}
	result := TreeSnapshot{Files: make(map[string][]byte)}
	total := 0
	for _, clean := range before.relevant {
		entry := before.entries[clean]
		namedBefore, err := root.Lstat(clean)
		if err != nil || !stableAttemptInfo(entry.info, namedBefore) || !namedBefore.Mode().IsRegular() {
			return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
		}
		handle, err := root.Open(clean)
		if err != nil {
			return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
		}
		openedBefore, statErr := handle.Stat()
		if statErr != nil || !stableAttemptInfo(namedBefore, openedBefore) {
			handle.Close()
			return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
		}
		first, readErr := io.ReadAll(io.LimitReader(handle, maxIntegrityFileBytes+1))
		if readErr == nil {
			readErr = runSnapshotAttemptHook("after-first-read", clean)
		}
		if readErr == nil {
			_, readErr = handle.Seek(0, io.SeekStart)
		}
		var second []byte
		if readErr == nil {
			second, readErr = io.ReadAll(io.LimitReader(handle, maxIntegrityFileBytes+1))
		}
		openedAfter, afterErr := handle.Stat()
		closeErr := handle.Close()
		if readErr != nil || afterErr != nil || closeErr != nil || len(first) > maxIntegrityFileBytes || !bytes.Equal(first, second) || !stableAttemptInfo(openedBefore, openedAfter) {
			return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
		}
		namedAfter, err := root.Lstat(clean)
		if err != nil || !stableAttemptInfo(openedAfter, namedAfter) || int64(len(first)) != namedAfter.Size() {
			return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
		}
		if total > maxIntegritySnapshotBytes-len(first) {
			return TreeSnapshot{}, errors.New("attempted integrity snapshot: resource limit exceeded")
		}
		total += len(first)
		result.Files[clean] = first
	}
	if err := runSnapshotAttemptHook("before-post-inventory", ""); err != nil {
		return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
	}
	after, err := inventoryAttempt(root)
	if err != nil || !equalAttemptInventories(before, after) {
		return TreeSnapshot{}, errors.New("attempted integrity snapshot changed while being read")
	}
	finalRoot, err := root.Stat(".")
	if err != nil || !stableAttemptInfo(openedInfo, finalRoot) {
		return TreeSnapshot{}, errors.New("attempted tree root changed while being read")
	}
	return result, nil
}

type attemptInventory struct {
	entries  map[string]attemptInventoryEntry
	relevant []string
}

type attemptInventoryEntry struct {
	info os.FileInfo
}

func inventoryAttempt(root *os.Root) (attemptInventory, error) {
	result := attemptInventory{entries: make(map[string]attemptInventoryEntry)}
	entries, pathBytes := 0, 0
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		pathBytes += len(name)
		if entries > maxIntegrityWalkEntries || pathBytes > maxIntegrityWalkPathBytes || strings.Count(name, "/") > maxIntegrityWalkDepth {
			return errIntegrityResourceLimit
		}
		if path.Base(name) == ".git" && entry.IsDir() {
			return fs.SkipDir
		}
		clean := "."
		if name != "." {
			clean = strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "./")
		}
		info, err := root.Lstat(clean)
		if err != nil {
			return err
		}
		result.entries[clean] = attemptInventoryEntry{info: info}
		if name != "." && !info.IsDir() && relevantIntegrityPath(clean) {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return errors.New("integrity path is not a stable regular file")
			}
			if info.Size() > maxIntegrityFileBytes || len(result.relevant) == maxIntegrityFiles {
				return errIntegrityResourceLimit
			}
			result.relevant = append(result.relevant, clean)
		}
		return nil
	})
	if err != nil {
		return attemptInventory{}, err
	}
	sort.Strings(result.relevant)
	return result, nil
}

func equalAttemptInventories(before, after attemptInventory) bool {
	if len(before.entries) != len(after.entries) {
		return false
	}
	for name, baseline := range before.entries {
		current, exists := after.entries[name]
		if !exists || !stableAttemptInfo(baseline.info, current.info) {
			return false
		}
	}
	return true
}

func stableAttemptInfo(before, after os.FileInfo) bool {
	if before == nil || after == nil || before.Mode() != after.Mode() || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) {
		return false
	}
	beforeSec, beforeNSec, beforeOK := attemptChangeTime(before)
	afterSec, afterNSec, afterOK := attemptChangeTime(after)
	return beforeOK == afterOK && (!beforeOK || (beforeSec == afterSec && beforeNSec == afterNSec))
}

func attemptChangeTime(info os.FileInfo) (int64, int64, bool) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, 0, false
	}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 0, 0, false
	}
	for _, fieldName := range []string{"Ctim", "Ctimespec"} {
		field := value.FieldByName(fieldName)
		if field.IsValid() {
			sec, nsec := field.FieldByName("Sec"), field.FieldByName("Nsec")
			if sec.IsValid() && nsec.IsValid() && sec.CanInt() && nsec.CanInt() {
				return sec.Int(), nsec.Int(), true
			}
		}
	}
	return 0, 0, false
}

func runSnapshotAttemptHook(stage, file string) error {
	if snapshotAttemptHook != nil {
		return snapshotAttemptHook(stage, file)
	}
	return nil
}

func publicAttemptSnapshotError(err error) error {
	if errors.Is(err, errIntegrityResourceLimit) {
		return errors.New("attempted integrity snapshot: resource limit exceeded")
	}
	return errors.New("walk attempted integrity tree: unstable filesystem state")
}

func parseNULPaths(output []byte) ([]string, error) {
	if len(output) > maxIntegrityGitListBytes {
		return nil, errors.New("original integrity snapshot: resource limit exceeded")
	}
	if len(output) > 0 && output[len(output)-1] != 0 {
		return nil, errors.New("malformed original tree listing")
	}
	parts := bytes.Split(output, []byte{0})
	paths := make([]string, 0, len(parts))
	for _, raw := range parts {
		if len(raw) == 0 {
			continue
		}
		file := string(raw)
		parsed, err := finding.ParsePath(file)
		if err != nil || parsed.String() != file {
			return nil, errors.New("malformed path in original tree listing")
		}
		paths = append(paths, file)
		if len(paths) > maxIntegrityFiles {
			return nil, errors.New("original integrity snapshot: resource limit exceeded")
		}
	}
	return paths, nil
}

func isHexObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}
