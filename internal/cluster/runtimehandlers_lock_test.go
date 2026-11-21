package cluster

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/runtime"
)

// handlersLockHeld reports whether another holder has the runtime-handlers lock now.
func handlersLockHeld(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	release, err := lockRuntimeHandlers(ctx, io.Discard)
	if err != nil {
		return true
	}
	release()
	return false
}

func TestRuntimeApplyAndRestartHoldTheHandlersLock(t *testing.T) {
	runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1"})
	var heldAtWrite, heldAtRestart bool
	applyHandlers = func(io.Writer, map[string]string, bool) (runtime.ApplyHandlersResult, error) {
		heldAtWrite = handlersLockHeld(t)
		return runtime.ApplyHandlersResult{Changed: true, Written: true}, nil
	}
	oldRestart := restartContainerd
	t.Cleanup(func() { restartContainerd = oldRestart })
	restartContainerd = func(io.Writer) error { heldAtRestart = handlersLockHeld(t); return nil }
	if _, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Handlers: map[string]string{"gvisor": executable(t)}}); err != nil {
		t.Fatal(err)
	}
	if err := RestartRuntime(io.Discard); err != nil {
		t.Fatal(err)
	}
	if !heldAtWrite || !heldAtRestart {
		t.Errorf("lock held at the drop-in write %v, at the restart %v; want both", heldAtWrite, heldAtRestart)
	}
	if handlersLockHeld(t) {
		t.Error("the lock outlived the apply and the restart")
	}
}

// The relabel a containerd start runs reads node.yaml under the lock, so one
// that starts while `stratum runtime apply` holds it labels from the handlers
// the apply saved, not the ones it would have read before.
func TestLabelOwnNodeReadsTheHandlersUnderTheLock(t *testing.T) {
	workerScratchHost(t)
	gvisor, kata := executable(t), executable(t)
	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: map[string]string{"gvisor": gvisor}}); err != nil {
		t.Fatal(err)
	}
	old := reconcileOwnNodeLabels
	t.Cleanup(func() { reconcileOwnNodeLabels = old })
	labelled := make(chan []string, 1)
	reconcileOwnNodeLabels = func(_ context.Context, _ string, _ map[string]string, handlers, _ []string) error {
		labelled <- handlers
		return nil
	}
	release, err := lockRuntimeHandlers(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	go labelOwnNode(context.Background(), &NodeState{Hostname: "w1"})
	time.Sleep(50 * time.Millisecond)
	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: map[string]string{"gvisor": gvisor, "kata": kata}}); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-labelled:
		if !slices.Equal(got, []string{"gvisor", "kata"}) {
			t.Errorf("labelled for %v, want [gvisor kata] the apply saved", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relabel never ran")
	}
}

// A worker start writes the drop-in from node.yaml under the lock, not from
// the state it read before host preparation: a `stratum runtime apply` in
// between saved newer handlers.
func TestWorkerStartWritesTheHandlersNodeYAMLHolds(t *testing.T) {
	workerScratchHost(t)
	gvisor, kata := executable(t), executable(t)
	started := &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: map[string]string{"gvisor": gvisor}}
	applied := map[string]string{"gvisor": gvisor, "kata": kata}
	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: applied}); err != nil {
		t.Fatal(err)
	}
	old := ensureContainerdConfigs
	t.Cleanup(func() { ensureContainerdConfigs = old })
	var written map[string]string
	var held bool
	ensureContainerdConfigs = func(_ io.Writer, st *NodeState) (bool, error) {
		written, held = st.RuntimeHandlers, handlersLockHeld(t)
		return false, nil
	}
	if err := ensureWorkerContainerdConfigs(context.Background(), started); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(written, applied) || !held {
		t.Errorf("wrote %v (lock held %v), want %v under the lock", written, held, applied)
	}
}

// A containerd that cannot say which handlers it loaded labels nothing: the
// reconcile fails, to be retried, rather than label every declared handler.
func TestNodeLabelsWaitForAContainerdThatCannotSay(t *testing.T) {
	workerScratchHost(t)
	oldLoaded, oldReconcile := loadedHandlers, reconcileOwnNodeLabels
	t.Cleanup(func() { loadedHandlers, reconcileOwnNodeLabels = oldLoaded, oldReconcile })
	loadedHandlers = func(context.Context, []string) ([]string, error) { return nil, errors.New("connection refused") }
	patched := false
	reconcileOwnNodeLabels = func(context.Context, string, map[string]string, []string, []string) error { patched = true; return nil }
	st := &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: map[string]string{"gvisor": executable(t)}}
	if err := reconcileNodeLabels(context.Background(), st); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("reconcile = %v, want containerd's error", err)
	}
	if patched {
		t.Error("the Node was labelled while containerd could not say what it loaded")
	}
}
