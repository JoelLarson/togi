package adapter

import "context"

// Sink persists raw adapter protocol output outside the target repository.
type Sink interface {
	WriteAdapterJSONL([]byte) error
}

// Request describes one agent invocation.
type Request struct {
	Root  string
	Brief string
	Sink  Sink
}

// Usage is the optional token accounting reported by an adapter.
type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// Result carries provider-neutral output from a completed invocation.
type Result struct {
	Usage *Usage
}

// Adapter runs an agent CLI against a worktree.
type Adapter interface {
	Name() string
	Run(context.Context, Request) (Result, error)
}

// Error classifies whether retrying an adapter invocation may succeed.
type Error struct {
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	if e == nil || e.Err == nil {
		return "adapter error"
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
