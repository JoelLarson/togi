package finding

import (
	"fmt"
	"sort"
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
	changedPaths, err := validatedChangedLines(changed)
	if err != nil {
		return nil, err
	}
	filtered := make([]Finding, 0, len(findings))
	for index, finding := range findings {
		if err := Validate(finding); err != nil {
			return nil, fmt.Errorf("finding %d: %w", index, err)
		}
		file, err := ParsePath(finding.File)
		if err != nil {
			return nil, fmt.Errorf("finding %d: %w", index, err)
		}
		ranges := changedPaths[file.String()]
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
		copied.File = file.String()
		copied.Line = survivors[0].Line
		copied.EndLine = survivors[0].EndLine
		copied.Occurrences = append([]Occurrence(nil), survivors[1:]...)
		filtered = append(filtered, copied)
	}
	return filtered, nil
}

// ValidateChangedLines verifies that changed-line paths and ranges are safe for scope filtering.
func ValidateChangedLines(changed ChangedLines) error {
	_, err := validatedChangedLines(changed)
	return err
}

func validatedChangedLines(changed ChangedLines) (map[string][]LineRange, error) {
	validated := make(map[string][]LineRange, len(changed))
	for path, ranges := range changed {
		canonical, err := ParsePath(path)
		if err != nil {
			return nil, err
		}
		for index, lineRange := range ranges {
			if lineRange.Start <= 0 || lineRange.End < lineRange.Start {
				return nil, fmt.Errorf("changed range %q[%d] is invalid: start=%d end=%d", path, index, lineRange.Start, lineRange.End)
			}
		}
		validated[canonical.String()] = append(validated[canonical.String()], ranges...)
	}
	return validated, nil
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
