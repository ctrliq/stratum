package comps

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// statusServer answers every status request with what next returns for the
// time since the server started.
func statusServer(t *testing.T, next func(since time.Duration) supervisor.ProcStatus) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	old := version.ControlSocket
	version.ControlSocket = socket
	t.Cleanup(func() { version.ControlSocket = old })
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req struct{ Op, Name string }
			if err := json.NewDecoder(conn).Decode(&req); err == nil {
				_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true,
					"procs": []supervisor.ProcStatus{next(time.Since(start))}})
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); wg.Wait() })
}

func shortWaits(t *testing.T, capWait time.Duration) {
	t.Helper()
	oldCap, oldPoll := gateWaitCap, replacementPoll
	gateWaitCap, replacementPoll = capWait, 5*time.Millisecond
	t.Cleanup(func() { gateWaitCap, replacementPoll = oldCap, oldPoll })
}

// A kubelet whose start is held for an image import comes back long after the
// time a restart gives it. The time at the gate is the import's, not
// kubelet's, so the restart waits it out.
func TestRestartWaitDoesNotCountTimeAtTheStartGate(t *testing.T) {
	shortWaits(t, time.Minute)
	const timeout = 100 * time.Millisecond
	statusServer(t, func(since time.Duration) supervisor.ProcStatus {
		if since < 4*timeout {
			return supervisor.ProcStatus{Name: "kubelet", Waiting: true}
		}
		return supervisor.ProcStatus{Name: "kubelet", Pid: 200, Running: true}
	})
	if err := waitForReplacement("kubelet", 100, "restart", timeout); err != nil {
		t.Fatalf("a restart held at its gate for four timeouts failed: %v", err)
	}
}

// A gate that never passes still ends the wait, and the error says the
// component was held at its gate rather than that it did not come back.
func TestRestartWaitEndsAtTheCapWhenTheGateNeverPasses(t *testing.T) {
	shortWaits(t, 150*time.Millisecond)
	statusServer(t, func(time.Duration) supervisor.ProcStatus {
		return supervisor.ProcStatus{Name: "kubelet", Waiting: true}
	})
	start := time.Now()
	err := waitForReplacement("kubelet", 100, "restart", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "held at its start gate") {
		t.Fatalf("error = %v, want one that names the start gate", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wait ran %s, past its cap", took)
	}
}

// The time after the gate is still limited: a component that passes its gate
// and then does not start fails, with the time at the gate named.
func TestRestartWaitStillLimitsTheTimeAfterTheGate(t *testing.T) {
	shortWaits(t, time.Minute)
	const timeout = 100 * time.Millisecond
	statusServer(t, func(since time.Duration) supervisor.ProcStatus {
		return supervisor.ProcStatus{Name: "kubelet", Waiting: since < 2*timeout}
	})
	start := time.Now()
	err := waitForReplacement("kubelet", 100, "restart", timeout)
	if err == nil || !strings.Contains(err.Error(), "did not come back") || !strings.Contains(err.Error(), "start gate") {
		t.Fatalf("error = %v, want a timeout that names the time at the gate", err)
	}
	if took := time.Since(start); took < 3*timeout || took > 5*time.Second {
		t.Errorf("the wait ran %s, want about the gate's 200ms and the timeout's 100ms", took)
	}
}

// A supervisor from before the waiting field reports nothing about the gate,
// so the wait is the plain timeout it always was, the old process's pid never
// counting as the replacement.
func TestRestartWaitAgainstASupervisorThatReportsNoGate(t *testing.T) {
	shortWaits(t, time.Minute)
	statusServer(t, func(time.Duration) supervisor.ProcStatus {
		return supervisor.ProcStatus{Name: "kubelet", Pid: 100, Running: true}
	})
	start := time.Now()
	err := waitForReplacement("kubelet", 100, "restart", 100*time.Millisecond)
	if err == nil || err.Error() != "kubelet did not come back within 100ms of restart" {
		t.Fatalf("error = %v, want the plain timeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wait ran %s on a 100ms timeout", took)
	}
}

// The cap is on the whole wait. A gate that passes just short of it leaves the
// component only what is left of the cap to start in, not a full timeout more.
func TestRestartWaitCapCoversTheTimeAfterTheGateToo(t *testing.T) {
	const capWait = 200 * time.Millisecond
	shortWaits(t, capWait)
	statusServer(t, func(since time.Duration) supervisor.ProcStatus {
		return supervisor.ProcStatus{Name: "kubelet", Waiting: since < capWait-50*time.Millisecond}
	})
	start := time.Now()
	err := waitForReplacement("kubelet", 100, "restart", 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wait ran %s, past its %s cap, on the timeout left after the gate", took, capWait)
	}
}
