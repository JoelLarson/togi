package flywheel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/finding"
)

const (
	maxSuppressionPaths          = 8_192
	maxSuppressionChangedPaths   = 4_096
	maxSuppressionGoFiles        = 2_048
	maxSuppressionPathBytes      = 4_096
	maxSuppressionFileBytes      = 2 << 20
	maxSuppressionSnapshotBytes  = 32 << 20
	maxSuppressionGoBytes        = 16 << 20
	maxSuppressionNodes          = 500_000
	maxSuppressionFileNodes      = 100_000
	maxSuppressionDepth          = 256
	maxSuppressionCandidates     = 20_000
	maxSuppressionSites          = 20_000
	maxSuppressionFindings       = 10_000
	maxSuppressionMatchWork      = 1_000_000
	maxSuppressionAnalysisWork   = 5_000_000
	maxSuppressionAggregateWork  = 10_000_000
	maxSuppressionConstraintTags = 16
	maxSuppressionConstraintNode = 1_024
	maxSuppressionBuildBytes     = 16 << 10
	maxSuppressionBuildDepth     = 64
	maxSuppressionBuildVariants  = 32
)

var errSuppressionResourceLimit = errors.New("suppression integrity resource limit exceeded")

var protectedGolangCIFiles = map[string]struct{}{
	".golangci.yml":  {},
	".golangci.yaml": {},
	".golangci.toml": {},
	".golangci.json": {},
}

type suppressionScope struct {
	all   bool
	rules []string
}

type suppressionSite struct {
	kind       string
	identity   string
	scope      suppressionScope
	constraint constraint.Expr
	line       int
	snippet    string
}

type parsedSuppressionFile struct {
	path        string
	source      []byte
	file        *ast.File
	fset        *token.FileSet
	info        *types.Info
	declKeys    map[ast.Decl]string
	nodePaths   map[ast.Node]string
	ownerHashes map[ast.Node]string
	testing     *testingUniverse
	provenSkips map[*ast.CallExpr]string
	candidates  int
	nodes       int
}

type suppressionSnapshot struct {
	fset  *token.FileSet
	files map[string]*parsedSuppressionFile
}

type suppressionPackage struct {
	key   string
	files []*parsedSuppressionFile
}

func checkSuppressionIntegrity(original, attempted TreeSnapshot) ([]finding.Finding, error) {
	changed, err := boundedChangedPaths(original, attempted)
	if err != nil {
		return nil, fmt.Errorf("inspect suppression snapshot: resource limit exceeded")
	}
	changedGo := make(map[string]bool)
	for _, file := range changed {
		if path.Ext(file) == ".go" {
			changedGo[file] = true
		}
	}
	analysisBudget := maxSuppressionAggregateWork
	var beforeTree, afterTree *suppressionSnapshot
	if len(changedGo) > 0 {
		beforeTree, err = loadSuppressionSnapshotBounded(original, changedGo, &analysisBudget)
		if err != nil {
			return nil, fmt.Errorf("inspect original Go snapshot: %w", publicSuppressionError(err))
		}
		afterTree, err = loadSuppressionSnapshotBounded(attempted, changedGo, &analysisBudget)
		if err != nil {
			return nil, fmt.Errorf("inspect attempted Go snapshot: %w", publicSuppressionError(err))
		}
	}

	findings := make([]finding.Finding, 0)
	for _, file := range changed {
		if _, protected := protectedGolangCIFiles[path.Base(file)]; protected {
			findings = append(findings, integrityFinding(
				"togi/golangci-config-change", file, 1, truncateSuppressionText(path.Base(file)),
				"golangci-lint configuration changed during a fix attempt",
			))
			continue
		}
		if path.Ext(file) != ".go" {
			continue
		}
		beforeSites, err := snapshotSitesBounded(beforeTree, file, &analysisBudget)
		if err != nil {
			return nil, fmt.Errorf("inspect original suppressions in %q: %w", file, publicSuppressionError(err))
		}
		afterSites, err := snapshotSitesBounded(afterTree, file, &analysisBudget)
		if err != nil {
			return nil, fmt.Errorf("inspect attempted suppressions in %q: %w", file, publicSuppressionError(err))
		}
		added, err := compareSuppressionSitesBounded(file, beforeSites, afterSites, &analysisBudget)
		if err != nil {
			return nil, fmt.Errorf("compare suppressions in %q: resource limit exceeded", file)
		}
		findings = append(findings, added...)
		if len(findings) > maxSuppressionFindings {
			return nil, fmt.Errorf("inspect suppression snapshot: resource limit exceeded")
		}
	}
	grouped, err := finding.Group(findings)
	if err != nil {
		return nil, fmt.Errorf("group suppression integrity findings: %w", err)
	}
	return grouped, nil
}

func publicSuppressionError(err error) error {
	if errors.Is(err, errSuppressionResourceLimit) {
		return errors.New("resource limit exceeded")
	}
	return errors.New("malformed Go syntax")
}

func boundedChangedPaths(original, attempted TreeSnapshot) ([]string, error) {
	if len(original.Files) > maxSuppressionPaths || len(attempted.Files) > maxSuppressionPaths {
		return nil, errSuppressionResourceLimit
	}
	union := make(map[string]struct{}, min(maxSuppressionPaths, len(original.Files)+len(attempted.Files)))
	totalBytes := 0
	for _, snapshot := range []TreeSnapshot{original, attempted} {
		for file, contents := range snapshot.Files {
			parsedPath, pathErr := finding.ParsePath(file)
			if len(file) == 0 || len(file) > maxSuppressionPathBytes || len(contents) > maxSuppressionFileBytes ||
				totalBytes > maxSuppressionSnapshotBytes-len(contents) || pathErr != nil || parsedPath.String() != file {
				return nil, errSuppressionResourceLimit
			}
			totalBytes += len(contents)
			if _, exists := union[file]; !exists {
				if len(union) == maxSuppressionPaths {
					return nil, errSuppressionResourceLimit
				}
				union[file] = struct{}{}
			}
		}
	}
	changed := make([]string, 0, min(maxSuppressionChangedPaths, len(union)))
	goFiles := 0
	for file := range union {
		before, beforeOK := original.Files[file]
		after, afterOK := attempted.Files[file]
		if beforeOK == afterOK && bytes.Equal(before, after) {
			continue
		}
		if len(changed) == maxSuppressionChangedPaths {
			return nil, errSuppressionResourceLimit
		}
		if path.Ext(file) == ".go" {
			goFiles++
			if goFiles > maxSuppressionGoFiles {
				return nil, errSuppressionResourceLimit
			}
		}
		changed = append(changed, file)
	}
	sort.Strings(changed)
	return changed, nil
}

