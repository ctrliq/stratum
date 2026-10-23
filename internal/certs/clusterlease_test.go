package certs

// The cluster maintenance fence for the two --cluster cert orchestrators.
// `rotate-ca --cluster` (RotateCACluster) and `migrate-etcd-ca --cluster`
// (MigrateEtcdCACluster) both dispatch privileged per-node Jobs that rewrite
// CA material and restart control-plane components on every node, so a second
// cluster orchestrator running beside either is the interleaving the
// phase-major design exists to prevent. Both take a shared
// cluster-wide maintenance Lease — kube-system/stratum-cluster-maintenance,
// the internal/maintlease machinery the cluster upgrade takes under its own
// released name — acquired before the run's first act on the cluster and held
// through the final restart/readiness waits, released on every exit including
// a Ctrl-C abort (the run's context cancels, the step returns, the defers
// fire).
//
// These tests drive the real entry points on the fake clientset behind the
// adminClientset seam — the seam MigrateEtcdCACluster already takes and
// RotateCACluster must take the same way — with the fixtures the lock-guard
// tests use (prep, redirectVar, readLeaf). The initiator's own phases run
// in-process against the fixture PKI, where they stop at this node's missing
// kubelet kubeconfig (no restarts, no etcd); what the tests pin is everything
// around that fence:
//
//   - a live shared lease held by another run refuses each orchestrator,
//     naming the holder and the lease, before any change — no staged key
//     Secret, no per-node Job, the node's CA material byte-identical;
//   - a free cluster sees the lease created before the run's first phase and
//     deleted again on the way out, whatever the run's own error;
//   - the pre-existing migration lock Secret still serializes two
//     migrations, and a refused run never leaves one behind.
//
// The lease name is pinned as a literal, the way released lock identities are
// pinned elsewhere; the constants below are the test's own.

import (
	"io"

	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	fenceTestMaintenanceNS    = "kube-system"
	fenceTestMaintenanceLease = "stratum-cluster-maintenance"
)

// fakeAdminClientset points the adminClientset seam at a fake clientset
// seeded with objs, so the cluster orchestrators run on it instead of
// admin.conf.
func fakeAdminClientset(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	old := adminClientset
	adminClientset = func() (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { adminClientset = old })
	return cs
}

// certNodeFixture is a Node for the orchestrators' classification: labeled a
// control plane when cp. No InternalIP: the migration's survey would dial it,
// so a fixture run must halt at the node roster, before any node is probed.
func certNodeFixture(name string, cp bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if cp {
		n.Labels[k8sutil.ControlPlaneLabel] = "true"
	}
	return n
}

// liveMaintenanceLease is the shared maintenance lease, held live by holder.
func liveMaintenanceLease(holder string) *coordinationv1.Lease {
	h := holder
	now := metav1.NewMicroTime(time.Now())
	dur := int32(60)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: fenceTestMaintenanceLease, Namespace: fenceTestMaintenanceNS},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &h, LeaseDurationSeconds: &dur, RenewTime: &now},
	}
}

// countMaintenanceLeaseActions reads what a run did: how many times it took
// and released the shared lease, and how many per-node Jobs it created.
func countMaintenanceLeaseActions(t *testing.T, cs *fake.Clientset) (creates, deletes, jobCreates int) {
	t.Helper()
	for _, a := range cs.Actions() {
		switch {
		case a.GetVerb() == "create" && a.GetResource().Resource == "leases":
			if ca, ok := a.(clienttesting.CreateAction); ok {
				if l, ok := ca.GetObject().(*coordinationv1.Lease); ok && l.Name == fenceTestMaintenanceLease {
					creates++
				}
			}
		case a.GetVerb() == "delete" && a.GetResource().Resource == "leases":
			if da, ok := a.(clienttesting.DeleteAction); ok && da.GetName() == fenceTestMaintenanceLease {
				deletes++
			}
		case a.GetVerb() == "create" && a.GetResource().Resource == "jobs":
			jobCreates++
		}
	}
	return creates, deletes, jobCreates
}

// readStagedCARotation reads the node's CA material, for the byte-identical
// assertion that a refused run changed nothing on this node.
func readStagedCARotation(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"ca.crt", "ca-bundle.crt"} {
		out[name] = readLeaf(t, name)
	}
	return out
}

func assertCARotationUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for name, want := range before {
		if got := readLeaf(t, name); !bytes.Equal(want, got) {
			t.Errorf("%s changed during a refused --cluster run", name)
		}
	}
}

// assertNoNodeJob fails if the run created any per-node Job.
func assertNoNodeJob(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			t.Errorf("a per-node Job was created although the run must refuse first: %v", a)
		}
	}
}

// thisCPNode seeds the fake with this host as a labeled control plane and one
// more control plane, the smallest cluster the --cluster entry points accept.
func thisCPNode(t *testing.T) []runtime.Object {
	t.Helper()
	me, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname")
	}
	return []runtime.Object{
		certNodeFixture(me, true),
		certNodeFixture("cp2", true),
	}
}

// TestRotateCAClusterRefusesWhileTheMaintenanceLeaseIsHeld: an upgrade, a
// migration or another rotate-ca holding the shared lease refuses the
// orchestrator — naming the holder — before any change: the node's CA
// material is byte-identical afterwards, no transient key Secret is staged,
// and no per-node Job is dispatched.
func TestRotateCAClusterRefusesWhileTheMaintenanceLeaseIsHeld(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf") // the seam must answer; admin.conf must not be read
	objs := append(thisCPNode(t), liveMaintenanceLease("another-run/47/2"))
	cs := fakeAdminClientset(t, objs...)

	before := readStagedCARotation(t)
	err := RotateCACluster(CARotateOptions{Quiet: true})
	if err == nil {
		t.Fatal("rotate-ca --cluster ran while another run held " + fenceTestMaintenanceNS + "/" + fenceTestMaintenanceLease)
	}
	for _, want := range []string{fenceTestMaintenanceLease, "another-run/47/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	assertCARotationUnchanged(t, before)
	if _, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a refused run staged the transient key Secret: %v", err)
	}
	assertNoNodeJob(t, cs)
}

// TestRotateCAClusterRefusesALostPublicHalfUpFront: a lost front-proxy-ca.crt
// is refused before the run starts, so the operator is not told to roll back
// a rotation that never began.
func TestRotateCAClusterRefusesALostPublicHalfUpFront(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf")
	cs := fakeAdminClientset(t, thisCPNode(t)...)
	if err := os.Remove(filepath.Join(version.PKIDir, "front-proxy-ca.crt")); err != nil {
		t.Fatal(err)
	}
	before := readStagedCARotation(t)
	err := RotateCACluster(CARotateOptions{Force: true, Quiet: true})
	if !errors.Is(err, pki.ErrNoOldPublicHalf) {
		t.Fatalf("RotateCACluster = %v, want ErrNoOldPublicHalf", err)
	}
	if strings.Contains(err.Error(), "--rollback") {
		t.Errorf("the refusal offers --rollback for a rotation that never started: %v", err)
	}
	assertCARotationUnchanged(t, before)
	if creates, _, _ := countMaintenanceLeaseActions(t, cs); creates != 0 {
		t.Error("the refused run took the maintenance lease")
	}
	assertNoNodeJob(t, cs)
}

// TestRotateCAClusterTakesTheMaintenanceLeaseBeforeItsFirstChange: on a free
// cluster the orchestrator takes the shared lease before its first phase and
// releases it on the way out — here on the error exit the fixture run ends
// with, which is every exit the fence must cover that is not a refusal.
func TestRotateCAClusterTakesTheMaintenanceLeaseBeforeItsFirstChange(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf") // the seam must answer; admin.conf must not be read
	cs := fakeAdminClientset(t, thisCPNode(t)...)

	err := RotateCACluster(CARotateOptions{Quiet: true})
	if err == nil {
		t.Fatal("the fixture run should halt at the initiator's own phase (this node runs no kubelet)")
	}
	if strings.Contains(err.Error(), "in progress") {
		t.Fatalf("the run refused although the cluster was free: %v", err)
	}
	creates, deletes, jobCreates := countMaintenanceLeaseActions(t, cs)
	if creates != 1 {
		t.Errorf("RotateCACluster created the maintenance lease %d time(s); it must take %s/%s before its first phase",
			creates, fenceTestMaintenanceNS, fenceTestMaintenanceLease)
	}
	if deletes != 1 {
		t.Errorf("the maintenance lease was released %d time(s) on the way out; the run must release it on every exit", deletes)
	}
	if jobCreates != 0 {
		t.Errorf("%d per-node Job(s) were created; the fixture run must halt before any node Job", jobCreates)
	}
	if _, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a halted run left the transient key Secret behind: %v", err)
	}
}

