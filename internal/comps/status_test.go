package comps

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// TestStatusIsOneSnapshotAndReportsWhenItCannotBeRead: the roster comes back
// by name with running and wanted as the supervisor reports them, and a
// supervisor that cannot be asked is an error, not an empty roster.
func TestStatusIsOneSnapshotAndReportsWhenItCannotBeRead(t *testing.T) {
	dir, err := os.MkdirTemp("", "status-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	old := version.ControlSocket
	version.ControlSocket = socket
	t.Cleanup(func() { version.ControlSocket = old })

	if _, err := Status(); err == nil {
		t.Fatal("Status succeeded with no supervisor to ask")
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read the request first, as the supervisor does: closing on a
			// client still writing is a broken pipe on Linux.
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true, "procs": []supervisor.ProcStatus{
				{Name: "containerd", Pid: 100, Running: true, Wanted: true},
				{Name: "kubelet", Wanted: true},
			}})
			conn.Close()
		}
	}()
	st, err := Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st["containerd"].Running || st["kubelet"].Running || !st["kubelet"].Wanted {
		t.Errorf("Status() = %+v; want containerd running, kubelet wanted in backoff", st)
	}
}

// TestIsActiveRefusesAForeignSocketOwner pins that a component with a pid
// whose socket another process serves is not active, so a health gate that asks
// IsActive does not pass on it.
func TestIsActiveRefusesAForeignSocketOwner(t *testing.T) {
	dir, err := os.MkdirTemp("", "active-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	old := version.ControlSocket
	version.ControlSocket = socket
	t.Cleanup(func() { version.ControlSocket = old })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true, "procs": []supervisor.ProcStatus{
				{Name: "containerd", Pid: 100, Running: true, Wanted: true, Foreign: true},
				{Name: "kubelet", Pid: 101, Running: true, Wanted: true},
			}})
			conn.Close()
		}
	}()
	if IsActive("containerd") {
		t.Error("containerd with a foreign socket owner reported active")
	}
	if !IsActive("kubelet") {
		t.Error("a healthy running kubelet reported inactive")
	}
}

// TestIsActiveContextDoesNotWaitOnAHungSupervisor: a
// supervisor that accepts the connection and never answers is given up on when
// ctx ends, not after the 30s of the plain request.
func TestIsActiveContextDoesNotWaitOnAHungSupervisor(t *testing.T) {
	dir, err := os.MkdirTemp("", "hung-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	old := version.ControlSocket
	version.ControlSocket = socket
	t.Cleanup(func() { version.ControlSocket = old })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { <-release; conn.Close() }() // never replies
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if IsActiveContext(ctx, "containerd") {
		t.Error("a supervisor that never answered reported containerd active")
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Errorf("took %v inside a 100ms window", took)
	}
}
