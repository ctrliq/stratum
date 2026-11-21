// Package maintlease holds self-renewing cluster maintenance Leases.
package maintlease

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var ErrTaken = errors.New("the lease is held by another run")

var (
	Duration   = 60 * time.Second
	RenewEvery = 20 * time.Second
	Now        = time.Now
	// Bounds each lease API call; a blackholed request otherwise hangs the renewal loop.
	CallTimeout = 10 * time.Second
	// Retries transient take failures: a take through a VIP loses calls to kube-vip handoffs.
	AcquireRetryBudget = 30 * time.Second
	// A release lost to a VIP handoff must retry, or the fence stays held for Duration.
	ReleaseRetryBudget = 15 * time.Second
)

// A stored duration longer than Duration did not come from stratum, so it is capped.
func heldByOther(l *coordinationv1.Lease, holder string, now time.Time) string {
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" || *l.Spec.HolderIdentity == holder {
		return ""
	}
	if l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
		return ""
	}
	expires := l.Spec.RenewTime.Add(min(storedDuration(l), Duration))
	if !now.Before(expires) {
		return ""
	}
	return fmt.Sprintf("%s, renewed %s ago", *l.Spec.HolderIdentity, now.Sub(l.Spec.RenewTime.Time).Round(time.Second))
}

func storedDuration(l *coordinationv1.Lease) time.Duration {
	return time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
}

// lost is permanent; stale clears on one successful same-holder renewal, a compare-and-swap.
type Guard struct {
	mu        sync.Mutex
	lastRenew time.Time
	lost      error
	stale     error
	// Unset in tests that only drive Observe.
	renew  func(context.Context) error
	holder string
	now    func() time.Time
	// compatible fences the released upgrade identity during mixed-release runs.
	compatible *Guard
}

// ID is the holder, or "" for a nil guard (dry-run, tests).
func (g *Guard) ID() string {
	if g == nil {
		return ""
	}
	return g.holder
}

// Err is nil while the lease is held.
func (g *Guard) Err() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return g.lost
	}
	if g.stale != nil {
		return g.stale
	}
	return g.compatible.Err()
}

// A successful same-holder renewal is a compare-and-swap, so it proves nobody held the lease.
func (g *Guard) Confirm(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.lost != nil {
		err := g.lost
		g.mu.Unlock()
		return err
	}
	renew := g.renew
	g.mu.Unlock()
	if err := g.compatible.Confirm(ctx); err != nil {
		return err
	}
	if renew == nil {
		return g.Err()
	}
	rctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	err := renew(rctx)
	now := g.now
	if now == nil {
		now = Now
	}
	g.Observe(err, now())
	// Terminal errors return as they came even when the guard is now lost or stale.
	if err != nil && terminalAPIError(err) {
		return err
	}
	if lerr := g.Err(); lerr != nil {
		return lerr
	}
	return err
}

func (g *Guard) Observe(err error, now time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return
	}
	switch {
	case err == nil:
		g.lastRenew = now
		g.stale = nil
	case errors.Is(err, ErrTaken):
		g.lost = fmt.Errorf("the maintenance lease is no longer ours (%w); stopping before any further node is touched", err)
	case now.Sub(g.lastRenew) >= Duration-RenewEvery:
		g.stale = fmt.Errorf("the maintenance lease could not be renewed for %s (%w) and may be taken by another run; "+
			"stopping before any further node is touched", now.Sub(g.lastRenew).Round(time.Second), err)
	}
}

// An API 4xx other than timeout, conflict or throttling; anything else may still clear.
func terminalAPIError(err error) bool {
	if errors.Is(err, ErrTaken) {
		return true
	}
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return false
	}
	c := st.Status().Code
	return c >= 400 && c < 500 && c != http.StatusRequestTimeout && c != http.StatusConflict && c != http.StatusTooManyRequests
}

// Refuses a live lease held by another run; takes an expired one by resourceVersion CAS.
func Acquire(ctx context.Context, cs kubernetes.Interface, namespace, name, holder, op string) (*Guard, func(), error) {
	return AcquireWithClock(ctx, cs, namespace, name, holder, op, Now)
}

