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

// fakeRoster wires a supervisor roster (or its failure) into fx and records what was restarted.
func fakeRoster(t *testing.T, fx *restoreEffects, roster map[string]supervisor.ProcStatus, err error) *[]string {
	t.Helper()
	var restarted []string
	fx.compsStatus = func() (map[string]supervisor.ProcStatus, error) { return roster, err }
	fx.compsRestart = func(name string) error { restarted = append(restarted, name); return nil }
	return &restarted
}

// runningRoster has every comps.Units component running, as a control plane's is.
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

func TestRestartKubeletSkipsWhenTheRosterHasNone(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, map[string]supervisor.ProcStatus{"operator": {Name: "operator"}}, nil)
	present, err := restartKubelet(fx)
	if present || err != nil || len(*restarted) != 0 {
		t.Errorf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

func TestRestartKubeletReportsALookupFailure(t *testing.T) {
	fx := &restoreEffects{}
	restarted := fakeRoster(t, fx, nil, errors.New("dial unix /run/stratum/stratum.sock: connection refused"))
	present, err := restartKubelet(fx)
	if err == nil || !present || len(*restarted) != 0 {
		t.Fatalf("present=%v err=%v restarted=%q", present, err, *restarted)
	}
}

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
