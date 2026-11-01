package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
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
