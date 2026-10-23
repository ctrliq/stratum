package upgrade

import (
	"context"
	"io"

	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// supervisorFixture stands in for the running supervisor: stale until it is
// replaced (by a re-exec or a service restart), current after. calls records
// the handoff's actions in order.
type supervisorFixture struct {
	stale     bool
	pid       int
	start     uint64
	mainPID   int
	reexecErr error
	comesBack bool
	calls     []string
}

func newSupervisorFixture(t *testing.T) *supervisorFixture {
	t.Helper()
	f := &supervisorFixture{stale: true, pid: 4242, start: 1000, comesBack: true}
	record := filepath.Join(t.TempDir(), "unreplaceable-supervisor.json")
	swap(t, &unreplaceableSupervisorPath, func() string { return record })
	swap(t, &supervisorStarted, func(pid int) (uint64, bool, error) { return f.start, pid == f.pid, nil })
	swap(t, &runningSupervisor, func() (comps.RunningSupervisor, error) {
		if f.stale {
			return comps.RunningSupervisor{PID: f.pid, Exe: "/usr/local/bin/stratum (deleted)"}, nil
		}
		return comps.RunningSupervisor{PID: f.pid, Exe: "/usr/local/bin/stratum", Installed: true}, nil
	})
	swap(t, &reexecSupervisor, func() error {
		f.calls = append(f.calls, "reexec")
		if f.reexecErr == nil && f.comesBack {
			f.stale = false
		}
		return f.reexecErr
	})
	swap(t, &serviceMainPID, func() (int, error) { return f.mainPID, nil })
	swap(t, &serviceExecPath, func() (string, error) { return version.StratumInstallPath, nil })
	swap(t, &restartService, func() error {
		f.calls = append(f.calls, "restart service")
		f.stale = !f.comesBack
		return nil
	})
	swap(t, &supervisorStatus, func() (map[string]supervisor.ProcStatus, error) {
		return map[string]supervisor.ProcStatus{"kubelet": {Name: "kubelet", Pid: 7, Running: true, Wanted: true}}, nil
	})
	swap(t, &supervisorSleep, func(time.Duration) {})
	return f
}

func TestHandOverSupervisor(t *testing.T) {
	unsupported := fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)

	t.Run("a supervisor on the installed build is left alone", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.stale = false
		replaced, err := handOverSupervisor(io.Discard)
		if err != nil || replaced || len(f.calls) != 0 {
			t.Fatalf("replaced=%v err=%v calls=%q; want nothing done", replaced, err, f.calls)
		}
	})
	t.Run("a stale supervisor re-execs in place", func(t *testing.T) {
		f := newSupervisorFixture(t)
		replaced, err := handOverSupervisor(io.Discard)
		if err != nil || !replaced || strings.Join(f.calls, ",") != "reexec" {
			t.Fatalf("replaced=%v err=%v calls=%q; want one re-exec", replaced, err, f.calls)
		}
	})
	t.Run("an older supervisor systemd runs is restarted through systemd", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.reexecErr, f.mainPID = unsupported, f.pid
		replaced, err := handOverSupervisor(io.Discard)
		if err != nil || !replaced || strings.Join(f.calls, ",") != "reexec,restart service" {
			t.Fatalf("replaced=%v err=%v calls=%q; want the service restarted", replaced, err, f.calls)
		}
	})
	t.Run("an older supervisor whose service starts another binary is not restarted onto it", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.reexecErr, f.mainPID = unsupported, f.pid
		swap(t, &serviceExecPath, func() (string, error) { return "/dist/stratum", nil })
		replaced, err := handOverSupervisor(io.Discard)
		if err == nil || replaced || !strings.Contains(err.Error(), "/dist/stratum") || strings.Join(f.calls, ",") != "reexec" {
			t.Fatalf("replaced=%v err=%v calls=%q; want a refusal naming the unit's program and no restart", replaced, err, f.calls)
		}
	})
	t.Run("an older supervisor whose systemd status cannot be read is an error, not a guess", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.reexecErr = unsupported
		swap(t, &serviceMainPID, func() (int, error) { return 0, errors.New("systemctl show: dbus timeout") })
		replaced, err := handOverSupervisor(io.Discard)
		if err == nil || replaced || !strings.Contains(err.Error(), "dbus timeout") || strings.Join(f.calls, ",") != "reexec" {
			t.Fatalf("replaced=%v err=%v calls=%q; want the systemd error reported and nothing restarted", replaced, err, f.calls)
		}
	})
	t.Run("an older supervisor started by hand is warned about, not restarted", func(t *testing.T) {
		for _, mainPID := range []int{0, 99} {
			f := newSupervisorFixture(t)
			f.reexecErr, f.mainPID = unsupported, mainPID
			var replaced bool
			out, err := runWriter(t, func(w io.Writer) error {
				var err error
				replaced, err = handOverSupervisor(w)
				return err
			})
			if err != nil || replaced || strings.Join(f.calls, ",") != "reexec" {
				t.Fatalf("systemd main pid %d: replaced=%v err=%v calls=%q; want no restart", mainPID, replaced, err, f.calls)
			}
			if !strings.Contains(out, "WARN") || !strings.Contains(out, "`sudo /usr/local/bin/stratum up --worker`") ||
				!strings.Contains(out, "`sudo /usr/local/bin/stratum up --config <path>`") {
				t.Errorf("systemd main pid %d: no warning saying how to restart it:\n%s", mainPID, out)
			}
			if sup, _ := runningSupervisor(); !knownUnreplaceable(sup) {
				t.Errorf("systemd main pid %d: the supervisor is not recorded as one the handoff cannot replace", mainPID)
			}
		}
	})
	t.Run("a supervisor that does not come back is an error", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.comesBack = false
		swap(t, &supervisorBackAfter, 0)
		_, err := handOverSupervisor(io.Discard)
		if err == nil || !strings.Contains(err.Error(), "did not come back") {
			t.Fatalf("err = %v, want the supervisor reported missing", err)
		}
	})
	t.Run("a component that does not come back is waited for", func(t *testing.T) {
		newSupervisorFixture(t)
		polls := 0
		swap(t, &supervisorStatus, func() (map[string]supervisor.ProcStatus, error) {
			polls++
			running := polls > 2
			return map[string]supervisor.ProcStatus{"kubelet": {Name: "kubelet", Running: running, Wanted: true}}, nil
		})
		if _, err := handOverSupervisor(io.Discard); err != nil || polls < 3 {
			t.Fatalf("err = %v after %d polls, want it to wait for kubelet", err, polls)
		}
	})
	t.Run("a component another process answers for is waited for", func(t *testing.T) {
		newSupervisorFixture(t)
		polls := 0
		swap(t, &supervisorStatus, func() (map[string]supervisor.ProcStatus, error) {
			polls++
			return map[string]supervisor.ProcStatus{"kubelet": {Name: "kubelet", Running: true, Wanted: true, Foreign: polls <= 2}}, nil
		})
		if _, err := handOverSupervisor(io.Discard); err != nil || polls < 3 {
			t.Fatalf("err = %v after %d polls, want it to wait out the foreign kubelet", err, polls)
		}
	})
	t.Run("a refused re-exec is an error", func(t *testing.T) {
		f := newSupervisorFixture(t)
		f.reexecErr = errors.New("re-exec target: no such file")
		if _, err := handOverSupervisor(io.Discard); err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("err = %v, want the refusal", err)
		}
	})
	t.Run("a supervisor that cannot be identified is warned about", func(t *testing.T) {
		f := newSupervisorFixture(t)
		swap(t, &runningSupervisor, func() (comps.RunningSupervisor, error) { return comps.RunningSupervisor{}, errors.New("no /proc") })
		out, err := runWriter(t, func(w io.Writer) error { _, err := handOverSupervisor(w); return err })
		if err != nil || len(f.calls) != 0 || !strings.Contains(out, "WARN") {
			t.Fatalf("err=%v calls=%q out=%q; want a warning and nothing done", err, f.calls, out)
		}
	})
}

