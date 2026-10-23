package etcdsnap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

// fakeKubectl stands in for seedKubectl: it records each call's arguments
// and answers with the exit status failures maps its key to. A key is the verb
// and object: "get ds/cilium", "restart deploy/coredns", "status ds/cilium".
type fakeKubectl struct {
	mu          sync.Mutex
	calls       []string
	args        map[string]string // each call's full argument line, by key
	failures    map[string]int
	noCilium    bool
	noCoreDNS   bool
	noLocalPath bool
	onStatus    func()          // called as each rollout status begins, before it returns
	hang        map[string]bool // keys whose kubectl never answers, until its context ends
}

// got is the recorded calls in a stable order: the rollout waits run together,
// so their order is not.
func (f *fakeKubectl) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := slices.Clone(f.calls)
	slices.Sort(g)
	return g
}

func sorted(s ...string) []string { slices.Sort(s); return s }

func (f *fakeKubectl) cmd(ctx context.Context, args ...string) *exec.Cmd {
	call := strings.Join(args, " ")
	key := ""
	for _, verb := range []string{"get", "restart", "status"} {
		if i := slices.Index(args, verb); i >= 0 {
			key = verb + " " + args[i+1]
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	if f.args == nil {
		f.args = map[string]string{}
	}
	f.args[key] = call
	f.mu.Unlock()
	if strings.HasPrefix(key, "status") && f.onStatus != nil {
		f.onStatus()
	}
	out := key
	if (key == "get ds/cilium" && f.noCilium) || (key == "get deploy/coredns" && f.noCoreDNS) ||
		(key == "get deploy/local-path-provisioner" && f.noLocalPath) {
		out = ""
	}
	if f.hang[key] {
		// exec, so the process the context kills is the one holding the pipes
		return exec.CommandContext(ctx, "sh", "-c", "exec sleep 30")
	}
	return exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("echo %q; exit %d", out, f.failures[key])+" # "+strings.ReplaceAll(call, "\n", " "))
}

// withFakeKubectl builds an effects value whose seedKubectl is f.cmd, with the
// production timing knobs: the caller drives restartClusterWorkloads and
// waitSeedHealthz through it, so no package variable is swapped.
func withFakeKubectl(t *testing.T, f *fakeKubectl) *restoreEffects {
	t.Helper()
	fx := &restoreEffects{
		seedKubectl:        func(ctx context.Context, args ...string) (*exec.Cmd, error) { return f.cmd(ctx, args...), nil },
		kubectlCallTimeout: defaultKubectlCallTimeout,
		healthzWindow:      defaultHealthzWindow,
		rolloutWait:        defaultRolloutWait,
	}
	return fx
}

// seedEffects wires the production kubectl chain onto fx: the seed's own
// apiserver over k8sutil.KubectlContext, at the production windows, so a test
// runs the real kubectl binary — vipKubectl's fake script in BinDir — rather
// than a stub.
func seedEffects(fx *restoreEffects) {
	fx.baseKubectl = k8sutil.KubectlContext
	fx.kubectlCallTimeout = defaultKubectlCallTimeout
	fx.healthzWindow = defaultHealthzWindow
	fx.rolloutWait = defaultRolloutWait
	fx.seedKubectl = fx.localKubectl
}

// realSeedGate replaces a harness's health-gate stub with the production gate
// over the same effects value: what Restore itself builds.
func realSeedGate(fx *restoreEffects) {
	fx.restoreWaitHealthz = func() error { return fx.waitSeedHealthz(fx.healthzWindow, healthzPollInterval) }
}

// workload is restartClusterWorkloads' result for one entry, by name.
func workload(t *testing.T, fx *restoreEffects, name string) workloadResult {
	t.Helper()
	for _, r := range fx.restartClusterWorkloads() {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no workload %q", name)
	return workloadResult{}
}

// restartCilium is Cilium's entry in the result, for the tests of its behavior.
func restartCilium(t *testing.T, fx *restoreEffects) (present bool, warn, err error) {
	t.Helper()
	r := workload(t, fx, "cilium agent + operator")
	return r.present, r.warn, r.err
}

func TestRestartClusterWorkloadsRestartsAllAndWaitsOnAll(t *testing.T) {
	f := &fakeKubectl{}
	fx := withFakeKubectl(t, f)
	for _, r := range fx.restartClusterWorkloads() {
		if !r.present || r.warn != nil || r.err != nil {
			t.Errorf("%s: present=%v warn=%v err=%v", r.name, r.present, r.warn, r.err)
		}
	}
	want := []string{"get ds/cilium", "get deploy/coredns", "get deploy/local-path-provisioner", "get deploy/stratum-dashboard",
		"restart ds/cilium", "restart deploy/cilium-operator", "restart deploy/coredns", "restart deploy/local-path-provisioner", "restart deploy/stratum-dashboard",
		"status ds/cilium", "status deploy/cilium-operator", "status deploy/coredns", "status deploy/local-path-provisioner", "status deploy/stratum-dashboard"}
	if !slices.Equal(f.got(), sorted(want...)) {
		t.Errorf("calls = %q, want %q", f.got(), sorted(want...))
	}
}

// TestRestartCiliumRestartsTheOperatorWhenTheDaemonSetFails: a failed
// restart of one target must not skip the other, and only a target that was
// restarted has a rollout to wait on.
func TestRestartCiliumRestartsTheOperatorWhenTheDaemonSetFails(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"restart ds/cilium": 1}}
	fx := withFakeKubectl(t, f)
	present, warn, err := restartCilium(t, fx)
	if !present || warn != nil {
		t.Fatalf("present=%v warn=%v", present, warn)
	}
	if err == nil || !strings.Contains(err.Error(), "restart ds/cilium") || strings.Contains(err.Error(), "operator") {
		t.Fatalf("err = %v, want only the daemonset's restart failure", err)
	}
	for _, want := range []string{"restart deploy/cilium-operator", "status deploy/cilium-operator"} {
		if !slices.Contains(f.got(), want) {
			t.Errorf("calls = %q, missing %q", f.got(), want)
		}
	}
	if slices.Contains(f.got(), "status ds/cilium") {
		t.Errorf("calls = %q; waited on a rollout that never started", f.got())
	}
}

