package maintlease

// Tests for the cluster-wide maintenance Lease: the self-renewing Lease taken
// by every cluster-scoped maintenance orchestrator (upgrade, CA rotation,
// etcd-CA migration) before it dispatches node work. The lease name and
// namespace are parameters — this package pins behavior, not identity, so no
// released lease name is written here; the identities of the individual
// operations are pinned by internal/upgrade's own tests through its wrappers.
//
// The API the tests pin:
//
//	func Acquire(ctx context.Context, cs kubernetes.Interface, namespace, name, holder, op string) (*Guard, func(), error)
//	func Renew(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) error
//	func Release(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string)
//	var ErrTaken
//	type Guard with methods Err, Confirm, Observe, ID
//
// injectable vars:
//
//	Now = time.Now; Duration = 60s; RenewEvery = 20s; CallTimeout = 10s
//
// Acquire creates the lease if absent, refuses while another holder holds it
// live, and otherwise takes it with a resourceVersion compare-and-swap. A
// renewal goroutine keeps it live; Release deletes it only while it still
// names this holder.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testNS   = "maintenance-test"
	testName = "cluster-maintenance"
	testOp   = "`stratum maintenance --test`"
)

func fakeClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	now := at
	old := Now
	Now = func() time.Time { return now }
	t.Cleanup(func() { Now = old })
	return &now
}

func storedLease(t *testing.T, cs *fake.Clientset) *coordinationv1.Lease {
	t.Helper()
	l, err := cs.CoordinationV1().Leases(testNS).Get(context.Background(), testName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the lease: %v", err)
	}
	return l
}

// TestAcquireCreatesTheLeaseWhenAbsent: a free cluster gets a fresh lease at
// exactly the namespace and name the caller asked for — they are parameters,
// not constants of this package — naming the caller, with the release func
// ready. Two leases in two namespaces at the same time prove nothing is
// hardcoded.
func TestAcquireCreatesTheLeaseWhenAbsent(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	guard, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatalf("a free cluster: %v", err)
	}
	defer release()
	if guard == nil || guard.ID() != "m1/100/1" {
		t.Fatalf("the guard's identity = %v, want the holder", guard)
	}
	l := storedLease(t, cs)
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "m1/100/1" {
		t.Errorf("the created lease is held by %v, want m1/100/1", l.Spec.HolderIdentity)
	}
	if l.Spec.LeaseDurationSeconds == nil || *l.Spec.LeaseDurationSeconds != 60 {
		t.Errorf("lease duration = %v, want 60s", l.Spec.LeaseDurationSeconds)
	}
	if l.Spec.RenewTime == nil || !l.Spec.RenewTime.Time.Equal(*now) {
		t.Errorf("renew time = %v, want the acquire time %v", l.Spec.RenewTime, now)
	}

	// A second lease under another name and namespace is independent.
	g2, release2, err := Acquire(ctx, cs, "other-ns", "other-maintenance", "m1/100/1", testOp)
	if err != nil {
		t.Fatalf("a second lease under another name: %v", err)
	}
	defer release2()
	if g2.ID() != "m1/100/1" {
		t.Errorf("the second guard's identity = %q, want the holder", g2.ID())
	}
	if _, err := cs.CoordinationV1().Leases("other-ns").Get(ctx, "other-maintenance", metav1.GetOptions{}); err != nil {
		t.Errorf("the second lease was not created where it was asked for: %v", err)
	}
}

// TestAcquireRefusesWhileHeldLive: a live lease held by someone else refuses
// the take-over, naming the operation, the holder, and how the lease frees
// itself, and leaves the holder's lease untouched.
func TestAcquireRefusesWhileHeldLive(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	held, renewed := "other/2/2", metav1.NewMicroTime(Now())
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &held, LeaseDurationSeconds: ptr(int32(60)),
			RenewTime: &renewed},
	})

	_, _, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err == nil || !strings.Contains(err.Error(), "another "+testOp+" is in progress") || !errors.Is(err, ErrTaken) {
		t.Fatalf("a live lease held by another run = %v, want a refusal naming the operation, wrapping ErrTaken", err)
	}
	for _, want := range []string{"other/2/2", "expires on its own", testNS + "/" + testName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if l := storedLease(t, cs); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "other/2/2" {
		t.Errorf("the refused take-over changed the lease: holder %v", l.Spec.HolderIdentity)
	}
}

