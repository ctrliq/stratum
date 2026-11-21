package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// fakeHostKubectl points BinDir at a kubectl that succeeds at once, off the host's state.
func fakeHostKubectl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := version.BinDir
	version.BinDir = dir
	t.Cleanup(func() { version.BinDir = old })
}

// loadClusterConfig loads exactly this body, not the host's /etc/stratum/config.yaml.
func loadClusterConfig(t *testing.T, yaml string) *ClusterConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig = %v, want nil", err)
	}
	return c
}

// redirectStateArea points the node's state area at a temp dir, off /var/lib/stratum.
func redirectStateArea(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := version.K8sConfDir
	version.K8sConfDir = dir
	t.Cleanup(func() { version.K8sConfDir = old })
	return dir
}

// stubLoops points the loop seams at counting stubs ticking on the given interval.
func stubLoops(t *testing.T, tick time.Duration) (renewals, snapshots func() int32) {
	t.Helper()
	redirectStateArea(t)
	oldRenewInterval, oldRenew, oldSnapInterval, oldSnap := certRenewInterval, certRenewRotate, snapshotInterval, snapshotSave
	oldKubeletAdmin, oldKubeVip, oldClusterAdmins := ensureKubeletAPIAdminBinding, ensureKubeVipRBAC, ensureClusterAdminsBinding
	oldDrift, oldRotateLeaves := leafSubjectDrift, rotateLeaves
	t.Cleanup(func() {
		certRenewInterval, certRenewRotate, snapshotInterval, snapshotSave = oldRenewInterval, oldRenew, oldSnapInterval, oldSnap
		ensureKubeletAPIAdminBinding, ensureKubeVipRBAC, ensureClusterAdminsBinding = oldKubeletAdmin, oldKubeVip, oldClusterAdmins
		leafSubjectDrift, rotateLeaves = oldDrift, oldRotateLeaves
	})
	leafSubjectDrift = func() ([]string, error) { return nil, nil }
	rotateLeaves = func(context.Context) error { t.Error("rotateLeaves called without drift"); return nil }
	ensureKubeletAPIAdminBinding = func(context.Context) error { return nil }
	ensureKubeVipRBAC = func(context.Context) error { return nil }
	ensureClusterAdminsBinding = func(context.Context, string) error { return nil }
	certRenewInterval = tick
	var renewN, snapN atomic.Int32
	certRenewRotate = func(context.Context) error { renewN.Add(1); return nil }
	snapshotInterval = func(*ClusterConfig) time.Duration { return tick }
	snapshotSave = func(context.Context, *ClusterConfig) (string, error) { snapN.Add(1); return "", nil }
	return renewN.Load, snapN.Load
}

func TestRunGroupRestartsAnEarlyReturningTaskWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)

		var runs atomic.Int32
		g.add("flapper", func(context.Context) { runs.Add(1) })
		synctest.Wait()
		if got := runs.Load(); got != 1 {
			t.Fatalf("task ran %d times before any time passed, want 1: the restart must wait out a backoff, not spin", got)
		}

		time.Sleep(time.Minute)
		synctest.Wait()
		if got := runs.Load(); got < 2 {
			t.Fatalf("task ran %d times in a minute, want at least one backoff restart", got)
		}

		waitDone := make(chan struct{})
		go func() { g.wait(); close(waitDone) }()
		cancel()
		<-waitDone
		stopped := runs.Load()
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := runs.Load(); got != stopped {
			t.Fatalf("task ran %d times after the context ended (was %d): the restarts must stop", got, stopped)
		}
	})
}

func TestRunGroupRecoversLogsAndRestartsAPanickingTask(t *testing.T) {
	// captured with a pipe, as netconfig_test.go does; one log line cannot fill the buffer
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origErr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = origErr
		w.Close()
		r.Close()
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)

		var runs atomic.Int32
		releasePanicker, releaseSteady := make(chan struct{}), make(chan struct{})
		g.add("panicker", func(context.Context) {
			if runs.Add(1) == 1 {
				// a value the task name does not contain, so the log assertion checks for both
				panic("kaboom")
			}
			<-releasePanicker
		})
		steadyDone := make(chan struct{})
		g.add("steady", func(context.Context) { <-releaseSteady; close(steadyDone) })

		synctest.Wait() // the first run has panicked and been recovered
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := runs.Load(); got < 2 {
			t.Fatalf("panicking task ran %d times, want a restart after the panic", got)
		}

		cancel()
		close(releasePanicker)
		close(releaseSteady)
		g.wait()
		select {
		case <-steadyDone:
		default:
			t.Error("the steady task never finished; the panic broke the group's other tasks")
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "panicker") || !strings.Contains(string(out), "kaboom") {
		t.Errorf("the recovered panic was not logged with the task name and the panic value; stderr said:\n%s", out)
	}
	// the stack is the operator's only pointer to the panicking line
	if !strings.Contains(string(out), "goroutine ") {
		t.Errorf("the recovered panic was not logged with a goroutine stack; stderr said:\n%s", out)
	}
}

func TestRunGroupWaitJoinsEveryTaskBeforeReturning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)

		releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
		g.add("first", func(context.Context) { <-releaseFirst })
		g.add("second", func(context.Context) { <-releaseSecond })

		waitDone := make(chan struct{})
		go func() { g.wait(); close(waitDone) }()
		synctest.Wait()
		select {
		case <-waitDone:
			t.Fatal("wait returned while both tasks were still running")
		default:
		}

		cancel()
		close(releaseFirst)
		synctest.Wait()
		select {
		case <-waitDone:
			t.Fatal("wait returned after the first task finished but while the second was still running")
		default:
		}

		close(releaseSecond)
		<-waitDone // the fake clock runs out any backoff the group sleeps between restarts
	})
}

func TestPostStartsLoopsAreRunGroupTasks(t *testing.T) {
	fakeHostKubectl(t)

	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"
	stubLoops(t, time.Millisecond)

	// a channel made outside the bubble is never durably blocked; synctest.Wait would hang
	var release chan struct{}
	var parkedRenew, parkedSnap atomic.Int32
	oldRenew, oldSnap := certRenewRotate, snapshotSave
	t.Cleanup(func() { certRenewRotate, snapshotSave = oldRenew, oldSnap })
	certRenewRotate = func(context.Context) error { parkedRenew.Add(1); <-release; return nil }
	snapshotSave = func(context.Context, *ClusterConfig) (string, error) { parkedSnap.Add(1); <-release; return "", nil }

	synctest.Test(t, func(t *testing.T) {
		release = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)

		time.Sleep(10 * time.Millisecond) // both loops have taken their first tick
		synctest.Wait()
		if parkedRenew.Load() < 1 || parkedSnap.Load() < 1 {
			t.Fatalf("renewal ticks = %d, snapshot ticks = %d; both loops must be ticking", parkedRenew.Load(), parkedSnap.Load())
		}

		waitDone := make(chan struct{})
		go func() { g.wait(); close(waitDone) }()
		synctest.Wait()
		select {
		case <-waitDone:
			t.Fatal("the run group's wait returned while both loops' ticks were still mid-flight: the loops are not run-group tasks")
		default:
		}

		cancel()
		close(release)
		select {
		case <-waitDone:
		case <-time.After(time.Minute):
			t.Fatal("the loops never stopped on the cancelled context")
		}
	})
}