// TestHandOverSupervisorRecordsOnlyWhatItCannotReplace: the handoff records a
// supervisor only when it predates restarting itself in place and systemd
// does not run it, the one answer a rerun may trust without asking again.
// Every other outcome drops a record left by an earlier run, so a supervisor
// that has since been replaced, or answered differently, is asked again.
func TestHandOverSupervisorRecordsOnlyWhatItCannotReplace(t *testing.T) {
	unsupported := fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)
	for _, c := range []struct {
		name   string
		setup  func(t *testing.T, f *supervisorFixture)
		record bool
	}{
		{"already on the installed build", func(t *testing.T, f *supervisorFixture) { f.stale = false }, false},
		{"re-exec in place", func(t *testing.T, f *supervisorFixture) {}, false},
		{"older supervisor systemd restarts", func(t *testing.T, f *supervisorFixture) {
			f.reexecErr, f.mainPID = unsupported, f.pid
		}, false},
		{"older supervisor started by hand", func(t *testing.T, f *supervisorFixture) { f.reexecErr = unsupported }, true},
		{"refused re-exec", func(t *testing.T, f *supervisorFixture) { f.reexecErr = errors.New("no target") }, false},
		{"older supervisor whose service starts another binary", func(t *testing.T, f *supervisorFixture) {
			f.reexecErr, f.mainPID = unsupported, f.pid
			swap(t, &serviceExecPath, func() (string, error) { return "/dist/stratum", nil })
		}, false},
		{"systemd status unreadable", func(t *testing.T, f *supervisorFixture) {
			f.reexecErr = unsupported
			swap(t, &serviceMainPID, func() (int, error) { return 0, errors.New("dbus timeout") })
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSupervisorFixture(t)
			c.setup(t, f)
			// What an earlier run left: this supervisor, recorded as one
			// the handoff cannot replace.
			if err := writeUnreplaceable(f.pid); err != nil {
				t.Fatal(err)
			}
			_, _ = handOverSupervisor(io.Discard)
			f.stale = true
			sup, _ := runningSupervisor()
			if got := knownUnreplaceable(sup); got != c.record {
				t.Errorf("recorded as unreplaceable = %v after the handoff, want %v", got, c.record)
			}
		})
	}
}

