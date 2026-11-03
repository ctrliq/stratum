package cli

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

// nodeGateClientset is the client `stratum node` writes through: this control
// plane's internal admin kubeconfig, super-admin.conf when it exists, else
// admin.conf (NodeAdminClientset). The API server's audit log records that
// shared identity, so the --by annotation is the record of who acted.
var nodeGateClientset = func() (kubernetes.Interface, error) {
	cs, err := NodeAdminClientset()
	if err != nil {
		return nil, fmt.Errorf("%w; run `stratum node` on a control plane", err)
	}
	return cs, nil
}

// nodeGateNow is the time a release or hold is recorded at.
var nodeGateNow = time.Now

func newNodeCommand() *cobra.Command {
	return newGroupCommand("node", "Hold a node behind the startup taint, or release it",
		newNodeReleaseCommand(), newNodeHoldCommand())
}

// oneNodeName takes exactly one node name and refuses an empty one, as an
// unset shell variable gives, before the command connects.
func oneNodeName(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return err
	}
	if strings.TrimSpace(args[0]) == "" {
		return errors.New("the node name is empty")
	}
	return nil
}

// kubectlHint is the `stratum kubectl` command for a follow-up step, naming
// this control plane's admin kubeconfig so a KUBECONFIG set for another
// cluster does not take it there.
func kubectlHint(args string) string {
	return "stratum kubectl --kubeconfig " + version.AdminConf + " " + args
}

// defaultActor is who a release or hold is recorded as without --by: the
// user who ran sudo, else the current user.
func defaultActor() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func newNodeReleaseCommand() *cobra.Command {
	var by string
	var force bool
	cmd := &cobra.Command{
		Use:   "release <node>",
		Short: "Remove the " + k8sutil.UnverifiedTaint + " taint and record who verified the node",
		Long: "Removes the " + k8sutil.UnverifiedTaint + " taint from a node and records who released it, and when, in the\n" +
			k8sutil.VerifiedAnnotation + " annotation, and removes the " + k8sutil.HeldAnnotation + " and " + k8sutil.HeldReasonAnnotation + "\n" +
			"annotations, in one update. It refuses a node without the taint unless --force.\n" +
			"Run it on a control plane.",
		Args: oneNodeName,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := k8sutil.ValidateNodeGateActor(by); err != nil {
				return fmt.Errorf("--by: %w", err)
			}
			cs, err := nodeGateClientset()
			if err != nil {
				return err
			}
			res, err := k8sutil.ReleaseNode(cmd.Context(), cs, args[0], by, force, nodeGateNow())
			if err != nil {
				if errors.Is(err, k8sutil.ErrNotHeld) {
					return fmt.Errorf("%w; pass --force to record it as verified anyway", err)
				}
				return err
			}
			out := cmd.OutOrStdout()
			if res.Removed {
				fmt.Fprintf(out, "released %s: removed the %s taint (verified by %s)\n", args[0], k8sutil.UnverifiedTaint, by)
			} else {
				fmt.Fprintf(out, "recorded %s as verified by %s; it did not carry the %s taint\n", args[0], by, k8sutil.UnverifiedTaint)
			}
			if res.Cordoned {
				fmt.Fprintf(out, "%s is still cordoned, as a drain leaves it: run `%s` once it is ready for workloads\n", args[0], kubectlHint("uncordon "+args[0]))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", defaultActor(), "who verified the node, recorded in the "+k8sutil.VerifiedAnnotation+" annotation")
	cmd.Flags().BoolVar(&force, "force", false, "record the node as verified even when it does not carry the taint")
	return cmd
}

func newNodeHoldCommand() *cobra.Command {
	var by, reason string
	cmd := &cobra.Command{
		Use:   "hold <node>",
		Short: "Add the " + k8sutil.UnverifiedTaint + " taint so new pods that do not tolerate it stay off the node",
		Long: "Adds the " + k8sutil.UnverifiedTaint + "=true:NoSchedule taint to a node, removes its " + k8sutil.VerifiedAnnotation + " annotation,\n" +
			"and records who held it and when in the " + k8sutil.HeldAnnotation + " annotation and why in the\n" +
			k8sutil.HeldReasonAnnotation + " annotation, in one update. Pods already running stay; drain the node to move\n" +
			"the evictable ones (DaemonSet and static pods stay). A drain also cordons it, which keeps a\n" +
			"burn-in job off too: after the maintenance, uncordon it while it is still held, run the burn-in,\n" +
			"then release it. Run it on a control plane.",
		Args: oneNodeName,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := k8sutil.ValidateNodeGateActor(by); err != nil {
				return fmt.Errorf("--by: %w", err)
			}
			if err := k8sutil.ValidateHoldReason(reason); err != nil {
				return fmt.Errorf("--reason: %w", err)
			}
			cs, err := nodeGateClientset()
			if err != nil {
				return err
			}
			if err := k8sutil.HoldNode(cmd.Context(), cs, args[0], by, reason, nodeGateNow()); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "held %s: new pods without a toleration for %s no longer schedule onto it, until `stratum node release %s`\n", args[0], k8sutil.UnverifiedTaint, args[0])
			fmt.Fprintf(out, "pods already running stay; run `%s` to move the evictable ones (DaemonSet and static pods stay), which also cordons it and keeps a burn-in job off: "+
				"after the maintenance, run `%s` while it is still held, run the burn-in, then release it\n",
				kubectlHint("drain "+args[0]+" --ignore-daemonsets"), kubectlHint("uncordon "+args[0]))
			fmt.Fprintln(out, "a drain refuses pods with emptyDir volumes, CoreDNS among them, and their data is lost when they move: "+
				"check it, and add --delete-emptydir-data only if losing it is acceptable")
			fmt.Fprintf(out, "CoreDNS has no PodDisruptionBudget: before the drain, check with `%s` that another replica, not Terminating, "+
				"is Running and Ready on another Ready node; on a single-node cluster, cluster DNS is down until you uncordon and release it, "+
				"since CoreDNS does not tolerate the hold\n",
				kubectlHint("-n kube-system get pods -l k8s-app=kube-dns -o wide"))
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", defaultActor(), "who held the node, recorded in the "+k8sutil.HeldAnnotation+" annotation")
	cmd.Flags().StringVar(&reason, "reason", "", "why the node is held (required)")
	return cmd
}