// TestAcquireTakesAFreeLease: a lease whose holder is gone is taken over with
// a compare-and-swap update of the existing object, not a create. Gone means
// expired past its duration, or a lease that names no holder, or one without
// the fields that say how long it lives.
func TestAcquireTakesAFreeLease(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	for name, spec := range map[string]coordinationv1.LeaseSpec{
		"expired":       {HolderIdentity: ptr("other/2/2"), LeaseDurationSeconds: ptr(int32(60)), RenewTime: ptr(metav1.NewMicroTime(now.Add(-2 * time.Minute)))},
		"no holder":     {LeaseDurationSeconds: ptr(int32(60)), RenewTime: ptr(metav1.NewMicroTime(*now))},
		"no timestamps": {HolderIdentity: ptr("other/2/2")},
	} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(&coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS, UID: "uid-1"},
				Spec:       spec,
			})
			_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
			if err != nil {
				t.Fatalf("a free lease was not taken: %v", err)
			}
			defer release()
			l := storedLease(t, cs)
			if l.UID != "uid-1" {
				t.Errorf("the lease was recreated (uid %q), not taken over with an update", l.UID)
			}
			if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "m1/100/1" {
				t.Errorf("the taken lease is held by %v, want m1/100/1", l.Spec.HolderIdentity)
			}
		})
	}
}

// TestAcquireHonoursAStoredDurationOnlyUpToDuration: every stratum run writes
// Duration into the lease, so a longer stored duration came from something
// else — a hand edit, or a client allowed to update the lease without being a
// stratum run — and honouring it would keep the fence held long after its
// holder stopped renewing, while the refusal promised expiry within Duration.
// Such a lease lives at most Duration past its last renewal, and while it is
// live the refusal names the delete that frees it. A lease with the duration
// stratum writes refuses and frees exactly as before, message included.
func TestAcquireHonoursAStoredDurationOnlyUpToDuration(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	lease := func(secs int32, renewed time.Time) *coordinationv1.Lease {
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr("other/2/2"), LeaseDurationSeconds: ptr(secs),
				RenewTime: ptr(metav1.NewMicroTime(renewed))},
		}
	}
	deleteHint := "`kubectl -n " + testNS + " delete lease " + testName + "`"

	t.Run("an overlong lease not renewed within Duration is free", func(t *testing.T) {
		cs := fake.NewSimpleClientset(lease(math.MaxInt32, now.Add(-2*Duration)))
		_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatalf("a lease last renewed %s ago was refused for its stored duration: %v", 2*Duration, err)
		}
		defer release()
		if l := storedLease(t, cs); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "m1/100/1" {
			t.Errorf("the free lease is held by %v, want m1/100/1", l.Spec.HolderIdentity)
		}
	})
	t.Run("an overlong lease renewed within Duration refuses, naming the delete", func(t *testing.T) {
		cs := fake.NewSimpleClientset(lease(math.MaxInt32, now.Add(-10*time.Second)))
		_, _, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
		if !errors.Is(err, ErrTaken) {
			t.Fatalf("a lease renewed 10s ago = %v, want a refusal wrapping ErrTaken", err)
		}
		for _, want := range []string{"other/2/2", deleteHint} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "expires on its own") {
			t.Errorf("the refusal %q promises an expiry the lease's holder can keep postponing", err)
		}
		if l := storedLease(t, cs); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "other/2/2" {
			t.Errorf("the refused take-over changed the lease: holder %v", l.Spec.HolderIdentity)
		}
	})
	t.Run("a lease with stratum's duration refuses as before", func(t *testing.T) {
		cs := fake.NewSimpleClientset(lease(int32(Duration/time.Second), now.Add(-10*time.Second)))
		_, _, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
		want := "another " + testOp + " is in progress (other/2/2, renewed 10s ago); two runs at once are not safe. " +
			"Wait for it to finish, or if its process is gone the lease " + testNS + "/" + testName +
			" expires on its own within 1m0s: " + ErrTaken.Error()
		if err == nil || err.Error() != want || !errors.Is(err, ErrTaken) {
			t.Fatalf("a live lease = %v, want exactly %q", err, want)
		}
	})
	t.Run("a lease with stratum's duration is free once it expires", func(t *testing.T) {
		cs := fake.NewSimpleClientset(lease(int32(Duration/time.Second), now.Add(-Duration)))
		_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatalf("a lease last renewed one duration ago: %v", err)
		}
		release()
	})
}

