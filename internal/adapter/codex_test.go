package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	helperArgsFile   = "helper-args.json"
	helperCWDFile    = "helper-cwd"
	helperStdinFile  = "helper-stdin"
	helperStdoutFile = "helper-stdout"
	helperExitFile   = "helper-exit"
	helperDelayFile  = "helper-delay"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == "--ask-for-approval" && os.Args[2] == "never" && os.Args[3] == "exec" {
		runCodexHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runCodexHelper() {
	root := argumentValue(os.Args[1:], "--cd")
	args, _ := json.Marshal(os.Args)
	_ = os.WriteFile(filepath.Join(root, helperArgsFile), args, 0o600)
	workingDir, _ := os.Getwd()
	_ = os.WriteFile(filepath.Join(root, helperCWDFile), []byte(workingDir), 0o600)
	stdin, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(filepath.Join(root, helperStdinFile), stdin, 0o600)
	delayText, _ := os.ReadFile(filepath.Join(root, helperDelayFile))
	delay, _ := time.ParseDuration(strings.TrimSpace(string(delayText)))
	time.Sleep(delay)
	stdout, _ := os.ReadFile(filepath.Join(root, helperStdoutFile))
	_, _ = os.Stdout.Write(stdout)
	exitText, _ := os.ReadFile(filepath.Join(root, helperExitFile))
	exitCode, _ := strconv.Atoi(strings.TrimSpace(string(exitText)))
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func argumentValue(args []string, name string) string {
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

type recordingSink struct {
	writes [][]byte
}

func (s *recordingSink) WriteAdapterJSONL(data []byte) error {
	s.writes = append(s.writes, append([]byte(nil), data...))
	return nil
}

func TestCodexRunsExactCommandAndReturnsUsage(t *testing.T) {
	root := t.TempDir()
	stdout := []byte("{\"type\":\"item.completed\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":41,\"cached_input_tokens\":17,\"output_tokens\":9}}\n")
	writeHelperOutput(t, root, stdout)
	sink := &recordingSink{}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	result, err := NewCodex(executable).Run(context.Background(), Request{
		Root:  root,
		Brief: "batch brief\n",
		Sink:  sink,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantUsage := &Usage{InputTokens: 41, CachedInputTokens: 17, OutputTokens: 9}
	if !reflect.DeepEqual(result.Usage, wantUsage) {
		t.Fatalf("usage = %#v, want %#v", result.Usage, wantUsage)
	}
	if got, want := sink.writes, [][]byte{stdout}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sink writes = %q, want %q", got, want)
	}

	var gotArgs []string
	readHelperJSON(t, root, helperArgsFile, &gotArgs)
	wantArgs := []string{
		executable, "--ask-for-approval", "never",
		"exec", "--ephemeral", "--json",
		"--sandbox", "workspace-write",
		"--ignore-user-config",
		"--cd", root, "-",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", gotArgs, wantArgs)
	}
	if got := string(readHelperFile(t, root, helperCWDFile)); got != root {
		t.Fatalf("cwd = %q, want %q", got, root)
	}
	if got, want := string(readHelperFile(t, root, helperStdinFile)), "batch brief\n"; got != want {
		t.Fatalf("stdin = %q, want %q", got, want)
	}
	if got, want := NewCodex(executable).Name(), "codex"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

func TestCodexAllowsCompletedTurnWithoutUsage(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("{\"type\":\"turn.completed\"}\n"))

	result, err := runTestCodex(t, root, &recordingSink{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Usage != nil {
		t.Fatalf("usage = %#v, want nil", result.Usage)
	}
}

func TestCodexAllowsZeroUsage(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":0,\"cached_input_tokens\":0,\"output_tokens\":0}}\n"))

	result, err := runTestCodex(t, root, &recordingSink{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := result.Usage, (&Usage{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %#v, want %#v", got, want)
	}
}

func TestCodexRejectsNegativeUsage(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage Usage
	}{
		{name: "input tokens", usage: Usage{InputTokens: -1}},
		{name: "cached input tokens", usage: Usage{CachedInputTokens: -1}},
		{name: "output tokens", usage: Usage{OutputTokens: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			output, err := json.Marshal(struct {
				Type  string `json:"type"`
				Usage Usage  `json:"usage"`
			}{Type: "turn.completed", Usage: test.usage})
			if err != nil {
				t.Fatalf("marshal helper output: %v", err)
			}
			writeHelperOutput(t, root, append(output, '\n'))

			_, err = runTestCodex(t, root, &recordingSink{})
			assertAdapterError(t, err, true)
		})
	}
}

func TestCodexRejectsOutputWithoutCompletedTurn(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("{\"type\":\"item.completed\"}\n"))

	_, err := runTestCodex(t, root, &recordingSink{})
	assertAdapterError(t, err, true)
}

func TestCodexRejectsMalformedJSONL(t *testing.T) {
	root := t.TempDir()
	stdout := []byte("{\"type\":\"turn.completed\"}\n{\"type\":")
	writeHelperOutput(t, root, stdout)
	sink := &recordingSink{}

	_, err := runTestCodex(t, root, sink)
	assertAdapterError(t, err, true)
	if got, want := sink.writes, [][]byte{stdout}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sink writes = %q, want %q", got, want)
	}
}

func TestCodexDoesNotExposeMalformedJSONLInErrors(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("{\"type\":secret-value}\n"))

	_, err := runTestCodex(t, root, &recordingSink{})
	assertAdapterError(t, err, true)
	if got, want := err.Error(), "decode codex JSONL: malformed JSON object"; got != want {
		t.Fatalf("error = %q, want sanitized %q", got, want)
	}
}

func TestCodexRequiresAJSONObjectOnEveryLine(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("null\n{\"type\":\"turn.completed\"}\n"))

	_, err := runTestCodex(t, root, &recordingSink{})
	assertAdapterError(t, err, true)
}

func TestCodexRejectsTruncatedOutput(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte(strings.Repeat("x", 1<<20+1)))
	sink := &recordingSink{}

	_, err := runTestCodex(t, root, sink)
	assertAdapterError(t, err, true)
	if len(sink.writes) != 1 {
		t.Fatalf("sink writes = %d, want 1", len(sink.writes))
	}
	if got, want := len(sink.writes[0]), 1<<20; got != want {
		t.Fatalf("persisted bytes = %d, want bounded %d", got, want)
	}
}

func TestCodexPersistsOutputOnProcessFailure(t *testing.T) {
	root := t.TempDir()
	stdout := []byte("{\"type\":\"turn.completed\"}\n")
	writeHelperOutput(t, root, stdout)
	if err := os.WriteFile(filepath.Join(root, helperExitFile), []byte("7"), 0o600); err != nil {
		t.Fatalf("write helper exit: %v", err)
	}
	sink := &recordingSink{}

	_, err := runTestCodex(t, root, sink)
	assertAdapterError(t, err, true)
	if got, want := sink.writes, [][]byte{stdout}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sink writes = %q, want %q", got, want)
	}
}

