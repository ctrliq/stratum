package certs

// Pairwise contention acceptance: the four maintenance operations refuse one another.

import (
	"io"

	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Fails unless err is the operation-naming node-lock refusal naming the holder pid.
func nodeLockRefusal(t *testing.T, err error, holder string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the operation-naming node-lock refusal, got nil")
	}
	for _, want := range []string{
		"already running on this node",
		"nothing was changed",
		holder,
		strconv.Itoa(os.Getpid()),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
}

func TestMaintenanceOperationsRefuseEachOtherOnTheHost(t *testing.T) {
	holders := []struct {
		what string
		op   string // the operation string the entry records on the lock
	}{
		{what: "the cluster upgrade", op: "upgrade"},
		{what: "leaf renewal", op: "certs rotate"},
		{what: "root-CA rotation", op: "certs rotate-ca"},
		{what: "the etcd-CA migration", op: "certs migrate-etcd-ca"},
	}
	refusers := []struct {
		what     string
		op       string
		setup    func(t *testing.T)
		snapshot func(t *testing.T) func(t *testing.T) // nothing-changed proof
		run      func() error
	}{
		{
			what: "leaf renewal", op: "certs rotate",
			setup: func(t *testing.T) { kubeVIPNode(t) },
			snapshot: func(t *testing.T) func(t *testing.T) {
				before := readLeaf(t, "kube-vip.crt")
				return func(t *testing.T) {
					if after := readLeaf(t, "kube-vip.crt"); !bytes.Equal(before, after) {
						t.Error("the refused renewal reissued the kube-vip leaf")
					}
				}
			},
			run: func() error {
				_, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true})
				return err
			},
		},
		{
			what: "root-CA rotation", op: "certs rotate-ca",
			setup: func(t *testing.T) { stagedCPNode(t) },
			snapshot: func(t *testing.T) func(t *testing.T) {
				before := stagedRotationFiles(t)
				return func(t *testing.T) { assertStagedRotationUnchanged(t, before) }
			},
			run: func() error { return RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true}) },
		},
		{
			what: "the etcd-CA migration", op: "certs migrate-etcd-ca",
			setup: func(t *testing.T) {
				t.Setenv("NODE_IP", "10.0.0.9") // hostNodeIP's fallback with no node state
				redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
			},
			snapshot: func(t *testing.T) func(t *testing.T) { return func(t *testing.T) {} },
			run: func() error {
				return MigrateEtcdCA(io.Discard, EtcdCAOptions{Phase: pki.EtcdPhaseTrustBoth, Quiet: true})
			},
		},
	}

	for _, h := range holders {
		for _, r := range refusers {
			if r.op == h.op {
				continue // an operation does not contend with itself here
			}
			t.Run(r.what+" refuses while "+h.what+" holds the node", func(t *testing.T) {
				r.setup(t)
				redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
				release, err := nodelock.Acquire(h.op)
				if err != nil {
					t.Fatalf("hold the node lock as %q: %v", h.op, err)
				}
				t.Cleanup(release)

				after := r.snapshot(t)
				err = r.run()
				nodeLockRefusal(t, err, h.op)
				after(t)
			})
		}
	}
}

// kube-system/name held live by holder, renewed now; also seeds the released upgrade lease.
func liveHeldLease(name, holder string) *coordinationv1.Lease {
	h := holder
	now := metav1.NewMicroTime(time.Now())
	dur := int32(60)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fenceTestMaintenanceNS},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &h, LeaseDurationSeconds: &dur, RenewTime: &now},
	}
}

