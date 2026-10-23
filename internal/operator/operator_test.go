package operator

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// Regression guards for the leader's two reconcile loops: HelmChart
// reconciliation on its own goroutine and ticker, and the shared pass of
// cluster controllers — manifests deployer, CoreDNS spreader, departed-worker
// cleanup — that the kube-system/stratum-operator Lease holder runs once and
// then on every tick. They pin the guarantees through the runLeaderLoop seam
// under a synctest bubble, whose fake clock drives the tickers and the
// per-iteration deadlines: no real-clock sleeps, no skips. runLeaderLoop is
// the body of the leader callback, so a guarantee pinned here — the helm loop
// joined before runLeaderLoop returns — is the callback's guarantee: the
// elector joins the callback before reporting leadership loss
// (leaderelect_test.go).

const testInterval = 20 * time.Second

const (
	helmStepName     = "helm"
	deployerStepName = "deployer"
	spreadStepName   = "spread"
	cleanupStepName  = "cleanup"
)

// stepRecord is one run of a loop step: its name, the bubble-clock time it
// started, and the deadline and liveness of the context it received.
type stepRecord struct {
	name     string
	start    time.Time
	deadline time.Time
	live     bool
}

// stepRecorder records every run of the leader loop's steps. Recording is
// guarded by a mutex, because a later change may move a step onto its own
// goroutine, and the guards must survive that.
type stepRecorder struct {
	mu          sync.Mutex
	records     []stepRecord
	inFlight    int
	maxInFlight int
}

func (r *stepRecorder) record(name string, ctx context.Context) {
	deadline, _ := ctx.Deadline()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, stepRecord{name: name, start: time.Now(), deadline: deadline, live: ctx.Err() == nil})
}

func (r *stepRecorder) enter() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
}

func (r *stepRecorder) leave() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
}

// runsOf reports how many times the step ran, counting the run in progress.
func (r *stepRecorder) runsOf(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.records {
		if rec.name == name {
			n++
		}
	}
	return n
}

func (r *stepRecorder) recordsOf(name string) []stepRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []stepRecord
	for _, rec := range r.records {
		if rec.name == name {
			out = append(out, rec)
		}
	}
	return out
}

// passTimes returns the distinct start times of the recorded passes, in order.
// It is only meaningful for loops whose steps do not sleep, where every step
// of a pass starts at the same bubble-clock instant.
func (r *stepRecorder) passTimes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []time.Time
	for _, rec := range r.records {
		if len(out) == 0 || !rec.start.Equal(out[len(out)-1]) {
			out = append(out, rec.start)
		}
	}
	return out
}

func (r *stepRecorder) namesAt(start time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		if rec.start.Equal(start) {
			out = append(out, rec.name)
		}
	}
	return out
}

// startTimesOf returns the recorded start times of one step's runs, in order.
func startTimesOf(records []stepRecord) []time.Time {
	out := make([]time.Time, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.start)
	}
	return out
}

// maxConcurrent reports the largest number of steps ever in flight at once.
func (r *stepRecorder) maxConcurrent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxInFlight
}

// stepOptions tell a recording step how to behave beyond recording: block
// until its context ends, block until released, sleep on the bubble's clock,
// panic, or run a hook with its 1-based run number.
type stepOptions struct {
	blockUntilDone bool
	release        chan struct{}
	sleep          time.Duration
	panicMsg       string
	onRun          func(run int)
}

// newStep returns a leaderStep that records its name, its start on the
// bubble's clock and the context it received, tracks its concurrency, then
// behaves as its options direct.
func (r *stepRecorder) newStep(name string, opts stepOptions) func(context.Context) {
	return func(ctx context.Context) {
		r.record(name, ctx)
		r.enter()
		defer r.leave()
		if opts.onRun != nil {
			opts.onRun(r.runsOf(name))
		}
		if opts.blockUntilDone {
			<-ctx.Done()
		}
		if opts.release != nil {
			<-opts.release
		}
		if opts.sleep > 0 {
			time.Sleep(opts.sleep)
		}
		if opts.panicMsg != "" {
			panic(opts.panicMsg)
		}
	}
}

