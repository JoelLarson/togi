package wiki

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	shippedRoot = "defaults/pages"
	pageSuffix  = ".md"
)

// ErrNotFound reports that no page exists under a name. Dangling aliases are
// tolerated by design (ADR-0006), so callers distinguish this from I/O failure.
var ErrNotFound = errors.New("page does not exist")

// Loader loads shipped principle pages and optional overrides. A page in
// OverrideDir shadows the shipped page of the same name wholesale.
type Loader struct {
	OverrideDir string
}

// Load loads one page by name.
func (l Loader) Load(name string) (Page, error) {
	if err := validateName(name); err != nil {
		return Page{}, err
	}

	if l.OverrideDir != "" {
		exists, err := validateOverrideRoot(l.OverrideDir)
		if err != nil {
			return Page{}, err
		}
		if exists {
			page, found, err := loadOverride(l.OverrideDir, name)
			if err != nil || found {
				return page, err
			}
		}
	}
	return loadShipped(name)
}

func loadOverride(overrideDir, name string) (Page, bool, error) {
	filename := filepath.Join(overrideDir, name+pageSuffix)
	info, err := os.Lstat(filename)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Page{}, false, nil
	case err != nil:
		return Page{}, false, fmt.Errorf("inspect page override %q: %w", filename, err)
	case info.Mode()&os.ModeSymlink != 0:
		return Page{}, false, fmt.Errorf("page override %q is a symlink", filename)
	case !info.Mode().IsRegular():
		return Page{}, false, fmt.Errorf("page override %q is not a regular file", filename)
	}

	content, err := os.ReadFile(filename)
	if err != nil {
		return Page{}, false, fmt.Errorf("read page override %q: %w", filename, err)
	}
	page, err := parsePage(name, content, Override)
	if err != nil {
		return Page{}, false, fmt.Errorf("%s: %w", filename, err)
	}
	return page, true, nil
}

func loadShipped(name string) (Page, error) {
	filename := path.Join(shippedRoot, name+pageSuffix)
	content, err := fs.ReadFile(shipped, filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Page{}, fmt.Errorf("%q: %w", name, ErrNotFound)
		}
		return Page{}, fmt.Errorf("read shipped page %q: %w", name, err)
	}
	page, err := parsePage(name, content, Shipped)
	if err != nil {
		return Page{}, fmt.Errorf("%s: %w", filename, err)
	}
	return page, nil
}

func validateOverrideRoot(root string) (bool, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect page override directory %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("page override directory %q is a symlink", root)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("page override path %q is not a directory", root)
	}
	return true, nil
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("unsafe page name %q", name)
	}
	if strings.HasSuffix(name, pageSuffix) {
		return fmt.Errorf("page name %q must not include the %q suffix", name, pageSuffix)
	}
	return nil
}
