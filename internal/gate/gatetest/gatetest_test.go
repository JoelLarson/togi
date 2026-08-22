package gatetest_test

import (
	"reflect"
	"testing"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
	"github.com/joellarson/togi/internal/gate/gatetest"
)

func TestWriteRoundTripsOptionsThroughLoader(t *testing.T) {
	dir := t.TempDir()
	aliases := map[string]string{"tool/*": "principle"}
	settings := map[string]any{"threshold": int64(12)}
	version := gate.Version{Command: []string{"fixture", "version"}, Pattern: `v([0-9.]+)`, Constraint: ">=1.0.0 <2.0.0"}

	gatetest.Write(t, dir, "custom",
		gatetest.Scope(gate.Diff),
		gatetest.Location(gate.EntityLocation),
		gatetest.Blocking(),
		gatetest.Command("fixture", "{{.threshold}}"),
		gatetest.Language("rust"),
		gatetest.Version(version),
		gatetest.Settings(settings),
		gatetest.Aliases(aliases),
	)
	aliases["tool/*"] = "mutated"
	settings["threshold"] = int64(99)
	version.Command[0] = "mutated"

	loaded, err := (gate.Loader{OverrideDir: dir}).Load("custom")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Valid() {
		t.Fatal("written fixture did not load as a valid gate")
	}
	if loaded.Manifest.Scope != gate.Diff || loaded.Manifest.Location != gate.EntityLocation {
		t.Fatalf("manifest = %#v", loaded.Manifest)
	}
	if !reflect.DeepEqual(loaded.Manifest.Blocking, []finding.Severity{}) {
		t.Fatalf("blocking = %#v", loaded.Manifest.Blocking)
	}
	binding := loaded.Bindings["rust"]
	if got := binding.Settings["threshold"]; got != int64(12) {
		t.Fatalf("threshold = %#v", got)
	}
	if got := binding.Aliases["tool/*"]; got != "principle" {
		t.Fatalf("alias = %q", got)
	}
	if !reflect.DeepEqual(binding.Version, gate.Version{Command: []string{"fixture", "version"}, Pattern: `v([0-9.]+)`, Constraint: ">=1.0.0 <2.0.0"}) {
		t.Fatalf("version = %#v", binding.Version)
	}
}

func TestCompileDoesNotShareMutableDefaults(t *testing.T) {
	first := gatetest.Compile(t, "first")
	first.Manifest.Blocking[0] = finding.Info
	firstBinding := first.Bindings["go"]
	firstBinding.Command[0] = "mutated"
	firstBinding.SeverityMap["default"] = finding.Error

	second := gatetest.Compile(t, "second")
	if !second.Valid() {
		t.Fatal("mutating one fixture changed a later fixture's defaults")
	}
	if second.Manifest.Blocking[0] != finding.Error || second.Bindings["go"].Command[0] != "fixture" || second.Bindings["go"].SeverityMap["default"] != finding.Warning {
		t.Fatalf("second fixture inherited mutations: %#v", second)
	}
}