func loadSuppressionSnapshot(snapshot TreeSnapshot, changed map[string]bool) (*suppressionSnapshot, error) {
	budget := maxSuppressionAggregateWork
	return loadSuppressionSnapshotBounded(snapshot, changed, &budget)
}

func loadSuppressionSnapshotBounded(snapshot TreeSnapshot, changed map[string]bool, analysisBudget *int) (*suppressionSnapshot, error) {
	result := &suppressionSnapshot{fset: token.NewFileSet(), files: make(map[string]*parsedSuppressionFile)}
	paths := make([]string, 0, len(snapshot.Files))
	for file := range snapshot.Files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	packages := make(map[string]*suppressionPackage)
	totalBytes, totalNodes, totalCandidates, goFiles := 0, 0, 0, 0
	for _, file := range paths {
		if path.Ext(file) != ".go" {
			continue
		}
		goFiles++
		if goFiles > maxSuppressionGoFiles {
			return nil, errSuppressionResourceLimit
		}
		source := snapshot.Files[file]
		if len(source) > maxSuppressionFileBytes || totalBytes > maxSuppressionGoBytes-len(source) {
			return nil, errSuppressionResourceLimit
		}
		totalBytes += len(source)
		if !sourceNestingWithinLimit(file, source) {
			return nil, errSuppressionResourceLimit
		}
		parsed, parseErr := parser.ParseFile(result.fset, file, source, parser.ParseComments|parser.AllErrors)
		if parseErr != nil {
			if changed[file] {
				return nil, parseErr
			}
			continue
		}
		nodes, depth, candidates := suppressionASTStats(parsed)
		analysisFactor := candidates + 4
		if nodes > maxSuppressionFileNodes || depth > maxSuppressionDepth || totalNodes > maxSuppressionNodes-nodes ||
			totalCandidates > maxSuppressionCandidates-candidates ||
			(nodes > 0 && (analysisFactor > maxSuppressionAnalysisWork/nodes || analysisFactor > *analysisBudget/nodes)) {
			return nil, errSuppressionResourceLimit
		}
		analysisWork := nodes * analysisFactor
		*analysisBudget -= analysisWork
		totalNodes += nodes
		totalCandidates += candidates
		entry := &parsedSuppressionFile{path: file, source: source, file: parsed, fset: result.fset, candidates: candidates, nodes: nodes}
		result.files[file] = entry
		key := path.Dir(file) + "\x00" + parsed.Name.Name
		group := packages[key]
		if group == nil {
			group = &suppressionPackage{key: key}
			packages[key] = group
		}
		group.files = append(group.files, entry)
	}
	keys := make([]string, 0, len(packages))
	for key := range packages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := checkSuppressionPackage(result.fset, packages[key], analysisBudget); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func sourceNestingWithinLimit(file string, source []byte) bool {
	files := token.NewFileSet()
	tokenFile := files.AddFile(file, -1, len(source))
	var value scanner.Scanner
	value.Init(tokenFile, source, func(token.Position, string) {}, 0)
	depth := 0
	for {
		_, current, _ := value.Scan()
		switch current {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
			if depth > maxSuppressionDepth {
				return false
			}
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth > 0 {
				depth--
			}
		case token.EOF:
			return true
		}
	}
}

func suppressionASTStats(root ast.Node) (nodes, maximumDepth, candidates int) {
	depth := 0
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			depth--
			return true
		}
		nodes++
		depth++
		maximumDepth = max(maximumDepth, depth)
		switch value := node.(type) {
		case *ast.Comment:
			if commentMaySuppress(value.Text) {
				candidates++
			}
		case *ast.CallExpr:
			if selector, ok := unparenthesized(value.Fun).(*ast.SelectorExpr); ok &&
				(selector.Sel.Name == "Skip" || selector.Sel.Name == "SkipNow") {
				candidates++
			}
		}
		return candidates <= maxSuppressionCandidates
	})
	return nodes, maximumDepth, candidates
}

func commentMaySuppress(text string) bool {
	return len(parseSuppressionDirectives(text)) > 0 || buildDirectiveText(text)
}

func checkSuppressionPackage(fset *token.FileSet, group *suppressionPackage, analysisBudget *int) error {
	active := make([]*parsedSuppressionFile, 0, len(group.files))
	buildContext := snapshotBuildContext(group)
	inactive := make([]*parsedSuppressionFile, 0)
	for _, file := range group.files {
		matches, err := buildContext.MatchFile(path.Dir(file.path), path.Base(file.path))
		if err == nil && matches {
			active = append(active, file)
		} else {
			inactive = append(inactive, file)
		}
	}
	checkSuppressionFileSet(fset, group.key, active, active)
	type buildVariant struct {
		context build.Context
		owned   []*parsedSuppressionFile
	}
	variants := make(map[string]*buildVariant)
	for _, file := range inactive {
		context, key, err := compatibleSuppressionBuildContext(buildContext, file, analysisBudget)
		if err != nil {
			return err
		}
		variant := variants[key]
		if variant == nil {
			if len(variants) == maxSuppressionBuildVariants {
				return errSuppressionResourceLimit
			}
			variant = &buildVariant{context: context}
			variants[key] = variant
		}
		variant.owned = append(variant.owned, file)
	}
	keys := make([]string, 0, len(variants))
	for key := range variants {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		variant := variants[key]
		files := make([]*parsedSuppressionFile, 0, len(group.files))
		included := make(map[*parsedSuppressionFile]bool, len(group.files))
		for _, file := range variant.owned {
			files = append(files, file)
			included[file] = true
		}
		for _, file := range group.files {
			if *analysisBudget == 0 {
				return errSuppressionResourceLimit
			}
			*analysisBudget--
			matches, err := variant.context.MatchFile(path.Dir(file.path), path.Base(file.path))
			if err == nil && matches && !included[file] {
				files = append(files, file)
				included[file] = true
			}
		}
		work := 0
		for _, file := range files {
			for range 2 { // Type checking plus the proven-call collection walk.
				if work > *analysisBudget-file.nodes {
					return errSuppressionResourceLimit
				}
				work += file.nodes
			}
		}
		*analysisBudget -= work
		checkSuppressionFileSet(fset, group.key+"#inactive"+strconv.Itoa(index), files, variant.owned)
	}
	return nil
}

