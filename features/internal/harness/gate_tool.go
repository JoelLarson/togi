package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// ToolBehavior describes the observable behavior of a scenario-owned gate tool.
type ToolBehavior struct {
	Stdout        []byte
	Stderr        []byte
	ExitCode      int
	Delay         time.Duration
	VersionStdout []byte
	VersionStderr []byte
	VersionExit   int
	WaitFor       string
	StartedMarker string
	InvokedMarker string
}

// InstallTool installs an executable fixture into the scenario PATH.
func (e *Environment) InstallTool(name string, behavior ToolBehavior) (string, error) {
	if runtime.GOOS == "windows" {
		return "", ErrUnsupportedCapability
	}
	if err := validateFixtureName("tool", name); err != nil {
		return "", err
	}
	if behavior.ExitCode < 0 || behavior.VersionExit < 0 {
		return "", errors.New("tool exit codes must not be negative")
	}
	if behavior.Delay < 0 {
		return "", errors.New("tool delay must not be negative")
	}

	path := filepath.Join(e.BinRoot, name)
	payloads := []struct {
		name string
		data []byte
	}{
		{name: path + ".stdout", data: behavior.Stdout},
		{name: path + ".stderr", data: behavior.Stderr},
		{name: path + ".version.stdout", data: behavior.VersionStdout},
		{name: path + ".version.stderr", data: behavior.VersionStderr},
	}
	for _, payload := range payloads {
		if err := os.WriteFile(payload.name, payload.data, 0o600); err != nil {
			return "", fmt.Errorf("write tool payload %q: %w", payload.name, err)
		}
	}

	script := toolScript(path, behavior)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write tool %q: %w", name, err)
	}
	return path, nil
}

func toolScript(path string, behavior ToolBehavior) string {
	version := "case \"${1-}\" in version|--version) mode=version ;; *) mode=run ;; esac\n"
	markers := markerScript("started", behavior.StartedMarker) + markerScript("invoked", behavior.InvokedMarker)
	wait := ""
	if behavior.WaitFor != "" {
		wait = "while [ -e " + shellQuote(behavior.WaitFor) + " ]; do sleep 0.01; done\n"
	}
	delay := ""
	if behavior.Delay > 0 {
		delay = "sleep " + strconv.FormatFloat(behavior.Delay.Seconds(), 'f', -1, 64) + "\n"
	}
	return "#!/bin/sh\nset -eu\n" + version + markers + wait + delay +
		"if [ \"$mode\" = version ]; then\n" +
		"  cat " + shellQuote(path+".version.stdout") + "\n" +
		"  cat " + shellQuote(path+".version.stderr") + " >&2\n" +
		"  exit " + fmt.Sprint(behavior.VersionExit) + "\n" +
		"fi\n" +
		"cat " + shellQuote(path+".stdout") + "\n" +
		"cat " + shellQuote(path+".stderr") + " >&2\n" +
		"exit " + fmt.Sprint(behavior.ExitCode) + "\n"
}

func markerScript(name, marker string) string {
	if marker == "" {
		return ""
	}
	quoted := shellQuote(marker)
	return "marker=" + quoted + "\n" +
		"(umask 077; : > \"$marker.$$\")\n" +
		"mv -f \"$marker.$$\" \"$marker\"\n"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\\"'\\\"'") + "'"
}

// VersionDefinition configures a tool version probe in a gate binding.
type VersionDefinition struct {
	Command    []string
	Pattern    string
	Constraint string
}

// GateDefinition is the concise fixture form of one Go gate binding.
type GateDefinition struct {
	Name, Description, Tool, Normalizer, RuleID, Message string
	Scope                                                string
	Location                                             string
	Timeout                                              time.Duration
	Command                                              []string
	Version                                              *VersionDefinition
	Settings                                             map[string]any
	SeverityMap                                          map[string]string
	Aliases                                              map[string]string
}

