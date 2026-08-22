package finding

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Path is a validated, canonical, slash-separated repository-relative path —
// the one definition of what may appear in Finding.File, ChangedLines keys,
// and diff output. The zero Path is invalid; the constructors own every
// rule: no absolute paths (POSIX, Windows drive, or backslash-rooted forms),
// no traversal, nothing blank.
type Path struct {
	value string
}

// ParsePath mints a Path from input that is already expected to be
// repository-relative: git output, changed-line keys, persisted findings.
// Input is canonicalized (cleaned and slash-separated); callers that require
// input to arrive canonical compare String against the original.
func ParsePath(path string) (Path, error) {
	if strings.TrimSpace(path) == "" {
		return Path{}, errors.New("repository path must not be empty")
	}
	portable := strings.ReplaceAll(path, `\`, "/")
	if filepath.IsAbs(path) || strings.HasPrefix(portable, "/") || isWindowsAbsolute(portable) {
		return Path{}, fmt.Errorf("repository path %q must be repository-relative", path)
	}
	for _, component := range strings.Split(portable, "/") {
		if component == ".." {
			return Path{}, fmt.Errorf("repository path %q must not contain traversal", path)
		}
	}
	return Path{value: normalizeFile(path)}, nil
}

// NewPath mints a Path from tool-reported output rooted at the canonical
// absolute repository root: absolute paths inside the root are rewritten
// relative, anything outside the root is rejected.
func NewPath(root, reported string) (Path, error) {
	if strings.TrimSpace(reported) == "" {
		return Path{}, errors.New("repository path must not be empty")
	}
	clean := filepath.Clean(reported)
	if filepath.IsAbs(clean) {
		relative, err := filepath.Rel(root, clean)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return Path{}, errors.New("path is outside repository root")
		}
		clean = relative
	}
	parsed, err := ParsePath(filepath.ToSlash(clean))
	if err != nil {
		return Path{}, errors.New("path is outside repository root")
	}
	return parsed, nil
}

func (p Path) String() string { return p.value }
func (p Path) IsZero() bool   { return p.value == "" }

func isWindowsAbsolute(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) && path[1] == ':' && path[2] == '/'
}
