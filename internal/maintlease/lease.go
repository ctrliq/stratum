// Package maintlease holds self-renewing cluster maintenance Leases. Upgrade,
// certificate rotation, migration and renewal use a common fence; the released
// upgrade identity is retained to exclude old upgrades during transition.
// Leases stay renewed until release and expire if their holder dies.
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

// ErrTaken reports that another run holds the lease.
var ErrTaken = errors.New("the lease is held by another run")

// Vars so tests and other operations can use a fake clock or different
// timings. The defaults are the released upgrade timings: a 60 s duration,
// renewal every 20 s, and a timeout that bounds each API call.
var (
	Duration   = 60 * time.Second
	RenewEvery = 20 * time.Second
	Now        = time.Now
	// CallTimeout bounds each lease API call. Without it a blackholed
	// request hung the renewal goroutine, so a lost lease was never observed,
	// and hung release, so the run never exited.
	CallTimeout = 10 * time.Second
	// AcquireRetryBudget bounds the transient-error retry of a fence take.
	// A control plane taking the fence through a VIP loses in-flight calls to
	// kube-vip leadership handoffs — a connection reset the apiserver never
	// saw — and one request that landed server-side anyway converges on the
	// retry, because the next read finds the lease under this holder.
	// AcquireRetryBudget = 0 disables the retry.
	AcquireRetryBudget = 30 * time.Second
	// ReleaseRetryBudget bounds the transient-error retry of a release. A
	// release lost to the same VIP handoff a take survives would leave the
	// fence held for Duration, and every fenced command in that window would
	// refuse. ReleaseRetryBudget = 0 makes a release a single attempt.
	ReleaseRetryBudget = 15 * time.Second
)

// heldByOther returns the other holder's description when l is a live lease
// held by someone else, or "" when it is free to take. Pure.
//
// The lease lives for its stored duration, but never longer than Duration.
// Every stratum run writes Duration, so a longer stored duration came from
// something else — a hand edit, or a client allowed to update the lease that
// is not a stratum run — and honouring it would keep the fence held long after
// its holder stopped renewing.
func heldByOther(l *coordinationv1.Lease, holder string, now time.Time) string {
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" || *l.Spec.HolderIdentity == holder {
		return ""
	}
	if l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
		return ""
	}
	expires := l.Spec.RenewTime.Add(min(storedDuration(l), Duration))
	if !now.Before(expires) {
		return "" // expired: its holder died without releasing it
	}
	return fmt.Sprintf("%s, renewed %s ago", *l.Spec.HolderIdentity, now.Sub(l.Spec.RenewTime.Time).Round(time.Second))
}

// storedDuration is the duration l records. l.Spec.LeaseDurationSeconds must
// be set.
func storedDuration(l *coordinationv1.Lease) time.Duration {
	return time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
}

// Guard reports whether this run still holds the lease. It is lost for good
// at once if someone else holds it. With no renewal for one renewal interval
// before expiry it is only doubtful: it may have been taken, so nothing new
// is started, but one renewal that succeeds clears it. That renewal is a
// compare-and-swap on a lease still naming this run, so nobody held it in
// between. This is what lets a run ride out its own API server restarting.
// An in-flight node Job is not aborted.
type Guard struct {
	mu        sync.Mutex
	lastRenew time.Time
	lost      error
	stale     error
	// renew is one renewal attempt, for Confirm. Unset in tests that only
	// drive Observe.
	renew func(context.Context) error
	// holder is this run's identity on the lease.
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

// Confirm renews the lease now and returns nil only once that succeeded: a
// same-holder renewal is a compare-and-swap, so it proves nobody held the
// lease since the last one. Err alone cannot say that, as it only reflects
// the renewals the background loop has made so far: after a long step, such
// as an API outage, a successor may already hold the lease before the next
// tick sees it. A takeover is lost for good and wraps ErrTaken; any other
// failure is for the caller to retry. A nil guard (dry-run, tests) is held.
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
	// An answer retrying cannot change is returned as it came, whatever state
	// the guard is in: Forbidden or NotFound on the lease, or a takeover.
	if err != nil && terminalAPIError(err) {
		return err
	}
	if lerr := g.Err(); lerr != nil {
		return lerr
	}
	return err
}

// Observe records one renewal attempt's outcome. Pure apart from the lock.
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

// terminalAPIError is an answer that retrying cannot change: a lease taken by
// another run, or an API 4xx other than a timeout, a conflict (the callers
// re-read) or throttling. Anything else, network errors and 5xx included, is
// the API server not being ready yet.
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

