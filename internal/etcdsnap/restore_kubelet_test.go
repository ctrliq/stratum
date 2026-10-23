package etcdsnap

import (
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
)

// fakeRoster wires a supervisor roster (or its failure) into fx's comps
// effects and records what was restarted. fx is the run's effects value: the
// caller's harness one, or a bare one for the restartKubelet/restartOperator
// pins.
func fakeRoster(t *testing.T, fx *restoreEffects, roster map[string]supervisor.ProcStatus, err error) *[]string {
	t.Helper()
	var restarted []string
	fx.compsStatus = func() (map[string]supervisor.ProcStatus, error) { return roster, err }
	fx.compsRestart = func(name string) error { restarted = append(restarted, name); return nil }
	return &restarted
}

// runningRoster is a supervisor roster with every comps.Units component
// running, as a control plane's is.
func runningRoster() map[string]supervisor.ProcStatus {
	roster := map[string]supervisor.ProcStatus{}
	for _, name := range comps.Units {
		roster[name] = supervisor.ProcStatus{Name: name, Running: true}
	}
	return roster
}

func TestRestartKubeletRestartsItWhenTheRosterHasIt(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, map[string]supervisor.ProcStatus{"kubelet": {Name: "kubelet", Running: true}}, nil)
	present, err := restartKubelet(fx)
	if !present || err != nil || len(*restarted) != 1 || (*restarted)[0] != "kubelet" {
		t.Errorf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

// Only a roster that was read, and has no kubelet, skips it.
func TestRestartKubeletSkipsWhenTheRosterHasNone(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, map[string]supervisor.ProcStatus{"operator": {Name: "operator"}}, nil)
	present, err := restartKubelet(fx)
	if present || err != nil || len(*restarted) != 0 {
		t.Errorf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

// TestRestartKubeletReportsALookupFailure: a supervisor that cannot be
// asked is not a node without kubelet. Skipping would report a restore that left
// kubelet's stale watch as complete.
func TestRestartKubeletReportsALookupFailure(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, nil, errors.New("dial unix /run/stratum/stratum.sock: connection refused"))
	present, err := restartKubelet(fx)
	if err == nil || !present || len(*restarted) != 0 {
		t.Fatalf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

// TestRestoreNamesKubeletWhenTheSupervisorCannotBeAsked: through Restore, with
// kubelet's restart dispatched from the set's howSupervisor entry, the failure
// is named and the other restarts still run.
func TestRestoreNamesKubeletWhenTheSupervisorCannotBeAsked(t *testing.T) {
	n := newFakeNode(t)
	fakeRoster(t, n.fx, nil, errors.New("connection refused"))
	err := n.restoreErr()
	if !errors.Is(err, ErrWatchersNotRestarted) || !strings.Contains(err.Error(), "kubelet") {
		t.Fatalf("err = %v, want kubelet named as not restarted", err)
	}
	if !n.called("restart:kube-scheduler") || !n.called("restart:cilium") {
		t.Errorf("events = %q; the lookup failure stopped the other restarts", n.events)
	}
}

// The stratum operator polls rather than watching, but it loads the shared
// requester-signing key and registers its webhook endpoint at start and works
// under a leader lease, so it is restarted too, but only where the supervisor
// runs it (control planes).
func TestRestartOperatorRestartsItWhenTheRosterHasIt(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, map[string]supervisor.ProcStatus{"operator": {Name: "operator", Running: true}}, nil)
	present, err := restartOperator(fx)
	if !present || err != nil || len(*restarted) != 1 || (*restarted)[0] != "operator" {
		t.Errorf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

func TestRestartOperatorSkipsWhenTheRosterHasNone(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, map[string]supervisor.ProcStatus{"kubelet": {Name: "kubelet"}}, nil)
	present, err := restartOperator(fx)
	if present || err != nil || len(*restarted) != 0 {
		t.Errorf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

func TestRestartOperatorReportsALookupFailure(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, nil, errors.New("connection refused"))
	present, err := restartOperator(fx)
	if err == nil || !present || len(*restarted) != 0 {
		t.Fatalf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

// TestRestoreNamesTheOperatorWhenItCannotBeRestarted: through Restore, with
// the operator's restart dispatched from the set's howSupervisor entry, the
// failure is named and everything else still runs.
func TestRestoreNamesTheOperatorWhenItCannotBeRestarted(t *testing.T) {
	n := newFakeNode(t)
	fakeRoster(t, n.fx, nil, errors.New("connection refused"))
	err := n.restoreErr()
	if !errors.Is(err, ErrWatchersNotRestarted) || !strings.Contains(err.Error(), "stratum operator") {
		t.Fatalf("err = %v, want the stratum operator named", err)
	}
	if !n.called("restart:cilium") || !n.called("restart:kube-vip") {
		t.Errorf("events = %q; the lookup failure stopped the other restarts", n.events)
	}
}

// kube-vip runs as a static pod on a control plane with a VIP, and is skipped
// when its manifest is absent. A failed restart is named and does not stop the
// rest — the supervisor's watchers go through the roster restarts, the
// workloads still roll.
func TestKubeVipIsRestartedFromItsManifestAndNamedWhenItFails(t *testing.T) {
	n := newFakeNode(t)
	restarted := fakeRoster(t, n.fx, runningRoster(), nil)
	n.fail("restart:kube-vip", 0)
	err := n.restoreErr()
	if !errors.Is(err, ErrWatchersNotRestarted) || !strings.Contains(err.Error(), "kube-vip") ||
		strings.Contains(err.Error(), "kube-scheduler") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(*restarted, "operator") || !n.called("restart:cilium") {
		t.Errorf("roster restarts %q, events = %q; kube-vip's failure stopped the rest", *restarted, n.events)
	}
}

func TestKubeVipSkippedWithoutAVIP(t *testing.T) {
	n := newFakeNode(t)
	fakeRoster(t, n.fx, runningRoster(), nil)
	n.fx.restoreStaticPodExists = func(name string) bool { return name != "kube-vip" }
	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	if n.called("restart:kube-vip") || !n.called("restart:kube-scheduler") {
		t.Errorf("events = %q", n.events)
	}
}

// TestRestoredStaticPodsAreEveryStaticPodButEtcdAndTheAPIServer pins the
// restore's static-pod restart set as a relation to the roster owner: the
// full candidate roster staticpod.Names renders, so a static pod added to the
// roster joins the restore's restart set without a second list to forget, and
// kube-vip stays a candidate even where this node renders none — the restore
// skips a component with no manifest and says so, it never drops it from the
// set the account and the guidance are rendered from. etcd and the apiserver
// are excluded because the restore itself restarts them.
func TestRestoredStaticPodsAreEveryStaticPodButEtcdAndTheAPIServer(t *testing.T) {
	var want []string
	for _, n := range staticpod.Names {
		if n != "etcd" && n != "kube-apiserver" {
			want = append(want, n)
		}
	}
	if got := restoredStaticPods(); !slices.Equal(got, want) {
		t.Errorf("restoredStaticPods() = %q, want every static pod in the roster but etcd and kube-apiserver: %q", got, want)
	}
}

// TestRestartWatchersExecutesFromTheSet: restartWatchers executes the restore's
// restart set, dispatching each entry by its How — a supervised watcher through
// restartSupervised by roster name, a static pod through the manifest gate, the
// workloads through restoreWorkloads — so what it restarts, and in what order,
// is the set's, never a hand-kept parallel sequence. A supervised unit added to
// comps.Units, with its command-table entry, is executed with the rest: it
// needs no restartWatchers edit of its own. The workloads are rolled once, not
// once per rollout entry; the run consults nothing hand-named outside the set,
// so the effects value carries no restoreKubelet/restoreOperator stub here.
func TestRestartWatchersExecutesFromTheSet(t *testing.T) {
	swap(t, &comps.Units, append(slices.Clone(comps.Units), "synthetic-agent"))
	commands := maps.Clone(supervisedWatcherCommands)
	commands["synthetic-agent"] = "sudo pkill -x synthetic-agent"
	swap(t, &supervisedWatcherCommands, commands)

	fx := &restoreEffects{}
	var done []string
	fx.compsStatus = func() (map[string]supervisor.ProcStatus, error) { return runningRoster(), nil }
	fx.compsRestart = func(name string) error { done = append(done, "supervisor:"+name); return nil }
	fx.restoreStaticPodExists = func(string) bool { return true }
	fx.restoreStaticPod = func(name string) error { done = append(done, "manifest:"+name); return nil }
	fx.restoreWorkloads = func() []workloadResult {
		done = append(done, "workloads")
		return []workloadResult{{name: "coredns", present: true}}
	}
	if err := fx.restartWatchers(io.Discard); err != nil {
		t.Fatal(err)
	}

	// The set's order, dispatched by How: kubelet first, since the rest are
	// pods it has to recreate; then the static pods; then the remaining
	// supervised watchers in roster order; then the workloads, once.
	want := []string{"supervisor:kubelet"}
	for _, pod := range restoredStaticPods() {
		want = append(want, "manifest:"+pod)
	}
	for _, name := range comps.Units {
		if name == "containerd" || name == "kubelet" {
			continue
		}
		want = append(want, "supervisor:"+name)
	}
	want = append(want, "workloads")
	if !slices.Equal(done, want) {
		t.Errorf("restartWatchers executed %q, want the set dispatched by How: %q", done, want)
	}
}

// TestRestoreAccountsForKubeVipOnANoVipControlPlane pins the restore's honesty
// contract on the one roster member a node may legitimately never run: a
// control plane whose node state carries no VIP renders no kube-vip manifest,
// so the manifest gate skips it — and the restore's own account must still
// name that skip ("not on this node, skipped") instead of saying nothing while
// the printed repeat commands tell the operator to restart it.
func TestRestoreAccountsForKubeVipOnANoVipControlPlane(t *testing.T) {
	n := newFakeNode(t)
	fakeRoster(t, n.fx, runningRoster(), nil)
	n.fx.restoreStaticPodExists = func(name string) bool { return name != "kube-vip" }

	if err := n.restoreErr(); err != nil {
		t.Fatal(err)
	}
	out := n.out.String()

	if !strings.Contains(out, "kube-vip: not on this node, skipped") {
		t.Errorf("the restore's account of a no-VIP control plane never mentions kube-vip:\n%s\nwant the skip named, since the repeat commands tell the operator to restart it", out)
	}
	if n.called("restart:kube-vip") {
		t.Error("kube-vip was restarted on a node whose state carries no VIP and has no manifest")
	}
	if !n.called("restart:kube-scheduler") {
		t.Errorf("events = %q; the kube-vip accounting stopped the other restarts", n.events)
	}
}
