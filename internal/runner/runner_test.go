package runner

import (
	"bytes"
	"fmt"
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