func compatibleSuppressionBuildContext(
	base build.Context,
	file *parsedSuppressionFile,
	analysisBudget *int,
) (build.Context, string, error) {
	tags := make(map[string]struct{})
	for _, group := range file.file.Comments {
		if group.End() >= file.file.Package || file.fset.Position(file.file.Package).Line-file.fset.Position(group.End()).Line < 2 {
			continue
		}
		for _, comment := range group.List {
			if !buildDirectiveText(comment.Text) {
				continue
			}
			expression, err := constraint.Parse(comment.Text)
			if err != nil {
				continue
			}
			collectConstraintTags(expression, tags)
		}
	}
	fixed := make(map[string]bool)
	for _, tag := range append(strings.Fields(suppressionKnownOS), strings.Fields(suppressionKnownArch)...) {
		fixed[tag] = true
	}
	fixed["cgo"] = true
	custom := make([]string, 0, len(tags))
	for tag := range tags {
		if !fixed[tag] && !strings.HasPrefix(tag, "go1.") && tag != "gc" {
			custom = append(custom, tag)
		}
	}
	if len(custom) > maxSuppressionConstraintTags {
		return build.Context{}, "", errSuppressionResourceLimit
	}
	sort.Strings(custom)
	operatingSystems := preferredSuppressionBuildValues(runtime.GOOS, suppressionKnownOS)
	architectures := preferredSuppressionBuildValues(runtime.GOARCH, suppressionKnownArch)
	for _, operatingSystem := range operatingSystems {
		for _, architecture := range architectures {
			for cgo := 0; cgo < 2; cgo++ {
				for mask := 0; mask < 1<<len(custom); mask++ {
					if *analysisBudget == 0 {
						return build.Context{}, "", errSuppressionResourceLimit
					}
					*analysisBudget--
					candidate := base
					candidate.GOOS = operatingSystem
					candidate.GOARCH = architecture
					candidate.CgoEnabled = cgo == 1
					candidate.BuildTags = candidate.BuildTags[:0]
					for index, tag := range custom {
						if mask&(1<<index) != 0 {
							candidate.BuildTags = append(candidate.BuildTags, tag)
						}
					}
					matches, err := candidate.MatchFile(path.Dir(file.path), path.Base(file.path))
					if err == nil && matches {
						key := operatingSystem + "/" + architecture + "/" + strconv.FormatBool(candidate.CgoEnabled) + "/" + strings.Join(candidate.BuildTags, ",")
						return candidate, key, nil
					}
				}
			}
		}
	}
	return base, "isolated:" + file.path, nil
}

const suppressionKnownOS = "aix android darwin dragonfly freebsd illumos ios js linux netbsd openbsd plan9 solaris wasip1 windows"
const suppressionKnownArch = "386 amd64 arm arm64 loong64 mips mips64 mips64le mipsle ppc64 ppc64le riscv64 s390x wasm"

func preferredSuppressionBuildValues(preferred, values string) []string {
	result := []string{preferred}
	for _, value := range strings.Fields(values) {
		if value != preferred {
			result = append(result, value)
		}
	}
	return result
}

func checkSuppressionFileSet(
	fset *token.FileSet,
	packagePath string,
	parsedFiles []*parsedSuppressionFile,
	ownedFiles []*parsedSuppressionFile,
) {
	if len(parsedFiles) == 0 {
		return
	}
	universe := newTestingUniverse()
	snapshotImports := &snapshotImporter{testing: universe, packages: make(map[string]*types.Package)}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	files := make([]*ast.File, 0, len(parsedFiles))
	for _, file := range parsedFiles {
		files = append(files, file.file)
	}
	configuration := types.Config{
		Importer:                 snapshotImports,
		GoVersion:                "go1.25",
		DisableUnusedImportCheck: true,
		Error:                    func(error) {},
	}
	_, _ = configuration.Check(packagePath, fset, files, info)
	for _, file := range parsedFiles {
		view := *file
		view.info = info
		view.testing = universe
		ast.Inspect(file.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := testingSkipCallWithTypes(&view, call); name != "" {
				if file.provenSkips == nil {
					file.provenSkips = make(map[*ast.CallExpr]string)
				}
				file.provenSkips[call] = name
			}
			return true
		})
	}
	for _, file := range ownedFiles {
		file.info = info
		file.testing = universe
	}
}

func snapshotBuildContext(group *suppressionPackage) build.Context {
	sources := make(map[string][]byte, len(group.files))
	for _, file := range group.files {
		sources[path.Clean(file.path)] = file.source
	}
	context := build.Default
	context.GOOS = runtime.GOOS
	context.GOARCH = runtime.GOARCH
	context.Compiler = "gc"
	context.CgoEnabled = false
	context.BuildTags = nil
	context.ToolTags = nil
	context.ReleaseTags = make([]string, 25)
	for index := range context.ReleaseTags {
		context.ReleaseTags[index] = "go1." + strconv.Itoa(index+1)
	}
	context.JoinPath = path.Join
	context.OpenFile = func(name string) (io.ReadCloser, error) {
		source, ok := sources[path.Clean(name)]
		if !ok {
			return nil, errors.New("snapshot file unavailable")
		}
		return io.NopCloser(bytes.NewReader(source)), nil
	}
	return context
}

type snapshotImporter struct {
	testing  *testingUniverse
	packages map[string]*types.Package
}

func (value *snapshotImporter) Import(importPath string) (*types.Package, error) {
	if importPath == "testing" {
		return value.testing.pkg, nil
	}
	if existing := value.packages[importPath]; existing != nil {
		return existing, nil
	}
	pkg := types.NewPackage(importPath, opaquePackageName(importPath))
	pkg.MarkComplete()
	value.packages[importPath] = pkg
	return pkg, nil
}