// AcquireWithClock captures the caller's clock for the lifetime of the lease.
func AcquireWithClock(ctx context.Context, cs kubernetes.Interface, namespace, name, holder, op string, clock func() time.Time) (*Guard, func(), error) {
	actx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	leases := cs.CoordinationV1().Leases(namespace)
	now := metav1.NewMicroTime(clock())
	secs := int32(Duration / time.Second)
	desired := coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, AcquireTime: &now, RenewTime: &now}

	cur, err := leases.Get(actx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = leases.Create(actx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       desired,
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil, nil, fmt.Errorf("another run created %s/%s at the same moment; run one at a time: %w", namespace, name, ErrTaken)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("take the lease %s/%s: %w", namespace, name, err)
		}
	case err != nil:
		return nil, nil, fmt.Errorf("read the lease %s/%s: %w", namespace, name, err)
	default:
		checkedAt := clock()
		if other := heldByOther(cur, holder, checkedAt); other != "" {
			// Renewal timestamps are client-supplied; a future one must not read as expiring soon.
			if cur.Spec.RenewTime.Time.After(checkedAt) {
				return nil, nil, fmt.Errorf("another %s is in progress (%s); two runs at once are not safe. The lease %s/%s "+
					"records a renewal time in the future (%s); check the holder's clock. Waiting %s may not free it: "+
					"if no %s is running, delete it with `kubectl -n %s delete lease %s`: %w",
					op, other, namespace, name, cur.Spec.RenewTime.Time.UTC().Format(time.RFC3339Nano), Duration,
					op, namespace, name, ErrTaken)
			}
			// A duration stratum did not set means the holder may not be a stratum run.
			if d := storedDuration(cur); d > Duration {
				return nil, nil, fmt.Errorf("another %s is in progress (%s); two runs at once are not safe. The lease %s/%s "+
					"records a duration of %s, longer than the %s stratum writes, so stratum did not set it and waiting may "+
					"never free it: if no %s is running, delete it with `kubectl -n %s delete lease %s`: %w",
					op, other, namespace, name, d, Duration, op, namespace, name, ErrTaken)
			}
			return nil, nil, fmt.Errorf("another %s is in progress (%s); two runs at once are not safe. Wait for it to finish, "+
				"or if its process is gone the lease %s/%s expires on its own within %s: %w", op, other, namespace, name, Duration, ErrTaken)
		}
		cur.Spec = desired
		// resourceVersion makes this a compare-and-swap; a racing taker hits a conflict.
		if _, err := leases.Update(actx, cur, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			return nil, nil, fmt.Errorf("take the lease %s/%s: %w: %w", namespace, name, ErrTaken, err)
		} else if err != nil {
			return nil, nil, fmt.Errorf("take the lease %s/%s: %w", namespace, name, err)
		}
	}
	guard := &Guard{lastRenew: clock(), holder: holder, now: clock, renew: func(ctx context.Context) error { return RenewWithClock(ctx, cs, namespace, name, holder, clock) }}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(RenewEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				rctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
				rerr := RenewWithClock(rctx, cs, namespace, name, holder, clock)
				cancel()
				if rerr != nil {
					log.Printf("stratum: renew the maintenance lease %s/%s: %v", namespace, name, rerr)
				}
				guard.Observe(rerr, clock())
			}
		}
	}()
	return guard, func() {
		close(stop)
		<-done
		Release(context.Background(), cs, namespace, name, holder)
	}, nil
}

// Fails wrapping ErrTaken when another run holds the lease now.
func Renew(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) error {
	return RenewWithClock(ctx, cs, namespace, name, holder, Now)
}

const renewConflictAttempts = 5