// TestUnreplaceableRecordNamesOneProcess: the record matches the process it
// was written for, not a later one that reuses its pid, nor another pid; and
// a record that cannot be written is a warning in a handoff that is done.
func TestUnreplaceableRecordNamesOneProcess(t *testing.T) {
	f := newSupervisorFixture(t)
	f.reexecErr = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)
	if _, err := handOverSupervisor(io.Discard); err != nil {
		t.Fatal(err)
	}
	sup, _ := runningSupervisor()
	if !knownUnreplaceable(sup) {
		t.Fatal("the hand-started older supervisor was not recorded")
	}
	f.start++
	if knownUnreplaceable(sup) {
		t.Error("a process that reuses the recorded pid matches the record")
	}
	f.start--
	// Another process that started in the same clock tick.
	swap(t, &supervisorStarted, func(int) (uint64, bool, error) { return f.start, true, nil })
	if knownUnreplaceable(comps.RunningSupervisor{PID: f.pid + 1}) {
		t.Error("another pid matches the record")
	}
	// The recorded process, gone, with its start time read anyway.
	swap(t, &supervisorStarted, func(int) (uint64, bool, error) { return f.start, false, nil })
	if knownUnreplaceable(sup) {
		t.Error("a process that is no longer running matches the record")
	}

	forgetUnreplaceable()
	swap(t, &supervisorStarted, func(int) (uint64, bool, error) { return 0, false, errors.New("no /proc") })
	out, err := runWriter(t, func(w io.Writer) error { _, err := handOverSupervisor(w); return err })
	if err != nil || !strings.Contains(out, "WARN: could not record that the supervisor (pid 4242)") {
		t.Errorf("err = %v; want the handoff to succeed with a warning about the record:\n%s", err, out)
	}
}

// TestHandOverAndGateGatesOnlyAReplacedSupervisor: restarting the supervisor
// restarts every component, so the node is gated again; a handoff that did
// nothing needs no second gate.
func TestHandOverAndGateGatesOnlyAReplacedSupervisor(t *testing.T) {
	f := newSupervisorFixture(t)
	gates := 0
	if replaced, err := handOverAndGate(io.Discard, "upgrade", func() error { gates++; return nil }); err != nil || !replaced || gates != 1 {
		t.Fatalf("replaced=%v err=%v gates=%d; want one gate after the re-exec", replaced, err, gates)
	}
	if replaced, err := handOverAndGate(io.Discard, "upgrade", func() error { gates++; return nil }); err != nil || replaced || gates != 1 {
		t.Fatalf("replaced=%v err=%v gates=%d; want no gate when nothing was replaced", replaced, err, gates)
	}
	f.stale = true
	replaced, err := handOverAndGate(io.Discard, "rollback", func() error { return errors.New("kubelet not ready") })
	if err == nil || !strings.Contains(err.Error(), "rollback done") || !strings.Contains(err.Error(), "kubelet not ready") {
		t.Fatalf("err = %v, want the gate failure after the restart", err)
	}
	if !replaced {
		t.Errorf("a gate that failed after the restart reported the supervisor not replaced; its caller would restart a component again")
	}
}

