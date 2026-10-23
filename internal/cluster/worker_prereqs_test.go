package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
)

// A worker's unit runs a bare `stratum up --worker`, so every start after the
// join takes the restart path. It must still prepare the host -- after a
// reboot or an upgrade that is the only place the node's sysctls, including
// the inotify limits, can land -- with the CIS mode and SELinux opt-out it
// joined with.
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

	// A failure stops the start, as it does on a control plane.
	hostPrereqs = func(host.PrereqOptions) error { return errors.New("cgroup v2 is required") }
	if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err == nil {
		t.Errorf("worker restart ignored a failed host preparation")
	}
}

// A restart that finds spec.selinux changed in the worker's config file uses
// and saves the new setting.
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

// A CIS worker whose config sets spec.selinux: false is refused before the
// opt-out is saved, so removing it from the file is enough to start again.
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

// A restart whose config file does not set spec.selinux keeps the opt-out
// saved at join and says so, since leaving the line out is not the way to
// clear it.
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

// A restart of a worker that saved no opt-out, whose config file does not set
// spec.selinux, stays confined and prints no notice: the common case.
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

// A restart that reads spec.selinux: true clears the saved opt-out without a
// notice: the setting was given, not merely left out.
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
