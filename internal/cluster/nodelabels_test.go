package cluster

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A first boot's kubelet starts before its Node registers, so the kubelet
// hook finds nothing to label: the node-registered task labels the node with
// its own state on the tick that finds it registered, once, and holds.
func TestNodeRegisteredTaskLabelsTheOwnNodeOnceItRegisters(t *testing.T) {
	var checks atomic.Int32
	stubNodeTask(t, time.Millisecond, time.Second, "cp1", func(context.Context, string) bool { return checks.Add(1) >= 3 })
	st := &NodeState{Hostname: "cp1", NodeLabels: map[string]string{"zone": "a"}}
	labelled := make(chan *NodeState, 8)
	labelOwnNode = func(_ context.Context, got *NodeState) { labelled <- got }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		nodeRegisteredTask(ctx, st, nil)
		close(done)
	}()
	select {
	case got := <-labelled:
		if got != st {
			t.Errorf("labelled with %+v, want the node's own state", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registered node was never labelled")
	}
	if checks.Load() < 3 {
		t.Errorf("labelled after %d registration checks, want after the node registered", checks.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(labelled); n != 0 {
		t.Errorf("labelled %d more times, want once", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the node-registered task did not return at shutdown")
	}
}

// A node that never registers is never labelled: the give-up is final for
// the run.
func TestNodeRegisteredTaskLabelsNothingWhenTheNodeNeverRegisters(t *testing.T) {
	stubNodeTask(t, time.Millisecond, 20*time.Millisecond, "cp1", func(context.Context, string) bool { return false })
	var labels atomic.Int32
	labelOwnNode = func(context.Context, *NodeState) { labels.Add(1) }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	nodeRegisteredTask(ctx, &NodeState{Hostname: "cp1"}, nil)
	if labels.Load() != 0 {
		t.Error("a node that never registered was labelled")
	}
}

// stubOwnLabels replaces the node's label reconcile with fail, recording each
// call's arguments, and paces retries every millisecond.
func stubOwnLabels(t *testing.T, warnAfter time.Duration, fail func(call int32) error) *atomic.Int32 {
	t.Helper()
	oldReconcile, oldInterval, oldWarn := reconcileOwnNodeLabels, nodeRegistrationInterval, bindingWarnAfter
	t.Cleanup(func() {
		reconcileOwnNodeLabels, nodeRegistrationInterval, bindingWarnAfter = oldReconcile, oldInterval, oldWarn
	})
	nodeRegistrationInterval, bindingWarnAfter = time.Millisecond, warnAfter
	var calls atomic.Int32
	reconcileOwnNodeLabels = func(_ context.Context, node string, declared map[string]string, handlers []string) error {
		if node != "w1" || declared["zone"] != "a" || !slices.Equal(handlers, []string{"gpu", "kata"}) {
			t.Errorf("labelled %s with %v, %v; want w1's own labels and handlers", node, declared, handlers)
		}
		return fail(calls.Add(1))
	}
	return &calls
}

var labelledNode = &NodeState{Hostname: "w1", NodeLabels: map[string]string{"zone": "a"},
	RuntimeHandlers: map[string]string{"kata": "/usr/bin/kata", "gpu": "/usr/bin/nvidia-container-runtime"}}

// One failed label call must not leave a node without its handler labels
// until its next kubelet start: labelOwnNode retries until the labels land,
// and says so, warning once they have been failing for bindingWarnAfter.
func TestLabelOwnNodeRetriesAFailedLabelUntilItLands(t *testing.T) {
	calls := stubOwnLabels(t, 0, func(call int32) error {
		if call < 4 {
			return errors.New("connection refused")
		}
		return nil
	})
	out := captureWorkerOutput(t, func() { labelOwnNode(context.Background(), labelledNode) })
	if calls.Load() != 4 {
		t.Errorf("labelled %d times, want until the fourth landed", calls.Load())
	}
	for _, want := range []string{"label w1: connection refused; retrying", "WARN: cannot label w1 yet", "labelled w1 after"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "WARN"); n != 1 {
		t.Errorf("warned %d times, want once:\n%s", n, out)
	}
}

// A label that never lands is retried until shutdown, and the retry ends
// with the node's context.
func TestLabelOwnNodeStopsRetryingAtShutdown(t *testing.T) {
	calls := stubOwnLabels(t, time.Hour, func(int32) error { return errors.New("connection refused") })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		captureWorkerOutput(t, func() { labelOwnNode(ctx, labelledNode) })
		close(done)
	}()
	for calls.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("labelOwnNode kept retrying after shutdown")
	}
}

// A labelled node writes once and does not retry.
func TestLabelOwnNodeLabelsOnceWhenTheFirstCallLands(t *testing.T) {
	calls := stubOwnLabels(t, 0, func(int32) error { return nil })
	if out := captureWorkerOutput(t, func() { labelOwnNode(context.Background(), labelledNode) }); out != "" {
		t.Errorf("a label that landed printed %q", out)
	}
	if calls.Load() != 1 {
		t.Errorf("labelled %d times, want once", calls.Load())
	}
}

// stubOwnRegistration replaces the worker's registration wait with one that
// fails until call registered, and the stamp with a no-op.
func stubOwnRegistration(t *testing.T, registered int32) *atomic.Int32 {
	t.Helper()
	oldWait, oldStamp := waitOwnNodeRegistered, stampOwnNode
	t.Cleanup(func() { waitOwnNodeRegistered, stampOwnNode = oldWait, oldStamp })
	var waits atomic.Int32
	waitOwnNodeRegistered = func(node string, timeout time.Duration) error {
		if node != "w1" || timeout != ownNodeRegistrationWarnAfter {
			t.Errorf("waited for %s for %v, want w1 for %v", node, timeout, ownNodeRegistrationWarnAfter)
		}
		if waits.Add(1) < registered {
			return errors.New("node w1 did not register")
		}
		return nil
	}
	stampOwnNode = func(string) error { return nil }
	return &waits
}

// A worker whose first registration outlasts one wait keeps waiting, and
// says so once, so a slow first join is still labelled for its handlers.
func TestWorkerKubeletStartLabelsASlowFirstRegistration(t *testing.T) {
	waits := stubOwnRegistration(t, 3)
	var labelled atomic.Int32
	old := labelOwnNode
	t.Cleanup(func() { labelOwnNode = old })
	labelOwnNode = func(_ context.Context, got *NodeState) {
		if got != labelledNode {
			t.Errorf("labelled with %+v, want the worker's own state", got)
		}
		labelled.Add(1)
	}
	out := captureWorkerOutput(t, func() { workerKubeletStart(context.Background(), labelledNode)("w1") })
	if waits.Load() != 3 || labelled.Load() != 1 {
		t.Errorf("waited %d times and labelled %d times, want 3 and once", waits.Load(), labelled.Load())
	}
	if n := strings.Count(out, "still waiting"); n != 1 {
		t.Errorf("said it is still waiting %d times, want once:\n%s", n, out)
	}
}

// A worker that never registers stops waiting at shutdown and labels nothing.
func TestWorkerKubeletStartStopsWaitingAtShutdown(t *testing.T) {
	stubOwnRegistration(t, 1<<30)
	old := labelOwnNode
	t.Cleanup(func() { labelOwnNode = old })
	labelOwnNode = func(context.Context, *NodeState) { t.Error("an unregistered worker was labelled") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	captureWorkerOutput(t, func() { workerKubeletStart(ctx, labelledNode)("w1") })
}

// A controller's kubelet start labels its own Node once it is registered, and
// leaves an unregistered one to the node-registered task.
func TestControllerKubeletStartLabelsTheRegisteredOwnNode(t *testing.T) {
	for _, registered := range []bool{true, false} {
		stubNodeTask(t, time.Millisecond, time.Second, "cp1", func(context.Context, string) bool { return registered })
		st := &NodeState{Hostname: "cp1"}
		var labelled atomic.Int32
		labelOwnNode = func(_ context.Context, got *NodeState) {
			if got != st {
				t.Errorf("labelled with %+v, want the controller's own state", got)
			}
			labelled.Add(1)
		}
		controllerKubeletStart(st)(context.Background(), "cp1")
		if want := map[bool]int32{true: 1, false: 0}[registered]; labelled.Load() != want {
			t.Errorf("registered=%v: labelled %d times, want %d", registered, labelled.Load(), want)
		}
	}
}

// stubOwnTaints saves st as this node's state and answers the Node's taints
// with have, or err.
func stubOwnTaints(t *testing.T, st *NodeState, have []corev1.Taint, err error) {
	t.Helper()
	redirectJoinDirs(t)
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	old := ownNodeTaints
	t.Cleanup(func() { ownNodeTaints = old })
	ownNodeTaints = func(_ context.Context, node string) ([]corev1.Taint, error) {
		if node != st.Hostname {
			t.Errorf("read %s's taints, want %s's", node, st.Hostname)
		}
		return have, err
	}
}

func joinedWithTaints() *NodeState {
	return &NodeState{Role: "worker", Hostname: "w1", NodeTaintsCheckPending: true,
		NodeTaints: []string{"stratum.ciq.com/unverified=true:NoSchedule", "dedicated=gpu:NoExecute"}}
}

// A join that registers onto a Node already there finds it without the
// taints the kubelet would have created it with: the worker warns, naming
// the missing taint and the control-plane fixes, once.
func TestCheckJoinedTaintsWarnsWhenTheNodeLacksADeclaredTaint(t *testing.T) {
	st := joinedWithTaints()
	stubOwnTaints(t, st, []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoExecute}}, nil)
	out := captureWorkerOutput(t, func() { checkJoinedTaints(context.Background(), st) })
	for _, want := range []string{"WARN: node w1 registered without its declared taints stratum.ciq.com/unverified=true:NoSchedule:", "stratum kubectl taint node w1", "stratum kubectl delete node w1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dedicated=gpu:NoExecute") {
		t.Errorf("warned about a taint the Node carries:\n%s", out)
	}
	if st.NodeTaintsCheckPending || mustLoadState(t).NodeTaintsCheckPending {
		t.Error("the check stayed pending after it ran")
	}
	if out := captureWorkerOutput(t, func() { checkJoinedTaints(context.Background(), st) }); out != "" {
		t.Errorf("a second start checked again: %s", out)
	}
}

// A Node created with its taints is quiet and clears the check, and a value
// changed by hand counts as missing.
func TestCheckJoinedTaintsIsQuietWhenTheNodeCarriesThem(t *testing.T) {
	st := joinedWithTaints()
	stubOwnTaints(t, st, []corev1.Taint{
		{Key: "stratum.ciq.com/unverified", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoExecute},
		{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule},
	}, nil)
	if out := captureWorkerOutput(t, func() { checkJoinedTaints(context.Background(), st) }); out != "" {
		t.Errorf("a Node with its taints warned: %s", out)
	}
	if mustLoadState(t).NodeTaintsCheckPending {
		t.Error("the check stayed pending after it ran")
	}

	st = joinedWithTaints()
	stubOwnTaints(t, st, []corev1.Taint{
		{Key: "stratum.ciq.com/unverified", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Value: "cpu", Effect: corev1.TaintEffectNoExecute},
	}, nil)
	if out := captureWorkerOutput(t, func() { checkJoinedTaints(context.Background(), st) }); !strings.Contains(out, "dedicated=gpu:NoExecute") {
		t.Errorf("a changed value did not warn: %s", out)
	}
}

// A Node the worker cannot read leaves the check for the next start, and a
// restart, which saves no pending check, never reads the Node.
func TestCheckJoinedTaintsWaitsForANodeItCanRead(t *testing.T) {
	st := joinedWithTaints()
	stubOwnTaints(t, st, nil, errors.New("connection refused"))
	if out := captureWorkerOutput(t, func() { checkJoinedTaints(context.Background(), st) }); !strings.Contains(out, "checking again on the next start") {
		t.Errorf("an unreadable Node printed %q", out)
	}
	if !st.NodeTaintsCheckPending || !mustLoadState(t).NodeTaintsCheckPending {
		t.Error("an unreadable Node cleared the pending check")
	}

	st.NodeTaintsCheckPending = false
	ownNodeTaints = func(context.Context, string) ([]corev1.Taint, error) {
		t.Error("a start with no pending check read the Node")
		return nil, nil
	}
	checkJoinedTaints(context.Background(), st)
}

// The worker's kubelet start checks a join's taints once its Node registers.
func TestWorkerKubeletStartChecksAJoinsTaints(t *testing.T) {
	stubOwnRegistration(t, 1)
	st := joinedWithTaints()
	stubOwnTaints(t, st, nil, nil)
	old := labelOwnNode
	t.Cleanup(func() { labelOwnNode = old })
	labelOwnNode = func(context.Context, *NodeState) {}
	if out := captureWorkerOutput(t, func() { workerKubeletStart(context.Background(), st)("w1") }); !strings.Contains(out, "registered without its declared taints") {
		t.Errorf("the kubelet start did not check the join's taints: %s", out)
	}
}
