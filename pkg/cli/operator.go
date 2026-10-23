package cli

import (
	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/operator"
)

// newOperatorCommand runs the in-binary operator (HelmChart + manifest
// deployer + CSR approver). Normally a supervised child of `stratum up`;
// runnable by hand for debugging. Flag parsing is disabled here:
// operator.ParseFlags owns its own flag.FlagSet, and passing the
// raw args straight through avoids cobra trying to interpret them itself.
func newOperatorCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "operator",
		Short:              "Run the CRD controllers",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := operator.ParseFlags(args)
			if err != nil {
				return err
			}
			ctx, stop := interrupt.DerivedContext(cmd.Context())
			defer stop()
			err = operator.Run(ctx, o)
			if interrupt.ExpectedStop(ctx, err) {
				return &cleanStopExit{stop: ctx.Err()}
			}
			return err
		},
	}
}

// cleanStopExit is a signal-caused stop's answer to main: the stop error
// itself, so it keeps surfacing through the CLI exactly as before (every
// reader still sees context.Canceled), paired with the *ErrExitCode main
// maps — the same type every other exit-code answer carries — so the stop
// exits 0 instead of being printed and exited 1 by main's default case. A
// host's cancellation and a real failure never become one, so both keep
// their own meaning and exit code.
type cleanStopExit struct{ stop error }

func (e *cleanStopExit) Error() string { return e.stop.Error() }

func (e *cleanStopExit) Unwrap() []error { return []error{&ErrExitCode{Code: 0}, e.stop} }
