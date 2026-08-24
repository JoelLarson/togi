//go:build !linux

package harness

import (
	"errors"
	"testing"
)

func TestInstallAgentReportsUnsupportedPlatform(t *testing.T) {
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	if _, err := environment.InstallAgent("codex", AgentBehavior{}); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("InstallAgent error = %v, want ErrUnsupportedCapability", err)
	}
}
