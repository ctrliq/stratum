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

// TestSnapshotLoopSavesOnceRightWhenItStarts pins when the loop's first
// scheduled snapshot happens: a control plane restarted more often than
// spec.snapshots.intervalHours would otherwise never take a scheduled
// snapshot, because every restart resets the ticker and the first save
// waited out a whole interval. The loop runs here on a 24h interval and the
// save is observed with no time advanced at all — with the ticker as the
// only save trigger, no tick could have fired, so a save must have happened
// when the loop started.
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

		// No fake time has passed here: with a 24h interval a tick is
		// impossible, so any save can only be the start-of-loop save.
		synctest.Wait()
		if got := saves.Load(); got != 1 {
			t.Fatalf("snapshotLoop took %d saves with no time advanced, want 1: the first scheduled snapshot must happen when the loop starts, before the first tick", got)
		}
	})
}

// TestSnapshotLoopRetriesTheFirstSaveUntilItSucceeds pins the start phase's
// retry: the loop starts before the supervisor has etcd up, so the first
// save is expected to fail and is retried every snapshotStartRetry until
// one succeeds. Two failures then a success — all inside the retry window —
// must take exactly three saves before any tick and stay silent: etcd
// coming up is not a warning worth the daemon's log.
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

		// Two failed retries at one-minute spacing, then the third save
		// succeeds — three minutes of fake time, far short of any tick on
		// a 24h interval and far short of the one-interval window.
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

// TestSnapshotLoopLogsTheLastErrorOnceWhenTheStartWindowEnds pins the end of
// the retry window: start attempts that keep failing for a full snapshot
// interval are not etcd coming up any more, so the window's last error is
// reported exactly once with the line a failed tick uses — and the loop
// still goes on to save on its regular ticks.
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

		// Attempts at 0 and 30m fail. The retry due at 1h falls on the
		// window's end, so no attempt is made under the expired deadline:
		// the window closes with no success. The ticker then arms at 1h and
		// its tick at 2h saves, so after two fake hours there are three.
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

// TestSnapshotLoopStopsRetryingWhenTheContextIsCancelled pins that the start
// phase watches the context while it waits between retries: a controller
// shut down during the wait returns promptly, without any further saves.
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

// TestSnapshotLoopSavesAgainOnItsTicks pins that the start-of-loop save did
// not replace the interval: after the first save the loop keeps the ticker,
// and a later tick saves again.
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
		// Two fake hours pass two ticks: with the start save, the second
		// save must have come from the ticker, not the loop start.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.Load(); got < 2 {
			t.Fatalf("saves after two fake hours = %d, want at least 2: the loop must keep saving on its interval after the start-of-loop save", got)
		}
	})
}

// TestSnapshotLoopKeepsRunningToItsNextTickAfterAFailedFirstSave pins the
// loop's liveness after a failed first save: the failed attempt is retried
// inside the start window without a word (the loop starts before etcd is
// up), the retry succeeds, and the loop goes on to its next regular tick.
// The reporting half — what is logged when the whole window fails — is
// pinned by TestSnapshotLoopLogsTheLastErrorOnceWhenTheStartWindowEnds.
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

		// Failed saves at 0, 15m and 30m, the retry succeeds at 45m, and
		// the ticker armed at 45m saves again on its 1h45m tick: the loop
		// kept running past the failed first save to its next tick.
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

// TestSnapshotLoopWithCancelledContextSavesNothing pins the start gate: when
// the loop's context is already cancelled when the loop starts, it returns
// without saving — a shutting-down controller takes no snapshot.
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

// TestSnapshotLoopEndsTheStartPhaseWhenOnlyTheUploadFailed pins that a save
// which wrote the local snapshot but failed its S3 upload is not retried:
// each retry would save and prune again, rotating the pre-restart snapshots
// out of local retention within minutes. The upload error is logged at once
// and the loop goes to its ticker.
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

// TestSnapshotLoopWarnsOnceWhenTheStartPhaseKeepsFailing pins the early
// warning: a start phase still failing after bindingWarnAfter says so once,
// long before the window's end, and keeps retrying.
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

// TestSnapshotLoopStopsAStartSaveThatOutlastsTheWindow pins the window's
// deadline on the start saves: etcdctl has no timeout of its own, so a save
// that hangs while etcd comes up would otherwise hold the loop past the
// window with no log line and no tick. The hung save is stopped when the
// window ends, its error is logged once, and the loop goes on to its ticks.
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

		// The first save hangs until the window ends at 1h, the ticker arms
		// then, and its tick at 2h saves.
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

// TestSnapshotLoopEndsTheWindowDuringARetryWait pins a window that ends
// between two retries: the wait ends with the window, no save is made under
// the expired deadline, the line reports the last real failure, and the
// ticker arms at the window's end rather than after the wait.
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

		// Attempts at 0, 25m and 50m fail. The next retry is due at 75m,
		// past the window's end at 1h, so the wait ends at 1h with no save.
		// The ticker arms at 1h and its tick at 2h saves: four saves.
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

// TestSnapshotLoopTicksKeepTheControllerContext pins that the window's
// deadline is for the start phase only: a tick's save runs under the
// controller's context, with no deadline.
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

// cfgForLoop returns a minimal config for snapshotLoop: the loop reads only
// the snapshot settings, which the faked interval seam covers.
func cfgForLoop(t *testing.T) *ClusterConfig {
	return loadClusterConfig(t, "spec:\n  snapshots:\n    intervalHours: 24\n")
}
