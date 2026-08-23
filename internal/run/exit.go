package run

import (
	"errors"
	"fmt"
	"reflect"
)

// ExitError carries the stable process status for a completed gauntlet run.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("togi exited with status %d", e.Code)
}

func (e *ExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ExitCode returns the stable process status carried by the error.
func (e *ExitError) ExitCode() int {
	if e == nil {
		return 0
	}
	return e.Code
}

// ResolveExit maps command results onto the published CLI exit codes.
func ResolveExit(err error) int {
	if err == nil {
		return 0
	}

	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || isNilExitCoder(exitCoder) {
		return 70
	}
	code := exitCoder.ExitCode()
	if code < 1 || code > 6 {
		return 70
	}
	return code
}

func isNilExitCoder(exitCoder interface{ ExitCode() int }) bool {
	value := reflect.ValueOf(exitCoder)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// ExitCode maps a persisted verdict onto the CLI contract.
func ExitCode(verdict Verdict) int {
	switch verdict {
	case VerdictFindings:
		return 1
	case VerdictBlocked:
		return 2
	case VerdictRails:
		return 3
	case VerdictErrored:
		return 4
	case VerdictUnverified:
		return 5
	case VerdictUnsealed:
		return 6
	default:
		return 70
	}
}