// clusterSequence filters the HelmChart step out of a recorded pass: the
// guards pin the cluster controllers' order, not where HelmChart work sits in
// the pass.
func clusterSequence(names []string) []string {
	var out []string
	for _, n := range names {
		if n != helmStepName {
			out = append(out, n)
		}
	}
	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func countName(names []string, name string) int {
	n := 0
	for _, got := range names {
		if got == name {
			n++
		}
	}
	return n
}

// TestLeaderPassRunsClusterControllersInOrder pins the pass's gating, order
// and cadence: the manifests deployer only with a manifests directory, CoreDNS
// spreading only with one and leader election, departed-worker cleanup always;
// the cluster controllers in that order; the first pass before the first tick
// and the next one one interval later, so a new leader applies the bundled
// add-ons at once and then on every tick.
func TestLeaderPassRunsClusterControllersInOrder(t *testing.T) {
	for _, row := range []struct {
		name         string
		manifestsDir bool
		leaderElect  bool
		wantSequence []string
	}{
		{"manifests directory and leader election", true, true, []string{deployerStepName, spreadStepName, cleanupStepName}},
		{"manifests directory without leader election", true, false, []string{deployerStepName, cleanupStepName}},
		{"no manifests directory with leader election", false, true, []string{cleanupStepName}},
		{"no manifests directory or leader election", false, false, []string{cleanupStepName}},
	} {
		t.Run(row.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &stepRecorder{}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cleanup := rec.newStep(cleanupStepName, stepOptions{onRun: func(run int) {
					if run >= 2 {
						cancel()
					}
				}})
				var deployer, spread func(context.Context)
				if row.manifestsDir {
					deployer = rec.newStep(deployerStepName, stepOptions{})
					spread = rec.newStep(spreadStepName, stepOptions{})
				}
				helm := rec.newStep(helmStepName, stepOptions{})
				loopStart := time.Now()
				loopDone := make(chan struct{})
				go func() {
					defer close(loopDone)
					runLeaderLoop(ctx, testInterval, row.manifestsDir, row.leaderElect, helm, deployer, spread, cleanup)
				}()
				<-loopDone

				passes := rec.passTimes()
				if len(passes) != 2 {
					t.Fatalf("loop ran %d passes, want 2", len(passes))
				}
				if !passes[0].Equal(loopStart) {
					t.Fatalf("first pass started at %v, want %v: the pass must run before the first tick, so a new leader applies the bundled add-ons at once", passes[0], loopStart)
				}
				if !passes[1].Equal(loopStart.Add(testInterval)) {
					t.Fatalf("second pass started at %v, want %v: the ticker must drive the next pass one interval later", passes[1], loopStart.Add(testInterval))
				}
				for _, start := range passes {
					if got := clusterSequence(rec.namesAt(start)); !equalNames(got, row.wantSequence) {
						t.Fatalf("pass at %v ran cluster controllers %v, want %v", start, got, row.wantSequence)
					}
				}
			})
		})
	}
}