// A client-supplied future renewal must not promise an expiry within Duration.
// Keep the fence intact: a skewed clock may belong to a live maintenance run.
func TestAcquireFutureRenewalNamesSafeRecovery(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	for _, ahead := range []time.Duration{time.Second, 24 * time.Hour} {
		t.Run(ahead.String(), func(t *testing.T) {
			cs := fake.NewSimpleClientset(&coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       ptr("system:bootstrap:abc123/node/1/1"),
					LeaseDurationSeconds: ptr(int32(Duration / time.Second)),
					RenewTime:            ptr(metav1.NewMicroTime(now.Add(ahead))),
				},
			})
			_, _, err := Acquire(t.Context(), cs, testNS, testName, "other/2/2", testOp)
			if !errors.Is(err, ErrTaken) {
				t.Fatalf("future renewal = %v, want ErrTaken", err)
			}
			for _, want := range []string{"renewal time in the future", "clock", "if no", "kubectl -n " + testNS + " delete lease " + testName} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not explain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "expires on its own within") {
				t.Errorf("refusal promises expiry within Duration despite a future renewal: %v", err)
			}
			for _, action := range cs.Actions() {
				if action.GetVerb() != "get" {
					t.Errorf("future holder's lease was changed: %v", action)
				}
			}
		})
	}
}

// TestAcquireRefusesWhenTheCreateRaces: two runs reaching an absent lease at
// the same moment must not both hold it — the loser of the create gets an
// error, and the winner's lease stands.
func TestAcquireRefusesWhenTheCreateRaces(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(coordinationv1.SchemeGroupVersion.WithResource("leases").GroupResource(), testName)
	})

	_, _, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err == nil || !strings.Contains(err.Error(), "same moment") || !errors.Is(err, ErrTaken) {
		t.Fatalf("a simultaneous create = %v, want a refusal naming the race, wrapping ErrTaken", err)
	}
}

// TestAcquireSurfacesAnUpdateConflict: the take-over of an existing lease is a
// compare-and-swap on its resourceVersion. A racing taker makes one of the two
// updates fail with a conflict, and that failure reaches the caller instead of
// being retried into a steal.
func TestAcquireSurfacesAnUpdateConflict(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	held, renewed := "other/2/2", metav1.NewMicroTime(now.Add(-2*time.Minute))
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &held, RenewTime: &renewed},
	})
	cs.PrependReactor("update", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(coordinationv1.SchemeGroupVersion.WithResource("leases").GroupResource(), testName, errors.New("the lease changed"))
	})

	_, _, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if !apierrors.IsConflict(err) || !errors.Is(err, ErrTaken) {
		t.Fatalf("a conflicted take-over = %v, want the conflict surfaced, wrapping ErrTaken", err)
	}
	if l := storedLease(t, cs); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "other/2/2" {
		t.Errorf("the conflicted take-over changed the lease: holder %v", l.Spec.HolderIdentity)
	}
}

// TestRenewExtendsALiveLeaseAndRejectsATakeover: renewal by the holder pushes
// the expiry out so a second run is still refused; renewal after someone else
// took the lease fails, naming who holds it now; a missing lease is an error.
func TestRenewExtendsALiveLeaseAndRejectsATakeover(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	_, releaseOwned, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseOwned)

	*now = now.Add(Duration - time.Second)
	if err := Renew(ctx, cs, testNS, testName, "m1/100/1"); err != nil {
		t.Fatalf("renew by the holder: %v", err)
	}
	*now = now.Add(Duration - time.Second)
	if _, _, err := Acquire(ctx, cs, testNS, testName, "m2/200/2", testOp); err == nil {
		t.Fatal("a renewed lease was taken over")
	}

	err = Renew(ctx, cs, testNS, testName, "m2/200/2")
	if !errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "m1/100/1") {
		t.Fatalf("renew after a takeover = %v, want ErrTaken naming the current holder", err)
	}
	if err := Renew(ctx, cs, testNS, testName+"-gone", "m1/100/1"); !apierrors.IsNotFound(err) {
		t.Fatalf("renew of a missing lease = %v, want not found", err)
	}
}

// conflictOnUpdate makes the next n lease updates fail with a conflict, after
// first applying change, when given, to the stored lease: a write that landed
// between the renewal's read and its update.
func conflictOnUpdate(t *testing.T, cs *fake.Clientset, n int, change func(*coordinationv1.Lease)) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
	cs.PrependReactor("update", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		if int(calls.Add(1)) > n {
			return false, nil, nil
		}
		if change != nil {
			obj, err := cs.Tracker().Get(gvr, testNS, testName)
			if err != nil {
				t.Errorf("read the stored lease: %v", err)
				return true, nil, err
			}
			l := obj.(*coordinationv1.Lease).DeepCopy()
			change(l)
			if err := cs.Tracker().Update(gvr, l, testNS); err != nil {
				t.Errorf("change the stored lease: %v", err)
				return true, nil, err
			}
		}
		return true, nil, apierrors.NewConflict(gvr.GroupResource(), testName, errors.New("the object has been modified"))
	})
	return &calls
}

