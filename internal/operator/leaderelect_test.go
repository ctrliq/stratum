package operator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
)

func electionConfig(client *fake.Clientset) leaderelection.LeaderElectionConfig {
	return leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: leaderLockName, Namespace: leaderLockNamespace},
			Client:     client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: "candidate"},
		},
		Name:          leaderLockName,
		LeaseDuration: time.Second,
		RenewDeadline: 100 * time.Millisecond,
		RetryPeriod:   10 * time.Millisecond,
	}
}

func awaitElectionSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for leader-election callback")
	}
}

func awaitElectionResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("leader election did not return")
		return nil
	}
}

func TestLeadershipLossWaitsForReconcilerAndReturnsError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := fake.NewClientset()
		var failRenew atomic.Bool
		client.PrependReactor("update", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if failRenew.Load() {
				return true, nil, errors.New("lease renewal failed")
			}
			return false, nil, nil
		})
		started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseLoop := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseLoop()
		result := make(chan error, 1)
		go func() {
			result <- runLeaderElection(ctx, electionConfig(client), func(runCtx context.Context) {
				close(started)
				<-runCtx.Done()
				close(stopped)
				<-release // Model cleanup that must finish before the caller regains control.
			})
		}()
		awaitElectionSignal(t, started)
		failRenew.Store(true)
		awaitElectionSignal(t, stopped)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("returned before reconciliation stopped: %v", err)
		default:
		}
		releaseLoop()
		if err := awaitElectionResult(t, result); !errors.Is(err, errLeadershipLost) {
			t.Fatalf("got %v, want leadership-loss error", err)
		}
		if ctx.Err() != nil {
			t.Fatal("leadership loss unexpectedly cancelled the caller's context")
		}
	})
}

func TestLeaderElectionReturnsCancellation(t *testing.T) {
	for _, leader := range []bool{false, true} {
		name := "follower"
		if leader {
			name = "leader"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := fake.NewClientset()
			if !leader {
				client = fake.NewClientset(&coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{Name: leaderLockName, Namespace: leaderLockNamespace},
					Spec: coordinationv1.LeaseSpec{
						HolderIdentity: ptr.To("other"), LeaseDurationSeconds: ptr.To(int32(3600)),
						RenewTime: ptr.To(metav1.NewMicroTime(time.Now())),
					},
				})
			}
			observed, started, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			client.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
				once.Do(func() { close(observed) })
				return false, nil, nil
			})
			result := make(chan error, 1)
			go func() {
				result <- runLeaderElection(ctx, electionConfig(client), func(runCtx context.Context) {
					close(started)
					<-runCtx.Done()
					close(stopped)
				})
			}()
			if leader {
				awaitElectionSignal(t, started)
			} else {
				awaitElectionSignal(t, observed)
			}
			cancel()
			if err := awaitElectionResult(t, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want cancellation", err)
			}
			if leader {
				select {
				case <-stopped:
				default:
					t.Fatal("returned while the reconciler was still running")
				}
			} else {
				select {
				case <-started:
					t.Fatal("reconciled without acquiring leadership")
				default:
				}
			}
		})
	}
}

func TestLeaderElectionInvalidConfigReturnsError(t *testing.T) {
	cfg := electionConfig(fake.NewClientset())
	cfg.RenewDeadline = cfg.LeaseDuration
	if err := runLeaderElection(context.Background(), cfg, func(context.Context) {
		t.Error("started with an invalid election configuration")
	}); err == nil {
		t.Fatal("expected a configuration error")
	}
}