func TestRestartCiliumJoinsBothRestartFailures(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"restart ds/cilium": 1, "restart deploy/cilium-operator": 1}}
	fx := withFakeKubectl(t, f)
	_, _, err := restartCilium(t, fx)
	if err == nil || !strings.Contains(err.Error(), "restart ds/cilium") || !strings.Contains(err.Error(), "restart deploy/cilium-operator") {
		t.Fatalf("err = %v, want both failures", err)
	}
	for _, c := range f.got() {
		if c == "status ds/cilium" || c == "status deploy/cilium-operator" {
			t.Errorf("calls = %q; a Cilium rollout was waited on though nothing was restarted", f.got())
		}
	}
}

// TestCiliumRolloutTimeoutIsAWarning: the targets were restarted and may still
// finish, so a rollout that does not is not a failure to restart, and each
// rollout is still checked.
func TestCiliumRolloutTimeoutIsAWarning(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"status ds/cilium": 1}}
	fx := withFakeKubectl(t, f)
	present, warn, err := restartCilium(t, fx)
	if !present || err != nil {
		t.Fatalf("present=%v err=%v", present, err)
	}
	if warn == nil || !strings.Contains(warn.Error(), "ds/cilium did not finish rolling out") {
		t.Fatalf("warn = %v", warn)
	}
	if !slices.Contains(f.got(), "status deploy/cilium-operator") {
		t.Errorf("calls = %q; the operator's rollout was not checked", f.got())
	}
}

func TestRestartCiliumSkipsAClusterWithoutIt(t *testing.T) {
	f := &fakeKubectl{noCilium: true}
	fx := withFakeKubectl(t, f)
	present, warn, err := restartCilium(t, fx)
	if present || warn != nil || err != nil {
		t.Errorf("present=%v warn=%v err=%v", present, warn, err)
	}
	for _, c := range f.got() {
		if strings.HasSuffix(c, "cilium") || strings.HasSuffix(c, "cilium-operator") {
			if c != "get ds/cilium" {
				t.Errorf("calls = %q; something was done to a Cilium that is not there", f.got())
			}
		}
	}
}