// TestRenewRetriesItsOwnConflict: a renewal whose update conflicts with
// another write by the same holder, such as the background renewal racing a
// Confirm, reads the lease again and renews it rather than failing with the
// bare conflict.
func TestRenewRetriesItsOwnConflict(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	ownWrite := func(l *coordinationv1.Lease) {
		at := metav1.NewMicroTime(now.Add(time.Second))
		l.Spec.RenewTime = &at
	}
	calls := conflictOnUpdate(t, cs, 2, ownWrite)

	*now = now.Add(RenewEvery)
	if err := Renew(ctx, cs, testNS, testName, "m1/100/1"); err != nil {
		t.Fatalf("renew after two of its own conflicts = %v, want it renewed", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("updates attempted = %d, want 3 (two conflicts, then the renewal)", got)
	}
	if l := storedLease(t, cs); l.Spec.RenewTime == nil || !l.Spec.RenewTime.Time.Equal(*now) {
		t.Errorf("renew time = %v, want %v", l.Spec.RenewTime, *now)
	}
}

// TestRenewConflictWithATakeoverIsErrTaken: a conflict caused by another
// holder taking the lease is a takeover, not a retry, and names the new
// holder.
func TestRenewConflictWithATakeoverIsErrTaken(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	calls := conflictOnUpdate(t, cs, 1, func(l *coordinationv1.Lease) {
		other := "m2/200/2"
		l.Spec.HolderIdentity = &other
	})

	err = Renew(ctx, cs, testNS, testName, "m1/100/1")
	if !errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "m2/200/2") {
		t.Fatalf("renew conflicting with a takeover = %v, want ErrTaken naming m2/200/2", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("updates attempted = %d, want 1: a takeover is not retried", got)
	}
}

// TestRenewGivesUpOnAPersistentConflict: a conflict that never clears is
// retried a bounded number of times and then returned, still a conflict and
// naming the lease, so Confirm's callers keep treating it as retryable.
func TestRenewGivesUpOnAPersistentConflict(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	calls := conflictOnUpdate(t, cs, math.MaxInt32, nil)

	err = Renew(ctx, cs, testNS, testName, "m1/100/1")
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), testNS+"/"+testName) {
		t.Fatalf("renew under a persistent conflict = %v, want the conflict naming the lease", err)
	}
	if terminalAPIError(err) {
		t.Errorf("a persistent renewal conflict is terminal; Confirm's callers must be able to retry it")
	}
	if got := calls.Load(); got != renewConflictAttempts {
		t.Errorf("updates attempted = %d, want %d", got, renewConflictAttempts)
	}
}

// TestReleaseDeletesOnlyWhileHolder: release by the holder deletes the lease,
// releasing an already-gone lease is a no-op, and a run that lost its lease
// never deletes the successor's on its way out.
func TestReleaseDeletesOnlyWhileHolder(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	release()
	Release(ctx, cs, testNS, testName, "m1/100/1") // the lease is already gone
	if _, err := cs.CoordinationV1().Leases(testNS).Get(ctx, testName, metav1.GetOptions{}); err == nil {
		t.Fatal("release left the lease behind")
	}

	// The stale run's release: its lease expired and a successor took over.
	_, releaseOwned, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseOwned)
	*now = now.Add(Duration + time.Second)
	_, release2, err := Acquire(ctx, cs, testNS, testName, "m2/200/2", testOp)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	Release(ctx, cs, testNS, testName, "m1/100/1") // the stale run finally exits
	if l := storedLease(t, cs); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "m2/200/2" {
		t.Fatalf("the successor's lease was removed or changed: %v", l.Spec.HolderIdentity)
	}
}

