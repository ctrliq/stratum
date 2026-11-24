package certs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/pki"
)

func TestClusterMigrationIsPhaseMajor(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("migration: %v", err)
	}

	want := []string{
		"cp1:trust-both", "cp2:trust-both", "cp3:trust-both",
		"cp1:issue-new", "cp2:issue-new", "cp3:issue-new",
		"cp1:drop-root", "cp2:drop-root", "cp3:drop-root",
	}
	if strings.Join(f.calls, " ") != strings.Join(want, " ") {
		t.Errorf("phases were not phase-major.\n got: %v\nwant: %v", f.calls, want)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateMigrated {
			t.Errorf("%s ended at %s, want %s", n, got, pki.EtcdStateMigrated)
		}
	}
	assertClusterStillWorks(t, f)
}

func TestDropRootRefusesUntilEveryNodeCompletedIssueNew(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.failAt["cp3:issue-new"] = true
	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration reported success with cp3 still at trust-both")
	}
	if len(phaseCalls(f, pki.EtcdPhaseDropRoot)) != 0 {
		t.Errorf("drop-root was dispatched despite a node that never completed issue-new: %v", f.calls)
	}
	assertClusterStillWorks(t, f)

	f2, nodes2 := newFakeCluster(t, "cp1", "cp2", "cp3")
	m := f2.migration(nodes2, false)
	for _, n := range nodes2 {
		if err := f2.RunPhase(n, pki.EtcdPhaseTrustBoth, n.Name != "cp1", false); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes2[:2] {
		if err := f2.RunPhase(n, pki.EtcdPhaseIssueNew, false, false); err != nil {
			t.Fatal(err)
		}
	}
	gateErr := m.gateDropRoot()
	if gateErr == nil {
		t.Fatal("the drop-root gate passed with cp3 still at trust-both")
	}
	if !strings.Contains(gateErr.Error(), "cp3") {
		t.Errorf("the gate must name the lagging node, got: %v", gateErr)
	}
}

func TestDropRootGateFailsClosedOnUnknownState(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	m := f.migration(nodes, false)
	for _, n := range nodes {
		if err := f.RunPhase(n, pki.EtcdPhaseTrustBoth, n.Name != "cp1", false); err != nil {
			t.Fatal(err)
		}
		if err := f.RunPhase(n, pki.EtcdPhaseIssueNew, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.gateDropRoot(); err != nil {
		t.Fatalf("precondition: the gate should pass here, got %v", err)
	}
	f.unreachable["cp2"] = true
	err := m.gateDropRoot()
	if err == nil {
		t.Fatal("the gate passed with cp2's etcd unreachable — an indeterminate node was read as ready")
	}
	if !strings.Contains(err.Error(), "cp2") {
		t.Errorf("the refusal must name the indeterminate node, got: %v", err)
	}
}

func TestGateRefusesWhenEtcdQuorumIsUnhealthy(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	f.quorumErr = errQuorum
	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration proceeded to drop-root with an unhealthy etcd quorum")
	}
	if len(phaseCalls(f, pki.EtcdPhaseDropRoot)) != 0 {
		t.Errorf("drop-root was dispatched despite an unhealthy quorum: %v", f.calls)
	}
	assertClusterStillWorks(t, f)
}

func TestRefusesOnPartiallyMigratedCluster(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.RunPhase(nodes[0], pki.EtcdPhaseTrustBoth, false, false); err != nil {
		t.Fatal(err)
	}
	f.calls = nil

	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration proceeded on a cluster whose nodes are at different phases")
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused run must dispatch nothing, got: %v", f.calls)
	}
	for _, want := range []string{"cp1=trust-both", "cp2=not-migrated", "cp3=not-migrated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must surface the per-node state %q, got: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "--resume") {
		t.Errorf("refusal must name the way forward, got: %v", err)
	}
}

