package operator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

var errLeadershipLost = errors.New("operator lost leadership")

const (
	leaderLockNamespace = "kube-system"
	// Shared engine identity declared once in k8sutil; the dashboard's SPA reads the same Lease.
	leaderLockName = k8sutil.OperatorLeaseName
)

// runLeaderElected blocks while campaigning; run must return when its ctx is cancelled.
func runLeaderElected(ctx context.Context, cfg *rest.Config, id string, run func(context.Context)) error {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating leader-election client: %w", err)
	}

	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		leaderLockNamespace,
		leaderLockName,
		client.CoreV1(),
		client.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: id},
	)
	if err != nil {
		return fmt.Errorf("creating leader-election lock: %w", err)
	}

	return runLeaderElection(ctx, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		Name:          leaderLockName,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		// ReleaseOnCancel would let a peer start while this process is still stopping its reconciler.
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnNewLeader: func(leader string) {
				if leader != id {
					log.Printf("leaderelection: %s is now leader; %s standing by", leader, id)
				}
			},
		},
	}, run)
}

// The elector launches OnStartedLeading asynchronously and does not wait for it when Run returns.
func runLeaderElection(ctx context.Context, cfg leaderelection.LeaderElectionConfig, run func(context.Context)) error {
	var mu sync.Mutex
	var stopped bool
	var running sync.WaitGroup
	cfg.Callbacks.OnStartedLeading = func(runCtx context.Context) {
		mu.Lock()
		if stopped || runCtx.Err() != nil {
			mu.Unlock()
			return
		}
		running.Add(1)
		mu.Unlock()
		defer running.Done()
		log.Printf("leaderelection: %s acquired lease %s — reconciling", cfg.Lock.Identity(), cfg.Lock.Describe())
		run(runCtx)
	}
	cfg.Callbacks.OnStoppedLeading = func() {}
	elector, err := leaderelection.NewLeaderElector(cfg)
	if err != nil {
		return fmt.Errorf("configuring leader election: %w", err)
	}
	elector.Run(ctx)
	mu.Lock()
	stopped = true
	mu.Unlock()
	running.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return errLeadershipLost
}