// TestLeaderPassGivesEachClusterControllerItsOwnDeadline pins the deadlines:
// the deployer's expires perIterationTimeout after the deployer step itself
// starts — not after the pass starts — so a HelmChart pass longer than the
// iteration window cannot spend the deployer's budget; and spreading and
// cleanup each start with a live context whose deadline is perIterationTimeout
// after their own start, so a slow manifest pass cannot spend theirs. The
// HelmChart step takes simulated time longer than the whole window, so a
// deadline anchored to the pass's start is visibly expired, or nearly so,
// before the deployer runs at all.
func TestLeaderPassGivesEachClusterControllerItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stepRecorder{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		helm := rec.newStep(helmStepName, stepOptions{sleep: 2 * perIterationTimeout})
		deployer := rec.newStep(deployerStepName, stepOptions{blockUntilDone: true})
		spread := rec.newStep(spreadStepName, stepOptions{})
		cleanup := rec.newStep(cleanupStepName, stepOptions{onRun: func(int) { cancel() }})
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			// Keep the ticker beyond the whole pass — HelmChart work plus the
			// blocked deployer: the loop may select a pending tick beside
			// cancellation.
			runLeaderLoop(ctx, 4*perIterationTimeout, true, true, helm, deployer, spread, cleanup)
		}()
		<-loopDone

		depRuns := rec.recordsOf(deployerStepName)
		if len(depRuns) != 1 {
			t.Fatalf("deployer ran %d times, want 1", len(depRuns))
		}
		dep := depRuns[0]
		if dep.deadline.IsZero() {
			t.Fatal("deployer step received a context with no deadline, want a per-iteration deadline so a stuck apiserver call cannot wedge the loop")
		}
		if window := dep.deadline.Sub(dep.start); window != perIterationTimeout {
			t.Fatalf("deployer deadline expired %v after the deployer step started, want %v: the deadline must start when the deployer starts, not with the pass — a HelmChart pass longer than the window must not spend the deployer's budget", window, perIterationTimeout)
		}
		if !dep.live {
			t.Fatal("deployer step started with a context that was already done, want a live one: its deadline must be created when the deployer starts, not before the HelmChart step")
		}
		for _, name := range []string{spreadStepName, cleanupStepName} {
			runs := rec.recordsOf(name)
			if len(runs) != 1 {
				t.Fatalf("%s ran %d times, want 1", name, len(runs))
			}
			got := runs[0]
			if !got.live {
				t.Fatalf("%s started with a context that was already done, want a live one", name)
			}
			if window := got.deadline.Sub(got.start); window != perIterationTimeout {
				t.Fatalf("%s deadline expired %v after its own start, want %v: spreading and cleanup each need a fresh deadline the deployer cannot spend", name, window, perIterationTimeout)
			}
			if got.start.Before(dep.deadline) {
				t.Fatalf("%s started at %v, before the deployer's deadline expired at %v: the deployer must block until its context ends first", name, got.start, dep.deadline)
			}
		}
	})
}

// TestLeaderPassKeepsClusterControllersOnCadenceWhenHelmBlocks pins that the
// shared pass does not wait behind HelmChart work: a helmStep blocked for
// four intervals — far past perIterationTimeout — must leave the deployer,
// spreader and cleanup running their own pass on every tick, on cadence, and
// must not start a second helm pass over the one still in flight. While
// HelmChart work runs inside the sequential pass, a blocked helmStep holds
// every cluster controller back instead.
func TestLeaderPassKeepsClusterControllersOnCadenceWhenHelmBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stepRecorder{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		release := make(chan struct{})
		helm := rec.newStep(helmStepName, stepOptions{release: release, onRun: func(run int) {
			if run == 1 {
				close(started)
			}
		}})
		deployer := rec.newStep(deployerStepName, stepOptions{})
		spread := rec.newStep(spreadStepName, stepOptions{})
		cleanup := rec.newStep(cleanupStepName, stepOptions{})
		loopStart := time.Now()
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			runLeaderLoop(ctx, testInterval, true, true, helm, deployer, spread, cleanup)
		}()
		<-started
		time.Sleep(4 * testInterval)
		synctest.Wait()
		// Snapshot before releasing HelmChart work: shutdown must not
		// satisfy the cadence pins.
		helmRuns := rec.runsOf(helmStepName)
		clusterRecords := make(map[string][]stepRecord)
		for _, name := range []string{deployerStepName, spreadStepName, cleanupStepName} {
			clusterRecords[name] = rec.recordsOf(name)
		}
		close(release)
		cancel()
		<-loopDone

		if helmRuns != 1 {
			t.Fatalf("helm step ran %d times while blocked, want 1: the helm loop must not start another pass over the one in flight", helmRuns)
		}
		for _, name := range []string{deployerStepName, spreadStepName, cleanupStepName} {
			records := clusterRecords[name]
			if len(records) < 3 {
				t.Fatalf("%s ran %d times while HelmChart work was blocked for four intervals, want at least 3: the cluster controllers must keep their own pass cadence instead of waiting behind HelmChart work", name, len(records))
			}
			for i, got := range startTimesOf(records) {
				if want := loopStart.Add(time.Duration(i) * testInterval); !got.Equal(want) {
					t.Fatalf("%s pass %d started at %v, want %v: the cluster controllers must run one pass per interval of their own while HelmChart work is blocked", name, i+1, got, want)
				}
			}
		}
	})
}

