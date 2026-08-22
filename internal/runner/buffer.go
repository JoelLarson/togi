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

// NewBuffer returns a Buffer holding at most limit bytes including marker.
func NewBuffer(limit int, marker []byte) *Buffer {
	return &Buffer{limit: limit, marker: append([]byte(nil), marker...)}
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
	// A marker longer than the limit still gets written whole, so a capture
	// is never silently truncated without its sentinel.
	dataLimit := max(0, b.limit-len(b.marker))
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
