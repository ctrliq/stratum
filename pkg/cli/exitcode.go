package cli

import (
	"errors"
	"fmt"
	"io"
)

// The exit-code contract every embedding binary shares; a new sentinel maps here once.
func ExitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var usageErr *ErrUsage
	var exitErr *ErrExitCode
	switch {
	case errors.Is(err, ErrNoCommand):
		// Help has already printed.
		return 2
	case errors.As(err, &usageErr):
		// The flag error and usage have already printed.
		return 2
	case errors.Is(err, ErrCheckFailed):
		// The report has already printed.
		return 1
	case errors.As(err, &exitErr):
		// Whoever returned it already told the operator.
		return exitErr.Code
	default:
		// The one place a failing command's error prints.
		fmt.Fprintln(stderr, "stratum:", err)
		return 1
	}
}
