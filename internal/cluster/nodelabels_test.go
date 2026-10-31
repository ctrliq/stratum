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
	labelOwnNodeOnceRegistered(context.Background(), st)
	if labels.Load() != 1 || checks.Load() < 3 {
		t.Errorf("labelled %d times after %d registration checks, want once, after the node registered", labels.Load(), checks.Load())
	}

	labels.Store(0)
	nodeRegistered = func(context.Context, string) bool { return false }
	nodeRegistrationGiveUp = 20 * time.Millisecond
	labelOwnNodeOnceRegistered(context.Background(), st)
	if labels.Load() != 0 {
		t.Error("a node that never registered was labelled")
	}
}
