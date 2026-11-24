package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// One JSON request per connection, one reply; internal/comps is the only intended client.
type request struct {
	Op      string    `json:"op"`
	Name    string    `json:"name,omitempty"`
	Process *ProcSpec `json:"process,omitempty"`
}

// ProcSpec is the wire form of a component's exec definition for a reload op.
type ProcSpec struct {
	Path string   `json:"path"`
	Args []string `json:"args"`
	Env  []string `json:"env,omitempty"`
}

type ProcStatus struct {
	Name    string `json:"name"`
	Pid     int    `json:"pid"`
	Running bool   `json:"running"`
	Wanted  bool   `json:"wanted"`
	// the socket is served by a process this supervisor does not own
	Foreign bool `json:"foreign,omitempty"`
}

type reply struct {
	OK    bool         `json:"ok"`
	Error string       `json:"error,omitempty"`
	Procs []ProcStatus `json:"procs,omitempty"`
}

func (s *Supervisor) serveControl(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// The lock sits outside the socket dir (uninstall removes it); never unlinked, its close frees it.
	lockPath := filepath.Dir(path) + "." + filepath.Base(path) + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("supervisor: control socket %s is already owned or cannot be locked: %w", path, err)
	}
	owned := false
	defer func() {
		if !owned {
			_ = lock.Close()
		}
	}()
	if err := refuseLegacySupervisor(); err != nil {
		return nil, err
	}
	if controlSocketLive(path) {
		return nil, fmt.Errorf("supervisor: control socket %s already has a live listener — another stratum supervisor is running on this node; refusing to start a second", path)
	}
	// A dead socket from a previous run would refuse the bind.
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	var once sync.Once
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			// UnixListener.Close unlinks its socket before releasing ownership.
			_ = l.Close()
			_ = lock.Close()
			close(done)
		})
	}
	owned = true
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return stop, nil
}

func controlSocketLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (s *Supervisor) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	var req request
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	if err := json.Unmarshal(line, &req); err != nil {
		writeReply(conn, reply{Error: "malformed request: " + err.Error()})
		return
	}

	switch req.Op {
	case "status":
		writeReply(conn, reply{OK: true, Procs: s.statuses()})
	case "restart", "stop", "start":
		if err := s.apply(req.Op, req.Name); err != nil {
			writeReply(conn, reply{Error: err.Error()})
			return
		}
		writeReply(conn, reply{OK: true})
	case "reload":
		if req.Process == nil {
			writeReply(conn, reply{Error: "reload requires a process spec"})
			return
		}
		if err := s.reload(req.Name, *req.Process); err != nil {
			writeReply(conn, reply{Error: err.Error()})
			return
		}
		writeReply(conn, reply{OK: true})
	case "reexec":
		if err := s.requestReExec(func() { writeReply(conn, reply{OK: true}) }); err != nil {
			writeReply(conn, reply{Error: err.Error()})
		}
	default:
		writeReply(conn, reply{Error: fmt.Sprintf("unknown op %q", req.Op)})
	}
}

func (s *Supervisor) statuses() []ProcStatus {
	var out []ProcStatus
	for name, st := range s.procs {
		v := st.get()
		out = append(out, ProcStatus{Name: name, Pid: v.pid, Running: v.pid != 0, Wanted: v.wanted, Foreign: v.foreign})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Supervisor) apply(op, name string) error {
	var targets []*state
	if name == "all" {
		for _, st := range s.procs {
			targets = append(targets, st)
		}
	} else {
		st, ok := s.procs[name]
		if !ok {
			return fmt.Errorf("no supervised component %q (have: %s)", name, strings.Join(s.knownNames(), " "))
		}
		targets = []*state{st}
	}

	for _, st := range targets {
		switch op {
		case "stop":
			stMu.Lock()
			st.wanted = false
			if st.cancelStart != nil {
				st.cancelStart()
			}
			cur := st.cur
			stMu.Unlock()
			if cur != nil && cur.Process != nil {
				_ = syscall.Kill(-cur.Process.Pid, syscall.SIGTERM)
			}
		case "start":
			stMu.Lock()
			st.wanted = true
			stMu.Unlock()
			select {
			case st.kick <- struct{}{}:
			default:
			}
		case "restart":
			st.restart()
		}
	}
	return nil
}

// Killing the child is the whole request; a deliberately stopped one is started instead.
func (st *state) restart() {
	stMu.Lock()
	st.wanted = true
	// Abort an in-flight start to re-run on current args; runOnce re-checks startCtx.Err() under stMu.
	if st.cancelStart != nil {
		st.cancelStart()
	}
	cur := st.cur
	stMu.Unlock()
	if cur == nil || cur.Process == nil {
		select {
		case st.kick <- struct{}{}:
		default:
		}
		return
	}
	pid := cur.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	// Escalate past a graceful drain that could outlast the requested restart.
	go func() {
		time.Sleep(30 * time.Second)
		stMu.Lock()
		same := st.cur != nil && st.cur.Process != nil && st.cur.Process.Pid == pid
		stMu.Unlock()
		if same {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

func (s *Supervisor) knownNames() []string {
	known := make([]string, 0, len(s.procs))
	for n := range s.procs {
		known = append(known, n)
	}
	sort.Strings(known)
	return known
}

// Reload replaces the exec definition and restarts; ArgsFn is cleared because args are explicit.
func (s *Supervisor) reload(name string, spec ProcSpec) error {
	st, ok := s.procs[name]
	if !ok {
		return fmt.Errorf("no supervised component %q (have: %s)", name, strings.Join(s.knownNames(), " "))
	}
	stMu.Lock()
	st.proc.Path = spec.Path
	st.proc.Args = spec.Args
	st.proc.Env = spec.Env
	st.proc.ArgsFn = nil
	stMu.Unlock()
	st.restart()
	return nil
}

func writeReply(conn net.Conn, r reply) {
	b, _ := json.Marshal(r)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

// Client side of the protocol; internal/comps wraps it with the systemctl-shaped API.
func Dial(socket, op, name string) ([]ProcStatus, error) {
	return DialContext(context.Background(), socket, op, name)
}

// Dial bounded by ctx: the connect, the write and the reply wait all end when ctx does.
func DialContext(ctx context.Context, socket, op, name string) ([]ProcStatus, error) {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()

	b, _ := json.Marshal(request{Op: op, Name: name})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, fmt.Errorf("supervisor: %s", r.Error)
	}
	return r.Procs, nil
}

func DialReload(socket, name string, spec ProcSpec) error {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	b, _ := json.Marshal(request{Op: "reload", Name: name, Process: &spec})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return err
	}
	if !r.OK {
		if r.Error == `unknown op "reload"` {
			return fmt.Errorf("%w: %s", ErrReloadUnsupported, r.Error)
		}
		return fmt.Errorf("supervisor: %s", r.Error)
	}
	return nil
}

// A supervisor that predates the request answers with ErrReExecUnsupported.
func DialReExec(socket string) error {
	_, err := Dial(socket, "reexec", "")
	if err != nil && err.Error() == `supervisor: unknown op "reexec"` {
		return fmt.Errorf("%w: %v", ErrReExecUnsupported, err)
	}
	return err
}

// Identifies a supervisor that predates the reexec request; other errors are not a fallback signal.
var ErrReExecUnsupported = errors.New("supervisor does not support reexec")

// Identifies supervisors that predate reload; other errors are not a compatibility fallback.
var ErrReloadUnsupported = errors.New("supervisor does not support reload")