// TestLeaderHelmStepRunsOnItsOwnTickerWhenAClusterControllerBlocks pins the
// mirror side of the two-loop shape: HelmChart reconciliation runs on its own
// ticker inside the leader callback, so a deployer blocked for four intervals
// must not stop the helm step from starting one pass per interval of its own.
// While HelmChart work runs inside the sequential pass, a blocked deployer
// holds it back after its first run.
func TestLeaderHelmStepRunsOnItsOwnTickerWhenAClusterControllerBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stepRecorder{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		release := make(chan struct{})
		helm := rec.newStep(helmStepName, stepOptions{})
		deployer := rec.newStep(deployerStepName, stepOptions{release: release, onRun: func(run int) {
			if run == 1 {
				close(started)
			}
		}})
		spread := rec.newStep(spreadStepName, stepOptions{})
		cleanup := rec.newStep(cleanupStepName, stepOptions{})
		loopStart := time.Now()
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			runLeaderLoop(ctx, testInterval, true, true, helm, deployer, spread, cleanup)
		}()
		<-started
		time.Sleep(4 * testInterval)
		synctest.Wait()
		// Snapshot before releasing the deployer: shutdown must not satisfy
		// the cadence pin.
		helmRecords := rec.recordsOf(helmStepName)
		close(release)
		cancel()
		<-loopDone

		if len(helmRecords) < 3 {
			t.Fatalf("helm step ran %d times in four intervals, want at least 3: HelmChart work must run on its own ticker instead of waiting behind a blocked cluster controller", len(helmRecords))
		}
		for i, got := range startTimesOf(helmRecords) {
			if want := loopStart.Add(time.Duration(i) * testInterval); !got.Equal(want) {
				t.Fatalf("helm pass %d started at %v, want %v: the helm loop must keep one pass per interval of its own", i+1, got, want)
			}
		}
	})
}

// TestLeaderPassDoesNotBoundHelmChartWorkByTheIterationWindow pins that the
// HelmChart step runs under the loop's context with no deadline, so an install
// that needs longer than the 30-second iteration window but less than its
// chart's spec.timeout completes. The step cancels the loop's context itself,
// after its work: the cluster controllers run on their own loop and do not
// wait behind HelmChart work, so a cancellation from another step would end the
// helm loop's context before the step could prove it stayed live.
func TestLeaderPassDoesNotBoundHelmChartWorkByTheIterationWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stepRecorder{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		helm := func(helmCtx context.Context) {
			rec.record(helmStepName, helmCtx)
			if deadline, _ := helmCtx.Deadline(); !deadline.IsZero() {
				t.Errorf("helm step received a context with deadline %v, want none: a chart is bounded by its own spec.timeout, not by the iteration window", deadline)
			}
			time.Sleep(2 * time.Minute)
			if err := helmCtx.Err(); err != nil {
				t.Errorf("helm step context was %v after two minutes of work, want still live", err)
			}
			cancel()
		}
		cleanup := rec.newStep(cleanupStepName, stepOptions{})
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			// No tick is pending when the helm step cancels after its
			// two-minute pass; this assertion concerns its context, not
			// select order.
			runLeaderLoop(ctx, 3*time.Minute, false, false, helm, nil, nil, cleanup)
		}()
		<-loopDone

		if runs := rec.runsOf(helmStepName); runs != 1 {
			t.Fatalf("helm step ran %d times, want 1", runs)
		}
		if runs := rec.runsOf(cleanupStepName); runs != 1 {
			t.Fatalf("cleanup ran %d times, want 1: the shared pass must run beside HelmChart work that outlasts the iteration window", runs)
		}
	})
}

