package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A --worker honours spec.network.nodeIP, spec.selinux and spec.node and refuses every
// other set field, naming it, rather than dropping it.
func TestLoadWorkerConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, wantIP string
		wantOptOut         bool
		wantNode           NodeSpec
		wantErr            []string
	}{
		{name: "empty file", yaml: ""},
		{name: "nodeIP", yaml: "spec:\n  network:\n    nodeIP: 10.50.0.4\n", wantIP: "10.50.0.4"},
		{name: "dual-stack nodeIP", yaml: "spec:\n  network:\n    nodeIP: 10.50.0.4,fd00::4\n", wantIP: "10.50.0.4,fd00::4"},
		{name: "kind only", yaml: "apiVersion: stratum.ciq.com/v1\nkind: ClusterConfig\n"},
		{name: "nodeIP key in other case", yaml: "spec:\n  network:\n    nodeip: 10.50.0.4\n", wantIP: "10.50.0.4"},
		{name: "empty and null entries set nothing", yaml: "spec:\n  network:\n    nodeIP: 10.50.0.4\n  vip: {}\n  operator:\n", wantIP: "10.50.0.4"},
		{name: "Spec key in other case", yaml: "Spec:\n  network:\n    nodeIP: 10.50.0.4\n  cis: true\n", wantErr: []string{"spec.cis"}},
		{name: "spec and Spec together", yaml: "spec:\n  network:\n    nodeIP: 10.50.0.4\nSpec:\n  cis: true\n", wantErr: []string{"spec.cis"}},
		{name: "Spec nodeIP only", yaml: "SPEC:\n  Network:\n    NodeIP: 10.50.0.4\n", wantIP: "10.50.0.4"},
		{name: "cis false", yaml: "spec:\n  cis: false\n", wantErr: []string{"spec.cis"}},
		{name: "retain zero", yaml: "spec:\n  snapshots:\n    retain: 0\n", wantErr: []string{"spec.snapshots.retain"}},
		{name: "empty string", yaml: "spec:\n  network:\n    podCIDR: \"\"\n", wantErr: []string{"spec.network.podCIDR"}},
		{name: "other network field", yaml: "spec:\n  network:\n    nodeIP: 10.50.0.4\n    podCIDR: 10.1.0.0/16\n", wantErr: []string{"spec.network.podCIDR"}},
		{name: "several fields", yaml: "spec:\n  cis: true\n  vip:\n    address: 10.0.0.10\n  snapshots:\n    retain: 3\n", wantErr: []string{"spec.cis", "spec.snapshots.retain", "spec.vip.address"}},
		{name: "explicit false", yaml: "spec:\n  operator: false\n", wantErr: []string{"spec.operator"}},
		{name: "misspelled key", yaml: "spec:\n  network:\n    nodeAddr: 10.50.0.4\n", wantErr: []string{"nodeAddr"}},
		{name: "wrong kind", yaml: "kind: Other\n", wantErr: []string{"not ClusterConfig"}},
		{name: "selinux off", yaml: "spec:\n  network:\n    nodeIP: 10.50.0.4\n  selinux: false\n", wantIP: "10.50.0.4", wantOptOut: true},
		{name: "selinux on", yaml: "spec:\n  selinux: true\n"},
		{name: "node facts", yaml: "spec:\n  node:\n    labels:\n      topology.kubernetes.io/zone: rack-1\n    taints:\n      - stratum.ciq.com/unverified=true:NoSchedule\n    runtimeHandlers:\n      nvidia: /usr/bin/nvidia-container-runtime\n",
			wantNode: NodeSpec{Labels: map[string]string{"topology.kubernetes.io/zone": "rack-1"}, Taints: []string{"stratum.ciq.com/unverified=true:NoSchedule"}, RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"}}},
		{name: "runtimeClasses are a control plane's", yaml: "spec:\n  runtimeClasses: [nvidia]\n", wantErr: []string{"spec.runtimeClasses"}},
		{name: "an engine-owned label", yaml: "spec:\n  node:\n    labels:\n      runtime.stratum.ciq.com/nvidia: \"true\"\n", wantErr: []string{"runtime.stratum.ciq.com/nvidia", "set by stratum itself"}},
		{name: "selinux off with another field", yaml: "spec:\n  selinux: false\n  cis: true\n", wantErr: []string{"spec.cis"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadWorkerConfig(path)
			if len(tc.wantErr) == 0 {
				want := WorkerConfig{NodeIP: tc.wantIP, SELinuxOptOut: tc.wantOptOut, Node: tc.wantNode}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("LoadWorkerConfig = %+v, %v; want %+v, nil", got, err, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("LoadWorkerConfig = %+v, nil; want an error naming %v", got, tc.wantErr)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestLoadWorkerConfigMissingFile(t *testing.T) {
	got, err := LoadWorkerConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || !reflect.DeepEqual(got, WorkerConfig{}) {
		t.Fatalf("LoadWorkerConfig(missing) = %+v, %v; want the zero config, nil", got, err)
	}
}