// TestUpgradeNodeHandsOverTheSupervisorLast: the upgrade is verified and
// stamped before the supervisor restarts every component, then the node is
// gated again.
func TestUpgradeNodeHandsOverTheSupervisorLast(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	f := newSupervisorFixture(t)
	swap(t, &reexecSupervisor, func() error {
		*events = append(*events, "reexec")
		f.stale = false
		return nil
	})
	if err := UpgradeNode(io.Discard, NodeOptions{}); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	got := strings.Join(*events, " ")
	if want := "healthz#1 healthz#2 healthz#3 stamp:cp-1.registered reexec healthz#4"; got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// TestUpgradeAtTargetStillHandsOverAStaleSupervisor: a rerun after a handoff
// that was interrupted, or that an older supervisor could not do, finds the
// node at its target; it still restarts a supervisor running the old build.
func TestUpgradeAtTargetStillHandsOverAStaleSupervisor(t *testing.T) {
	// setRoots runs before resumeFixture: a SetRoots after it would recompute
	// every derived path (BinDir, K8sConfDir, StageDir) from the new roots and
	// step over the fixture's own per-var redirects, leaving the node state it
	// staged where the test no longer reads it.
	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "this build", "this build")
	runtimeFixture(t, version.K8sVersion, version.K8sVersion, version.K8sVersion)
	f := newSupervisorFixture(t)

	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{}) })
	if err != nil || strings.Contains(out, "nothing to do") || !strings.Contains(out, "its supervisor (pid 4242) runs") {
		t.Fatalf("err = %v; want the stale supervisor named, not \"nothing to do\"\n%s", err, out)
	}
	if strings.Join(f.calls, ",") != "reexec" {
		t.Errorf("calls = %q, want the stale supervisor re-exec'd", f.calls)
	}

	f.calls, f.stale = nil, true
	out, err = runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
	if !errors.Is(err, ErrUpgradeNeeded) || len(f.calls) != 0 || strings.Contains(out, "would restart the supervisor") {
		t.Errorf("--check: err %v, calls %q; want the node reported as needing the upgrade, and nothing done\n%s", err, f.calls, out)
	}
	f.stale = false
	if err := UpgradeNode(io.Discard, NodeOptions{Check: true}); err != nil {
		t.Errorf("--check with a current supervisor: %v, want nil", err)
	}
	saved := runningSupervisor
	runningSupervisor = func() (comps.RunningSupervisor, error) {
		return comps.RunningSupervisor{}, errors.New("no supervisor answers")
	}
	err = UpgradeNode(io.Discard, NodeOptions{Check: true})
	runningSupervisor = saved
	if err == nil || errors.Is(err, ErrUpgradeNeeded) || !strings.Contains(err.Error(), "no supervisor answers") {
		t.Errorf("--check with a supervisor it cannot identify: %v, want an error that is not ErrUpgradeNeeded", err)
	}
	f.stale = true

	out, err = runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{DryRun: true}) })
	if err != nil || len(f.calls) != 0 || !strings.Contains(out, "would restart the supervisor (pid 4242") {
		t.Errorf("--dry-run: calls %q, err %v; want the restart previewed and not done:\n%s", f.calls, err, out)
	}

	// The handoff restarts every component as an upgrade does, so a node that
	// fails the SELinux prerequisites is refused before it, by --check too:
	// otherwise the cluster preflight would pass it and its upgrade would
	// restart it unconfined.
	oldPrereqs := checkNodePrereqs
	checkNodePrereqs = func(io.Writer, bool) error {
		return fmt.Errorf("%w: SELinux is enforcing and the container-selinux package is not installed", ErrPrereqMissing)
	}
	t.Cleanup(func() { checkNodePrereqs = oldPrereqs })
	for _, o := range []NodeOptions{{}, {Check: true}, {DryRun: true}} {
		f.calls = nil
		if err := UpgradeNode(io.Discard, o); !errors.Is(err, ErrPrereqMissing) || len(f.calls) != 0 {
			t.Errorf("%+v with failing prerequisites: err %v, calls %q; want ErrPrereqMissing and no restart", o, err, f.calls)
		}
	}
}

