package finding

import (
	"fmt"
	"sort"
)

// Group combines findings with the same fingerprint and orders them for reports.
func Group(findings []Finding) ([]Finding, error) {
	groups := make(map[string][]Finding, len(findings))
	for index, finding := range findings {
		if err := Validate(finding); err != nil {
			return nil, fmt.Errorf("finding %d: %w", index, err)
		}
		copied := clone(finding)
		copied.File = normalizeFile(copied.File)
		copied.Fingerprint = Fingerprint(copied)
		groups[copied.Fingerprint] = append(groups[copied.Fingerprint], copied)
	}

	grouped := make([]Finding, 0, len(groups))
	for _, members := range groups {
		grouped = append(grouped, combine(members))
	}
	sort.Slice(grouped, func(left, right int) bool {
		return lessFinding(grouped[left], grouped[right])
	})
	return grouped, nil
}

func clone(finding Finding) Finding {
	finding.Occurrences = append([]Occurrence(nil), finding.Occurrences...)
	return finding
}

type locatedFinding struct {
	findingIndex int
	location     Occurrence
}

func combine(findings []Finding) Finding {
	sort.Slice(findings, func(left, right int) bool {
		return lessMetadata(findings[left], findings[right])
	})

	locations := make([]locatedFinding, 0)
	seen := make(map[Occurrence]struct{})
	for findingIndex, finding := range findings {
		locations = appendLocation(locations, seen, findingIndex, Occurrence{Line: finding.Line, EndLine: finding.EndLine})
		for _, occurrence := range finding.Occurrences {
			locations = appendLocation(locations, seen, findingIndex, occurrence)
		}
	}
	sort.SliceStable(locations, func(left, right int) bool {
		return lessOccurrence(locations[left].location, locations[right].location)
	})

	primary := locations[0]
	combined := clone(findings[primary.findingIndex])
	combined.Line = primary.location.Line
	combined.EndLine = primary.location.EndLine
	combined.Occurrences = make([]Occurrence, 0, len(locations)-1)
	for _, located := range locations[1:] {
		combined.Occurrences = append(combined.Occurrences, located.location)
	}
	return combined
}

func lessMetadata(left, right Finding) bool {
	if left.Language != right.Language {
		return left.Language < right.Language
	}
	if left.Severity != right.Severity {
		return left.Severity < right.Severity
	}
	if left.Snippet != right.Snippet {
		return left.Snippet < right.Snippet
	}
	if left.Message != right.Message {
		return left.Message < right.Message
	}
	return false
}

func appendLocation(locations []locatedFinding, seen map[Occurrence]struct{}, findingIndex int, location Occurrence) []locatedFinding {
	if _, exists := seen[location]; exists {
		return locations
	}
	seen[location] = struct{}{}
	return append(locations, locatedFinding{findingIndex: findingIndex, location: location})
}

func lessOccurrence(left, right Occurrence) bool {
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	return left.EndLine < right.EndLine
}

func lessFinding(left, right Finding) bool {
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	if left.Gate != right.Gate {
		return left.Gate < right.Gate
	}
	if left.RuleID != right.RuleID {
		return left.RuleID < right.RuleID
	}
	return left.Fingerprint < right.Fingerprint
}
