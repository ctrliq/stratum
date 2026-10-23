package ha

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// controllerAddressPollBudget is how long the node-inventory read keeps
// retrying: API outages first, and — the part the first draft got wrong — a
// list that succeeds while an existing controller's node object has not been
// registered yet. A joining third controller starts the moment the second's
// etcd membership lands, but its kubelet registers the node object seconds
// later, so the first successful list is regularly incomplete; treating that
// as the answer refused a join the cluster was ready for. The join elsewhere
// waits up to 10m for its own node registration, so a lagging registration is
// a state to wait through, not a misconfiguration to refuse.
var controllerAddressPollBudget = 2 * time.Minute

// controllerNodeIPsForJoin reads the existing controllers' addresses through
// the join server, using the admin credentials the join just generated. The
// kubeconfig's server already names the proposed VIP, so it must be overridden
// here: this read must not depend on that VIP being correct or reachable.
func controllerNodeIPsForJoin(ctx context.Context, server string, primary []string) ([]string, error) {
	cfg, err := k8sutil.AdminRESTConfig()
	if err != nil {
		return nil, err
	}
	cfg.Host = server
	cfg.Timeout = 10 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	var lastGood *corev1.NodeList // the most recent successful list; failures never clobber it
	var last error
	pollCtx, cancel := context.WithTimeout(ctx, controllerAddressPollBudget)
	defer cancel()
	err = wait.PollUntilContextCancel(pollCtx, time.Second, true, func(attempt context.Context) (bool, error) {
		nodes, err := cs.CoreV1().Nodes().List(attempt, metav1.ListOptions{})
		if err == nil {
			lastGood = nodes
			// A successful list is not yet an answer: until every existing
			// controller's node object advertises its primary address, the
			// inventory is incomplete, and acting on it could mistake a
			// controller's secondary address for a free VIP. Keep polling —
			// the budget-expiry refusal below is for a controller that never
			// registers, not for one whose registration is seconds away.
			return controllersAllAdvertised(nodes, primary), nil
		}
		last = err
		var ne net.Error
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) ||
			(errors.As(err, &ne) && ne.Timeout()) || apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) {
			return false, nil
		}
		return false, err
	})
	if err != nil {
		missing := ""
		if lastGood != nil {
			missing = missingPrimary(lastGood, primary)
		}
		return nil, controllerAddressPollError(ctx, pollCtx, missing,
			fmt.Errorf("read controller addresses (last API error: %v): %w", last, err))
	}
	nodes := lastGood
	ips := append([]string(nil), primary...)
	for _, node := range nodes.Items {
		controller := node.Labels[k8sutil.ControlPlaneLabel] == "true"
		// Control-plane labelling is best-effort at start. Match the etcd
		// primary too, so an unlabelled controller still contributes both
		// families of its node address.
		for _, addr := range node.Status.Addresses {
			if addr.Type != corev1.NodeInternalIP && addr.Type != corev1.NodeExternalIP {
				continue
			}
			for _, ip := range primary {
				if sameIP(addr.Address, ip) {
					controller = true
				}
			}
		}
		if controller {
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
					ips = append(ips, addr.Address)
				}
			}
		}
	}
	return ips, nil
}

// controllerAddressPollError distinguishes an exhausted registration wait from
// a caller cancellation or an API failure while the polling budget is live.
func controllerAddressPollError(ctx, pollCtx context.Context, missing string, err error) error {
	callerErr := ctx.Err()
	if deadline, ok := ctx.Deadline(); callerErr == nil && ok && !time.Now().Before(deadline) {
		callerErr = context.DeadlineExceeded
	}
	if callerErr != nil {
		return fmt.Errorf("read controller addresses: %w", callerErr)
	}
	// The client limiter checks the deadline independently of Done. Its
	// reservation can fail before the context timer has delivered expiry.
	expired := pollCtx.Err() != nil
	if deadline, ok := pollCtx.Deadline(); ok && !time.Now().Before(deadline) {
		expired = true
	}
	if expired && missing != "" {
		return fmt.Errorf("no registered node advertises controller address %s; wait for that controller's node registration before re-running the join", missing)
	}
	return err
}

// controllersAllAdvertised reports whether every etcd primary address in
// primary is advertised by some node object in nodes.
func controllersAllAdvertised(nodes *corev1.NodeList, primary []string) bool {
	return missingPrimary(nodes, primary) == ""
}

// missingPrimary returns the first etcd primary address no node object in
// nodes advertises, or "" when every one is advertised.
func missingPrimary(nodes *corev1.NodeList, primary []string) string {
	for _, ip := range primary {
		found := false
		for _, node := range nodes.Items {
			for _, addr := range node.Status.Addresses {
				if (addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP) && sameIP(addr.Address, ip) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return ip
		}
	}
	return ""
}
