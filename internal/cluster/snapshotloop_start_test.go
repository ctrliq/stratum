package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// the first save happens at loop start, before any tick could fire on a 24h interval.
func TestSnapshotLoopSavesOnceRightWhenItStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    intervalHours: 24\n")
		oldSnap := snapshotSave
		t.Cleanup(func() { snapshotSave = oldSnap })
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) { saves.Add(1); return "", nil }

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfg)

		// No fake time has passed: any save can only be the start-of-loop save.
		synctest.Wait()
		if got := saves.Load(); got != 1 {
			t.Fatalf("snapshotLoop took %d saves with no time advanced, want 1: the first scheduled snapshot must happen when the loop starts, before the first tick", got)
		}
	})
}

// the first save is retried every snapshotStartRetry until it succeeds, and stays silent.
func TestSnapshotLoopRetriesTheFirstSaveUntilItSucceeds(t *testing.T) {
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
		oldInterval, oldSnap, oldRetry := snapshotInterval, snapshotSave, snapshotStartRetry
		t.Cleanup(func() { snapshotInterval, snapshotSave, snapshotStartRetry = oldInterval, oldSnap, oldRetry })
		snapshotInterval = func(*ClusterConfig) time.Duration { return 24 * time.Hour }
		snapshotStartRetry = time.Minute
		var attempts atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) {
			if attempts.Add(1) < 3 {
				return "", errors.New("connection refused")
			}
			return "", nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// Two failed retries, then a success: far short of any tick or the window.
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if got := attempts.Load(); got != 3 {
			t.Fatalf("start saves = %d, want 3 (the failed first save plus one retry, then the save that succeeded): the loop must retry the first save every snapshotStartRetry until one succeeds", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), "stratum: scheduled etcd snapshot:") {
		t.Errorf("start-retry failures were logged while the retry window was still open; stderr said:\n%s", out)
	}
}

// a start phase that fails for a full interval reports its last error once, then ticks on.
func TestSnapshotLoopLogsTheLastErrorOnceWhenTheStartWindowEnds(t *testing.T) {
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
		oldInterval, oldSnap, oldRetry := snapshotInterval, snapshotSave, snapshotStartRetry
		t.Cleanup(func() { snapshotInterval, snapshotSave, snapshotStartRetry = oldInterval, oldSnap, oldRetry })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		snapshotStartRetry = 30 * time.Minute
		var attempts atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) {
			if n := attempts.Add(1); n <= 2 {
				return "", fmt.Errorf("attempt %d", n)
			}
			return "", nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// The 1h retry falls on the window's end; the ticker arms at 1h and saves at 2h.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := attempts.Load(); got != 3 {
			t.Fatalf("saves = %d, want 3 (two failed start attempts across the window plus the first tick's save): the loop must keep running on its interval after the start window ends", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if lines := strings.Count(string(out), "stratum: scheduled etcd snapshot:"); lines != 1 {
		t.Errorf("the end-of-window failure was logged %d times, want exactly once; stderr said:\n%s", lines, out)
	}
	if !strings.Contains(string(out), "stratum: scheduled etcd snapshot: attempt 2\n") {
		t.Errorf("the logged error is not the window's LAST start failure (attempt 2); stderr said:\n%s", out)
	}
}

// the start phase watches the context between retries, so shutdown is prompt and saves no more.
func TestSnapshotLoopStopsRetryingWhenTheContextIsCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldInterval, oldSnap, oldRetry := snapshotInterval, snapshotSave, snapshotStartRetry
		t.Cleanup(func() { snapshotInterval, snapshotSave, snapshotStartRetry = oldInterval, oldSnap, oldRetry })
		snapshotInterval = func(*ClusterConfig) time.Duration { return 24 * time.Hour }
		snapshotStartRetry = time.Minute
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) {
			saves.Add(1)
			return "", errors.New("connection refused")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			snapshotLoop(ctx, cfgForLoop(t))
		}()

		// The first save has failed and the loop is waiting out the retry.
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("snapshotLoop did not return promptly on the cancelled context during a retry wait")
		}
		if got := saves.Load(); got != 1 {
			t.Fatalf("saves = %d, want 1: the cancelled context must stop the start phase before any further save", got)
		}
	})
}

