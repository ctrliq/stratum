package cluster

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
)

func TestNodeSpecValidate(t *testing.T) {
	good := NodeSpec{
		Labels:          map[string]string{"topology.kubernetes.io/zone": "rack-1", "gpu": ""},
		Taints:          []string{"stratum.ciq.com/unverified=true:NoSchedule", "dedicated:NoExecute"},
		RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed spec.node was refused: %v", err)
	}
	for name, n := range map[string]NodeSpec{
		"label key":              {Labels: map[string]string{"bad key": "x"}},
		"label value":            {Labels: map[string]string{"zone": "rack 1"}},
		"control-plane label":    {Labels: map[string]string{"stratum.ciq.com/control-plane": "true"}},
		"handler label":          {Labels: map[string]string{"runtime.stratum.ciq.com/nvidia": "true"}},
		"taint without effect":   {Taints: []string{"dedicated=gpu"}},
		"taint effect":           {Taints: []string{"dedicated=gpu:NoRun"}},
		"taint two colons":       {Taints: []string{"a:b:NoSchedule"}},
		"taint key":              {Taints: []string{"bad key:NoSchedule"}},
		"taint value":            {Taints: []string{"dedicated=bad value:NoSchedule"}},
		"engine taint":           {Taints: []string{"runtime.stratum.ciq.com/nvidia=true:NoSchedule"}},
		"taint declared twice":   {Taints: []string{"dedicated=a:NoSchedule", "dedicated=b:NoSchedule"}},
		"handler named runc":     {RuntimeHandlers: map[string]string{"runc": "/usr/bin/runc"}},
		"handler relative path":  {RuntimeHandlers: map[string]string{"nvidia": "nvidia-container-runtime"}},
		"handler metacharacters": {RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/$(id)"}},
	} {
		if err := n.Validate(); err == nil {
			t.Errorf("%s: %+v was accepted", name, n)
		}
	}
	same := NodeSpec{Taints: []string{"dedicated=a:NoSchedule", "dedicated=a:NoExecute"}}
	if err := same.Validate(); err != nil {
		t.Errorf("one key with two effects is two taints, and was refused: %v", err)
	}
}

func TestParseKeyValues(t *testing.T) {
	got, err := ParseKeyValues("--node-label", []string{"zone=rack-1", "gpu=", "a=b=c"})
	want := map[string]string{"zone": "rack-1", "gpu": "", "a": "b=c"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseKeyValues = %v, %v; want %v", got, err, want)
	}
	if got, err := ParseKeyValues("--node-label", nil); got != nil || err != nil {
		t.Errorf("no values = %v, %v; want nil, which a restart reads as not given", got, err)
	}
	for _, bad := range [][]string{{"zone"}, {"=rack-1"}, {"zone=a", "zone=b"}} {
		if _, err := ParseKeyValues("--node-label", bad); err == nil || !strings.Contains(err.Error(), "--node-label") {
			t.Errorf("ParseKeyValues(%q) = %v; want a refusal naming the flag", bad, err)
		}
	}
}

func TestMergeNodeSpecLetsAFlagReplaceTheFilesEntry(t *testing.T) {
	file := NodeSpec{
		Labels:          map[string]string{"zone": "rack-1", "team": "ml"},
		Taints:          []string{"dedicated=ml:NoSchedule", "stratum.ciq.com/unverified=true:NoSchedule"},
		RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"},
	}
	flags := NodeSpec{
		Labels:          map[string]string{"zone": "rack-2"},
		Taints:          []string{"dedicated=gpu:NoSchedule"},
		RuntimeHandlers: map[string]string{"gvisor": "/usr/local/bin/runsc"},
	}
	got := mergeNodeSpec(file, flags)
	want := NodeSpec{
		Labels:          map[string]string{"zone": "rack-2", "team": "ml"},
		Taints:          []string{"stratum.ciq.com/unverified=true:NoSchedule", "dedicated=gpu:NoSchedule"},
		RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime", "gvisor": "/usr/local/bin/runsc"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeNodeSpec = %+v, want %+v", got, want)
	}
	if file.Labels["zone"] != "rack-1" {
		t.Error("the merge modified the config file's labels in place")
	}
	if got := mergeNodeSpec(NodeSpec{}, NodeSpec{}); !reflect.DeepEqual(got, NodeSpec{}) {
		t.Errorf("merging nothing = %+v, want the zero spec.node", got)
	}
}

