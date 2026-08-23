package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

const (
	indexStart = "<!-- feature-index:start -->"
	indexEnd   = "<!-- feature-index:end -->"
)

var indexLine = regexp.MustCompile(`^- \[[^]]+\]\(([^)]+\.feature)\)$`)

var capabilityExclusions = map[string]map[string]string{
	"cli": {
		"@simulated-platform": "requires test-only platform selection",
	},
}

func TestFeatureIndex(t *testing.T) {
	root, err := findModuleRoot(".")
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	featuresRoot := filepath.Join(root, "features")
	readmePath := filepath.Join(featuresRoot, "README.md")

	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read acceptance catalog: %v", err)
	}

	indexed := parseFeatureIndex(t, string(readme))
	discovered := discoverFeatures(t, featuresRoot)
	compareFeatureSets(t, indexed, discovered)

	for feature := range discovered {
		testPath := strings.TrimSuffix(filepath.Join(featuresRoot, feature), ".feature") + "_test.go"
		if _, err := os.Stat(testPath); err != nil {
			t.Errorf("feature %q has no adjacent test %q: %v", feature, filepath.ToSlash(testPath), err)
		}
	}
}

func TestDriverCapabilityMatrix(t *testing.T) {
	root, err := findModuleRoot(".")
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(root, "features", "README.md"))
	if err != nil {
		t.Fatalf("read acceptance catalog: %v", err)
	}

	scenarioTags := indexedScenarioTags(t, root, parseFeatureIndex(t, string(readme)))
	for driver, exclusions := range capabilityExclusions {
		for tag, reason := range exclusions {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("driver %q excludes %q without a reason", driver, tag)
			}
			if !scenarioTags[tag] {
				t.Errorf("driver %q excludes %q, but no indexed scenario declares it", driver, tag)
			}
		}
	}

	for _, factory := range []DriverFactory{newServiceFactory(), newCLIFactory("togi")} {
		for _, tag := range excludedTags(factory.CapabilityTags()) {
			if tag == linuxPlatformTag || tag == unsupportedHostTag {
				continue
			}
			if _, declared := capabilityExclusions[factory.Name()][tag]; !declared {
				t.Errorf("driver %q excludes undeclared capability tag %q", factory.Name(), tag)
			}
		}
	}
}

func indexedScenarioTags(t *testing.T, root string, indexed map[string]struct{}) map[string]bool {
	t.Helper()

	features := mapsKeys(indexed)
	slices.Sort(features)
	tags := make(map[string]bool)
	for _, feature := range features {
		parsed, err := (godog.TestSuite{Options: &godog.Options{
			Paths: []string{filepath.Join(root, "features", feature)},
		}}).RetrieveFeatures()
		if err != nil {
			t.Fatalf("parse indexed feature %q: %v", feature, err)
		}
		if len(parsed) != 1 {
			t.Fatalf("parse indexed feature %q: got %d features, want 1", feature, len(parsed))
		}
		for _, pickle := range parsed[0].Pickles {
			for _, tag := range pickle.Tags {
				tags[tag.Name] = true
			}
		}
	}
	return tags
}

func mapsKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	return keys
}

var excludedTag = regexp.MustCompile(`~(@[[:alnum:]_-]+)`)

func excludedTags(expression string) []string {
	matches := excludedTag.FindAllStringSubmatch(expression, -1)
	tags := make([]string, 0, len(matches))
	for _, match := range matches {
		tags = append(tags, match[1])
	}
	return tags
}

func parseFeatureIndex(t *testing.T, readme string) map[string]struct{} {
	t.Helper()

	start, end := -1, -1
	for lineNumber, line := range strings.Split(readme, "\n") {
		switch line {
		case indexStart:
			if start >= 0 {
				t.Fatalf("feature index has more than one start marker")
			}
			start = lineNumber
		case indexEnd:
			if end >= 0 {
				t.Fatalf("feature index has more than one end marker")
			}
			end = lineNumber
		}
	}
	if start < 0 || end < 0 || start >= end {
		t.Fatalf("feature index must contain one ordered marker pair")
	}

	features := make(map[string]struct{})
	for _, line := range strings.Split(readme, "\n")[start+1 : end] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		match := indexLine.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("invalid feature index line %q", line)
		}
		feature := filepath.ToSlash(match[1])
		if _, exists := features[feature]; exists {
			t.Fatalf("duplicate feature index entry %q", feature)
		}
		features[feature] = struct{}{}
	}
	return features
}

func discoverFeatures(t *testing.T, featuresRoot string) map[string]struct{} {
	t.Helper()

	features := make(map[string]struct{})
	err := filepath.WalkDir(featuresRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "internal" || entry.Name() == "testdata") {
			return filepath.SkipDir
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".feature" {
			return nil
		}

		relative, err := filepath.Rel(featuresRoot, path)
		if err != nil {
			return err
		}
		feature := filepath.ToSlash(relative)
		if _, exists := features[feature]; exists {
			return fmt.Errorf("duplicate discovered feature %q", feature)
		}
		features[feature] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("discover acceptance features: %v", err)
	}
	return features
}

func compareFeatureSets(t *testing.T, indexed, discovered map[string]struct{}) {
	t.Helper()

	for feature := range indexed {
		if _, exists := discovered[feature]; !exists {
			t.Errorf("catalog indexes missing feature %q", feature)
		}
	}
	for feature := range discovered {
		if _, exists := indexed[feature]; !exists {
			t.Errorf("feature %q is missing from catalog", feature)
		}
	}
}