func opaquePackageName(importPath string) string {
	name := path.Base(importPath)
	var result strings.Builder
	for index, character := range name {
		if (index == 0 && (unicode.IsLetter(character) || character == '_')) ||
			(index > 0 && (unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_')) {
			result.WriteRune(character)
		}
	}
	if result.Len() == 0 {
		return "opaque"
	}
	return result.String()
}

type testingUniverse struct {
	pkg     *types.Package
	tb      *types.Named
	methods map[*types.Func]string
}

func newTestingUniverse() *testingUniverse {
	pkg := types.NewPackage("testing", "testing")
	result := &testingUniverse{pkg: pkg, methods: make(map[*types.Func]string)}
	anyType := types.NewInterfaceType(nil, nil)
	anyType.Complete()
	skipSignature := func(receiver *types.Var) *types.Signature {
		parameter := types.NewVar(token.NoPos, pkg, "args", types.NewSlice(anyType))
		return types.NewSignatureType(receiver, nil, nil, types.NewTuple(parameter), nil, true)
	}
	skipNowSignature := func(receiver *types.Var) *types.Signature {
		return types.NewSignatureType(receiver, nil, nil, nil, nil, false)
	}
	for _, name := range []string{"T", "B", "F"} {
		object := types.NewTypeName(token.NoPos, pkg, name, nil)
		named := types.NewNamed(object, types.NewStruct(nil, nil), nil)
		pkg.Scope().Insert(object)
		receiver := types.NewVar(token.NoPos, pkg, "", types.NewPointer(named))
		for _, method := range []struct {
			name      string
			signature *types.Signature
		}{{"Skip", skipSignature(receiver)}, {"SkipNow", skipNowSignature(receiver)}} {
			function := types.NewFunc(token.NoPos, pkg, method.name, method.signature)
			named.AddMethod(function)
			result.methods[function] = method.name
		}
	}
	tbMethods := make([]*types.Func, 0, 2)
	for _, method := range []struct {
		name      string
		signature *types.Signature
	}{{"Skip", skipSignature(nil)}, {"SkipNow", skipNowSignature(nil)}} {
		function := types.NewFunc(token.NoPos, pkg, method.name, method.signature)
		tbMethods = append(tbMethods, function)
		result.methods[function] = method.name
	}
	underlying := types.NewInterfaceType(tbMethods, nil)
	underlying.Complete()
	tbObject := types.NewTypeName(token.NoPos, pkg, "TB", nil)
	result.tb = types.NewNamed(tbObject, underlying, nil)
	pkg.Scope().Insert(tbObject)
	pkg.MarkComplete()
	return result
}

func snapshotSitesBounded(snapshot *suppressionSnapshot, file string, analysisBudget *int) ([]suppressionSite, error) {
	if snapshot == nil || snapshot.files[file] == nil {
		return nil, nil
	}
	parsed := snapshot.files[file]
	sites := make([]suppressionSite, 0, parsed.candidates)
	build, buildLine, buildDuplicates, hasBuild, err := effectiveFileBuildConstraint(parsed, analysisBudget)
	if err != nil {
		return nil, err
	}
	if hasBuild {
		sites = append(sites, suppressionSite{
			kind:       "build",
			identity:   "file\x00header\x00build",
			constraint: build,
			line:       buildLine,
			snippet:    "build constraint",
		})
		sites = append(sites, buildDuplicates...)
	}
	directives := make(map[*ast.CommentGroup]map[*ast.Comment][]parsedDirective)
	for _, group := range parsed.file.Comments {
		groupDirectives := make(map[*ast.Comment][]parsedDirective)
		for _, comment := range group.List {
			if err := spendSuppressionWork(analysisBudget, 1); err != nil {
				return nil, err
			}
			if buildDirectiveText(comment.Text) {
				continue
			}
			if parsedDirectives := parseSuppressionDirectives(comment.Text); len(parsedDirectives) > 0 {
				groupDirectives[comment] = parsedDirectives
			}
		}
		if len(groupDirectives) > 0 {
			directives[group] = groupDirectives
		}
	}
	if len(directives) == 0 && len(parsed.provenSkips) == 0 {
		return sites, nil
	}
	if err := spendSuppressionWork(analysisBudget, parsed.nodes*2); err != nil {
		return nil, err
	}
	parsed.ownerHashes = normalizedOwnerFingerprints(parsed.file)
	parsed.declKeys = declarationKeys(parsed.file, parsed.ownerHashes)
	parsed.nodePaths = semanticNodePaths(parsed.file, parsed.ownerHashes)
	for _, group := range parsed.file.Comments {
		groupDirectives := directives[group]
		if len(groupDirectives) == 0 {
			continue
		}
		if err := spendSuppressionWork(analysisBudget, parsed.nodes); err != nil {
			return nil, err
		}
		declaration := enclosingSuppressionDeclaration(parsed.file, group)
		declarationKey := "file"
		if declaration != nil {
			declarationKey = parsed.declKeys[declaration]
		}
		role := commentRole(parsed, declaration, group)
		for _, comment := range group.List {
			line := snapshotLine(snapshot.fset, comment.Pos())
			for _, directive := range groupDirectives[comment] {
				sites = append(sites, suppressionSite{
					kind:     directive.kind,
					identity: declarationKey + "\x00" + role + "\x00" + directive.kind,
					scope:    directive.scope,
					line:     line + directive.lineOffset,
					snippet:  boundedSuppressionSnippet(declarationKey, directive.kind, role),
				})
			}
		}
	}
	if len(parsed.provenSkips) > 0 {
		if err := spendSuppressionWork(analysisBudget, parsed.nodes); err != nil {
			return nil, err
		}
	}
	for declaration, declarationKey := range parsed.declKeys {
		ast.Inspect(declaration, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := testingSkipCall(parsed, call)
			if name == "" {
				return true
			}
			role := parsed.nodePaths[call]
			role += ":" + testingCallTargetFingerprint(call, parsed.ownerHashes)
			sites = append(sites, suppressionSite{
				kind:     "call:" + name,
				identity: declarationKey + "\x00" + role + "\x00call:" + name,
				line:     snapshotLine(snapshot.fset, call.Pos()),
				snippet:  boundedSuppressionSnippet(declarationKey, name+"()", role),
			})
			return len(sites) <= maxSuppressionSites
		})
		if len(sites) > maxSuppressionSites {
			return nil, errSuppressionResourceLimit
		}
	}
	if len(sites) > maxSuppressionSites {
		return nil, errSuppressionResourceLimit
	}
	sort.SliceStable(sites, func(left, right int) bool {
		if sites[left].identity != sites[right].identity {
			return sites[left].identity < sites[right].identity
		}
		if sites[left].line != sites[right].line {
			return sites[left].line < sites[right].line
		}
		return sites[left].kind < sites[right].kind
	})
	return sites, nil
}

func effectiveFileBuildConstraint(
	file *parsedSuppressionFile,
	analysisBudget *int,
) (constraint.Expr, int, []suppressionSite, bool, error) {
	type legacyDirective struct {
		expression constraint.Expr
		line       int
	}
	var modern constraint.Expr
	legacy := make([]legacyDirective, 0, 2)
	duplicates := make([]suppressionSite, 0)
	line := 1
	for _, group := range file.file.Comments {
		for _, comment := range group.List {
			if err := spendSuppressionWork(analysisBudget, 1); err != nil {
				return nil, 0, nil, false, err
			}
			expression, effective, err := effectiveBuildSuppression(file, group, comment)
			if err != nil {
				return nil, 0, nil, true, err
			}
			if !effective {
				continue
			}
			if modern == nil && len(legacy) == 0 {
				line = snapshotLine(file.fset, comment.Pos())
			}
			if constraint.IsGoBuild(strings.TrimSpace(comment.Text)) {
				if modern == nil {
					modern = expression
				} else {
					duplicates = append(duplicates, buildDuplicateSite(
						"go:build", snapshotLine(file.fset, comment.Pos()),
					))
					modern = &constraint.AndExpr{X: modern, Y: expression}
				}
			} else {
				legacy = append(legacy, legacyDirective{
					expression: expression,
					line:       snapshotLine(file.fset, comment.Pos()),
				})
			}
		}
	}
	representatives := make([]constraint.Expr, 0, len(legacy))
	for _, directive := range legacy {
		duplicate := false
		for _, representative := range representatives {
			equivalent, err := constraintsEquivalent(
				directive.expression, representative, nil, analysisBudget,
			)
			if err != nil {
				return nil, 0, nil, true, err
			}
			if equivalent {
				duplicate = true
				break
			}
		}
		if duplicate {
			duplicates = append(duplicates, suppressionSite{
				kind:       "build-duplicate-legacy",
				identity:   "file\x00header\x00duplicate:+build",
				constraint: directive.expression,
				line:       directive.line,
				snippet:    "duplicate build constraint",
			})
		} else {
			representatives = append(representatives, directive.expression)
		}
	}
	result := modern
	if result == nil {
		for _, directive := range legacy {
			if result == nil {
				result = directive.expression
			} else {
				result = &constraint.AndExpr{X: result, Y: directive.expression}
			}
		}
	}
	if result == nil {
		return nil, 0, nil, false, nil
	}
	if !constraintWithinSuppressionLimits(result) {
		return nil, 0, nil, true, errSuppressionResourceLimit
	}
	return result, line, duplicates, true, nil
}

