package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
)

// After a reboot the restart is the only place the node's sysctls can land.
func TestWorkerRestartPreparesTheHost(t *testing.T) {
	registerCISForTest(t)
	workerScratchHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1", CIS: true, SELinuxOptOut: true}); err != nil {
		t.Fatal(err)
	}

	var calls []host.PrereqOptions
	oldPrereqs := hostPrereqs
	oldTrust := hostTrustPodCIDRs
	hostPrereqs = func(o host.PrereqOptions) error { calls = append(calls, o); return nil }
	hostTrustPodCIDRs = func(string) {}
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })

	if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Profile.Name != nodestate.CISProfile || !calls[0].SELinuxOptOut || !calls[0].Existing {
		t.Errorf("host prerequisites on restart: calls = %+v, want one with the cis profile, the opt-out and Existing", calls)
	}

	hostPrereqs = func(host.PrereqOptions) error { return errors.New("cgroup v2 is required") }
	if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err == nil {
		t.Errorf("worker restart ignored a failed host preparation")
	}
}

func TestWorkerRestartTakesAChangedSELinuxSetting(t *testing.T) {
	workerScratchHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	var calls []host.PrereqOptions
	oldPrereqs, oldTrust := hostPrereqs, hostTrustPodCIDRs
	hostPrereqs = func(o host.PrereqOptions) error { calls = append(calls, o); return nil }
	hostTrustPodCIDRs = func(string) {}
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })

	optOut := true
	if _, err := PrepareWorker(context.Background(), WorkerOptions{SELinuxOptOut: &optOut}); err != nil {
		t.Fatal(err)
	}
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !calls[0].SELinuxOptOut || !st.SELinuxOptOut {
		t.Errorf("prereqs %+v, saved opt-out %v; want the changed setting used and saved", calls, st.SELinuxOptOut)
	}
}

// Removing the line from the config must be enough to start again.
func TestWorkerRestartRefusesACISOptOutBeforeSavingIt(t *testing.T) {
	registerCISForTest(t)
	workerScratchHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1", CIS: true}); err != nil {
		t.Fatal(err)
	}
	oldPrereqs, oldTrust := hostPrereqs, hostTrustPodCIDRs
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	hostTrustPodCIDRs = func(string) {}
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })

	optOut := true
	if _, err := PrepareWorker(context.Background(), WorkerOptions{SELinuxOptOut: &optOut}); err == nil {
		t.Fatal("a CIS worker accepted spec.selinux: false")
	}
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.SELinuxOptOut {
		t.Error("the refused opt-out was saved")
	}
}

// Leaving spec.selinux out is not the way to clear the saved opt-out.
func TestWorkerRestartKeepsASavedOptOutWhenUnset(t *testing.T) {
	workerScratchHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1", SELinuxOptOut: true}); err != nil {
		t.Fatal(err)
	}
	var calls []host.PrereqOptions
	oldPrereqs, oldTrust := hostPrereqs, hostTrustPodCIDRs
	hostPrereqs = func(o host.PrereqOptions) error { calls = append(calls, o); return nil }
	hostTrustPodCIDRs = func(string) {}
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })

	out := captureWorkerOutput(t, func() {
		if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !calls[0].SELinuxOptOut || !st.SELinuxOptOut {
		t.Errorf("prereqs %+v, saved opt-out %v; want the saved opt-out kept", calls, st.SELinuxOptOut)
	}
	if strings.Count(out, "Set spec.selinux: true in its config file to confine this worker") != 1 {
		t.Errorf("restart with no spec.selinux in the config printed %q; want the notice once, saying how to clear it", out)
	}
}

func TestWorkerRestartWithoutAnOptOutPrintsNoNotice(t *testing.T) {
	workerScratchHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	var calls []host.PrereqOptions
	oldPrereqs, oldTrust := hostPrereqs, hostTrustPodCIDRs
	hostPrereqs = func(o host.PrereqOptions) error { calls = append(calls, o); return nil }
	hostTrustPodCIDRs = func(string) {}
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })

	out := captureWorkerOutput(t, func() {
		if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].SELinuxOptOut || st.SELinuxOptOut {
		t.Errorf("prereqs %+v, saved opt-out %v; want the worker to stay confined", calls, st.SELinuxOptOut)
	}
	if strings.Contains(out, "NOTE: keeping the spec.selinux: false") {
		t.Errorf("a worker with no saved opt-out printed the keep notice: %q", out)
	}
}

// spec.selinux: true is a setting, not a line left out.
func TestWorkerRestartClearsTheOptOutOnSELinuxTrue(t *testing.T) {
	workerScratchHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1", SELinuxOptOut: true}); err != nil {
		t.Fatal(err)
	}
	var calls []host.PrereqOptions
	oldPrereqs, oldTrust := hostPrereqs, hostTrustPodCIDRs
	hostPrereqs = func(o host.PrereqOptions) error { calls = append(calls, o); return nil }
	hostTrustPodCIDRs = func(string) {}
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })

	selinux := false
	out := captureWorkerOutput(t, func() {
		if _, err := PrepareWorker(context.Background(), WorkerOptions{SELinuxOptOut: &selinux}); err != nil {
			t.Fatal(err)
		}
	})
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].SELinuxOptOut || st.SELinuxOptOut {
		t.Errorf("prereqs %+v, saved opt-out %v; want spec.selinux: true to clear it", calls, st.SELinuxOptOut)
	}
	if strings.Contains(out, "NOTE: keeping the spec.selinux: false") {
		t.Errorf("restart with spec.selinux: true printed the keep notice: %q", out)
	}
}
