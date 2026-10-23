package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodejob"
)

// One `stratum upgrade --cluster` at a time, cluster-wide. The lease is the
// maintenance lease machinery (internal/maintlease) taken under the upgrade's
// own released identity: its name is a cluster-wide contract, not a constant
// of this build. An orchestrator on the previous release takes
// kube-system/stratum-cluster-upgrade and reads it before upgrading; renaming
// the lease here, or dropping it early, lets an old and a new orchestrator
// upgrade the same cluster at once.
const upgradeLeaseName = "stratum-cluster-upgrade"

// Vars so tests can use a fake clock. They are the upgrade's face of the
// shared maintenance-lease seams: acquisition captures the test clock for
// each lease without changing shared package globals.
var (
	leaseDuration = 60 * time.Second
	leaseRenew    = 20 * time.Second
	leaseNow      = time.Now
	// leaseCallTimeout bounds each lease API call. Without it a blackholed
	// request hung the renewal goroutine, so a lost lease was never observed,
	// and hung release, so the run never exited.
	leaseCallTimeout = 10 * time.Second
)

func upgradeHolder() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%d", h, os.Getpid(), time.Now().UnixNano())
}

// errLeaseTaken is the shared package's taken error: renewals and confirms
// fail with it, wrapped, when another run holds the lease.
var errLeaseTaken = maintlease.ErrTaken

// leaseGuard reports whether this run still holds the lease. It is lost for
// good at once if someone else holds it. With no renewal for one renewal
// interval before expiry it is only doubtful: it may have been taken, so
// nothing new is started, but one renewal that succeeds clears it. That
// renewal is a compare-and-swap on a lease still naming this run, so nobody
// held it in between. This is what lets a run ride out its own API server
// restarting. An in-flight upgrade Job is not aborted.
//
// The guard keeps the renewal state its own confirm sees, and consults
// shared — the maintenance lease guard behind the acquired lease, fed by its
// renewal loop — for what it has not seen yet, so a takeover the loop notices
// first still stops the run.
type leaseGuard struct {
	mu        sync.Mutex
	lastRenew time.Time
	lost      error
	stale     error
	// renew is one renewal attempt, for confirm. Unset in tests that only
	// drive observe.
	renew func(context.Context) error
	// holder is this run's identity on the lease, and on its cordon claims.
	holder string
	// shared is the maintlease guard behind this run's lease. Nil on guards
	// built directly (tests, dry-run), which run on their own fields alone.
	shared *maintlease.Guard
	// maintenance is checked by every confirm, including cordon retries.
	maintenance *maintlease.Guard
}

// id is the holder, or "" for a nil guard (dry-run, tests).
func (g *leaseGuard) id() string {
	if g == nil {
		return ""
	}
	return g.holder
}

// Err is nil while the lease is held.
func (g *leaseGuard) Err() error {
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
	if err := g.shared.Err(); err != nil {
		return err
	}
	return g.maintenance.Err()
}