func TestPostStartRunsItsLoopsOnlyWhenTheConfigEnablesThem(t *testing.T) {
	fakeHostKubectl(t)

	for _, tc := range []struct {
		name         string
		yaml         string
		wantRenewal  bool
		wantSnapshot bool
	}{
		{"defaults run both loops", "spec: {}\n", true, true},
		{"certRenew false runs no renewal loop", "spec:\n  certRenew: false\n", false, true},
		{"snapshots disabled runs no snapshot loop", "spec:\n  snapshots:\n    disabled: true\n", true, false},
		{"both gates closed runs neither loop", "spec:\n  certRenew: false\n  snapshots:\n    disabled: true\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadClusterConfig(t, tc.yaml)
			joined := seedState()
			joined.EtcdState = "existing"
			renewals, snapshots := stubLoops(t, time.Millisecond)

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				g := newRunGroup(ctx)
				postStart(g, cfg, joined)

				time.Sleep(50 * time.Millisecond)
				synctest.Wait()
				if got := renewals(); (got > 0) != tc.wantRenewal {
					t.Errorf("renewal ticks = %d, want ticks: %v", got, tc.wantRenewal)
				}
				if got := snapshots(); (got > 0) != tc.wantSnapshot {
					t.Errorf("snapshot ticks = %d, want ticks: %v", got, tc.wantSnapshot)
				}

				cancel()
				g.wait()
			})
		})
	}
}

func TestPostStartKeepsRenewalAndSnapshotsThroughASlowApiserver(t *testing.T) {
	fakeHostKubectl(t)

	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"
	renewals, snapshots := stubLoops(t, time.Minute)

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)

		time.Sleep(3 * time.Minute)
		synctest.Wait()
		earlyRenewal, earlySnapshot := renewals(), snapshots()
		if earlyRenewal < 1 || earlySnapshot < 1 {
			t.Fatalf("at minute 3: renewal ticks = %d, snapshot ticks = %d; both loops must tick while the apiserver is not ready", earlyRenewal, earlySnapshot)
		}

		time.Sleep(12 * time.Minute)
		synctest.Wait()
		if lateRenewal, lateSnapshot := renewals(), snapshots(); lateRenewal <= earlyRenewal || lateSnapshot <= earlySnapshot {
			t.Fatalf("renewal ticks %d -> %d, snapshot ticks %d -> %d; both loops must keep ticking", earlyRenewal, lateRenewal, earlySnapshot, lateSnapshot)
		}

		cancel()
		g.wait()
	})
}

// stubNodeTask points the node-registered task's seams at counting stubs, like stubLoops.
func stubNodeTask(t *testing.T, interval, giveUp time.Duration, wantHost string, registered func(context.Context, string) bool) (labels, stamps, checks func() int32) {
	t.Helper()
	oldStart, oldLabelOwn := startEtcdGrowthRecorder, labelOwnNode
	oldInterval, oldGiveUp, oldCheck, oldLabel, oldStamp := nodeRegistrationInterval, nodeRegistrationGiveUp, nodeRegistered, labelControlPlane, stampNode
	t.Cleanup(func() {
		startEtcdGrowthRecorder, labelOwnNode = oldStart, oldLabelOwn
		nodeRegistrationInterval, nodeRegistrationGiveUp, nodeRegistered, labelControlPlane, stampNode = oldInterval, oldGiveUp, oldCheck, oldLabel, oldStamp
	})
	labelOwnNode = func(context.Context, *NodeState) {}
	startEtcdGrowthRecorder = func(ctx context.Context) { <-ctx.Done() }
	nodeRegistrationInterval, nodeRegistrationGiveUp = interval, giveUp
	var labelN, stampN, checkN atomic.Int32
	nodeRegistered = func(ctx context.Context, hostname string) bool {
		checkN.Add(1)
		return registered(ctx, hostname)
	}
	labelControlPlane = func(_ context.Context, hostname string) error {
		if hostname != wantHost {
			t.Errorf("labeled %q, want the state's own node %q", hostname, wantHost)
		}
		labelN.Add(1)
		return nil
	}
	stampNode = func(_ context.Context, hostname string) {
		if hostname != wantHost {
			t.Errorf("stamped %q, want the state's own node %q", hostname, wantHost)
		}
		stampN.Add(1)
	}
	return labelN.Load, stampN.Load, checkN.Load
}

func TestPostStartLabelsAndStampsTheNodeOnceItRegisters(t *testing.T) {
	joined := seedState()
	joined.EtcdState = "existing"
	cfg := loadClusterConfig(t, "spec:\n  certRenew: false\n  snapshots:\n    disabled: true\n")

	t.Run("a node that registers a few ticks in", func(t *testing.T) {
		var calls atomic.Int32
		labels, stamps, _ := stubNodeTask(t, time.Millisecond, time.Hour, joined.Hostname,
			func(context.Context, string) bool { return calls.Add(1) >= 3 })

		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := newRunGroup(ctx)
			postStart(g, cfg, joined)

			time.Sleep(50 * time.Millisecond) // well past the ticks that register the node
			synctest.Wait()
			if got := labels(); got != 1 {
				t.Fatalf("labeled %d times, want exactly 1", got)
			}
			if got := stamps(); got != 1 {
				t.Fatalf("stamped %d times, want exactly 1", got)
			}

			time.Sleep(5 * time.Minute)
			synctest.Wait()
			if got := labels(); got != 1 {
				t.Fatalf("labeled %d times over five minutes, want the one label-and-stamp on registration", got)
			}
			if got := stamps(); got != 1 {
				t.Fatalf("stamped %d times over five minutes, want the one label-and-stamp on registration", got)
			}

			cancel()
			g.wait()
		})
	})

	t.Run("a node that registers at minute six", func(t *testing.T) {
		var registeredAt time.Time
		labels, stamps, _ := stubNodeTask(t, 100*time.Millisecond, 15*time.Minute, joined.Hostname,
			func(context.Context, string) bool {
				return !registeredAt.IsZero() && time.Since(registeredAt) >= 6*time.Minute
			})

		synctest.Test(t, func(t *testing.T) {
			registeredAt = time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := newRunGroup(ctx)
			postStart(g, cfg, joined)

			time.Sleep(3 * time.Minute)
			synctest.Wait()
			if got := labels(); got != 0 {
				t.Fatalf("labeled %d times before the node registered, want 0", got)
			}

			time.Sleep(4 * time.Minute) // minute seven: one fake minute past registration
			synctest.Wait()
			if got := labels(); got != 1 || stamps() != 1 {
				t.Fatalf("a node that registered at minute six was labeled %d and stamped %d times, want 1 and 1", got, stamps())
			}

			cancel()
			g.wait()
		})
	})
}