// TestUpgradeAtTargetLeavesAnUnreplaceableSupervisorAlone: once a handoff
// found the supervisor predates restarting itself in place and systemd does
// not run it, a rerun at the target cannot do more than warn. --check answers
// that the node is current, with the warning saying how to restart the
// supervisor, so the cluster upgrade relays the warning and does not drain the
// node; the run and the dry run warn and change nothing. A supervisor not
// asked yet, or a different process, still gets the upgrade.
func TestUpgradeAtTargetLeavesAnUnreplaceableSupervisorAlone(t *testing.T) {
	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "this build", "this build")
	runtimeFixture(t, version.K8sVersion, version.K8sVersion, version.K8sVersion)
	f := newSupervisorFixture(t)
	f.reexecErr = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)

	// Not asked yet: it may restart itself in place, so it needs the upgrade.
	if err := UpgradeNode(io.Discard, NodeOptions{Check: true}); !errors.Is(err, ErrUpgradeNeeded) {
		t.Fatalf("--check before any handoff: %v, want ErrUpgradeNeeded", err)
	}
	if err := UpgradeNode(io.Discard, NodeOptions{}); err != nil || strings.Join(f.calls, ",") != "reexec" {
		t.Fatalf("the first run: err %v, calls %q; want it asked to restart in place", err, f.calls)
	}

	restartHint := "`sudo /usr/local/bin/stratum up --worker`"
	f.calls = nil
	for _, o := range []NodeOptions{{Check: true}, {DryRun: true}, {}} {
		out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, o) })
		if err != nil || len(f.calls) != 0 {
			t.Errorf("%+v: err %v, calls %q; want nil and nothing done\n%s", o, err, f.calls, out)
		}
		if !strings.Contains(out, "WARN: the supervisor (pid 4242) predates restarting itself in place") ||
			!strings.Contains(out, restartHint) {
			t.Errorf("%+v: no warning saying how to restart the supervisor:\n%s", o, out)
		}
		if strings.Contains(out, "would restart the supervisor") {
			t.Errorf("%+v: previews a restart the run would not do:\n%s", o, out)
		}
	}

	// Nothing restarts, so the prerequisites do not decide.
	oldPrereqs := checkNodePrereqs
	swap(t, &checkNodePrereqs, func(io.Writer, bool) error { return fmt.Errorf("%w: no container-selinux", ErrPrereqMissing) })
	err := UpgradeNode(io.Discard, NodeOptions{Check: true})
	checkNodePrereqs = oldPrereqs
	if err != nil {
		t.Errorf("--check with failing prerequisites: %v, want nil: nothing would restart", err)
	}

	// A new process at that pid is asked again.
	f.start++
	if err := UpgradeNode(io.Discard, NodeOptions{Check: true}); !errors.Is(err, ErrUpgradeNeeded) {
		t.Errorf("--check for a new process at the recorded pid: %v, want ErrUpgradeNeeded", err)
	}
}

// TestClusterRerunRelaysAnUnreplaceableSupervisorWithoutDrainingIt: the node's
// own --check output, once a handoff found its supervisor cannot be replaced,
// reaches the cluster upgrade's screen with the node named, and the rerun
// leaves the node undrained and runs no upgrade Job on it. Every node is at
// the target, as on a rerun after an upgrade that completed.
func TestClusterRerunRelaysAnUnreplaceableSupervisorWithoutDrainingIt(t *testing.T) {
	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "this build", "this build")
	runtimeFixture(t, version.K8sVersion, version.K8sVersion, version.K8sVersion)
	f := newSupervisorFixture(t)
	f.reexecErr = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)
	if err := UpgradeNode(io.Discard, NodeOptions{}); err != nil {
		t.Fatal(err)
	}
	checkLog, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
	if err != nil {
		t.Fatalf("w1's --check: %v, want exit 0", err)
	}

	objs := upgradeNodesFixture()
	for _, o := range objs {
		o.(*corev1.Node).Status.NodeInfo.KubeletVersion = version.K8sVersion
	}
	cs := fake.NewSimpleClientset(objs...)
	jobs := fastOrchestrator(t, nil)
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return 0, nil }
	swap(t, &runExitCodeLogs, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, string, error) {
		if s.Node == "w1" {
			return 0, checkLog, nil
		}
		return 0, "", nil
	})
	var buf strings.Builder
	if err := UpgradeCluster(&buf, ClusterOptions{Target: version.K8sVersion, From: stagedBinary(t), Client: cs}); err != nil {
		t.Fatalf("UpgradeCluster: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if want := "  w1: WARN: the supervisor (pid 4242) predates restarting itself in place"; !strings.Contains(out, want) ||
		!strings.Contains(out, "`sudo /usr/local/bin/stratum up --worker`") {
		t.Errorf("the rerun does not relay w1's warning with how to restart its supervisor:\n%s", out)
	}
	if !strings.Contains(out, "w1 already runs this build of "+version.K8sVersion+"; not drained") || *jobs != 0 {
		t.Errorf("upgrade Jobs = %d; want w1 reported current and not drained:\n%s", *jobs, out)
	}
	for _, a := range cs.Actions() {
		if a.GetSubresource() == "eviction" {
			t.Errorf("the rerun evicted a pod: %v", a)
		}
	}
}

// TestRollbackDryRunPreviewsTheSupervisorRestart: every rollback preview
// covers the restart a real rollback ends with, and does not do it.
func TestRollbackDryRunPreviewsTheSupervisorRestart(t *testing.T) {
	rollbackRestartFixture(t, "")
	f := newSupervisorFixture(t)
	out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{DryRun: true}) })
	if err != nil || len(f.calls) != 0 || !strings.Contains(out, "would restart the supervisor (pid 4242") {
		t.Fatalf("calls %q, err %v; want the restart previewed and not done:\n%s", f.calls, err, out)
	}
	f.stale = false
	out, _ = runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{DryRun: true}) })
	if strings.Contains(out, "would restart the supervisor") {
		t.Errorf("previewed a restart of a supervisor already on the installed build, with no stratum to restore:\n%s", out)
	}

	writeFile(t, stratumInstallPath+".bak-20260928-100000", "old stratum", 0o755)
	out, _ = runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{DryRun: true}) })
	if !strings.Contains(out, "would restart the supervisor (pid 4242) on the restored") || len(f.calls) != 0 {
		t.Errorf("a rollback that restores stratum must preview the restart onto it, even for a current supervisor (calls %q):\n%s", f.calls, out)
	}
}

