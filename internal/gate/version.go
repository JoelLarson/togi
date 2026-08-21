package gate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Observe extracts a semantic version from raw tool output and evaluates its
// configured constraint. An absent version block accepts all output.
func (v Version) Observe(raw string) (observed string, matches bool, err error) {
	if len(v.Command) == 0 && v.Pattern == "" && v.Constraint == "" {
		return "", true, nil
	}

	pattern, err := regexp.Compile(v.Pattern)
	if err != nil {
		return "", false, fmt.Errorf("invalid version pattern: %w", err)
	}
	if pattern.NumSubexp() < 1 {
		return "", false, fmt.Errorf("version pattern requires a capture group")
	}
	match := pattern.FindStringSubmatch(raw)
	if match == nil || match[1] == "" {
		return "", false, fmt.Errorf("could not extract version with pattern %q", v.Pattern)
	}
	observed = match[1]

	observedVersion, err := parseSemanticVersion(observed)
	if err != nil {
		return observed, false, fmt.Errorf("invalid observed version %q: %w", observed, err)
	}
	if !strings.HasPrefix(v.Constraint, ">=") {
		return observed, false, fmt.Errorf("unsupported version constraint %q: only >= is supported", v.Constraint)
	}
	minimumText := strings.TrimSpace(strings.TrimPrefix(v.Constraint, ">="))
	minimum, err := parseSemanticVersion(minimumText)
	if err != nil {
		return observed, false, fmt.Errorf("invalid constraint version %q: %w", minimumText, err)
	}

	return observed, compareSemanticVersions(observedVersion, minimum) >= 0, nil
}

type semanticVersion [3]uint64

func parseSemanticVersion(value string) (semanticVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, fmt.Errorf("want major.minor.patch")
	}
	var version semanticVersion
	for index, part := range parts {
		if part == "" {
			return semanticVersion{}, fmt.Errorf("empty component")
		}
		if len(part) > 1 && part[0] == '0' {
			return semanticVersion{}, fmt.Errorf("component %q has a leading zero", part)
		}
		component, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, fmt.Errorf("component %q: %w", part, err)
		}
		version[index] = component
	}
	return version, nil
}

func compareSemanticVersions(left, right semanticVersion) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}
