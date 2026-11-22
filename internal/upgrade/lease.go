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

// A released identity; renaming it lets an old and a new orchestrator run at once.
const upgradeLeaseName = "stratum-cluster-upgrade"

// Vars so tests can use a fake clock.
var (
	leaseDuration = 60 * time.Second
	leaseRenew    = 20 * time.Second
	leaseNow      = time.Now
	// Without it a blackholed request hangs renewal and release, so a lost lease is never observed.
	leaseCallTimeout = 10 * time.Second
)

func upgradeHolder() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%d", h, os.Getpid(), time.Now().UnixNano())
}

var errLeaseTaken = maintlease.ErrTaken

// A missed window is only doubtful; a takeover is lost for good; a same-holder renewal clears it.
type leaseGuard struct {
	mu        sync.Mutex
	lastRenew time.Time
	lost      error
	stale     error
	// Unset in tests that only drive observe.
	renew  func(context.Context) error
	holder string
	// Nil in tests and dry-run, which run on their own fields alone.
	shared      *maintlease.Guard
	maintenance *maintlease.Guard
}

func (g *leaseGuard) id() string {
	if g == nil {
		return ""
	}
	return g.holder
}

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

// A same-holder renewal is a compare-and-swap proving nobody held the lease since.
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
	// A terminal answer is returned as it came, whatever state the guard is in.
	if err != nil && terminalAPIError(err) {
		return err
	}
	if lerr := g.Err(); lerr != nil {
		return lerr
	}
	return err
}

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

// A refusal may name a maintenance run: certificate operations take this lease too.
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

// Never deletes a successor's lease; the delete carries a resourceVersion precondition.
func releaseUpgradeLease(ctx context.Context, cs kubernetes.Interface, holder string) {
	maintlease.Release(ctx, cs, cuNamespace, upgradeLeaseName, holder)
}

const upgradeJobLabel = "stratum.ciq.com/upgrade"

// A Job being deleted may still be upgrading its host; one with no node is never removed.
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

// The lease fences this process, not the Jobs it started; a takeover waits for running Jobs.
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
