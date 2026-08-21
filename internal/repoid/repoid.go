package repoid

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
	if err := ctx.Err(); err != nil {
		return ID{}, err
	}

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
		var headExists bool
		headExists, err = hasHead(ctx, root)
		if err != nil {
			return ID{}, err
		}
		if headExists {
			key, err = rootCommitKey(ctx, root)
		} else {
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

func hasHead(ctx context.Context, root string) (bool, error) {
	_, err := gitOutput(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD")
	if err == nil {
		return true, nil
	}
	if gitExitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("verify repository HEAD: %w", err)
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
	remote, ok, err := remoteKey(ctx, root)
	if err != nil {
		return "", err
	}
	if ok {
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

func remoteKey(ctx context.Context, root string) (string, bool, error) {
	remotes, err := gitOutput(ctx, root, "remote")
	if err != nil {
		return "", false, fmt.Errorf("list repository remotes: %w", err)
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
			return "", false, fmt.Errorf("read remote %q: %w", name, err)
		}
		if normalized, ok := normalizeRemote(remote); ok {
			return normalized, true, nil
		}
	}
	return "", false, nil
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
				return normalizedHostPath("", authority, "", remote[colon+1:])
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
	return normalizedHostPath(parsed.Scheme, parsed.Hostname(), parsed.Port(), parsed.Path)
}

func normalizedHostPath(scheme, host, port, path string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if port != "" && port != defaultPort(scheme) {
		host = net.JoinHostPort(host, port)
	}
	path = strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	path = strings.TrimLeft(path, "/")
	if host == "" || path == "" {
		return "", false
	}
	return host + "/" + path, true
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "ssh":
		return "22"
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	command := append([]string{"-C", directory}, args...)
	cmd := exec.CommandContext(ctx, "git", command...)
	cmd.Env = gitEnvironment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", &gitCommandError{args: args, err: err, output: strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

type gitCommandError struct {
	args   []string
	err    error
	output string
}

func (err *gitCommandError) Error() string {
	if err.output == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(err.args, " "), err.err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(err.args, " "), err.err, err.output)
}

func (err *gitCommandError) Unwrap() error {
	return err.err
}

func gitExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func gitEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_NO_REPLACE_OBJECTS=1")
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func sanitize(name string) string {
	const maxBasename = 255 - 1 - 12

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
	if len(result) > maxBasename {
		return result[:maxBasename]
	}
	return result
}
