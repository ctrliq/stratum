package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/k8sver"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/nodestate"
)

// apiRetryWindow is how long retryAPI keeps trying: the per-node health gate's
// window, since a control plane's own API server restarts inside it.
// apiCallTimeout bounds each attempt, so a request the API server accepts and
// never answers costs one attempt, not the window. Vars so tests can shorten
// them.
var (
	apiRetryWindow   = 5 * time.Minute
	apiRetryInterval = 2 * time.Second
	apiCallTimeout   = 10 * time.Second
)

// terminalAPIError is an answer that retrying cannot change: a lease or a
// node claim taken by another run, or an API 4xx other
// than a timeout, a conflict (the callers re-read) or throttling. Anything else,
// network errors and 5xx included, is the API server not being ready yet.
func terminalAPIError(err error) bool {
	if errors.Is(err, errLeaseTaken) || errors.Is(err, errClaimTaken) {
		return true
	}
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return false
	}
	c := st.Status().Code
	return c >= 400 && c < 500 && c != http.StatusRequestTimeout && c != http.StatusConflict && c != http.StatusTooManyRequests
}

// retryAPI runs f until it succeeds, fails terminally, or apiRetryWindow passes, for the writes
// that follow a node's gate: on a one-node cluster the API server they go to
// may still be coming back from the restart the upgrade caused. f gets a
// context that ends with the attempt's timeout and the window, and must make
// its API calls with it.
func retryAPI(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, apiRetryWindow)
	defer cancel()
	for {
		cctx, ccancel := context.WithTimeout(ctx, apiCallTimeout)
		err := f(cctx)
		ccancel()
		if err == nil || ctx.Err() != nil || terminalAPIError(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(apiRetryInterval):
		}
	}
}

func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// NodeFromAPI is one Node as the cluster upgrade plans with it. It is the
// canonical classifier, shared by classifyNodes and the exported Plan: a node
// is a control plane only when the Stratum label is exactly "true"; any other
// value, and the kubeadm role label alone, plans a worker.
func NodeFromAPI(n *corev1.Node) Node {
	return Node{
		Name:       n.Name,
		IP:         internalIP(n),
		Version:    NodeKubeletVersion(n.Status.NodeInfo.KubeletVersion),
		Ready:      nodejob.Ready(n),
		IsCP:       n.Labels[k8sutil.ControlPlaneLabel] == "true",
		InProgress: n.Annotations[k8sutil.UpgradeInProgressAnnotation],
		Arch:       n.Status.NodeInfo.Architecture,
	}
}

// NodeKubeletVersion is the kubelet's reported version without its build
// suffix: an SKE kubelet reports "v<k8s>-stratum", and every comparison here
// is against a bare --target. Compared raw, no node ever looked at target, so
// the plan could never say `check build` or `finish`. A version that
// does not parse is kept as reported, so the node is upgraded rather than
// taken for at target.
func NodeKubeletVersion(reported string) string {
	if v, err := parseVersion(reported); err == nil {
		return v
	}
	return reported
}

// nodeFromAPI is NodeFromAPI under the name this package's own tests pin.
func nodeFromAPI(n *corev1.Node) Node { return NodeFromAPI(n) }

// classifyNodes lists the cluster's nodes as the plan sees them, in the order
// the API served them. Control planes are marked by NodeFromAPI's label rule:
// only stratum.ciq.com/control-plane="true" counts. Plan adds the etcd-roster
// fallback for unlabeled clusters before preflight and ordering.
func classifyNodes(ctx context.Context, cs kubernetes.Interface) ([]Node, error) {
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(nl.Items))
	for i := range nl.Items {
		nodes = append(nodes, NodeFromAPI(&nl.Items[i]))
	}
	return nodes, nil
}

var (
	fallbackNodeIP    = nodestate.NodeIP
	fallbackMemberIPs = ha.MemberClientIPs
)

