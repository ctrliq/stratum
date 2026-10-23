package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestManagedRestampCoalescesAndJoinsAtShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		calls := 0
		release := make(chan struct{})
		onStart := managedKubeletOnStart(g, "cp1", func(taskCtx context.Context, hostname string) {
			calls++
			if taskCtx != ctx || hostname != "cp1" {
				t.Errorf("restamp context/hostname = %v/%s", taskCtx, hostname)
			}
			<-taskCtx.Done()
			<-release // prove the join waits for the callback, not merely cancellation
		})
		onStart("containerd", 1)
		onStart("operator", 2)
		synctest.Wait()
		if calls != 0 {
			t.Fatal("non-kubelet process triggered a restamp")
		}
		onStart("kubelet", 3)
		synctest.Wait()
		onStart("kubelet", 4)
		onStart("kubelet", 5)
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("overlapping starts ran %d restamps, want 1", calls)
		}
		cancel()
		done := make(chan struct{})
		go func() { g.wait(); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("shutdown abandoned the in-flight restamp")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("shutdown did not join the completed restamp")
		}
	})
}

func TestManagedRestampRecoversPanicAndHandlesLaterStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := newRunGroup(ctx)
		calls := 0
		onStart := managedKubeletOnStart(g, "cp1", func(context.Context, string) {
			calls++
			if calls == 1 {
				panic("restamp failure")
			}
		})
		onStart("kubelet", 1)
		synctest.Wait()
		time.Sleep(runGroupMinBackoff)
		synctest.Wait()
		onStart("kubelet", 2)
		synctest.Wait()
		if calls != 2 {
			t.Fatalf("restamp never recovered after panic: %d calls", calls)
		}
		cancel()
		g.wait()
	})
}

// The supervisor outlives `stratum upgrade`, so a kubelet restart is
// the moment it must re-assert the node's version annotations.
func TestKubeletOnStartRestampsOnKubeletOnly(t *testing.T) {
	var calls atomic.Int32
	hostnames := make(chan string, 1)
	onStart := kubeletOnStart("n1", func(h string) {
		calls.Add(1)
		hostnames <- h
	})

	// Non-kubelet starts go first, with nothing in flight: the single-flight
	// guard would otherwise absorb a wrongly restamping one.
	onStart("containerd", 1)
	onStart("operator", 1)
	time.Sleep(50 * time.Millisecond) // an erroneous restamp would land by now
	if n := calls.Load(); n != 0 {
		t.Fatalf("non-kubelet starts restamped %d times, want 0", n)
	}

	onStart("kubelet", 1)
	select {
	case h := <-hostnames:
		if h != "n1" {
			t.Errorf("restamped %q, want %q", h, "n1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kubelet start did not restamp")
	}
}

// A crash-looping kubelet must not pile up restamp goroutines while
// the node is unregistered. Overlapping starts share the one in flight; a start
// after it returns (the `stratum upgrade` restart) restamps again.
func TestKubeletOnStartSingleFlight(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	started := make(chan struct{}, 8)
	release := make(chan struct{}, 8)
	onStart := kubeletOnStart("n1", func(string) {
		mu.Lock()
		calls++
		mu.Unlock()
		started <- struct{}{}
		<-release
	})
	wait := func(what string) {
		t.Helper()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal(what)
		}
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	onStart("kubelet", 1)
	wait("first kubelet start did not restamp")
	onStart("kubelet", 2) // overlaps the blocked restamp
	onStart("kubelet", 3)
	time.Sleep(50 * time.Millisecond) // an unguarded extra restamp would land by now
	if n := count(); n != 1 {
		t.Fatalf("overlapping starts ran restamp %d times, want 1", n)
	}

	release <- struct{}{}
	// The guard clears just after restamp returns; a start that lands in that
	// gap is dropped, so retry until one runs.
	deadline := time.After(5 * time.Second)
	for count() < 2 {
		onStart("kubelet", 4)
		select {
		case <-deadline:
			t.Fatal("kubelet start after restamp returned did not restamp")
		case <-time.After(5 * time.Millisecond):
		}
	}
	release <- struct{}{}
	if n := count(); n != 2 {
		t.Errorf("restamp ran %d times, want 2", n)
	}
}
