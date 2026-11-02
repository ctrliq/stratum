package cli

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// nodeGateClientset is the client `stratum node` writes through: this control
// plane's admin.conf.
var nodeGateClientset = func() (kubernetes.Interface, error) {
	cs, err := k8sutil.AdminClientset()
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
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := k8sutil.ValidateNodeGateActor(by); err != nil {
				return fmt.Errorf("--by: %w", err)
			}
			cs, err := nodeGateClientset()
			if err != nil {
				return err
			}
			if err := k8sutil.ReleaseNode(cmd.Context(), cs, args[0], by, force, nodeGateNow()); err != nil {
				if errors.Is(err, k8sutil.ErrNotHeld) {
					return fmt.Errorf("%w; pass --force to record it as verified anyway", err)
				}
				return err
			}
			out := cmd.OutOrStdout()
			n, err := cs.CoreV1().Nodes().Get(cmd.Context(), args[0], metav1.GetOptions{})
			if err == nil && n.Spec.Unschedulable {
				fmt.Fprintf(out, "released %s (verified by %s), but it is still cordoned, as a drain leaves it: run `stratum kubectl uncordon %s` once it is ready for workloads\n", args[0], by, args[0])
				return nil
			}
			fmt.Fprintf(out, "released %s: workloads can now schedule onto it (verified by %s)\n", args[0], by)
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
			"them, and uncordon it after `stratum node release`, which does not. Run it on a control plane.",
		Args: cobra.ExactArgs(1),
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
			fmt.Fprintf(out, "pods already running stay; run `stratum kubectl drain %s --ignore-daemonsets` to move them, "+
				"which also cordons it: uncordon it with `stratum kubectl uncordon %s` after the release\n", args[0], args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", defaultActor(), "who held the node, recorded in the "+k8sutil.HeldAnnotation+" annotation")
	cmd.Flags().StringVar(&reason, "reason", "", "why the node is held (required)")
	return cmd
}
