package certs

import (
	"bytes"
	"io"

	"context"
	"errors"
	"github.com/ctrliq/stratum/internal/maintlease"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

func TestRestartControlPlaneReportsBestEffortFailure(t *testing.T) {
	oldConf, oldSocket := version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() { version.K8sConfDir, version.ControlSocket = oldConf, oldSocket })
	version.K8sConfDir = t.TempDir()
	version.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staticpod.Path("kube-vip"), []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	onStdout := captureRealStdout(t, func() {
		err := restartControlPlaneGuarded(&progress, func(string) error { return errors.New("VIP restart refused") }, func(string) error { return nil }, nil, false)
		if err != nil {
			t.Fatalf("best-effort restart failed the rotation: %v", err)
		}
	})
	if onStdout != "" {
		t.Fatalf("restart warning reached stdout: %q", onStdout)
	}
	if !strings.Contains(progress.String(), "WARN: restart kube-vip: VIP restart refused") {
		t.Fatalf("the successful rotation lost its best-effort restart warning: %q", progress.String())
	}
}

func TestRestartControlPlaneFailures(t *testing.T) {
	oldConf, oldSocket := version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() { version.K8sConfDir, version.ControlSocket = oldConf, oldSocket })
	version.K8sConfDir = t.TempDir()
	// Ensure host-process probes never reach a running supervisor.
	version.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kube-controller-manager", "kube-scheduler", "kube-vip"} {
		if err := os.WriteFile(staticpod.Path(name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmErr, schedulerErr := errors.New("controller failure"), errors.New("scheduler failure")
	for _, tc := range []struct {
		name          string
		restart, wait map[string]error
		want          []error
		wantNames     []string
	}{
		{name: "healthy"},
		{name: "controller restart", restart: map[string]error{"kube-controller-manager": cmErr}, want: []error{cmErr}, wantNames: []string{"kube-controller-manager"}},
		{name: "scheduler restart", restart: map[string]error{"kube-scheduler": schedulerErr}, want: []error{schedulerErr}, wantNames: []string{"kube-scheduler"}},
		{name: "controller readiness", wait: map[string]error{"kube-controller-manager": cmErr}, want: []error{cmErr}, wantNames: []string{"kube-controller-manager"}},
		{name: "scheduler readiness", wait: map[string]error{"kube-scheduler": schedulerErr}, want: []error{schedulerErr}, wantNames: []string{"kube-scheduler"}},
		{name: "both failures", restart: map[string]error{"kube-controller-manager": cmErr}, wait: map[string]error{"kube-scheduler": schedulerErr}, want: []error{cmErr, schedulerErr}, wantNames: []string{"kube-controller-manager", "kube-scheduler"}},
		{name: "vip remains best effort", restart: map[string]error{"kube-vip": errors.New("vip failure")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var restarted, waited []string
			err := restartControlPlaneGuarded(io.Discard, func(name string) error {
				restarted = append(restarted, name)
				return tc.restart[name]
			}, func(name string) error {
				waited = append(waited, name)
				return tc.wait[name]
			}, nil, false)
			if len(tc.want) == 0 && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for i, want := range tc.want {
				if !errors.Is(err, want) || !strings.Contains(err.Error(), tc.wantNames[i]) {
					t.Errorf("error = %v, want %s failure wrapping %v", err, tc.wantNames[i], want)
				}
			}
			if want := []string{"kube-controller-manager", "kube-scheduler", "kube-vip"}; !reflect.DeepEqual(restarted, want) {
				t.Errorf("restarted = %v, want %v (continue after failures; skip absent pods)", restarted, want)
			}
			var wantWait []string
			for _, name := range []string{"kube-controller-manager", "kube-scheduler"} {
				if tc.restart[name] == nil {
					wantWait = append(wantWait, name)
				}
			}
			if !reflect.DeepEqual(waited, wantWait) {
				t.Errorf("waited = %v, want %v", waited, wantWait)
			}
		})
	}
}

// A failed etcd or apiserver gate must not end the sequence, and the
// failure must still reach the caller.
func TestRestartControlPlaneCarriesOnAfterGateFailure(t *testing.T) {
	oldConf, oldSocket, oldHealthz := version.K8sConfDir, version.ControlSocket, waitHealthz
	t.Cleanup(func() { version.K8sConfDir, version.ControlSocket, waitHealthz = oldConf, oldSocket, oldHealthz })
	version.K8sConfDir = t.TempDir()
	version.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	all := []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-vip"}
	for _, name := range all {
		if err := os.WriteFile(staticpod.Path(name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	etcdErr, apiErr, healthzErr := errors.New("etcd not serving"), errors.New("apiserver not serving"), errors.New("healthz down")
	for _, tc := range []struct {
		name    string
		wait    map[string]error
		healthz error
		want    []error
		healthy bool
	}{
		{name: "healthy", healthz: nil, healthy: true},
		{name: "etcd gate", wait: map[string]error{"etcd": etcdErr}, want: []error{etcdErr}},
		{name: "apiserver gate", wait: map[string]error{"kube-apiserver": apiErr}, want: []error{apiErr}},
		{name: "apiserver healthz", healthz: healthzErr, want: []error{healthzErr}},
		{name: "etcd and apiserver gates", wait: map[string]error{"etcd": etcdErr, "kube-apiserver": apiErr}, want: []error{etcdErr, apiErr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var restarted, healthzCalls = []string{}, 0
			waitHealthz = func(int) error { healthzCalls++; return tc.healthz }
			err := restartControlPlaneGuarded(io.Discard, func(name string) error {
				restarted = append(restarted, name)
				return nil
			}, func(name string) error { return tc.wait[name] }, nil, false)
			if tc.healthy && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want it to wrap %v", err, want)
				}
			}
			if len(tc.want) > 0 && !strings.Contains(err.Error(), "remaining restart was still attempted") {
				t.Errorf("error = %v, want it to say the rest of the sequence ran", err)
			}
			if !reflect.DeepEqual(restarted, all) {
				t.Errorf("restarted = %v, want %v (a failed gate must not skip later restarts)", restarted, all)
			}
			// /healthz only gates an apiserver that came back.
			wantCalls := 1
			if tc.wait["kube-apiserver"] != nil {
				wantCalls = 0
			}
			if healthzCalls != wantCalls {
				t.Errorf("healthz calls = %d, want %d", healthzCalls, wantCalls)
			}
		})
	}
}

// A failed restart of etcd or kube-apiserver (not just a failed
// gate) must not end the sequence either. Here etcd restarts but never comes back,
// then kube-apiserver's restart itself fails: both errors are reported, the failed
// restart is not gated, and every later pod is still restarted.
func TestRestartControlPlaneCarriesOnAfterAFailedRestart(t *testing.T) {
	oldConf, oldSocket, oldHealthz := version.K8sConfDir, version.ControlSocket, waitHealthz
	t.Cleanup(func() { version.K8sConfDir, version.ControlSocket, waitHealthz = oldConf, oldSocket, oldHealthz })
	version.K8sConfDir = t.TempDir()
	version.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	all := []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-vip"}
	for _, name := range all {
		if err := os.WriteFile(staticpod.Path(name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	etcdGate, apiRestart := errors.New("etcd not serving"), errors.New("manifest write failed")
	healthzCalls := 0
	waitHealthz = func(int) error { healthzCalls++; return nil }

	var restarted, waited []string
	err := restartControlPlaneGuarded(io.Discard, func(name string) error {
		restarted = append(restarted, name)
		if name == "kube-apiserver" {
			return apiRestart
		}
		return nil
	}, func(name string) error {
		waited = append(waited, name)
		if name == "etcd" {
			return etcdGate
		}
		return nil
	}, nil, false)

	for _, want := range []error{etcdGate, apiRestart} {
		if !errors.Is(err, want) {
			t.Errorf("error = %v, want it to wrap %v", err, want)
		}
	}
	for _, want := range []string{"wait for etcd after restart", "restart kube-apiserver"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if !reflect.DeepEqual(restarted, all) {
		t.Errorf("restarted = %v, want %v (controller-manager, scheduler and kube-vip must still be restarted)", restarted, all)
	}
	if want := []string{"etcd", "kube-controller-manager", "kube-scheduler"}; !reflect.DeepEqual(waited, want) {
		t.Errorf("waited = %v, want %v (a pod whose restart failed has no gate)", waited, want)
	}
	if healthzCalls != 0 {
		t.Errorf("healthz calls = %d, want 0: the apiserver was never restarted", healthzCalls)
	}
}

func TestServeBudget(t *testing.T) {
	// 163s is the slowest kubelet recreate seen on an idle node; 5 min
	// clears it with room to spare. The others keep the 90s they always had.
	if got := serveBudget("etcd"); got != 5*time.Minute {
		t.Errorf("etcd budget = %v, want 5m0s", got)
	}
	for _, name := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		if got := serveBudget(name); got != 90*time.Second {
			t.Errorf("%s budget = %v, want 90s", name, got)
		}
	}
}

func TestRestartThenReportsFilesWrittenWithoutARecoveryCommand(t *testing.T) {
	cause := errors.New("etcd not serving on :2379 within 5m0s")
	for _, phase := range []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize} {
		err := restartThen(phase, func() error { return cause })
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "files are written on this node") {
			t.Errorf("%s: error = %v, want the cause plus a note that the files are written", phase, err)
		}
		// Which command is safe depends on the other nodes; a node-local phase
		// cannot know, so it must not suggest one.
		if strings.Contains(err.Error(), "rotate-ca") {
			t.Errorf("%s: error = %v, must not name a recovery command", phase, err)
		}
	}
	if err := restartThen(PhaseSignNew, func() error { return nil }); err != nil {
		t.Errorf("success must pass through, got %v", err)
	}
}

// A halt between trust-new and sign-new has one recovery path: sign-new then
// finalize per node, with rollback as the stated alternative, and no advice that
// contradicts it.
func TestHaltedBeforeSignNewGivesOneResumePath(t *testing.T) {
	if haltedBeforeSignNew("restart in-cluster API clients", nil) != nil {
		t.Fatal("nil must stay nil")
	}
	cause := errors.New("2 workload(s) use in-cluster API credentials")
	err := haltedBeforeSignNew("restart in-cluster API clients", cause)
	if !errors.Is(err, cause) {
		t.Error("must keep the cause")
	}
	msg := err.Error()
	for _, want := range []string{
		"restart in-cluster API clients",
		"after trust-new",
		"`stratum certs rotate-ca --phase=sign-new --yes` on each control plane and then on each worker",
		"`kubectl get csr` shows none pending and the local apiserver /healthz responds",
		// Finalize goes through the documented procedure, which scans every node's
		// tokens first; a node's own finalize checks only that node.
		"\"Finishing a rotation by hand\"",
		"checks every node's mounted service-account tokens before the first finalize",
		"`stratum certs rotate-ca --rollback --yes` on each control plane and then on each worker",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q missing %q", msg, want)
		}
	}
	for _, bad := range []string{
		"run this phase again", "do not run a single --phase", "on each node", "on every node",
		// Finalize one node at a time, or retry after a refusal, is what the
		// documented procedure replaces.
		"--phase=finalize --yes", "run it again",
	} {
		if strings.Contains(msg, bad) {
			t.Errorf("%q must not contain %q", msg, bad)
		}
	}
}

// The wiring of the two halts after trust-new: a failed client restart carries the
// resume path, and so does a failed client rebuild, which must not restart them.
func TestRunAfterTrustNewPicksTheHaltPerStep(t *testing.T) {
	ok := func() error { return nil }
	boom := errors.New("boom")
	const resume = "--phase=sign-new --yes"

	if err := runAfterTrustNew(afterTrustNewOps{rebuildClient: ok, restartClients: ok}); err != nil {
		t.Fatalf("success must pass through, got %v", err)
	}

	err := runAfterTrustNew(afterTrustNewOps{rebuildClient: ok, restartClients: func() error { return boom }})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "restart in-cluster API clients") || !strings.Contains(err.Error(), resume) {
		t.Errorf("a restart failure must carry the resume path, got %v", err)
	}

	restarted := false
	err = runAfterTrustNew(afterTrustNewOps{
		rebuildClient:  func() error { return boom },
		restartClients: func() error { restarted = true; return nil },
	})
	if restarted {
		t.Error("restarted the clients after the rebuild failed")
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "rebuild admin client") ||
		!strings.Contains(err.Error(), resume) || !strings.Contains(err.Error(), "--rollback --yes") {
		t.Errorf("a rebuild failure must carry the resume path, got %v", err)
	}
}

// A --cluster run that halts must not point at a node-local phase command: after
// a failed trust-new, sign-new would promote a CA the followers never trusted;
// after a failed sign-new, finalize would drop the old CA before FinalizePreflight.
func TestHaltedAtNamesOnlyClusterSafeNextSteps(t *testing.T) {
	cause := errors.New("initiator sign-new: static pod etcd not serving")
	for _, phase := range []string{PhaseTrustNew, PhaseSignNew} {
		msg := haltedAt(phase, cause).Error()
		for _, want := range []string{"stratum certs check", "every node", "--rollback --yes"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: %q missing %q", phase, msg, want)
			}
		}
		for _, bad := range []string{"--phase=" + PhaseTrustNew, "--phase=" + PhaseSignNew, "--phase=" + PhaseFinalize + " "} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: %q must not suggest %q", phase, msg, bad)
			}
		}
	}
	// Finalize cannot be rolled back on a node that finished it.
	msg := haltedAt(PhaseFinalize, cause).Error()
	if strings.Contains(msg, "--rollback --yes") || !strings.Contains(msg, "--phase=finalize --yes") || !strings.Contains(msg, "stratum certs check") {
		t.Errorf("finalize: %q should offer finish-on-each-node and no rollback", msg)
	}
	if !errors.Is(haltedAt(PhaseSignNew, cause), cause) {
		t.Error("haltedAt must keep the cause")
	}
}