// TestPreviewSupervisorHandoffMirrorsTheRealRun: whether a supervisor can
// restart itself in place is only learnt by asking it to, so the preview
// cannot promise a restart that the real run's fallback for an older one
// would not do: one systemd does not run is left running, and one systemd
// runs onto another build ends the run with an error. It promises one only
// where both outcomes restart it.
func TestPreviewSupervisorHandoffMirrorsTheRealRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mainPID  func(pid int) (int, error)
		execPath func() (string, error)
		want     string
	}{
		{
			name:    "systemd runs it and starts the installed build",
			mainPID: func(pid int) (int, error) { return pid, nil },
		},
		{
			name:    "started by hand",
			mainPID: func(int) (int, error) { return 0, nil },
			want:    "if it can restart itself in place; one that predates that is left running, with a warning saying how to restart it, since systemd does not run it",
		},
		{
			name:    "systemd runs another pid",
			mainPID: func(pid int) (int, error) { return pid + 1, nil },
			want:    "if it can restart itself in place; one that predates that is left running, with a warning saying how to restart it, since systemd does not run it",
		},
		{
			name:    "systemd cannot say",
			mainPID: func(int) (int, error) { return 0, errors.New("systemctl show: boom") },
			want:    "if it can restart itself in place; for one that predates that, the run ends with an error, keeping the installed build, if systemd still cannot say whether it runs it (systemctl show: boom)",
		},
		{
			name:     "the service starts another build",
			mainPID:  func(pid int) (int, error) { return pid, nil },
			execPath: func() (string, error) { return "/opt/old/stratum", nil },
			want:     "if it can restart itself in place; for one that predates that, the run ends with an error, keeping the installed build, since the stratum service starts /opt/old/stratum, not the installed " + version.StratumInstallPath,
		},
		{
			name:     "the service's program cannot be read",
			mainPID:  func(pid int) (int, error) { return pid, nil },
			execPath: func() (string, error) { return "", errors.New("no ExecStart") },
			want:     "if it can restart itself in place; for one that predates that, the run ends with an error, keeping the installed build, if the program the stratum service starts still cannot be read (no ExecStart)",
		},
	} {
		for _, restores := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restores=%v", tc.name, restores), func(t *testing.T) {
				f := newSupervisorFixture(t)
				swap(t, &serviceMainPID, func() (int, error) { return tc.mainPID(f.pid) })
				if tc.execPath != nil {
					swap(t, &serviceExecPath, tc.execPath)
				}
				var b strings.Builder
				previewSupervisorHandoff(&b, restores)
				out := b.String()
				if !strings.Contains(out, "would restart the supervisor (pid 4242") || len(f.calls) != 0 {
					t.Fatalf("calls %q; want the restart previewed and not done:\n%s", f.calls, out)
				}
				if tc.want == "" && strings.Contains(out, "if it can") {
					t.Errorf("hedged a restart that both of the real run's outcomes do:\n%s", out)
				}
				if tc.want != "" && !strings.Contains(out, tc.want) {
					t.Errorf("preview = %q; want it to say %q", out, tc.want)
				}
			})
		}
	}

	// A rollback that cannot find the supervisor now may not find it after the
	// restore either, and the real run then only warns.
	t.Run("a rollback that cannot tell which stratum the supervisor runs", func(t *testing.T) {
		f := newSupervisorFixture(t)
		swap(t, &runningSupervisor, func() (comps.RunningSupervisor, error) {
			return comps.RunningSupervisor{}, errors.New("no supervisor answers")
		})
		var b strings.Builder
		previewSupervisorHandoff(&b, true)
		want := "would restart the supervisor on the restored " + version.StratumInstallPath +
			" if it can then tell which stratum the supervisor runs, and it can restart itself in place or systemd runs it" +
			" with a service that starts that build (could not now: no supervisor answers)"
		if !strings.Contains(b.String(), want) || len(f.calls) != 0 {
			t.Errorf("calls %q, preview = %q; want %q and nothing done", f.calls, b.String(), want)
		}
	})
}

