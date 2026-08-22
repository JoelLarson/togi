package gate

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/normalizer"
	"github.com/pelletier/go-toml/v2"
)

// Compile validates a gate wholesale and compiles each binding's normalizer.
// It is the only way to mint a valid Gate: the loader funnels every decoded
// gate through it, and tests build gates with it directly. A Gate that
// reports Valid() is guaranteed runnable — the executor re-checks nothing
// the compiler already proved.
func Compile(manifest Manifest, bindings map[string]Binding) (Gate, error) {
	if err := validateManifest(manifest); err != nil {
		return Gate{}, err
	}
	if len(bindings) == 0 {
		return Gate{}, errors.New("at least one language binding is required")
	}
	languages := make([]string, 0, len(bindings))
	for language := range bindings {
		languages = append(languages, language)
	}
	sort.Strings(languages)

	owner := &ownership{marker: 1}
	compiled := make(map[string]Binding, len(bindings))
	snapshots := make(map[string]*bindingSnapshot, len(bindings))
	for _, language := range languages {
		wireBinding := bindings[language]
		if wireBinding.Language != language {
			return Gate{}, fmt.Errorf("binding %q declares language %q", language, wireBinding.Language)
		}
		binding, err := cloneBindingState(wireBinding)
		if err != nil {
			return Gate{}, fmt.Errorf("binding %s: %w", language, err)
		}
		if err := validateBindingValue(binding); err != nil {
			return Gate{}, fmt.Errorf("binding %s: %w", language, err)
		}
		parser, err := normalizer.Parse(binding.Normalizer, normalizer.Config{
			Language:    binding.Language,
			RuleID:      binding.RuleID,
			Message:     binding.Message,
			SeverityMap: cloneSeverityMap(binding.SeverityMap),
		})
		if err != nil {
			return Gate{}, fmt.Errorf("binding %s: %w", language, err)
		}
		snapshotWire, err := cloneBindingState(binding)
		if err != nil {
			return Gate{}, fmt.Errorf("binding %s: %w", language, err)
		}
		snapshot := &bindingSnapshot{wire: snapshotWire}
		binding.compiled = parser
		binding.owner = owner
		binding.snapshot = snapshot
		compiled[language] = binding
		snapshots[language] = snapshot
	}
	compiledManifest := cloneManifest(manifest)
	return Gate{
		Manifest: compiledManifest, Bindings: compiled, owner: owner,
		manifestSnapshot: cloneManifest(compiledManifest), bindingSnapshots: snapshots,
	}, nil
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Blocking = cloneSlice(manifest.Blocking)
	return manifest
}

func cloneBindingState(binding Binding) (Binding, error) {
	binding.Command = cloneSlice(binding.Command)
	binding.SuccessExitCodes = cloneSlice(binding.SuccessExitCodes)
	binding.FindingExitCodes = cloneSlice(binding.FindingExitCodes)
	settings, err := cloneSettings(binding.Settings)
	if err != nil {
		return Binding{}, err
	}
	binding.Settings = settings
	binding.SeverityMap = cloneSeverityMap(binding.SeverityMap)
	binding.Version.Command = cloneSlice(binding.Version.Command)
	binding.Aliases = cloneAliases(binding.Aliases)
	binding.compiled = nil
	binding.owner = nil
	binding.snapshot = nil
	return binding, nil
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append(make([]T, 0, len(values)), values...)
}

func cloneSettings(settings map[string]any) (map[string]any, error) {
	if settings == nil {
		return nil, nil
	}
	cloned := make(map[string]any, len(settings))
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := cloneSettingValue(key, settings[key])
		if err != nil {
			return nil, err
		}
		cloned[key] = value
	}
	return cloned, nil
}

func cloneSettingValue(path string, value any) (any, error) {
	switch value := value.(type) {
	case string, bool, int64, time.Time, toml.LocalDate, toml.LocalTime, toml.LocalDateTime:
		return value, nil
	case float64:
		if math.IsNaN(value) {
			return nil, fmt.Errorf("settings %s: NaN is not supported", path)
		}
		return value, nil
	case map[string]any:
		if value == nil {
			return nil, fmt.Errorf("settings %s: null is not a TOML value", path)
		}
		cloned := make(map[string]any, len(value))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item, err := cloneSettingValue(path+"."+key, value[key])
			if err != nil {
				return nil, err
			}
			cloned[key] = item
		}
		return cloned, nil
	case []any:
		if value == nil {
			return nil, fmt.Errorf("settings %s: null is not a TOML value", path)
		}
		cloned := make([]any, len(value))
		for index, item := range value {
			item, err := cloneSettingValue(fmt.Sprintf("%s[%d]", path, index), item)
			if err != nil {
				return nil, err
			}
			cloned[index] = item
		}
		return cloned, nil
	default:
		return nil, fmt.Errorf("settings %s: unsupported TOML value type %T", path, value)
	}
}

