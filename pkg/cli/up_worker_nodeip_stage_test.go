package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/version"
)

// the pre-stage check is the real cluster.ResolveJoinNodeIP; only it can refuse a bad --node-ip
func runUpWorkerJoinReal(t *testing.T, o runUpOptions) (stages func() int32, err error) {
	t.Helper()
	return runUpWorkerJoinRealWithConfig(t, o, "spec: {}\n")
}

func runUpWorkerJoinRealWithConfig(t *testing.T, o runUpOptions, config string) (stages func() int32, err error) {
	t.Helper()
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	o.configPath = filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(o.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	oldStage := stagePayload
	t.Cleanup(func() { stagePayload = oldStage })
	var stageN atomic.Int32
	stagePayload = func(string) ([]string, error) { stageN.Add(1); return nil, nil }
	oldInstall, oldRun, oldJoin := installUnit, runWorkerFn, prepareWorkerFn
	t.Cleanup(func() { installUnit, runWorkerFn, prepareWorkerFn = oldInstall, oldRun, oldJoin })
	installUnit = func([]string) error { return nil }
	runWorkerFn = func(context.Context, *cluster.NodeState) error { return nil }
	prepareWorkerFn = func(_ context.Context, w cluster.WorkerOptions) (*cluster.NodeState, error) {
		return &cluster.NodeState{Role: "worker", Hostname: "w1", Server: w.Server}, nil
	}

	o.worker = true
	err = runUp(context.Background(), o)
	return stageN.Load, err
}

func TestRunUpWorkerJoinRefusesABadNodeIPBeforeItStages(t *testing.T) {
	for _, tc := range []struct {
		name, nodeIP, wantErr string
		dual                  bool
	}{
		{name: "loopback", nodeIP: "127.0.0.1", wantErr: `"127.0.0.1" is a loopback address`},
		{name: "all zeros", nodeIP: "0.0.0.0", wantErr: `"0.0.0.0" is the all-zeros address`},
		{name: "multicast", nodeIP: "224.0.0.1", wantErr: `"224.0.0.1" is a multicast address`},
		{name: "link-local", nodeIP: "169.254.10.1", wantErr: `"169.254.10.1" is a link-local address`},
		{name: "broadcast", nodeIP: "255.255.255.255", wantErr: `"255.255.255.255" is the broadcast address`},
		{name: "not an IP address", nodeIP: "10.80.0.300", wantErr: `"10.80.0.300" is not an IP address`},
		{name: "an interface zone", nodeIP: "fe80::1%en0", wantErr: `"fe80::1%en0" carries an interface zone`},
		{name: "IPv6 on a single-stack worker", nodeIP: "fd00::1", wantErr: "a single-stack node's IP is an IPv4 address"},
		{name: "a reversed dual-stack pair", nodeIP: "fd00::1,10.80.0.4", dual: true, wantErr: "a dual-stack node's pair lists its IPv4 first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa", nodeIP: tc.nodeIP, dual: tc.dual}
			stages, err := runUpWorkerJoinReal(t, o)
			if err == nil {
				t.Fatalf("runUp(--worker --node-ip %q) = nil; want a refusal", tc.nodeIP)
			}
			if !strings.Contains(err.Error(), "cannot be this worker's node IP") || !strings.Contains(err.Error(), tc.wantErr) ||
				!strings.Contains(err.Error(), "Nothing on this host was changed") {
				t.Errorf("the refusal %q does not name %s", err, tc.wantErr)
			}
			if n := stages(); n != 0 {
				t.Errorf("the refused join staged the payload %d time(s) before refusing: a join must check --node-ip before it changes anything on the host, and staging is that change", n)
			}
		})
	}
}

func TestRunUpWorkerJoinWithAValidNodeIPStillStages(t *testing.T) {
	o := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa", nodeIP: "10.80.0.4"}
	stages, err := runUpWorkerJoinReal(t, o)
	if err != nil {
		t.Fatalf("runUp(--worker --node-ip 10.80.0.4) = %v, want a completed join", err)
	}
	if n := stages(); n != 1 {
		t.Errorf("the join staged the payload %d time(s), want 1", n)
	}
}

func TestRunUpWorkerJoinRefusesABadConfigOrEnvNodeIPBeforeItStages(t *testing.T) {
	for _, tc := range []struct {
		name, config, env, wantSource string
	}{
		{name: "spec.network.nodeIP", config: "spec:\n  network:\n    nodeIP: 127.0.0.1\n", wantSource: `spec.network.nodeIP "127.0.0.1"`},
		{name: "$NODE_IP", config: "spec: {}\n", env: "127.0.0.1", wantSource: `$NODE_IP "127.0.0.1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NODE_IP", tc.env)
			o := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa"}
			stages, err := runUpWorkerJoinRealWithConfig(t, o, tc.config)
			if err == nil {
				t.Fatalf("runUp(--worker) with a loopback %s = nil; want a refusal", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSource) || !strings.Contains(err.Error(), "Nothing on this host was changed") {
				t.Errorf("the refusal %q does not name %s", err, tc.wantSource)
			}
			if n := stages(); n != 0 {
				t.Errorf("the refused join staged the payload %d time(s) before refusing", n)
			}
		})
	}
}

func TestRunUpWorkerJoinWithAValidFlagOverABadConfigNodeIPStillStages(t *testing.T) {
	t.Setenv("NODE_IP", "")
	o := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa", nodeIP: "10.80.0.4"}
	stages, err := runUpWorkerJoinRealWithConfig(t, o, "spec:\n  network:\n    nodeIP: 127.0.0.1\n")
	if err != nil {
		t.Fatalf("runUp(--worker --node-ip 10.80.0.4) with a loopback spec.network.nodeIP = %v, want a completed join", err)
	}
	if n := stages(); n != 1 {
		t.Errorf("the join staged the payload %d time(s), want 1", n)
	}
}