// TestRollbackHandsOverTheSupervisorLast: a rollback restores the previous
// stratum on disk, so the supervisor is restarted onto it too.
func TestRollbackHandsOverTheSupervisorLast(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	f := newSupervisorFixture(t)
	swap(t, &reexecSupervisor, func() error {
		*restarted = append(*restarted, "reexec")
		f.stale = false
		return nil
	})
	if err := RollbackNode(io.Discard, RollbackOptions{SoakSec: 1}); err != nil {
		t.Fatalf("RollbackNode: %v", err)
	}
	if got := strings.Join(*restarted, ","); got != "containerd,kubelet,reexec" {
		t.Errorf("calls = %q, want the supervisor re-exec'd after the host units", got)
	}
}

// operatorHandoffCases are the ways a supervisor handoff can end, and whether
// each restarts the operator with the supervisor: only a replaced supervisor
// does, whether or not it then comes back. Every other ending leaves the
// operator for the upgrade or rollback to restart, once, after the handoff.
var operatorHandoffCases = []struct {
	name  string
	setup func(t *testing.T, f *supervisorFixture)
	calls string // the handoff's calls, then the run's operator restart
	fails bool
}{
	{"supervisor already on the installed build", func(t *testing.T, f *supervisorFixture) { f.stale = false },
		"operator", false},
	{"re-exec in place", func(*testing.T, *supervisorFixture) {}, "reexec", false},
	{"older supervisor systemd restarts", func(t *testing.T, f *supervisorFixture) {
		f.reexecErr, f.mainPID = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported), f.pid
	}, "reexec,restart service", false},
	{"older supervisor started by hand", func(t *testing.T, f *supervisorFixture) {
		f.reexecErr = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)
	}, "reexec,operator", false},
	{"older supervisor whose service starts another build", func(t *testing.T, f *supervisorFixture) {
		f.reexecErr, f.mainPID = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported), f.pid
		swap(t, &serviceExecPath, func() (string, error) { return "/dist/stratum", nil })
	}, "reexec,operator", true},
	{"refused re-exec", func(t *testing.T, f *supervisorFixture) { f.reexecErr = errors.New("re-exec target: no such file") },
		"reexec,operator", true},
	{"supervisor that cannot be identified", func(t *testing.T, f *supervisorFixture) {
		swap(t, &runningSupervisor, func() (comps.RunningSupervisor, error) { return comps.RunningSupervisor{}, errors.New("no /proc") })
	}, "operator", false},
	{"older supervisor whose systemd status cannot be read", func(t *testing.T, f *supervisorFixture) {
		f.reexecErr = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported)
		swap(t, &serviceMainPID, func() (int, error) { return 0, errors.New("systemctl show: dbus timeout") })
	}, "reexec,operator", true},
	{"older supervisor whose service restart fails", func(t *testing.T, f *supervisorFixture) {
		f.reexecErr, f.mainPID = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported), f.pid
		swap(t, &restartService, func() error { f.calls = append(f.calls, "restart service"); return errors.New("job failed") })
	}, "reexec,restart service,operator", true},
	{"older supervisor systemd restarts that does not come back", func(t *testing.T, f *supervisorFixture) {
		f.reexecErr, f.mainPID = fmt.Errorf("%w: unknown op", supervisor.ErrReExecUnsupported), f.pid
		f.comesBack = false
		swap(t, &supervisorBackAfter, 0)
	}, "reexec,restart service", true},
	{"re-exec'd supervisor that does not come back", func(t *testing.T, f *supervisorFixture) {
		f.comesBack = false
		swap(t, &supervisorBackAfter, 0)
	}, "reexec", true},
}