func buildDuplicateSite(form string, line int) suppressionSite {
	return suppressionSite{
		kind:     "build-duplicate",
		identity: "file\x00header\x00duplicate:" + form,
		scope:    suppressionScope{all: true},
		line:     line,
		snippet:  "duplicate build constraint",
	}
}

func spendSuppressionWork(budget *int, amount int) error {
	if budget == nil || amount < 0 || *budget < amount {
		return errSuppressionResourceLimit
	}
	*budget -= amount
	return nil
}

func testingSkipCall(file *parsedSuppressionFile, call *ast.CallExpr) string {
	if name := file.provenSkips[call]; name != "" {
		return name
	}
	return testingSkipCallWithTypes(file, call)
}

func testingSkipCallWithTypes(file *parsedSuppressionFile, call *ast.CallExpr) string {
	selector, ok := unparenthesized(call.Fun).(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Skip" && selector.Sel.Name != "SkipNow") {
		return ""
	}
	selection := file.info.Selections[selector]
	if selection == nil || selection.Kind() == types.FieldVal {
		return ""
	}
	function, ok := selection.Obj().(*types.Func)
	if !ok {
		return ""
	}
	if name := file.testing.methods[function]; name != "" {
		return name
	}
	receiver := selection.Recv()
	underlying, ok := types.Unalias(receiver).Underlying().(*types.Interface)
	if !ok {
		return ""
	}
	underlying.Complete()
	testingInterface, _ := file.testing.tb.Underlying().(*types.Interface)
	if testingInterface == nil || !interfaceEmbedsTestingTB(receiver, file.testing.tb, make(map[types.Type]bool)) ||
		!types.Implements(receiver, testingInterface) {
		return ""
	}
	want := testingMethod(file.testing.tb, selector.Sel.Name)
	if want == nil || !sameMethodSignature(function, want) {
		return ""
	}
	return selector.Sel.Name
}

func interfaceEmbedsTestingTB(value types.Type, testingTB *types.Named, seen map[types.Type]bool) bool {
	value = types.Unalias(value)
	if types.Identical(value, testingTB) {
		return true
	}
	if seen[value] {
		return false
	}
	seen[value] = true
	if parameter, ok := value.(*types.TypeParam); ok {
		return interfaceEmbedsTestingTB(parameter.Constraint(), testingTB, seen)
	}
	underlying, ok := value.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	underlying.Complete()
	for index := 0; index < underlying.NumEmbeddeds(); index++ {
		if interfaceEmbedsTestingTB(underlying.EmbeddedType(index), testingTB, seen) {
			return true
		}
	}
	return false
}

func testingMethod(named *types.Named, name string) *types.Func {
	methodSet := types.NewMethodSet(named)
	for index := 0; index < methodSet.Len(); index++ {
		selection := methodSet.At(index)
		if selection.Obj().Name() == name {
			function, _ := selection.Obj().(*types.Func)
			return function
		}
	}
	return nil
}

func sameMethodSignature(left, right *types.Func) bool {
	leftSignature, leftOK := left.Type().(*types.Signature)
	rightSignature, rightOK := right.Type().(*types.Signature)
	if !leftOK || !rightOK {
		return false
	}
	leftWithoutReceiver := types.NewSignatureType(nil, nil, nil, leftSignature.Params(), leftSignature.Results(), leftSignature.Variadic())
	rightWithoutReceiver := types.NewSignatureType(nil, nil, nil, rightSignature.Params(), rightSignature.Results(), rightSignature.Variadic())
	return types.Identical(leftWithoutReceiver, rightWithoutReceiver)
}

func unparenthesized(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

func declarationKeys(file *ast.File, owners map[ast.Node]string) map[ast.Decl]string {
	result := make(map[ast.Decl]string, len(file.Decls))
	counts := make(map[string]int)
	for _, declaration := range file.Decls {
		base := declarationBaseKey(declaration) + ":" + owners[declaration]
		ordinal := counts[base]
		counts[base]++
		result[declaration] = base + "#" + strconv.Itoa(ordinal)
	}
	return result
}

func declarationBaseKey(declaration ast.Decl) string {
	switch value := declaration.(type) {
	case *ast.FuncDecl:
		receiver := ""
		if value.Recv != nil && len(value.Recv.List) > 0 {
			receiver = compactNode(value.Recv.List[0].Type) + "."
		}
		return "func:" + receiver + value.Name.Name
	case *ast.GenDecl:
		names := make([]string, 0)
		for _, specification := range value.Specs {
			switch item := specification.(type) {
			case *ast.TypeSpec:
				names = append(names, item.Name.Name)
			case *ast.ValueSpec:
				for _, name := range item.Names {
					names = append(names, name.Name)
				}
			}
		}
		return value.Tok.String() + ":" + strings.Join(names, ",")
	default:
		return fmt.Sprintf("decl:%T", declaration)
	}
}

func compactNode(node ast.Node) string {
	if node == nil {
		return ""
	}
	var output strings.Builder
	_ = printer.Fprint(&output, token.NewFileSet(), node)
	return strings.Join(strings.Fields(output.String()), "")
}

func normalizedOwnerFingerprints(file *ast.File) map[ast.Node]string {
	type frame struct {
		node     ast.Node
		children []string
	}
	result := make(map[ast.Node]string)
	var stack []frame
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			value := ownerNodeLabel(current.node) + ":" + strings.Join(current.children, ";")
			if literal, ok := current.node.(*ast.FuncLit); ok {
				value = "func-literal:" + compactNode(literal.Type)
			}
			fingerprint := structuralPathDigest(value)
			result[current.node] = fingerprint
			if len(stack) > 0 {
				stack[len(stack)-1].children = append(stack[len(stack)-1].children, fingerprint)
			}
			return true
		}
		switch node.(type) {
		case *ast.Comment, *ast.CommentGroup:
			return false
		}
		stack = append(stack, frame{node: node})
		return true
	})
	return result
}