// TestHaltedResumeLineCarriesTheFirstRunsWorkloadFlags: every re-run of
// rotate-ca --cluster passes the workload preflight again, which refuses a
// cluster with API-using workloads unless --restart-workloads or
// --skip-workload-check is given, so a resume line without them names a
// command that refuses on exactly the clusters that needed them first time.
func TestHaltedResumeLineCarriesTheFirstRunsWorkloadFlags(t *testing.T) {
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir() // no forced marker: the unforced texts
	cause := errors.New("node unreachable")
	for name, err := range map[string]error{
		"halted in trust-new":    haltedAt(PhaseTrustNew, cause),
		"halted in sign-new":     haltedAt(PhaseSignNew, cause),
		"halted in finalize":     haltedAt(PhaseFinalize, cause),
		"halted after trust-new": haltedBeforeSignNew("restart in-cluster API clients", cause),
	} {
		msg := err.Error()
		if !strings.Contains(msg, "rotate-ca --cluster --yes") || !strings.Contains(msg, "--restart-workloads or --skip-workload-check the first run had") {
			t.Errorf("%s: %q, want the resume with the first run's workload flags", name, msg)
		}
	}
}

// A rotation started with --force must not send anyone to --rollback: a control
// plane that lost a key refuses it, and rolling back the others splits the
// cluster. Before sign-new the run resumes with --cluster --force; after it each
// node finishes sign-new and then the documented finish.
func TestHaltGuidanceForAForcedRotation(t *testing.T) {
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(version.PKIDir, "carotate-forced"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("control-plane cp2 trust-new: job failed")
	for _, tc := range []struct {
		phase string
		want  []string
	}{
		{PhaseTrustNew, []string{"do not roll back any node", "--cluster --force --yes", "--restart-workloads or --skip-workload-check"}},
		{PhaseSignNew, []string{"do not roll back any node", "--phase=" + PhaseSignNew + " --yes", "Finishing a rotation by hand"}},
	} {
		msg := haltedAt(tc.phase, cause).Error()
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: %q missing %q", tc.phase, msg, want)
			}
		}
		if strings.Contains(msg, "--rollback") {
			t.Errorf("%s: %q must not offer --rollback", tc.phase, msg)
		}
	}
	msg := haltedBeforeSignNew("restart in-cluster API clients", cause).Error()
	if strings.Contains(msg, "--rollback") || !strings.Contains(msg, "--cluster --force --yes") || !strings.Contains(msg, "--restart-workloads or --skip-workload-check") {
		t.Errorf("before sign-new: %q should resume and not offer --rollback", msg)
	}
}