func TestPostStartRetriesAFailedControlPlaneLabel(t *testing.T) {
	joined := seedState()
	joined.EtcdState = "existing"
	cfg := loadClusterConfig(t, "spec:\n  certRenew: false\n  snapshots:\n    disabled: true\n")
	_, stamps, _ := stubNodeTask(t, time.Millisecond, time.Hour, joined.Hostname,
		func(context.Context, string) bool { return true })
	var attempts, applied atomic.Int32
	labelControlPlane = func(context.Context, string) error {
		if attempts.Add(1) <= 3 {
			return errors.New("The connection to the server 172.30.99.10:6443 was refused")
		}
		applied.Add(1)
		return nil
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)

		time.Sleep(time.Second)
		synctest.Wait()
		if got := applied.Load(); got != 1 {
			t.Fatalf("label applied %d times after %d attempts, want it applied once after the refusals", got, attempts.Load())
		}
		if got := attempts.Load(); got != 4 {
			t.Errorf("label attempts = %d, want 3 refusals and the one that lands", got)
		}
		if got := stamps(); got != 1 {
			t.Errorf("stamped %d times, want exactly 1", got)
		}

		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got := attempts.Load(); got != 4 {
			t.Errorf("label attempts = %d after it landed, want no more", got)
		}

		cancel()
		g.wait()
	})
}

func TestPostStartLogsTheHandLabellingInstructionWhenTheNodeNeverRegisters(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origErr, origLog := os.Stderr, log.Writer()
	os.Stderr = w
	log.SetOutput(w)
	t.Cleanup(func() {
		os.Stderr = origErr
		log.SetOutput(origLog)
		w.Close()
		r.Close()
	})

	joined := seedState()
	joined.EtcdState = "existing"
	cfg := loadClusterConfig(t, "spec:\n  certRenew: false\n  snapshots:\n    disabled: true\n")
	labels, stamps, checks := stubNodeTask(t, 10*time.Millisecond, 100*time.Millisecond, joined.Hostname,
		func(context.Context, string) bool { return false })

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)

		time.Sleep(300 * time.Millisecond) // past the 100ms budget and ticks more
		synctest.Wait()
		frozen := checks()
		if frozen < 5 {
			t.Fatalf("registration checks = %d before give-up, want the wait to run on the tick cadence for the whole budget", frozen)
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if got := checks(); got != frozen {
			t.Fatalf("registration checks grew %d -> %d after the give-up budget; giving up must stop the wait", frozen, got)
		}
		if got := labels(); got != 0 || stamps() != 0 {
			t.Fatalf("labeled %d and stamped %d times on a node that never registered, want neither", got, stamps())
		}
		cancel()
		g.wait()
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if n := strings.Count(string(out), "stratum kubectl label node"); n != 1 {
		t.Errorf("the hand-labelling instruction logged %d times, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(string(out), "cp1") {
		t.Errorf("the give-up log does not name the node it gave up on (cp1):\n%s", out)
	}
	if !strings.Contains(string(out), k8sutil.ControlPlaneLabel+"=true") {
		t.Errorf("the give-up log does not carry the label to set by hand (%s=true):\n%s", k8sutil.ControlPlaneLabel, out)
	}
	if !strings.Contains(string(out), "worker") {
		t.Errorf("the give-up log does not say `stratum upgrade --cluster` will treat the node as a worker:\n%s", out)
	}
}

// a stalled API request consumes the registration budget, even one returning success after it
func TestNodeRegistrationBudgetBoundsStalledProbe(t *testing.T) {
	for _, lateSuccess := range []bool{false, true} {
		name := "stalled failure"
		if lateSuccess {
			name = "success after deadline"
		}
		t.Run(name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			origErr := os.Stderr
			os.Stderr = w
			t.Cleanup(func() {
				os.Stderr = origErr
				w.Close()
				r.Close()
			})

			const budget = 100 * time.Millisecond
			var probeEndedAt time.Time
			labels, stamps, checks := stubNodeTask(t, 10*time.Millisecond, budget, "cp1",
				func(ctx context.Context, _ string) bool {
					<-ctx.Done()
					probeEndedAt = time.Now()
					return lateSuccess
				})
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				g := newRunGroup(ctx)
				defer func() { cancel(); g.wait() }()
				startedAt := time.Now()
				g.add("node registration", func(ctx context.Context) { nodeRegisteredTask(ctx, &NodeState{Hostname: "cp1"}, nil) })
				synctest.Wait()
				time.Sleep(budget)
				synctest.Wait()
				if probeEndedAt.IsZero() || probeEndedAt.Sub(startedAt) != budget {
					t.Fatalf("stalled probe ended at %v, want the registration deadline %v", probeEndedAt, startedAt.Add(budget))
				}
				if ctx.Err() != nil {
					t.Fatal("registration deadline ended the controller context")
				}
				time.Sleep(2 * budget)
				synctest.Wait()
				if checks() != 1 || labels() != 0 || stamps() != 0 {
					t.Fatalf("checks/labels/stamps = %d/%d/%d after give-up, want 1/0/0", checks(), labels(), stamps())
				}
			})
			os.Stderr = origErr
			w.Close()
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(out), "stratum kubectl label node cp1"); n != 1 {
				t.Fatalf("give-up instruction logged %d times, want 1: %s", n, out)
			}
		})
	}
}

func TestPostStartNodeRegisteredTaskIsOwnedByTheRunGroup(t *testing.T) {
	joined := seedState()
	joined.EtcdState = "existing"
	cfg := loadClusterConfig(t, "spec:\n  certRenew: false\n  snapshots:\n    disabled: true\n")
	stubNodeTask(t, 10*time.Millisecond, time.Hour, joined.Hostname,
		func(context.Context, string) bool { return false })

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)

		waitDone := make(chan struct{})
		go func() { g.wait(); close(waitDone) }()
		synctest.Wait()
		select {
		case <-waitDone:
			t.Fatal("the run group's wait returned while the node-registered task was still waiting for the node to register: the task is not a run-group task")
		default:
		}

		cancel()
		select {
		case <-waitDone:
		case <-time.After(time.Minute):
			t.Fatal("the node-registered task never stopped on the cancelled context")
		}
	})
}