func TestMaintenanceOperationsRefuseEachOtherOnTheCluster(t *testing.T) {
	holders := []struct {
		what string
		id   string
		both bool   // a live upgrade also holds the released lease
		seed string // the released lease's name, when both
	}{
		{what: "the cluster upgrade", id: "upgrade/4711/1", both: true, seed: "stratum-cluster-upgrade"},
		{what: "rotate-ca --cluster", id: "rotate-ca --cluster/4712/2"},
		{what: "migrate-etcd-ca --cluster", id: "migrate-etcd-ca --cluster/4713/3"},
	}
	refusers := []struct {
		what  string
		setup func(t *testing.T)
		run   func() error
		write string // the cluster write a refused run must not have made
	}{
		{
			what:  "rotate-ca --cluster",
			setup: func(t *testing.T) { redirectVar(t, &version.AdminConf, "missing-admin.conf") },
			run:   func() error { return RotateCACluster(CARotateOptions{Quiet: true}) },
			write: caRotateKeySecret,
		},
		{
			what: "migrate-etcd-ca --cluster",
			setup: func(t *testing.T) {
				redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
			},
			run:   func() error { return MigrateEtcdCACluster(EtcdCAOptions{Quiet: true}) },
			write: etcdMigrationLockSecret,
		},
	}

	for _, h := range holders {
		for _, r := range refusers {
			t.Run(r.what+" refuses while "+h.what+" holds the lease", func(t *testing.T) {
				prep(t)
				r.setup(t)
				objs := append(thisCPNode(t), liveMaintenanceLease(h.id))
				if h.both {
					objs = append(objs, liveHeldLease(h.seed, h.id))
				}
				cs := fakeAdminClientset(t, objs...)

				err := r.run()
				if err == nil {
					t.Fatalf("%s ran while %s held %s/%s", r.what, h.id, fenceTestMaintenanceNS, fenceTestMaintenanceLease)
				}
				refusingLease := fenceTestMaintenanceLease
				if h.both {
					refusingLease = h.seed
				}
				for _, want := range []string{refusingLease, h.id} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal missing %q: %v", want, err)
					}
				}
				if _, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), r.write, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Errorf("the refused run staged %s: %v", r.write, err)
				}
				assertNoNodeJob(t, cs)
			})
		}
	}
}

func TestReleasedNodeLockHolderExcludesTheNewCertOperations(t *testing.T) {
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	// The released holder: raw flock, pid-only file, no operation recorded.
	f, err := os.OpenFile(version.NodeUpgradeLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold a raw flock the way a released binary does: %v", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() })

	t.Run("leaf renewal refuses", func(t *testing.T) {
		kubeVIPNode(t)
		before := readLeaf(t, "kube-vip.crt")
		_, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true})
		nodeLockRefusal(t, err, "pid "+strconv.Itoa(os.Getpid()))
		if after := readLeaf(t, "kube-vip.crt"); !bytes.Equal(before, after) {
			t.Error("the refused renewal reissued the kube-vip leaf")
		}
	})
	t.Run("root-CA rotation refuses", func(t *testing.T) {
		stagedCPNode(t)
		before := stagedRotationFiles(t)
		err := RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
		nodeLockRefusal(t, err, "pid "+strconv.Itoa(os.Getpid()))
		assertStagedRotationUnchanged(t, before)
	})
	t.Run("the etcd-CA migration refuses", func(t *testing.T) {
		t.Setenv("NODE_IP", "10.0.0.9")
		redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
		err := MigrateEtcdCA(io.Discard, EtcdCAOptions{Phase: pki.EtcdPhaseTrustBoth, Quiet: true})
		nodeLockRefusal(t, err, "pid "+strconv.Itoa(os.Getpid()))
	})
}

func TestLeafRenewalUnderARotateCAHolderHoldsRatherThanReacquires(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	release, err := nodelock.Acquire("certs rotate-ca")
	if err != nil {
		t.Fatalf("hold the node lock as the CA phase does: %v", err)
	}
	defer release()

	before := readLeaf(t, "kube-vip.crt")
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true, NodeLockHeld: true}); err != nil {
		t.Fatalf("a nested leaf renewal refused against its own holder's lock: %v", err)
	}
	if after := readLeaf(t, "kube-vip.crt"); bytes.Equal(before, after) {
		t.Fatal("the nested renewal reissued nothing; the test proves nothing")
	}

	_, err = RotateContext(context.Background(), io.Discard, Options{Quiet: true}) // without the capability: the reacquire path
	nodeLockRefusal(t, err, "certs rotate-ca")

	release()
	rel2, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatalf("the node lock is still held after the holder released it: %v", err)
	}
	rel2()
}

