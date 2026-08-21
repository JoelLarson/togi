package gate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/pelletier/go-toml/v2"
)

const shippedRoot = "defaults/gates"

// Loader loads shipped gates and optional wholesale overrides.
type Loader struct {
	OverrideDir string
}

// Load loads one gate by name.
func (l Loader) Load(name string) (Gate, error) {
	if err := validateComponent("gate name", name); err != nil {
		return Gate{}, err
	}

	if l.OverrideDir != "" {
		overridePath := filepath.Join(l.OverrideDir, name)
		info, err := os.Stat(overridePath)
		switch {
		case err == nil && !info.IsDir():
			return Gate{}, fmt.Errorf("gate override %q is not a directory", overridePath)
		case err == nil:
			return loadGate(os.DirFS(l.OverrideDir), name, name)
		case !errors.Is(err, os.ErrNotExist):
			return Gate{}, fmt.Errorf("inspect gate override %q: %w", overridePath, err)
		}
	}

	root := path.Join(shippedRoot, name)
	if _, err := fs.Stat(shipped, root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Gate{}, fmt.Errorf("gate %q does not exist", name)
		}
		return Gate{}, fmt.Errorf("inspect shipped gate %q: %w", name, err)
	}
	return loadGate(shipped, root, name)
}

// LoadAll loads all shipped and override-only gates in name order.
func (l Loader) LoadAll() ([]Gate, error) {
	names := make(map[string]struct{})
	entries, err := fs.ReadDir(shipped, shippedRoot)
	if err != nil {
		return nil, fmt.Errorf("read shipped gates: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			names[entry.Name()] = struct{}{}
		}
	}

	if l.OverrideDir != "" {
		entries, err := os.ReadDir(l.OverrideDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read gate override directory %q: %w", l.OverrideDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if err := validateComponent("override gate name", entry.Name()); err != nil {
					return nil, err
				}
				names[entry.Name()] = struct{}{}
			}
		}
	}

	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	gates := make([]Gate, 0, len(ordered))
	for _, name := range ordered {
		loaded, err := l.Load(name)
		if err != nil {
			return nil, err
		}
		gates = append(gates, loaded)
	}
	return gates, nil
}

type manifestWire struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	CostClass   string   `toml:"cost_class"`
	FixPolicy   string   `toml:"fix_policy"`
	Scope       string   `toml:"scope"`
	Blocking    []string `toml:"blocking"`
	Timeout     *string  `toml:"timeout"`
}

type bindingWire struct {
	Language         string            `toml:"language"`
	Tool             string            `toml:"tool"`
	Command          []string          `toml:"command"`
	SuccessExitCodes []int             `toml:"success_exit_codes"`
	Normalizer       string            `toml:"normalizer"`
	RuleID           string            `toml:"rule_id"`
	Message          string            `toml:"message"`
	Settings         map[string]any    `toml:"settings"`
	SeverityMap      map[string]string `toml:"severity_map"`
	Version          *versionWire      `toml:"version"`
	Aliases          map[string]string `toml:"aliases"`
}

type versionWire struct {
	Command    []string `toml:"command"`
	Pattern    string   `toml:"pattern"`
	Constraint string   `toml:"constraint"`
}

func loadGate(fsys fs.FS, root, requestedName string) (Gate, error) {
	manifestPath := path.Join(root, "gate.toml")
	var encoded manifestWire
	if err := decodeStrictFile(fsys, manifestPath, &encoded); err != nil {
		return Gate{}, err
	}
	manifest, err := encoded.toManifest(requestedName)
	if err != nil {
		return Gate{}, fmt.Errorf("%s: %w", manifestPath, err)
	}

	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return Gate{}, fmt.Errorf("read gate directory %s: %w", root, err)
	}
	bindings := make(map[string]Binding)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		language := entry.Name()
		if err := validateComponent("binding directory", language); err != nil {
			return Gate{}, fmt.Errorf("%s: %w", root, err)
		}
		bindingPath := path.Join(root, language, "binding.toml")
		var encoded bindingWire
		if err := decodeStrictFile(fsys, bindingPath, &encoded); err != nil {
			return Gate{}, err
		}
		binding, err := encoded.toBinding(language)
		if err != nil {
			return Gate{}, fmt.Errorf("%s: %w", bindingPath, err)
		}
		bindings[language] = binding
	}
	if len(bindings) == 0 {
		return Gate{}, fmt.Errorf("%s: at least one language binding is required", root)
	}

	return Gate{Manifest: manifest, Bindings: bindings}, nil
}