func TestRunControllerJoinsItsTasksWhenTheSupervisorFailsEarly(t *testing.T) {
	fakeHostKubectl(t)
	redirectStateArea(t)
	// a regular file as log dir fails the supervisor's MkdirAll before any child starts
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldLogDir := version.LogDir
	version.LogDir = blocker
	t.Cleanup(func() { version.LogDir = oldLogDir })
	for _, sw := range []struct {
		p *string
	}{
		{&version.ManifestsDir},
		{&version.CoreDNSUpstreamDir},
	} {
		dir := filepath.Join(t.TempDir(), "scratch")
		old := *sw.p
		*sw.p = dir
		t.Cleanup(func() { *sw.p = old })
	}

	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    disabled: true\n")
	st := seedState()
	st.EtcdState = "existing"
	// the node registers on the first tick, so every task parks on its context awaiting shutdown
	stubNodeTask(t, time.Millisecond, time.Hour, st.Hostname,
		func(context.Context, string) bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunController(ctx, cfg, st) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunController with a log dir the supervisor cannot create returned nil; it must report the supervisor's early failure")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunController did not return after the supervisor failed early: its tasks were never ended on the derived context")
	}
}

func TestRunControllerHandsTheRosterTheConfigsOperatorSwitch(t *testing.T) {
	oldManifests, oldPKI, oldKubeconfig, oldCilium := version.ManifestsDir, version.PKIDir, version.KubeconfigDir, version.CiliumSettingsPath
	t.Cleanup(func() {
		version.ManifestsDir, version.PKIDir, version.KubeconfigDir, version.CiliumSettingsPath = oldManifests, oldPKI, oldKubeconfig, oldCilium
	})
	oldProcesses, oldRun, oldUpstream := processes, runSupervisor, writeCoreDNSUpstream
	t.Cleanup(func() { processes, runSupervisor, writeCoreDNSUpstream = oldProcesses, oldRun, oldUpstream })
	writeCoreDNSUpstream = func(*NodeState) error { return nil }
	runSupervisor = func(ctx context.Context, _ []supervisor.Process, _ supervisor.Options) error { return ctx.Err() }

	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"spec.operator false", "spec:\n  operator: false\n", false},
		{"spec.operator left unset", "spec: {}\n", true},
		{"spec.operator true", "spec:\n  operator: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// fresh dirs per case: a staged dashboard would make the next operator-off start refuse
			redirectStateArea(t)
			version.ManifestsDir, version.PKIDir, version.KubeconfigDir = t.TempDir(), t.TempDir(), t.TempDir()
			version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")

			cfg := loadClusterConfig(t, tc.yaml)
			st := seedState()
			var seen []bool
			processes = func(rendered *NodeState, operator bool) []supervisor.Process {
				if rendered != st {
					t.Errorf("the roster was rendered from %+v, want the controller's own node state", rendered)
				}
				seen = append(seen, operator)
				return nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := RunController(ctx, cfg, st); !errors.Is(err, context.Canceled) {
				t.Fatalf("RunController returned %v; want the supervisor seam's context error", err)
			}
			if want := []bool{tc.want}; !slices.Equal(seen, want) {
				t.Errorf("the roster seam saw the operator switch %v, want one render with %v", seen, tc.want)
			}
		})
	}
}

func TestTheCompetingNodeRegistrationWaitersAreGone(t *testing.T) {
	run, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, waiter := range []string{"WaitNodeRegistered(hostname, 30)", "WaitNodeRegistered(st.Hostname, 30)"} {
		if strings.Contains(string(run), waiter) {
			t.Errorf("run.go still waits %s itself; the node-registered task is the single owner of that wait", waiter)
		}
	}
	join, err := os.ReadFile("../../internal/ha/join.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(join), "LabelJoinedControlPlane") || strings.Contains(string(join), "WaitNodeRegistered") {
		t.Error("internal/ha/join.go still labels or waits out a joined control plane's registration itself; the node-registered task in internal/cluster owns that wait")
	}
	up, err := os.ReadFile("../../pkg/cli/up.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(up), "LabelJoinedControlPlane") {
		t.Error("pkg/cli/up.go still fires ha.LabelJoinedControlPlane beside the controller it starts; the controller's node-registered task owns the labelling")
	}
}

// a source pin: a gate that comes back must be re-expressed against this test first
func TestNoGateCameBackInFrontOfThePostStartTasks(t *testing.T) {
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "postStartWaitAPI") {
		t.Error("run.go still carries a postStartWaitAPI seam; the gate it waited on is gone, and a seam nothing calls pins nothing")
	}
	if strings.Contains(string(src), "waitAPIReady(") {
		t.Error("run.go calls waitAPIReady itself; postStart's tasks check their own preconditions per tick and must not sit behind an apiserver gate again (the kubelet's own startup wait lives in processes.go)")
	}
}

// rotationEnv stages a temp PKI one day from expiry, the node IP, and no supervisor socket.
func rotationEnv(t *testing.T) {
	t.Helper()
	origPKI, origKC, origConf, origAdmin, origNet, origSock :=
		version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath, version.ControlSocket
	t.Cleanup(func() {
		version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath, version.ControlSocket =
			origPKI, origKC, origConf, origAdmin, origNet, origSock
	})
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	version.NetConfigPath = filepath.Join(base, "netconfig") // absent: the IPv4 defaults
	version.ControlSocket = filepath.Join(base, "stratum.sock")

	// one-day leaves: the if-expiring gate passes, so a cancelled context is tellable from a no-op
	origValidity := pki.LeafValidity
	pki.LeafValidity = 24 * time.Hour
	t.Cleanup(func() { pki.LeafValidity = origValidity })

	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
	}); err != nil {
		t.Fatal(err)
	}
	if err := pki.GenerateAll("cp1", "192.168.99.12", nil); err != nil {
		t.Fatal(err)
	}
}

// fakeEtcdctl points BinDir at an etcdctl running body; the save's $3 is the snapshot path.
func fakeEtcdctl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "etcdctl"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := version.BinDir
	version.BinDir = dir
	t.Cleanup(func() { version.BinDir = old })
}