func ownerNodeLabel(node ast.Node) string {
	value := fmt.Sprintf("%T", node)
	switch typed := node.(type) {
	case *ast.Ident:
		value += ":" + typed.Name
	case *ast.BasicLit:
		value += ":" + typed.Kind.String() + ":" + typed.Value
	case *ast.BinaryExpr:
		value += ":" + typed.Op.String()
	case *ast.UnaryExpr:
		value += ":" + typed.Op.String()
	case *ast.AssignStmt:
		value += ":" + typed.Tok.String()
	case *ast.IncDecStmt:
		value += ":" + typed.Tok.String()
	case *ast.BranchStmt:
		value += ":" + typed.Tok.String()
	case *ast.ChanType:
		value += ":" + strconv.Itoa(int(typed.Dir))
	}
	return value
}

func semanticNodePaths(file *ast.File, owners map[ast.Node]string) map[ast.Node]string {
	result := make(map[ast.Node]string)
	for _, declaration := range file.Decls {
		var frames []int
		var paths []string
		occurrences := make(map[string]int)
		ast.Inspect(declaration, func(node ast.Node) bool {
			if node == nil {
				if len(frames) > 0 {
					frames = frames[:len(frames)-1]
					paths = paths[:len(paths)-1]
				}
				return true
			}
			if _, comment := node.(*ast.Comment); comment {
				return false
			}
			if _, comment := node.(*ast.CommentGroup); comment {
				return false
			}
			ordinal := 0
			if len(frames) > 0 {
				ordinal = frames[len(frames)-1]
				frames[len(frames)-1]++
			}
			semantic := semanticNodeFingerprint(node, owners)
			occurrenceIdentity := semantic
			if occurrenceIdentity == "" {
				switch node.(type) {
				case ast.Stmt, ast.Decl:
					occurrenceIdentity = owners[node]
				}
			}
			occurrence := 0
			if occurrenceIdentity != "" {
				key := fmt.Sprintf("%T:%s", node, occurrenceIdentity)
				occurrence = occurrences[key]
				occurrences[key]++
			}
			component := fmt.Sprintf("%T:%d:%s:%d", node, ordinal, semantic, occurrence)
			current := component
			if len(paths) > 0 {
				current = paths[len(paths)-1] + "/" + component
			}
			current = structuralPathDigest(current)
			switch node.(type) {
			case ast.Stmt, *ast.CallExpr, ast.Decl:
				result[node] = current
			}
			frames = append(frames, 0)
			paths = append(paths, current)
			return true
		})
	}
	return result
}

func semanticNodeFingerprint(node ast.Node, owners map[ast.Node]string) string {
	var value string
	switch typed := node.(type) {
	case *ast.IfStmt:
		value = owners[typed.Init] + ":" + owners[typed.Cond]
	case *ast.ForStmt:
		value = owners[typed.Init] + ":" + owners[typed.Cond] + ":" + owners[typed.Post]
	case *ast.RangeStmt:
		value = owners[typed.Key] + ":" + owners[typed.Value] + ":" + owners[typed.X]
	case *ast.SwitchStmt:
		value = owners[typed.Init] + ":" + owners[typed.Tag]
	case *ast.TypeSwitchStmt:
		value = owners[typed.Init] + ":" + owners[typed.Assign]
	case *ast.CaseClause:
		for _, expression := range typed.List {
			value += ":" + owners[expression]
		}
	case *ast.CommClause:
		value = owners[typed.Comm]
	case *ast.FuncLit:
		value = owners[typed]
	case *ast.CallExpr:
		value = owners[unparenthesized(typed.Fun)]
		value += ":args:" + strconv.Itoa(len(typed.Args))
		for index, argument := range typed.Args {
			value += ":arg:" + strconv.Itoa(index) + ":" + owners[argument]
		}
	case *ast.AssignStmt:
		value = typed.Tok.String()
		for _, expression := range typed.Lhs {
			value += ":" + owners[expression]
		}
	case *ast.ValueSpec:
		value = owners[typed.Type]
		for _, name := range typed.Names {
			value += ":" + name.Name
		}
	case *ast.CompositeLit:
		value = owners[typed.Type]
	case *ast.SendStmt:
		value = owners[typed.Chan]
	case *ast.ReturnStmt:
		value = "results:" + strconv.Itoa(len(typed.Results))
	case *ast.KeyValueExpr:
		value = owners[typed.Key]
	default:
		return ""
	}
	return structuralPathDigest(value)
}

func structuralPathDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:12])
}

func enclosingSuppressionDeclaration(file *ast.File, comment *ast.CommentGroup) ast.Decl {
	var containing ast.Decl
	for _, declaration := range file.Decls {
		if declaration.Pos() <= comment.Pos() && comment.End() <= declaration.End() {
			containing = declaration
			break
		}
		if comment.End() <= declaration.Pos() {
			return declaration
		}
	}
	return containing
}

func commentRole(file *parsedSuppressionFile, declaration ast.Decl, comment *ast.CommentGroup) string {
	if declaration == nil {
		return "file"
	}
	var sameLine ast.Stmt
	var following ast.Stmt
	commentLine := file.fset.Position(comment.Pos()).Line
	ast.Inspect(declaration, func(node ast.Node) bool {
		statement, ok := node.(ast.Stmt)
		if !ok {
			return true
		}
		startLine := file.fset.Position(statement.Pos()).Line
		endLine := file.fset.Position(statement.End()).Line
		if startLine <= commentLine && commentLine <= endLine {
			if sameLine == nil || statement.End()-statement.Pos() < sameLine.End()-sameLine.Pos() {
				sameLine = statement
			}
		}
		if statement.Pos() > comment.End() && (following == nil || statement.Pos() < following.Pos()) {
			following = statement
		}
		return true
	})
	target := ast.Node(sameLine)
	if target == nil {
		target = following
	}
	if target == nil {
		target = declaration
	}
	return file.nodePaths[target] + ":" + file.ownerHashes[target]
}

func testingCallTargetFingerprint(call *ast.CallExpr, owners map[ast.Node]string) string {
	selector, ok := unparenthesized(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return owners[call.Fun]
	}
	return structuralPathDigest(owners[selector.X] + "." + selector.Sel.Name)
}

func snapshotLine(fset *token.FileSet, position token.Pos) int {
	line := fset.Position(position).Line
	if line < 1 {
		return 1
	}
	return line
}

type parsedDirective struct {
	kind       string
	scope      suppressionScope
	lineOffset int
}

