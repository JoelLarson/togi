// Package repoid identifies a target repository.
package repoid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ID identifies a target repository and its external state directory.
type ID struct {
	Key       string
	Directory string
	Root      string
}

// Resolve derives a stable identity for the repository containing start.
func Resolve(ctx context.Context, start string) (ID, error) {
	root, err := gitOutput(ctx, start, "rev-parse", "--show-toplevel")
	if err != nil {
		return ID{}, fmt.Errorf("find repository root: %w", err)
	}

	key, err := rootCommitKey(ctx, root)
	if err != nil {
		key, err = fallbackKey(ctx, root)
		if err != nil {
			return ID{}, err
		}
	}

	return ID{
		Key:       key,
		Directory: sanitize(filepath.Base(root)) + "-" + key[:12],
		Root:      root,
	}, nil
}

func rootCommitKey(ctx context.Context, root string) (string, error) {
	output, err := gitOutput(ctx, root, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return "", fmt.Errorf("list root commits: %w", err)
	}

	roots := strings.Fields(output)
	switch len(roots) {
	case 0:
		return "", fmt.Errorf("list root commits: no root commits")
	case 1:
		return roots[0], nil
	default:
		sort.Strings(roots)
		return hash(strings.Join(roots, "\n")), nil
	}
}

func fallbackKey(ctx context.Context, root string) (string, error) {
	if remote, ok := remoteKey(ctx, root); ok {
		return hash(remote), nil
	}

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("make repository path absolute: %w", err)
	}
	return hash(abs), nil
}

func remoteKey(ctx context.Context, root string) (string, bool) {
	remotes, err := gitOutput(ctx, root, "remote")
	if err != nil {
		return "", false
	}

	names := strings.Fields(remotes)
	sort.Strings(names)
	for index, name := range names {
		if name == "origin" {
			names[0], names[index] = names[index], names[0]
			break
		}
	}
	for _, name := range names {
		remote, err := gitOutput(ctx, root, "remote", "get-url", name)
		if err != nil {
			continue
		}
		if normalized, ok := normalizeRemote(remote); ok {
			return normalized, true
		}
	}
	return "", false
}

func normalizeRemote(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}

	if at := strings.LastIndex(remote, "@"); at >= 0 && !strings.Contains(remote, "://") {
		if colon := strings.Index(remote[at+1:], ":"); colon >= 0 {
			host := remote[at+1 : at+1+colon]
			path := remote[at+2+colon:]
			return normalizedHostPath(host, path)
		}
	}

	parsed, err := url.Parse(remote)
	if err != nil || parsed.Hostname() == "" {
		return "", false
	}
	return normalizedHostPath(parsed.Hostname(), parsed.Path)
}

func normalizedHostPath(host, path string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	path = strings.TrimLeft(path, "/")
	if host == "" || path == "" {
		return "", false
	}
	return host + "/" + path, true
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	command := append([]string{"-C", directory}, args...)
	output, err := exec.CommandContext(ctx, "git", command...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func sanitize(name string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if valid {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}

	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "repo"
	}
	return result
}