// TestMigrateEtcdCAClusterRefusesWhileTheMaintenanceLeaseIsHeld: the
// migration refuses — naming the holder — before it surveys or dispatches
// anything, and a refused run never leaves the migration lock Secret behind,
// whether the lease check came before or after the lock.
func TestMigrateEtcdCAClusterRefusesWhileTheMaintenanceLeaseIsHeld(t *testing.T) {
	prep(t)
	redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes")) // no etcd manifest: the node phases would refuse "runs no etcd"
	objs := append(thisCPNode(t), liveMaintenanceLease("another-run/47/2"))
	cs := fakeAdminClientset(t, objs...)

	err := MigrateEtcdCACluster(EtcdCAOptions{Quiet: true})
	if err == nil {
		t.Fatal("migrate-etcd-ca --cluster ran while another run held " + fenceTestMaintenanceNS + "/" + fenceTestMaintenanceLease)
	}
	for _, want := range []string{fenceTestMaintenanceLease, "another-run/47/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if _, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), etcdMigrationLockSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a refused run left the migration lock Secret behind: %v", err)
	}
	assertNoNodeJob(t, cs)
}

// TestMigrateEtcdCAClusterTakesTheMaintenanceLeaseBeforeItsFirstChange: on a
// free cluster the migration takes the shared lease before its run begins —
// the survey is its first act on the cluster and the per-node Jobs follow it —
// and releases it on the way out, here on the error exit the fixture run ends
// with.
func TestMigrateEtcdCAClusterTakesTheMaintenanceLeaseBeforeItsFirstChange(t *testing.T) {
	prep(t)
	redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes")) // no etcd manifest: the node phases would refuse "runs no etcd"
	cs := fakeAdminClientset(t, thisCPNode(t)...)

	err := MigrateEtcdCACluster(EtcdCAOptions{Quiet: true})
	if err == nil {
		t.Fatal("the fixture run should halt when no running etcd answers its survey")
	}
	if strings.Contains(err.Error(), "in progress") {
		t.Fatalf("the run refused although the cluster was free: %v", err)
	}
	creates, deletes, jobCreates := countMaintenanceLeaseActions(t, cs)
	if creates != 1 {
		t.Errorf("MigrateEtcdCACluster created the maintenance lease %d time(s); it must take %s/%s before its first phase",
			creates, fenceTestMaintenanceNS, fenceTestMaintenanceLease)
	}
	if deletes != 1 {
		t.Errorf("the maintenance lease was released %d time(s) on the way out; the run must release it on every exit", deletes)
	}
	if jobCreates != 0 {
		t.Errorf("%d per-node Job(s) were created; the fixture run must halt before any node Job", jobCreates)
	}
}

// TestMigrationLockSecretStillSerializesMigrations pins the pre-existing
// guard: a second migration refuses on the create-if-absent lock Secret,
// naming its holder, and the refused run leaves another run's lock Secret in
// place. Green before the lease wiring lands, so any change it reports is the
// wiring's.
func TestMigrationLockSecretStillSerializesMigrations(t *testing.T) {
	prep(t)
	redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
	holder, since, token := "operator-2", "2026-10-12T08:00:00Z", "theirs"
	cs := fakeAdminClientset(t, append(thisCPNode(t), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: etcdMigrationLockSecret, Namespace: caNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"holder": []byte(holder),
			"since":  []byte(since),
			"token":  []byte(token),
		},
	})...)

	err := MigrateEtcdCACluster(EtcdCAOptions{Quiet: true})
	if err == nil {
		t.Fatal("a second migration ran although the lock Secret is held")
	}
	for _, want := range []string{"operator-2", etcdMigrationLockSecret} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), etcdMigrationLockSecret, metav1.GetOptions{})
	if err != nil || string(sec.Data["holder"]) != holder || string(sec.Data["token"]) != token {
		t.Fatalf("the refused run removed or rewrote another run's lock Secret: %v, %v", sec, err)
	}
	assertNoNodeJob(t, cs)
}

