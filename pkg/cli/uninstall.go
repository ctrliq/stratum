package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/uninstall"
)

func newUninstallCommand() *cobra.Command {
	var yes, assumeNoForeign bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Stratum from this node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The CLI prints to the process's stdout, the writer
			// RunWithOptions takes: same bytes, same stream.
			return uninstall.RunWithOptions(os.Stdout, uninstall.Options{Force: yes, AssumeNoForeignRuntimes: assumeNoForeign})
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&yes, "yes", false, "actually perform the uninstall (otherwise prints what would be removed)")
	fs.BoolVar(&yes, "force", false, "alias for --yes")
	fs.BoolVar(&assumeNoForeign, "assume-no-foreign-runtimes", false,
		"proceed when uninstall cannot see the host's processes (a private PID namespace); you have checked that no other kubelet or container runtime runs on this host")
	return cmd
}