func TestResumeDoesNotOverrideUnknown(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.unreachable["cp2"] = true

	for _, resume := range []bool{false, true} {
		f.calls = nil
		err := f.migration(nodes, resume).run()
		if err == nil {
			t.Fatalf("resume=%v: migration proceeded with cp2 indeterminate", resume)
		}
		if !strings.Contains(err.Error(), "cp2") {
			t.Errorf("resume=%v: refusal must name the indeterminate node, got: %v", resume, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("resume=%v: a refused run must dispatch nothing, got: %v", resume, f.calls)
		}
	}
}

func TestResumeContinuesFromTheSurveyedState(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.failAt["cp3:issue-new"] = true
	if err := f.migration(nodes, false).run(); err == nil {
		t.Fatal("precondition: the first run should have halted at cp3")
	}
	if got := f.state("cp1"); got != pki.EtcdStateIssueNew {
		t.Fatalf("precondition: cp1 should be at issue-new, got %s", got)
	}

	delete(f.failAt, "cp3:issue-new")
	f.calls = nil
	if err := f.migration(nodes, true).run(); err != nil {
		t.Fatalf("resumed migration: %v", err)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateMigrated {
			t.Errorf("%s ended at %s after a resumed run, want %s", n, got, pki.EtcdStateMigrated)
		}
	}
	if got := phaseCalls(f, pki.EtcdPhaseTrustBoth); len(got) != 3 {
		t.Errorf("resumed run dispatched trust-both to %v, want all three nodes", got)
	}
	assertClusterStillWorks(t, f)
}

func TestHaltOnUnreachableNodeLeavesAWorkingCluster(t *testing.T) {
	for _, phase := range EtcdCAPhases {
		t.Run(phase, func(t *testing.T) {
			f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
			f.failAt["cp3:"+phase] = true
			if err := f.migration(nodes, false).run(); err == nil {
				t.Fatalf("migration reported success though cp3 failed %s", phase)
			}
			for i, p := range EtcdCAPhases {
				if p != phase {
					continue
				}
				for _, later := range EtcdCAPhases[i+1:] {
					if got := phaseCalls(f, later); len(got) != 0 {
						t.Errorf("%s was dispatched to %v after %s failed", later, got, phase)
					}
				}
			}
			assertClusterStillWorks(t, f)
		})
	}
}

func TestRollbackReachableFromEachIntermediatePhase(t *testing.T) {
	for _, upto := range []string{pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew} {
		t.Run("from-"+upto, func(t *testing.T) {
			f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
			for _, phase := range EtcdCAPhases {
				for _, n := range nodes {
					if err := f.RunPhase(n, phase, phase == pki.EtcdPhaseTrustBoth && n.Name != "cp1", false); err != nil {
						t.Fatal(err)
					}
				}
				if phase == upto {
					break
				}
			}
			f.calls = nil

			if err := f.migration(nodes, false).rollback(false); err != nil {
				t.Fatalf("rollback from %s: %v", upto, err)
			}
			for _, n := range f.order {
				if got := f.state(n); got != pki.EtcdStateTrustBoth {
					t.Errorf("%s is at %s after rollback, want %s", n, got, pki.EtcdStateTrustBoth)
				}
			}
			assertClusterStillWorks(t, f)
		})
	}
}

func TestRollbackRefusesOnAMigratedClusterWithoutForce(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	f.calls = nil

	err := f.migration(nodes, false).rollback(false)
	if err == nil {
		t.Fatal("rollback un-migrated a fully migrated cluster without --force — the etcd trust hole re-opened by a recovery command")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal must name the opt-in, got: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused rollback must dispatch nothing, got: %v", f.calls)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateMigrated {
			t.Errorf("%s moved to %s despite the refusal", n, got)
		}
	}
}

func TestNothingToDoOnAMigratedClusterStillVerifies(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	f.calls = nil
	f.confirmed = 0
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("re-run on a migrated cluster: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a migrated cluster must have no phases dispatched, got: %v", f.calls)
	}
	if f.confirmed != len(nodes) {
		t.Errorf("the nothing-to-do path verified %d of %d nodes; it must confirm every one", f.confirmed, len(nodes))
	}
}

func TestNothingToDoSurfacesAVerificationFailure(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	f.confirmFail["cp2"] = true
	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("a migrated-but-unverified cluster was reported as done")
	}
	if !strings.Contains(err.Error(), "cp2") {
		t.Errorf("the failure must name the unverified node cp2, got: %v", err)
	}
}

