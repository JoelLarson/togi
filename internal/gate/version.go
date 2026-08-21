package gate

import (
	"fmt"
	"regexp"
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
	constraints, err := parseConstraint(v.Constraint)
	if err != nil {
		return observed, false, err
	}
	for _, constraint := range constraints {
		comparison := compareSemanticVersions(observedVersion, constraint.version)
		switch constraint.operator {
		case ">=":
			if comparison < 0 {
				return observed, false, nil
			}
		case "<":
			if comparison >= 0 {
				return observed, false, nil
			}
		}
	}
	return observed, true, nil
}

type semanticVersion struct {
	core       [3]string
	prerelease []prereleaseIdentifier
}

type prereleaseIdentifier struct {
	value   string
	numeric bool
}

func parseSemanticVersion(value string) (semanticVersion, error) {
	versionText, build, hasBuild := strings.Cut(value, "+")
	if hasBuild {
		if strings.Contains(build, "+") || !validIdentifiers(build, false) {
			return semanticVersion{}, fmt.Errorf("invalid build metadata")
		}
	}

	coreText, prereleaseText, hasPrerelease := strings.Cut(versionText, "-")
	coreParts := strings.Split(coreText, ".")
	if len(coreParts) != 3 {
		return semanticVersion{}, fmt.Errorf("want major.minor.patch")
	}

	var version semanticVersion
	for index, part := range coreParts {
		if !validNumericIdentifier(part) {
			return semanticVersion{}, fmt.Errorf("invalid core component %q", part)
		}
		version.core[index] = part
	}
	if hasPrerelease {
		if !validIdentifiers(prereleaseText, true) {
			return semanticVersion{}, fmt.Errorf("invalid prerelease %q", prereleaseText)
		}
		for _, identifier := range strings.Split(prereleaseText, ".") {
			version.prerelease = append(version.prerelease, prereleaseIdentifier{
				value:   identifier,
				numeric: allDigits(identifier),
			})
		}
	}
	return version, nil
}

func validNumericIdentifier(identifier string) bool {
	return identifier != "" && allDigits(identifier) && (len(identifier) == 1 || identifier[0] != '0')
}

func validIdentifiers(value string, prerelease bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for _, character := range identifier {
			if !asciiLetterOrDigit(character) && character != '-' {
				return false
			}
		}
		if prerelease && allDigits(identifier) && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func asciiLetterOrDigit(character rune) bool {
	return character >= '0' && character <= '9' ||
		character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z'
}

func compareSemanticVersions(left, right semanticVersion) int {
	for index := range left.core {
		if comparison := compareNumericIdentifiers(left.core[index], right.core[index]); comparison != 0 {
			return comparison
		}
	}

	switch {
	case len(left.prerelease) == 0 && len(right.prerelease) == 0:
		return 0
	case len(left.prerelease) == 0:
		return 1
	case len(right.prerelease) == 0:
		return -1
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		leftIdentifier := left.prerelease[index]
		rightIdentifier := right.prerelease[index]
		switch {
		case leftIdentifier.numeric && rightIdentifier.numeric:
			if comparison := compareNumericIdentifiers(leftIdentifier.value, rightIdentifier.value); comparison != 0 {
				return comparison
			}
		case leftIdentifier.numeric:
			return -1
		case rightIdentifier.numeric:
			return 1
		case leftIdentifier.value < rightIdentifier.value:
			return -1
		case leftIdentifier.value > rightIdentifier.value:
			return 1
		}
	}
	switch {
	case len(left.prerelease) < len(right.prerelease):
		return -1
	case len(left.prerelease) > len(right.prerelease):
		return 1
	default:
		return 0
	}
}

func compareNumericIdentifiers(left, right string) int {
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

type versionConstraint struct {
	operator string
	version  semanticVersion
}

func parseConstraint(value string) ([]versionConstraint, error) {
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return nil, fmt.Errorf("version constraint is required")
	}

	constraints := make([]versionConstraint, 0, len(parts))
	for _, part := range parts {
		operator := ""
		versionText := ""
		switch {
		case strings.HasPrefix(part, ">="):
			operator, versionText = ">=", strings.TrimPrefix(part, ">=")
		case strings.HasPrefix(part, "<") && !strings.HasPrefix(part, "<="):
			operator, versionText = "<", strings.TrimPrefix(part, "<")
		default:
			return nil, fmt.Errorf("unsupported version constraint %q: only >= and < comparisons are supported", part)
		}
		if versionText == "" {
			return nil, fmt.Errorf("malformed version constraint %q", part)
		}
		parsed, err := parseSemanticVersion(versionText)
		if err != nil {
			return nil, fmt.Errorf("invalid constraint version %q: %w", versionText, err)
		}
		constraints = append(constraints, versionConstraint{operator: operator, version: parsed})
	}
	return constraints, nil
}