func parseSuppressionDirectives(text string) []parsedDirective {
	lines := normalizedCommentLines(text)
	result := make([]parsedDirective, 0)
	for offset, line := range lines {
		trimmed := strings.TrimSpace(line)
		if remainder, ok := suppressionTokenRemainder(trimmed, "nolint", true); ok {
			remainder = strings.TrimSpace(remainder)
			scope := suppressionScope{all: true}
			if strings.HasPrefix(remainder, ":") {
				fields := strings.Fields(strings.TrimSpace(remainder[1:]))
				if len(fields) > 0 {
					scope = ruleScope(fields[0])
				}
			}
			result = append(result, parsedDirective{kind: "nolint", scope: scope, lineOffset: offset})
			continue
		}
		if remainder, ok := suppressionTokenRemainder(trimmed, "lint:ignore", false); ok {
			fields := strings.Fields(strings.TrimSpace(remainder))
			if len(fields) > 0 {
				result = append(result, parsedDirective{kind: "lint:ignore", scope: ruleScope(fields[0]), lineOffset: offset})
			}
			continue
		}
		if remainder, ok := suppressionTokenRemainder(trimmed, "#nosec", false); ok {
			fields := strings.Fields(strings.TrimSpace(remainder))
			var rules []string
			for _, field := range fields {
				if strings.HasPrefix(field, "--") || !looksLikeNosecRule(field) {
					break
				}
				rules = append(rules, strings.Split(field, ",")...)
			}
			scope := suppressionScope{all: len(rules) == 0}
			if len(rules) > 0 {
				scope = normalizedRuleScope(rules)
			}
			result = append(result, parsedDirective{kind: "nosec", scope: scope, lineOffset: offset})
		}
	}
	return result
}

func suppressionTokenRemainder(text, token string, colon bool) (string, bool) {
	if len(text) < len(token) || !strings.EqualFold(text[:len(token)], token) {
		return "", false
	}
	if len(text) == len(token) {
		return "", true
	}
	next, _ := utf8.DecodeRuneInString(text[len(token):])
	if unicode.IsSpace(next) || (colon && next == ':') {
		return text[len(token):], true
	}
	return "", false
}

func normalizedCommentLines(text string) []string {
	if strings.HasPrefix(text, "//") {
		return []string{strings.TrimSpace(strings.TrimPrefix(text, "//"))}
	}
	text = strings.TrimPrefix(text, "/*")
	text = strings.TrimSuffix(text, "*/")
	lines := strings.Split(text, "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[index]), "*"))
	}
	return lines
}

func ruleScope(value string) suppressionScope {
	return normalizedRuleScope(strings.Split(value, ","))
}

func normalizedRuleScope(rules []string) suppressionScope {
	set := make(map[string]struct{})
	for _, rule := range rules {
		rule = strings.ToLower(strings.Trim(strings.TrimSpace(rule), ","))
		if rule != "" {
			set[rule] = struct{}{}
		}
	}
	result := suppressionScope{rules: make([]string, 0, len(set))}
	for rule := range set {
		result.rules = append(result.rules, rule)
	}
	sort.Strings(result.rules)
	result.all = len(result.rules) == 0
	return result
}

