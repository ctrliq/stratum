package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
)

// A worker join hands PrepareWorker the spec.node flags apart from the config
// file's, so the merge, and which source a refusal names, are PrepareWorker's.
func TestRunUpWorkerJoinHandsOnTheNodeFacts(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "nvidia-container-runtime")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	join := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa",
		nodeLabels: []string{"zone=rack-2"}, nodeTaints: []string{"stratum.ciq.com/unverified=true:NoSchedule"},
		runtimeHandlers: []string{"nvidia=" + bin}}
	config := "spec:\n  node:\n    labels:\n      team: ml\n"
	call, err := runUpWorker(t, join, config, nil)
	if err != nil {
		t.Fatalf("runUp = %v", err)
	}
	wantFlags := cluster.NodeSpec{
		Labels:          map[string]string{"zone": "rack-2"},
		Taints:          []string{"stratum.ciq.com/unverified=true:NoSchedule"},
		RuntimeHandlers: map[string]string{"nvidia": bin},
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

// A malformed or refused flag stops runUp before anything else: the start
// lock it takes next is never created.
func TestRunUpRefusesABadNodeFlagBeforeAnything(t *testing.T) {
	for _, o := range []runUpOptions{
		{nodeLabels: []string{"zone"}},
		{runtimeHandlers: []string{"nvidia"}},
		{nodeLabels: []string{"zone=a", "zone=b"}},
		{nodeTaints: []string{"dedicated"}},
		{nodeLabels: []string{"stratum.ciq.com/control-plane=true"}},
		{nodeLabels: []string{"node-role.kubernetes.io/worker="}},
		{runtimeHandlers: []string{"runc=/usr/bin/runc"}},
	} {
		call, err := runUpWorker(t, o, "", &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.50.0.4"})
		if err == nil || !strings.Contains(err.Error(), "Nothing on this host was changed") {
			t.Errorf("runUp(%+v) = %v, want a refusal", o, err)
		}
		if call.called {
			t.Errorf("runUp(%+v) reached PrepareWorker", o)
		}
		if _, err := os.Stat(startLockPath); !os.IsNotExist(err) {
			t.Errorf("runUp(%+v) took the start lock before refusing the flag: %v", o, err)
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

// A control-plane --join reads spec.node, so the refusal of the node flags
// points there rather than telling the operator to drop them; the network
// flags beside them are still to be removed.
func TestUpJoinRefusalPointsTheNodeFlagsAtSpecNode(t *testing.T) {
	join := runUpOptions{join: "https://10.0.0.11:6443", nodeLabels: []string{"zone=a"}}
	err := upFlagConflict(join)
	if err == nil || !strings.Contains(err.Error(), "spec.node field named beside it") || strings.Contains(err.Error(), "remove these flags") {
		t.Errorf("node flags on a --join: %v; want them pointed at spec.node, not removed", err)
	}
	join.podCIDR = "10.50.0.0/16"
	err = upFlagConflict(join)
	for _, want := range []string{"remove the network flags", "spec.node field named beside it"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("network and node flags on a --join: %v; want it to say %q", err, want)
		}
	}
}

// A worker join whose config file declares a handler with no binary on the
// host is refused before the run takes its start lock, so staging and the
// SELinux relabel that follow it never touch the host.
func TestRunUpRefusesAJoinsMissingHandlerBinaryBeforeAnything(t *testing.T) {
	join := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa"}
	config := "spec:\n  selinux: false\n  node:\n    runtimeHandlers:\n      nvidia: /nonexistent/nvidia-container-runtime\n"
	call, err := runUpWorker(t, join, config, nil)
	if err == nil || !strings.Contains(err.Error(), "Nothing on this host was changed") || !strings.Contains(err.Error(), "nvidia") {
		t.Fatalf("runUp = %v, want the join refused naming the handler", err)
	}
	if call.called {
		t.Error("the join reached PrepareWorker")
	}
	if _, err := os.Stat(startLockPath); !os.IsNotExist(err) {
		t.Errorf("the join took the start lock before refusing its handler: %v", err)
	}
}
