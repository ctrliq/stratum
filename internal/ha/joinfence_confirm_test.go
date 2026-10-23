package ha

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
)

// takeThenRefuse takes the production join fence on cs and then refuses the
// next n lease calls with a refused dial, as the VIP does while the previous
// control plane settles.
func takeThenRefuse(cs *fake.Clientset, n int) func() (*k8sutil.MaintenanceLease, error) {
	return func() (*k8sutil.MaintenanceLease, error) {
		lease, err := takeJoinMaintenanceFence()
		if err != nil {
			return nil, err
		}
		refusals := 0
		cs.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			if refusals >= n {
				return false, nil, nil
			}
			refusals++
			return true, nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		})
		return lease, nil
	}
}

func shortJoinFenceConfirm(t *testing.T) {
	t.Helper()
	saved := joinFenceConfirmWait
	t.Cleanup(func() { joinFenceConfirmWait = saved })
	joinFenceConfirmWait = 30 * time.Second
}

// The join confirms its fence through the VIP before the member-add. A VIP
// that refuses a connection for a moment is waited out, and the member-add
// runs once a same-holder renewal succeeds.
func TestControlPlaneJoinWaitsOutARefusedFenceConfirm(t *testing.T) {
	joinFixture(t)
	shortJoinFenceConfirm(t)
	cs := joinerClientset(publishedCertsReaderRole(t), "abc123")
	joinFenceFixture(t, cs, "abc123")
	r := &recordedJoin{}
	fx := fakeJoinEffects(r)
	fx.takeMaintenanceFence = takeThenRefuse(cs, 2)
	if _, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx); err != nil {
		t.Fatalf("the join gave up on a refused fence confirm: %v", err)
	}
	if r.addCalls != 1 {
		t.Fatalf("member-adds = %d, want the one the confirmed fence allows", r.addCalls)
	}
}

// Waiting never stands in for ownership: a fence another run took while the
// VIP was refusing stops the join before the member-add, at once.
func TestControlPlaneJoinStillRefusesAFenceTakenOver(t *testing.T) {
	joinFixture(t)
	shortJoinFenceConfirm(t)
	cs := joinerClientset(publishedCertsReaderRole(t), "abc123")
	joinFenceFixture(t, cs, "abc123")
	r := &recordedJoin{}
	fx := fakeJoinEffects(r)
	fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
		lease, err := takeJoinMaintenanceFence()
		if err != nil {
			return nil, err
		}
		gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
		o, err := cs.Tracker().Get(gvr, "kube-system", "stratum-cluster-maintenance")
		if err != nil {
			return nil, err
		}
		l := o.(*coordinationv1.Lease).DeepCopy()
		other, now := "cp1/4242/1", metav1.NewMicroTime(time.Now())
		l.Spec.HolderIdentity, l.Spec.RenewTime = &other, &now
		if err := cs.Tracker().Update(gvr, l, "kube-system"); err != nil {
			return nil, err
		}
		return lease, nil
	}
	start := time.Now()
	_, err := prepareControlPlaneJoin(context.Background(), freshJoinOpts(), t.TempDir(), fx)
	if !errors.Is(err, maintlease.ErrTaken) && (err == nil || !strings.Contains(err.Error(), "no longer ours")) {
		t.Fatalf("err = %v, want the takeover", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the takeover was retried for %s; it must refuse at once", took)
	}
	if r.addCalls != 0 {
		t.Fatalf("member-adds = %d after the fence was taken over, want none", r.addCalls)
	}
}

// A nil capability, the join that needs no fence, confirms at once.
func TestNilMaintenanceLeaseConfirmsWithin(t *testing.T) {
	var lease *k8sutil.MaintenanceLease
	if err := lease.ConfirmWithin(context.Background(), time.Minute); err != nil {
		t.Fatalf("nil lease ConfirmWithin = %v, want nil", err)
	}
}