func TestQuorumMustReconvergeBetweenNodes(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.quorumErr = errQuorum

	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration continued through a cluster that never re-converged")
	}
	if got := phaseCalls(f, pki.EtcdPhaseTrustBoth); len(got) != 1 {
		t.Errorf("trust-both was dispatched to %v after the cluster failed to re-converge — it should halt on the first node", got)
	}
	if !strings.Contains(err.Error(), "re-converge") {
		t.Errorf("the halt must say the cluster did not re-converge, got: %v", err)
	}
}

func TestSkippedPhaseDoesNotRestartEtcd(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	f.failRestartAt["cp1:"+pki.EtcdPhaseTrustBoth] = true
	f.failRestartAt["cp1:"+pki.EtcdPhaseIssueNew] = true
	if err := f.RunPhase(nodes[0], pki.EtcdPhaseTrustBoth, false, false); err != nil {
		t.Errorf("a skipped trust-both restarted etcd: %v", err)
	}
	if err := f.RunPhase(nodes[0], pki.EtcdPhaseIssueNew, false, false); err != nil {
		t.Errorf("a skipped issue-new restarted etcd: %v", err)
	}
}

func TestGateRefusesWhenAWrittenPhaseNeverRestarted(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	m := f.migration(nodes, false)

	for _, phase := range []string{pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew} {
		for i, n := range nodes {
			fetchShared := i > 0 && phase == pki.EtcdPhaseTrustBoth
			if err := f.RunPhase(n, phase, fetchShared, false); err != nil {
				t.Fatalf("precondition %s on %s: %v", phase, n.Name, err)
			}
		}
	}
	if disk := f.diskState("cp3"); disk != pki.EtcdStateIssueNew {
		t.Fatalf("precondition: cp3 disk = %s, want issue-new", disk)
	}
	f.running["cp3"] = pki.EtcdStateTrustBoth

	err := m.gateDropRoot()
	if err == nil {
		t.Fatal("gateDropRoot allowed drop-root while cp3's running etcd was still at trust-both")
	}
	if !strings.Contains(err.Error(), "cp3") || !strings.Contains(err.Error(), pki.EtcdPhaseIssueNew) {
		t.Errorf("the refusal must name cp3 and the incomplete issue-new phase, got: %v", err)
	}
}

func TestRollbackWithForceUnMigratesEveryNode(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateMigrated {
			t.Fatalf("precondition: %s is at %s, want %s", n, got, pki.EtcdStateMigrated)
		}
	}
	f.calls = nil

	if err := f.migration(nodes, false).rollback(true); err != nil {
		t.Fatalf("forced rollback of a migrated cluster: %v", err)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateTrustBoth {
			t.Errorf("%s is at %s after a forced rollback, want %s", n, got, pki.EtcdStateTrustBoth)
		}
	}
	if len(phaseCalls(f, pki.EtcdPhaseRollback)) != 3 {
		t.Errorf("forced rollback did not reach every node: %v", f.calls)
	}
	assertClusterStillWorks(t, f)
}

func TestRollbackSkipsNodesWithNothingToUndo(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.failAt["cp2:"+pki.EtcdPhaseTrustBoth] = true
	if err := f.migration(nodes, false).run(); err == nil {
		t.Fatal("precondition: the forward run should have halted at cp2")
	}
	f.calls = nil

	if err := f.migration(nodes, true).rollback(false); err != nil {
		t.Fatalf("rollback after a halted run: %v", err)
	}
	dispatched := phaseCalls(f, pki.EtcdPhaseRollback)
	if len(dispatched) != 1 || dispatched[0] != "cp1" {
		t.Errorf("rollback dispatched to %v; only cp1 had anything to undo", dispatched)
	}
	assertClusterStillWorks(t, f)
}

var errQuorum = errors.New("etcd members do not agree on a leader — the cluster is partitioned")

func TestForcedRollbackIsHaltSafeAtEveryStep(t *testing.T) {
	for _, phase := range []string{EtcdPhaseReAddRoot, pki.EtcdPhaseRollback} {
		for _, victim := range []string{"cp1", "cp2", "cp3"} {
			t.Run(phase+"/"+victim, func(t *testing.T) {
				f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
				if err := f.migration(nodes, false).run(); err != nil {
					t.Fatalf("precondition: %v", err)
				}
				f.calls = nil
				f.failAt[victim+":"+phase] = true

				if err := f.migration(nodes, false).rollback(true); err == nil {
					t.Fatalf("forced rollback reported success though %s failed %s", victim, phase)
				}
				assertClusterStillWorks(t, f)
			})
		}
	}
}

