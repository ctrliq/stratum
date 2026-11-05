package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/version"
)

// runUpWorkerJoinReal drives the real runUp worker-join path the way
// runUpWorker (up_worker_runup_test.go) does, with one difference: the
// prepareWorkerFn stub keeps the join's real refusal head — the same
// ResolveNodeIP precedence and the same host.CheckNodeIP call real
// PrepareWorker makes at the head of a new join (internal/cluster/worker.go
// ~303-313) — because the package's TestMain keeps the real PrepareWorker
// away from the host's firewall and kernel settings. Under the fix, a bad
// --node-ip is refused by runUp's pre-stage check before anything is staged,
// so the stub's refusal is never reached in the refusal subtests; the stub
// exists for the valid-IP path, which exercises the same resolution and
// check real PrepareWorker runs. The paths a start writes are
// redirected into a temp dir, the staging extraction is a counting stub, and
// the steps past the join (the reboot unit, the worker run) are stubbed: a
// refused join never reaches them, a valid one does.
func runUpWorkerJoinReal(t *testing.T, o runUpOptions) (stages func() int32, err error) {
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
	if err := os.WriteFile(o.configPath, []byte("spec: {}\n"), 0o600); err != nil {
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
		// The head of a new join, exactly as real PrepareWorker runs it:
		// the same resolution precedence and the same check. --node-ip is
		// always given here, so detection is never reached.
		choice, err := host.ResolveNodeIP(w.NodeIP, w.ConfigNodeIP, "", func() (string, error) {
			return "", errors.New("no node-IP detection in the test")
		})
		if err != nil {
			return nil, err
		}
		if err := host.CheckNodeIP(choice.IP, strings.Contains(choice.IP, ",")); err != nil {
			return nil, fmt.Errorf("worker join: %s %q cannot be this worker's node IP: %w", choice.Source, choice.IP, err)
		}
		return &cluster.NodeState{Role: "worker", Hostname: "w1", Server: w.Server}, nil
	}

	o.worker = true
	err = runUp(context.Background(), o)
	return stageN.Load, err
}

// A worker join refuses every node IP the kubelet could not register — the
// refusals host.CheckNodeIP makes — before it changes anything on the host:
// the install guide's "The join checks the address before it changes anything
// on the host". Staging the payload (211M of binaries, 510M of images) is the
// first big change a join makes, so every refusal must come back with the
// staging seam never called. Each case expects a substring of the real
// refusal, in the words CheckNodeIP or its callers use.
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
			if !strings.Contains(err.Error(), "cannot be this worker's node IP") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the refusal %q does not name %s", err, tc.wantErr)
			}
			if n := stages(); n != 0 {
				t.Errorf("the refused join staged the payload %d time(s) before refusing: a join must check --node-ip before it changes anything on the host, and staging is that change", n)
			}
		})
	}
}

// The guard for the refusal above: a valid --node-ip still reaches staging
// and the join goes on to run, so the pre-stage check refuses only what
// CheckNodeIP refuses, and nothing else about the join moved.
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
