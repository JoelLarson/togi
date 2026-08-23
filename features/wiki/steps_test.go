package wiki

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"
	"github.com/joellarson/togi/features/internal/harness"
	internalwiki "github.com/joellarson/togi/internal/wiki"
)

type principlePagesFeature struct {
	world        *harness.World
	operatorName string
	operatorBody []byte
}

func newPrinciplePagesFeature(factory harness.DriverFactory) *principlePagesFeature {
	return &principlePagesFeature{world: harness.NewWorld(factory, harness.NeedsWiki)}
}

func (f *principlePagesFeature) initialize(sc *godog.ScenarioContext) {
	sc.Before(f.before)
	sc.After(f.world.After)
	sc.Step(`^no operator copy of "([^"]*)"$`, f.noOperatorCopy)
	sc.Step(`^an operator copy of "([^"]*)"$`, f.operatorCopy)
	sc.Step(`^several gate aliases for "([^"]*)"$`, f.severalAliases)
	sc.Step(`^a gate alias whose principle page does not exist$`, f.danglingAlias)
	sc.Step(`^one rule is aliased to two principle pages$`, f.conflictingAliases)
	sc.Step(`^an existing operator copy of "([^"]*)"$`, f.existingOperatorCopy)
	sc.Step(`^I show the "([^"]*)" principle page$`, f.show)
	sc.Step(`^I lint the principle pages$`, f.lint)
	sc.Step(`^I eject the "([^"]*)" principle page$`, f.eject)
	sc.Step(`^the shipped page body and provenance are displayed$`, f.shippedPageDisplayed)
	sc.Step(`^the operator page body and provenance are displayed$`, f.operatorPageDisplayed)
	sc.Step(`^its aliases are displayed in gate, language, and rule order$`, f.aliasesDisplayed)
	sc.Step(`^the dangling alias is warned and the outcome is (\d+)$`, f.danglingWarned)
	sc.Step(`^both conflicting pages are reported and the outcome is (\d+)$`, f.conflictsReported)
	sc.Step(`^the operator copy equals the shipped page$`, f.operatorCopyEqualsShipped)
	sc.Step(`^the eject is rejected and the operator copy is unchanged$`, f.ejectRejected)
}

func (f *principlePagesFeature) before(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
	f.operatorName, f.operatorBody = "", nil
	return f.world.Before(ctx, scenario)
}

func (f *principlePagesFeature) noOperatorCopy(name string) error {
	filename := f.operatorPath(name)
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("operator page %q already exists", filename)
		}
		return fmt.Errorf("inspect operator page %q: %w", filename, err)
	}
	return nil
}

func (f *principlePagesFeature) operatorCopy(name string) error {
	body := []byte("# Operator guidance\n\nThis operator-specific page replaces the shipped guidance.\n")
	return f.writeOperatorPage(name, body)
}

func (f *principlePagesFeature) existingOperatorCopy(name string) error {
	body := []byte("# Existing guidance\n\nDo not overwrite this operator-owned page.\n")
	return f.writeOperatorPage(name, body)
}

func (f *principlePagesFeature) writeOperatorPage(name string, body []byte) error {
	filename := f.operatorPath(name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return fmt.Errorf("create operator wiki directory: %w", err)
	}
	if err := os.WriteFile(filename, body, 0o600); err != nil {
		return fmt.Errorf("write operator page: %w", err)
	}
	f.operatorName, f.operatorBody = name, append([]byte(nil), body...)
	return nil
}

func (f *principlePagesFeature) severalAliases(page string) error {
	for _, definition := range []harness.GateDefinition{
		{Name: "alpha", Description: "alpha aliases", Tool: "alpha-tool", Aliases: map[string]string{"z-rule": page, "a-rule": page}},
		{Name: "beta", Description: "beta aliases", Tool: "beta-tool", Aliases: map[string]string{"b-rule": page}},
	} {
		if err := f.world.Environment().WriteGate(definition); err != nil {
			return err
		}
	}
	return nil
}

func (f *principlePagesFeature) danglingAlias() error {
	return f.world.Environment().WriteGate(harness.GateDefinition{
		Name: "dangling", Description: "dangling alias", Tool: "dangling-tool",
		Aliases: map[string]string{"dangling/rule": "missing-principle-page"},
	})
}

func (f *principlePagesFeature) conflictingAliases() error {
	return f.world.Environment().WriteGate(harness.GateDefinition{
		Name: "conflict", Description: "conflicting alias", Tool: "conflict-tool",
		Aliases: map[string]string{"gocyclo/complexity": "other-principle-page"},
	})
}

