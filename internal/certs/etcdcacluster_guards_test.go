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

func TestForcedRollbackRefusesUnknownAfterWidening(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2", "cp3")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("precondition (migrate): %v", err)
	}
	f.unreachableAfter["cp2:"+EtcdPhaseReAddRoot] = true
	f.calls = nil

	err := f.migration(nodes, false).rollback(true)
	if err == nil {
		t.Fatal("pass 2 proceeded though cp2's adoption of the widened trust was unconfirmed (unknown)")
	}
	if !strings.Contains(err.Error(), "cp2") || !strings.Contains(err.Error(), "could not be determined") {
		t.Errorf("the refusal must name cp2 as indeterminate, got: %v", err)
	}
	if got := phaseCalls(f, pki.EtcdPhaseRollback); len(got) != 0 {
		t.Errorf("pass 2 dispatched a leaf move despite an unconfirmed node: %v", got)
	}
}

func TestResumeRefusesNonAdjacentStates(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	for _, phase := range EtcdCAPhases {
		if err := f.RunPhase(nodes[0], phase, false, false); err != nil {
			t.Fatalf("precondition %s on cp1: %v", phase, err)
		}
	}
	if f.state("cp1") != pki.EtcdStateMigrated || f.state("cp2") != pki.EtcdStateNotMigrated {
		t.Fatalf("precondition: states are cp1=%s cp2=%s, want migrated/not-migrated", f.state("cp1"), f.state("cp2"))
	}

	err := f.migration(nodes, true).run()
	if err == nil {
		t.Fatal("resume proceeded across non-adjacent states (migrated + not-migrated)")
	}
	if !strings.Contains(err.Error(), "non-adjacent") {
		t.Errorf("want a non-adjacent-states refusal, got: %v", err)
	}
}

func TestResumeAllowsAdjacentStates(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1", "cp2")
	if err := f.RunPhase(nodes[0], pki.EtcdPhaseTrustBoth, false, false); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if err := f.migration(nodes, true).run(); err != nil {
		t.Fatalf("resume across adjacent states was refused: %v", err)
	}
	assertClusterStillWorks(t, f)
}

func TestPhaseRestartsExpectedComponents(t *testing.T) {
	f, nodes := newFakeCluster(t, "cp1")
	if err := f.migration(nodes, false).run(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
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