// controlPlaneFallback marks control planes by the etcd member roster when
// no node carries the Stratum control-plane label, supporting clusters created
// before install-time labeling. Preview and apply both run on a Stratum host
// and use its address and etcd client certificate to read the same roster.
func controlPlaneFallback(nodes []Node) error {
	labeled := false
	for _, n := range nodes {
		if n.IsCP {
			labeled = true
			break
		}
	}
	if labeled {
		return nil
	}
	// The state error first: dialling etcd at "" would report a misleading
	// roster failure for what is really a node-state problem.
	nodeIP, err := fallbackNodeIP()
	if err != nil {
		return fmt.Errorf("cannot classify control-plane nodes: no %s labels, and this node's address "+
			"for the etcd roster is unknown: %w", k8sutil.ControlPlaneLabel, err)
	}
	ips, err := fallbackMemberIPs(nodeIP)
	if err != nil {
		return fmt.Errorf("cannot classify control-plane nodes: no %s labels and etcd roster unavailable: %w (label CP nodes %s=true)",
			k8sutil.ControlPlaneLabel, err, k8sutil.ControlPlaneLabel)
	}
	cp := map[string]bool{nodeIP: true}
	for _, ip := range ips {
		cp[ip] = true
	}
	for i := range nodes {
		if cp[nodes[i].IP] {
			nodes[i].IsCP = true
		}
	}
	return nil
}

// orderedNodes returns control-plane nodes first (serial), then workers.
func orderedNodes(nodes []Node) []Node {
	var cp, wk []Node
	for _, n := range nodes {
		if n.IsCP {
			cp = append(cp, n)
		} else {
			wk = append(wk, n)
		}
	}
	return append(cp, wk...)
}

// markUpgradeInProgress is one attempt to record on the Node the version its
// upgrade Job is about to take it to, before the Job runs. The
// kubelet version alone cannot tell a finished node from one interrupted after
// its kubelet swap; this can, and only this orchestrator writes and clears it.
// It refuses, with errClaimTaken, a node whose claim names another run. The
// caller confirms the lease on every attempt.
func markUpgradeInProgress(ctx context.Context, cs kubernetes.Interface, name, target, holder string) error {
	_, err := updateNode(ctx, cs, name, func(n *corev1.Node) (bool, error) {
		if _, h := parseCordonClaim(n); h != "" && h != holder {
			return false, fmt.Errorf("%w (claimed by %q)", errClaimTaken, h)
		}
		setAnnotation(n, k8sutil.UpgradeInProgressAnnotation, target)
		return true, nil
	})
	return err
}

// Kinds of claim on k8sutil.UpgradeCordonAnnotation besides the target version.
const (
	cordonPreexisting = "preexisting"
	cordonReleased    = "released"
)

// errClaimTaken: the node's cordon claim names another run, which holds the
// lease now. Terminal: retrying cannot make the node this run's again.
var errClaimTaken = errors.New("the node's cordon is claimed by another upgrade run")

// cordonClaim is the value of k8sutil.UpgradeCordonAnnotation: a kind (the
// target version when the upgrade cordoned a schedulable node, else
// cordonPreexisting or cordonReleased) and the lease holder of the run that
// wrote it, as "<kind>@<holder>". The holder is what lets a run tell its own
// claim from a successor's for the same target. A value with no
// holder is an older one, or a run with no lease.
func cordonClaim(kind, holder string) string {
	if holder == "" {
		return kind
	}
	return kind + "@" + holder
}

func parseCordonClaim(n *corev1.Node) (kind, holder string) {
	kind, holder, _ = strings.Cut(n.Annotations[k8sutil.UpgradeCordonAnnotation], "@")
	return kind, holder
}

// ownsCordon reports whether the cordon on n is one the upgrade to target
// made, and so may undo, whichever run made it: a run that holds the lease takes
// over a halted one's claim (cordonForUpgrade), and the Job's backstop acts
// for an orchestrator that is gone. The orchestrator writes the claim in the
// same update that cordons, and before it records the upgrade in progress, so a
// node with that record and no claim was cordoned by an older orchestrator,
// which cordoned unconditionally.
func ownsCordon(n *corev1.Node, target string) bool {
	if _, claimed := n.Annotations[k8sutil.UpgradeCordonAnnotation]; !claimed {
		return n.Annotations[k8sutil.UpgradeInProgressAnnotation] == target
	}
	kind, _ := parseCordonClaim(n)
	// The claim must be the target and the target a real version, as for the
	// run that holds the lease (ownedByRunning).
	_, ok := k8sver.ParseTarget(kind)
	return ok && kind == target
}