// TestRenewalLoopKeepsTheLeaseLiveAndStopsOnRelease: the renewal goroutine
// keeps the lease live without a manual call, a competing run is refused while
// it does, and release stops the loop and deletes the lease for good.
func TestRenewalLoopKeepsTheLeaseLiveAndStopsOnRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldLog := log.Writer()
		t.Cleanup(func() { log.SetOutput(oldLog) })
		cs := fake.NewSimpleClientset()
		ctx := context.Background()
		t0 := time.Now()

		guard, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatal(err)
		}
		releaseOnce := sync.OnceFunc(release)
		defer releaseOnce()

		time.Sleep(25 * time.Second)
		synctest.Wait()
		l := storedLease(t, cs)
		if l.Spec.RenewTime == nil || !l.Spec.RenewTime.Time.After(t0) {
			t.Fatalf("the renewal loop did not renew: renew time %v, want one after %v", l.Spec.RenewTime, t0)
		}
		if err := guard.Err(); err != nil {
			t.Fatalf("the guard tripped while renewals succeeded: %v", err)
		}
		if _, _, err := Acquire(ctx, cs, testNS, testName, "m2/200/2", testOp); err == nil {
			t.Error("a lease the loop keeps live was taken over")
		}

		releaseOnce()
		if _, err := cs.CoordinationV1().Leases(testNS).Get(ctx, testName, metav1.GetOptions{}); err == nil {
			t.Fatal("release did not delete the lease")
		}
		time.Sleep(2 * Duration)
		synctest.Wait()
		if _, err := cs.CoordinationV1().Leases(testNS).Get(ctx, testName, metav1.GetOptions{}); err == nil {
			t.Fatal("the lease came back after release stopped the renewal loop")
		}
	})
}

// TestRenewalLoopRidesOutAnAPIServerGap: while the API server is unreachable
// the guard goes stale one renewal interval before expiry — nothing new may
// start — and the first renewal that succeeds afterwards, which is a
// compare-and-swap on a lease still naming this run, clears it again.
func TestRenewalLoopRidesOutAnAPIServerGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldLog := log.Writer()
		t.Cleanup(func() { log.SetOutput(oldLog) })
		cs := fake.NewSimpleClientset()

		// The reactor goes in before Acquire: the renewal goroutine that
		// Acquire starts reads the reactor chain, and writing it while it
		// runs would race under -race. A flag gates it so the acquire-time
		// Get still succeeds: the API server is up until the outage begins.
		var down atomic.Bool
		gapEnd := time.Now().Add(45 * time.Second)
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if down.Load() && time.Now().Before(gapEnd) {
				return true, nil, errors.New("dial tcp 127.0.0.1:6443: connection refused")
			}
			return false, nil, nil
		})

		guard, release, err := Acquire(context.Background(), cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		// The API server goes down right after the acquire and comes back
		// 45s in: renewals fail at the 20s and 40s ticks and succeed at 60s.
		down.Store(true)

		time.Sleep(Duration - RenewEvery + time.Second) // two failed renewal ticks
		synctest.Wait()
		err = guard.Err()
		if err == nil || !strings.Contains(err.Error(), "may be taken by another run") {
			t.Fatalf("no renewal for %s: %v, want the guard stale", Duration-RenewEvery, err)
		}

		time.Sleep(2 * RenewEvery) // the gap ended; the next tick renews
		synctest.Wait()
		if err := guard.Err(); err != nil {
			t.Fatalf("a same-holder renewal after the gap left the guard tripped: %v", err)
		}
	})
}

// TestRenewalLoopStopsTheRunOnATakeover: once another run holds the lease the
// guard is lost for good — the loop sees it on its next tick, and a renewal
// that succeeds afterwards does not bring it back.
func TestRenewalLoopStopsTheRunOnATakeover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldLog := log.Writer()
		t.Cleanup(func() { log.SetOutput(oldLog) })
		cs := fake.NewSimpleClientset()
		ctx := context.Background()

		guard, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		time.Sleep(25 * time.Second)
		synctest.Wait()
		if err := guard.Err(); err != nil {
			t.Fatalf("the guard tripped before the takeover: %v", err)
		}

		// A successor takes the live lease over, as a takeover does: it
		// rewrites the holder in place.
		l := storedLease(t, cs)
		successor, renewed := "m2/200/2", metav1.NewMicroTime(time.Now())
		l.Spec.HolderIdentity, l.Spec.RenewTime = &successor, &renewed
		if err := cs.Tracker().Update(coordinationv1.SchemeGroupVersion.WithResource("leases"), l, testNS); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * RenewEvery)
		synctest.Wait()
		if err := guard.Err(); !errors.Is(err, ErrTaken) {
			t.Fatalf("the guard after a takeover = %v, want it lost with ErrTaken", err)
		}
		guard.Observe(nil, time.Now()) // even a renewal that now succeeds
		if err := guard.Err(); !errors.Is(err, ErrTaken) {
			t.Fatalf("a lost lease came back: %v", err)
		}
	})
}

