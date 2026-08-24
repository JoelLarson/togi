package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("TOGI_RUNNER_EXTRA_FILE_HELPER") != "" {
		contents, err := os.ReadFile("/proc/self/fd/3/marker")
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(contents)
		if os.Getenv("TOGI_RUNNER_EXTRA_FILE_BLOCK") != "" {
			for {
				time.Sleep(time.Hour)
			}
		}
		os.Exit(0)
	}
	if os.Getenv("TOGI_RUNNER_STDIN_HELPER") != "" {
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunPassesExplicitExtraFilesWithoutTakingOwnership(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc descriptor binding is Linux-only")
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "marker"), []byte("bound\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result := Run(context.Background(), ".", []string{executable, "-test.run=TestRunExtraFileHelper"}, Options{
		Env: append(os.Environ(), "TOGI_RUNNER_EXTRA_FILE_HELPER=1"), ExtraFiles: []*os.File{root}, StdoutLimit: 64, StderrLimit: 64,
	})
	if result.RunErr != nil || result.CleanupErr != nil || string(result.Stdout.Bytes()) != "bound\n" {
		t.Fatalf("Run() = (%q, %v, %v)", result.Stdout.Bytes(), result.RunErr, result.CleanupErr)
	}
	if _, err := root.Stat(); err != nil {
		t.Fatalf("Run() closed caller-owned descriptor: %v", err)
	}
}

func TestRunCancellationReapsChildWithoutClosingExtraFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc descriptor binding is Linux-only")
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "marker"), []byte("bound\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := Run(ctx, ".", []string{executable, "-test.run=TestRunExtraFileHelper"}, Options{
		Env: append(os.Environ(), "TOGI_RUNNER_EXTRA_FILE_HELPER=1", "TOGI_RUNNER_EXTRA_FILE_BLOCK=1"), ExtraFiles: []*os.File{root}, StdoutLimit: 64, StderrLimit: 64,
	})
	if !errors.Is(result.RunErr, context.DeadlineExceeded) && ctx.Err() == nil {
		t.Fatalf("Run() cancellation = %v", result.RunErr)
	}
	if _, err := root.Stat(); err != nil {
		t.Fatalf("canceled Run() closed caller-owned descriptor: %v", err)
	}
}

func TestRunSuppliesStdin(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	result := Run(context.Background(), ".", []string{executable, "-test.run=TestRunStdinHelper"}, Options{
		Env:         append(os.Environ(), "TOGI_RUNNER_STDIN_HELPER=1"),
		Stdin:       strings.NewReader("batch brief\n"),
		StdoutLimit: 64,
		StderrLimit: 64,
	})
	if result.RunErr != nil || result.CleanupErr != nil {
		t.Fatalf("Run() errors = (%v, %v), stderr = %q", result.RunErr, result.CleanupErr, result.Stderr.Bytes())
	}
	if got, want := string(result.Stdout.Bytes()), "batch brief\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestBufferTruncatesOnceAtLimitWithMarker(t *testing.T) {
	marker := []byte("[cut]")
	buffer := NewBuffer(16, marker)
	if n, err := buffer.Write(bytes.Repeat([]byte("a"), 10)); n != 10 || err != nil {
		t.Fatalf("Write = (%d, %v)", n, err)
	}
	if buffer.Truncated() {
		t.Fatal("buffer truncated below its limit")
	}
	if n, err := buffer.Write(bytes.Repeat([]byte("b"), 10)); n != 10 || err != nil {
		t.Fatalf("overflow Write = (%d, %v)", n, err)
	}
	if !buffer.Truncated() || buffer.Len() != 16 {
		t.Fatalf("buffer = %q (len %d, truncated %v)", buffer.Bytes(), buffer.Len(), buffer.Truncated())
	}
	if got := string(buffer.Bytes()); !strings.HasSuffix(got, string(marker)) {
		t.Fatalf("buffer = %q, want %q suffix", got, marker)
	}
	if n, err := buffer.Write([]byte("more")); n != 4 || err != nil || buffer.Len() != 16 {
		t.Fatalf("post-truncation Write grew the buffer: %q", buffer.Bytes())
	}
}

func TestBufferClampsOversizedMarkerToLimit(t *testing.T) {
	marker := []byte("[output truncated]")
	buffer := NewBuffer(8, marker)

	if n, err := buffer.Write([]byte("overflowing output")); n != 18 || err != nil {
		t.Fatalf("Write = (%d, %v), want (18, nil)", n, err)
	}
	if got, want := string(buffer.Bytes()), string(marker[:8]); got != want {
		t.Fatalf("buffer = %q, want marker prefix %q", got, want)
	}
	if buffer.Len() != 8 || !buffer.Truncated() {
		t.Fatalf("buffer len = %d, truncated = %v; want 8, true", buffer.Len(), buffer.Truncated())
	}
}

func TestBufferNonpositiveLimitDiscardsNonemptyWrites(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			buffer := NewBuffer(limit, []byte("[cut]"))

			if n, err := buffer.Write([]byte("output")); n != 6 || err != nil {
				t.Fatalf("Write = (%d, %v), want (6, nil)", n, err)
			}
			if buffer.Len() != 0 || !buffer.Truncated() {
				t.Fatalf("buffer len = %d, truncated = %v; want 0, true", buffer.Len(), buffer.Truncated())
			}
			if n, err := buffer.Write([]byte("more")); n != 4 || err != nil || buffer.Len() != 0 {
				t.Fatalf("post-truncation Write = (%d, %v), len = %d; want (4, nil), 0", n, err, buffer.Len())
			}
		})
	}
}

func TestBufferCopiesMarker(t *testing.T) {
	marker := []byte("[cut]")
	buffer := NewBuffer(5, marker)
	copy(marker, "xxxxx")

	if _, err := buffer.Write([]byte("overflow")); err != nil {
		t.Fatalf("Write error = %v", err)
	}
	if got, want := string(buffer.Bytes()), "[cut]"; got != want {
		t.Fatalf("buffer = %q, want %q", got, want)
	}
}

func TestRunRejectsInvalidRequestsThroughRunErr(t *testing.T) {
	result := Run(nil, ".", []string{"true"}, Options{StdoutLimit: 64, StderrLimit: 64}) //nolint:staticcheck // nil context is the case under test
	if result.RunErr == nil || result.Stdout == nil || result.Stderr == nil {
		t.Fatalf("nil-context result = %#v", result)
	}
}
