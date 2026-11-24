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

// node registration can lag etcd membership by seconds; a first complete list is not guaranteed
var controllerAddressPollBudget = 2 * time.Minute

// the kubeconfig's server names the proposed VIP, which may not be reachable; override it
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
	var lastGood *corev1.NodeList
	var last error
	pollCtx, cancel := context.WithTimeout(ctx, controllerAddressPollBudget)
	defer cancel()
	err = wait.PollUntilContextCancel(pollCtx, time.Second, true, func(attempt context.Context) (bool, error) {
		nodes, err := cs.CoreV1().Nodes().List(attempt, metav1.ListOptions{})
		if err == nil {
			lastGood = nodes
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
		// control-plane labelling is best-effort; an unlabelled primary still contributes its addresses
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

func controllerAddressPollError(ctx, pollCtx context.Context, missing string, err error) error {
	callerErr := ctx.Err()
	if deadline, ok := ctx.Deadline(); callerErr == nil && ok && !time.Now().Before(deadline) {
		callerErr = context.DeadlineExceeded
	}
	if callerErr != nil {
		return fmt.Errorf("read controller addresses: %w", callerErr)
	}
	// the client limiter can refuse a reservation before the context timer delivers expiry
	expired := pollCtx.Err() != nil
	if deadline, ok := pollCtx.Deadline(); ok && !time.Now().Before(deadline) {
		expired = true
	}
	if expired && missing != "" {
		return fmt.Errorf("no registered node advertises controller address %s; wait for that controller's node registration before re-running the join", missing)
	}
	return err
}

func controllersAllAdvertised(nodes *corev1.NodeList, primary []string) bool {
	return missingPrimary(nodes, primary) == ""
}

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
