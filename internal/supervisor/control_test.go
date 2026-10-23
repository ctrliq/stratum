package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortSockDir returns a temp dir with a short absolute path. A unix socket path
// has a ~104-char limit (sun_path), which t.TempDir() under /var/folders on macOS
// can exceed; /tmp keeps it well under. Cleaned up automatically.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(dir + ".stratum.sock.lock")
	})
	return dir
}

// TestServeControlRefusesLiveSocket pins a defense in depth: a second
// supervisor must NOT remove-and-rebind a control socket that a running
// supervisor is still serving (which would leave two supervisors racing the same
// host processes). A stale socket from a crashed run has no listener, so it is
// still rebindable.
func TestServeControlRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "stratum.sock")

	s1 := &Supervisor{procs: map[string]*state{}}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	stop1, err := s1.serveControl(ctx1, path)
	if err != nil {
		t.Fatalf("first serveControl: %v", err)
	}

	// Second supervisor over the live socket: refuse, don't steal.
	s2 := &Supervisor{procs: map[string]*state{}}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if _, err := s2.serveControl(ctx2, path); err == nil {
		t.Fatal("second serveControl stole a live control socket — a racing supervisor could run over the same host processes")
	}

	// After the first stops (socket removed), a fresh supervisor can bind again.
	stop1()
	s3 := &Supervisor{procs: map[string]*state{}}
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	stop3, err := s3.serveControl(ctx3, path)
	if err != nil {
		t.Fatalf("serveControl after the live socket stopped: %v", err)
	}
	// Repeated shutdown of the old owner must not unlink the new listener.
	stop1()
	if !controlSocketLive(path) {
		t.Fatal("old shutdown removed the replacement socket")
	}
	stop3()
}

// TestServeControlRebindsStaleSocket: a leftover socket FILE with no listener
// (a crashed prior run) is not "live", so serveControl removes and rebinds it.
func TestServeControlRebindsStaleSocket(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "stratum.sock")
	// Leave an actual stale Unix socket as a crashed process would.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()
	s := &Supervisor{procs: map[string]*state{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := s.serveControl(ctx, path)
	if err != nil {
		t.Fatalf("serveControl over a stale socket file: %v", err)
	}
	stop()
}

func TestServeControlConcurrentRecovery(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "stratum.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const contenders = 16
	start := make(chan struct{})
	winners := make(chan func(), contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Go(func() {
			<-start
			s := &Supervisor{procs: map[string]*state{}}
			if stop, err := s.serveControl(ctx, path); err == nil {
				winners <- stop
			}
		})
	}
	close(start)
	wg.Wait()
	close(winners)
	count := len(winners)
	defer func() {
		for stop := range winners {
			stop()
		}
	}()
	if count != 1 {
		t.Fatalf("%d listeners acquired the same socket; want 1", count)
	}
	if !controlSocketLive(path) {
		t.Fatal("winning listener is unreachable")
	}
	// Uninstall must not allow a new listener while the old owner still lives.
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{procs: map[string]*state{}}
	if stop, err := s.serveControl(ctx, path); err == nil {
		stop()
		t.Fatal("runtime directory removal bypassed control ownership")
	}
}

// Older supervisors do not hold the coordination lock, so the socket probe must
// still refuse to replace an existing listener.
func TestServeControlRefusesLegacyListener(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "stratum.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Supervisor{procs: map[string]*state{}}
	if stop, err := s.serveControl(ctx, path); err == nil {
		stop()
		t.Fatal("replaced a legacy listener")
	}
	if !controlSocketLive(path) {
		t.Fatal("legacy listener is unreachable")
	}
	_ = l.Close()
	// The refused attempt must not leak the coordination lock.
	stop, err := s.serveControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

// TestControlSocketWireFormatIsReadableAcrossReleases pins the wire contract
// itself, not the Go types either side compiles: every request and reply
// crosses two releases during an in-place upgrade, so a renamed key or a
// changed framing breaks an older peer that this package's own structs would
// still read (Go's decoding matches field names case-insensitively). The fake
// listener decodes each request into a plain map and answers with literal JSON
// lines — a status reply captured from this release, and the refusal a
// supervisor that predates the reload op sends.
func TestControlSocketWireFormatIsReadableAcrossReleases(t *testing.T) {
	dir := shortSockDir(t)
	socket := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// One reply per accepted connection, in call order.
	replies := []string{
		`{"ok":true,"procs":[{"name":"kubelet","pid":7,"running":true,"wanted":true}]}`,
		`{"ok":false,"error":"unknown op \"reload\""}`,
	}
	var mu sync.Mutex
	var lines []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(conn)
			line, err := r.ReadBytes('\n')
			if err != nil {
				conn.Close()
				continue
			}
			mu.Lock()
			lines = append(lines, string(line))
			idx := i
			if idx >= len(replies) {
				idx = len(replies) - 1
			}
			mu.Unlock()
			if _, err := conn.Write([]byte(replies[idx] + "\n")); err != nil {
				t.Errorf("write the reply: %v", err)
			}
			// One request per connection: nothing else may ride it before the
			// caller, holding its reply, closes.
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if extra, _ := r.ReadBytes('\n'); len(extra) > 0 {
				t.Errorf("a second request rode one connection: %q", extra)
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	procs, err := DialContext(ctx, socket, "status", "")
	if err != nil {
		t.Fatalf("DialContext status: %v", err)
	}
	wantStatus := []ProcStatus{{Name: "kubelet", Pid: 7, Running: true, Wanted: true}}
	if !reflect.DeepEqual(procs, wantStatus) {
		t.Errorf("status decoded %v, want %v", procs, wantStatus)
	}
	spec := ProcSpec{Path: "/payload/bin/containerd", Args: []string{"--config", "/payload/config/containerd.toml"}, Env: []string{"PATH=/payload/bin"}}
	if err := DialReload(socket, "containerd", spec); !errors.Is(err, ErrReloadUnsupported) {
		t.Fatalf("reload against a supervisor that predates the op: %v, want ErrReloadUnsupported", err)
	}

	ln.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("%d requests crossed the socket, want one per call: %q", len(lines), lines)
	}
	for _, line := range lines {
		if strings.Count(line, "\n") != 1 {
			t.Errorf("request %q is not exactly one newline-terminated line", line)
		}
	}
	var statusReq map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &statusReq); err != nil {
		t.Fatalf("status request %q: %v", lines[0], err)
	}
	if want := map[string]any{"op": "status"}; !reflect.DeepEqual(statusReq, want) {
		t.Errorf("status request = %v, want exactly %v with lower-case keys", statusReq, want)
	}
	var reloadReq map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &reloadReq); err != nil {
		t.Fatalf("reload request %q: %v", lines[1], err)
	}
	wantReload := map[string]any{
		"op":   "reload",
		"name": "containerd",
		"process": map[string]any{
			"path": "/payload/bin/containerd",
			"args": []any{"--config", "/payload/config/containerd.toml"},
			"env":  []any{"PATH=/payload/bin"},
		},
	}
	if !reflect.DeepEqual(reloadReq, wantReload) {
		t.Errorf("reload request = %v, want exactly %v with lower-case keys", reloadReq, wantReload)
	}
}

