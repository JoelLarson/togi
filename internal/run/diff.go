package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/joellarson/togi/internal/finding"
)

const (
	gitReferenceOutputLimit = 4 << 10
	gitStatusOutputLimit    = 1 << 20
	gitPathsOutputLimit     = 8 << 20
	gitDiffOutputLimit      = 32 << 20
)

var diffHunkHeader = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@[^\r\n]*\r?$`)

// Diff describes the committed feature diff resolved for a run.
type Diff struct {
	BaseRef      string
	BaseCommit   string
	MergeBase    string
	Head         string
	ChangedFiles int
	ChangedLines int
	Lines        finding.ChangedLines
}

func resolveDiff(ctx context.Context, root, requestedBase string) (Diff, error) {
	if ctx == nil {
		return Diff{}, errors.New("diff context is required")
	}
	if root == "" {
		return Diff{}, errors.New("repository root is required")
	}
	if err := ctx.Err(); err != nil {
		return Diff{}, fmt.Errorf("resolve committed diff: %w", err)
	}
	canonicalRoot, err := canonicalDiffRoot(root)
	if err != nil {
		return Diff{}, err
	}

	status, err := diffGitOutput(ctx, canonicalRoot, gitStatusOutputLimit,
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return Diff{}, fmt.Errorf("inspect repository cleanliness: %w", err)
	}
	if len(status) != 0 {
		return Diff{}, errors.New("worktree must be clean before resolving diff scope")
	}

	head, err := resolveDiffCommit(ctx, canonicalRoot, "HEAD")
	if err != nil {
		return Diff{}, fmt.Errorf("resolve HEAD commit: %w", err)
	}

	baseRef := requestedBase
	if baseRef == "" {
		output, refErr := diffGitOutput(ctx, canonicalRoot, gitReferenceOutputLimit,
			"symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
		if refErr != nil {
			if errors.Is(refErr, context.Canceled) || errors.Is(refErr, context.DeadlineExceeded) {
				return Diff{}, fmt.Errorf("detect origin default branch: %w", refErr)
			}
			return Diff{}, errors.New("no base was provided and origin/HEAD is unavailable; pass --base")
		}
		baseRef, err = parseDiffRef(output)
		if err != nil {
			return Diff{}, errors.New("origin/HEAD is invalid; pass --base")
		}
	} else if err := validateRequestedBase(baseRef); err != nil {
		return Diff{}, err
	}

	baseCommit, err := resolveDiffCommit(ctx, canonicalRoot, baseRef)
	if err != nil {
		return Diff{}, fmt.Errorf("resolve base commit: %w", err)
	}
	mergeOutput, err := diffGitOutput(ctx, canonicalRoot, gitReferenceOutputLimit,
		"merge-base", head, baseCommit)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Diff{}, fmt.Errorf("compute merge base: %w", err)
		}
		return Diff{}, errors.New("base and HEAD have unrelated histories")
	}
	mergeBase, err := parseObjectID(mergeOutput)
	if err != nil {
		return Diff{}, fmt.Errorf("parse merge-base commit: %w", err)
	}

	pathOutput, err := diffGitOutput(ctx, canonicalRoot, gitPathsOutputLimit,
		"diff", "--name-status", "--diff-filter=ACMRTUXB", "--no-ext-diff", "--find-renames", "-z", mergeBase, head, "--")
	if err != nil {
		return Diff{}, fmt.Errorf("list changed paths: %w", err)
	}
	paths, err := parseChangedPaths(canonicalRoot, pathOutput)
	if err != nil {
		return Diff{}, fmt.Errorf("parse changed paths: %w", err)
	}

	lines := make(finding.ChangedLines, len(paths))
	changedLineCount := 0
	for _, path := range paths {
		args := []string{"diff", "--unified=0", "--inter-hunk-context=0", "--no-color", "--no-ext-diff", "--find-renames", mergeBase, head, "--"}
		if path.previous != "" {
			args = append(args, literalPathspec(path.previous))
		}
		args = append(args, literalPathspec(path.current))
		patch, err := diffGitOutput(ctx, canonicalRoot, gitDiffOutputLimit, args...)
		if err != nil {
			return Diff{}, fmt.Errorf("read changed-line ranges: %w", err)
		}
		ranges, err := parseDiffHunks(ctx, canonicalRoot, path.current, patch)
		if err != nil {
			return Diff{}, fmt.Errorf("parse changed-line ranges: %w", err)
		}
		lines[path.current] = ranges
		for _, lineRange := range ranges {
			changedLineCount += lineRange.End - lineRange.Start + 1
		}
	}
	if err := ctx.Err(); err != nil {
		return Diff{}, fmt.Errorf("resolve committed diff: %w", err)
	}

	return Diff{
		BaseRef:      baseRef,
		BaseCommit:   baseCommit,
		MergeBase:    mergeBase,
		Head:         head,
		ChangedFiles: len(paths),
		ChangedLines: changedLineCount,
		Lines:        lines,
	}, nil
}

type changedPath struct {
	previous string
	current  string
}

func canonicalDiffRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("repository root cannot be resolved")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("repository root cannot be resolved")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("repository root must be a directory")
	}
	return canonical, nil
}

func resolveDiffCommit(ctx context.Context, root, ref string) (string, error) {
	output, err := diffGitOutput(ctx, root, gitReferenceOutputLimit,
		"rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", errors.New("selected revision is not a commit")
	}
	return parseObjectID(output)
}

func parseObjectID(output []byte) (string, error) {
	value, err := parseSingleGitLine(output)
	if err != nil || (len(value) != 40 && len(value) != 64) {
		return "", errors.New("Git returned an invalid object ID")
	}
	for _, char := range value {
		if char < '0' || (char > '9' && char < 'a') || char > 'f' {
			return "", errors.New("Git returned an invalid object ID")
		}
	}
	return value, nil
}

func parseDiffRef(output []byte) (string, error) {
	value, err := parseSingleGitLine(output)
	if err != nil {
		return "", err
	}
	if err := validateRequestedBase(value); err != nil {
		return "", err
	}
	return value, nil
}

func parseSingleGitLine(output []byte) (string, error) {
	output = bytes.TrimSuffix(output, []byte{'\n'})
	if len(output) == 0 || bytes.ContainsAny(output, "\r\n\x00") || !utf8.Valid(output) {
		return "", errors.New("Git returned malformed output")
	}
	return string(output), nil
}

func validateRequestedBase(base string) error {
	if strings.TrimSpace(base) == "" || strings.TrimSpace(base) != base || strings.HasPrefix(base, "-") || !utf8.ValidString(base) {
		return errors.New("base revision is invalid")
	}
	for _, char := range base {
		if char < ' ' || char == 0x7f {
			return errors.New("base revision is invalid")
		}
	}
	return nil
}

func parseChangedPaths(root string, output []byte) ([]changedPath, error) {
	if len(output) == 0 {
		return []changedPath{}, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("Git returned unterminated changed-path output")
	}
	records := bytes.Split(output[:len(output)-1], []byte{0})
	paths := make([]changedPath, 0, len(records)/2)
	seen := make(map[string]struct{}, len(records))
	for index := 0; index < len(records); {
		status := string(records[index])
		index++
		pathCount, err := changedPathCount(status)
		if err != nil || index+pathCount > len(records) {
			return nil, errors.New("Git returned malformed changed-path status")
		}
		names := make([]string, pathCount)
		for nameIndex := range names {
			record := records[index+nameIndex]
			if len(record) == 0 || !utf8.Valid(record) {
				return nil, errors.New("Git returned an invalid changed path")
			}
			names[nameIndex] = string(record)
			if err := validateDiffPath(root, names[nameIndex]); err != nil {
				return nil, err
			}
		}
		index += pathCount
		path := changedPath{current: names[len(names)-1]}
		if len(names) == 2 {
			path.previous = names[0]
		}
		if _, exists := seen[path.current]; exists {
			return nil, errors.New("Git returned a duplicate changed path")
		}
		seen[path.current] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

func changedPathCount(status string) (int, error) {
	if len(status) == 1 && strings.ContainsRune("AMTUXB", rune(status[0])) {
		return 1, nil
	}
	if len(status) >= 2 && (status[0] == 'R' || status[0] == 'C') {
		score, err := strconv.Atoi(status[1:])
		if err == nil && score >= 0 && score <= 100 {
			return 2, nil
		}
	}
	return 0, errors.New("invalid changed-path status")
}

func literalPathspec(path string) string {
	return ":(literal)" + path
}

func validateDiffPath(root, path string) error {
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "/") || isWindowsDiffAbsolute(path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))) != path {
		return errors.New("Git returned an unsafe repository path")
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("Git returned an unsafe repository path")
		}
	}
	joined := filepath.Join(root, filepath.FromSlash(path))
	if !pathWithinRoot(root, joined) {
		return errors.New("Git returned an unsafe repository path")
	}
	return nil
}

func isWindowsDiffAbsolute(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) && path[1] == ':' && path[2] == '/'
}

func parseDiffHunks(ctx context.Context, root, path string, patch []byte) ([]finding.LineRange, error) {
	if len(patch) == 0 {
		return []finding.LineRange{}, nil
	}
	if !bytes.HasPrefix(patch, []byte("diff --git ")) || bytes.IndexByte(patch, 0) >= 0 {
		return nil, errors.New("Git returned malformed diff output")
	}
	ranges := make([]finding.LineRange, 0)
	for _, line := range bytes.Split(patch, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("@@")) {
			continue
		}
		match := diffHunkHeader.FindSubmatch(line)
		if match == nil {
			return nil, errors.New("Git returned a malformed hunk header")
		}
		start, err := strconv.Atoi(string(match[1]))
		if err != nil {
			return nil, errors.New("Git returned an invalid hunk start")
		}
		count := 1
		if len(match[2]) != 0 {
			count, err = strconv.Atoi(string(match[2]))
			if err != nil {
				return nil, errors.New("Git returned an invalid hunk count")
			}
		}
		if count == 0 {
			anchor, err := deletionAnchor(ctx, root, path, start)
			if err != nil {
				return nil, err
			}
			if anchor != 0 {
				ranges = append(ranges, finding.LineRange{Start: anchor, End: anchor})
			}
			continue
		}
		if start <= 0 || count < 0 || start > int(^uint(0)>>1)-count+1 {
			return nil, errors.New("Git returned an invalid changed-line range")
		}
		ranges = append(ranges, finding.LineRange{Start: start, End: start + count - 1})
	}
	return mergeLineRanges(ranges), nil
}

func mergeLineRanges(ranges []finding.LineRange) []finding.LineRange {
	if len(ranges) == 0 {
		return []finding.LineRange{}
	}
	merged := append([]finding.LineRange(nil), ranges...)
	slices.SortFunc(merged, func(left, right finding.LineRange) int {
		if left.Start != right.Start {
			return left.Start - right.Start
		}
		return left.End - right.End
	})
	write := 0
	for _, candidate := range merged[1:] {
		current := &merged[write]
		if candidate.Start <= current.End || (current.End < int(^uint(0)>>1) && candidate.Start == current.End+1) {
			current.End = max(current.End, candidate.End)
			continue
		}
		write++
		merged[write] = candidate
	}
	return merged[:write+1]
}

func deletionAnchor(ctx context.Context, root, path string, requested int) (int, error) {
	if ctx == nil {
		return 0, errors.New("deletion anchor context is required")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	currentPath, exists, err := safeCurrentPath(root, path)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	file, err := os.Open(currentPath)
	if err != nil {
		return 0, errors.New("read current repository file for deletion anchor")
	}
	defer file.Close()

	lineCount, err := countFileLines(ctx, file)
	if err != nil {
		return 0, fmt.Errorf("read current repository file for deletion anchor: %w", err)
	}
	if lineCount == 0 {
		return 0, nil
	}
	if requested > 0 && requested <= lineCount {
		return requested, nil
	}
	return lineCount, nil
}

func safeCurrentPath(root, path string) (string, bool, error) {
	canonicalRoot, err := canonicalDiffRoot(root)
	if err != nil {
		return "", false, err
	}
	if err := validateDiffPath(canonicalRoot, path); err != nil {
		return "", false, err
	}
	current := canonicalRoot
	components := strings.Split(filepath.FromSlash(path), string(filepath.Separator))
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return current, false, nil
		}
		if err != nil {
			return "", false, errors.New("inspect current repository file")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil || !pathWithinRoot(canonicalRoot, resolved) {
				return "", false, errors.New("current repository path escapes repository root")
			}
			current = resolved
		}
		if index < len(components)-1 && !isDirectoryPath(current) {
			return "", false, errors.New("current repository path is not a file")
		}
	}
	return current, true, nil
}

func isDirectoryPath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func countFileLines(ctx context.Context, reader io.Reader) (int, error) {
	buffer := make([]byte, 32<<10)
	lines := 0
	hasData := false
	last := byte('\n')
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		read, err := reader.Read(buffer)
		if read > 0 {
			hasData = true
			last = buffer[read-1]
			lines += bytes.Count(buffer[:read], []byte{'\n'})
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if hasData && last != '\n' {
		lines++
	}
	return lines, nil
}

func diffGitOutput(ctx context.Context, root string, limit int, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Env = diffGitEnvironment()
	stdout := newBoundedBuffer(limit, []byte("[output truncated]"))
	stderr := newBoundedBuffer(gitReferenceOutputLimit, []byte("[output truncated]"))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("Git command failed: %w", err)
	}
	if stdout.Truncated() || stderr.Truncated() {
		return nil, errors.New("Git command output exceeded its limit")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func diffGitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}
