// Package upgrade implements the stratum upgrade and rollback commands.
package upgrade

import (
	"fmt"
	"strconv"

	"github.com/ctrliq/stratum/internal/k8sver"
)

// Node is the orchestrator's view of a cluster node.
type Node struct {
	Name    string
	IP      string // InternalIP
	IsCP    bool
	Ready   bool
	Version string // status.nodeInfo.kubeletVersion, e.g. "v1.36.2"
	// The version a cluster upgrade was taking this node to and has not finished.
	InProgress string
	// status.nodeInfo.architecture ("amd64", "arm64"); same-arch nodes pin to --from's exact bytes.
	Arch string
}

// Interrupted reports the version an unfinished upgrade recorded, even after the kubelet swap.
func (n Node) Interrupted() string {
	if n.InProgress != "" && n.Version == n.InProgress {
		return n.InProgress
	}
	return ""
}

func (n Node) NeedsUpgrade(target string) bool {
	return n.Version != target || n.Interrupted() == target
}

// NeedsBuildCheck: at target by a finished upgrade, so only the stratum build can differ.
func (n Node) NeedsBuildCheck(target string) bool {
	return !n.NeedsUpgrade(target)
}

// Preflight reports "" when the upgrade is safe, else a human reason.
func Preflight(target string, nodes []Node) string {
	t, ok := k8sver.ParseTarget(target)
	if !ok {
		if err := suffixedTarget(target, func(base string) bool { _, ok := k8sver.ParseTarget(base); return ok }); err != nil {
			return err.Error()
		}
		// Quoted: a rejected target is user-facing and may carry control bytes.
		return "target must be vMAJOR.MINOR.PATCH (e.g. v1.36.2), got " + strconv.Quote(target)
	}
	// The downgrade gate holds for the highest node; the skew and cross-major gates for the lowest.
	var lo, hi k8sver.Version
	seen := false
	cpTotal, cpReady := 0, 0
	for _, n := range nodes {
		// Refused before any version logic; which way to go is the operator's call.
		if iv := n.Interrupted(); iv != "" && iv != target {
			return fmt.Sprintf("node %s has an unfinished upgrade to %s (interrupted after its kubelet swap); "+
				"finish it with `stratum upgrade --cluster --target %s`, or undo it with `stratum rollback` on that node, "+
				"then retry", n.Name, iv, iv)
		}
		if v, ok := k8sver.Parse(n.Version); ok {
			if !seen {
				lo, hi, seen = v, v, true
			}
			if v.Less(lo) {
				lo = v
			}
			if hi.Less(v) {
				hi = v
			}
		}
		if n.IsCP {
			cpTotal++
			if n.Ready {
				cpReady++
			}
		}
	}
	if seen {
		// Cross-major must clear both watermarks, or a mixed-major cluster escapes every check below.
		if t.Major != lo.Major || t.Major != hi.Major {
			if lo.Major != hi.Major {
				return fmt.Sprintf("cluster spans major versions %s to %s; resolve that before upgrading to %s", lo, hi, t)
			}
			return fmt.Sprintf("cross-major change not allowed (target %s, cluster at %s)", t, lo)
		}
		// A full triple comparison: major/minor alone would wave a patch downgrade through.
		if t.Less(hi) {
			return fmt.Sprintf("downgrade not allowed (target %s < highest node %s)", t, hi)
		}
		if t.Minor > lo.Minor+1 {
			return fmt.Sprintf("cannot skip minor versions (target %s, lowest node %s; upgrade one minor at a time)", t, lo)
		}
	}
	if cpTotal > 0 && cpReady <= cpTotal/2 {
		return fmt.Sprintf("control-plane not quorum-healthy: %d/%d CP nodes Ready", cpReady, cpTotal)
	}
	// With a control plane already down, draining a Ready one must still leave a majority.
	if cpReady < cpTotal && cpReady-1 <= cpTotal/2 {
		return fmt.Sprintf("control-plane has no margin: %d/%d CP nodes Ready, and draining another would leave "+
			"etcd without quorum; bring the NotReady control plane back (or upgrade it on its own with "+
			"`stratum upgrade --from`) and retry", cpReady, cpTotal)
	}
	return ""
}