// The client-side pin above cannot detect changes to what this release's
// server emits. Drive the real handler with released JSON, and inspect its
// reply without using the package's wire structs on the peer side.
func TestControlSocketServerKeepsReleasedWireFormat(t *testing.T) {
	for _, tc := range []struct {
		name, request, reply string
	}{
		{"status", `{"op":"status"}`, `{"ok":true,"procs":[{"name":"kubelet","pid":7,"running":true,"wanted":true,"foreign":true}]}`},
		{"reload", `{"op":"reload","name":"kubelet","process":{"path":"/payload/kubelet","args":["--config","/payload/kubelet.conf"],"env":["PATH=/payload"]}}`, `{"ok":true}`},
		{"unknown operation", `{"op":"future-op"}`, `{"ok":false,"error":"unknown op \"future-op\""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state{pid: 7, wanted: true, foreign: true, kick: make(chan struct{}, 1)}
			s := &Supervisor{procs: map[string]*state{"kubelet": st}}
			server, client := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); s.handle(server) }()
			t.Cleanup(func() { client.Close(); server.Close(); <-done })
			if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(client, tc.request+"\n"); err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(client)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(raw), "\n") || strings.Count(string(raw), "\n") != 1 {
				t.Fatalf("reply %q is not one newline-terminated JSON line", raw)
			}
			var got, want map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.reply), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("server reply = %s, want %s", raw, tc.reply)
			}
			<-done
			if tc.name == "reload" {
				if st.proc.Path != "/payload/kubelet" || !reflect.DeepEqual(st.proc.Args, []string{"--config", "/payload/kubelet.conf"}) || !reflect.DeepEqual(st.proc.Env, []string{"PATH=/payload"}) {
					t.Errorf("released reload request did not install its path, args and env: %+v", st.proc)
				}
				if len(st.kick) != 1 {
					t.Error("released reload request did not request a restart")
				}
			}
		})
	}
}