func TestTheRenewalSeamStopsItsRotationWhenTheContextEnds(t *testing.T) {
	rotationEnv(t)
	before, err := pki.CertNotAfter("apiserver")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = certRenewRotate(ctx)
	if err == nil {
		t.Fatal("the renewal seam ran a rotation whose context was already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled, so a shutdown is tellable from a rotation failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(version.KubeconfigDir, "admin.kubeconfig")); statErr == nil {
		t.Error("the rotation rewrote the kubeconfigs although its context was cancelled")
	}
	after, aerr := pki.CertNotAfter("apiserver")
	if aerr != nil {
		t.Fatal(aerr)
	}
	if !after.Equal(before) {
		t.Error("the leaves were reissued although the context was cancelled")
	}
}

func TestTheSnapshotSeamAbortsItsEtcdctlRunWhenTheContextEnds(t *testing.T) {
	redirectEvidenceArea(t)
	snapDir := t.TempDir()
	fakeEtcdctl(t, ": > \"$3.part\"\ntouch \"$3.started\"\nexec sleep 60")
	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    dir: "+snapDir+"\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := snapshotSave(ctx, cfg); done <- err }()

	started := filepath.Join(snapDir, "*.started")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m, _ := filepath.Glob(started); len(m) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the etcdctl snapshot run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the snapshot seam reported success for a save whose context ended")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the snapshot seam did not return after its context ended: the in-flight etcdctl run was waited out")
	}
	if took := time.Since(begin); took > 10*time.Second {
		t.Errorf("took %s to abort a run whose etcdctl sleeps 60s; the kill must be prompt", took)
	}
	if db, _ := filepath.Glob(filepath.Join(snapDir, "*.db")); len(db) > 0 {
		t.Errorf("a snapshot file exists although the etcdctl run was aborted: %v", db)
	}
	if part, _ := filepath.Glob(filepath.Join(snapDir, "*.part")); len(part) > 0 {
		t.Errorf("partial snapshot remains after cancellation: %v", part)
	}
	// the aborted tick is still evidence: the failed attempt sits in the redirected record
	rec := readEvidence(t)
	if rec.Save.AttemptedAt.IsZero() || rec.Save.Result == "" {
		t.Errorf("the aborted save was not recorded: %+v", rec.Save)
	}
	if !rec.LastSaveSuccess.IsZero() {
		t.Errorf("an aborted save moved the last-success stamp to %v", rec.LastSaveSuccess)
	}
}

func TestCertRenewLoopJittersItsFirstTickPerNode(t *testing.T) {
	const interval = time.Hour
	js := map[string]time.Duration{}
	for _, h := range []string{"cp1", "cp2", "cp3", "cp4", "cp5"} {
		j := renewalJitter(h, interval)
		if j <= 0 || j >= interval {
			t.Fatalf("renewalJitter(%q, %v) = %v, want an offset inside (0, interval)", h, interval, j)
		}
		if again := renewalJitter(h, interval); again != j {
			t.Fatalf("renewalJitter(%q, %v) = %v then %v: the offset must be deterministic, so a node keeps its slot", h, interval, j, again)
		}
		js[h] = j
	}
	slots := map[time.Duration]bool{}
	for _, j := range js {
		slots[j] = true
	}
	if len(slots) < 3 {
		t.Fatalf("the renewal jitter spreads five nodes over %d slots; too little spread for leaves issued together: %v", len(slots), js)
	}
	if js["cp1"] == js["cp2"] {
		t.Fatalf("cp1 and cp2 share the renewal slot %v: two control planes installed together would still renew together", js["cp1"])
	}
	// the real cadence is a day; its offset must sit inside it too
	if j := renewalJitter("cp1", 24*time.Hour); j <= 0 || j >= 24*time.Hour {
		t.Fatalf("renewalJitter(\"cp1\", 24h) = %v, want an offset inside (0, 24h)", j)
	}

	oldInterval, oldRotate := certRenewInterval, certRenewRotate
	t.Cleanup(func() { certRenewInterval, certRenewRotate = oldInterval, oldRotate })
	certRenewInterval = interval
	redirectStateArea(t)

	for _, hostname := range []string{"cp1", "cp2"} {
		t.Run(hostname, func(t *testing.T) {
			var mu sync.Mutex
			var attempts []time.Time
			certRenewRotate = func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				attempts = append(attempts, time.Now())
				return nil
			}

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go certRenewLoop(ctx, hostname)
				synctest.Wait()
				start := time.Now()

				// just short of the offset, the first fire must not have come
				time.Sleep(js[hostname] - time.Millisecond)
				synctest.Wait()
				mu.Lock()
				n := len(attempts)
				mu.Unlock()
				if n != 0 {
					t.Fatalf("renewal fired %d times before its offset: the first fire carries no per-node jitter offset", n)
				}

				time.Sleep(time.Millisecond)
				synctest.Wait()
				mu.Lock()
				n = len(attempts)
				var first time.Time
				if n >= 1 {
					first = attempts[0]
				}
				mu.Unlock()
				if n != 1 {
					t.Fatalf("renewal attempts = %d at the jitter offset, want exactly the first", n)
				}
				if got := first.Sub(start); got != js[hostname] {
					t.Fatalf("first fire %v after start, want exactly the jitter offset %v", got, js[hostname])
				}

				time.Sleep(interval)
				synctest.Wait()
				mu.Lock()
				n = len(attempts)
				second := attempts[len(attempts)-1]
				mu.Unlock()
				if n != 2 {
					t.Fatalf("renewal attempts = %d after two intervals, want 2", n)
				}
				if got := second.Sub(first); got != interval {
					t.Fatalf("second fire %v after the first, want exactly one interval: the jitter offsets the first fire only", got)
				}
			})
		})
	}
}

func TestCertRenewLoopRecordsEachAttemptTimeAndResult(t *testing.T) {
	const interval = time.Hour
	j := renewalJitter("cp1", interval)
	stateDir := redirectStateArea(t)
	oldInterval, oldRotate := certRenewInterval, certRenewRotate
	t.Cleanup(func() { certRenewInterval, certRenewRotate = oldInterval, oldRotate })
	certRenewInterval = interval

	boom := errors.New("renew failed: apiserver leaf unreadable")
	var mu sync.Mutex
	var attempts []time.Time
	certRenewRotate = func(context.Context) error {
		mu.Lock()
		attempts = append(attempts, time.Now())
		fail := len(attempts) == 2
		mu.Unlock()
		if fail {
			return boom
		}
		return nil
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go certRenewLoop(ctx, "cp1")

		time.Sleep(j) // the first, successful attempt
		synctest.Wait()
		rec, err := certs.LoadRenewalRecord()
		if err != nil {
			t.Fatalf("no renewal record after the first attempt: %v", err)
		}
		mu.Lock()
		if len(attempts) < 1 {
			t.Fatal("the first attempt never ran; the test's timing is off")
		}
		first := attempts[0]
		mu.Unlock()
		if !rec.AttemptedAt.Equal(first) {
			t.Errorf("recorded attempt time %v, want the attempt's own time %v", rec.AttemptedAt, first)
		}
		if rec.Result != "" {
			t.Errorf("a successful attempt recorded result %q, want the empty success result", rec.Result)
		}
		if es, _ := os.ReadDir(stateDir); len(es) == 0 {
			t.Error("nothing was written under the node's state area: the renewal record lives there, beside node.yaml")
		}

		time.Sleep(interval) // the second, failing attempt
		synctest.Wait()
		rec, err = certs.LoadRenewalRecord()
		if err != nil {
			t.Fatalf("renewal record unreadable after the failed attempt: %v", err)
		}
		mu.Lock()
		if len(attempts) < 2 {
			t.Fatal("the second attempt never ran; the test's timing is off")
		}
		second := attempts[1]
		mu.Unlock()
		if !rec.AttemptedAt.Equal(second) {
			t.Errorf("record holds %v after the later attempt at %v: the record keeps the LAST attempt", rec.AttemptedAt, second)
		}
		if rec.Result == "" || !strings.Contains(rec.Result, boom.Error()) {
			t.Errorf("failed attempt recorded result %q, want it to carry the rotation error %q", rec.Result, boom.Error())
		}

		cancel()
	})
}

