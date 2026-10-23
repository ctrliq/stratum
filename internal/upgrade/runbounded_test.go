package upgrade

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestRunBoundedConcurrencyLimit: never more than `limit` fns run at once, and all
// items run when none fail.
func TestRunBoundedConcurrencyLimit(t *testing.T) {
	const limit = 3
	var inflight, maxSeen int32
	var done int32
	items := make([]int, 12)
	err := runBounded(items, limit, func(int) error {
		n := atomic.AddInt32(&inflight, 1)
		for {
			m := atomic.LoadInt32(&maxSeen)
			if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		atomic.AddInt32(&done, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if done != int32(len(items)) {
		t.Errorf("ran %d items, want %d", done, len(items))
	}
	if maxSeen > limit {
		t.Errorf("max concurrency %d exceeded limit %d", maxSeen, limit)
	}
}

// TestRunBoundedStopsLaunchingOnError preserves the serial failure behavior.
func TestRunBoundedStopsLaunchingOnError(t *testing.T) {
	boom := errors.New("boom")
	var started int32
	// item 0 fails fast; with limit 1 the failure must stop items 1..9 launching.
	err := runBounded([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 1, func(i int) error {
		atomic.AddInt32(&started, 1)
		if i == 0 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if started != 1 {
		t.Errorf("started %d fns, want 1 (must stop launching after the first error)", started)
	}
}

// TestRunBoundedSerialDefault: limit<1 behaves as fully serial and runs everything
// on the success path.
func TestRunBoundedSerialDefault(t *testing.T) {
	var n int32
	if err := runBounded([]int{1, 2, 3}, 0, func(int) error { atomic.AddInt32(&n, 1); return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("ran %d, want 3", n)
	}
}

func TestRunBoundedWaitsForInflightAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("first worker failed")
		secondStarted := make(chan struct{})
		releaseSecond := make(chan struct{})
		secondFinished := make(chan struct{})
		result := make(chan error, 1)
		var started atomic.Int32
		go func() {
			result <- runBounded([]int{0, 1, 2, 3}, 2, func(i int) error {
				started.Add(1)
				switch i {
				case 0:
					<-secondStarted
					return boom
				case 1:
					close(secondStarted)
					<-releaseSecond
					close(secondFinished)
				}
				return nil
			})
		}()
		// Worker 0 has failed while worker 1 remains blocked. The runner must
		// wait for worker 1 without starting any later items.
		synctest.Wait()
		if got := started.Load(); got != 2 {
			t.Errorf("started %d workers, want 2", got)
		}
		select {
		case err := <-result:
			t.Errorf("returned before in-flight work completed: %v", err)
		default:
		}
		close(releaseSecond)
		synctest.Wait()
		select {
		case <-secondFinished:
		default:
			t.Error("in-flight worker did not finish")
		}
		select {
		case err := <-result:
			if !errors.Is(err, boom) {
				t.Errorf("error = %v, want first failure", err)
			}
		default:
			t.Error("runner did not return after in-flight work finished")
		}
		if got := started.Load(); got != 2 {
			t.Errorf("started %d workers after failure, want 2", got)
		}
	})
}
