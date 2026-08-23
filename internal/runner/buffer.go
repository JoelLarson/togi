package runner

import "bytes"

// Buffer captures a stream up to a fixed limit, appending a truncation
// marker exactly once when the limit is exceeded. Writes never fail; excess
// input is discarded so a chatty process cannot exhaust memory.
type Buffer struct {
	buffer    bytes.Buffer
	limit     int
	marker    []byte
	truncated bool
}

// NewBuffer returns a Buffer holding at most max(limit, 0) bytes including
// marker. When marker exceeds that budget, its prefix is retained so the
// capture keeps the sentinel's recognizable opening.
func NewBuffer(limit int, marker []byte) *Buffer {
	limit = max(limit, 0)
	return &Buffer{limit: limit, marker: append([]byte(nil), marker[:min(len(marker), limit)]...)}
}

func (b *Buffer) Write(input []byte) (int, error) {
	inputLength := len(input)
	if b.truncated {
		return inputLength, nil
	}
	remaining := b.limit - b.buffer.Len()
	if len(input) <= remaining {
		_, _ = b.buffer.Write(input)
		return inputLength, nil
	}
	dataLimit := b.limit - len(b.marker)
	if b.buffer.Len() > dataLimit {
		b.buffer.Truncate(dataLimit)
	} else if b.buffer.Len() < dataLimit {
		needed := dataLimit - b.buffer.Len()
		_, _ = b.buffer.Write(input[:min(needed, len(input))])
	}
	_, _ = b.buffer.Write(b.marker)
	b.truncated = true
	return inputLength, nil
}

func (b *Buffer) Bytes() []byte   { return b.buffer.Bytes() }
func (b *Buffer) Len() int        { return b.buffer.Len() }
func (b *Buffer) Truncated() bool { return b.truncated }