func TestForcedRollbackWidensEverywhereBeforeAnyLeafMoves(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	f.calls = nil

	if err := f.migration(nodes, false).rollback(true); err != nil {
		t.Fatalf("forced rollback: %v", err)
	}
	want := []string{
		"cp1:" + EtcdPhaseReAddRoot, "cp2:" + EtcdPhaseReAddRoot, "cp3:" + EtcdPhaseReAddRoot,
		"cp1:" + pki.EtcdPhaseRollback, "cp2:" + pki.EtcdPhaseRollback, "cp3:" + pki.EtcdPhaseRollback,
	}
	if strings.Join(f.calls, " ") != strings.Join(want, " ") {
		t.Errorf("forced rollback was not widen-everywhere-first.\n got: %v\nwant: %v", f.calls, want)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateTrustBoth {
			t.Errorf("%s is at %s after a forced rollback, want %s", n, got, pki.EtcdStateTrustBoth)
		}
	}
	assertClusterStillWorks(t, f)
}

func TestForcedRollbackRefusesToMoveALeafIfWideningDidNotTake(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	f.calls = nil
	f.failRestartAt["cp3:"+EtcdPhaseReAddRoot] = true

	err := f.migration(nodes, false).rollback(true)
	if err == nil {
		t.Fatal("forced rollback moved leaves though cp3 never adopted the widened trust")
	}
	if len(phaseCalls(f, pki.EtcdPhaseRollback)) != 0 {
		t.Errorf("a leaf was re-derived despite the widening not taking: %v", f.calls)
	}
	assertClusterStillWorks(t, f)
}

func TestFetchKeyIsRemovedOnEveryExitPath(t *testing.T) {
	for _, tc := range []struct{ name, failAt string }{
		{"success", ""},
		{"halt during trust-both", "cp2:" + pki.EtcdPhaseTrustBoth},
		{"halt during issue-new", "cp2:" + pki.EtcdPhaseIssueNew},
		{"halt during drop-root", "cp2:" + pki.EtcdPhaseDropRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
			if tc.failAt != "" {
				f.failAt[tc.failAt] = true
			}
			_ = f.migration(nodes, false).run()
			if f.published == 0 {
				t.Fatal("precondition: nothing was published")
			}
			if f.cleaned != f.published {
				t.Errorf("published the fetch key %d time(s) but removed it %d — a key that decrypts every cluster private key was left behind",
					f.published, f.cleaned)
			}
		})
	}
}

func TestSingleControlPlaneNeverPublishesTheFetchKey(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if f.published != 0 {
		t.Errorf("published the fetch key %d time(s) on a single-control-plane cluster", f.published)
	}
}

func TestInterruptHaltsOnAPhaseBoundary(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.interrupt = context.Canceled

	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("an interrupted run reported success")
	}
	if len(f.calls) != 0 {
		t.Errorf("an interrupted run dispatched work: %v", f.calls)
	}
	if f.cleaned != f.published {
		t.Errorf("interrupted run published %d fetch key(s) and cleaned %d", f.published, f.cleaned)
	}
	assertClusterStillWorks(t, f)
}

func TestMigrationRunStopsWhenTheMaintenanceLeaseIsLost(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	m := f.migration(nodes, false)
	m.confirmLease = func(phase string) error {
		if strings.HasPrefix(phase, pki.EtcdPhaseIssueNew) {
			return fmt.Errorf("the maintenance lease is no longer ours; stopping before phase %s: %w", phase, maintlease.ErrTaken)
		}
		return nil
	}
	err := m.run()
	if err == nil || !strings.Contains(err.Error(), "no longer ours") {
		t.Fatalf("a run whose maintenance lease was taken over = %v; it must stop naming the lease", err)
	}
	if got := phaseCalls(f, pki.EtcdPhaseIssueNew); len(got) != 0 {
		t.Errorf("issue-new was dispatched after the lease was lost: %v", f.calls)
	}
	if got := phaseCalls(f, pki.EtcdPhaseTrustBoth); len(got) != 3 {
		t.Errorf("trust-both dispatched %d node jobs, want 3 (the phase before the loss): %v", len(got), f.calls)
	}
}