// TestCoreDNSIsRestartedAndNamedWhenItFails: CoreDNS
// watches Services and EndpointSlices, so a restore that leaves it on its
// pre-restore watch answers headless-Service names from post-snapshot endpoints.
func TestCoreDNSIsRestartedAndNamedWhenItFails(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"restart deploy/coredns": 1}}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "coredns")
	if !r.present || r.err == nil || !strings.Contains(r.err.Error(), "restart deploy/coredns") {
		t.Fatalf("present=%v err=%v", r.present, r.err)
	}
	if c, _, cerr := restartCilium(t, fx); cerr != nil {
		t.Errorf("Cilium failed because CoreDNS did: %v (%v)", cerr, c)
	}
}

func TestCoreDNSRolloutTimeoutIsAWarning(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"status deploy/coredns": 1}}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "coredns")
	if r.err != nil || r.warn == nil || !strings.Contains(r.warn.Error(), "deploy/coredns did not finish rolling out") {
		t.Fatalf("warn=%v err=%v", r.warn, r.err)
	}
}

func TestCoreDNSSkippedWhenTheClusterHasNone(t *testing.T) {
	f := &fakeKubectl{noCoreDNS: true}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "coredns")
	if r.present || r.warn != nil || r.err != nil {
		t.Errorf("present=%v warn=%v err=%v", r.present, r.warn, r.err)
	}
	for _, c := range f.got() {
		if strings.Contains(c, "coredns") && c != "get deploy/coredns" {
			t.Errorf("calls = %q; something was done to a CoreDNS that is not there", f.got())
		}
	}
}

// The local-path provisioner watches PersistentVolumeClaims, so a restored claim
// stays unprovisioned until it restarts. It lives in
// its own namespace and is there only when the default storage is local-path.
func TestLocalPathProvisionerIsRestartedInItsOwnNamespace(t *testing.T) {
	f := &fakeKubectl{}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "local-path provisioner")
	if !r.present || r.err != nil || r.warn != nil {
		t.Fatalf("present=%v warn=%v err=%v", r.present, r.warn, r.err)
	}
	for _, key := range []string{"get deploy/local-path-provisioner", "restart deploy/local-path-provisioner", "status deploy/local-path-provisioner"} {
		if !strings.Contains(f.args[key], "-n local-path-storage ") {
			t.Errorf("%s: args = %q, want -n local-path-storage", key, f.args[key])
		}
	}
}

func TestLocalPathProvisionerFailureIsNamedAndIsolated(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"restart deploy/local-path-provisioner": 1}}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "local-path provisioner")
	if !r.present || r.err == nil || !strings.Contains(r.err.Error(), "restart deploy/local-path-provisioner") {
		t.Fatalf("present=%v err=%v", r.present, r.err)
	}
	if _, _, cerr := restartCilium(t, fx); cerr != nil {
		t.Errorf("Cilium failed because local-path did: %v", cerr)
	}
}

func TestLocalPathProvisionerRolloutTimeoutIsAWarning(t *testing.T) {
	f := &fakeKubectl{failures: map[string]int{"status deploy/local-path-provisioner": 1}}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "local-path provisioner")
	if r.err != nil || r.warn == nil || !strings.Contains(r.warn.Error(), "deploy/local-path-provisioner did not finish rolling out") {
		t.Fatalf("warn=%v err=%v", r.warn, r.err)
	}
}

// A cluster whose default storage is not local-path has no provisioner, which is
// not an error.
func TestLocalPathProvisionerSkippedWhenTheClusterHasNone(t *testing.T) {
	f := &fakeKubectl{noLocalPath: true}
	fx := withFakeKubectl(t, f)
	r := workload(t, fx, "local-path provisioner")
	if r.present || r.warn != nil || r.err != nil {
		t.Errorf("present=%v warn=%v err=%v", r.present, r.warn, r.err)
	}
	for _, c := range f.got() {
		if strings.Contains(c, "local-path") && c != "get deploy/local-path-provisioner" {
			t.Errorf("calls = %q; something was done to a provisioner that is not there", f.got())
		}
	}
}

