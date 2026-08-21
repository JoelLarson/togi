package run

import "fmt"

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

// ExitCode maps a persisted verdict onto the CLI contract.
func ExitCode(verdict Verdict) int {
	switch verdict {
	case VerdictFindings:
		return 1
	case VerdictErrored:
		return 4
	case VerdictUnverified:
		return 5
	default:
		return 70
	}
}
