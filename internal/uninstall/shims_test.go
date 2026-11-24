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

// one process per entry: pid -> "state ppid", each started at tick 1000.
func fakeProc(t *testing.T, fx *hostEffects, procs map[int]string) {
	t.Helper()
	root := t.TempDir()
	fx.procRoot = root
	for pid, stat := range procs {
		writeStat(t, fx, pid, stat, "1000")
	}
}

// the command name holds a space and a ')': parsing must read after the last ')'.
func writeStat(t *testing.T, fx *hostEffects, pid int, stateAndPPID, start string) {
	t.Helper()
	dir := filepath.Join(fx.procRoot, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// fields 3 (state) and 4 (ppid), 16 zeros for 5-20, 0 for 21, then field 22.
	line := fmt.Sprintf("%d (weird) name) %s %s0 %s 0 0\n", pid, stateAndPPID, strings.Repeat("0 ", 16), start)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withShimSignals(t *testing.T, alive *[]int, onSignal func(pid int, sig syscall.Signal)) (*[]string, *hostEffects) {
	t.Helper()
	// sequential by swap's exception: it shortens the shared idleShimWait and pollInterval.
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

func TestStopIdleShimsSignalsNothingWhenItCannotReadTheProcessTable(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	fx := &hostEffects{}
	fx.procRoot = filepath.Join(t.TempDir(), "absent")
	fx.shimPIDs = func() ([]int, error) { return []int{100}, nil }
	fx.signalPID = func(pid int, _ syscall.Signal) error { t.Errorf("signalled %d with no process table", pid); return nil }
	stopIdleShims(io.Discard, fx)
}

func TestStopIdleShimsNeverSignalsAReusedPID(t *testing.T) {
	alive := []int{102}
	reused := false
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{102: "S 1"})
	pids := fx.shimPIDs
	fx.shimPIDs = func() ([]int, error) {
		// after idleShims chose 102, it exits and a new process takes its PID.
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

func TestStopIdleShimsTreatsAnUnreadableChildAsLive(t *testing.T) {
	alive := []int{103}
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{103: "S 1"})
	// a directory where the stat file should be: unreadable as any user.
	if err := os.MkdirAll(filepath.Join(fx.procRoot, "300", "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	stopIdleShims(io.Discard, fx)
	if len(*signals) != 0 {
		t.Errorf("signals = %q; a shim with an unreadable child was treated as idle", *signals)
	}
}

func TestSignalShimsRechecksEachShimBeforeItsSignal(t *testing.T) {
	alive := []int{101, 102}
	// fx is assigned before the closure can run: it only fires from signalShims.
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

func TestUnreadableStartTimeSkipsAShimButAbortsTheTopLevelCheck(t *testing.T) {
	alive := []int{101, 102}
	signals, fx := withShimSignals(t, &alive, nil)
	fakeProc(t, fx, map[int]string{102: "S 1"})
	// too few fields for startTime to reach field 22: unreadable, not gone.
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
