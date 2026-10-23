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

// The control protocol: one JSON request per connection, one JSON reply.
// Deliberately line-oriented and dependency-free — internal/comps is the only
// intended client, but `nc -U` works in a pinch.
//
//	{"op":"status"}                      -> {"ok":true,"procs":[{name,pid,running,wanted}]}
//	{"op":"restart","name":"etcd"}       -> {"ok":true}
//	{"op":"stop","name":"etcd"}          -> {"ok":true}   (stays down until start)
//	{"op":"start","name":"etcd"}         -> {"ok":true}
//	{"op":"restart","name":"all"}        -> every supervised component
//	{"op":"reload","name":"containerd","process":{path,args,env}} -> update argv + restart
//	{"op":"reexec"}                      -> {"ok":true}, then every component stops
//	                                        and the supervisor execs Options.ReExec
type request struct {
	Op      string    `json:"op"`
	Name    string    `json:"name,omitempty"`
	Process *ProcSpec `json:"process,omitempty"`
}

// ProcSpec is the wire form of a component's exec definition, sent with a
// "reload" op. It carries only the marshalable fields of Process: an in-place
// `stratum upgrade` runs as the NEW binary and uses it to hand the running
// (old-binary) supervisor a component's new argv/config path, so the change
// applies without a full restart.
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
	// Foreign: the component's socket is served by a process the supervisor
	// does not own, so Running (if set) is not this component.
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
	// Keep the coordination inode outside the socket directory: uninstall removes
	// that directory while a supervisor may still be unwinding. Never unlink this
	// lock file; closing the descriptor (or exiting) releases the kernel lock.
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
	// Probe legacy supervisors that do not take this lock before recovering a
	// stale socket. The lock serializes recovery and binding by current callers.
	if controlSocketLive(path) {
		return nil, fmt.Errorf("supervisor: control socket %s already has a live listener — another stratum supervisor is running on this node; refusing to start a second", path)
	}
	// A dead socket from a previous run would refuse the bind.
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil { // root-only control
		l.Close()
		return nil, err
	}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return // listener closed
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

// controlSocketLive reports whether the control socket at path has a listener
// answering right now — a stale socket file left by a crash refuses the
// connection, so this distinguishes a running supervisor from a leftover file.
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

// restart bounces the component: the supervise loop brings it back up. Killing
// the child is the whole request; a deliberately-stopped one is started instead.
func (st *state) restart() {
	stMu.Lock()
	st.wanted = true
	// Abort an in-flight start so it re-runs with the current st.proc. Without
	// this, a reload that lands after runOnce snapshotted the OLD proc but before
	// it published st.cur would see cur==nil, only kick, and let the old argv
	// start and run until it exits — the exact stale-args window reload closes.
	// The start section re-checks startCtx.Err() under stMu, so this is atomic
	// with cmd.Start (the same contract the stop op relies on).
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
	// Escalate: a graceful-shutdown drain (the apiserver's can run to
	// --request-timeout) must not stall a requested restart.
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

// knownNames lists the supervised component names, sorted, for error messages.
func (s *Supervisor) knownNames() []string {
	known := make([]string, 0, len(s.procs))
	for n := range s.procs {
		known = append(known, n)
	}
	sort.Strings(known)
	return known
}

// reload replaces a component's exec definition (path/args/env) with the one the
// caller computed from the NEW binary, then restarts it — so an in-place upgrade
// applies changed argv/config paths that the supervisor's stored (old-binary)
// Process would otherwise keep across a plain restart. Any ArgsFn
// re-render hook is cleared: the caller supplied explicit args.
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

// Dial sends one request to a control socket and returns the reply. Client
// side of the protocol; internal/comps wraps it with the systemctl-shaped API.
func Dial(socket, op, name string) ([]ProcStatus, error) {
	return DialContext(context.Background(), socket, op, name)
}

// DialContext is Dial bounded by ctx: the connect, the write and the wait for
// the reply all end when ctx does (or at Dial's own limits, whichever is
// sooner), so a caller on a deadline is not held for the 30s a hung supervisor
// would otherwise take.
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

// DialReload sends a "reload" op: it updates the named component's exec
// definition in the running supervisor and restarts it. Separate from
// Dial because it carries a process payload.
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

// DialReExec asks the supervisor to stop every component and replace itself
// with its configured re-exec command. A supervisor that predates the request
// answers with ErrReExecUnsupported, and nothing changes.
func DialReExec(socket string) error {
	_, err := Dial(socket, "reexec", "")
	if err != nil && err.Error() == `supervisor: unknown op "reexec"` {
		return fmt.Errorf("%w: %v", ErrReExecUnsupported, err)
	}
	return err
}

// ErrReExecUnsupported identifies a supervisor that predates the reexec
// request, from the exact response such a supervisor gives, so the caller
// must restart it some other way. Other rejection and transport errors are
// not a reason to fall back.
var ErrReExecUnsupported = errors.New("supervisor does not support reexec")

// ErrReloadUnsupported identifies the exact response from supervisors that
// predate reload. Other rejection, transport and recovery errors are not a
// compatibility fallback.
var ErrReloadUnsupported = errors.New("supervisor does not support reload")