// TestLeaderPassSurvivesAPanickingStep pins the panic recovery: a panic in any
// one step stops neither the later steps of that pass nor the next pass, so
// the loop and the process survive a broken step.
func TestLeaderPassSurvivesAPanickingStep(t *testing.T) {
	for _, victim := range []string{helmStepName, deployerStepName, spreadStepName, cleanupStepName} {
		t.Run(victim, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &stepRecorder{}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				optsFor := func(name string) stepOptions {
					var opts stepOptions
					if name == victim {
						opts.panicMsg = name + " step panicked"
					}
					return opts
				}
				helm := rec.newStep(helmStepName, optsFor(helmStepName))
				deployer := rec.newStep(deployerStepName, optsFor(deployerStepName))
				spread := rec.newStep(spreadStepName, optsFor(spreadStepName))
				cleanupOpts := optsFor(cleanupStepName)
				cleanupOpts.onRun = func(run int) {
					if run >= 2 {
						// Cancel just past the tick instant: in the
						// two-loop shape the helm loop's ticker and the
						// shared pass's fire together, and a cancellation
						// landing exactly on that instant would race the
						// helm loop's select, leaving whether one more
						// helm pass starts to the scheduler.
						time.Sleep(time.Millisecond)
						cancel()
					}
				}
				cleanup := rec.newStep(cleanupStepName, cleanupOpts)
				loopDone := make(chan struct{})
				go func() {
					defer close(loopDone)
					runLeaderLoop(ctx, testInterval, true, true, helm, deployer, spread, cleanup)
				}()
				<-loopDone

				passes := rec.passTimes()
				if len(passes) != 2 {
					t.Fatalf("loop ran %d passes, want 2: the loop must survive the panic and run again on the next tick", len(passes))
				}
				for i, start := range passes {
					got := rec.namesAt(start)
					for _, name := range []string{helmStepName, deployerStepName, spreadStepName, cleanupStepName} {
						if n := countName(got, name); n != 1 {
							t.Fatalf("pass %d ran step %q %d times, want exactly once: a panic in one step must not stop the others", i+1, name, n)
						}
					}
					if cluster := clusterSequence(got); !equalNames(cluster, []string{deployerStepName, spreadStepName, cleanupStepName}) {
						t.Fatalf("pass %d ran cluster controllers %v, want [%s %s %s]", i+1, cluster, deployerStepName, spreadStepName, cleanupStepName)
					}
				}
				if runs := rec.runsOf(victim); runs != 2 {
					t.Fatalf("panicking step %q ran %d times, want 2: it must run again on the next tick", victim, runs)
				}
			})
		})
	}
}

// TestLeaderLoopWaitsForHelmChartWorkBeforeReturning pins that the leader
// callback does not return while HelmChart work is still running: the helm
// loop is joined before the callback returns, so a demoted leader neither goes
// on installing charts while the new leader reconciles the same releases nor
// starts new helm work after leadership loss. The elector joins this callback
// before returning errLeadershipLost (leaderelect_test.go), which joins the
// helm loop with it.
func TestLeaderLoopWaitsForHelmChartWorkBeforeReturning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stepRecorder{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		started := make(chan struct{})
		helm := func(helmCtx context.Context) {
			rec.record(helmStepName, helmCtx)
			close(started)
			<-release
		}
		cleanup := rec.newStep(cleanupStepName, stepOptions{})
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			runLeaderLoop(ctx, testInterval, false, false, helm, nil, nil, cleanup)
		}()
		<-started
		cancel()
		synctest.Wait()
		select {
		case <-loopDone:
			t.Fatal("leader loop returned while HelmChart work was still running")
		default:
		}
		close(release)
		<-loopDone
		if runs := rec.runsOf(helmStepName); runs != 1 {
			t.Fatalf("helm step ran %d times, want 1: after cancellation the helm loop must join the in-flight pass and start no new one", runs)
		}
	})
}

