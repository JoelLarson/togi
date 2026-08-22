package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	indexStart = "<!-- feature-index:start -->"
	indexEnd   = "<!-- feature-index:end -->"
)

var indexLine = regexp.MustCompile(`^- \[[^]]+\]\(([^)]+\.feature)\)$`)

func TestFeatureIndex(t *testing.T) {
	root, err := findModuleRoot(".")
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	acceptanceRoot := filepath.Join(root, "acceptance")
	readmePath := filepath.Join(acceptanceRoot, "README.md")

	readme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read acceptance catalog: %v", err)
	}

	indexed := parseFeatureIndex(t, string(readme))
	discovered := discoverFeatures(t, acceptanceRoot)
	compareFeatureSets(t, indexed, discovered)

	for feature := range discovered {
		testPath := strings.TrimSuffix(filepath.Join(acceptanceRoot, feature), ".feature") + "_test.go"
		if _, err := os.Stat(testPath); err != nil {
			t.Errorf("feature %q has no adjacent test %q: %v", feature, filepath.ToSlash(testPath), err)
		}
	}
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

func discoverFeatures(t *testing.T, acceptanceRoot string) map[string]struct{} {
	t.Helper()

	features := make(map[string]struct{})
	err := filepath.WalkDir(acceptanceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "internal" || entry.Name() == "testdata") {
			return filepath.SkipDir
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".feature" {
			return nil
		}

		relative, err := filepath.Rel(acceptanceRoot, path)
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