func cloneSeverityMap(severities map[string]finding.Severity) map[string]finding.Severity {
	if severities == nil {
		return nil
	}
	cloned := make(map[string]finding.Severity, len(severities))
	for source, severity := range severities {
		cloned[source] = severity
	}
	return cloned
}

func cloneAliases(aliases map[string]string) map[string]string {
	if aliases == nil {
		return nil
	}
	cloned := make(map[string]string, len(aliases))
	for ruleID, page := range aliases {
		cloned[ruleID] = page
	}
	return cloned
}

func validateManifest(manifest Manifest) error {
	if err := validateComponent("gate name", manifest.Name); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return errors.New("manifest name is required")
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return errors.New("manifest description is required")
	}
	if manifest.CostClass.defaultTimeout() == 0 {
		return fmt.Errorf("invalid cost class %q", manifest.CostClass)
	}
	switch manifest.FixPolicy {
	case AutofixOnly, AutofixThenLLM, LLMFix, ReportOnly:
	default:
		return fmt.Errorf("invalid fix policy %q", manifest.FixPolicy)
	}
	switch manifest.Scope {
	case Diff, Repo:
	default:
		return fmt.Errorf("invalid scope %q", manifest.Scope)
	}
	switch manifest.Location {
	case PointLocation, EntityLocation:
	default:
		return fmt.Errorf("invalid location %q", manifest.Location)
	}
	if manifest.Blocking == nil {
		return errors.New("blocking severities are required (an empty list means nothing blocks)")
	}
	seen := make(map[finding.Severity]struct{}, len(manifest.Blocking))
	for _, severity := range manifest.Blocking {
		if _, err := canonicalSeverity(string(severity)); err != nil {
			return fmt.Errorf("blocking severity: %w", err)
		}
		if _, exists := seen[severity]; exists {
			return fmt.Errorf("duplicate blocking severity %q", severity)
		}
		seen[severity] = struct{}{}
	}
	if manifest.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

func validateBindingValue(binding Binding) error {
	if err := validateComponent("binding language", binding.Language); err != nil {
		return err
	}
	if strings.TrimSpace(binding.Language) == "" {
		return errors.New("binding language is required")
	}
	if strings.TrimSpace(binding.Tool) == "" {
		return errors.New("binding tool is required")
	}
	if len(binding.Command) == 0 {
		return errors.New("binding command is required")
	}
	for index, argument := range binding.Command {
		if argument == "" {
			return fmt.Errorf("binding command argument %d is empty", index)
		}
	}
	rendered, err := binding.RenderCommand()
	if err != nil {
		return fmt.Errorf("render binding command: %w", err)
	}
	for index, argument := range rendered {
		if argument == "" {
			return fmt.Errorf("rendered binding command argument %d is empty", index)
		}
	}
	if len(binding.SuccessExitCodes) == 0 {
		return errors.New("success exit codes cannot be explicitly empty")
	}
	seenExits, err := validateExitCodes("success", binding.SuccessExitCodes)
	if err != nil {
		return err
	}
	if _, err := validateExitCodes("finding", binding.FindingExitCodes); err != nil {
		return err
	}
	for _, exitCode := range binding.FindingExitCodes {
		if _, exists := seenExits[exitCode]; exists {
			return fmt.Errorf("exit code %d cannot be both success and finding", exitCode)
		}
	}
	if len(binding.SeverityMap) == 0 {
		return errors.New("severity map is required")
	}
	for source, value := range binding.SeverityMap {
		if strings.TrimSpace(source) == "" {
			return errors.New("severity map source is required")
		}
		if _, err := canonicalSeverity(string(value)); err != nil {
			return fmt.Errorf("severity map entry %q: %w", source, err)
		}
	}
	if len(binding.Version.Command) > 0 || binding.Version.Pattern != "" || binding.Version.Constraint != "" {
		if err := validateVersion(binding.Version); err != nil {
			return err
		}
	}
	for ruleID, page := range binding.Aliases {
		if strings.TrimSpace(ruleID) == "" || strings.TrimSpace(page) == "" {
			return errors.New("alias rule ID and principle page are required")
		}
	}
	return nil
}
