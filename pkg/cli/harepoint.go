package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/ha"
)

func newHARepointCommand() *cobra.Command {
	var (
		vip    string
		cilium bool
	)
	cmd := &cobra.Command{
		Use:   "ha-repoint",
		Short: "Move this node's clients onto the cluster VIP",
		Long: `ha-repoint moves this node's clients (kubeconfigs + local components) onto
the cluster VIP. With --cilium it instead re-points Cilium cluster-wide and
leaves this node's clients as they are: run it without --cilium on each node
whose clients need moving, and with --cilium once for the whole cluster. Used
once when a cluster that began as a single controller grows into HA.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if vip == "" {
				return fmt.Errorf("ha-repoint: --vip is required")
			}
			// A failed best-effort restart is a warning beside a nil error
			// (the repoint landed), and the operation prints the warning
			// itself, so the command still succeeds and says so.
			if cilium {
				_, err := ha.RepointCilium(os.Stdout, vip)
				return err
			}
			_, err := ha.NodeRepoint(os.Stdout, vip)
			return err
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&vip, "vip", "", "cluster VIP address (required)")
	fs.BoolVar(&cilium, "cilium", false, "re-point the Cilium agents/operator at the VIP instead of this node's clients (run ONCE, cluster-wide)")
	return cmd
}