// ownedByRunning reports whether the cordon on n is the upgrade's own as far as
// a run that holds the lease is concerned: a claim for any version, not only
// its target. A run that halted after cordoning, before it recorded the upgrade
// in progress, leaves a claim for the target it was taking the node to, and
// the next run may take the node to another one (preflight allows that). The
// lease holder takes the claim over, so what it finds is the upgrade's own
// cordon. Only a valid version counts: the reserved kinds, and anything this
// code did not write (another case, stray whitespace, a kind a newer build adds),
// are not the upgrade's to undo, so an unknown claim fails closed. Used where the
// lease is confirmed (cordonForUpgrade, finishNode). The Job's backstop is
// stricter (ownsCordon): it holds no lease and acts for its own target only.
func ownedByRunning(n *corev1.Node, target string) bool {
	if _, claimed := n.Annotations[k8sutil.UpgradeCordonAnnotation]; !claimed {
		return n.Annotations[k8sutil.UpgradeInProgressAnnotation] == target
	}
	kind, _ := parseCordonClaim(n)
	// ParseTarget, not ValidTarget: the pattern bounds the shape of a version but
	// not its digits, so a component that overflows an int passes it and can
	// never be a real target.
	_, ok := k8sver.ParseTarget(kind)
	return ok
}

func setAnnotation(n *corev1.Node, key, val string) {
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[key] = val
}

// updateNode reads the node, lets mutate change it, and writes it back with the
// resourceVersion it read, so a change made in between fails the write with a
// conflict instead of being overwritten (a merge patch would be). mutate
// returns false to write nothing, or an error to refuse; retryAPI re-reads
// after a conflict.
func updateNode(ctx context.Context, cs kubernetes.Interface, name string, mutate func(*corev1.Node) (bool, error)) (bool, error) {
	n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if write, err := mutate(n); !write || err != nil {
		return false, err
	}
	_, err = cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
	return err == nil, err
}

// cordonForUpgrade cordons the node and records, in the same write, that the
// upgrade did it and which run (holder). A node that is already cordoned is
// left cordoned and claimed as it was found: the upgrade's own cordon, left by
// a run that halted, is taken over by this one, which holds the lease; any
// other is marked so, which is how the end of the run knows to leave it
// cordoned. The lease is what makes taking a claim over safe, so it
// is confirmed on every attempt: a run that lost it while retrying must not
// overwrite its successor's claim.
func cordonForUpgrade(ctx context.Context, cs kubernetes.Interface, name, target string, lease *leaseGuard) error {
	holder := lease.id()
	return retryAPI(ctx, func(ctx context.Context) error {
		if err := lease.confirm(ctx); err != nil {
			return err
		}
		_, err := updateNode(ctx, cs, name, func(n *corev1.Node) (bool, error) {
			kind, cordoning := cordonPreexisting, !n.Spec.Unschedulable
			if cordoning {
				n.Spec.Unschedulable = true
				kind = target
			} else if ownedByRunning(n, target) {
				kind = target
			}
			claim := cordonClaim(kind, holder)
			if !cordoning && n.Annotations[k8sutil.UpgradeCordonAnnotation] == claim {
				return false, nil
			}
			setAnnotation(n, k8sutil.UpgradeCordonAnnotation, claim)
			return true, nil
		})
		return err
	})
}

// finishNode is one attempt to record a node's finished upgrade, clear its
// in-progress record, and uncordon it if the upgrade cordoned it, in one write:
// no reader sees one without the others, and a cordon an admin made is not
// undone. It refuses, with errClaimTaken, a node whose claim names another run:
// that run took the lease over and its cordon and records are its own. The
// caller confirms the lease on every attempt.
func finishNode(ctx context.Context, cs kubernetes.Interface, name, target, stratumVersion, holder string) error {
	_, err := updateNode(ctx, cs, name, func(n *corev1.Node) (bool, error) {
		if _, h := parseCordonClaim(n); h != "" && h != holder {
			return false, fmt.Errorf("%w (claimed by %q)", errClaimTaken, h)
		}
		if ownedByRunning(n, target) {
			n.Spec.Unschedulable = false
		}
		setAnnotation(n, k8sutil.VersionAnnotation, target)
		// The staged binary's stratum version, not this process's: an older
		// orchestrator may be rolling out a newer build, and a worker has no
		// admin.conf to stamp itself. Unknown leaves it as it was.
		if stratumVersion != "" {
			setAnnotation(n, k8sutil.StratumVersionAnnotation, stratumVersion)
		}
		delete(n.Annotations, k8sutil.UpgradeInProgressAnnotation)
		delete(n.Annotations, k8sutil.UpgradeCordonAnnotation)
		return true, nil
	})
	return err
}