func decodeStrictFile(fsys fs.FS, filename string, destination any) error {
	file, err := fsys.Open(filename)
	if err != nil {
		return fmt.Errorf("%s: open: %w", filename, err)
	}
	defer file.Close()

	decoder := toml.NewDecoder(file).DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%s: decode TOML (unknown or malformed field): %w", filename, err)
	}
	return nil
}

func (wire manifestWire) toManifest(requestedName string) (Manifest, error) {
	if strings.TrimSpace(wire.Name) == "" {
		return Manifest{}, fmt.Errorf("manifest name is required")
	}
	if wire.Name != requestedName {
		return Manifest{}, fmt.Errorf("manifest name %q does not match directory %q", wire.Name, requestedName)
	}
	if strings.TrimSpace(wire.Description) == "" {
		return Manifest{}, fmt.Errorf("manifest description is required")
	}

	costClass := CostClass(wire.CostClass)
	if costClass.defaultTimeout() == 0 {
		return Manifest{}, fmt.Errorf("invalid cost class %q", wire.CostClass)
	}
	fixPolicy := FixPolicy(wire.FixPolicy)
	switch fixPolicy {
	case AutofixOnly, AutofixThenLLM, LLMFix, ReportOnly:
	default:
		return Manifest{}, fmt.Errorf("invalid fix policy %q", wire.FixPolicy)
	}
	scope := Scope(wire.Scope)
	switch scope {
	case Diff, Repo:
	default:
		return Manifest{}, fmt.Errorf("invalid scope %q", wire.Scope)
	}

	if len(wire.Blocking) == 0 {
		return Manifest{}, fmt.Errorf("at least one blocking severity is required")
	}
	blocking := make([]finding.Severity, len(wire.Blocking))
	seen := make(map[finding.Severity]struct{}, len(wire.Blocking))
	for index, value := range wire.Blocking {
		severity, err := canonicalSeverity(value)
		if err != nil {
			return Manifest{}, fmt.Errorf("blocking severity: %w", err)
		}
		if _, exists := seen[severity]; exists {
			return Manifest{}, fmt.Errorf("duplicate blocking severity %q", severity)
		}
		seen[severity] = struct{}{}
		blocking[index] = severity
	}

	timeout := costClass.defaultTimeout()
	if wire.Timeout != nil {
		parsed, err := time.ParseDuration(*wire.Timeout)
		if err != nil {
			return Manifest{}, fmt.Errorf("invalid timeout %q: %w", *wire.Timeout, err)
		}
		if parsed <= 0 {
			return Manifest{}, fmt.Errorf("timeout must be positive")
		}
		timeout = parsed
	}

	return Manifest{
		Name:        wire.Name,
		Description: wire.Description,
		CostClass:   costClass,
		FixPolicy:   fixPolicy,
		Scope:       scope,
		Blocking:    blocking,
		Timeout:     timeout,
	}, nil
}

