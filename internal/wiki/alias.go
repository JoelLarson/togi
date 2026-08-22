package wiki

import (
	"sort"
	"strings"

	"github.com/joellarson/togi/internal/gate"
)

// Ref names one alias pointing at a page: the gate and language whose binding
// declares it, and the rule ID pattern it matches.
type Ref struct {
	Gate     string
	Language string
	RuleID   string
}

// Conflict reports one rule ID pattern aliased to different pages by
// different bindings. Resolution still succeeds — a finding carries its gate,
// so it reaches that gate's binding — but the wiki is saying two things about
// one rule, which is nearly always a mistake.
type Conflict struct {
	RuleID string
	Pages  []string
	Refs   []Ref
}

// Resolve maps a rule ID to a principle page using one binding's aliases.
// An exact key wins over any glob; among globs the longest prefix wins, so
// "golangci-lint/errcheck" beats "golangci-lint/*". Globs are the trailing-"*"
// prefix form only, which is the whole of the pattern language.
func Resolve(aliases map[string]string, ruleID string) (string, bool) {
	if page, ok := aliases[ruleID]; ok {
		return page, true
	}

	best, bestLen, found := "", -1, false
	for pattern, page := range aliases {
		prefix, ok := globPrefix(pattern)
		if !ok || !strings.HasPrefix(ruleID, prefix) {
			continue
		}
		// Map keys are unique, so no two globs share a prefix length; the
		// longest match is always unambiguous.
		if len(prefix) > bestLen {
			best, bestLen, found = page, len(prefix), true
		}
	}
	return best, found
}

func globPrefix(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, "*") {
		return "", false
	}
	return strings.TrimSuffix(pattern, "*"), true
}

// ReverseIndex computes page -> the aliases pointing at it, across every gate
// and language. ADR-0006 keeps aliases in bindings so a page need not know its
// callers; this is that index computed on demand rather than stored.
func ReverseIndex(gates []gate.Gate) map[string][]Ref {
	index := make(map[string][]Ref)
	forEachAlias(gates, func(ref Ref, page string) {
		index[page] = append(index[page], ref)
	})
	for page := range index {
		sortRefs(index[page])
	}
	return index
}

// Conflicts reports rule ID patterns aliased to more than one page.
func Conflicts(gates []gate.Gate) []Conflict {
	pages := make(map[string]map[string]struct{})
	refs := make(map[string][]Ref)
	forEachAlias(gates, func(ref Ref, page string) {
		if pages[ref.RuleID] == nil {
			pages[ref.RuleID] = make(map[string]struct{})
		}
		pages[ref.RuleID][page] = struct{}{}
		refs[ref.RuleID] = append(refs[ref.RuleID], ref)
	})

	conflicts := make([]Conflict, 0)
	for ruleID, targets := range pages {
		if len(targets) < 2 {
			continue
		}
		ordered := make([]string, 0, len(targets))
		for page := range targets {
			ordered = append(ordered, page)
		}
		sort.Strings(ordered)
		sortRefs(refs[ruleID])
		conflicts = append(conflicts, Conflict{RuleID: ruleID, Pages: ordered, Refs: refs[ruleID]})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].RuleID < conflicts[j].RuleID })
	return conflicts
}

func forEachAlias(gates []gate.Gate, visit func(Ref, string)) {
	for _, g := range gates {
		for _, language := range g.BindingLanguages() {
			binding := g.Bindings[language]
			patterns := make([]string, 0, len(binding.Aliases))
			for pattern := range binding.Aliases {
				patterns = append(patterns, pattern)
			}
			sort.Strings(patterns)
			for _, pattern := range patterns {
				ref := Ref{Gate: g.Manifest.Name, Language: language, RuleID: pattern}
				visit(ref, binding.Aliases[pattern])
			}
		}
	}
}

func sortRefs(refs []Ref) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Gate != refs[j].Gate {
			return refs[i].Gate < refs[j].Gate
		}
		if refs[i].Language != refs[j].Language {
			return refs[i].Language < refs[j].Language
		}
		return refs[i].RuleID < refs[j].RuleID
	})
}
