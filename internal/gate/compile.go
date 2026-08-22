package gate

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/normalizer"
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
		binding := cloneBindingState(bindings[language])
		if binding.Language != language {
			return Gate{}, fmt.Errorf("binding %q declares language %q", language, binding.Language)
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
		snapshot := &bindingSnapshot{wire: cloneBindingState(binding)}
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

func cloneBindingState(binding Binding) Binding {
	binding.Command = cloneSlice(binding.Command)
	binding.SuccessExitCodes = cloneSlice(binding.SuccessExitCodes)
	binding.FindingExitCodes = cloneSlice(binding.FindingExitCodes)
	binding.Settings = cloneSettings(binding.Settings)
	binding.SeverityMap = cloneSeverityMap(binding.SeverityMap)
	binding.Version.Command = cloneSlice(binding.Version.Command)
	binding.Aliases = cloneAliases(binding.Aliases)
	binding.compiled = nil
	binding.owner = nil
	binding.snapshot = nil
	return binding
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append(make([]T, 0, len(values)), values...)
}

func cloneSettings(settings map[string]any) map[string]any {
	if settings == nil {
		return nil
	}
	cloned := make(map[string]any, len(settings))
	for key, value := range settings {
		cloned[key] = cloneSettingValue(value)
	}
	return cloned
}

func cloneSettingValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneSettingReflect(reflect.ValueOf(value)).Interface()
}

func cloneSettingReflect(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(cloneSettingReflect(value.Elem()))
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned.SetMapIndex(iterator.Key(), cloneSettingReflect(iterator.Value()))
		}
		return cloned
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneSettingReflect(value.Elem()))
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := range value.Len() {
			cloned.Index(index).Set(cloneSettingReflect(value.Index(index)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for index := range value.Len() {
			cloned.Index(index).Set(cloneSettingReflect(value.Index(index)))
		}
		return cloned
	default:
		return value
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
