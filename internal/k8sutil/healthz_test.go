package k8sutil

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A credential the API server refuses ends the wait at once, naming the rule
// it lacks, instead of spending the whole window on a /healthz it cannot read.
func TestWaitHealthzStopsOnARefusedCredential(t *testing.T) {
	for name, refusal := range map[string]error{
		"forbidden":    apierrors.NewForbidden(schema.GroupResource{}, "/healthz", errors.New("no rule")),
		"unauthorized": apierrors.NewUnauthorized("token expired"),
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			start := time.Now()
			err := waitHealthz(context.Background(), func(context.Context) ([]byte, error) {
				calls++
				return nil, refusal
			})
			if err == nil || !strings.Contains(err.Error(), "nonResourceURL /healthz") || !errors.Is(err, refusal) {
				t.Fatalf("got %v, want an error naming the /healthz rule and wrapping the refusal", err)
			}
			if calls != 1 || time.Since(start) > time.Second {
				t.Errorf("probed %d times over %v, want one probe and no wait", calls, time.Since(start))
			}
		})
	}
}

// One probe that accepts the connection and never answers must cost one
// attempt, not the window: its per-attempt bound frees the wait to try again,
// and the next probe — here, the first one after the hang — still gets its
// shot at an ok. Both times are fake (testing/synctest): a 10s attempt bound
// inside a 40s window, with the 2s poll sleeps, all elapse instantly.
func TestWaitHealthzHungProbeCostsOneAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		calls := 0
		err := waitHealthz(ctx, func(ctx context.Context) ([]byte, error) {
			calls++
			if calls == 1 {
				<-ctx.Done() // hangs until the attempt bound frees it
				return nil, ctx.Err()
			}
			return []byte("ok\n"), nil
		})
		if err != nil {
			t.Fatalf("got %v after %d probes; want the ok answer on the retry after the hung probe", err, calls)
		}
		if calls != 2 {
			t.Errorf("probed %d times; want the hung probe retried once, then an ok", calls)
		}
	})
}

// When every probe hangs, the last error names the per-attempt bound, so a
// failed gate says the probes got no answer in time rather than a bare
// "context deadline exceeded". Same fake clock as above.
func TestWaitHealthzNamesTheAttemptTimeoutWhenEveryProbeHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		err := waitHealthz(ctx, func(ctx context.Context) ([]byte, error) {
			<-ctx.Done() // every probe hangs until its attempt bound frees it
			return nil, ctx.Err()
		})
		if err == nil || !strings.Contains(err.Error(), "no answer within 10s") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v; want the last error naming the 10s per-attempt bound and the window deadline", err)
		}
	})
}

// When the window runs out, the error says what the last check answered.
func TestWaitHealthzNamesTheLastAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := waitHealthz(ctx, func(context.Context) ([]byte, error) { return nil, errors.New("connection refused") })
	if err == nil || !strings.Contains(err.Error(), "last check: connection refused") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the last error and the deadline", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	err = waitHealthz(ctx2, func(context.Context) ([]byte, error) { return []byte("[-]etcd failed"), nil })
	if err == nil || !strings.Contains(err.Error(), `answered "[-]etcd failed"`) {
		t.Fatalf("got %v, want the unhealthy answer", err)
	}

	if err := waitHealthz(context.Background(), func(context.Context) ([]byte, error) { return []byte("ok\n"), nil }); err != nil {
		t.Fatalf("an ok answer: %v", err)
	}
}
