package uninstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProc writes a run's /proc (fx.procRoot) with one process per entry:
// pid -> "state ppid", each started at tick 1000.
func fakeProc(t *testing.T, fx *hostEffects, procs map[int]string) {
	t.Helper()
	root := t.TempDir()
	fx.procRoot = root
	for pid, stat := range procs {
		writeStat(t, fx, pid, stat, "1000")
	}
}

// writeStat writes a full-length /proc/<pid>/stat. The command name has a space
// and a ')' in it, to check the parsing reads fields after the last ')'.
func writeStat(t *testing.T, fx *hostEffects, pid int, stateAndPPID, start string) {
	t.Helper()
	dir := filepath.Join(fx.procRoot, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fields 3 (state) and 4 (ppid), 16 zeros for fields 5-20, 0 for 21,
	// then field 22, the start time.
	line := fmt.Sprintf("%d (weird) name) %s %s0 %s 0 0\n", pid, stateAndPPID, strings.Repeat("0 ", 16), start)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withShimSignals(t *testing.T, alive *[]int, onSignal func(pid int, sig syscall.Signal)) (*[]string, *hostEffects) {
	t.Helper()
	// Sequential by swap's stated exception: it shortens the shared
	// idleShimWait and pollInterval.
	fx := &hostEffects{}
	var signals []string
	oldWait, oldPoll := idleShimWait, pollInterval
	t.Cleanup(func() { idleShimWait, pollInterval = oldWait, oldPoll })
	idleShimWait, pollInterval = 50*time.Millisecond, time.Millisecond
	fx.shimPIDs = func() ([]int, error) { return slices.Clone(*alive), nil }
	fx.signalPID = func(pid int, sig syscall.Signal) error {
		signals = append(signals, fmt.Sprintf("%v %d", sig, pid))
		if onSignal != nil {
			onSignal(pid, sig)
		}
		return nil
	}
	return &signals, fx
}

// TestStopIdleShimsLeavesAShimThatStillHoldsAContainer: a shim with a
// live child is a container still running, and is never signalled; one with no
// child, or only a zombie, holds nothing and is stopped.
func TestStopIdleShimsLeavesAShimThatStillHoldsAContainer(t *testing.T) {
	alive := []int{100, 101, 102}
	signals, fx := withShimSignals(t, &alive, func(pid int, sig syscall.Signal) {
		if sig == syscall.SIGTERM && pid == 101 {
			alive = slices.DeleteFunc(alive, func(p int) bool { return p == 101 })
		}
	})
	fakeProc(t, fx, map[int]string{
		100: "S 1", // shim with a live container
		200: "S 100",
		101: "S 1", // shim whose container is a zombie
		201: "Z 101",
		102: "S 1", // shim with nothing under it
	})

	stopIdleShims(io.Discard, fx)

	want := []string{"terminated 101", "terminated 102", "killed 102"}
	if !slices.Equal(*signals, want) {
		t.Errorf("signals = %q, want %q (101 exits on SIGTERM, 102 needs SIGKILL, 100 is never touched)", signals, want)
	}
}

// TestStopIdleShimsSignalsNothingWhenItCannotReadTheProcessTable: not knowing
// whether a shim holds a container is not the same as knowing it holds none.
func TestStopIdleShimsSignalsNothingWhenItCannotReadTheProcessTable(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{}
	fx.procRoot = filepath.Join(t.TempDir(), "absent")
	fx.shimPIDs = func() ([]int, error) { return []int{100}, nil }
	fx.signalPID = func(pid int, _ syscall.Signal) error { t.Errorf("signalled %d with no process table", pid); return nil }
	stopIdleShims(io.Discard, fx)
}

// TestStopIdleShimsNeverSignalsAReusedPID: an idle shim that
// exits can have its PID reused before the signal. The start time is checked
// before every signal, so the new process is left alone.
func TestStopIdleShimsNeverSignalsAReusedPID(t *testing.T) {
	alive := []int{102}
	reused := false
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{102: "S 1"})
	pids := fx.shimPIDs
	fx.shimPIDs = func() ([]int, error) {
		// After idleShims chose 102, it exits and a new process takes its PID.
		if !reused {
			reused = true
			return pids()
		}
		writeStat(t, fx, 102, "S 1", "2000")
		return pids()
	}
	stopIdleShims(io.Discard, fx)
	if len(*signals) != 0 {
		t.Errorf("signals = %q; a PID reused by another process was signalled", *signals)
	}
}

// TestStopIdleShimsTreatsAnUnreadableChildAsLive: a stat that
// cannot be read is not proof the process is gone, so its parent shim is not
// idle.
func TestStopIdleShimsTreatsAnUnreadableChildAsLive(t *testing.T) {
	alive := []int{103}
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{103: "S 1"})
	// A directory where the child's stat file should be: unreadable as a file
	// whatever user the test runs as.
	if err := os.MkdirAll(filepath.Join(fx.procRoot, "300", "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	stopIdleShims(io.Discard, fx)
	if len(*signals) != 0 {
		t.Errorf("signals = %q; a shim with an unreadable child was treated as idle", *signals)
	}
}

// TestSignalShimsRechecksEachShimBeforeItsSignal: signalling
// one shim can let a later one's PID be reused before it is reached, so each
// is checked again immediately before its own signal.
func TestSignalShimsRechecksEachShimBeforeItsSignal(t *testing.T) {
	alive := []int{101, 102}
	// fx is assigned before the onSignal closure below can ever run: the
	// closure only fires once signalShims, after fakeProc, is called.
	var fx *hostEffects
	signals, created := withShimSignals(t, &alive, func(pid int, _ syscall.Signal) {
		if pid == 101 {
			writeStat(t, fx, 102, "S 1", "2000")
		}
	})
	fx = created
	fakeProc(t, fx, map[int]string{101: "S 1", 102: "S 1"})
	signalShims(fx, []shimProc{{101, "1000"}, {102, "1000"}}, syscall.SIGTERM)
	if want := []string{"terminated 101"}; !slices.Equal(*signals, want) {
		t.Errorf("signals = %q, want %q; 102 was reused after the first signal", *signals, want)
	}
}

// stillProcs reads the same identity for shims and the top-level process but
// each caller keeps its own rule for a start time it cannot read: a shim is
// skipped (best-effort; the host-process check that follows still refuses),
// while the top-level process fails closed and aborts.
func TestUnreadableStartTimeSkipsAShimButAbortsTheTopLevelCheck(t *testing.T) {
	alive := []int{101, 102}
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{102: "S 1"})
	// Too few fields for startTime to reach field 22: unreadable, not gone.
	if err := os.MkdirAll(filepath.Join(fx.procRoot, "101"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.procRoot, "101", "stat"), []byte("101 (weird) name) S 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	procs := []shimProc{{101, "1000"}, {102, "1000"}}

	t.Run("shims skip the unreadable one", func(t *testing.T) {
		if got := stillShims(fx, procs); !slices.Equal(got, []shimProc{{102, "1000"}}) {
			t.Errorf("stillShims = %v, want only 102", got)
		}
		signalShims(fx, procs, syscall.SIGTERM)
		if want := []string{"terminated 102"}; !slices.Equal(*signals, want) {
			t.Errorf("signals = %q, want %q; 101 is skipped, 102 still signalled", *signals, want)
		}
	})

	t.Run("top level fails closed", func(t *testing.T) {
		*signals = (*signals)[:0]
		fx.topLevelPIDs = func() ([]int, error) { return []int{101, 102}, nil }
		if _, err := stillTopLevel(fx, procs); err == nil {
			t.Fatal("stillTopLevel read an unreadable start time as gone")
		}
		if err := signalTopLevel(fx, procs, syscall.SIGTERM); err == nil {
			t.Fatal("signalTopLevel carried on past an unreadable start time")
		}
		if len(*signals) != 0 {
			t.Errorf("signals = %q, want none: nothing is signalled on an unconfirmed identity", *signals)
		}
	})
}

// A poll lists the shims once however many it tracks, and signalShims lists
// afresh before each signal, since a signal can let a later PID be reused.
func TestStillShimsListsOnceAndSignalShimsListsPerSignal(t *testing.T) {
	alive := []int{101, 102, 103}
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{101: "S 1", 102: "S 1", 103: "S 1"})
	listings := 0
	list := fx.shimPIDs
	fx.shimPIDs = func() ([]int, error) { listings++; return list() }
	procs := []shimProc{{101, "1000"}, {102, "1000"}, {103, "1000"}}

	if got := stillShims(fx, procs); !slices.Equal(got, procs) {
		t.Fatalf("stillShims = %v, want all three", got)
	}
	if listings != 1 {
		t.Errorf("stillShims listed the shims %d times for 3 shims, want 1", listings)
	}

	listings = 0
	signalShims(fx, procs, syscall.SIGTERM)
	if listings != len(procs) {
		t.Errorf("signalShims listed %d times for %d signals, want one fresh listing each", listings, len(procs))
	}
	if len(*signals) != len(procs) {
		t.Errorf("signals = %q, want one per shim", *signals)
	}

	listings = 0
	fx.shimPIDs = func() ([]int, error) { listings++; return nil, errors.New("read /proc: denied") }
	if got := stillShims(fx, procs); len(got) != 0 || listings != 1 {
		t.Errorf("stillShims on a listing error = %v after %d listings, want nil after 1", got, listings)
	}
}