func (f *principlePagesFeature) show(ctx context.Context, name string) error {
	return f.world.Show(ctx, name)
}

func (f *principlePagesFeature) lint(ctx context.Context) error { return f.world.Lint(ctx) }

func (f *principlePagesFeature) eject(ctx context.Context, name string) error {
	return f.world.Eject(ctx, name)
}

func (f *principlePagesFeature) shippedPageDisplayed() error {
	output := f.world.LastCommand().Stdout()
	if !strings.Contains(output, "# Small, composable functions\n") {
		return fmt.Errorf("shipped page body is absent from output: %q", output)
	}
	if !strings.Contains(output, "page: small-composable-functions (shipped)") {
		return fmt.Errorf("shipped provenance is absent from output: %q", output)
	}
	return nil
}

func (f *principlePagesFeature) operatorPageDisplayed() error {
	output := f.world.LastCommand().Stdout()
	if !strings.Contains(output, string(f.operatorBody)) {
		return fmt.Errorf("operator page body is absent from output: %q", output)
	}
	if !strings.Contains(output, "page: "+f.operatorName+" (override)") {
		return fmt.Errorf("operator provenance is absent from output: %q", output)
	}
	return nil
}

func (f *principlePagesFeature) aliasesDisplayed() error {
	output := f.world.LastCommand().Stdout()
	want := []string{
		"  alpha/go\ta-rule",
		"  alpha/go\tz-rule",
		"  beta/go\tb-rule",
		"  complexity/go\tgocyclo/complexity",
	}
	for _, line := range want {
		if !strings.Contains(output, line+"\n") {
			return fmt.Errorf("expected alias line %q in output: %q", line, output)
		}
	}
	last := -1
	for _, line := range want {
		position := strings.Index(output, line)
		if position <= last {
			return fmt.Errorf("aliases are not sorted: %q", output)
		}
		last = position
	}
	return nil
}

func (f *principlePagesFeature) danglingWarned(want int) error {
	observation := f.world.LastCommand()
	outcome, err := observation.Outcome()
	if err != nil {
		return err
	}
	if outcome.Code != want {
		return fmt.Errorf("outcome = %d, want %d", outcome.Code, want)
	}
	if !strings.Contains(observation.Stderr(), `warning: dangling/go aliases "dangling/rule" to "missing-principle-page"`) {
		return fmt.Errorf("dangling alias warning is absent from stderr: %q", observation.Stderr())
	}
	if !strings.Contains(observation.Stdout(), "pages referenced,") || !strings.Contains(observation.Stdout(), "dangling,") {
		return fmt.Errorf("lint summary is absent from stdout: %q", observation.Stdout())
	}
	return nil
}

func (f *principlePagesFeature) conflictsReported(want int) error {
	observation := f.world.LastCommand()
	outcome, err := observation.Outcome()
	if err != nil {
		return err
	}
	if outcome.Code != want {
		return fmt.Errorf("outcome = %d, want %d", outcome.Code, want)
	}
	for _, page := range []string{"other-principle-page", "small-composable-functions"} {
		if !strings.Contains(observation.Stderr(), page) {
			return fmt.Errorf("conflict page %q is absent from stderr: %q", page, observation.Stderr())
		}
	}
	return nil
}

func (f *principlePagesFeature) operatorCopyEqualsShipped() error {
	got, err := os.ReadFile(f.operatorPath("small-composable-functions"))
	if err != nil {
		return fmt.Errorf("read ejected operator page: %w", err)
	}
	page, err := (internalwiki.Loader{}).Load("small-composable-functions")
	if err != nil {
		return err
	}
	if string(got) != page.Body {
		return fmt.Errorf("ejected page does not equal shipped page bytes")
	}
	return nil
}

func (f *principlePagesFeature) ejectRejected() error {
	got, err := os.ReadFile(f.operatorPath(f.operatorName))
	if err != nil {
		return fmt.Errorf("read operator page after rejected eject: %w", err)
	}
	if string(got) != string(f.operatorBody) {
		return fmt.Errorf("eject overwrote operator page: got %q, want %q", got, f.operatorBody)
	}
	outcome, err := f.world.LastCommand().Outcome()
	if err != nil {
		return err
	}
	if outcome.Code == 0 {
		return fmt.Errorf("eject outcome = 0, want rejection")
	}
	return nil
}

func (f *principlePagesFeature) operatorPath(name string) string {
	return filepath.Join(f.world.Environment().ConfigRoot, "wiki", name+".md")
}
