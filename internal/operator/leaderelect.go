// leaderelect.go — single-active-instance leader election via a Lease.
//
// runLeaderElected campaigns for a coordination.k8s.io Lease
// (kube-system/stratum-operator) and only runs the reconcile loop while this
// instance holds it. This lets multiple control-plane hosts run stratum-operator
// for HA while guaranteeing exactly one actively reconciles (the HelmChart and
// manifests controllers make mutating cluster changes — two concurrent writers
// would fight).
//
// On losing leadership we stop the reconcile loop and return an error. The
// standalone CLI exits nonzero so its supervisor can restart it; importers
// retain control of their own process and cleanup.
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
	// leaderLockName is k8sutil.OperatorLeaseName: the Lease is a shared
	// engine identity (the dashboard's SPA reads the same Lease to report an
	// operator live), declared once in k8sutil and named here as the election
	// code and its tests call it.
	leaderLockName = k8sutil.OperatorLeaseName
)

// runLeaderElected blocks, campaigning for the stratum-operator Lease. When this
// instance becomes leader it invokes run(ctx); run should honor ctx and return
// when it is cancelled (loss of leadership cancels it). id is this instance's
// unique identity (the hostname).
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
		// Let the lease expire: the elector cancels reconciliation only after
		// its renew loop returns. Releasing it inside that loop would allow a
		// peer to start while this process is still stopping its reconciler.
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

// The elector launches OnStartedLeading asynchronously and does not wait for
// it when Run returns. Guard its start against shutdown and join any running
// callback before returning control to the caller.
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