// TestLeaseDurationsAreInjectableWithTheReleasedDefaults: the timings are
// package vars so tests and other operations can tighten or stretch them, and
// the defaults are the released 60s duration, 20s renewal interval, and a call
// timeout that bounds each API call inside the renewal interval.
func TestLeaseDurationsAreInjectableWithTheReleasedDefaults(t *testing.T) {
	if Duration != 60*time.Second || RenewEvery != 20*time.Second || CallTimeout != 10*time.Second {
		t.Fatalf("defaults = %s/%s/%s, want 60s/20s/10s", Duration, RenewEvery, CallTimeout)
	}
	if CallTimeout <= 0 || CallTimeout >= RenewEvery {
		t.Fatalf("CallTimeout = %s; it must be positive and shorter than the renewal interval %s", CallTimeout, RenewEvery)
	}

	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	old := Duration
	t.Cleanup(func() { Duration = old })
	Duration = 90 * time.Second
	cs := fake.NewSimpleClientset()
	_, release, err := Acquire(context.Background(), cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if l := storedLease(t, cs); l.Spec.LeaseDurationSeconds == nil || *l.Spec.LeaseDurationSeconds != 90 {
		t.Errorf("the injected duration did not reach the lease: %v", l.Spec.LeaseDurationSeconds)
	}
}

func ptr[T any](v T) *T { return &v }

// TestAcquireCompatibleRetriesATransientTake: a fence take through a VIP can
// lose its in-flight call to a kube-vip leadership handoff — a connection
// reset the apiserver never saw. The take retries the transient failure
// within AcquireRetryBudget and takes the fence on the retry; a take that
// landed server-side before the connection died converges the same way,
// because the retry's read finds the lease under this holder.
func TestAcquireCompatibleRetriesATransientTake(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	var calls atomic.Int32
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) == 1 {
			return true, nil, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
		}
		return false, nil, nil
	})
	guard, release, err := AcquireCompatible(ctx, cs, testNS, testName, "m1/100/1")
	if err != nil {
		t.Fatalf("a transient take did not converge on the retry: %v", err)
	}
	defer release()
	if guard == nil || guard.holder != "m1/100/1" {
		t.Fatalf("the retry lost the holder: %+v", guard)
	}
	if now := storedLease(t, cs); now.Spec.HolderIdentity == nil || *now.Spec.HolderIdentity != "m1/100/1" {
		t.Errorf("the fence did not end up held by the taker: %+v", now.Spec.HolderIdentity)
	}
	if calls.Load() < 2 {
		t.Errorf("lease reads=%d; the retried take must have attempted again after the transient failure", calls.Load())
	}
}

// TestAcquireCompatibleStopsImmediatelyOnATakenFence: refusing fast against a
// live maintenance run is the fence's contract, so a take that names another
// live holder must not wait out the retry budget.
func TestAcquireCompatibleStopsImmediatelyOnATakenFence(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	held, renewed := "other/2/2", metav1.NewMicroTime(now.Add(-2*time.Second))
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNS},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &held, LeaseDurationSeconds: ptr(int32(60)),
			RenewTime: &renewed},
	})
	budget := AcquireRetryBudget
	AcquireRetryBudget = 5 * time.Second
	t.Cleanup(func() { AcquireRetryBudget = budget })
	started := time.Now()
	_, _, err := AcquireCompatible(context.Background(), cs, testNS, testName, "m1/100/1")
	elapsed := time.Since(started)
	if !errors.Is(err, ErrTaken) {
		t.Fatalf("a live fence = %v, want ErrTaken immediately", err)
	}
	if elapsed > time.Second {
		t.Errorf("the taken fence was refused after %s; a live run must stop the take immediately", elapsed)
	}
}

// TestAcquireCompatibleStopsOnARefusedDial: nothing listening is a
// deployment's current reality, not a transient blip — a cold start takes the
// fence against an apiserver that is not up yet, and its contract is to skip
// the fence at once. A refused dial stops the take immediately, so the
// caller's classification sees the dial error and not a retry budget's wrap.
func TestAcquireCompatibleStopsOnARefusedDial(t *testing.T) {
	budget := AcquireRetryBudget
	AcquireRetryBudget = 5 * time.Second
	t.Cleanup(func() { AcquireRetryBudget = budget })
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("dial", syscall.ECONNREFUSED)}
	})
	started := time.Now()
	_, _, err := AcquireCompatible(context.Background(), cs, testNS, testName, "m1/100/1")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("a refused dial was retried for %s; nothing listening must stop the take immediately", elapsed)
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("a refused dial = %v; want the dial error surfaced for the caller's classification", err)
	}
}