// On a conflict, read the lease again and retry while it still names this holder.
func RenewWithClock(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string, clock func() time.Time) error {
	leases := cs.CoordinationV1().Leases(namespace)
	var err error
	for range renewConflictAttempts {
		var l *coordinationv1.Lease
		l, err = leases.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != holder {
			who := "nobody"
			if l.Spec.HolderIdentity != nil {
				who = *l.Spec.HolderIdentity
			}
			return fmt.Errorf("%w (%s)", ErrTaken, who)
		}
		now := metav1.NewMicroTime(clock())
		l.Spec.RenewTime = &now
		if _, err = leases.Update(ctx, l, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
			return err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("renew the lease %s/%s: still conflicting after %d attempts: %w", namespace, name, renewConflictAttempts, err)
}

// Deletes only while the lease still names holder; transient failures retry within budget.
func Release(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) {
	if ReleaseRetryBudget <= 0 {
		if err := releaseOnce(ctx, cs, namespace, name, holder); err != nil {
			log.Printf("stratum: release the maintenance lease %s/%s: %v (it expires on its own within %s)", namespace, name, err, Duration)
		}
		return
	}
	bctx, cancel := context.WithTimeout(ctx, ReleaseRetryBudget)
	defer cancel()
	for {
		err := releaseOnce(bctx, cs, namespace, name, holder)
		if err == nil {
			return
		}
		retry := releaseRetryable(err)
		if !retry || bctx.Err() != nil {
			log.Printf("stratum: release the maintenance lease %s/%s: %v (it expires on its own within %s)", namespace, name, err, Duration)
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-bctx.Done():
			timer.Stop()
			log.Printf("stratum: release the maintenance lease %s/%s: %v (it expires on its own within %s)", namespace, name, err, Duration)
			return
		case <-timer.C:
		}
	}
}

// A take stops at a refused dial (a cold start skips the fence); a release retries one.
func releaseRetryable(err error) bool {
	if transientTakeError(err) || apierrors.IsConflict(err) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial" && errors.Is(err, syscall.ECONNREFUSED)
}

// A lease that is gone or names another holder is not ours to delete, and counts as released.
func releaseOnce(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) error {
	actx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	leases := cs.CoordinationV1().Leases(namespace)
	l, err := leases.Get(actx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != holder {
		return nil
	}
	rv := l.ResourceVersion
	err = leases.Delete(actx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &rv},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Takes the released upgrade fence too, so an old upgrade cannot start after the check.
func AcquireCompatible(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) (*Guard, func(), error) {
	legacy, releaseLegacy, err := acquireRetrying(ctx, cs, namespace, "stratum-cluster-upgrade", holder)
	if err != nil {
		return nil, nil, err
	}
	common, releaseCommon, err := acquireRetrying(ctx, cs, namespace, name, holder)
	if err != nil {
		releaseLegacy()
		return nil, nil, err
	}
	common.compatible = legacy
	return common, func() { releaseCommon(); releaseLegacy() }, nil
}

// A refused dial is excluded: a cold start legitimately meets an apiserver not up yet.
func transientTakeError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		c := st.Status().Code
		return c == http.StatusRequestTimeout || c == http.StatusTooManyRequests || c >= 500
	}
	return false
}

// Retries transient failures within the budget; a refusal naming a live holder stops at once.
func acquireRetrying(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) (*Guard, func(), error) {
	if AcquireRetryBudget <= 0 {
		return Acquire(ctx, cs, namespace, name, holder, "maintenance run")
	}
	rctx, cancel := context.WithTimeout(ctx, AcquireRetryBudget)
	defer cancel()
	var last error
	uncertain := false
	defer func() {
		if uncertain {
			Release(context.Background(), cs, namespace, name, holder)
		}
	}()
	for {
		guard, release, err := Acquire(rctx, cs, namespace, name, holder, "maintenance run")
		if err == nil {
			uncertain = false
			return guard, release, nil
		}
		last = err
		if terminalAPIError(err) || !transientTakeError(err) {
			if errors.Is(err, context.Canceled) {
				uncertain = true
			}
			return nil, nil, err
		}
		uncertain = true
		if ctx.Err() != nil {
			return nil, nil, err
		}
		if rctx.Err() != nil {
			return nil, nil, fmt.Errorf("take the maintenance lease %s/%s: %w (last attempt: %v)", namespace, name, rctx.Err(), last)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-rctx.Done():
			timer.Stop()
			return nil, nil, fmt.Errorf("take the maintenance lease %s/%s: %w (last attempt: %v)", namespace, name, rctx.Err(), last)
		case <-timer.C:
		}
	}
}

// Retries until a confirm succeeds or the budget ends; a terminal error stops at once.
func (g *Guard) ConfirmWithin(ctx context.Context, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last == nil {
				return err
			}
			return fmt.Errorf("confirm maintenance ownership: %w: %w", err, last)
		}
		err := g.Confirm(ctx)
		last = err
		if err == nil || terminalAPIError(err) {
			return err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("confirm maintenance ownership: %w: %w", ctx.Err(), err)
		case <-timer.C:
		}
	}
}
