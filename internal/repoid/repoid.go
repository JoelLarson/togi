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

	shallow, err := isShallowRepository(ctx, root)
	if err != nil {
		return ID{}, err
	}

	var key string
	if shallow {
		key, err = fallbackKey(ctx, root)
	} else {
		key, err = rootCommitKey(ctx, root)
		if err != nil {
			key, err = fallbackKey(ctx, root)
		}
	}
	if err != nil {
		return ID{}, err
	}

	return ID{
		Key:       key,
		Directory: sanitize(filepath.Base(root)) + "-" + key[:12],
		Root:      root,
	}, nil
}

func isShallowRepository(ctx context.Context, root string) (bool, error) {
	output, err := gitOutput(ctx, root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, fmt.Errorf("determine whether repository is shallow: %w", err)
	}
	return output == "true", nil
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

	if !strings.Contains(remote, "://") {
		if colon := strings.Index(remote, ":"); colon >= 0 {
			authority := remote[:colon]
			if at := strings.LastIndex(authority, "@"); at >= 0 {
				authority = authority[at+1:]
			}
			if authority != "" && !strings.Contains(authority, "/") {
				return normalizedHostPath(authority, remote[colon+1:])
			}
		}
	}

	parsed, err := url.Parse(remote)
	if err != nil {
		return "", false
	}
	if parsed.Scheme == "file" && parsed.Host == "" && parsed.Path != "" {
		path := strings.TrimSuffix(strings.TrimRight(parsed.EscapedPath(), "/"), ".git")
		return "file://" + path, path != ""
	}
	if parsed.Hostname() == "" {
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