func TestCertRenewLoopRecordsPanicBeforeRestart(t *testing.T) {
	redirectStateArea(t)
	oldInterval, oldRotate := certRenewInterval, certRenewRotate
	t.Cleanup(func() { certRenewInterval, certRenewRotate = oldInterval, oldRotate })
	certRenewInterval = time.Hour
	var mu sync.Mutex
	var attemptedAt time.Time
	var attempts int
	certRenewRotate = func(context.Context) error {
		mu.Lock()
		attempts++
		attemptedAt = time.Now()
		mu.Unlock()
		panic("renewal write failed")
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		g.add("cert renewal", func(ctx context.Context) { certRenewLoop(ctx, "cp1") })
		time.Sleep(renewalJitter("cp1", certRenewInterval))
		synctest.Wait()
		rec, err := certs.LoadRenewalRecord()
		mu.Lock()
		firstAttempt := attemptedAt
		mu.Unlock()
		if err != nil || !rec.AttemptedAt.Equal(firstAttempt) || rec.Result != "panic: renewal write failed" {
			t.Fatalf("panic attempt record = %+v, err = %v", rec, err)
		}
		// The failed task still restarts under the group's backoff policy.
		time.Sleep(runGroupMinBackoff + renewalJitter("cp1", certRenewInterval))
		synctest.Wait()
		mu.Lock()
		gotAttempts := attempts
		mu.Unlock()
		if gotAttempts != 2 {
			t.Fatalf("rotation attempts = %d, want two panic-recovered runs", gotAttempts)
		}
		cancel()
		g.wait()
	})
}

func TestCertRenewLoopShutdownPreservesRealErrors(t *testing.T) {
	boom := errors.New("certificate write failed")
	for _, tc := range []struct {
		name        string
		result      func(error) error
		wantFailure bool
	}{
		{"cancellation", func(err error) error { return err }, false},
		{"wrapped cancellation", func(err error) error { return fmt.Errorf("rotation stopped: %w", err) }, false},
		{"joined failure", func(err error) error { return errors.Join(err, boom) }, true},
		{"unrelated failure", func(error) error { return boom }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectStateArea(t)
			previous := certs.RenewalRecord{AttemptedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Result: "previous attempt"}
			if err := certs.SaveRenewalRecord(previous); err != nil {
				t.Fatal(err)
			}
			capture, err := os.CreateTemp(t.TempDir(), "renewal-stderr")
			if err != nil {
				t.Fatal(err)
			}
			oldStderr := os.Stderr
			os.Stderr = capture
			t.Cleanup(func() { os.Stderr = oldStderr; capture.Close() })
			oldInterval, oldRotate := certRenewInterval, certRenewRotate
			t.Cleanup(func() { certRenewInterval, certRenewRotate = oldInterval, oldRotate })
			const interval = time.Hour
			certRenewInterval = interval
			calls := 0
			certRenewRotate = func(ctx context.Context) error {
				calls++
				<-ctx.Done()
				return tc.result(ctx.Err())
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { defer close(done); certRenewLoop(ctx, "cp1") }()
				synctest.Wait()
				// a pending tick beside ctx.Done(): no new attempt may start after cancel
				time.Sleep(3*interval + renewalJitter("cp1", interval))
				synctest.Wait()
				cancel()
				<-done
				if calls != 1 {
					t.Fatalf("rotation attempts = %d, want only the in-flight attempt", calls)
				}
			})
			os.Stderr = oldStderr
			logs, err := os.ReadFile(capture.Name())
			if err != nil {
				t.Fatal(err)
			}
			record, err := certs.LoadRenewalRecord()
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantFailure {
				if !strings.Contains(string(logs), boom.Error()) || !strings.Contains(record.Result, boom.Error()) {
					t.Fatalf("real failure lost during shutdown: log = %q, record = %+v", logs, record)
				}
			} else if len(logs) != 0 || record != previous {
				t.Fatalf("expected shutdown changed diagnostics: log = %q, record = %+v, previous = %+v", logs, record, previous)
			}
		})
	}
}

// the jitter must be the renewal loop's only contention reduction; RotateContext holds the lease
func TestCertRenewJitterAddsNoLockOrLease(t *testing.T) {
	run, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	body := func(name string) string {
		src := string(run)
		i := strings.Index(src, "func "+name+"(")
		if i < 0 {
			t.Fatalf("run.go has no %s: the renewal jitter lives there", name)
		}
		fn := src[i:]
		if j := strings.Index(fn, "\nfunc "); j > 0 {
			fn = fn[:j]
		}
		return fn
	}
	for _, fn := range []string{"certRenewLoop", "renewalJitter"} {
		b := body(fn)
		for _, locked := range []string{"etcddata", "Lock(", "TryLock", "sync.Mutex"} {
			if strings.Contains(b, locked) {
				t.Errorf("%s uses %q: jitter must be the renewal loop's only contention reduction — RotateContext holds the maintenance lease", fn, locked)
			}
		}
	}
}

func TestNodeClientBindingIsReservedOnlyWithoutAnOperator(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Minute)
	oldReserve, oldRetry := reserveNodeClientBinding, nodeClientBindingRetry
	t.Cleanup(func() { reserveNodeClientBinding, nodeClientBindingRetry = oldReserve, oldRetry })
	nodeClientBindingRetry = time.Second

	for _, tc := range []struct {
		name string
		yaml string
		want int32
	}{
		{"operator on", "spec: {}\n", 0},
		{"operator off", "spec:\n  operator: false\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadClusterConfig(t, tc.yaml)
			joined := seedState()
			joined.EtcdState = "existing"
			var calls atomic.Int32
			reserveNodeClientBinding = func(context.Context) error {
				if calls.Add(1) < 3 {
					return errors.New("apiserver not up")
				}
				return nil
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				g := newRunGroup(ctx)
				postStart(g, cfg, joined)
				time.Sleep(time.Minute)
				synctest.Wait()
				if got := calls.Load(); got != tc.want {
					t.Errorf("binding reservations = %d, want %d", got, tc.want)
				}
				cancel()
				g.wait()
			})
		})
	}
}