// TestConfirmMaintenanceLeasePolicy pins the phase-boundary policy both
// cluster orchestrators share: a takeover stops the run naming the lease; a
// renewal answer that retrying cannot change stops immediately; a
// healthy renewal passes.
func TestConfirmMaintenanceLeasePolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("a takeover stops the run", func(t *testing.T) {
		cs := fakeAdminClientset(t)
		_, guard, release, err := acquireMaintenanceLease(ctx, cs, "m1/100/1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		// A successor takes the lease: live holder, fresh renew time.
		l, err := cs.CoordinationV1().Leases(caNamespace).Get(ctx, maintenanceLeaseName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		successor := "successor/200/2"
		l.Spec.HolderIdentity = &successor
		if _, err := cs.CoordinationV1().Leases(caNamespace).Update(ctx, l, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		err = confirmMaintenanceLease(ctx, guard, "phase trust-both")
		if !errors.Is(err, maintlease.ErrTaken) || !strings.Contains(err.Error(), "stopping before") {
			t.Fatalf("a confirm against a taken-over lease = %v; it must stop the run naming the lease", err)
		}
	})

	t.Run("a deleted lease stops the run", func(t *testing.T) {
		cs := fakeAdminClientset(t)
		_, guard, release, err := acquireMaintenanceLease(ctx, cs, "m1/100/1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		// A deleted lease is a NotFound renewal: retrying cannot change it, so
		// Confirm returns it as it came and the policy stops immediately.
		if err := cs.Tracker().Delete(coordinationv1.SchemeGroupVersion.WithResource("leases"), caNamespace, maintenanceLeaseName); err != nil {
			t.Fatal(err)
		}
		if err := confirmMaintenanceLease(ctx, guard, "phase trust-both"); !apierrors.IsNotFound(err) {
			t.Fatalf("a NotFound renewal must stop, got: %v", err)
		}
	})

	t.Run("a healthy renewal passes", func(t *testing.T) {
		cs := fakeAdminClientset(t)
		_, guard, release, err := acquireMaintenanceLease(ctx, cs, "m1/100/1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		if err := confirmMaintenanceLease(ctx, guard, "phase trust-both"); err != nil {
			t.Fatalf("a healthy confirm: %v", err)
		}
	})
}

func TestCertClusterOperationsRefuseReleasedUpgrade(t *testing.T) {
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"rotate-ca", func() error { return RotateCACluster(CARotateOptions{Quiet: true}) }},
		{"migrate-etcd-ca", func() error { return MigrateEtcdCACluster(EtcdCAOptions{Quiet: true}) }},
	} {
		t.Run(op.name, func(t *testing.T) {
			prep(t)
			lease := liveMaintenanceLease("released-upgrade/123/1")
			lease.Name = "stratum-cluster-upgrade"
			cs := fakeAdminClientset(t, append(thisCPNode(t), lease)...)
			before := readStagedCARotation(t)
			err := op.run()
			if err == nil || !strings.Contains(err.Error(), "released-upgrade/123/1") {
				t.Fatalf("must refuse released upgrade: %v", err)
			}
			assertCARotationUnchanged(t, before)
			assertNoNodeJob(t, cs)
		})
	}
}

func TestLeafRenewalRefusesClusterMaintenance(t *testing.T) {
	for _, window := range []int{0, 400} {
		t.Run(fmt.Sprint("window=", window), func(t *testing.T) {
			kubeVIPNode(t)
			cs := fakeAdminClientset(t, liveMaintenanceLease("rotation-on-other-node"))
			before := readLeaf(t, "kube-vip.crt")
			_, err := Rotate(io.Discard, Options{Quiet: true, IfExpiringDays: window})
			if err == nil || !strings.Contains(err.Error(), "rotation-on-other-node") {
				t.Fatalf("renewal must refuse cluster maintenance: %v", err)
			}
			if !bytes.Equal(before, readLeaf(t, "kube-vip.crt")) {
				t.Fatal("renewal changed leaves under another cluster holder")
			}
			assertNoNodeJob(t, cs)
		})
	}
}
