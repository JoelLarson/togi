package harness

import (
	"slices"
	"testing"
)

func TestSelectDrivers(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{"", []string{"service"}},
		{"service", []string{"service"}},
		{"cli", []string{"cli"}},
		{"all", []string{"service", "cli"}},
	} {
		got, err := selectDriverNames(tc.value)
		if err != nil {
			t.Fatalf("selectDriverNames(%q): %v", tc.value, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("selectDriverNames(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestSelectDriversRejectsUnknownValue(t *testing.T) {
	if _, err := selectDriverNames("unknown"); err == nil {
		t.Fatal("selectDriverNames() error = nil, want invalid selection error")
	}
}