func looksLikeNosecRule(value string) bool {
	for _, rule := range strings.Split(strings.Trim(value, ","), ",") {
		if len(rule) < 2 || (rule[0] != 'G' && rule[0] != 'g') {
			return false
		}
		for _, character := range rule[1:] {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func effectiveBuildSuppression(file *parsedSuppressionFile, group *ast.CommentGroup, comment *ast.Comment) (constraint.Expr, bool, error) {
	if group.End() >= file.file.Package || !buildDirectiveText(comment.Text) {
		return nil, false, nil
	}
	endLine := file.fset.Position(group.End()).Line
	packageLine := file.fset.Position(file.file.Package).Line
	if packageLine-endLine < 2 {
		return nil, false, nil
	}
	if !buildDirectiveWithinLimits(comment.Text) {
		return nil, true, errSuppressionResourceLimit
	}
	expression, err := constraint.Parse(comment.Text)
	if err != nil {
		return nil, true, err
	}
	if !constraintWithinSuppressionLimits(expression) {
		return nil, true, errSuppressionResourceLimit
	}
	return expression, true, nil
}

func buildDirectiveWithinLimits(text string) bool {
	if len(text) > maxSuppressionBuildBytes {
		return false
	}
	depth := 0
	for _, character := range text {
		switch character {
		case '(':
			depth++
			if depth > maxSuppressionBuildDepth {
				return false
			}
		case ')':
			if depth > 0 {
				depth--
			}
		}
	}
	return true
}

func buildDirectiveText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return constraint.IsGoBuild(trimmed) || constraint.IsPlusBuild(trimmed)
}

func constraintWithinSuppressionLimits(expression constraint.Expr) bool {
	nodes, tags := 0, make(map[string]struct{})
	var visit func(constraint.Expr) bool
	visit = func(value constraint.Expr) bool {
		nodes++
		if nodes > maxSuppressionConstraintNode {
			return false
		}
		switch typed := value.(type) {
		case *constraint.TagExpr:
			tags[typed.Tag] = struct{}{}
		case *constraint.NotExpr:
			return visit(typed.X)
		case *constraint.AndExpr:
			return visit(typed.X) && visit(typed.Y)
		case *constraint.OrExpr:
			return visit(typed.X) && visit(typed.Y)
		}
		return len(tags) <= maxSuppressionConstraintTags
	}
	return visit(expression) && len(tags) <= maxSuppressionConstraintTags
}

func compareSuppressionSites(file string, original, attempted []suppressionSite) ([]finding.Finding, error) {
	budget := maxSuppressionAggregateWork
	return compareSuppressionSitesBounded(file, original, attempted, &budget)
}

func compareSuppressionSitesBounded(
	file string,
	original, attempted []suppressionSite,
	analysisBudget *int,
) ([]finding.Finding, error) {
	candidates := append([]suppressionSite(nil), attempted...)
	work := 0
	edges := make([][]int, len(candidates))
	for candidateIndex, candidate := range candidates {
		for index, baseline := range original {
			if err := spendSuppressionWork(analysisBudget, 1); err != nil {
				return nil, err
			}
			work++
			if work > maxSuppressionMatchWork {
				return nil, errSuppressionResourceLimit
			}
			if baseline.identity != candidate.identity {
				continue
			}
			narrows, err := suppressionNarrows(candidate, baseline, &work, analysisBudget)
			if err != nil {
				return nil, err
			}
			if !narrows {
				continue
			}
			edges[candidateIndex] = append(edges[candidateIndex], index)
		}
	}
	matchOriginal := make([]int, len(original))
	matchCandidate := make([]int, len(candidates))
	for index := range matchOriginal {
		matchOriginal[index] = -1
	}
	for index := range matchCandidate {
		matchCandidate[index] = -1
	}
	seenCandidate := make([]int, len(candidates))
	seenOriginal := make([]int, len(original))
	previous := make([]int, len(original))
	queue := make([]int, 0, len(candidates))
	for start := range candidates {
		stamp := start + 1
		queue = append(queue[:0], start)
		seenCandidate[start] = stamp
		free := -1
		for len(queue) > 0 && free < 0 {
			candidate := queue[0]
			queue = queue[1:]
			for _, baseline := range edges[candidate] {
				if err := spendSuppressionWork(analysisBudget, 1); err != nil {
					return nil, err
				}
				work++
				if work > maxSuppressionMatchWork {
					return nil, errSuppressionResourceLimit
				}
				if seenOriginal[baseline] == stamp {
					continue
				}
				seenOriginal[baseline] = stamp
				previous[baseline] = candidate
				if matchOriginal[baseline] < 0 {
					free = baseline
					break
				}
				next := matchOriginal[baseline]
				if seenCandidate[next] != stamp {
					seenCandidate[next] = stamp
					queue = append(queue, next)
				}
			}
		}
		for free >= 0 {
			candidate := previous[free]
			prior := matchCandidate[candidate]
			matchCandidate[candidate] = free
			matchOriginal[free] = candidate
			free = prior
		}
	}
	result := make([]finding.Finding, 0)
	for index, candidate := range candidates {
		if err := spendSuppressionWork(analysisBudget, 1); err != nil {
			return nil, err
		}
		if matchCandidate[index] >= 0 {
			continue
		}
		result = append(result, integrityFinding(
			"togi/new-suppression", file, candidate.line, candidate.snippet,
			"new or moved suppression introduced during a fix attempt",
		))
		if len(result) > maxSuppressionFindings {
			return nil, errSuppressionResourceLimit
		}
	}
	return result, nil
}

func suppressionNarrows(attempted, original suppressionSite, work, analysisBudget *int) (bool, error) {
	if attempted.kind == "build-duplicate-legacy" && original.kind == attempted.kind {
		return constraintsEquivalent(attempted.constraint, original.constraint, work, analysisBudget)
	}
	if attempted.constraint != nil || original.constraint != nil {
		if attempted.constraint == nil || original.constraint == nil {
			return false, nil
		}
		return constraintImplies(attempted.constraint, original.constraint, work, analysisBudget)
	}
	if original.scope.all {
		return true, nil
	}
	if attempted.scope.all {
		return false, nil
	}
	baseline := make(map[string]struct{}, len(original.scope.rules))
	for _, rule := range original.scope.rules {
		baseline[rule] = struct{}{}
	}
	for _, rule := range attempted.scope.rules {
		if _, exists := baseline[rule]; !exists {
			return false, nil
		}
	}
	return true, nil
}

func constraintsEquivalent(left, right constraint.Expr, work, analysisBudget *int) (bool, error) {
	tags := make(map[string]struct{})
	collectConstraintTags(left, tags)
	collectConstraintTags(right, tags)
	if len(tags) > maxSuppressionConstraintTags {
		return false, errSuppressionResourceLimit
	}
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	evaluationWork := constraintNodeCount(left) + constraintNodeCount(right) + len(names)
	values := make(map[string]bool, len(names))
	for mask := 0; mask < 1<<len(names); mask++ {
		if err := spendSuppressionWork(analysisBudget, evaluationWork); err != nil {
			return false, err
		}
		if work != nil {
			if *work > maxSuppressionMatchWork-evaluationWork {
				return false, errSuppressionResourceLimit
			}
			*work += evaluationWork
		}
		for index, name := range names {
			values[name] = mask&(1<<index) != 0
		}
		if evalConstraint(left, values) != evalConstraint(right, values) {
			return false, nil
		}
	}
	return true, nil
}

func constraintImplies(left, right constraint.Expr, work, analysisBudget *int) (bool, error) {
	tags := make(map[string]struct{})
	collectConstraintTags(left, tags)
	collectConstraintTags(right, tags)
	if len(tags) > maxSuppressionConstraintTags {
		return false, errSuppressionResourceLimit
	}
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	evaluationWork := constraintNodeCount(left) + constraintNodeCount(right)
	for mask := 0; mask < 1<<len(names); mask++ {
		if err := spendSuppressionWork(analysisBudget, evaluationWork); err != nil {
			return false, err
		}
		if *work > maxSuppressionMatchWork-evaluationWork {
			return false, errSuppressionResourceLimit
		}
		*work += evaluationWork
		values := make(map[string]bool, len(names))
		for index, name := range names {
			values[name] = mask&(1<<index) != 0
		}
		if evalConstraint(left, values) && !evalConstraint(right, values) {
			return false, nil
		}
	}
	return true, nil
}

func constraintNodeCount(expression constraint.Expr) int {
	switch value := expression.(type) {
	case *constraint.NotExpr:
		return 1 + constraintNodeCount(value.X)
	case *constraint.AndExpr:
		return 1 + constraintNodeCount(value.X) + constraintNodeCount(value.Y)
	case *constraint.OrExpr:
		return 1 + constraintNodeCount(value.X) + constraintNodeCount(value.Y)
	default:
		return 1
	}
}

func collectConstraintTags(expression constraint.Expr, tags map[string]struct{}) {
	switch value := expression.(type) {
	case *constraint.TagExpr:
		tags[value.Tag] = struct{}{}
	case *constraint.NotExpr:
		collectConstraintTags(value.X, tags)
	case *constraint.AndExpr:
		collectConstraintTags(value.X, tags)
		collectConstraintTags(value.Y, tags)
	case *constraint.OrExpr:
		collectConstraintTags(value.X, tags)
		collectConstraintTags(value.Y, tags)
	}
}

func evalConstraint(expression constraint.Expr, values map[string]bool) bool {
	switch value := expression.(type) {
	case *constraint.TagExpr:
		return values[value.Tag]
	case *constraint.NotExpr:
		return !evalConstraint(value.X, values)
	case *constraint.AndExpr:
		return evalConstraint(value.X, values) && evalConstraint(value.Y, values)
	case *constraint.OrExpr:
		return evalConstraint(value.X, values) || evalConstraint(value.Y, values)
	default:
		return false
	}
}

func boundedSuppressionSnippet(declaration, mechanism, role string) string {
	return truncateSuppressionText(declaration + ": " + mechanism + " at " + role)
}

func truncateSuppressionText(value string) string {
	if len(value) <= 240 {
		return value
	}
	end := 0
	for index := range value {
		if index > 240 {
			break
		}
		end = index
	}
	if end == 0 {
		return "suppression"
	}
	return value[:end]
}

var _ types.Importer = (*snapshotImporter)(nil)
