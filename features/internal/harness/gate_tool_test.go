package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/joellarson/togi/internal/gate"
)

func TestInstallToolEmitsConfiguredBytesAndExitCode(t *testing.T) {
	requireUnixTool(t)
	environment := newToolEnvironment(t)
	path, err := environment.InstallTool("fake-lint", ToolBehavior{
		Stdout:   []byte("finding\x00\n"),
		Stderr:   []byte("warning\n"),
		ExitCode: 7,
	})
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(path)
	command.Env = environment.Environ()
	stdout, err := command.Output()
	if !bytes.Equal(stdout, []byte("finding\x00\n")) {
		t.Fatalf("stdout = %q", stdout)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("error = %v, want exit 7", err)
	}
	if got := exitError.Stderr; !bytes.Equal(got, []byte("warning\n")) {
		t.Fatalf("stderr = %q", got)
	}
}

func TestInstallToolHandlesVersionAndInvocationMarker(t *testing.T) {
	requireUnixTool(t)
	environment := newToolEnvironment(t)
	marker := filepath.Join(environment.TempRoot, "invoked")
	path, err := environment.InstallTool("fake-version", ToolBehavior{
		Stdout:        []byte("run\n"),
		VersionStdout: []byte("fake-version 1.2.3\n"),
		VersionStderr: []byte("version-note\n"),
		VersionExit:   3,
		InvokedMarker: marker,
	})
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(path, "--version")
	command.Env = environment.Environ()
	stdout, err := command.Output()
	if !bytes.Equal(stdout, []byte("fake-version 1.2.3\n")) {
		t.Fatalf("stdout = %q", stdout)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 3 {
		t.Fatalf("error = %v, want version exit 3", err)
	}
	if got := exitError.Stderr; !bytes.Equal(got, []byte("version-note\n")) {
		t.Fatalf("stderr = %q", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("invocation marker: %v", err)
	}
}

func TestInstallToolWaitsForReleaseAndDelays(t *testing.T) {
	requireUnixTool(t)
	environment := newToolEnvironment(t)
	waitFor := filepath.Join(environment.TempRoot, "hold")
	started := filepath.Join(environment.TempRoot, "started")
	if err := os.WriteFile(waitFor, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := environment.InstallTool("fake-wait", ToolBehavior{
		WaitFor:       waitFor,
		StartedMarker: started,
		Delay:         75 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	command := exec.CommandContext(context.Background(), path)
	command.Env = environment.Environ()
	finished := make(chan error, 1)
	startedAt := time.Now()
	go func() { finished <- command.Run() }()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tool did not create started marker")
		}
	}
	select {
	case err := <-finished:
		t.Fatalf("tool completed before release: %v", err)
	default:
	}
	if err := os.Remove(waitFor); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed < 75*time.Millisecond {
		t.Fatalf("elapsed = %v, want at least %v", elapsed, 75*time.Millisecond)
	}
}

func TestWriteGateRoundTripsAbsoluteAndNamedCommands(t *testing.T) {
	environment := newToolEnvironment(t)
	absoluteTool := filepath.Join(environment.BinRoot, "absolute-tool")
	definitions := []GateDefinition{
		{
			Name: "absolute", Description: "absolute command", Tool: absoluteTool,
			Command: []string{absoluteTool, "--machine"}, RuleID: "absolute-rule", Message: "absolute message",
			SeverityMap: map[string]string{"default": "warning"},
		},
		{
			Name: "named", Description: "named command", Tool: "named-tool",
			Command: []string{"named-tool", "--machine"}, RuleID: "named-rule", Message: "named message",
			Version:     &VersionDefinition{Command: []string{"named-tool", "version"}, Pattern: "([0-9]+\\.[0-9]+\\.[0-9]+)", Constraint: ">=1.0.0"},
			SeverityMap: map[string]string{"default": "warning"},
		},
	}
	for _, definition := range definitions {
		if err := environment.WriteGate(definition); err != nil {
			t.Fatal(err)
		}
	}

	loader := gate.Loader{OverrideDir: filepath.Join(environment.ConfigRoot, "gates")}
	for _, definition := range definitions {
		loaded, err := loader.Load(definition.Name)
		if err != nil {
			t.Fatalf("load %q: %v", definition.Name, err)
		}
		binding := loaded.Bindings["go"]
		if !slices.Equal(binding.Command, definition.Command) {
			t.Fatalf("%s command = %q, want %q", definition.Name, binding.Command, definition.Command)
		}
		if binding.Tool != definition.Tool {
			t.Fatalf("%s tool = %q, want %q", definition.Name, binding.Tool, definition.Tool)
		}
	}
}

func TestWriteGateOverridesShippedGateAndAddsOverrideOnlyGate(t *testing.T) {
	environment := newToolEnvironment(t)
	for _, definition := range []GateDefinition{
		{Name: "lint", Description: "replacement lint", Tool: "replacement", Command: []string{"replacement"}, RuleID: "replacement", Message: "replacement", SeverityMap: map[string]string{"default": "warning"}},
		{Name: "scenario-only", Description: "scenario gate", Tool: "scenario-tool", Command: []string{"scenario-tool"}, RuleID: "scenario", Message: "scenario", SeverityMap: map[string]string{"default": "warning"}},
	} {
		if err := environment.WriteGate(definition); err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := (gate.Loader{OverrideDir: filepath.Join(environment.ConfigRoot, "gates")}).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(loaded))
	for _, candidate := range loaded {
		names = append(names, candidate.Manifest.Name)
		if candidate.Manifest.Name == "lint" && candidate.Bindings["go"].Tool != "replacement" {
			t.Fatalf("lint override tool = %q", candidate.Bindings["go"].Tool)
		}
	}
	if !slices.Contains(names, "scenario-only") {
		t.Fatalf("loaded names = %q, want scenario-only", names)
	}
}

func TestWriteInvalidGateWritesRawContents(t *testing.T) {
	environment := newToolEnvironment(t)
	if err := environment.WriteInvalidGate("broken", "not = [valid"); err != nil {
		t.Fatal(err)
	}
	_, err := (gate.Loader{OverrideDir: filepath.Join(environment.ConfigRoot, "gates")}).Load("broken")
	if err == nil {
		t.Fatal("Load(broken) succeeded")
	}
}

func newToolEnvironment(t *testing.T) *Environment {
	t.Helper()
	environment, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Close() })
	return environment
}

func requireUnixTool(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("executable gate fixtures require a POSIX shell")
	}
}