// TestRolloutRestartCommandsNameEverythingTheRestoreRolls: the guidance that
// tells an operator to restart the workloads by hand, or to repeat the restore's
// restarts, is built from the same list the restore uses, one command per
// namespace.
func TestRolloutRestartCommandsNameEverythingTheRestoreRolls(t *testing.T) {
	want := []string{
		"kubectl -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns",
		"kubectl -n local-path-storage rollout restart deploy/local-path-provisioner",
		"kubectl -n stratum-dashboard rollout restart deploy/stratum-dashboard",
	}
	if got := RolloutRestartCommands("kubectl"); !slices.Equal(got, want) {
		t.Errorf("RolloutRestartCommands = %q, want %q", got, want)
	}
	for _, c := range SeedRolloutRestartCommands() {
		if !strings.Contains(c, "--server https://127.0.0.1:6443 -n ") {
			t.Errorf("%q does not name the seed", c)
		}
	}
}

// TestCiliumKubectlTargetsTheLocalApiserver: admin.conf may name the
// VIP, which before the other control planes rejoin can lead to a member that
// still holds the old datastore.
func TestCiliumKubectlTargetsTheLocalApiserver(t *testing.T) {
	fx := &restoreEffects{baseKubectl: k8sutil.KubectlContext}
	cmd, err := fx.localKubectl(context.Background(), "-n", "kube-system", "get", "ds/cilium")
	if err != nil {
		t.Fatal(err)
	}
	args := cmd.Args[1:]
	if len(args) < 2 || args[0] != "--server" || args[1] != "https://127.0.0.1:6443" {
		t.Errorf("args = %q, want --server https://127.0.0.1:6443 first", args)
	}
	if !slices.Contains(args, "get") {
		t.Errorf("args = %q lost the caller's arguments", args)
	}
}

// workloads stubs a run's workload restarts with one result per entry.
func workloads(t *testing.T, fx *restoreEffects, rs ...workloadResult) {
	t.Helper()
	fx.restoreWorkloads = func() []workloadResult { return rs }
}

// TestRolloutWarningDoesNotFailTheRestore: the recovery steps follow a restore
// that only has a slow rollout.
func TestRolloutWarningDoesNotFailTheRestore(t *testing.T) {
	n := newFakeNode(t)
	workloads(t, n.fx, workloadResult{name: "cilium agent + operator", present: true, warn: errors.New("ds/cilium did not finish rolling out")})
	if err := n.restoreErr(); err != nil {
		t.Fatalf("a slow rollout failed the restore: %v", err)
	}
}

// TestWorkloadRestartFailureIsNamed: a workload that could not be restarted at
// all is reported by name, with the data still restored and the rest restarted.
func TestWorkloadRestartFailureIsNamed(t *testing.T) {
	names := []string{"cilium agent + operator", "coredns", "local-path provisioner"}
	for _, name := range names {
		n := newFakeNode(t)
		workloads(t, n.fx,
			workloadResult{name: "cilium agent + operator", present: true},
			workloadResult{name: "coredns", present: true},
			workloadResult{name: "local-path provisioner", present: true})
		rs := n.fx.restoreWorkloads()
		for i := range rs {
			if rs[i].name == name {
				rs[i].err = errors.New("restart: boom")
			}
		}
		workloads(t, n.fx, rs...)
		err := n.restoreErr()
		if !errors.Is(err, ErrWatchersNotRestarted) || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: err = %v", name, err)
		}
		for _, other := range names {
			if other != name && strings.Contains(err.Error(), other) {
				t.Errorf("%s: err = %v names %s, which worked", name, err, other)
			}
		}
		if !n.called("restart:kube-scheduler") || !dataDirHolds(t, n.fx.etcdDataDir, "RESTORED") {
			t.Errorf("%s: events = %q", name, n.events)
		}
	}
}