func TestMigrationStopsAfterLeaseLossWithinPhase(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint("rollback=", rollback), func(t *testing.T) {
			f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
			if rollback {
				for i, n := range nodes {
					if err := f.RunPhase(n, pki.EtcdPhaseTrustBoth, i > 0, false); err != nil {
						t.Fatal(err)
					}
				}
				f.calls = nil
			}
			m := f.migration(nodes, false)
			m.confirmLease = func(string) error {
				if len(f.calls) > 0 {
					return maintlease.ErrTaken
				}
				return nil
			}
			states := map[string]string{"cp1": pki.EtcdStateNotMigrated, "cp2": pki.EtcdStateNotMigrated, "cp3": pki.EtcdStateNotMigrated}
			var err error
			if rollback {
				err = m.rollbackPass(states, EtcdPhaseReAddRoot, false, func(string) bool { return true })
			} else {
				err = m.runPhase(pki.EtcdPhaseTrustBoth, states)
			}
			if !errors.Is(err, maintlease.ErrTaken) {
				t.Fatalf("must stop on lease loss: %v", err)
			}
			if len(f.calls) != 1 {
				t.Fatalf("dispatched after lease loss: %v", f.calls)
			}
		})
	}
}

func TestResumeAfterPowerLossMidIssueNew(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	for _, n := range nodes {
		if err := f.RunPhase(n, pki.EtcdPhaseTrustBoth, n.Name != "cp1", false); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes[:2] {
		if err := f.RunPhase(n, pki.EtcdPhaseIssueNew, false, false); err != nil {
			t.Fatal(err)
		}
	}

	f.transientFor["cp3"] = 2
	if err := f.migration(nodes, true).run(); err != nil {
		t.Fatalf("resumed migration with a restarting etcd: %v", err)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateMigrated {
			t.Errorf("%s ended at %s, want %s", n, got, pki.EtcdStateMigrated)
		}
	}
	assertClusterStillWorks(t, f)
}

func TestNonTransientUnknownIsNotRetried(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.unreachable["cp2"] = true

	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration proceeded with cp2 indeterminate")
	}
	for _, n := range f.order {
		if got := f.surveys[n]; got != 1 {
			t.Errorf("%s was surveyed %d times, want exactly 1 (non-transient unknowns are never retried)", n, got)
		}
	}
}

func TestSurveyRetryHonoursInterrupt(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 99
	f.interrupt = errors.New("interrupt requested")

	err := f.migration(nodes, false).run()
	if !errors.Is(err, f.interrupt) {
		t.Fatalf("run() = %v, want the interrupt", err)
	}
	if !strings.Contains(err.Error(), "cp2") {
		t.Errorf("the stop must name the member that was still starting, got: %v", err)
	}
	if got := f.surveys["cp2"]; got != 1 {
		t.Errorf("cp2 was surveyed %d times after the interrupt, want 1 — the run must not sleep through it", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("an interrupted run must dispatch nothing, got: %v", f.calls)
	}
}

func TestSurveyRetryHonoursInterruptDuringTheWait(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 99
	stop := errors.New("interrupt requested")
	m := f.migration(nodes, false)
	m.sleep = func(time.Duration) { f.interrupt = stop }

	err := m.run()
	if !errors.Is(err, stop) {
		t.Fatalf("run() = %v, want the interrupt", err)
	}
	if got := f.surveys["cp2"]; got != 1 {
		t.Errorf("cp2 was surveyed %d times, want 1 — no retry probe may start after the interrupt", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("an interrupted run must dispatch nothing, got: %v", f.calls)
	}
}

func TestSurveyRetryReportsALeaseLostDuringTheFinalWait(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 99
	lost := errors.New("maintenance lease taken by another run")
	held := true
	m := f.migration(nodes, false)
	m.surveyBudget = surveyBackoff(0) // room for exactly one wait
	m.sleep = func(time.Duration) { held = false }
	m.confirmLease = func(string) error {
		if !held {
			return lost
		}
		return nil
	}

	err := m.run()
	if !errors.Is(err, lost) {
		t.Fatalf("run() = %v, want the lease error", err)
	}
	if strings.Contains(err.Error(), "could not be determined") {
		t.Errorf("a lost lease was reported as an undetermined member: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a run that lost its lease must dispatch nothing, got: %v", f.calls)
	}
}

func TestSurveyRetryReportsALostLease(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 99
	lost := errors.New("maintenance lease taken by another run")
	m := f.migration(nodes, false)
	m.confirmLease = func(string) error { return lost }

	err := m.run()
	if !errors.Is(err, lost) {
		t.Fatalf("run() = %v, want the lease error", err)
	}
	if strings.Contains(err.Error(), "could not be determined") {
		t.Errorf("a lost lease was reported as an undetermined member: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a run that lost its lease must dispatch nothing, got: %v", f.calls)
	}
}

func TestMixedUnknownsKeepTheStartupRemedy(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.unreachable["cp2"] = true
	f.transientFor["cp3"] = 1 << 30

	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration proceeded with cp2 and cp3 unknown")
	}
	for _, want := range []string{"cp2", "cp3", "read etcd's log, under /var/log/pods/kube-system_etcd-*", "re-run with --resume once it is serving"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q, got: %v", want, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused run must dispatch nothing, got: %v", f.calls)
	}
}

