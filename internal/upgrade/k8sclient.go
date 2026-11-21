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

// Sized to the per-node health gate's window; vars so tests can shorten them.
var (
	apiRetryWindow   = 5 * time.Minute
	apiRetryInterval = 2 * time.Second
	apiCallTimeout   = 10 * time.Second
)

// True for errors retrying cannot change; the excluded 4xx answers can still change.
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

// Retries writes through the API-server restart the upgrade itself causes.
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

// Control plane only when the Stratum control-plane label is exactly "true".
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

// Drops the build suffix an SKE kubelet reports, so comparisons against a bare --target work.
func NodeKubeletVersion(reported string) string {
	if v, err := parseVersion(reported); err == nil {
		return v
	}
	return reported
}

// nodeFromAPI is NodeFromAPI under the name this package's own tests pin.
func nodeFromAPI(n *corev1.Node) Node { return NodeFromAPI(n) }

// classifyNodes lists the cluster's nodes in the order the API served them.
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

// Marks control planes from the etcd member roster for clusters with no control-plane labels.
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
	// Report the node-state error first: dialling etcd at "" would mislead.
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

// Distinguishes an interrupted upgrade from a finished one, which the kubelet version cannot.
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

var errClaimTaken = errors.New("the node's cordon is claimed by another upgrade run")

// The holder lets a run tell its own claim from a successor's for the same target.
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

// No claim with an in-progress record is an older orchestrator's cordon.
func ownsCordon(n *corev1.Node, target string) bool {
	if _, claimed := n.Annotations[k8sutil.UpgradeCordonAnnotation]; !claimed {
		return n.Annotations[k8sutil.UpgradeInProgressAnnotation] == target
	}
	kind, _ := parseCordonClaim(n)
	_, ok := k8sver.ParseTarget(kind)
	return ok && kind == target
}

// Any valid-version claim is the lease holder's to take over; unknown claims fail closed.
func ownedByRunning(n *corev1.Node, target string) bool {
	if _, claimed := n.Annotations[k8sutil.UpgradeCordonAnnotation]; !claimed {
		return n.Annotations[k8sutil.UpgradeInProgressAnnotation] == target
	}
	kind, _ := parseCordonClaim(n)
	// ParseTarget, not ValidTarget: the pattern bounds the shape, not the digits.
	_, ok := k8sver.ParseTarget(kind)
	return ok
}

func setAnnotation(n *corev1.Node, key, val string) {
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[key] = val
}

// Writes back with the resourceVersion it read, so an intervening change fails with a conflict.
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

// Confirms the lease on every attempt: a run that lost it must not overwrite a successor's claim.
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

// One write so no reader sees a stamp without the uncordon; refuses a claim another run holds.
func finishNode(ctx context.Context, cs kubernetes.Interface, name, target, stratumVersion, holder string) error {
	_, err := updateNode(ctx, cs, name, func(n *corev1.Node) (bool, error) {
		if _, h := parseCordonClaim(n); h != "" && h != holder {
			return false, fmt.Errorf("%w (claimed by %q)", errClaimTaken, h)
		}
		if ownedByRunning(n, target) {
			n.Spec.Unschedulable = false
		}
		setAnnotation(n, k8sutil.VersionAnnotation, target)
		// The staged binary's version, not this process's; a worker cannot stamp itself.
		if stratumVersion != "" {
			setAnnotation(n, k8sutil.StratumVersionAnnotation, stratumVersion)
		}
		delete(n.Annotations, k8sutil.UpgradeInProgressAnnotation)
		delete(n.Annotations, k8sutil.UpgradeCordonAnnotation)
		return true, nil
	})
	return err
}

// Releases a halted run's own cordon on an already-current node; an admin's cordon stays.
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

// The per-node Job's backstop for an orchestrator that died mid-upgrade.
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

// A slice of the per-node Job's deadline; a var so tests can shorten it.
var backstopWindow = 4 * time.Minute

// Through the node's admin kubeconfig; a var so a test can see the call.
var uncordonUpgraded = func(node, target string) (bool, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return false, err
	}
	return runBackstop(cs, node, target)
}

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
