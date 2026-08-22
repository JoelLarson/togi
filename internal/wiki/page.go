package wiki

import (
	"fmt"
	"strings"
)

// Origin records where a loaded page came from.
type Origin string

const (
	// Shipped marks a page read from the embedded defaults.
	Shipped Origin = "shipped"
	// Override marks a page read from the config directory.
	Override Origin = "override"
)

// Page is one principle page: a language-agnostic engineering practice, its
// named refactoring techniques, and its constraints.
type Page struct {
	Name   string
	Title  string
	Body   string
	Origin Origin
}

// parsePage reads a page body, taking its title from the leading heading.
// Nothing else about the markdown is interpreted: briefs concatenate the body
// verbatim, so any further structure is for human readers only (ADR-0006).
func parsePage(name string, content []byte, origin Origin) (Page, error) {
	body := string(content)
	title, err := parseTitle(body)
	if err != nil {
		return Page{}, fmt.Errorf("page %q: %w", name, err)
	}
	return Page{Name: name, Title: title, Body: body, Origin: origin}, nil
}

func parseTitle(body string) (string, error) {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "# ") {
			return "", fmt.Errorf("must open with a %q heading", "# ")
		}
		title := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		if title == "" {
			return "", fmt.Errorf("heading is empty")
		}
		return title, nil
	}
	return "", fmt.Errorf("is empty")
}