// TestAcquireCompatibleRetriesAMidCallReset: the CI failure this retry exists
// for — a fence take through a VIP lost to a kube-vip leadership handoff,
// which surfaces as a connection reset on an established call. The take
// retries it and takes the fence once the blip passes.
func TestAcquireCompatibleRetriesAMidCallReset(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	var calls atomic.Int32
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) == 1 {
			return true, nil, &url.Error{Op: "Put", URL: "https://vip:6443/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-cluster-maintenance",
				Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
		}
		return false, nil, nil
	})
	_, release, err := AcquireCompatible(context.Background(), cs, testNS, testName, "m1/100/1")
	if err != nil {
		t.Fatalf("a mid-call reset did not converge on the retry: %v", err)
	}
	defer release()
	if calls.Load() < 2 {
		t.Errorf("lease reads=%d; the reset must have been retried", calls.Load())
	}
}

func resetErr() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
}

// TestReleaseRetriesThroughAVIPHandoff: a release lost to a kube-vip
// leadership handoff is retried, so the fence is not left held for Duration.
func TestReleaseRetriesThroughAVIPHandoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
		cs := fake.NewSimpleClientset()
		_, release, err := Acquire(context.Background(), cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatal(err)
		}
		var gets, deletes atomic.Int32
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if gets.Add(1) == 1 {
				return true, nil, resetErr()
			}
			return false, nil, nil
		})
		cs.PrependReactor("delete", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if deletes.Add(1) == 1 {
				return true, nil, resetErr()
			}
			return false, nil, nil
		})
		release()
		if _, err := cs.CoordinationV1().Leases(testNS).Get(context.Background(), testName, metav1.GetOptions{}); err == nil {
			t.Fatal("the release gave up on a transient failure and left the lease held")
		}
		if deletes.Load() < 2 {
			t.Errorf("deletes=%d; the reset delete must have been retried", deletes.Load())
		}
	})
}

// TestReleaseGivesUpWithinItsBudgetAndSaysSo: a release that cannot reach the
// apiserver stops at ReleaseRetryBudget and logs, a failed read included, so
// a run never hangs on its way out and the stale lease is explained.
func TestReleaseGivesUpWithinItsBudgetAndSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logged strings.Builder
		oldLog := log.Writer()
		log.SetOutput(&logged)
		t.Cleanup(func() { log.SetOutput(oldLog) })
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, resetErr()
		})
		started := time.Now()
		Release(context.Background(), cs, testNS, testName, "m1/100/1")
		if elapsed := time.Since(started); elapsed > ReleaseRetryBudget+time.Second {
			t.Errorf("the release ran %s, past its %s budget", elapsed, ReleaseRetryBudget)
		}
		if !strings.Contains(logged.String(), "release the maintenance lease") || !strings.Contains(logged.String(), "expires on its own") {
			t.Errorf("a release that gave up logged %q; want it named", logged.String())
		}
	})
}

// TestAcquireCompatibleReleasesATakeThatLandedBeforeItGaveUp: a take whose
// create reached the apiserver but whose answer was lost, followed by a
// retry budget spent on resets, leaves the lease under this holder. The
// take releases it on the way out instead of leaving it held for Duration.
func TestAcquireCompatibleReleasesATakeThatLandedBeforeItGaveUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
		budget := AcquireRetryBudget
		AcquireRetryBudget = 5 * time.Second
		t.Cleanup(func() { AcquireRetryBudget = budget })
		cs := fake.NewSimpleClientset()
		gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
		var landed atomic.Bool
		cs.PrependReactor("create", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
			obj := a.(clienttesting.CreateAction).GetObject()
			if err := cs.Tracker().Create(gvr, obj, testNS); err != nil {
				return true, nil, err
			}
			landed.Store(true)
			return true, nil, resetErr()
		})
		started := time.Now()
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if landed.Load() && time.Since(started) < AcquireRetryBudget+2*time.Second {
				return true, nil, resetErr()
			}
			return false, nil, nil
		})
		if _, _, err := AcquireCompatible(context.Background(), cs, testNS, testName, "m1/100/1"); err == nil {
			t.Fatal("setup: the take converged")
		}
		if !landed.Load() {
			t.Fatal("setup: the create never landed")
		}
		for _, n := range []string{"stratum-cluster-upgrade", testName} {
			obj, err := cs.Tracker().Get(gvr, testNS, n)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				t.Fatalf("read %s: %v", n, err)
			}
			if l := obj.(*coordinationv1.Lease); l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "m1/100/1" {
				t.Errorf("the failed take left %s held by this holder", n)
			}
		}
	})
}

