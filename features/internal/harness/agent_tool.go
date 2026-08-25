package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"
)

const agentHelperSpecEnvironment = "TOGI_ACCEPTANCE_AGENT_SPEC"

// AgentBehavior describes one scenario-owned Codex process.
type AgentBehavior struct {
	Edits          map[string]string
	Delete         []string
	ExitCode       int
	MalformedJSONL bool
	Sleep          time.Duration
	GitArgs        []string
}

// AgentInvocation records the observable input received by a fake agent.
type AgentInvocation struct {
	Root  string
	Brief string
}

type agentHelperSpec struct {
	Behavior    AgentBehavior
	EditPaths   []string
	DeletePaths []string
	RecordRoot  string
}

// InstallAgent installs a controlled Codex-compatible executable in PATH.
func (e *Environment) InstallAgent(name string, behavior AgentBehavior) (string, error) {
	if runtime.GOOS != "linux" {
		return "", ErrUnsupportedCapability
	}
	if err := validateFixtureName("agent", name); err != nil {
		return "", err
	}
	if behavior.ExitCode < 0 {
		return "", errors.New("agent exit code must not be negative")
	}
	if behavior.Sleep < 0 {
		return "", errors.New("agent sleep must not be negative")
	}
	editPaths := make([]string, 0, len(behavior.Edits))
	canonicalEdits := make(map[string]string, len(behavior.Edits))
	for path := range behavior.Edits {
		clean, err := fixturePath(path)
		if err != nil {
			return "", fmt.Errorf("agent edit: %w", err)
		}
		if _, exists := canonicalEdits[clean]; exists {
			return "", fmt.Errorf("agent edit paths resolve to the same file: %q", clean)
		}
		canonicalEdits[clean] = behavior.Edits[path]
		editPaths = append(editPaths, clean)
	}
	behavior.Edits = canonicalEdits
	sort.Strings(editPaths)
	deletePaths := make([]string, len(behavior.Delete))
	for index, path := range behavior.Delete {
		clean, err := fixturePath(path)
		if err != nil {
			return "", fmt.Errorf("agent deletion: %w", err)
		}
		deletePaths[index] = clean
	}

	recordRelative := filepath.ToSlash(filepath.Join("agents", name, "invocations"))
	if err := secureMkdirAll(e.TempRoot, recordRelative, 0o700); err != nil {
		return "", fmt.Errorf("create agent fixture: %w", err)
	}
	spec := agentHelperSpec{
		Behavior: behavior, EditPaths: editPaths, DeletePaths: deletePaths,
		RecordRoot: filepath.Join(e.TempRoot, filepath.FromSlash(recordRelative)),
	}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode agent fixture: %w", err)
	}
	specRelative := filepath.ToSlash(filepath.Join("agents", name, "spec.json"))
	if err := secureAtomicWrite(e.TempRoot, specRelative, specBytes, 0o600); err != nil {
		return "", fmt.Errorf("write agent fixture: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve agent helper executable: %w", err)
	}
	script := "#!/bin/sh\nexport " + agentHelperSpecEnvironment + "=" + shellQuote(filepath.Join(e.TempRoot, filepath.FromSlash(specRelative))) + "\nexec " + shellQuote(executable) + " \"$@\"\n"
	pathRelative := filepath.ToSlash(filepath.Join("bin", name))
	if err := secureAtomicWrite(e.TempRoot, pathRelative, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write agent executable: %w", err)
	}
	return filepath.Join(e.BinRoot, name), nil
}