func TestNotListeningMemberGetsTheRepairRemedy(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp3"] = 1 << 30
	f.notListening["cp3"] = true

	err := f.migration(nodes, false).run()
	if err == nil {
		t.Fatal("migration proceeded with cp3 not listening")
	}
	for _, want := range []string{"cp3's etcd is not listening on its client port", "`sudo /usr/local/bin/stratum certs migrate-etcd-ca --phase=issue-new --yes`", "re-run with --resume once etcd is serving"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "did not answer TLS within the wait") {
		t.Errorf("a not-listening member got the stalled member's steps: %v", err)
	}
}

func TestDropRootGateAndRollbackKeepTheStartupRemedy(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	m := f.migration(nodes, false)
	f.transientFor["cp2"] = 1 << 30
	if err := m.gateDropRoot(); err == nil || !strings.Contains(err.Error(), "read etcd's log, under /var/log/pods/kube-system_etcd-*") {
		t.Errorf("gateDropRoot = %v; want the refusal with the start-up steps", err)
	}
	if err := f.migration(nodes, false).rollback(false); err == nil || !strings.Contains(err.Error(), "read etcd's log, under /var/log/pods/kube-system_etcd-*") {
		t.Errorf("rollback = %v; want the refusal with the start-up steps", err)
	}
}

func TestSurveyInterruptAfterTheLastWait(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 99
	stop := errors.New("interrupt requested")
	m := f.migration(nodes, false)
	m.surveyBudget = surveyBackoff(0) // room for exactly one wait
	survey := 0
	m.drv = interruptAfterSurveys{fakeCluster: f, after: len(nodes) + 1, n: &survey, err: stop}

	if err := m.run(); !errors.Is(err, stop) {
		t.Fatalf("run() = %v, want the interrupt", err)
	}
}

type interruptAfterSurveys struct {
	*fakeCluster
	after int
	n     *int
	err   error
}

func (d interruptAfterSurveys) Survey(n etcdNode) (string, string, bool) {
	*d.n++
	return d.fakeCluster.Survey(n)
}

func (d interruptAfterSurveys) Interrupted() error {
	if *d.n >= d.after {
		return d.err
	}
	return nil
}

