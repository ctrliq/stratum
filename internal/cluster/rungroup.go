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

// A task's restart backoff doubles from the floor to the max; vars so a test can shorten them.
var (
	runGroupMinBackoff = time.Second
	runGroupMaxBackoff = 30 * time.Second
)

// runGroupUpReset is how long a task run must stay up before its early end resets the backoff.
const runGroupUpReset = 30 * time.Second

// Owns the node's in-process tasks; wait joins all: KillMode=process would cut mid-flight work.
type runGroup struct {
	ctx   context.Context
	wg    sync.WaitGroup
	names []string
}

func newRunGroup(ctx context.Context) *runGroup {
	return &runGroup{ctx: ctx}
}

// add starts fn as a named task; the name reaches the restart, panic and wait logs.
func (g *runGroup) add(name string, fn func(context.Context)) {
	g.names = append(g.names, name)
	g.wg.Add(1)
	go g.supervise(name, fn)
}

// wait blocks until every task has returned; it names them first, so a hung stop is tellable.
func (g *runGroup) wait() {
	fmt.Fprintf(os.Stderr, "stratum: waiting for background tasks to finish (%s)\n", strings.Join(g.names, ", "))
	g.wg.Wait()
}

func (g *runGroup) supervise(name string, fn func(context.Context)) {
	defer g.wg.Done()
	backoff := runGroupMinBackoff
	for g.ctx.Err() == nil {
		start := time.Now()
		runRecovered(g.ctx, name, fn)
		if g.ctx.Err() != nil {
			return
		}
		// A run that stayed up a while and then ended is a fresh failure, not part of a flap.
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

func runRecovered(ctx context.Context, name string, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "stratum: task %s panicked: %v, restarting\n%s\n",
				name, r, debug.Stack())
		}
	}()
	fn(ctx)
}