// TestUpgradeRestartsTheOperatorOnce: a handoff that replaces the supervisor
// restarts every component it runs, the operator included, so the upgrade
// restarts the operator itself only when the handoff did not, and after it.
func TestUpgradeRestartsTheOperatorOnce(t *testing.T) {
	for _, tc := range operatorHandoffCases {
		t.Run(tc.name, func(t *testing.T) {
			controllerUpgradeFixture(t, "v1.37.9", 0)
			f := newSupervisorFixture(t)
			tc.setup(t, f)
			swap(t, &unitExists, func(string) bool { return true })
			swap(t, &restartUnit, func(u string) error {
				if u == "operator" {
					f.calls = append(f.calls, u)
				}
				return nil
			})
			out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
			if (err != nil) != tc.fails {
				t.Fatalf("UpgradeNode = %v, want failure %v\n%s", err, tc.fails, out)
			}
			if got := strings.Join(f.calls, ","); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
		})
	}
}

// TestUpgradeRestartsTheOperatorWhenItStopsShortOfTheHandoff: a failure
// after the node gate and the host-config refresh still restarts the
// operator onto the installed build, as it did before the handoff came last.
func TestUpgradeRestartsTheOperatorWhenItStopsShortOfTheHandoff(t *testing.T) {
	controllerUpgradeFixture(t, "v1.37.9", 3)
	f := newSupervisorFixture(t)
	swap(t, &unitExists, func(string) bool { return true })
	swap(t, &restartUnit, func(u string) error {
		if u == "operator" {
			f.calls = append(f.calls, u)
		}
		return nil
	})
	_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
	if err == nil || !strings.Contains(err.Error(), "post-upgrade host reload health gate") {
		t.Fatalf("UpgradeNode = %v, want the post-reload gate failure", err)
	}
	if got := strings.Join(f.calls, ","); got != "operator" {
		t.Errorf("calls = %q, want the operator restarted and no handoff", got)
	}
}

// TestRollbackRestartsTheOperatorOnce: as for an upgrade, a rollback leaves
// the operator to the handoff that replaces the supervisor, and restarts it
// itself, after the handoff, only when it did not.
func TestRollbackRestartsTheOperatorOnce(t *testing.T) {
	for _, tc := range operatorHandoffCases {
		t.Run(tc.name, func(t *testing.T) {
			rollbackRestartFixture(t, "")
			f := newSupervisorFixture(t)
			tc.setup(t, f)
			swap(t, &unitExists, func(string) bool { return true })
			// The operator's restarts join the handoff's calls, in the order
			// they happen, so a restart before the handoff fails too.
			hostRestart := restartUnit
			swap(t, &restartUnit, func(u string) error {
				if u == "operator" {
					f.calls = append(f.calls, u)
				}
				return hostRestart(u)
			})
			out, err := runWriter(t, func(w io.Writer) error { return RollbackNode(w, RollbackOptions{SoakSec: 1}) })
			if (err != nil) != tc.fails {
				t.Fatalf("RollbackNode = %v, want failure %v\n%s", err, tc.fails, out)
			}
			if got := strings.Join(f.calls, ","); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
		})
	}
}

// TestRollbackRestartsTheOperatorWhenItStopsShortOfTheHandoff: a rollback
// that restored and gated the node but fails before the handoff, here in
// the etcd restore, still restarts the operator onto the restored build.
func TestRollbackRestartsTheOperatorWhenItStopsShortOfTheHandoff(t *testing.T) {
	restarted := rollbackRestartFixture(t, "")
	f := newSupervisorFixture(t)
	swap(t, &unitExists, func(string) bool { return true })
	swap(t, &rollbackNodeIP, func() (string, error) { return "", errors.New("no node IP") })
	_, err := runWriter(t, func(w io.Writer) error {
		return RollbackNode(w, RollbackOptions{SoakSec: 1, EtcdSnapshot: "/nonexistent.db"})
	})
	if err == nil || !strings.Contains(err.Error(), "no node IP") {
		t.Fatalf("RollbackNode = %v, want the etcd step's failure", err)
	}
	if len(f.calls) != 0 || len(*restarted) == 0 || (*restarted)[len(*restarted)-1] != "operator" {
		t.Errorf("handoff calls %q, restarted %q; want no handoff and the operator restarted last", f.calls, *restarted)
	}
}
