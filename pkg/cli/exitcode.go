package cli

import (
	"errors"
	"fmt"
	"io"
)

// ExitCode maps err — the error Execute returned on the root command — to
// the process exit code the caller must end with, writing any error report
// it owes the operator to stderr. It is the single mapping every binary that
// embeds pkg/cli shares: an embedding binary imports pkg/cli, so a new sentinel
// is mapped once where every embedding binary can adopt it, and
// TestExitCodeCoversEveryExportedSentinel fails until it is.
//
// The mapping is an observable exit-code contract:
//
//   - ErrNoCommand and *ErrUsage exit 2 (usage error). Both have already
//     printed everything the operator needs — help, or the flag error plus
//     usage — so nothing more is written.
//   - ErrCheckFailed exits 1 (lint-style failure). The full report has
//     already printed, so nothing more is written.
//   - *ErrExitCode propagates its Code exactly. Whoever returned it has
//     already told the operator what it needed to (the passthrough's child
//     inherits stdout/stderr; `upgrade --check` prints its answer), so
//     nothing is written.
//   - any other error exits 1, and because no sentinel has reported it,
//     this is the one place a failing command's error prints, as a
//     "stratum: <message>" line on stderr.
//
// A nil err is success: exit 0, nothing written. Callers can therefore hand
// Execute's return value straight over and end with the returned code:
//
//	if code := cli.ExitCode(root.Execute(), os.Stderr); code != 0 {
//		os.Exit(code)
//	}
func ExitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var usageErr *ErrUsage
	var exitErr *ErrExitCode
	switch {
	case errors.Is(err, ErrNoCommand):
		// Help has already printed; a bare invocation is a usage error.
		return 2
	case errors.As(err, &usageErr):
		// The flag error and usage have already printed.
		return 2
	case errors.Is(err, ErrCheckFailed):
		// The report has already printed; this is a lint-style exit 1.
		return 1
	case errors.As(err, &exitErr):
		// The command's answer is its exit status, already reported.
		return exitErr.Code
	default:
		// No sentinel reported it: this is the one place a failing
		// command's error prints.
		fmt.Fprintln(stderr, "stratum:", err)
		return 1
	}
}
