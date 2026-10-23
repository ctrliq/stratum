package upgrade

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CoreDNS has no PodDisruptionBudget by design, so the orchestrator gives DNS
// the guarantee a PDB would have: at most one node hosting a replica drains at
// a time, and only once another replica is Ready elsewhere.
const dnsSelector = "k8s-app=kube-dns"

// dnsReadyWait bounds the wait for a Ready replica elsewhere. A var for tests.
var dnsReadyWait = 5 * time.Minute

var dnsDrainMu sync.Mutex

// dnsDrainState reads, for a cordoned node about to drain: whether it hosts a
// CoreDNS replica, whether a Running and Ready replica exists on another node
// that is itself Ready, and whether the cluster has any other node at all.
//
// A replica counts as serving elsewhere only if its node is NodeReady too, a
// terminating replica here still counts as hosting, and one elsewhere does
// not count as serving. Pure.
func dnsDrainState(pods []corev1.Pod, nodes []corev1.Node, node string) (hosts, readyElsewhere, otherNodes bool) {
	ready := map[string]bool{}
	for _, n := range nodes {
		ready[n.Name] = isNodeReady(n)
		if n.Name != node {
			otherNodes = true
		}
	}
	for _, p := range pods {
		if p.Spec.NodeName == node {
			hosts = true
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Spec.NodeName != "" && ready[p.Spec.NodeName] && p.Status.Phase == corev1.PodRunning && isPodReady(p) {
			readyElsewhere = true
		}
	}
	return hosts, readyElsewhere, otherNodes
}

func isPodReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func isNodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// gateCoreDNSDrain runs after node has been cordoned and returns a release
// func for once the drain is done. On timeout it fails with the node still
// cordoned and nothing evicted.
func gateCoreDNSDrain(w io.Writer, ctx context.Context, cs kubernetes.Interface, node string) (release func(), err error) {
	noop := func() {}
	read := func() (bool, bool, bool, error) {
		pods, err := cs.CoreV1().Pods(cuNamespace).List(ctx, metav1.ListOptions{LabelSelector: dnsSelector})
		if err != nil {
			return false, false, false, fmt.Errorf("list CoreDNS replicas: %w", err)
		}
		nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, false, false, fmt.Errorf("list nodes: %w", err)
		}
		h, r, e := dnsDrainState(pods.Items, nodes.Items, node)
		return h, r, e, nil
	}
	hosts, _, _, err := read()
	if err != nil || !hosts {
		return noop, err
	}
	dnsDrainMu.Lock()
	deadline := time.Now().Add(dnsReadyWait)
	for {
		_, ready, others, err := read()
		if err != nil {
			dnsDrainMu.Unlock()
			return noop, err
		}
		if ready {
			return dnsDrainMu.Unlock, nil
		}
		if !others {
			fmt.Fprintf(w, "  %s is the only node; CoreDNS is down while it drains\n", node)
			return dnsDrainMu.Unlock, nil
		}
		if time.Now().After(deadline) {
			dnsDrainMu.Unlock()
			return noop, fmt.Errorf("%s hosts a CoreDNS replica and no other replica became Ready within %s; "+
				"not draining it, so cluster DNS keeps a serving replica (node left cordoned; check "+
				"`kubectl -n kube-system get pods -l %s -o wide`)", node, dnsReadyWait, dnsSelector)
		}
		fmt.Fprintf(w, "  %s hosts CoreDNS; waiting for a replica to be Ready on another node\n", node)
		select {
		case <-ctx.Done():
			dnsDrainMu.Unlock()
			return noop, ctx.Err()
		case <-time.After(dnsPollInterval):
		}
	}
}

var dnsPollInterval = 5 * time.Second