// releaseCurrentCordon releases the upgrade's own cordon on a node this run
// leaves alone as already current: one a halted run left before it recorded the
// upgrade in progress, so no full path will finish it. That is a claim
// for any version (ownedByRunning), or no claim and an in-progress record, which
// an older orchestrator wrote when it cordoned. An admin's cordon
// (cordonPreexisting, cordonReleased, or none with no record) stays. A claim left
// on a node that is no longer cordoned is marked released, so an admin who
// cordons it later is not mistaken for the upgrade. The holder in the claim is
// not compared, as in cordonForUpgrade: this run holds the lease, so a claim by
// another holder is a dead run's. The lease is confirmed first, whatever the
// node turns out to need, so a run that lost it never reports the node current;
// and again after the node is read and before the write, so a successor that
// took it over and claimed the node in between is refused. A claim written after
// the read fails the write's resourceVersion check.
func releaseCurrentCordon(ctx context.Context, cs kubernetes.Interface, name string, lease *leaseGuard) error {
	if err := lease.confirm(ctx); err != nil {
		return err
	}
	_, err := updateNode(ctx, cs, name, func(n *corev1.Node) (bool, error) {
		if _, claimed := n.Annotations[k8sutil.UpgradeCordonAnnotation]; claimed {
			if !ownedByRunning(n, "") {
				return false, nil
			}
			if n.Spec.Unschedulable {
				n.Spec.Unschedulable = false
				delete(n.Annotations, k8sutil.UpgradeCordonAnnotation)
			} else {
				_, holder := parseCordonClaim(n)
				setAnnotation(n, k8sutil.UpgradeCordonAnnotation, cordonClaim(cordonReleased, holder))
			}
		} else if n.Spec.Unschedulable && n.Annotations[k8sutil.UpgradeInProgressAnnotation] != "" {
			n.Spec.Unschedulable = false
		} else {
			return false, nil
		}
		return true, lease.confirm(ctx)
	})
	return err
}

// releaseUpgradeCordon is the per-node Job's backstop for a cluster upgrade
// whose orchestrator is gone: on a one-node cluster the upgrade restarts the
// API server the orchestrator talks to, it halts, and nothing would uncordon
// the node. It undoes only a cordon the upgrade made, as
// k8sutil.UpgradeCordonAnnotation says, and only if the node is still as it
// read it. The in-progress record stays: the orchestrator, or the re-run,
// stamps and clears it.
func releaseUpgradeCordon(ctx context.Context, cs kubernetes.Interface, node, target string) (bool, error) {
	return updateNode(ctx, cs, node, func(n *corev1.Node) (bool, error) {
		if !n.Spec.Unschedulable || !ownsCordon(n, target) {
			return false, nil
		}
		_, holder := parseCordonClaim(n)
		n.Spec.Unschedulable = false
		setAnnotation(n, k8sutil.UpgradeCordonAnnotation, cordonClaim(cordonReleased, holder))
		return true, nil
	})
}

// backstopWindow bounds the Job's backstop, retries included. It is a slice of
// the per-node Job's deadline kept apart from the upgrade's own expected budget
// (upgradeWorkBudget, see upgradeJobDeadline), so that an upgrade within that
// budget is not failed by a backstop that runs long. A var so tests can shorten
// it.
var backstopWindow = 4 * time.Minute

// uncordonUpgraded is releaseUpgradeCordon from the node, through its admin
// kubeconfig; a var so a test can see the call.
var uncordonUpgraded = func(node, target string) (bool, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return false, err
	}
	return runBackstop(cs, node, target)
}

// runBackstop is releaseUpgradeCordon, retried through an API outage, all
// within backstopWindow.
func runBackstop(cs kubernetes.Interface, node, target string) (bool, error) {
	var done bool
	ctx, cancel := context.WithTimeout(context.Background(), backstopWindow)
	defer cancel()
	err := retryAPI(ctx, func(ctx context.Context) (err error) {
		done, err = releaseUpgradeCordon(ctx, cs, node, target)
		return err
	})
	return done, err
}