// Acquire takes the named maintenance lease or refuses. It returns a Guard to
// check before destructive steps, and a release func that stops renewal and
// deletes the lease if it is still ours. The lease is created if absent; a
// live lease held by another run refuses, naming the operation; an expired or
// empty one is taken over with a resourceVersion compare-and-swap. A renewal
// goroutine keeps it live until release or the process dies.
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
			// Renewal timestamps are supplied by clients. Never steal a live
			// fence on a clock-skew guess, or promise bounded expiry when its
			// timestamp can postpone it arbitrarily.
			if cur.Spec.RenewTime.Time.After(checkedAt) {
				return nil, nil, fmt.Errorf("another %s is in progress (%s); two runs at once are not safe. The lease %s/%s "+
					"records a renewal time in the future (%s); check the holder's clock. Waiting %s may not free it: "+
					"if no %s is running, delete it with `kubectl -n %s delete lease %s`: %w",
					op, other, namespace, name, cur.Spec.RenewTime.Time.UTC().Format(time.RFC3339Nano), Duration,
					op, namespace, name, ErrTaken)
			}
			// A duration stratum did not set means the holder may not be a
			// stratum run either, and one that keeps renewing never lets the
			// lease expire: name the delete rather than promise an expiry.
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
		// The resourceVersion makes this a compare-and-swap: a racing taker
		// makes one of the two updates fail with a conflict.
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

// Renew pushes the lease's expiry out for holder. It fails with an error
// wrapping ErrTaken when another run holds the lease now, and passes an API
// error — NotFound on a deleted lease among them — to the caller as it came.
func Renew(ctx context.Context, cs kubernetes.Interface, namespace, name, holder string) error {
	return RenewWithClock(ctx, cs, namespace, name, holder, Now)
}

// renewConflictAttempts bounds how many times one renewal reads the lease
// again after a compare-and-swap conflict.
const renewConflictAttempts = 5

// RenewWithClock renews using the same clock as AcquireWithClock. A conflict
// on the update means the lease changed since it was read, most often by this
// run's own background renewal landing first, so the lease is read again:
// still naming holder, nobody else took it and the renewal is retried; naming
// another holder, it wraps ErrTaken.
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

// Release deletes the lease only while it is still ours, with the
// resourceVersion as a precondition, so it never deletes a successor's. Each
// call is bounded by CallTimeout, and a transient failure or a conflict is
// retried within ReleaseRetryBudget. A release that gives up says so: the
// lease then expires on its own within Duration.
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

// releaseRetryable reports whether a failed release attempt is worth another
// within the budget: anything a take retries, a conflict, and a refused
// connection too. A take stops at a refused dial so a cold start skips the
// fence at once; a release meets one when the apiserver restarts or the VIP
// moves under it, and giving up there leaves the fence held for Duration.
func releaseRetryable(err error) bool {
	if transientTakeError(err) || apierrors.IsConflict(err) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial" && errors.Is(err, syscall.ECONNREFUSED)
}

// releaseOnce is one release attempt. A lease that is gone or names another
// holder is not this run's to delete, and counts as released.
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

// AcquireCompatible takes the released upgrade fence before the common fence.
// Holding both, rather than just checking the old name, prevents an old upgrade
// from starting after the check. Old certificate operations still need their
// Secret and staged-rotation guards until every node adopts the common fence.
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

// transientTakeError reports whether err is the kind of transport failure a
// fence take through a VIP survives by retrying: a connection reset, dropped
// mid-call or closed before the answer arrived (EOF) — a kube-vip leadership handoff the apiserver never saw — a timed
// out request, or a 5xx/429/408 answer. A refused DIAL is deliberately not
// transient: nothing listening is a deployment's current reality, not a blip —
// a cold start takes the fence against an apiserver that is not up yet, and
// retrying would delay every such start instead of skipping the fence at once,
// which is the caller's contract.
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

// acquireRetrying takes one lease, retrying transient failures within
// AcquireRetryBudget. A refusal that names another live holder (ErrTaken) and
// every other terminal answer stop immediately — refusing fast against a live
// run is the fence's contract, not a failure to survive. The retry cannot
// double-take either: a take that reached the apiserver before the connection
// died shows up as this holder's lease, and Acquire takes its own lease over.
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
			return nil, nil, err // the caller canceled: surface its cause, not the budget's
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

// ConfirmWithin waits for a successful ownership check within a bounded API
// recovery window. Permanent errors stop immediately; a transient outage never
// authorizes further changes until a same-holder renewal succeeds.
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
