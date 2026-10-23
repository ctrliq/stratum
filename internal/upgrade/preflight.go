// Package upgrade implements `stratum upgrade` / `stratum rollback`: a per-node
// binary-swap primitive and a `--cluster` orchestrator that rolls an HA cluster
// (serial control planes, then workers) natively, version-stamped and
// self-contained (the binary embeds the new component binaries).
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
	// InProgress is the node's UpgradeInProgressAnnotation: the version a
	// cluster upgrade was taking it to and has not yet seen it finish at.
	InProgress string
	// Arch is status.nodeInfo.architecture ("amd64", "arm64"): the nodes that
	// share the orchestrator's can be pinned to its --from's exact bytes.
	Arch string
}

// Interrupted returns the version an interrupted cluster upgrade left this
// node on, or "". The node-side upgrade swaps kubelet last, just
// before its soak and gate, so a Job killed in between leaves a kubelet that
// already reports the new version on a node whose upgrade never finished.
// Reporting the recorded version is what makes it that case: a node rolled
// back since reports the old one, and its annotation is only stale.
func (n Node) Interrupted() string {
	if n.InProgress != "" && n.Version == n.InProgress {
		return n.InProgress
	}
	return ""
}

// NeedsUpgrade reports whether a cluster upgrade to target must run on n: it
// is not at target, or it got there by an upgrade that never finished. The
// node side resumes that one, or finds it done, and the
// orchestrator's own post-Job checks run either way.
func (n Node) NeedsUpgrade(target string) bool {
	return n.Version != target || n.Interrupted() == target
}

// NeedsBuildCheck reports whether n is at target by a finished upgrade, so
// only its stratum build can differ from the upgrade binary's: a new build of
// the same Kubernetes release leaves the kubelet version where it was. The
// orchestrator asks the node itself before draining it.
func (n Node) NeedsBuildCheck(target string) bool {
	return !n.NeedsUpgrade(target)
}

// Preflight validates an upgrade is safe before any node is touched. Returns ""
// if OK, else a human reason. Enforces: valid target semver, no cross-major
// change, no downgrade, no minor-skip (one minor at a time per the k8s skew
// policy), and control-plane quorum-healthy: a majority of CP nodes Ready, and
// still a majority with one more drained while any is NotReady.
//
// The target is held to k8sver.ParseTarget (exact vMAJOR.MINOR.PATCH, every
// component fitting an int) because a user chose it. Observed node versions go
// through the lenient k8sver.Parse, because a kubelet legitimately reports
// "v1.31.2+k3s1". Do not "tighten" the node-version side: the gates below are
// guarded on having seen a parseable node version, so a parser that rejects real
// kubelet versions would skip the downgrade and skew checks altogether and fail
// OPEN.
func Preflight(target string, nodes []Node) string {
	t, ok := k8sver.ParseTarget(target)
	if !ok {
		if err := suffixedTarget(target, func(base string) bool { _, ok := k8sver.ParseTarget(base); return ok }); err != nil {
			return err.Error()
		}
		// Quoted: a rejected target may contain newlines or control bytes, and
		// this string is surfaced to users and written to logs.
		return "target must be vMAJOR.MINOR.PATCH (e.g. v1.36.2), got " + strconv.Quote(target)
	}
	// Two watermarks, because the three gates look in opposite directions. The
	// downgrade gate must hold for the HIGHEST node (going below it is the
	// downgrade). The skew and cross-major gates must hold for the LOWEST node:
	// every node converges on target, so the laggard is the one that would jump
	// several minors. Using the high watermark for those two lets a mixed
	// cluster — which halt-on-failure leaves behind — skip minors unnoticed.
	var lo, hi k8sver.Version
	seen := false
	cpTotal, cpReady := 0, 0
	for _, n := range nodes {
		// Before any version logic, and before anything is drained: the node
		// would refuse this upgrade anyway, holding the baseline of the other
		// one, and it is the operator's call which way to go.
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
		// Cross-major first, against BOTH watermarks. It has to clear the high
		// one too, or a mixed-major cluster escapes every check below: the
		// comparisons that follow are only meaningful once the whole cluster and
		// the target agree on a major.
		if t.Major != lo.Major || t.Major != hi.Major {
			if lo.Major != hi.Major {
				return fmt.Sprintf("cluster spans major versions %s to %s; resolve that before upgrading to %s", lo, hi, t)
			}
			return fmt.Sprintf("cross-major change not allowed (target %s, cluster at %s)", t, lo)
		}
		// Full triple comparison: major/minor alone would treat v1.36.1 as
		// equal to v1.36.2 and wave a patch downgrade through.
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
	// With a control plane already down, draining a Ready one must still
	// leave a majority: the rollout takes them one at a time in API order,
	// so it can reach a Ready member before the NotReady one. 2/3 Ready is
	// quorum now and not after the next drain. The NotReady
	// member can be upgraded on its own with `stratum upgrade --from`.
	if cpReady < cpTotal && cpReady-1 <= cpTotal/2 {
		return fmt.Sprintf("control-plane has no margin: %d/%d CP nodes Ready, and draining another would leave "+
			"etcd without quorum; bring the NotReady control plane back (or upgrade it on its own with "+
			"`stratum upgrade --from`) and retry", cpReady, cpTotal)
	}
	return ""
}
