package comps

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// Exercise the real client protocol: acknowledging reload is not evidence that
// the old process has exited, let alone that the replacement is running.
func TestReloadWaitsForReplacement(t *testing.T) {
	for _, mode := range []string{"delayed restart", "old supervisor", "baseline status error", "other rejection", "stopped component"} {
		t.Run(mode, func(t *testing.T) {
			// Keep the socket path below the Unix-domain limit on macOS too.
			dir, err := os.MkdirTemp("", "reload-")
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
			var mu sync.Mutex
			var reloaded bool
			var after int
			var received supervisor.ProcSpec
			var receivedName string
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					var req struct {
						Op, Name string
						Process  supervisor.ProcSpec
					}
					if err := json.NewDecoder(conn).Decode(&req); err != nil {
						conn.Close()
						continue
					}
					mu.Lock()
					reply := map[string]any{"ok": true}
					switch req.Op {
					case "reload":
						reloaded, received, receivedName = true, req.Process, req.Name
						if mode == "old supervisor" {
							reply = map[string]any{"error": `unknown op "reload"`}
						} else if mode == "other rejection" {
							reply = map[string]any{"error": "reload rejected"}
						}
					case "status":
						p := supervisor.ProcStatus{Name: "containerd", Pid: 100, Running: true}
						if mode == "baseline status error" && !reloaded {
							reply = map[string]any{"error": "status unavailable"}
						}
						if mode == "stopped component" {
							p.Pid, p.Running = 0, false
						}
						if reloaded {
							after++
							switch after {
							case 1: // old process still draining: must not count as recovery
							case 2:
								p.Pid, p.Running = 0, false
							default:
								p.Pid, p.Running = 200, true
							}
						}
						reply["procs"] = []supervisor.ProcStatus{p}
					}
					mu.Unlock()
					_ = json.NewEncoder(conn).Encode(reply)
					conn.Close()
				}
			}()
			t.Cleanup(func() { ln.Close(); <-done })
			spec := supervisor.ProcSpec{Path: "/new/containerd", Args: []string{"--config", "/new/config"}, Env: []string{"TEST=1"}}
			err = Reload("stratum-containerd.service", spec)
			mu.Lock()
			defer mu.Unlock()
			if mode == "baseline status error" {
				if err == nil || !strings.Contains(err.Error(), "status unavailable") {
					t.Fatalf("error = %v, want baseline status failure", err)
				}
				if reloaded {
					t.Error("reload was sent without a reliable baseline")
				}
				return
			}
			if mode == "old supervisor" || mode == "other rejection" {
				if err == nil {
					t.Fatal("accepted rejected reload")
				}
				if errors.Is(err, supervisor.ErrReloadUnsupported) != (mode == "old supervisor") {
					t.Fatalf("wrong compatibility classification: %v", err)
				}
				if after != 0 {
					t.Errorf("polled %d times after rejected reload", after)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if after < 3 {
					t.Errorf("Reload returned before replacement was running: %d status polls", after)
				}
			}
			if receivedName != "containerd" || received.Path != spec.Path || strings.Join(received.Args, " ") != strings.Join(spec.Args, " ") || strings.Join(received.Env, " ") != strings.Join(spec.Env, " ") {
				t.Errorf("reload request lost normalized name or new spec: %s %+v", receivedName, received)
			}
		})
	}
}
