package upgrade

import (
	"io"

	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func dnsReplica(name, node string, ready bool) *corev1.Pod {
	s := corev1.ConditionFalse
	if ready {
		s = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cuNamespace, Labels: map[string]string{"k8s-app": "kube-dns"}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: s}}},
	}
}

func readyNode(name string, ready, cordoned bool) *corev1.Node {
	s := corev1.ConditionFalse
	if ready {
		s = corev1.ConditionTrue
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: s}}}}
}

func shortDNSWait(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldPoll := dnsReadyWait, dnsPollInterval
	dnsReadyWait, dnsPollInterval = wait, 10*time.Millisecond
	t.Cleanup(func() { dnsReadyWait, dnsPollInterval = oldWait, oldPoll })
}

func TestDNSDrainState(t *testing.T) {
	pods := func(ps ...*corev1.Pod) []corev1.Pod {
		out := make([]corev1.Pod, len(ps))
		for i, p := range ps {
			out[i] = *p
		}
		return out
	}
	nodes := []corev1.Node{*readyNode("w1", true, true), *readyNode("w2", true, false)}
	h, r, e := dnsDrainState(pods(dnsReplica("a", "w1", true), dnsReplica("b", "w2", true)), nodes, "w1")
	if !h || !r || !e {
		t.Errorf("spread replicas: hosts=%v readyElsewhere=%v possible=%v, want all true", h, r, e)
	}
	if _, r, _ := dnsDrainState(pods(dnsReplica("a", "w1", true), dnsReplica("b", "w2", false)), nodes, "w1"); r {
		t.Error("a replica that is not Ready counted as serving elsewhere")
	}
	if h, _, _ := dnsDrainState(pods(dnsReplica("b", "w2", true)), nodes, "w1"); h {
		t.Error("a node without a replica reported as hosting one")
	}
	// A terminating replica here still counts as hosting; one elsewhere is
	// not serving.
	gone := dnsReplica("b", "w1", true)
	gone.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if h, _, _ := dnsDrainState(pods(gone), nodes, "w1"); !h {
		t.Error("a terminating replica on the node did not count as hosting CoreDNS")
	}
	goneElsewhere := dnsReplica("c", "w2", true)
	goneElsewhere.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if _, r, _ := dnsDrainState(pods(dnsReplica("a", "w1", true), goneElsewhere), nodes, "w1"); r {
		t.Error("a terminating replica elsewhere counted as serving")
	}
	// A replica whose node is NotReady is not serving, however stale its own
	// Ready condition is.
	failed := []corev1.Node{*readyNode("w1", true, true), *readyNode("w2", false, false)}
	if _, r, _ := dnsDrainState(pods(dnsReplica("a", "w1", true), dnsReplica("b", "w2", true)), failed, "w1"); r {
		t.Error("a Ready replica on a NotReady node counted as serving elsewhere")
	}
	// A cordoned or NotReady peer is still another node.
	if _, _, o := dnsDrainState(nil, failed, "w1"); !o {
		t.Error("a NotReady peer was treated as no other node, the single-node escape")
	}
	if _, _, o := dnsDrainState(nil, []corev1.Node{*readyNode("cp1", true, true)}, "cp1"); o {
		t.Error("a single-node cluster reported another node")
	}
}

// TestGateCoreDNSDrainSerializesHosts: the second host's drain waits until the
// first has released the gate.
func TestGateCoreDNSDrainSerializesHosts(t *testing.T) {
	shortDNSWait(t, 5*time.Second)
	cs := fake.NewSimpleClientset([]runtime.Object{
		dnsReplica("a", "w1", true), dnsReplica("b", "w2", true),
		readyNode("w1", true, true), readyNode("w2", true, true), readyNode("w3", true, false),
	}...)
	release1, err := gateCoreDNSDrain(io.Discard, context.Background(), cs, "w1")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		release2, err := gateCoreDNSDrain(io.Discard, context.Background(), cs, "w2")
		if err == nil {
			release2()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the second CoreDNS host passed the gate while the first was draining (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	release1()
	if err := <-got; err != nil {
		t.Fatalf("second host after release: %v", err)
	}
}

func TestGateCoreDNSDrainRefusesWithoutAServingReplica(t *testing.T) {
	shortDNSWait(t, 50*time.Millisecond)
	cs := fake.NewSimpleClientset([]runtime.Object{
		dnsReplica("a", "w1", true), dnsReplica("b", "w2", false),
		readyNode("w1", true, true), readyNode("w2", true, false),
	}...)
	_, err := gateCoreDNSDrain(io.Discard, context.Background(), cs, "w1")
	if err == nil || !strings.Contains(err.Error(), "no other replica became Ready") {
		t.Fatalf("draining the last serving replica's node = %v, want a refusal", err)
	}
	// The lock must not leak with the refusal.
	if !dnsDrainMu.TryLock() {
		t.Fatal("the gate stayed locked after refusing")
	}
	dnsDrainMu.Unlock()
}

// TestGateCoreDNSDrainSingleNodeStillDrains: the case a PDB would have broken.
// Nowhere else can run a replica, so the drain proceeds at once.
func TestGateCoreDNSDrainSingleNodeStillDrains(t *testing.T) {
	shortDNSWait(t, time.Hour)
	cs := fake.NewSimpleClientset([]runtime.Object{
		dnsReplica("a", "cp1", true), dnsReplica("b", "cp1", true), readyNode("cp1", true, true),
	}...)
	done := make(chan error, 1)
	go func() {
		release, err := gateCoreDNSDrain(io.Discard, context.Background(), cs, "cp1")
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a single-node drain waited for a replica that can never exist elsewhere")
	}
}

func TestGateCoreDNSDrainIgnoresNodesWithoutAReplica(t *testing.T) {
	shortDNSWait(t, time.Hour)
	cs := fake.NewSimpleClientset([]runtime.Object{dnsReplica("a", "w2", true), readyNode("w1", true, true), readyNode("w2", true, false)}...)
	dnsDrainMu.Lock() // another host is mid-drain
	defer dnsDrainMu.Unlock()
	release, err := gateCoreDNSDrain(io.Discard, context.Background(), cs, "w1")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// TestGateCoreDNSDrainDoesNotTreatCordonedPeersAsASingleNode: a cordoned peer
// is not "nowhere else", so the drain waits for a Ready replica and fails
// closed without one.
func TestGateCoreDNSDrainDoesNotTreatCordonedPeersAsASingleNode(t *testing.T) {
	shortDNSWait(t, 50*time.Millisecond)
	cs := fake.NewSimpleClientset([]runtime.Object{
		dnsReplica("b", "w2", true), // a's replica was evicted from w1 and is not back yet
		readyNode("w1", true, true), readyNode("w2", true, true),
	}...)
	if _, err := gateCoreDNSDrain(io.Discard, context.Background(), cs, "w2"); err == nil {
		t.Fatal("drained the last Ready replica because its only peer was cordoned")
	}
}
