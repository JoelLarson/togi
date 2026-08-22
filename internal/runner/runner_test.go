package runner

import (
	"bytes"
	"strings"
	"testing"
)

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

func TestRunRejectsInvalidRequestsThroughRunErr(t *testing.T) {
	result := Run(nil, ".", []string{"true"}, Options{StdoutLimit: 64, StderrLimit: 64}) //nolint:staticcheck // nil context is the case under test
	if result.RunErr == nil || result.Stdout == nil || result.Stderr == nil {
		t.Fatalf("nil-context result = %#v", result)
	}
}
