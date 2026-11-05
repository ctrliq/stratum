package cluster

import (
	"context"
	"errors"
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
		snapshotSave = func(context.Context, *ClusterConfig) error { saves.Add(1); return nil }

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

// TestSnapshotLoopSavesAgainOnItsTicks pins that the start-of-loop save did
// not replace the interval: after the first save the loop keeps the ticker,
// and a later tick saves again.
func TestSnapshotLoopSavesAgainOnItsTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldInterval, oldSnap := snapshotInterval, snapshotSave
		t.Cleanup(func() { snapshotInterval, snapshotSave = oldInterval, oldSnap })
		snapshotInterval = func(*ClusterConfig) time.Duration { return time.Hour }
		var saves atomic.Int32
		snapshotSave = func(context.Context, *ClusterConfig) error { saves.Add(1); return nil }

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

// TestSnapshotLoopLogsAFailedFirstSaveAndKeepsRunning pins the failure path
// of the start-of-loop save: it is reported with the very stderr line a
// failed tick uses, and the loop does not exit or retry in a tight loop —
// its next save is the next tick's.
func TestSnapshotLoopLogsAFailedFirstSaveAndKeepsRunning(t *testing.T) {
	// Capture stderr through a pipe the way run_test.go does.
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
		snapshotSave = func(context.Context, *ClusterConfig) error {
			if saves.Add(1) == 1 {
				return errors.New("disk full")
			}
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go snapshotLoop(ctx, cfgForLoop(t))

		// The failed start save and the next tick's save: the loop must
		// still be alive to take the tick's save, and exactly one tick in
		// one fake hour means a tight-loop retry would have saved more.
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := saves.Load(); got != 2 {
			t.Fatalf("saves = %d, want 2 (the failed start save plus the next tick's save): the loop must keep running to its next tick after a failed first save, without retrying in a tight loop", got)
		}
	})

	os.Stderr = origErr
	w.Close()
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "stratum: scheduled etcd snapshot: disk full\n") {
		t.Errorf("the failed first save was not reported with the scheduled-snapshot stderr line; stderr said:\n%s", out)
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
		snapshotSave = func(context.Context, *ClusterConfig) error { saves.Add(1); return nil }

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		snapshotLoop(ctx, cfgForLoop(t))

		synctest.Wait()
		if got := saves.Load(); got != 0 {
			t.Fatalf("saves = %d, want 0: an already-cancelled context must return before the first save", got)
		}
	})
}

// cfgForLoop returns a minimal config for snapshotLoop: the loop reads only
// the snapshot settings, which the faked interval seam covers.
func cfgForLoop(t *testing.T) *ClusterConfig {
	return loadClusterConfig(t, "spec:\n  snapshots:\n    intervalHours: 24\n")
}
