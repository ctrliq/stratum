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

// an idle shim holds nothing: the reason not to kill one is the containers beneath it.

// Variables, not constants: tests shorten them.
var idleShimWait = 5 * time.Second

// PID and start time together identify one process: a PID alone can be reused.
type shimProc struct {
	pid   int
	start string
}

// a reused PID must never be signalled: identity is re-checked before each signal.
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

// best-effort: an unconfirmable shim is skipped, neither waited on nor signalled.
func stillShims(fx *hostEffects, procs []shimProc) []shimProc {
	if fx.shimPIDs == nil {
		return nil
	}
	pids, err := fx.shimPIDs()
	if err != nil {
		return nil
	}
	listed := func() ([]int, error) { return pids, nil }
	var left []shimProc
	for _, p := range procs {
		if still, err := stillProcs(listed, fx, []shimProc{p}); err == nil {
			left = append(left, still...)
		}
	}
	return left
}

// only a confirmed exit or identity change counts as gone; unreadable is an error.
func stillProcs(list func() ([]int, error), fx *hostEffects, procs []shimProc) ([]shimProc, error) {
	now, err := list()
	if err != nil {
		return nil, err
	}
	var left []shimProc
	for _, p := range procs {
		if !slices.Contains(now, p.pid) {
			continue // confirmed gone: not in the current process list at all
		}
		start, err := startTime(fx, p.pid)
		if err != nil {
			if isProcGone(err) {
				continue // confirmed gone: exited between the list above and this read
			}
			return nil, fmt.Errorf("read start time for pid %d: %w", p.pid, err)
		}
		if start == p.start {
			left = append(left, p)
		}
	}
	return left, nil
}

// a signal to one can let a later PID be reused before it is reached.
func signalShims(fx *hostEffects, procs []shimProc, sig syscall.Signal) {
	for _, p := range procs {
		if len(stillShims(fx, []shimProc{p})) == 1 {
			_ = fx.signalPID(p.pid, sig)
		}
	}
}

// field 22 of /proc/<pid>/stat, in clock ticks since boot.
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

// fields after the command name, which is parenthesized and may contain spaces.
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

// a shim is only stopped when it is known to hold nothing.
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
			// unreadable is not "no child": a live container could be behind it.
			return false, fmt.Errorf("read %s/%s/stat: %w", fx.procRoot, e.Name(), err)
		}
		// the command name is parenthesized and may hold spaces: read after the last ')'.
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
