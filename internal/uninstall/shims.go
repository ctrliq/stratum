package uninstall

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Removing every pod sandbox still left idle runtime shims on a control plane
// once containerd had stopped: measured, 5 of 11 survived with every container
// under them already killed, which blocked the uninstall exactly as before.
// An idle shim holds nothing: the reason not to kill a shim is the containers
// beneath it, and it has none. So once containerd is down, a
// Stratum shim with no live child process is stopped; one with a live child
// is left, and the host-process check that follows still refuses.

// The shim probe is a field on the run's hostEffects value (fx.shimPIDs), and
// the process table it inspects is fx.procRoot; the signal itself is the run's
// fx.signalPID. The idle wait below is read, never written, once the package is
// initialized, so two Runs cannot race over it; tests shorten it, which is why
// it stays a variable.
var idleShimWait = 5 * time.Second

// shimProc is a shim's PID and its start time, which together identify one
// process: a PID alone can be reused.
type shimProc struct {
	pid   int
	start string
}

// stopIdleShims sends SIGTERM to every Stratum runtime shim with no live child,
// then SIGKILL to any still there after idleShimWait, reporting the count
// through w. Before each signal it checks the PID is still a Stratum shim with
// the same start time, so a PID reused by another process is never signalled.
// The signal is the run's signalPID effect; without it there is nothing to
// signal with, so nothing is signalled and the host-process sweep that follows
// still refuses.
func stopIdleShims(w io.Writer, fx *hostEffects) {
	if fx.signalPID == nil {
		return
	}
	idle := idleShims(fx)
	if len(idle) == 0 {
		return
	}
	signalShims(fx, idle, syscall.SIGTERM)
	deadline := time.Now().Add(idleShimWait)
	for time.Now().Before(deadline) && len(stillShims(fx, idle)) > 0 {
		time.Sleep(pollInterval)
	}
	if left := stillShims(fx, idle); len(left) > 0 {
		signalShims(fx, left, syscall.SIGKILL)
	}
	fmt.Fprintf(w, "  stopped %d idle runtime shims (no container left under them)\n", len(idle))
}

// idleShims is the Stratum shims that have no live child process.
func idleShims(fx *hostEffects) []shimProc {
	if fx.shimPIDs == nil {
		return nil
	}
	pids, err := fx.shimPIDs()
	if err != nil {
		return nil
	}
	var idle []shimProc
	for _, pid := range pids {
		start, err := startTime(fx, pid)
		if err != nil {
			continue
		}
		if busy, err := hasLiveChildren(fx, pid); err == nil && !busy {
			idle = append(idle, shimProc{pid, start})
		}
	}
	return idle
}

// stillShims is the subset of procs that are still the same Stratum shims.
func stillShims(fx *hostEffects, procs []shimProc) []shimProc {
	if fx.shimPIDs == nil {
		return nil
	}
	now, err := fx.shimPIDs()
	if err != nil {
		return nil
	}
	var left []shimProc
	for _, p := range procs {
		if !slices.Contains(now, p.pid) {
			continue
		}
		if start, err := startTime(fx, p.pid); err == nil && start == p.start {
			left = append(left, p)
		}
	}
	return left
}

// signalShims checks each shim again immediately before its own signal: a
// signal to one can let a later one's PID be reused before it is reached.
func signalShims(fx *hostEffects, procs []shimProc, sig syscall.Signal) {
	for _, p := range procs {
		if len(stillShims(fx, []shimProc{p})) == 1 {
			_ = fx.signalPID(p.pid, sig)
		}
	}
}

// startTime is field 22 of /proc/<pid>/stat, the process's start time in clock
// ticks since boot.
func startTime(fx *hostEffects, pid int) (string, error) {
	f, err := statFields(filepath.Join(fx.procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	if len(f) < 20 {
		return "", fmt.Errorf("short stat for %d", pid)
	}
	return f[19], nil
}

// statFields is the fields of a /proc/<pid>/stat after the command name, which
// is in parentheses and may contain spaces: state, ppid, ...
func statFields(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil, fmt.Errorf("malformed %s", path)
	}
	return strings.Fields(s[i+1:]), nil
}

// hasLiveChildren reports whether any process that is not a zombie has pid as
// its parent. An unreadable process table is an error, which the caller reads
// as "not idle": a shim is only stopped when it is known to hold nothing.
func hasLiveChildren(fx *hostEffects, pid int) (bool, error) {
	entries, err := os.ReadDir(fx.procRoot)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(fx.procRoot, e.Name(), "stat"))
		if os.IsNotExist(err) {
			continue // exited since the listing
		}
		if err != nil {
			// Unreadable is not "no child": a live container could be behind
			// it.
			return false, fmt.Errorf("read %s/%s/stat: %w", fx.procRoot, e.Name(), err)
		}
		// The command name is in parentheses and may contain spaces, so the
		// fields are read from after the last ')': state, then ppid.
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil || ppid != pid {
			continue
		}
		if f[0] != "Z" && f[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}
