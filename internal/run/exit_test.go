package run

import (
	"errors"
	"fmt"
	"testing"
)

type testExitError struct {
	code int
}

func (e testExitError) Error() string { return "test exit error" }
func (e testExitError) ExitCode() int { return e.code }

type nilTestExitError struct{}

func (*nilTestExitError) Error() string { return "nil test exit error" }
func (*nilTestExitError) ExitCode() int { panic("called ExitCode on a nil receiver") }

func TestResolveExit(t *testing.T) {
	var typedNil *ExitError
	var otherTypedNil *nilTestExitError
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: 0},
		{name: "direct exit error", err: &ExitError{Code: 1}, want: 1},
		{name: "wrapped exit error", err: fmt.Errorf("run failed: %w", &ExitError{Code: 5}), want: 5},
		{name: "other exit coder", err: testExitError{code: 2}, want: 2},
		{name: "wrapped other exit coder", err: fmt.Errorf("lint failed: %w", testExitError{code: 3}), want: 3},
		{name: "highest published code", err: testExitError{code: 6}, want: 6},
		{name: "zero code", err: testExitError{code: 0}, want: 70},
		{name: "code above published range", err: testExitError{code: 7}, want: 70},
		{name: "negative code", err: testExitError{code: -1}, want: 70},
		{name: "generic error", err: errors.New("broken"), want: 70},
		{name: "typed nil exit error", err: typedNil, want: 70},
		{name: "other typed nil exit coder", err: otherTypedNil, want: 70},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveExit(tc.err); got != tc.want {
				t.Fatalf("ResolveExit() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExitErrorExposesExitCode(t *testing.T) {
	if got := (&ExitError{Code: 4}).ExitCode(); got != 4 {
		t.Fatalf("ExitCode() = %d, want 4", got)
	}
}