func TestRenewalFenceHeldThroughReadinessAndStopsLaterRestarts(t *testing.T) {
	redirectVar(t, &version.K8sConfDir, t.TempDir())
	redirectVar(t, &version.ControlSocket, filepath.Join(t.TempDir(), "absent.sock"))
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"etcd", "kube-apiserver"} {
		if err := os.WriteFile(staticpod.Path(name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cs := fakeAdminClientset(t)
	ctx := context.Background()
	_, guard, release, err := acquireMaintenanceLease(ctx, cs, "renewal/1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var restarts []string
	err = restartControlPlaneGuarded(io.Discard, func(name string) error { restarts = append(restarts, name); return nil }, func(name string) error {
		lease, err := cs.CoordinationV1().Leases(caNamespace).Get(ctx, maintenanceLeaseName, v1.GetOptions{})
		if err != nil || *lease.Spec.HolderIdentity != "renewal/1" {
			t.Fatalf("lease not held through readiness: %v", err)
		}
		successor := "migration/2"
		lease.Spec.HolderIdentity = &successor
		_, err = cs.CoordinationV1().Leases(caNamespace).Update(ctx, lease, v1.UpdateOptions{})
		return err
	}, func(what string) error { return confirmMaintenanceLease(ctx, guard, what) }, false)
	if !errors.Is(err, maintlease.ErrTaken) {
		t.Fatalf("must stop after takeover: %v", err)
	}
	if !reflect.DeepEqual(restarts, []string{"etcd"}) {
		t.Fatalf("restarted after lease loss: %v", restarts)
	}
}

func TestRestartFailureSurvivesLaterFenceCancellation(t *testing.T) {
	redirectVar(t, &version.K8sConfDir, t.TempDir())
	redirectVar(t, &version.ControlSocket, filepath.Join(t.TempDir(), "absent.sock"))
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"etcd", "kube-apiserver"} {
		if err := os.WriteFile(staticpod.Path(name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{"restart", "readiness"} {
		t.Run(phase, func(t *testing.T) {
			failure := errors.New("etcd restart failed")
			var restarted []string
			err := restartControlPlaneGuarded(io.Discard, func(name string) error {
				restarted = append(restarted, name)
				if phase == "restart" {
					return failure
				}
				return nil
			}, func(string) error { return failure }, func(what string) error {
				if what == "restarting kube-apiserver" {
					return context.Canceled
				}
				return nil
			}, false)
			if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) || interrupt.CancellationOnly(err) {
				t.Fatalf("fence cancellation hid the restart failure: %v", err)
			}
			if !reflect.DeepEqual(restarted, []string{"etcd"}) {
				t.Fatalf("restart continued after fence cancellation: %v", restarted)
			}
		})
	}
}

// TestKubeVipRequiredFailsTheRestart pins that a kube-vip restart,
// best-effort otherwise, fails the restart when the caller requires it: when
// the restart request fails, and when the manifest is rewritten but kubelet
// does not replace the process that ran before it. Only a required restart
// waits for the replacement.
func TestKubeVipRequiredFailsTheRestart(t *testing.T) {
	redirectVar(t, &version.K8sConfDir, t.TempDir())
	redirectVar(t, &version.ControlSocket, filepath.Join(t.TempDir(), "absent.sock"))
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staticpod.Path("kube-vip"), []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNow, oldWait := markNow, waitReplaced
	t.Cleanup(func() { markNow, waitReplaced = oldNow, oldWait })
	markNow = func() (staticpod.Mark, error) { return staticpod.Mark{}, nil }
	vipErr, staleErr := errors.New("vip failure"), errors.New("pid 42 still runs")
	wait := func(string) error { return nil }
	for _, tc := range []struct {
		name      string
		restart   error
		replaced  error
		required  bool
		want      error
		wantWaits int
	}{
		{name: "not required, restart fails", restart: vipErr},
		{name: "not required, not replaced", replaced: staleErr},
		{name: "required, restart fails", restart: vipErr, required: true, want: vipErr},
		{name: "required, not replaced", replaced: staleErr, required: true, want: staleErr, wantWaits: 1},
		{name: "required, replaced", required: true, wantWaits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := 0
			waitReplaced = func(name string, _ staticpod.Mark, _ time.Duration) error {
				if name != "kube-vip" {
					t.Errorf("waited for %s, want kube-vip", name)
				}
				waits++
				return tc.replaced
			}
			err := restartControlPlaneGuarded(io.Discard, func(string) error { return tc.restart }, wait, nil, tc.required)
			switch {
			case tc.want == nil && err != nil:
				t.Errorf("restart = %v, want success", err)
			case tc.want != nil && (!errors.Is(err, tc.want) || !strings.Contains(err.Error(), "kube-vip")):
				t.Errorf("restart = %v, want the kube-vip failure %v", err, tc.want)
			}
			if waits != tc.wantWaits {
				t.Errorf("waited for the replacement %d times, want %d", waits, tc.wantWaits)
			}
		})
	}
}