// TestLeaderLoopNeverOverlapsHelmChartPasses pins that one HelmChart pass
// never overlaps another: the loop starts the next pass only after the
// previous one returns, so no chart is reconciled twice at once.
func TestLeaderLoopNeverOverlapsHelmChartPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &stepRecorder{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		// Only Helm contributes to the concurrency recorder: cleanup may
		// overlap Helm without two Helm passes ever running at once.
		helm := rec.newStep(helmStepName, stepOptions{sleep: 70 * time.Second, onRun: func(run int) {
			if run == 1 {
				close(started)
			}
		}})
		cleanup := func(context.Context) {}
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			runLeaderLoop(ctx, testInterval, false, false, helm, nil, nil, cleanup)
		}()
		<-started
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		// Snapshot the observation window before cancellation and joining:
		// calls during shutdown must not satisfy the minimum cadence.
		max, runs := rec.maxConcurrent(), rec.runsOf(helmStepName)
		cancel()
		<-loopDone

		if max > 1 {
			t.Fatalf("up to %d HelmChart passes were in flight at once, want at most one: two passes would reconcile the same chart at the same time", max)
		}
		if runs < 4 {
			t.Fatalf("helm step ran %d times in five minutes, want at least 4: a pass must start after the previous one returns, not after each tick is dropped", runs)
		}
	})
}

// TestGroupConstantIsAnAliasOfK8sutilHelmChartGroup pins the operator's group
// constant to its one declared source, k8sutil.HelmChartGroup — the same pair
// the HelmChart CRDs, the dynamic client and the webhook's admission rule
// already read. group is the domain of the operator's annotations and labels
// (requester stamps, managed-by, stack names); that it coincides with the
// HelmChart API group must be spelled through the alias, not as a second
// string literal that could drift from internal/k8sutil while every GVR/GVK
// construction follows it. An alias and a same-valued copy are
// indistinguishable through the compiled value — both are "stratum.ciq.com"
// today — so the test reads the declaration in operator.go itself and refuses
// a string literal on the right-hand side.
func TestGroupConstantIsAnAliasOfK8sutilHelmChartGroup(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate this test file, so the group declaration in operator.go cannot be read")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "operator.go"))
	if err != nil {
		t.Fatalf("reading operator.go: %v", err)
	}
	decl := regexp.MustCompile(`^group\s*=\s*(.*)$`)
	var rhs string
	for n, line := range strings.Split(string(src), "\n") {
		if m := decl.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if rhs != "" {
				t.Fatalf("operator.go:%d: group is declared a second time — one constant, aliased to k8sutil.HelmChartGroup, is the whole point", n+1)
			}
			rhs = m[1]
		}
	}
	if rhs == "" {
		t.Fatal("operator.go declares no group constant — the annotation and label domain has no single declaration to pin")
	}
	// Drop a trailing // comment before comparing the expression itself.
	if i := strings.Index(rhs, "//"); i >= 0 {
		rhs = strings.TrimSpace(rhs[:i])
	}
	if rhs != "k8sutil.HelmChartGroup" {
		t.Errorf("operator.go declares group = %s, want k8sutil.HelmChartGroup — a second copy of the group string can drift from the one declaration in internal/k8sutil that the CRDs, the dynamic client and the webhook rule read", rhs)
	}
	if group != k8sutil.HelmChartGroup {
		t.Errorf("group = %q, want k8sutil.HelmChartGroup %q — annotation and label keys built from group would change under the requester stamps already stored on charts", group, k8sutil.HelmChartGroup)
	}
	if group != "stratum.ciq.com" {
		t.Errorf("group = %q, want %q — the operator's annotation and label keys (requester stamps, managed-by, stack names) must keep the same string", group, "stratum.ciq.com")
	}
}

// TestParseFlagsClusterDomainDefaultsToClusterLocal: --cluster-domain parses
// to the given domain, and to the default domain when it is not given.
func TestParseFlagsClusterDomainDefaultsToClusterLocal(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "cluster.local"},
		{[]string{"--cluster-domain", "fuzz.internal"}, "fuzz.internal"},
	} {
		o, err := ParseFlags(tc.args)
		if err != nil {
			t.Fatalf("ParseFlags(%q): %v", tc.args, err)
		}
		if o.ClusterDomain != tc.want {
			t.Errorf("ParseFlags(%q).ClusterDomain = %q, want %q", tc.args, o.ClusterDomain, tc.want)
		}
	}
}