// TestReleaseRetriesARefusedConnection: a release that meets a refused dial
// (an apiserver restarting, a VIP moving) retries it and deletes the lease
// once the apiserver answers, though a take stops at one at once.
func TestReleaseRetriesARefusedConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
		cs := fake.NewSimpleClientset()
		_, release, err := Acquire(context.Background(), cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatal(err)
		}
		var gets atomic.Int32
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if gets.Add(1) <= 2 {
				return true, nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
			}
			return false, nil, nil
		})
		release()
		if _, err := cs.Tracker().Get(coordinationv1.SchemeGroupVersion.WithResource("leases"), testNS, testName); !apierrors.IsNotFound(err) {
			t.Fatalf("the release gave up on a refused connection and left the lease held (%v)", err)
		}
	})
}

// TestAcquireCompatibleReleasesATakeCancelledAfterItLanded: a create that
// committed but whose answer the caller's cancellation cut off leaves the
// lease under this holder; the take releases it on its way out.
func TestAcquireCompatibleReleasesATakeCancelledAfterItLanded(t *testing.T) {
	fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
	cs.PrependReactor("create", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if err := cs.Tracker().Create(gvr, a.(clienttesting.CreateAction).GetObject(), testNS); err != nil {
			return true, nil, err
		}
		return true, nil, fmt.Errorf("Post lease: %w", context.Canceled)
	})
	if _, _, err := AcquireCompatible(context.Background(), cs, testNS, testName, "m1/100/1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireCompatible = %v, want the cancellation", err)
	}
	for _, n := range []string{"stratum-cluster-upgrade", testName} {
		obj, err := cs.Tracker().Get(gvr, testNS, n)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		if l := obj.(*coordinationv1.Lease); l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "m1/100/1" {
			t.Errorf("the cancelled take left %s held by this holder", n)
		}
	}
}

// TestReleaseRetriesAnEOF: a release whose answer is cut off by a closed
// connection retries it and deletes the lease once the apiserver answers.
func TestReleaseRetriesAnEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
		cs := fake.NewSimpleClientset()
		_, release, err := Acquire(context.Background(), cs, testNS, testName, "m1/100/1", testOp)
		if err != nil {
			t.Fatal(err)
		}
		var gets atomic.Int32
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			switch gets.Add(1) {
			case 1:
				return true, nil, fmt.Errorf("Get lease: %w", io.EOF)
			case 2:
				return true, nil, fmt.Errorf("Get lease: %w", io.ErrUnexpectedEOF)
			}
			return false, nil, nil
		})
		release()
		if _, err := cs.Tracker().Get(coordinationv1.SchemeGroupVersion.WithResource("leases"), testNS, testName); !apierrors.IsNotFound(err) {
			t.Fatalf("the release gave up on an EOF and left the lease held (%v)", err)
		}
	})
}

// TestAcquireCompatibleReleasesATakeWhoseAnswerHitEOF: a create that
// committed but whose answer was lost to a closed connection leaves the lease
// under this holder; a take that cannot converge releases it on its way out.
func TestAcquireCompatibleReleasesATakeWhoseAnswerHitEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
		budget := AcquireRetryBudget
		AcquireRetryBudget = 5 * time.Second
		t.Cleanup(func() { AcquireRetryBudget = budget })
		cs := fake.NewSimpleClientset()
		gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
		var landed atomic.Bool
		cs.PrependReactor("create", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if err := cs.Tracker().Create(gvr, a.(clienttesting.CreateAction).GetObject(), testNS); err != nil {
				return true, nil, err
			}
			landed.Store(true)
			return true, nil, fmt.Errorf("Post lease: %w", io.EOF)
		})
		started := time.Now()
		cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if landed.Load() && time.Since(started) < AcquireRetryBudget+2*time.Second {
				return true, nil, fmt.Errorf("Get lease: %w", io.ErrUnexpectedEOF)
			}
			return false, nil, nil
		})
		if _, _, err := AcquireCompatible(context.Background(), cs, testNS, testName, "m1/100/1"); err == nil {
			t.Fatal("setup: the take converged")
		}
		if !landed.Load() {
			t.Fatal("setup: the create never landed")
		}
		for _, n := range []string{"stratum-cluster-upgrade", testName} {
			obj, err := cs.Tracker().Get(gvr, testNS, n)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				t.Fatalf("read %s: %v", n, err)
			}
			if l := obj.(*coordinationv1.Lease); l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "m1/100/1" {
				t.Errorf("the failed take left %s held by this holder", n)
			}
		}
	})
}