func TestSurveyBackoffAndBudget(t *testing.T) {
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second} {
		if got := surveyBackoff(attempt); got != want {
			t.Errorf("surveyBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 1 << 30
	m := f.migration(nodes, false)
	m.surveyBudget = 0 // the production budget
	var waits []time.Duration
	m.sleep = func(d time.Duration) { waits = append(waits, d) }
	_ = m.run()
	var total time.Duration
	for _, w := range waits {
		total += w
	}
	if total > surveyRetryBudget || total < surveyRetryBudget-15*time.Second {
		t.Errorf("pauses %v add up to %s; want within the last step of the %s budget", waits, total, surveyRetryBudget)
	}
}

func TestRecoveredMemberLosesItsStartupReason(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp3"] = 1
	m := f.migration(nodes, false)
	if _, err := m.survey(); err != nil {
		t.Fatal(err)
	}
	if d, ok := m.details["cp3"]; ok {
		t.Errorf("cp3 recovered but kept the reason %q", d)
	}
}

func TestClusterDriverForwardsTransient(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens there now: the dial is refused
	old := probeEtcdTrust
	probeEtcdTrust = func(string) pki.EtcdRemoteTrust { return pki.ProbeEtcdTrustAt(addr) }
	t.Cleanup(func() { probeEtcdTrust = old })

	f, _ := newFakeCluster(t, "cp1")
	var state, detail string
	var transient bool
	f.in("cp1", func() {
		state, detail, transient = (&clusterDriver{}).Survey(etcdNode{Name: "cp1", IP: "127.0.0.1"})
	})
	if state != pki.EtcdStateUnknown || !transient {
		t.Errorf("Survey of a refused port = (%s, %q, %v), want unknown and transient", state, detail, transient)
	}
}

func TestTransientUnknownPastBudgetStillRefuses(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp3"] = 1 << 30 // never starts serving

	for _, resume := range []bool{false, true} {
		f.calls = nil
		err := f.migration(nodes, resume).run()
		if err == nil {
			t.Fatalf("resume=%v: migration proceeded with cp3 still unknown", resume)
		}
		for _, want := range []string{
			"cp3",
			"--resume does NOT override",
			"did not answer TLS within the wait",
			"read etcd's log, under /var/log/pods/kube-system_etcd-*",
			"re-run with --resume once it is serving",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("resume=%v: refusal missing %q, got: %v", resume, want, err)
			}
		}
		if len(f.calls) != 0 {
			t.Errorf("resume=%v: a refused run must dispatch nothing, got: %v", resume, f.calls)
		}
	}
}

func TestSurveyInterruptWithEmptyPendingHasNoEmptyNameList(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	f.transientFor["cp2"] = 1
	f.transientFor["cp3"] = 1
	stop := errors.New("interrupt requested")
	m := f.migration(nodes, false)
	m.surveyBudget = surveyBackoff(0)
	survey := 0
	m.drv = interruptAfterSurveys{fakeCluster: f, after: len(nodes) + 2, n: &survey, err: stop}
	if err := m.run(); !errors.Is(err, stop) {
		t.Fatalf("run() = %v, want the interrupt", err)
	} else {
		if strings.Contains(err.Error(), "on  to") {
			t.Errorf("interrupt message has empty name list: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "to start serving") {
			t.Errorf("interrupt message lost its wording: %q", err.Error())
		}
	}
}

type transientAfterSurveys struct {
	*fakeCluster
	after  int
	n      *int
	target string
}

func (d transientAfterSurveys) Survey(n etcdNode) (string, string, bool) {
	*d.n++
	if n.Name == d.target && *d.n > d.after {
		return pki.EtcdStateUnknown, "could not complete a TLS handshake with etcd at " + n.IP + ": etcd accepted the connection but " + pki.EtcdStalled + " (starting up or not yet back in raft): timeout", true
	}
	return d.fakeCluster.Survey(n)
}

func TestRollbackAfterWideningKeepsTheStartupRemedy(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	for _, n := range f.order {
		if got := f.state(n); got != pki.EtcdStateMigrated {
			t.Fatalf("precondition: %s is at %s, want %s", n, got, pki.EtcdStateMigrated)
		}
	}
	f.calls = nil
	n := 0
	d := transientAfterSurveys{fakeCluster: f, after: len(nodes), n: &n, target: "cp3"}
	m := f.migration(nodes, false)
	m.drv = d
	m.sleep = func(time.Duration) {}
	err := m.rollback(true)
	if err == nil {
		t.Fatal("rollback proceeded though cp3 still had not adopted the widened trust")
	}
	for _, want := range []string{"cp3", "refusing to re-derive", "read etcd's log, under /var/log/pods/kube-system_etcd-*", "re-run with --resume once it is serving"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q, got: %v", want, err)
		}
	}
}
