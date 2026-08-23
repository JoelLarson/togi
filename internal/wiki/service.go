package wiki

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joellarson/togi/internal/finding"
	"github.com/joellarson/togi/internal/gate"
)

// ErrConflictingAliases reports that lint found a rule ID aliased to more
// than one page.
var ErrConflictingAliases = errors.New("conflicting aliases")

// AliasConflictError reports how many rule IDs point at conflicting pages.
type AliasConflictError struct {
	RuleIDs int
}

func (e *AliasConflictError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("conflicting aliases: %d rule IDs", e.RuleIDs)
}

// ExitCode marks conflicting aliases as findings for CLI callers.
func (e *AliasConflictError) ExitCode() int { return 1 }

// Is preserves compatibility with ErrConflictingAliases.
func (e *AliasConflictError) Is(target error) bool {
	return target == ErrConflictingAliases
}

// GateSource supplies the compiled gate definitions whose bindings own wiki aliases.
type GateSource interface {
	LoadAll() ([]gate.Gate, error)
}

// Service renders the wiki commands over a page loader and a gate loader.
type Service struct {
	Pages  Loader
	Gates  GateSource
	Stdout io.Writer
	Stderr io.Writer
}

// For returns the principle page aliased from a finding's gate and language
// binding. Missing mappings and dangling pages are legal and return found=false.
func (s Service) For(f finding.Finding) (Page, bool, error) {
	gates, err := s.Gates.LoadAll()
	if err != nil {
		return Page{}, false, err
	}

	var matched *gate.Gate
	seen := make(map[string]struct{}, len(gates))
	for index := range gates {
		loaded := &gates[index]
		if !loaded.Valid() {
			return Page{}, false, fmt.Errorf("invalid gate %q from gate source", loaded.Manifest.Name)
		}
		name := loaded.Manifest.Name
		if _, exists := seen[name]; exists {
			return Page{}, false, fmt.Errorf("duplicate gate %q from gate source", name)
		}
		seen[name] = struct{}{}
		if name == f.Gate {
			matched = loaded
		}
	}
	if matched == nil {
		return Page{}, false, nil
	}
	binding, ok := matched.Bindings[f.Language]
	if !ok {
		return Page{}, false, nil
	}
	name, ok := resolve(binding.Aliases, f.RuleID)
	if !ok {
		return Page{}, false, nil
	}
	page, err := s.Pages.Load(name)
	if errors.Is(err, ErrNotFound) {
		return Page{}, false, nil
	}
	if err != nil {
		return Page{}, false, err
	}
	return page, true, nil
}

// Show writes a page verbatim, followed by the aliases that reach it.
func (s Service) Show(name string) error {
	page, err := s.Pages.Load(name)
	if err != nil {
		return err
	}
	gates, err := s.Gates.LoadAll()
	if err != nil {
		return err
	}

	body := page.Body
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if _, err := io.WriteString(s.Stdout, body); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.Stdout, "\n---\npage: %s (%s)\n", page.Name, page.Origin); err != nil {
		return err
	}

	refs := ReverseIndex(gates)[page.Name]
	if len(refs) == 0 {
		_, err := fmt.Fprintln(s.Stdout, "aliases: none — no binding points at this page")
		return err
	}
	if _, err := fmt.Fprintf(s.Stdout, "aliases: %d\n", len(refs)); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := fmt.Fprintf(s.Stdout, "  %s/%s\t%s\n", ref.Gate, ref.Language, ref.RuleID); err != nil {
			return err
		}
	}
	return nil
}

// Lint checks every alias target. A dangling alias is a warning: ADR-0006
// makes a page-less gate legal, and the brief is simply thinner. A rule ID
// aliased to two different pages is an error.
func (s Service) Lint() error {
	gates, err := s.Gates.LoadAll()
	if err != nil {
		return err
	}

	index := ReverseIndex(gates)
	pages := make([]string, 0, len(index))
	for page := range index {
		pages = append(pages, page)
	}
	sort.Strings(pages)

	dangling := 0
	for _, page := range pages {
		_, err := s.Pages.Load(page)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			continue
		}
		dangling++
		for _, ref := range index[page] {
			if _, err := fmt.Fprintf(s.Stderr, "warning: %s/%s aliases %q to %q, which has no page\n",
				ref.Gate, ref.Language, ref.RuleID, page); err != nil {
				return err
			}
		}
	}

	conflicts := Conflicts(gates)
	for _, conflict := range conflicts {
		if _, err := fmt.Fprintf(s.Stderr, "error: %q is aliased to %v\n", conflict.RuleID, conflict.Pages); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(s.Stdout, "%d pages referenced, %d dangling, %d conflicting\n",
		len(pages), dangling, len(conflicts)); err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return &AliasConflictError{RuleIDs: len(conflicts)}
	}
	return nil
}

// Eject copies a shipped page into the config directory for editing. It
// refuses to overwrite an existing override, so a hand-edited page is never
// silently replaced by the shipped text.
func (s Service) Eject(name string) error {
	if s.Pages.OverrideDir == "" {
		return fmt.Errorf("no wiki override directory is configured")
	}
	page, err := loadShippedFor(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Pages.OverrideDir, 0o755); err != nil {
		return fmt.Errorf("create wiki directory %q: %w", s.Pages.OverrideDir, err)
	}

	filename := filepath.Join(s.Pages.OverrideDir, name+pageSuffix)
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("page override %q already exists", filename)
		}
		return fmt.Errorf("create page override %q: %w", filename, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := io.WriteString(file, page.Body); err != nil {
		return fmt.Errorf("write page override %q: %w", filename, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync page override %q: %w", filename, err)
	}
	_, err = fmt.Fprintf(s.Stdout, "%s\n", filename)
	return err
}

func loadShippedFor(name string) (Page, error) {
	if err := validateName(name); err != nil {
		return Page{}, err
	}
	return loadShipped(name)
}
