package cli

import (
	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/operator"
)

// DisableFlagParsing: operator.ParseFlags owns its own flag.FlagSet; pass raw args.
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

// Unwrap pairs the stop with ErrExitCode{0} so main maps the stop to exit 0, unlogged.
type cleanStopExit struct{ stop error }

func (e *cleanStopExit) Error() string { return e.stop.Error() }

func (e *cleanStopExit) Unwrap() []error { return []error{&ErrExitCode{Code: 0}, e.stop} }