// the start-of-loop save does not replace the interval; later ticks still save.
func TestSnapshotLoopSavesAgainOnItsTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldInterval, oldSnap := snapshotInterval, snapshotSave
		t.Cleanup(func() { snapshotInterval, snapshotSave = oldInterval, oldSnap })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) { saves.Add(1); return "", nil }

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		synctest.Wait()
		if got := saves.Load(); got != 1 {
			t.Fatalf("saves after start = %d, want 1", got)
		}
		// With the start save counted, the second save must come from the ticker.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.Load(); got < 2 {
			t.Fatalf("saves after two fake hours = %d, want at least 2: the loop must keep saving on its interval after the start-of-loop save", got)
		}
	})
}

// a failed first save that recovers inside the window is retried silently, then the loop ticks on.
func TestSnapshotLoopKeepsRunningToItsNextTickAfterAFailedFirstSave(t *testing.T) {
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
		oldInterval, oldSnap, oldRetry := snapshotInterval, snapshotSave, snapshotStartRetry
		t.Cleanup(func() { snapshotInterval, snapshotSave, snapshotStartRetry = oldInterval, oldSnap, oldRetry })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		snapshotStartRetry = 15 * time.Minute
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) {
			if saves.Add(1) < 4 {
				return "", errors.New("disk full")
			}
			return "", nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// Three failed saves, a success at 45m, then the ticker's 1h45m tick.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.Load(); got != 5 {
			t.Fatalf("saves = %d, want 5 (three failed start retries, the successful retry, then the next tick's save): the loop must keep running to its next tick after a failed first save", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), "stratum: scheduled etcd snapshot:") {
		t.Errorf("a failed first save that recovers inside the retry window was logged; stderr said:\n%s", out)
	}
}

// an already-cancelled context returns before the first save.
func TestSnapshotLoopWithCancelledContextSavesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldSnap := snapshotSave
		t.Cleanup(func() { snapshotSave = oldSnap })
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) { saves.Add(1); return "", nil }

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		snapshotLoop(ctx, cfgForLoop(t))

		synctest.Wait()
		if got := saves.Load(); got != 0 {
			t.Fatalf("saves = %d, want 0: an already-cancelled context must return before the first save", got)
		}
	})
}

