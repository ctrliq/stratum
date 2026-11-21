package maintlease

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

func TestReleaseDeletesOnlyWhileHolder(t *testing.T) {
	now := fakeClock(t, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	_, release, err := Acquire(ctx, cs, testNS, testName, "m1/100/1", testOp)
	if err != nil {
		t.Fatal(err)
	}
	release()
	Release(ctx, cs, testNS, testName, "m1/100/1")
	if _, err := cs.CoordinationV1().Leases(testNS).Get(ctx, testName, metav1.GetOptions{}); err == nil {
		t.Fatal("release left the lease behind")
	}

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
	Release(ctx, cs, testNS, testName, "m1/100/1")
	if l := storedLease(t, cs); l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "m2/200/2" {
		t.Fatalf("the successor's lease was removed or changed: %v", l.Spec.HolderIdentity)
	}
}

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

func TestRenewalLoopRidesOutAnAPIServerGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldLog := log.Writer()
		t.Cleanup(func() { log.SetOutput(oldLog) })
		cs := fake.NewSimpleClientset()

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

		down.Store(true)

		time.Sleep(Duration - RenewEvery + time.Second)
		synctest.Wait()
		err = guard.Err()
		if err == nil || !strings.Contains(err.Error(), "may be taken by another run") {
			t.Fatalf("no renewal for %s: %v, want the guard stale", Duration-RenewEvery, err)
		}

		time.Sleep(2 * RenewEvery)
		synctest.Wait()
		if err := guard.Err(); err != nil {
			t.Fatalf("a same-holder renewal after the gap left the guard tripped: %v", err)
		}
	})
}

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
		guard.Observe(nil, time.Now())
		if err := guard.Err(); !errors.Is(err, ErrTaken) {
			t.Fatalf("a lost lease came back: %v", err)
		}
	})
}

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