// A --cluster run holds the shared lease before reaching the host fence.
func TestClusterFenceIsTakenBeforeTheHostFence(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf") // the seam must answer; admin.conf must not be read
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	holdNodeLock(t) // an upgrade holds this host, exactly the contention below it

	cs := fakeAdminClientset(t, thisCPNode(t)...)
	err := RotateCACluster(CARotateOptions{Quiet: true})
	if err == nil {
		t.Fatal("rotate-ca --cluster ran while an upgrade held this host's node lock")
	}
	nodeLockRefusal(t, err, "upgrade")

	creates, deletes, jobCreates := countMaintenanceLeaseActions(t, cs)
	if creates != 1 {
		t.Errorf("the run took the shared lease %d time(s); the cluster fence must be held before the host fence is met", creates)
	}
	if deletes != 1 {
		t.Errorf("the shared lease was released %d time(s) on the refused exit; the run must release it on every exit", deletes)
	}
	if jobCreates != 0 {
		t.Errorf("%d per-node Job(s) were created; a host-level refusal must dispatch nothing", jobCreates)
	}
	if _, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the refused run staged the transient key Secret: %v", err)
	}
}

func TestStagedRotationRefusalsAreRetained(t *testing.T) {
	t.Run("the node-side phase refuses a staged rotation", func(t *testing.T) {
		stagedCPNode(t)
		redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))

		before := stagedRotationFiles(t)
		err := RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
		if err == nil {
			t.Fatal("trust-new ran on a node with a rotation already staged")
		}
		for _, want := range []string{"already staged on", "--rollback"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("staged-rotation refusal missing %q: %v", want, err)
			}
		}
		assertStagedRotationUnchanged(t, before)
	})
	t.Run("rotate-ca --cluster refuses a staged rotation before taking the lease", func(t *testing.T) {
		stagedCPNode(t)
		cs := fakeAdminClientset(t, append(thisCPNode(t), liveMaintenanceLease("another-run/47/2"))...)

		err := RotateCACluster(CARotateOptions{Quiet: true})
		if err == nil {
			t.Fatal("rotate-ca --cluster ran on a node with a rotation already staged")
		}
		if !strings.Contains(err.Error(), "already staged on") {
			t.Errorf("want the staged-rotation refusal, got: %v", err)
		}
		if creates, _, _ := countMaintenanceLeaseActions(t, cs); creates != 0 {
			t.Errorf("the state-refused run took the shared lease %d time(s); the staged guard must precede the fence", creates)
		}
	})
	t.Run("migrate-etcd-ca --cluster refuses a rotation in flight before taking the lease", func(t *testing.T) {
		stagedCPNode(t)
		cs := fakeAdminClientset(t, append(thisCPNode(t), liveMaintenanceLease("another-run/47/2"))...)

		err := MigrateEtcdCACluster(EtcdCAOptions{Quiet: true})
		if err == nil {
			t.Fatal("migrate-etcd-ca --cluster ran while a CA rotation was in flight")
		}
		if !strings.Contains(err.Error(), "rotation is in flight") {
			t.Errorf("want the rotation-in-flight refusal, got: %v", err)
		}
		if creates, _, _ := countMaintenanceLeaseActions(t, cs); creates != 0 {
			t.Errorf("the state-refused run took the shared lease %d time(s); the rotation guard must precede the fence", creates)
		}
	})
}

func TestRollbackCARefusesWhileAMaintenanceOperationHoldsTheNodeLock(t *testing.T) {
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	f, err := os.OpenFile(version.NodeUpgradeLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold a raw flock: %v", err)
	}
	if _, err := f.WriteAt([]byte("upgrade "+strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() })

	err = RollbackCA(io.Discard, CARotateOptions{})
	nodeLockRefusal(t, err, "upgrade")
}