func TestCodexClassifiesTimeoutAsRetryable(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("{\"type\":\"turn.completed\"}\n"))
	if err := os.WriteFile(filepath.Join(root, helperDelayFile), []byte("1s"), 0o600); err != nil {
		t.Fatalf("write helper delay: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = NewCodex(executable).Run(ctx, Request{Root: root, Brief: "brief", Sink: &recordingSink{}})
	assertAdapterError(t, err, true)
	if _, statErr := os.Stat(filepath.Join(root, helperStdinFile)); statErr != nil {
		t.Fatalf("helper did not start before timeout: %v", statErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestCodexPreservesCancellationAfterProcessStarts(t *testing.T) {
	root := t.TempDir()
	writeHelperOutput(t, root, []byte("{\"type\":\"turn.completed\"}\n"))
	if err := os.WriteFile(filepath.Join(root, helperDelayFile), []byte("2s"), 0o600); err != nil {
		t.Fatalf("write helper delay: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errResult := make(chan error, 1)
	go func() {
		_, runErr := NewCodex(executable).Run(ctx, Request{Root: root, Brief: "brief", Sink: &recordingSink{}})
		errResult <- runErr
	}()
	waitForFile(t, filepath.Join(root, helperStdinFile))
	cancel()

	err = <-errResult
	assertAdapterError(t, err, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestCodexClassifiesMissingExecutableAsNonRetryable(t *testing.T) {
	root := t.TempDir()
	sink := &recordingSink{}
	missing := filepath.Join(root, "missing-codex")

	_, err := NewCodex(missing).Run(context.Background(), Request{Root: root, Brief: "brief", Sink: sink})
	assertAdapterError(t, err, false)
	if len(sink.writes) != 1 || len(sink.writes[0]) != 0 {
		t.Fatalf("sink writes = %#v, want one empty write", sink.writes)
	}
}

func TestCodexClassifiesInvalidSetupAsNonRetryable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	for _, test := range []struct {
		name       string
		executable string
		root       string
	}{
		{name: "blank executable", root: t.TempDir()},
		{name: "blank root", executable: executable},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewCodex(test.executable).Run(context.Background(), Request{
				Root: test.root, Brief: "brief", Sink: &recordingSink{},
			})
			assertAdapterError(t, err, false)
		})
	}
}

func runTestCodex(t *testing.T, root string, sink Sink) (Result, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	return NewCodex(executable).Run(context.Background(), Request{Root: root, Brief: "brief", Sink: sink})
}

func assertAdapterError(t *testing.T, err error, retryable bool) {
	t.Helper()
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	var adapterErr *Error
	if !errors.As(err, &adapterErr) {
		t.Fatalf("error = %T %v, want *Error", err, err)
	}
	if adapterErr.Retryable != retryable {
		t.Fatalf("Retryable = %v, want %v", adapterErr.Retryable, retryable)
	}
}

func writeHelperOutput(t *testing.T, root string, output []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, helperStdoutFile), output, 0o600); err != nil {
		t.Fatalf("write helper output: %v", err)
	}
}

func readHelperFile(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func readHelperJSON(t *testing.T, root, name string, target any) {
	t.Helper()
	if err := json.Unmarshal(readHelperFile(t, root, name), target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