func runAgentHelperFromEnvironment() (int, bool) {
	specPath := os.Getenv(agentHelperSpecEnvironment)
	if specPath == "" {
		return 0, false
	}
	_ = os.Unsetenv(agentHelperSpecEnvironment)
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	var spec agentHelperSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	if err := runAgentHelper(spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	return spec.Behavior.ExitCode, true
}

func runAgentHelper(spec agentHelperSpec) error {
	args := os.Args[1:]
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve agent cwd: %w", err)
	}
	if len(args) != 0 {
		if len(args) != 11 || args[0] != "--ask-for-approval" || args[1] != "never" || args[2] != "exec" ||
			args[3] != "--ephemeral" || args[4] != "--json" || args[5] != "--sandbox" || args[6] != "workspace-write" ||
			args[7] != "--ignore-user-config" || args[8] != "--cd" || args[10] != "-" {
			return errors.New("unexpected agent arguments")
		}
		root, err = filepath.EvalSymlinks(args[9])
		if err != nil {
			return fmt.Errorf("resolve agent root: %w", err)
		}
	} else {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return fmt.Errorf("resolve agent cwd: %w", err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve agent cwd: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil || cwd != root {
		return errors.New("agent cwd mismatch")
	}
	brief, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read agent brief: %w", err)
	}
	if err := recordAgentInvocation(spec.RecordRoot, root, brief); err != nil {
		return err
	}
	if spec.Behavior.Sleep > 0 {
		time.Sleep(spec.Behavior.Sleep)
	}
	if len(spec.DeletePaths) != 0 || len(spec.EditPaths) != 0 || len(spec.Behavior.GitArgs) != 0 {
		if err := withWorkspaceMutation(root, func() error {
			for _, path := range spec.DeletePaths {
				if err := secureRemove(root, path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("delete %q: %w", path, err)
				}
			}
			for _, path := range spec.EditPaths {
				if err := secureWrite(root, path, []byte(spec.Behavior.Edits[path]), 0o600); err != nil {
					return fmt.Errorf("edit %q: %w", path, err)
				}
			}
			if len(spec.Behavior.GitArgs) != 0 {
				command := exec.Command("git", spec.Behavior.GitArgs...)
				command.Dir = root
				var output bytes.Buffer
				command.Stdout = &output
				command.Stderr = &output
				if err := command.Run(); err != nil {
					return fmt.Errorf("run fixture git: %w: %s", err, output.String())
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if spec.Behavior.MalformedJSONL {
		_, err = fmt.Fprintln(os.Stdout, "{malformed")
	} else {
		_, err = fmt.Fprintln(os.Stdout, `{"type":"turn.completed"}`)
	}
	return err
}

func recordAgentInvocation(recordRoot, root string, brief []byte) error {
	directory, err := os.OpenRoot(recordRoot)
	if err != nil {
		return fmt.Errorf("open agent invocation root: %w", err)
	}
	defer directory.Close()
	for n := 1; ; n++ {
		name := strconv.Itoa(n)
		if err := directory.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("create agent invocation: %w", err)
		}
		if err := directory.WriteFile(name+"/root", []byte(root), 0o600); err != nil {
			return fmt.Errorf("record agent root: %w", err)
		}
		if err := directory.WriteFile(name+"/brief", brief, 0o600); err != nil {
			return fmt.Errorf("record agent brief: %w", err)
		}
		return nil
	}
}

// AgentInvocations returns the fake agent's recorded calls in invocation order.
func (e *Environment) AgentInvocations(name string) ([]AgentInvocation, error) {
	if err := validateFixtureName("agent", name); err != nil {
		return nil, err
	}
	root := filepath.Join(e.TempRoot, "agents", name, "invocations")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent invocations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		left, _ := strconv.Atoi(entries[i].Name())
		right, _ := strconv.Atoi(entries[j].Name())
		return left < right
	})
	result := make([]AgentInvocation, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		invocationRoot := filepath.Join(root, entry.Name())
		rootBytes, rootErr := os.ReadFile(filepath.Join(invocationRoot, "root"))
		briefBytes, briefErr := os.ReadFile(filepath.Join(invocationRoot, "brief"))
		if err := errors.Join(ignoreMissing(rootErr), ignoreMissing(briefErr)); err != nil {
			return nil, fmt.Errorf("read agent invocation %q: %w", entry.Name(), err)
		}
		result = append(result, AgentInvocation{Root: string(rootBytes), Brief: string(briefBytes)})
	}
	return result, nil
}

func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