func TestKubeVipRBACIsEnsuredOnlyWithAVIP(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Minute)
	oldReserve, oldRetry := reserveNodeClientBinding, nodeClientBindingRetry
	t.Cleanup(func() { reserveNodeClientBinding, nodeClientBindingRetry = oldReserve, oldRetry })
	reserveNodeClientBinding = func(context.Context) error { return nil }
	nodeClientBindingRetry = time.Second

	for _, tc := range []struct {
		vip  string
		want int32
	}{{"", 0}, {"10.0.0.100", 4}} {
		cfg := loadClusterConfig(t, "spec: {}\n")
		st := seedState()
		st.EtcdState = "existing"
		st.VIP.Address = tc.vip
		var calls atomic.Int32
		ensureKubeVipRBAC = func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("apiserver not up")
			}
			return nil
		}
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			g := newRunGroup(ctx)
			postStart(g, cfg, st)
			time.Sleep(time.Minute + kubeVipRBACResync)
			synctest.Wait()
			if got := calls.Load(); got != tc.want {
				t.Errorf("VIP %q: kube-vip RBAC attempts = %d, want %d (two failures, a success, one resync)", tc.vip, got, tc.want)
			}
			cancel()
			g.wait()
		})
	}
}

func TestKubeletAPIAdminBindingIsEnsuredOnEveryController(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Minute)
	oldReserve, oldRetry := reserveNodeClientBinding, nodeClientBindingRetry
	t.Cleanup(func() { reserveNodeClientBinding, nodeClientBindingRetry = oldReserve, oldRetry })
	reserveNodeClientBinding = func(context.Context) error { return nil }
	nodeClientBindingRetry = time.Second

	for _, yaml := range []string{"spec: {}\n", "spec:\n  operator: false\n"} {
		cfg := loadClusterConfig(t, yaml)
		joined := seedState()
		joined.EtcdState = "existing"
		var calls atomic.Int32
		ensureKubeletAPIAdminBinding = func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("apiserver not up")
			}
			return nil
		}
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			g := newRunGroup(ctx)
			postStart(g, cfg, joined)
			time.Sleep(time.Minute)
			synctest.Wait()
			if got := calls.Load(); got != 3 {
				t.Errorf("%q: kubelet API admin binding attempts = %d, want 3 (retried until the apiserver answered, then held)", yaml, got)
			}
			cancel()
			g.wait()
		})
	}
}

func TestClusterAdminsBindingIsEnsuredOnEveryController(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Minute)
	oldReserve, oldRetry, oldResync := reserveNodeClientBinding, nodeClientBindingRetry, clusterAdminsResync
	t.Cleanup(func() {
		reserveNodeClientBinding, nodeClientBindingRetry, clusterAdminsResync = oldReserve, oldRetry, oldResync
	})
	reserveNodeClientBinding = func(context.Context) error { return nil }
	nodeClientBindingRetry = time.Second
	clusterAdminsResync = 10 * time.Minute

	for _, yaml := range []string{"spec: {}\n", "spec:\n  operator: false\n"} {
		cfg := loadClusterConfig(t, yaml)
		joined := seedState()
		joined.EtcdState = "existing"
		var calls atomic.Int32
		ensureClusterAdminsBinding = func(_ context.Context, holder string) error {
			if holder != joined.Hostname {
				t.Errorf("holder = %q, want the node name %q", holder, joined.Hostname)
			}
			if calls.Add(1) < 3 {
				return errors.New("apiserver not up")
			}
			return nil
		}
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			g := newRunGroup(ctx)
			postStart(g, cfg, joined)
			time.Sleep(time.Hour)
			synctest.Wait()
			if got := calls.Load(); got != 8 {
				t.Errorf("%q: cluster-admins binding attempts = %d, want 8 (3 until the apiserver answered at 4s, then every 10m)", yaml, got)
			}
			cancel()
			g.wait()
		})
	}
}

func TestLeafSubjectMigration(t *testing.T) {
	stubLoops(t, time.Minute)
	legacy := []string{"admin (CN=admin,O=system:masters)"}
	for _, tc := range []struct {
		name       string
		vip        string
		drift      []string
		bindingErr error
		rotateErrs []error
		restart    bool
		want       []string
		wantErrs   int
		wantMarker bool
	}{
		{name: "no drift", want: []string{"drift", "drift"}},
		{name: "drift, no VIP", drift: legacy,
			want: []string{"drift", "cluster-admins", "kubelet-api-admin", "rotate", "drift"}},
		{name: "drift on a VIP node", vip: "10.0.0.10", drift: []string{"kube-vip (CN=kube-vip,O=system:masters)"},
			want: []string{"drift", "cluster-admins", "kubelet-api-admin", "kube-vip", "rotate", "drift"}},
		{name: "a binding fails: no rotation", drift: legacy, bindingErr: errors.New("403"),
			want: []string{"drift", "cluster-admins", "cluster-admins"}, wantErrs: 2},
		{name: "a rotation refused for the lease is retried at once", drift: legacy, rotateErrs: []error{maintlease.ErrTaken},
			want: []string{"drift", "cluster-admins", "kubelet-api-admin", "rotate", "cluster-admins", "kubelet-api-admin", "rotate"}, wantErrs: 1},
		{name: "a failed rotation backs off", drift: legacy, rotateErrs: []error{errors.New("etcd did not come back")},
			want: []string{"drift", "cluster-admins", "kubelet-api-admin", "rotate"}, wantErrs: 2, wantMarker: true},
		{name: "a rotation cut short is resumed after a restart", drift: legacy, rotateErrs: []error{context.Canceled}, restart: true,
			want: []string{"drift", "cluster-admins", "kubelet-api-admin", "rotate", "cluster-admins", "kubelet-api-admin", "rotate"}, wantErrs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectStateArea(t)
			var calls []string
			drift := tc.drift
			leafSubjectDrift = func() ([]string, error) { calls = append(calls, "drift"); return drift, nil }
			ensureClusterAdminsBinding = func(_ context.Context, holder string) error {
				if holder != "cp1" {
					t.Errorf("holder = %q, want cp1", holder)
				}
				calls = append(calls, "cluster-admins")
				return tc.bindingErr
			}
			ensureKubeletAPIAdminBinding = func(context.Context) error { calls = append(calls, "kubelet-api-admin"); return nil }
			ensureKubeVipRBAC = func(context.Context) error { calls = append(calls, "kube-vip"); return nil }
			rotateErrs := tc.rotateErrs
			rotateLeaves = func(context.Context) error {
				calls = append(calls, "rotate")
				drift = nil
				if len(rotateErrs) > 0 {
					err := rotateErrs[0]
					rotateErrs = rotateErrs[1:]
					return err
				}
				return nil
			}
			st := &NodeState{Hostname: "cp1", VIP: nodestate.VIP{Address: tc.vip}}
			apply := leafSubjectMigration(st)
			errs := 0
			for i := range 2 {
				if i == 1 && tc.restart {
					apply = leafSubjectMigration(st)
				}
				if err := apply(context.Background()); err != nil {
					errs++
				}
			}
			if errs != tc.wantErrs {
				t.Errorf("errors = %d, want %d", errs, tc.wantErrs)
			}
			if !slices.Equal(calls, tc.want) {
				t.Errorf("calls = %q, want %q", calls, tc.want)
			}
			_, err := os.Stat(leafSubjectMigrationMarker())
			if pending := tc.bindingErr != nil || tc.wantMarker; pending != (err == nil) {
				t.Errorf("marker present = %v, want %v", err == nil, pending)
			}
		})
	}
}

