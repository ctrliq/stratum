package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
)

// A worker join hands PrepareWorker the spec.node flags apart from the config
// file's, so the merge, and which source a refusal names, are PrepareWorker's.
func TestRunUpWorkerJoinHandsOnTheNodeFacts(t *testing.T) {
	join := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa",
		nodeLabels: []string{"zone=rack-2"}, nodeTaints: []string{"stratum.ciq.com/unverified=true:NoSchedule"},
		runtimeHandlers: []string{"nvidia=/usr/bin/nvidia-container-runtime"}}
	config := "spec:\n  node:\n    labels:\n      team: ml\n"
	call, err := runUpWorker(t, join, config, nil)
	if err != nil {
		t.Fatalf("runUp = %v", err)
	}
	wantFlags := cluster.NodeSpec{
		Labels:          map[string]string{"zone": "rack-2"},
		Taints:          []string{"stratum.ciq.com/unverified=true:NoSchedule"},
		RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"},
	}
	if !reflect.DeepEqual(call.node, wantFlags) {
		t.Errorf("flags reached PrepareWorker as %+v, want %+v", call.node, wantFlags)
	}
	if want := (cluster.NodeSpec{Labels: map[string]string{"team": "ml"}}); !reflect.DeepEqual(call.configNode, want) {
		t.Errorf("config spec.node reached PrepareWorker as %+v, want %+v", call.configNode, want)
	}
}

// A restart reads no spec.node from the file and passes only the flags it was
// given; a flag not given stays nil so the saved field is kept.
func TestRunUpWorkerRestartHandsOnOnlyTheGivenNodeFlags(t *testing.T) {
	saved := &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.50.0.4", Server: "https://10.50.0.3:6443"}
	call, err := runUpWorker(t, runUpOptions{runtimeHandlers: []string{"nvidia=/usr/bin/nvidia-container-runtime"}},
		"spec:\n  node:\n    labels:\n      team: ml\n", saved)
	if err != nil {
		t.Fatalf("runUp = %v", err)
	}
	if call.node.Labels != nil || call.node.Taints != nil {
		t.Errorf("restart passed labels %v and taints %v it was not given", call.node.Labels, call.node.Taints)
	}
	if !reflect.DeepEqual(call.node.RuntimeHandlers, map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"}) {
		t.Errorf("restart passed handlers %v, want the flag's", call.node.RuntimeHandlers)
	}
	if call.configNode.Labels != nil {
		t.Errorf("restart read spec.node from the config file: %+v", call.configNode)
	}
}

// A malformed flag is refused as the first thing runUp does, before staging
// or any other host write.
func TestRunUpRefusesAMalformedNodeFlagBeforePrepareWorker(t *testing.T) {
	for _, o := range []runUpOptions{
		{nodeLabels: []string{"zone"}},
		{runtimeHandlers: []string{"nvidia"}},
		{nodeLabels: []string{"zone=a", "zone=b"}},
	} {
		o.worker = true
		if err := runUp(context.Background(), o); err == nil || !strings.Contains(err.Error(), "Nothing on this host was changed") {
			t.Errorf("runUp(%+v) = %v, want a refusal before anything else runs", o, err)
		}
		call, err := runUpWorker(t, o, "", &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.50.0.4"})
		if err == nil || !strings.Contains(err.Error(), "Nothing on this host was changed") {
			t.Errorf("runUp(%+v) = %v, want a refusal", o, err)
		}
		if call.called {
			t.Errorf("runUp(%+v) reached PrepareWorker", o)
		}
	}
}

// A control plane reads spec.node from its file, so the worker flags without
// --worker would be dropped: each is refused, named with its field.
func TestUpRefusesTheNodeFlagsWithoutWorker(t *testing.T) {
	err := upFlagConflict(runUpOptions{nodeLabels: []string{"zone=a"}, nodeTaints: []string{"d:NoSchedule"}, runtimeHandlers: []string{"nvidia=/usr/bin/x"}})
	if err == nil {
		t.Fatal("upFlagConflict accepted the node flags without --worker")
	}
	for _, want := range []string{`--node-label "zone=a" (spec.node.labels)`, `--node-taint "d:NoSchedule" (spec.node.taints)`, `--runtime-handler "nvidia=/usr/bin/x" (spec.node.runtimeHandlers)`, "spec.node at every start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not say %q: %v", want, err)
		}
	}
	if err := upFlagConflict(runUpOptions{worker: true, nodeLabels: []string{"zone=a"}}); err != nil {
		t.Errorf("a worker given --node-label was refused: %v", err)
	}
}
