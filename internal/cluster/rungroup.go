package cluster

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// The restart rule for a run-group task is the supervisor's own
// (internal/supervisor): a task that ends before shutdown is started again
// after a backoff that doubles from runGroupMinBackoff to runGroupMaxBackoff,
// reset to the floor when the previous run stayed up long enough
// (runGroupUpReset) that its end is a fresh failure rather than part of a
// flap. Vars so a test can shorten them.
var (
	runGroupMinBackoff = time.Second
	runGroupMaxBackoff = 30 * time.Second
)

// runGroupUpReset is how long a task run must stay up before its early end
// resets the restart backoff to the floor.
const runGroupUpReset = 30 * time.Second

// runGroup owns the controller's in-process tasks the way internal/supervisor
// owns its child processes: each task runs with the controller's context
// until that context is cancelled, a panicking or early-returning task is
// recovered, logged by name, and restarted with backoff instead of taking the
// controller down or dying silently, and wait joins every task before
// RunController returns. Without that join, `stratum up` could
// print "stratum: stopped" and release the node-start lock while a cert
// rotation or an etcd snapshot save was still mid-flight, and systemd's
// KillMode=process could cut that work down.
type runGroup struct {
	ctx   context.Context
	wg    sync.WaitGroup
	names []string
}

func newRunGroup(ctx context.Context) *runGroup {
	return &runGroup{ctx: ctx}
}

// add starts fn as a named task. The name reaches the restart and panic logs,
// so an operator reading them can tell which loop is misbehaving, and the
// wait log, so a stop that waits out in-flight work says what it waits for.
func (g *runGroup) add(name string, fn func(context.Context)) {
	g.names = append(g.names, name)
	g.wg.Add(1)
	go g.supervise(name, fn)
}

// wait blocks until every task added to the group has returned. RunController
// returns through it, which is what makes its return mean the in-process work
// has stopped. It names the tasks before it blocks: supervisor.Run prints
// nothing at shutdown, so between the context's end and "stratum: stopped"
// the journal is otherwise silent while a cert rotation serves out its
// restart budgets or a snapshot save uploads — minutes in which a hung stop
// is indistinguishable from a prompt one.
func (g *runGroup) wait() {
	fmt.Fprintf(os.Stderr, "stratum: waiting for background tasks to finish (%s)\n", strings.Join(g.names, ", "))
	g.wg.Wait()
}

// supervise runs one task under the group's restart rule until the context
// ends. A task is meant to run until shutdown (the loops postStart hands the
// group are); one that returns early has not finished its job, so it is
// started again after a backoff — never instantly, which would be a busy
// loop, and never not at all, which would be a dead loop that still looks
// alive to the operator.
func (g *runGroup) supervise(name string, fn func(context.Context)) {
	defer g.wg.Done()
	backoff := runGroupMinBackoff
	for g.ctx.Err() == nil {
		start := time.Now()
		runRecovered(g.ctx, name, fn)
		if g.ctx.Err() != nil {
			return
		}
		// A run that stayed up a while and then ended is a fresh failure,
		// not part of a flap; reset the backoff.
		if time.Since(start) > runGroupUpReset {
			backoff = runGroupMinBackoff
		}
		fmt.Fprintf(os.Stderr, "stratum: task %s ended before shutdown, restarting in %v\n", name, backoff)
		select {
		case <-g.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > runGroupMaxBackoff {
			backoff = runGroupMaxBackoff
		}
	}
}

// runRecovered runs fn once, recovering a panic so one broken task cannot
// take the controller down with it — the supervisor gives containerd and
// kubelet this, and the in-process tasks get it here. The panic is logged
// naming the task and the value; the restart rule then starts the task
// again, because a renewal loop that died once is still supposed to renew
// tomorrow.
func runRecovered(ctx context.Context, name string, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "stratum: task %s panicked: %v, restarting\n%s\n",
				name, r, debug.Stack())
		}
	}()
	fn(ctx)
}