func TestLeafSubjectMigrationBackoffAndLogging(t *testing.T) {
	stubLoops(t, time.Minute)
	redirectStateArea(t)
	oldBackoff, oldMax := leafReissueBackoff, leafReissueBackoffMax
	t.Cleanup(func() { leafReissueBackoff, leafReissueBackoffMax = oldBackoff, oldMax })
	leafReissueBackoff, leafReissueBackoffMax = time.Minute, 3*time.Minute
	leafSubjectDrift = func() ([]string, error) { return []string{"admin (CN=admin,O=system:masters)"}, nil }
	ensureClusterAdminsBinding = func(context.Context, string) error { return nil }
	ensureKubeletAPIAdminBinding = func(context.Context) error { return nil }
	failed := errors.New("etcd did not come back")
	// leaseHeld stands for a real maintlease refusal against a live holder
	leaseHeld := errors.New("acquire the maintenance lease")
	errs := []error{leaseHeld, leaseHeld, failed, failed, failed, failed}
	var rotations []time.Duration

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origErr := os.Stderr
	os.Stderr = w
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		other, renewed := "cp2/4242/1", metav1.NewMicroTime(start)
		cs := fake.NewClientset(&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "stratum-maintenance", Namespace: "kube-system"},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &other, LeaseDurationSeconds: ptrInt32(3600),
				RenewTime: &renewed},
		})
		rotateLeaves = func(ctx context.Context) error {
			rotations = append(rotations, time.Since(start))
			if len(errs) == 0 {
				return failed
			}
			err := errs[0]
			errs = errs[1:]
			if err == leaseHeld {
				_, _, err = maintlease.Acquire(ctx, cs, "kube-system", "stratum-maintenance", "cp1/1/1", "maintenance run")
				if err == nil {
					t.Fatal("a lease held live by another run was taken")
				}
			}
			return err
		}
		apply := leafSubjectMigration(&NodeState{Hostname: "cp1"})
		for range 20 {
			_ = apply(context.Background())
			time.Sleep(30 * time.Second)
		}
	})
	os.Stderr = origErr
	w.Close()
	out, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 7 * time.Minute}
	if !slices.Equal(rotations, want) {
		t.Errorf("rotations at %v, want %v (held lease retried at once; failures back off 1m, 2m, then the 3m cap)", rotations, want)
	}
	if n := strings.Count(string(out), "waiting for the maintenance lease"); n != 1 {
		t.Errorf("lease wait logged %d times, want once for one wait:\n%s", n, out)
	}
	if n := strings.Count(string(out), "leaf reissue failed: etcd did not come back"); n != 4 {
		t.Errorf("failures logged %d times, want each of the 4:\n%s", n, out)
	}
}

func TestPostStartRegistersLeafSubjectMigration(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Minute)
	oldReserve, oldRetry := reserveNodeClientBinding, nodeClientBindingRetry
	t.Cleanup(func() { reserveNodeClientBinding, nodeClientBindingRetry = oldReserve, oldRetry })
	reserveNodeClientBinding = func(context.Context) error { return nil }
	nodeClientBindingRetry = time.Second

	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"
	var mu sync.Mutex
	var bound, rotated int
	var rotatedBeforeBound bool
	drift := []string{"admin (CN=admin,O=system:masters)"}
	leafSubjectDrift = func() ([]string, error) { mu.Lock(); defer mu.Unlock(); return drift, nil }
	ensureKubeletAPIAdminBinding = func(context.Context) error { mu.Lock(); defer mu.Unlock(); bound++; return nil }
	rotateLeaves = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if bound == 0 {
			rotatedBeforeBound = true
		}
		rotated++
		drift = nil
		return nil
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)
		time.Sleep(time.Hour)
		synctest.Wait()
		cancel()
		g.wait()
	})
	if rotated != 1 || rotatedBeforeBound {
		t.Errorf("rotations = %d (before a binding: %v), want 1, after the bindings", rotated, rotatedBeforeBound)
	}
}

func ptrInt32(v int32) *int32 { return &v }

// a torn manifest is what the atomic writer makes impossible; the rename lands the declared mode
func TestOperatorStackManifestsAreWrittenThroughTheAtomicWriter(t *testing.T) {
	origDir, origCilium := version.ManifestsDir, version.CiliumSettingsPath
	version.ManifestsDir = t.TempDir()
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.ManifestsDir, version.CiliumSettingsPath = origDir, origCilium })
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}

	stale := filepath.Join(version.ManifestsDir, "coredns", "coredns.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "stale\n" {
		t.Errorf("%s still holds the stale bytes; writeBundles did not rewrite it", stale)
	}
	fi, err := os.Stat(stale)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("replaced manifest mode = %v, want 0644: the staging write must land at the declared mode even when it replaces a file whose mode differs — os.WriteFile only applies perm on create, the atomic writer on every write", got)
	}
	fresh := filepath.Join(version.ManifestsDir, "cilium", "cilium.yaml")
	if fi, err = os.Stat(fresh); err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("fresh manifest mode = %v, want 0644", got)
	}
	entries, err := os.ReadDir(filepath.Dir(stale))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("the staged coredns directory holds %q after the write: the atomic writer's temp file must be consumed by the rename, never left beside the manifest", e.Name())
		}
	}
}

// a torn .args file would record an argv the component does not run
func TestComponentArgsAreWrittenThroughTheAtomicWriter(t *testing.T) {
	confDir := redirectStateArea(t)
	procs := []supervisor.Process{
		{Name: "kubelet", Path: "/var/lib/stratum/bin/kubelet", Args: []string{"--config=" + version.K8sConfDir + "/kubelet.conf"}},
		{Name: "containerd", Path: "/var/lib/stratum/bin/containerd", Args: []string{"--config", version.ContainerdConfig}},
	}

	stale := filepath.Join(confDir, "components", "kubelet.args")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeComponentArgs(procs); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/var/lib/stratum/bin/kubelet \\\n  --config=" + version.K8sConfDir + "/kubelet.conf\n"; string(b) != want {
		t.Errorf("kubelet.args = %q, want the rendered argv %q", b, want)
	}
	fi, err := os.Stat(stale)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("replaced .args file mode = %v, want 0644: the component .args write must land at the declared mode even when it replaces a file whose mode differs — os.WriteFile only applies perm on create, the atomic writer on every write", got)
	}
	entries, err := os.ReadDir(filepath.Dir(stale))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("the components directory holds %q after the write: the atomic writer's temp file must be consumed by the rename, never left beside the .args files", e.Name())
		}
	}
	slices.Sort(names)
	if want := []string{"containerd.args", "kubelet.args"}; !slices.Equal(names, want) {
		t.Errorf("components directory holds %v, want exactly %v", names, want)
	}
}