// a save that wrote locally but failed its upload ends the start phase: retrying would prune.
func TestSnapshotLoopEndsTheStartPhaseWhenOnlyTheUploadFailed(t *testing.T) {
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
		oldInterval, oldSnap, oldRetry := snapshotInterval, snapshotSave, snapshotStartRetry
		t.Cleanup(func() { snapshotInterval, snapshotSave, snapshotStartRetry = oldInterval, oldSnap, oldRetry })
		snapshotInterval = func(*ClusterConfig) time.Duration { return 24 * time.Hour }
		snapshotStartRetry = time.Minute
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) {
			saves.Add(1)
			return "/var/lib/stratum/snapshots/x.db", errors.New("S3 upload failed")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := saves.Load(); got != 1 {
			t.Fatalf("start saves = %d, want 1: a save that wrote the local snapshot must end the start phase even when its upload failed", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if lines := strings.Count(string(out), "stratum: scheduled etcd snapshot: S3 upload failed\n"); lines != 1 {
		t.Errorf("the upload failure was logged %d times, want exactly once; stderr said:\n%s", lines, out)
	}
}

// a start phase still failing after bindingWarnAfter warns once and keeps retrying.
func TestSnapshotLoopWarnsOnceWhenTheStartPhaseKeepsFailing(t *testing.T) {
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
		oldInterval, oldSnap, oldRetry, oldWarn := snapshotInterval, snapshotSave, snapshotStartRetry, bindingWarnAfter
		t.Cleanup(func() {
			snapshotInterval, snapshotSave, snapshotStartRetry, bindingWarnAfter = oldInterval, oldSnap, oldRetry, oldWarn
		})
		snapshotInterval = func(*ClusterConfig) time.Duration { return 24 * time.Hour }
		snapshotStartRetry = time.Minute
		bindingWarnAfter = 5 * time.Minute
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) (string, error) {
			saves.Add(1)
			return "", errors.New("connection refused")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// Attempts at 0..10m: the warning fires on the 5m attempt only.
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := saves.Load(); got != 11 {
			t.Fatalf("start saves = %d, want 11: the warning must not stop the retries", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if lines := strings.Count(string(out), "stratum: WARN: scheduled etcd snapshot still failing"); lines != 1 {
		t.Errorf("the still-failing warning was logged %d times, want exactly once; stderr said:\n%s", lines, out)
	}
	if !strings.Contains(string(out), "stratum: WARN: scheduled etcd snapshot still failing after 5m0s: connection refused; retrying every 1m0s\n") {
		t.Errorf("the warning did not fire on the first attempt past bindingWarnAfter; stderr said:\n%s", out)
	}
	if strings.Contains(string(out), "stratum: scheduled etcd snapshot:") {
		t.Errorf("the end-of-window line was logged before the window ended; stderr said:\n%s", out)
	}
}

// a hung start save is cut off at the window's deadline, logged once, and the loop ticks on.
func TestSnapshotLoopStopsAStartSaveThatOutlastsTheWindow(t *testing.T) {
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
		oldInterval, oldSnap := snapshotInterval, snapshotSave
		t.Cleanup(func() { snapshotInterval, snapshotSave = oldInterval, oldSnap })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		var saves atomic.Int32
		snapshotSave = func(ctx context.Context, _ *ClusterConfig) (string, error) {
			if saves.Add(1) == 1 {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "", nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// The first save hangs to the window's end at 1h; the ticker's tick saves at 2h.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.Load(); got != 2 {
			t.Fatalf("saves = %d, want 2 (the hung start save, stopped at the window's end, then the first tick's): a start save must not outlast the window", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if lines := strings.Count(string(out), "stratum: scheduled etcd snapshot: context deadline exceeded\n"); lines != 1 {
		t.Errorf("the stopped start save was logged %d times, want exactly once; stderr said:\n%s", lines, out)
	}
}

// a window ending between retries ends the wait with it; the ticker arms at the window's end.
func TestSnapshotLoopEndsTheWindowDuringARetryWait(t *testing.T) {
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
		oldInterval, oldSnap, oldRetry := snapshotInterval, snapshotSave, snapshotStartRetry
		t.Cleanup(func() { snapshotInterval, snapshotSave, snapshotStartRetry = oldInterval, oldSnap, oldRetry })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		snapshotStartRetry = 25 * time.Minute
		var saves, expired atomic.Int32
		snapshotSave = func(ctx context.Context, _ *ClusterConfig) (string, error) {
			n := saves.Add(1)
			if ctx.Err() != nil {
				expired.Add(1)
			}
			if n <= 3 {
				return "", fmt.Errorf("attempt %d", n)
			}
			return "", nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// The 75m retry falls past the window's 1h end; the ticker arms at 1h, saves at 2h.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.Load(); got != 4 {
			t.Fatalf("saves = %d, want 4 (three failed start attempts, then the tick at 2h): the window's end must end the retry wait and arm the ticker", got)
		}
		if n := expired.Load(); n != 0 {
			t.Errorf("%d save(s) ran under an expired context; the window's end must not trigger another start save", n)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if lines := strings.Count(string(out), "stratum: scheduled etcd snapshot:"); lines != 1 {
		t.Errorf("the window's end was logged %d times, want exactly once; stderr said:\n%s", lines, out)
	}
	if !strings.Contains(string(out), "stratum: scheduled etcd snapshot: attempt 3\n") {
		t.Errorf("the logged error is not the last real start failure (attempt 3); stderr said:\n%s", out)
	}
}

// the window's deadline bounds the start phase only; a tick keeps the controller's context.
func TestSnapshotLoopTicksKeepTheControllerContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldInterval, oldSnap := snapshotInterval, snapshotSave
		t.Cleanup(func() { snapshotInterval, snapshotSave = oldInterval, oldSnap })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		var saves, tickDeadlines atomic.Int32
		snapshotSave = func(ctx context.Context, _ *ClusterConfig) (string, error) {
			if saves.Add(1) > 1 {
				if _, ok := ctx.Deadline(); ok {
					tickDeadlines.Add(1)
				}
			}
			return "", nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.Load(); got < 2 {
			t.Fatalf("saves = %d, want at least 2", got)
		}
		if n := tickDeadlines.Load(); n != 0 {
			t.Errorf("%d tick save(s) ran under a deadline; a tick must keep the controller's context", n)
		}
	})
}

// cfgForLoop returns a minimal config; the faked interval seam covers the snapshot settings.
func cfgForLoop(t *testing.T) *ClusterConfig {
	return loadClusterConfig(t, "spec:\n  snapshots:\n    intervalHours: 24\n")
}
