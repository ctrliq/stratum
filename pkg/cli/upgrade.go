package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/upgrade"
	"github.com/ctrliq/stratum/internal/version"
)

// upgradeNode is upgrade.UpgradeNode behind a seam: the node upgrade needs a
// real node, so a test can pin `upgrade --check`'s sentinel (*ErrExitCode)
// only by supplying the outcome itself.
var upgradeNode = upgrade.UpgradeNode

func newUpgradeCommand() *cobra.Command {
	var (
		clusterWide bool
		from        string
		target      string
		yes         bool
		wmu         int
		grace       int
		soak        int
		skipSnap    bool
		dry         bool
		check       bool
		importImgs  bool
		expect      string
		expectSum   string
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade this node, or roll the whole HA cluster with --cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if clusterWide && check {
				return fmt.Errorf("--check asks one node; it cannot be combined with --cluster")
			}
			if importImgs && (clusterWide || check || dry) {
				return fmt.Errorf("--import-images is one node's step of a cluster upgrade; it cannot be combined with --cluster, --check or --dry-run")
			}
			if clusterWide {
				// Before the confirmation gate and the orchestrator: a cluster
				// rolled from an unstamped binary renders unpullable :unknown
				// node images and, without --target, aims at Kubernetes
				// "unknown" (refuseUnstampedBuild has the full why).
				if err := refuseUnstampedBuild("stratum upgrade --cluster"); err != nil {
					return err
				}
				if !yes && !dry {
					return fmt.Errorf("--cluster performs a rolling upgrade of every node; re-run with --yes (or --dry-run to preview)")
				}
				if target == "" {
					target = version.K8sVersion
				}
				return upgrade.UpgradeCluster(os.Stdout, upgrade.ClusterOptions{
					Target: target, From: from, WorkersMaxUnavailable: wmu,
					DrainGraceSec: grace, SoakSec: soak, SkipSnapshot: skipSnap, DryRun: dry,
				})
			}
			// The node upgrade runs as the version it installs, so an installed
			// binary given a different --from becomes that binary.
			if err := upgrade.HandOff(os.Stdout, from, os.Args[1:], dry || check); err != nil {
				return err
			}
			err := upgradeNode(os.Stdout, upgrade.NodeOptions{
				Target: target, From: from, SoakSec: soak, SkipSnapshot: skipSnap, DryRun: dry, Check: check,
				ImportImages: importImgs, ExpectBuild: expect, ExpectSHA256: expectSum,
			})
			// --check answers in its exit status: the cluster orchestrator reads
			// it from the Job's pod before deciding whether to drain.
			if errors.Is(err, upgrade.ErrUpgradeNeeded) {
				fmt.Println("stratum upgrade: this node needs the upgrade")
				return &ErrExitCode{Code: upgrade.ExitUpgradeNeeded}
			}
			if errors.Is(err, upgrade.ErrPrereqMissing) {
				// A refused node is not a drainable one: its own exit status says
				// so, and the refusal itself is what the operator acts on.
				fmt.Fprintln(os.Stderr, err)
				return &ErrExitCode{Code: upgrade.ExitPrereqMissing}
			}
			return err
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&clusterWide, "cluster", false, "roll the whole HA cluster (run on a control-plane node; requires --from)")
	fs.StringVar(&from, "from", "", "path to the upgrade binary — the new stratum binary; run from the installed binary, it hands off to it (default: this binary's own embedded payload)")
	fs.StringVar(&target, "target", "", "target Kubernetes version, bare vMAJOR.MINOR.PATCH (default: one node uses --from binary if set, else this binary; --cluster uses this binary)")
	fs.BoolVar(&yes, "yes", false, "proceed with a --cluster rollout without the confirmation")
	fs.IntVar(&wmu, "workers-max-unavailable", 1, "(--cluster) upgrade this many workers concurrently (default 1 = serial); control planes are always serial")
	fs.IntVar(&grace, "drain-grace", 60, "drain grace period in seconds (--cluster)")
	fs.IntVar(&soak, "soak", 15, "health-gate soak in seconds")
	fs.BoolVar(&skipSnap, "skip-snapshot", false, "skip the pre-upgrade etcd snapshot on control-plane nodes")
	fs.BoolVar(&dry, "dry-run", false, "print the plan; make no changes")
	fs.BoolVar(&check, "check", false, fmt.Sprintf("report whether this node already runs the upgrade binary's version and build, and change nothing: exit 0 if it does, %d if it needs the upgrade, %d if it refuses because the node cannot take the upgrade (SELinux enforcing, or a profile that requires SELinux, with container-selinux, policycoreutils, policycoreutils-python-utils or libselinux-utils missing, or such a profile combined with spec.selinux: false), 1 if it cannot tell (e.g. another upgrade or rollback is running); not with --cluster", upgrade.ExitUpgradeNeeded, upgrade.ExitPrereqMissing))
	// Set by the cluster upgrade on every node's Job, never by hand: the build
	// the orchestrator read from its own --from, so a node whose staged copy is
	// another build refuses rather than answer for it.
	fs.StringVar(&expect, "expect-build", "", "refuse unless this upgrade binary has the given build ID")
	_ = fs.MarkHidden("expect-build")
	fs.StringVar(&expectSum, "expect-sha256", "", "refuse unless this upgrade binary has the given sha256")
	_ = fs.MarkHidden("expect-sha256")
	// Also set only by the cluster upgrade, on every node before the first is
	// upgraded: the add-ons roll out as soon as one control plane is, and the
	// other nodes must already hold their images.
	fs.BoolVar(&importImgs, "import-images", false, "import this upgrade binary's images into the node's runtime, and change nothing else")
	_ = fs.MarkHidden("import-images")
	return cmd
}

func newRollbackCommand() *cobra.Command {
	var (
		snap string
		soak int
		yes  bool
		dry  bool
	)
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Undo the last upgrade",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if snap != "" && !yes && !dry {
				return fmt.Errorf("--etcd-snapshot is disruptive (stops apiserver+etcd, restores data); re-run with --yes")
			}
			return upgrade.RollbackNode(os.Stdout, upgrade.RollbackOptions{
				SoakSec: soak, EtcdSnapshot: snap, DryRun: dry,
			})
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&snap, "etcd-snapshot", "", "also restore etcd from this snapshot (disruptive; single-CP only)")
	fs.IntVar(&soak, "soak", 15, "health-gate soak in seconds")
	fs.BoolVar(&yes, "yes", false, "required when --etcd-snapshot is set")
	fs.BoolVar(&dry, "dry-run", false, "print the plan; make no changes")
	return cmd
}