// vipKubectl installs a kubectl that answers "ok" only for an apiserver on
// 127.0.0.1, working out the server as the real one does: --server wins over
// the kubeconfig. admin.conf names a VIP, as after `stratum ha-repoint`.
func vipKubectl(t *testing.T) {
	t.Helper()
	root := version.BinDir
	swap(t, &version.AdminConf, filepath.Join(t.TempDir(), "admin.conf"))
	if err := os.WriteFile(version.AdminConf, []byte("clusters:\n- cluster:\n    server: https://10.0.0.100:6443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
server=$(sed -n 's/^ *server: *//p' "$KUBECONFIG")
while [ $# -gt 0 ]; do [ "$1" = --server ] && server=$2; shift; done
case "$server" in https://127.0.0.1:*) echo ok ;; *) echo "answered by $server" >&2; exit 1 ;; esac
`
	if err := os.WriteFile(filepath.Join(root, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestHealthGateChecksTheSeedNotTheVIP: with admin.conf on a VIP that
// is down, or that an old control plane answers, the gate must still be about
// the seed's own apiserver.
func TestHealthGateChecksTheSeedNotTheVIP(t *testing.T) {
	swap(t, &version.BinDir, t.TempDir())
	vipKubectl(t)
	fx := &restoreEffects{}
	seedEffects(fx)
	if err := fx.waitSeedHealthz(5*time.Second, 0); err != nil {
		t.Fatalf("a healthy seed failed the gate because admin.conf names the VIP: %v", err)
	}
	// The same fake, asked the way k8sutil.WaitHealthz asks, is answered by the VIP.
	if err := k8sutil.Kubectl("get", "--raw", "/healthz").Run(); err == nil {
		t.Fatal("the fake did not model a VIP admin.conf")
	}
}

// TestHealthGateUsedByRestoreAndRollbackIsTheSeedOne: the restore's gate, and
// the rollback's recovery after a failed restore, call restoreWaitHealthz, whose
// default must be the seed gate rather than k8sutil.WaitHealthz.
func TestHealthGateUsedByRestoreAndRollbackIsTheSeedOne(t *testing.T) {
	seedGate := func(fx *restoreEffects) { seedEffects(fx); realSeedGate(fx) }
	n := newFakeNode(t)
	swap(t, &version.BinDir, filepath.Dir(n.fx.etcdutlPath))
	vipKubectl(t) // in the fake node's BinDir, beside the etcdutl Restore looks for
	seedGate(n.fx)
	// Restore's own, and the rollback's: a restore that fails after etcd is up.
	if err := n.restoreErr(); err != nil {
		t.Fatalf("restore failed its health gate against a healthy seed: %v", err)
	}
	n2 := newFakeNode(t)
	swap(t, &version.BinDir, filepath.Dir(n2.fx.etcdutlPath))
	vipKubectl(t)
	seedGate(n2.fx)
	n2.fail("run", 0)
	if err := n2.restoreErr(); err == nil || strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("rollback's recovery gate failed against a healthy seed: %v", err)
	}
}

// TestRolloutsShareOneBudget: live, a Cilium rollout may not finish
// while a worker's kubelet is stale, and two sequential 300 s waits held the
// restore for about 13 minutes before it printed what the operator must do. All
// the rollouts, CoreDNS's and local-path's included, are waited on together, each for the one
// budget.
//
// A barrier proves the waits overlap without timing them: each status call
// blocks as it begins until all have begun. Waited on one after the other, the
// first would wait for another that cannot begin, and the barrier times out.
func TestRolloutsShareOneBudget(t *testing.T) {
	if defaultRolloutWait != 60*time.Second {
		t.Errorf("the shared budget is %v, want 60s", defaultRolloutWait)
	}
	const waits = 5 // ds/cilium, deploy/cilium-operator, deploy/coredns, deploy/local-path-provisioner, deploy/stratum-dashboard
	var arrived sync.WaitGroup
	arrived.Add(waits)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	var overlapped atomic.Int32
	f := &fakeKubectl{onStatus: func() {
		arrived.Done()
		select {
		case <-all:
			overlapped.Add(1)
		case <-time.After(5 * time.Second): // far past any scheduling delay
		}
	}}
	fx := withFakeKubectl(t, f)
	for _, r := range fx.restartClusterWorkloads() {
		if r.warn != nil || r.err != nil {
			t.Fatalf("%s: warn=%v err=%v", r.name, r.warn, r.err)
		}
	}
	if n := overlapped.Load(); n != waits {
		t.Errorf("%d of %d rollout waits saw the others begin: they were waited on one after the other", n, waits)
	}
	for _, key := range []string{"status ds/cilium", "status deploy/cilium-operator", "status deploy/coredns", "status deploy/local-path-provisioner", "status deploy/stratum-dashboard"} {
		if !strings.Contains(f.args[key], "--timeout=60s") {
			t.Errorf("%s: args = %q, want --timeout=60s", key, f.args[key])
		}
		if strings.Contains(f.args[key], "300s") {
			t.Errorf("%s: a 300 s wait is still in use", key)
		}
	}
}

// A kubectl that never answers (a stalled apiserver request blocks before
// `--timeout` applies) must cost its deadline and nothing more: the call is
// killed, named, and the rest of the recovery goes on.
// Each test fails by not returning, so it is bounded by a generous limit.
func within(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s: still blocked after %s; a hung kubectl is not bounded", what, limit)
	}
}

func TestHungDiscoveryIsNamedAndBounded(t *testing.T) {
	f := &fakeKubectl{hang: map[string]bool{"get deploy/coredns": true}}
	fx := withFakeKubectl(t, f)
	fx.kubectlCallTimeout = 300 * time.Millisecond
	var rs []workloadResult
	within(t, 20*time.Second, "discovery", func() { rs = fx.restartClusterWorkloads() })
	for _, r := range rs {
		switch r.name {
		case "coredns":
			if !r.present || r.err == nil || !strings.Contains(r.err.Error(), "look for deploy/coredns") ||
				!strings.Contains(r.err.Error(), "no answer within") {
				t.Errorf("coredns: present=%v err=%v", r.present, r.err)
			}
		default:
			if r.err != nil || r.warn != nil {
				t.Errorf("%s was affected by coredns's hung discovery: warn=%v err=%v", r.name, r.warn, r.err)
			}
		}
	}
}

func TestHungRestartIsNamedAndTheOthersProceed(t *testing.T) {
	f := &fakeKubectl{hang: map[string]bool{"restart ds/cilium": true}}
	fx := withFakeKubectl(t, f)
	fx.kubectlCallTimeout = 300 * time.Millisecond
	var rs []workloadResult
	within(t, 20*time.Second, "restart", func() { rs = fx.restartClusterWorkloads() })
	for _, r := range rs {
		if r.name == "cilium agent + operator" {
			if r.err == nil || !strings.Contains(r.err.Error(), "restart ds/cilium") || strings.Contains(r.err.Error(), "operator:") {
				t.Errorf("cilium: err = %v, want only the daemonset's restart to have failed", r.err)
			}
			continue
		}
		if r.err != nil {
			t.Errorf("%s: err = %v", r.name, r.err)
		}
	}
	if !slices.Contains(f.got(), "restart deploy/cilium-operator") || !slices.Contains(f.got(), "status deploy/local-path-provisioner") {
		t.Errorf("calls = %q; the hung restart stopped the rest", f.got())
	}
}

// A rollout whose status call never answers is killed at the shared deadline and
// reported as that rollout's warning; the others are not held up by it.
func TestHungRolloutStatusIsAWarningWithinTheBudget(t *testing.T) {
	f := &fakeKubectl{hang: map[string]bool{"status deploy/coredns": true}}
	fx := withFakeKubectl(t, f)
	fx.rolloutWait = 400 * time.Millisecond
	var rs []workloadResult
	within(t, 20*time.Second, "rollout status", func() { rs = fx.restartClusterWorkloads() })
	for _, r := range rs {
		if r.err != nil {
			t.Errorf("%s: err = %v; a hung status is not a failed restart", r.name, r.err)
		}
		if r.name == "coredns" {
			if r.warn == nil || !strings.Contains(r.warn.Error(), "deploy/coredns did not finish rolling out in 400ms") {
				t.Errorf("coredns: warn = %v", r.warn)
			}
		} else if r.warn != nil {
			t.Errorf("%s: warn = %v; it was held up by coredns's hung status", r.name, r.warn)
		}
	}
}

func TestHungHealthzFailsTheGateInsteadOfBlocking(t *testing.T) {
	f := &fakeKubectl{hang: map[string]bool{"get --raw": true}}
	fx := withFakeKubectl(t, f)
	fx.kubectlCallTimeout = 100 * time.Millisecond
	var err error
	within(t, 20*time.Second, "health gate", func() { err = fx.waitSeedHealthz(600*time.Millisecond, 20*time.Millisecond) })
	if err == nil || !strings.Contains(err.Error(), "did not become healthy within") {
		t.Fatalf("err = %v", err)
	}
}
