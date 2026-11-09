package cluster

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

func stubLabelPatch(t *testing.T) (recordAtPatch, prune *[]string) {
	t.Helper()
	old := reconcileOwnNodeLabels
	t.Cleanup(func() { reconcileOwnNodeLabels = old })
	var rec, pr []string
	reconcileOwnNodeLabels = func(_ context.Context, _ string, _ map[string]string, _, p []string) error {
		pr = p
		if st, err := LoadState(); err == nil {
			rec = st.AppliedNodeLabels
		}
		return nil
	}
	return &rec, &pr
}

// A caller whose copy of node.yaml is older than the record still records
// before it patches: the skip is decided from node.yaml, not the copy.
func TestNodeLabelsRecordFromNodeYAMLNotTheCallersCopy(t *testing.T) {
	workerScratchHost(t)
	recordAtPatch, _ := stubLabelPatch(t)
	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	stale := &NodeState{Role: "worker", Hostname: "w1", NodeLabels: map[string]string{"zone": "r1"}, AppliedNodeLabels: []string{"zone"}}
	if err := reconcileNodeLabels(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*recordAtPatch, []string{"zone"}) {
		t.Errorf("node.yaml recorded %v when the label was patched, want [zone]", *recordAtPatch)
	}
}

// A reconcile that changes nothing in the record writes nothing.
func TestNodeLabelsLeaveAnUnchangedRecordUnwritten(t *testing.T) {
	workerScratchHost(t)
	stubLabelPatch(t)
	st := &NodeState{Role: "worker", Hostname: "w1", NodeLabels: map[string]string{"zone": "r1"}, AppliedNodeLabels: []string{"zone"}}
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	old := saveState
	t.Cleanup(func() { saveState = old })
	saves := 0
	saveState = func(s *NodeState) error { saves++; return old(s) }
	if err := reconcileNodeLabels(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if saves != 0 {
		t.Errorf("an unchanged record was saved %d times", saves)
	}
}

// A node.yaml that is missing or names another host is never written to:
// the labels are applied, but none recorded or pruned.
func TestNodeLabelsRecordNothingInAnotherNodesState(t *testing.T) {
	for name, saved := range map[string]*NodeState{
		"another host": {Role: "worker", Hostname: "other", AppliedNodeLabels: []string{"pool"}},
		"missing":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			workerScratchHost(t)
			_, prune := stubLabelPatch(t)
			if saved != nil {
				if err := SaveState(saved); err != nil {
					t.Fatal(err)
				}
			}
			patched := false
			inner := reconcileOwnNodeLabels
			reconcileOwnNodeLabels = func(ctx context.Context, h string, l map[string]string, hs, p []string) error {
				patched = true
				return inner(ctx, h, l, hs, p)
			}
			st := &NodeState{Role: "worker", Hostname: "w1", NodeLabels: map[string]string{"zone": "r1"}}
			if err := reconcileNodeLabels(context.Background(), st); err != nil {
				t.Fatal(err)
			}
			if !patched || *prune != nil {
				t.Errorf("patched %v with prune %v, want the labels applied and nothing pruned", patched, *prune)
			}
			got, err := LoadState()
			switch {
			case saved == nil && !os.IsNotExist(err):
				t.Errorf("a missing node.yaml was created: %v, %v", got, err)
			case saved != nil && (err != nil || got.Hostname != "other" || !slices.Equal(got.AppliedNodeLabels, []string{"pool"})):
				t.Errorf("another node's state changed to %+v, %v", got, err)
			}
		})
	}
}

// A bare worker restart validates the labels it saved, so a key the engine
// no longer accepts is refused before anything changes, and
// --clear-node-labels or --node-label replaces it.
func TestWorkerRestartRefusesSavedLabelsItNoLongerAccepts(t *testing.T) {
	saved := &NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8", KubeletNodeIP: "192.0.2.8",
		NodeLabels: map[string]string{"failure-domain.beta.kubernetes.io/zone": "r1"}}
	for name, tc := range map[string]struct {
		node    NodeSpec
		wantErr bool
		want    map[string]string
	}{
		"a bare restart":        {wantErr: true, want: saved.NodeLabels},
		"--clear-node-labels":   {node: NodeSpec{Labels: map[string]string{}}, want: nil},
		"--node-label replaces": {node: NodeSpec{Labels: map[string]string{"topology.kubernetes.io/zone": "r1"}}, want: map[string]string{"topology.kubernetes.io/zone": "r1"}},
	} {
		t.Run(name, func(t *testing.T) {
			workerScratchHost(t)
			stubWorkerStartHost(t)
			if err := SaveState(saved); err != nil {
				t.Fatal(err)
			}
			_, err := PrepareWorker(context.Background(), WorkerOptions{Node: tc.node})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--clear-node-labels") || !strings.Contains(err.Error(), "failure-domain") {
					t.Fatalf("PrepareWorker = %v, want a refusal naming the label and --clear-node-labels", err)
				}
			} else if err != nil {
				t.Fatalf("PrepareWorker = %v", err)
			}
			if got := mustLoadState(t).NodeLabels; !maps.Equal(got, tc.want) {
				t.Errorf("saved labels = %v, want %v", got, tc.want)
			}
		})
	}
}