func TestLoadConfigRefusesABadSpecNode(t *testing.T) {
	for name, yaml := range map[string]string{
		"engine label":         "spec:\n  node:\n    labels:\n      stratum.ciq.com/control-plane: \"true\"\n",
		"bad taint":            "spec:\n  node:\n    taints: [dedicated]\n",
		"handler named runc":   "spec:\n  node:\n    runtimeHandlers:\n      runc: /usr/bin/runc\n",
		"runtime class":        "spec:\n  runtimeClasses: [Nvidia]\n",
		"runtime class twice":  "spec:\n  runtimeClasses: [nvidia, nvidia]\n",
		"runtime class runc":   "spec:\n  runtimeClasses: [runc]\n",
		"misspelled node key":  "spec:\n  node:\n    label:\n      zone: a\n",
		"runtime classes list": "spec:\n  runtimeClasses: nvidia\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: LoadConfig accepted\n%s", name, yaml)
		}
	}
}

// executable writes an executable file standing in for a handler's runtime.
func executable(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nvidia-container-runtime")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWorkerJoinSavesItsNodeFacts(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "gpu-1", "10.80.0.5", []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"))
	recordTrust(t)
	bin := executable(t)

	_, err := PrepareWorker(context.Background(), WorkerOptions{
		Server: "https://cp:6443", Token: "abcdef.0123456789abcdef", CAHash: "sha256:aa",
		ConfigNode: NodeSpec{Labels: map[string]string{"zone": "rack-1"}, RuntimeHandlers: map[string]string{"nvidia": bin}},
		Node:       NodeSpec{Taints: []string{"stratum.ciq.com/unverified=true:NoSchedule"}},
	})
	if err != nil {
		t.Fatalf("PrepareWorker = %v", err)
	}
	st := mustLoadState(t)
	if !reflect.DeepEqual(st.NodeLabels, map[string]string{"zone": "rack-1"}) ||
		!reflect.DeepEqual(st.NodeTaints, []string{"stratum.ciq.com/unverified=true:NoSchedule"}) ||
		!reflect.DeepEqual(st.RuntimeHandlers, map[string]string{"nvidia": bin}) {
		t.Errorf("saved node facts = labels %v, taints %v, handlers %v; want the file's and the flags'", st.NodeLabels, st.NodeTaints, st.RuntimeHandlers)
	}
}

// A join refuses a bad or uninstalled spec.node before it touches the host:
// the host-preparation and directory seams fail the test if reached.
func TestWorkerJoinRefusesBadNodeFactsBeforeTheHost(t *testing.T) {
	for name, o := range map[string]WorkerOptions{
		"engine-owned label": {Node: NodeSpec{Labels: map[string]string{"runtime.stratum.ciq.com/x": "true"}}},
		"bad taint":          {ConfigNode: NodeSpec{Taints: []string{"dedicated"}}},
		"missing binary":     {Node: NodeSpec{RuntimeHandlers: map[string]string{"nvidia": "/nonexistent/nvidia-container-runtime"}}},
	} {
		t.Run(name, func(t *testing.T) {
			redirectJoinDirs(t)
			stubJoinHost(t, "gpu-1", "10.80.0.5", nil)
			hostDirs = func() error { t.Fatal("the join created directories before refusing spec.node"); return nil }
			hostPrereqs = func(host.PrereqOptions) error {
				t.Fatal("the join prepared the host before refusing spec.node")
				return nil
			}
			o.Server, o.Token, o.CAHash = "https://cp:6443", "abcdef.0123456789abcdef", "sha256:aa"
			if _, err := PrepareWorker(context.Background(), o); err == nil {
				t.Fatal("PrepareWorker accepted it")
			}
		})
	}
}