// confirm renews the lease now and returns nil only once that succeeded: a
// same-holder renewal is a compare-and-swap, so it proves nobody held the lease
// since the last one. Err alone cannot say that, as it only reflects the
// renewals the background loop has made so far: after a long step, such as an
// API outage, a successor may already hold the lease before the next tick sees
// it. A takeover is lost for good and wraps errLeaseTaken; any other failure
// is for the caller to retry. A nil guard (dry-run, tests) is held.
func (g *leaseGuard) confirm(ctx context.Context) error {
	if g != nil {
		if err := g.maintenance.Confirm(ctx); err != nil {
			return err
		}
	}
	if g == nil || g.renew == nil {
		return g.Err()
	}
	err := g.renew(ctx)
	g.observe(err, leaseNow())
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

// observe records one renewal attempt's outcome. Pure apart from the lock.
func (g *leaseGuard) observe(err error, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != nil {
		return
	}
	switch {
	case err == nil:
		g.lastRenew = now
		g.stale = nil
	case errors.Is(err, errLeaseTaken):
		g.lost = fmt.Errorf("the cluster-upgrade lease is no longer ours (%w); stopping before any further node is touched", err)
	case now.Sub(g.lastRenew) >= leaseDuration-leaseRenew:
		g.stale = fmt.Errorf("the cluster-upgrade lease could not be renewed for %s (%w) and may be taken by another run; "+
			"stopping before any further node is touched", now.Sub(g.lastRenew).Round(time.Second), err)
	}
}

// acquireUpgradeLease takes the cluster-upgrade lease or refuses. The lease
// mechanics live in internal/maintlease; the upgrade pins the released name
// and namespace. It returns a guard to check before destructive steps, and a
// release func that stops renewal and deletes the lease if it is still ours.
//
// A refusal names a maintenance run, not an upgrade: the certificate
// operations take this lease too, as the fence released upgrades honour
// (maintlease.AcquireCompatible), so its holder may be either.
func acquireUpgradeLease(ctx context.Context, cs kubernetes.Interface, holder string) (*leaseGuard, func(), error) {
	shared, release, err := maintlease.AcquireWithClock(ctx, cs, cuNamespace, upgradeLeaseName, holder, "maintenance run", leaseNow)
	if err != nil {
		return nil, nil, err
	}
	guard := &leaseGuard{
		lastRenew: leaseNow(),
		holder:    holder,
		renew:     shared.Confirm,
		shared:    shared,
	}
	return guard, release, nil
}

func renewUpgradeLease(ctx context.Context, cs kubernetes.Interface, holder string) error {
	return maintlease.RenewWithClock(ctx, cs, cuNamespace, upgradeLeaseName, holder, leaseNow)
}

// releaseUpgradeLease deletes the lease only while it is still ours, with the
// resourceVersion as a precondition, so it never deletes a successor's.
func releaseUpgradeLease(ctx context.Context, cs kubernetes.Interface, holder string) {
	maintlease.Release(ctx, cs, cuNamespace, upgradeLeaseName, holder)
}

// upgradeJobLabel is the label nodejob puts on every per-node upgrade Job.
const upgradeJobLabel = "stratum.ciq.com/upgrade"

// sortUpgradeJobs sorts the per-node upgrade Jobs into those still running and
// the finished ones whose node has left the cluster. A Job being deleted is
// still running: its Pod can still be upgrading the host until
// it is gone. A running Job whose node is not in present is named as such: a
// Pod may still run on a node the API server no longer lists. A Job
// with no node to resolve is never removed, since nothing says it is an
// orphan, but while it runs it still blocks like any other: it may still be
// upgrading a host. Pure.
func sortUpgradeJobs(jobs []batchv1.Job, present []string) (active []string, orphans []*batchv1.Job) {
	for i := range jobs {
		j := &jobs[i]
		node := nodejob.JobNode(j, upgradeJobLabel)
		departed := node != "" && !slices.Contains(present, node)
		switch {
		case !nodejob.Finished(j) && departed:
			active = append(active, fmt.Sprintf("%s (node %s, which is no longer in the cluster; if it is gone for good, "+
				"remove the Job: kubectl -n %s delete job %s)", j.Name, node, cuNamespace, j.Name))
		case !nodejob.Finished(j):
			active = append(active, fmt.Sprintf("%s (node %s)", j.Name, node))
		case departed:
			orphans = append(orphans, j)
		}
	}
	return active, orphans
}

// sweepUpgradeJobs runs right after the lease is taken.
//
// It removes every finished upgrade Job whose node has left the cluster.
// Neither later sweep can see one: the cluster pass walks the nodes
// the cluster has, and clearStaleUpgradeJobs selects by the node being
// upgraded. And its name can be the one a current node's Job needs, where
// nodejob.Run refuses to replace another node's Job, so that node could never
// upgrade until someone deleted it by hand.
//
// Then it refuses while any earlier upgrade Job is still running. The lease
// fences this process, not the per-node Jobs it starts: a Job outlives a run
// that dies mid-upgrade, for up to its 20-minute deadline, while the lease
// expires in 60 s. A run that takes over would then find that node still at
// the old version, and nodejob.Run would delete the "stale" Job and start
// another, interrupting the binary upgrade in progress. So a
// takeover waits for every earlier upgrade Job to finish or fail. A finished
// Job of a current node is fine: nodejob.Run replaces it, and must, so its
// stale status cannot pass the node.
func sweepUpgradeJobs(w io.Writer, ctx context.Context, cs kubernetes.Interface, present []string) error {
	jobs, err := cs.BatchV1().Jobs(cuNamespace).List(ctx, metav1.ListOptions{LabelSelector: upgradeJobLabel})
	if err != nil {
		return fmt.Errorf("check for per-node upgrade Jobs from an earlier run: %w", err)
	}
	active, orphans := sortUpgradeJobs(jobs.Items, present)
	for _, j := range orphans {
		fmt.Fprintf(w, "  removing the upgrade Job %s of node %s, which is no longer in the cluster\n", j.Name, nodejob.JobNode(j, upgradeJobLabel))
		if err := nodejob.DeleteExactAndWait(ctx, cs, j); err != nil {
			return fmt.Errorf("remove the upgrade Job %s of a node no longer in the cluster: %w", j.Name, err)
		}
	}
	if len(active) > 0 {
		return fmt.Errorf("a per-node upgrade Job from an earlier run is still running: %s. Replacing it would "+
			"interrupt that node's upgrade; wait for it to complete or fail (its deadline is 20 minutes), "+
			"then run `stratum upgrade --cluster` again", strings.Join(active, ", "))
	}
	return nil
}