// WriteGate writes a valid gate override in the format consumed by gate.Loader.
func (e *Environment) WriteGate(definition GateDefinition) error {
	if err := validateFixtureName("gate", definition.Name); err != nil {
		return err
	}
	if strings.TrimSpace(definition.Description) == "" || strings.TrimSpace(definition.Tool) == "" {
		return errors.New("gate description and tool are required")
	}
	if definition.Scope == "" {
		definition.Scope = "repo"
	}
	if definition.Location == "" {
		definition.Location = "point"
	}
	if definition.Timeout == 0 {
		definition.Timeout = 5 * time.Second
	}
	if definition.Timeout < 0 {
		return errors.New("gate timeout must be positive")
	}
	if definition.Normalizer == "" {
		definition.Normalizer = "golangci-json"
	}
	if definition.RuleID == "" {
		definition.RuleID = definition.Name
	}
	if definition.Message == "" {
		definition.Message = definition.Description
	}
	if len(definition.Command) == 0 {
		definition.Command = []string{definition.Tool}
	}
	if definition.SeverityMap == nil {
		definition.SeverityMap = map[string]string{"default": "warning"}
	}

	root := filepath.Join(e.Paths().GateOverrides(), definition.Name)
	if err := os.MkdirAll(filepath.Join(root, "go"), 0o700); err != nil {
		return fmt.Errorf("create gate fixture %q: %w", definition.Name, err)
	}
	manifest, err := toml.Marshal(gateManifestWire{
		Name: definition.Name, Description: definition.Description, CostClass: "fast",
		FixPolicy: "report-only", Scope: definition.Scope, Location: definition.Location,
		Blocking: []string{"error", "warning"}, Timeout: definition.Timeout.String(),
	})
	if err != nil {
		return fmt.Errorf("encode gate manifest: %w", err)
	}
	binding := gateBindingWire{
		Language: "go", Tool: definition.Tool, Command: definition.Command,
		SuccessExitCodes: []int{0}, Normalizer: definition.Normalizer, RuleID: definition.RuleID,
		Message: definition.Message, Settings: definition.Settings, SeverityMap: definition.SeverityMap,
		Aliases: definition.Aliases,
	}
	if definition.Version != nil {
		binding.Version = &gateVersionWire{
			Command: definition.Version.Command, Pattern: definition.Version.Pattern, Constraint: definition.Version.Constraint,
		}
	}
	encodedBinding, err := toml.Marshal(binding)
	if err != nil {
		return fmt.Errorf("encode gate binding: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "gate.toml"), manifest, 0o600); err != nil {
		return fmt.Errorf("write gate manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go", "binding.toml"), encodedBinding, 0o600); err != nil {
		return fmt.Errorf("write gate binding: %w", err)
	}
	return nil
}

// WriteInvalidGate writes a malformed manifest for loader failure scenarios.
func (e *Environment) WriteInvalidGate(name, contents string) error {
	if err := validateFixtureName("gate", name); err != nil {
		return err
	}
	root := filepath.Join(e.Paths().GateOverrides(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create invalid gate fixture: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "gate.toml"), []byte(contents), 0o600); err != nil {
		return fmt.Errorf("write invalid gate fixture: %w", err)
	}
	return nil
}

func validateFixtureName(kind, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("invalid %s name %q", kind, name)
	}
	return nil
}

type gateManifestWire struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	CostClass   string   `toml:"cost_class"`
	FixPolicy   string   `toml:"fix_policy"`
	Scope       string   `toml:"scope"`
	Location    string   `toml:"location"`
	Blocking    []string `toml:"blocking"`
	Timeout     string   `toml:"timeout"`
}

type gateBindingWire struct {
	Language         string            `toml:"language"`
	Tool             string            `toml:"tool"`
	Command          []string          `toml:"command"`
	SuccessExitCodes []int             `toml:"success_exit_codes"`
	Normalizer       string            `toml:"normalizer"`
	RuleID           string            `toml:"rule_id"`
	Message          string            `toml:"message"`
	Settings         map[string]any    `toml:"settings,omitempty"`
	SeverityMap      map[string]string `toml:"severity_map"`
	Version          *gateVersionWire  `toml:"version,omitempty"`
	Aliases          map[string]string `toml:"aliases,omitempty"`
}

type gateVersionWire struct {
	Command    []string `toml:"command"`
	Pattern    string   `toml:"pattern"`
	Constraint string   `toml:"constraint"`
}
