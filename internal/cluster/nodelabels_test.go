package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A first boot's kubelet starts before its Node registers, so the kubelet
// hook finds nothing to label: the control plane's labelling task waits out
// the registration and labels once, with the node's own state.
func TestLabelOwnNodeOnceRegisteredWaitsForTheNode(t *testing.T) {
	oldInterval, oldGiveUp, oldCheck, oldLabel := nodeRegistrationInterval, nodeRegistrationGiveUp, nodeRegistered, labelOwnNode
	t.Cleanup(func() {
		nodeRegistrationInterval, nodeRegistrationGiveUp, nodeRegistered, labelOwnNode = oldInterval, oldGiveUp, oldCheck, oldLabel
	})
	nodeRegistrationInterval, nodeRegistrationGiveUp = time.Millisecond, time.Second
	var checks, labels atomic.Int32
	nodeRegistered = func(context.Context, string) bool { return checks.Add(1) >= 3 }
	st := &NodeState{Hostname: "cp1", NodeLabels: map[string]string{"zone": "a"}}
	labelOwnNode = func(_ context.Context, got *NodeState) {
		if got != st {
			t.Errorf("labelled with %+v, want the node's own state", got)
		}
		labels.Add(1)
	}
	labelOwnNodeWhenRegistered(context.Background(), st)
	if labels.Load() != 1 || checks.Load() < 3 {
		t.Errorf("labelled %d times after %d registration checks, want once, after the node registered", labels.Load(), checks.Load())
	}

	labels.Store(0)
	nodeRegistered = func(context.Context, string) bool { return false }
	nodeRegistrationGiveUp = 20 * time.Millisecond
	labelOwnNodeWhenRegistered(context.Background(), st)
	if labels.Load() != 0 {
		t.Error("a node that never registered was labelled")
	}
}

// The run group restarts a task that returns before shutdown, so the
// labelling task holds once it has labelled: returning would relabel the Node
// on every backoff for the life of the controller.
func TestLabelOwnNodeOnceRegisteredHoldsUntilShutdown(t *testing.T) {
	oldInterval, oldCheck, oldLabel := nodeRegistrationInterval, nodeRegistered, labelOwnNode
	t.Cleanup(func() { nodeRegistrationInterval, nodeRegistered, labelOwnNode = oldInterval, oldCheck, oldLabel })
	nodeRegistrationInterval = time.Millisecond
	nodeRegistered = func(context.Context, string) bool { return true }
	labelled := make(chan struct{}, 8)
	labelOwnNode = func(context.Context, *NodeState) { labelled <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		labelOwnNodeOnceRegistered(ctx, &NodeState{Hostname: "cp1"})
		close(done)
	}()
	<-labelled
	select {
	case <-done:
		t.Fatal("the labelling task returned after labelling; the run group would restart it")
	case <-time.After(50 * time.Millisecond):
	}
	if n := len(labelled); n != 0 {
		t.Errorf("labelled %d more times, want once", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the labelling task did not return at shutdown")
	}
}