func (wire bindingWire) toBinding(directoryLanguage string) (Binding, error) {
	if strings.TrimSpace(wire.Language) == "" {
		return Binding{}, fmt.Errorf("binding language is required")
	}
	if wire.Language != directoryLanguage {
		return Binding{}, fmt.Errorf("binding language %q does not match directory %q", wire.Language, directoryLanguage)
	}
	if strings.TrimSpace(wire.Tool) == "" {
		return Binding{}, fmt.Errorf("binding tool is required")
	}
	if len(wire.Command) == 0 {
		return Binding{}, fmt.Errorf("binding command is required")
	}
	for index, argument := range wire.Command {
		if argument == "" {
			return Binding{}, fmt.Errorf("binding command argument %d is empty", index)
		}
	}
	if len(wire.SuccessExitCodes) == 0 {
		return Binding{}, fmt.Errorf("at least one success exit code is required")
	}
	seenExits := make(map[int]struct{}, len(wire.SuccessExitCodes))
	for _, exitCode := range wire.SuccessExitCodes {
		if exitCode < 0 || exitCode > 255 {
			return Binding{}, fmt.Errorf("invalid success exit code %d", exitCode)
		}
		if _, exists := seenExits[exitCode]; exists {
			return Binding{}, fmt.Errorf("duplicate success exit code %d", exitCode)
		}
		seenExits[exitCode] = struct{}{}
	}
	if strings.TrimSpace(wire.Normalizer) == "" {
		return Binding{}, fmt.Errorf("binding normalizer is required")
	}
	if strings.HasPrefix(wire.Normalizer, "regex:") {
		pattern := strings.TrimPrefix(wire.Normalizer, "regex:")
		if pattern == "" {
			return Binding{}, fmt.Errorf("regex normalizer pattern is required")
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return Binding{}, fmt.Errorf("invalid regex normalizer: %w", err)
		}
		if strings.TrimSpace(wire.RuleID) == "" {
			return Binding{}, fmt.Errorf("regex normalizer rule ID is required")
		}
		if strings.TrimSpace(wire.Message) == "" {
			return Binding{}, fmt.Errorf("regex normalizer message is required")
		}
	}

	if len(wire.SeverityMap) == 0 {
		return Binding{}, fmt.Errorf("severity map is required")
	}
	severityMap := make(map[string]finding.Severity, len(wire.SeverityMap))
	for source, value := range wire.SeverityMap {
		if strings.TrimSpace(source) == "" {
			return Binding{}, fmt.Errorf("severity map source is required")
		}
		severity, err := canonicalSeverity(value)
		if err != nil {
			return Binding{}, fmt.Errorf("severity map entry %q: %w", source, err)
		}
		severityMap[source] = severity
	}

	version := Version{}
	if wire.Version != nil {
		version = Version{
			Command:    append([]string(nil), wire.Version.Command...),
			Pattern:    wire.Version.Pattern,
			Constraint: wire.Version.Constraint,
		}
		if err := validateVersion(version); err != nil {
			return Binding{}, err
		}
	}
	for ruleID, page := range wire.Aliases {
		if strings.TrimSpace(ruleID) == "" || strings.TrimSpace(page) == "" {
			return Binding{}, fmt.Errorf("alias rule ID and principle page are required")
		}
	}

	return Binding{
		Language:         wire.Language,
		Tool:             wire.Tool,
		Command:          append([]string(nil), wire.Command...),
		SuccessExitCodes: append([]int(nil), wire.SuccessExitCodes...),
		Normalizer:       wire.Normalizer,
		RuleID:           wire.RuleID,
		Message:          wire.Message,
		Settings:         wire.Settings,
		SeverityMap:      severityMap,
		Version:          version,
		Aliases:          wire.Aliases,
	}, nil
}

func canonicalSeverity(value string) (finding.Severity, error) {
	severity := finding.Severity(value)
	switch severity {
	case finding.Error, finding.Warning, finding.Info:
		return severity, nil
	default:
		return "", fmt.Errorf("invalid severity %q", value)
	}
}

func validateVersion(version Version) error {
	if len(version.Command) == 0 || version.Pattern == "" || version.Constraint == "" {
		return fmt.Errorf("version command, pattern, and constraint are required")
	}
	for index, argument := range version.Command {
		if argument == "" {
			return fmt.Errorf("version command argument %d is empty", index)
		}
	}
	pattern, err := regexp.Compile(version.Pattern)
	if err != nil {
		return fmt.Errorf("invalid version pattern: %w", err)
	}
	if pattern.NumSubexp() < 1 {
		return fmt.Errorf("version pattern requires a capture group")
	}
	if !strings.HasPrefix(version.Constraint, ">=") {
		return fmt.Errorf("unsupported version constraint %q: only >= is supported", version.Constraint)
	}
	minimum := strings.TrimSpace(strings.TrimPrefix(version.Constraint, ">="))
	if _, err := parseSemanticVersion(minimum); err != nil {
		return fmt.Errorf("invalid constraint version %q: %w", minimum, err)
	}
	return nil
}

func validateComponent(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("unsafe %s %q", label, value)
	}
	return nil
}
