package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// recordTrust stands in for the worker's firewalld step and returns every
// pod CIDR it was asked to trust.
func recordTrust(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := hostTrustPodCIDRs
	hostTrustPodCIDRs = func(cidr string) { got = append(got, cidr) }
	t.Cleanup(func() { hostTrustPodCIDRs = old })
	return &got
}

func workerState(t *testing.T, st *nodestate.NodeState) {
	t.Helper()
	oldPrereqs := hostPrereqs
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	t.Cleanup(func() { hostPrereqs = oldPrereqs })
	workerScratchHost(t)
	if err := nodestate.SaveState(st); err != nil {
		t.Fatal(err)
	}
}

// A worker's unit runs a bare `stratum up --worker`, so every start after the
// join takes the restart path. It must still trust the pod CIDR, and the one
// the worker joined with, not the default.
func TestWorkerRestartTrustsTheRecordedPodCIDR(t *testing.T) {
	registerCISForTest(t)
	got := recordTrust(t)
	workerState(t, &nodestate.NodeState{Role: "worker", Hostname: "w1", PodCIDR: "10.42.0.0/16,fd00:42::/56", CIS: true})
	var prereqCIS []bool
	hostPrereqs = func(o host.PrereqOptions) error {
		prereqCIS = append(prereqCIS, o.Profile.Name == nodestate.CISProfile)
		return nil
	}

	if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0] != "10.42.0.0/16,fd00:42::/56" {
		t.Errorf("restart trusted %q, want the recorded pod CIDR once", *got)
	}
	if len(prereqCIS) != 1 || !prereqCIS[0] {
		t.Errorf("host prerequisites on restart: calls = %v, want one with saved cis=true", prereqCIS)
	}
}

// A legacy worker has no recorded range, but interface-scoped firewalld
// convergence still runs. It must not invent a CIDR for any other caller.
func TestWorkerRestartWithoutRecordedPodCIDRStillConvergesFirewall(t *testing.T) {
	got := recordTrust(t)
	workerState(t, &nodestate.NodeState{Role: "worker", Hostname: "w1"})

	if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0] != "" {
		t.Errorf("restart passed a guessed range or skipped firewalld: %q", *got)
	}
}

func TestWorkerRestartMigratesExplicitPodCIDR(t *testing.T) {
	got := recordTrust(t)
	workerState(t, &nodestate.NodeState{Role: "worker", Hostname: "w1"})
	const cidr = "10.42.0.0/16,fd00:42::/56"
	if _, err := PrepareWorker(context.Background(), WorkerOptions{PodCIDR: cidr}); err != nil {
		t.Fatal(err)
	}
	st, err := nodestate.LoadState()
	if err != nil || st.PodCIDR != cidr {
		t.Fatalf("saved pod CIDR = %v, %v", st, err)
	}
	if _, err := PrepareWorker(context.Background(), WorkerOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || (*got)[0] != cidr || (*got)[1] != cidr {
		t.Errorf("migration and restart trusted %q, want %s twice", *got, cidr)
	}
}

// A malformed --pod-cidr is refused before anything on the host changes,
// firewalld included.
func TestWorkerJoinRefusesAMalformedPodCIDR(t *testing.T) {
	got := recordTrust(t)
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })

	_, err := PrepareWorker(context.Background(), WorkerOptions{Server: "https://cp:6443", Token: "abcdef.0123456789abcdef", CAHash: "sha256:aa", PodCIDR: "10.42.0.0"})
	if err == nil || !strings.Contains(err.Error(), "--pod-cidr") {
		t.Fatalf("PrepareWorker = %v, want a --pod-cidr refusal", err)
	}
	if len(*got) != 0 {
		t.Errorf("trusted %q before refusing", *got)
	}
}