func TestWorkerRestartNodeFacts(t *testing.T) {
	bin := executable(t)
	saved := &NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8", KubeletNodeIP: "192.0.2.8",
		NodeLabels: map[string]string{"zone": "rack-1"}, NodeTaints: []string{"dedicated=ml:NoSchedule"},
		RuntimeHandlers: map[string]string{"nvidia": bin}}

	t.Run("a bare restart keeps what the join saved", func(t *testing.T) {
		workerScratchHost(t)
		stubWorkerStartHost(t)
		if err := SaveState(saved); err != nil {
			t.Fatal(err)
		}
		st, err := PrepareWorker(context.Background(), WorkerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got := mustLoadState(t)
		for _, s := range []*NodeState{st, got} {
			if !reflect.DeepEqual(s.NodeLabels, saved.NodeLabels) || !reflect.DeepEqual(s.NodeTaints, saved.NodeTaints) || !reflect.DeepEqual(s.RuntimeHandlers, saved.RuntimeHandlers) {
				t.Errorf("node facts = %v %v %v, want the saved ones kept", s.NodeLabels, s.NodeTaints, s.RuntimeHandlers)
			}
		}
	})

	t.Run("a flag replaces its field and the others stay", func(t *testing.T) {
		workerScratchHost(t)
		stubWorkerStartHost(t)
		if err := SaveState(saved); err != nil {
			t.Fatal(err)
		}
		moved := map[string]string{"nvidia": bin, "gvisor": bin}
		st, err := PrepareWorker(context.Background(), WorkerOptions{Node: NodeSpec{RuntimeHandlers: moved}})
		if err != nil {
			t.Fatal(err)
		}
		got := mustLoadState(t)
		for _, s := range []*NodeState{st, got} {
			if !reflect.DeepEqual(s.RuntimeHandlers, moved) {
				t.Errorf("runtime handlers = %v, want the restart's %v", s.RuntimeHandlers, moved)
			}
			if !reflect.DeepEqual(s.NodeLabels, saved.NodeLabels) || !reflect.DeepEqual(s.NodeTaints, saved.NodeTaints) {
				t.Errorf("labels %v, taints %v; want the fields no flag gave kept", s.NodeLabels, s.NodeTaints)
			}
		}
	})

	t.Run("a bad value refuses before the host and saves nothing", func(t *testing.T) {
		workerScratchHost(t)
		stubWorkerStartHost(t)
		if err := SaveState(saved); err != nil {
			t.Fatal(err)
		}
		hostPrereqs = func(host.PrereqOptions) error {
			t.Fatal("the restart prepared the host before refusing spec.node")
			return nil
		}
		_, err := PrepareWorker(context.Background(), WorkerOptions{Node: NodeSpec{Labels: map[string]string{"stratum.ciq.com/control-plane": "true"}}})
		if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
			t.Fatalf("PrepareWorker = %v, want a refusal", err)
		}
		if got := mustLoadState(t); !reflect.DeepEqual(got.NodeLabels, saved.NodeLabels) {
			t.Errorf("labels = %v, want the saved ones untouched", got.NodeLabels)
		}
	})

	t.Run("a missing binary only warns on a restart", func(t *testing.T) {
		workerScratchHost(t)
		stubWorkerStartHost(t)
		if err := SaveState(saved); err != nil {
			t.Fatal(err)
		}
		gone := map[string]string{"nvidia": "/nonexistent/nvidia-container-runtime"}
		if _, err := PrepareWorker(context.Background(), WorkerOptions{Node: NodeSpec{RuntimeHandlers: gone}}); err != nil {
			t.Fatalf("PrepareWorker = %v; a restart must not refuse a handler whose binary is mid-reinstall", err)
		}
		if got := mustLoadState(t); !reflect.DeepEqual(got.RuntimeHandlers, gone) {
			t.Errorf("runtime handlers = %v, want %v saved", got.RuntimeHandlers, gone)
		}
	})
}
