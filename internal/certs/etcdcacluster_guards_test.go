package certs

import (
	"io"

	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestForcedRollbackRefusesUnknownAfterWidening pins the rollback
// pass-2 rule: after pass 1 (re-add-root) widens trust everywhere, a node that
// went indeterminate DURING that pass (its etcd flapped on the restart) must not
// be admitted to pass 2, which moves leaves. Moving a leaf while another member's
// adoption of the widened trust is unproven is the raft eviction the two-pass
// design exists to prevent.
func TestForcedRollbackRefusesUnknownAfterWidening(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition (migrate): %v", err)
	}
	// cp2's etcd goes unreachable right after its re-add-root restart, so the
	// post-pass-1 re-survey reads it as unknown rather than widened.
	f.unreachableAfter["cp2:"+EtcdPhaseReAddRoot] = true
	f.calls = nil

	err := f.migration(nodes, false).rollback(true)
	if err == nil {
		t.Fatal("pass 2 proceeded though cp2's adoption of the widened trust was unconfirmed (unknown)")
	}
	if !strings.Contains(err.Error(), "cp2") || !strings.Contains(err.Error(), "could not be determined") {
		t.Errorf("the refusal must name cp2 as indeterminate, got: %v", err)
	}
	// The safety property: no leaf-move (rollback phase) dispatched to anyone.
	if got := phaseCalls(f, pki.EtcdPhaseRollback); len(got) != 0 {
		t.Errorf("pass 2 dispatched a leaf move despite an unconfirmed node: %v", got)
	}
}

// TestResumeRefusesNonAdjacentStates pins the --resume rule:
// --resume may only continue a HALTED run, which spans at most two adjacent
// phases. A wider spread (one member migrated while another never started) is an
// already-split cluster, not a paused one, and must be refused even with --resume.
func TestResumeRefusesNonAdjacentStates(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	// Drive cp1 alone all the way to migrated; leave cp2 untouched (not-migrated).
	for _, phase := range EtcdCAPhases {
		if err := f.RunPhase(nodes[0], phase, false, false); err != nil {
			t.Fatalf("precondition %s on cp1: %v", phase, err)
		}
	}
	if f.state("cp1") != pki.EtcdStateMigrated || f.state("cp2") != pki.EtcdStateNotMigrated {
		t.Fatalf("precondition: states are cp1=%s cp2=%s, want migrated/not-migrated", f.state("cp1"), f.state("cp2"))
	}

	err := f.migration(nodes, true).run() // resume=true
	if err == nil {
		t.Fatal("resume proceeded across non-adjacent states (migrated + not-migrated)")
	}
	if !strings.Contains(err.Error(), "non-adjacent") {
		t.Errorf("want a non-adjacent-states refusal, got: %v", err)
	}
}

// TestResumeAllowsAdjacentStates is the companion: a genuinely halted run (two
// ADJACENT phases) must still be resumable, so the adjacency guard does not
// over-refuse.
func TestResumeAllowsAdjacentStates(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	// cp1 completed trust-both; cp2 not yet — adjacent (not-migrated/trust-both),
	// exactly what a run halted partway through trust-both leaves.
	if err := f.RunPhase(nodes[0], pki.EtcdPhaseTrustBoth, false, false); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if err := f.migration(nodes, true).run(); err != nil {
		t.Fatalf("resume across adjacent states was refused: %v", err)
	}
	assertClusterStillWorks(t, f)
}

// TestPhaseRestartsExpectedComponents pins the per-phase restart contract
// (etcdMigrationRestart): issue-new must bounce kube-apiserver after etcd so the
// apiserver adopts its new etcd-client leaf; trust-both and drop-root bounce only
// etcd; and no phase may bounce kubelet/kube-vip/controller-manager/scheduler.
func TestPhaseRestartsExpectedComponents(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// trust-both -> etcd; issue-new -> etcd, kube-apiserver; drop-root -> etcd.
	want := []string{"etcd", "etcd", "kube-apiserver", "etcd"}
	if got := f.restarts["cp1"]; !slices.Equal(got, want) {
		t.Errorf("restart sequence = %v, want %v", got, want)
	}
	for _, forbidden := range []string{"kubelet", "kube-vip", "kube-controller-manager", "kube-scheduler"} {
		if slices.Contains(f.restarts["cp1"], forbidden) {
			t.Errorf("%s was restarted by a migration phase; only etcd and kube-apiserver may be bounced", forbidden)
		}
	}
}

// TestAcquireMigrationLockSerializes pins the concurrency rule: a
// second `migrate-etcd-ca --cluster` cannot hold the lock while the first does,
// and once released a fresh run can take it.
func TestAcquireMigrationLockSerializes(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()

	release, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp2"); err == nil {
		t.Fatal("a second concurrent migration acquired the lock; two runs can restart different members at once and lose quorum")
	} else if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("want an in-progress refusal, got: %v", err)
	}

	release()
	rel2, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp3")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	rel2()
}

// TestMigrationRefusesOnANodeMidRotation pins the migration's fence against a
// CA rotation in flight on THIS node. Both non-idle answers of RotationPhase —
// a staged new CA, and a trust bundle holding two CAs — must refuse before the
// migration lock Secret or any node Job exists. A halted rotation holds no
// lock, so a fence that trusts only a lock or Lease would let a migration start
// on a node whose bundle still holds two roots.
func TestMigrationRefusesOnANodeMidRotation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T)
		phase string
	}{
		{"staged new CA", func(t *testing.T) {
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
		}, "trust-new"},
		{"trust bundle with two CAs", func(t *testing.T) {
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			if err := pki.PromoteCA(); err != nil {
				t.Fatal(err)
			}
			if err := pki.PromoteSA(); err != nil {
				t.Fatal(err)
			}
		}, "sign-new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origPKI, origSeam := version.PKIDir, adminClientset
			t.Cleanup(func() { version.PKIDir, adminClientset = origPKI, origSeam })
			version.PKIDir = t.TempDir()
			cs := k8sfake.NewSimpleClientset()
			adminClientset = func() (kubernetes.Interface, error) { return cs, nil }

			if err := pki.GenerateAll("cp1", "192.168.99.12", nil); err != nil {
				t.Fatal(err)
			}
			tc.stage(t)
			if got := RotationPhase(); !strings.Contains(got, tc.phase) {
				t.Fatalf("fixture reports RotationPhase %q, want %s; the test proves nothing", got, tc.phase)
			}

			err := MigrateEtcdCACluster(EtcdCAOptions{Quiet: true})
			if err == nil {
				t.Fatal("a migration started on a node whose own CA rotation is in flight")
			}
			if !strings.Contains(err.Error(), "rotation is in flight") {
				t.Errorf("err = %v, want the in-flight-rotation refusal", err)
			}
			if hasMigrationLock(t, cs) {
				t.Error("the refusal left the migration lock Secret behind")
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() == "create" {
					t.Errorf("the refusal created a %s; no Secret or Job may leave this path", a.GetResource().Resource)
				}
			}
		})
	}
}
