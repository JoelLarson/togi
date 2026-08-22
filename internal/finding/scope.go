package finding

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// LineRange identifies an inclusive range of changed lines.
type LineRange struct {
	Start int
	End   int
}

// ChangedLines maps canonical, repository-relative file paths to changed lines.
type ChangedLines map[string][]LineRange

// FilterTouched keeps only finding locations that overlap changed lines.
// The returned findings are independent clones; filtering never changes identity.
func FilterTouched(findings []Finding, changed ChangedLines) ([]Finding, error) {
	changedPaths, err := validateChangedLines(changed)
	if err != nil {
		return nil, err
	}
	filtered := make([]Finding, 0, len(findings))
	for index, finding := range findings {
		if err := Validate(finding); err != nil {
			return nil, fmt.Errorf("finding %d: %w", index, err)
		}
		file := normalizeFile(finding.File)
		ranges := changedPaths[file]
		if len(ranges) == 0 {
			continue
		}

		locations := make([]Occurrence, 0, 1+len(finding.Occurrences))
		locations = append(locations, Occurrence{Line: finding.Line, EndLine: finding.EndLine})
		locations = append(locations, finding.Occurrences...)
		survivors := locations[:0]
		for _, location := range locations {
			if overlapsChanged(location, ranges) {
				survivors = append(survivors, location)
			}
		}
		if len(survivors) == 0 {
			continue
		}
		sort.SliceStable(survivors, func(left, right int) bool {
			return lessOccurrence(survivors[left], survivors[right])
		})

		copied := clone(finding)
		copied.File = file
		copied.Line = survivors[0].Line
		copied.EndLine = survivors[0].EndLine
		copied.Occurrences = append([]Occurrence(nil), survivors[1:]...)
		filtered = append(filtered, copied)
	}
	return filtered, nil
}

func validateChangedLines(changed ChangedLines) (map[string][]LineRange, error) {
	validated := make(map[string][]LineRange, len(changed))
	for path, ranges := range changed {
		canonical, err := validateChangedPath(path)
		if err != nil {
			return nil, err
		}
		for index, lineRange := range ranges {
			if lineRange.Start <= 0 || lineRange.End < lineRange.Start {
				return nil, fmt.Errorf("changed range %q[%d] is invalid: start=%d end=%d", path, index, lineRange.Start, lineRange.End)
			}
		}
		validated[canonical] = append(validated[canonical], ranges...)
	}
	return validated, nil
}

func validateChangedPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("changed path must not be empty")
	}
	portable := strings.ReplaceAll(path, `\`, "/")
	if filepath.IsAbs(path) || strings.HasPrefix(portable, "/") {
		return "", fmt.Errorf("changed path %q must be repository-relative", path)
	}
	for _, component := range strings.Split(portable, "/") {
		if component == ".." {
			return "", fmt.Errorf("changed path %q must not contain traversal", path)
		}
	}
	return normalizeFile(path), nil
}

func overlapsChanged(location Occurrence, changed []LineRange) bool {
	end := location.EndLine
	if end == 0 {
		end = location.Line
	}
	for _, lineRange := range changed {
		if location.Line <= lineRange.End && end >= lineRange.Start {
			return true
		}
	}
	return false
}
