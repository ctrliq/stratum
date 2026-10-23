// Package supervisor runs and restarts the host processes that make up a SKE
// node: containerd, kubelet, and the operator. (The control plane itself is
// kubelet-managed static pods, owned by internal/staticpod.)
// Start, restart with backoff, stop everything on shutdown — plus a control
// socket (/run/stratum/stratum.sock) so separate invocations (`stratum certs
// rotate`, HA join) can restart or stop individual components the way
// systemctl does for systemd units.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Process is one supervised component.
type Process struct {
	Name string
	Path string
	Args []string
	// ArgsFn, when set, is evaluated on every (re)start instead of Args — for
	// components whose flags must re-render after external surgery (etcd
	// membership after `stratum snapshot rejoin`).
	ArgsFn func() []string
	Env    []string
	// WaitFor gates each start. Without it, every apiserver client crash-loops
	// through the seconds before the apiserver has bootstrapped its RBAC — the
	// supervisor recovers, but a control plane that always logs a crash on boot
	// trains people to ignore crash logs.
	WaitFor func(context.Context) error
	// Socket, when set, is the unix socket this component serves. If the
	// component exits while that socket is still live, someone else owns it
	// (an orphan from an earlier run): the component is reported Foreign
	// instead of silently retrying forever.
	Socket string
	// ExecLabel, when set, is the SELinux context the component is exec'd in,
	// rather than the one it would inherit from this process.
	ExecLabel string
}

// Options tunes restart behaviour.
type Options struct {
	LogDir     string
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// ControlSocket, when set, serves the restart/stop/start/status protocol
	// that internal/comps speaks (the supervisor's systemctl equivalent).
	ControlSocket string
	// OnStart is called after each successful start, with the pid.
	OnStart func(name string, pid int)
	// ReExec, when set, is the command line a "reexec" request replaces this
	// process with once every component has stopped; ReExec[0] is the binary.
	// Without it the request is refused.
	ReExec []string
}

func (o *Options) defaults() {
	if o.MinBackoff == 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 30 * time.Second
	}
}

// Run starts every process and supervises them until ctx is cancelled.
//
// A component that exits is restarted with exponential backoff: a control plane
// where the scheduler died and stayed dead is worse than one that is visibly
// flapping, because only the second is obvious.
func Run(ctx context.Context, procs []Process, opts Options) error {
	opts.defaults()
	if opts.LogDir != "" {
		if err := os.MkdirAll(opts.LogDir, 0o755); err != nil {
			return err
		}
	}

	runCtx, stopAll := context.WithCancel(ctx)
	defer stopAll()
	sup := &Supervisor{procs: map[string]*state{}, reexecArgv: opts.ReExec, stopAll: stopAll}
	for _, p := range procs {
		sup.procs[p.Name] = &state{proc: p, wanted: true, kick: make(chan struct{}, 1)}
	}
	stopControl := func() {}
	if opts.ControlSocket != "" {
		stop, err := sup.serveControl(runCtx, opts.ControlSocket)
		if err != nil {
			return err
		}
		stopControl = stop
		defer stop()
	}

	var wg sync.WaitGroup
	for _, st := range sup.procs {
		wg.Add(1)
		go func(st *state) {
			defer wg.Done()
			supervise(runCtx, st, opts)
		}(st)
	}
	wg.Wait()
	if sup.reexecRequested.Load() && ctx.Err() == nil {
		stopControl()
		argv := sup.reexecArgv
		fmt.Fprintf(os.Stderr, "supervisor: every component stopped; running %s\n", strings.Join(argv, " "))
		if err := execProcess(argv[0], argv, os.Environ()); err != nil {
			return fmt.Errorf("supervisor: re-exec %s after stopping every component: %w", argv[0], err)
		}
	}
	return ctx.Err()
}

// execProcess is syscall.Exec, indirected so a test can see the re-exec
// without replacing the test binary.
var execProcess = syscall.Exec

// Supervisor tracks the running components for the control socket.
type Supervisor struct {
	mu              sync.Mutex
	procs           map[string]*state
	reexecArgv      []string
	stopAll         context.CancelFunc
	reexecRequested atomic.Bool
}

// requestReExec checks that the re-exec target can be run, calls accepted,
// then stops every component: Run execs it once they have all exited. Checked
// first, because a node whose components were stopped for a re-exec that then
// cannot happen is left down. accepted runs before anything stops, so the
// caller hears the request was taken before the exec can close its connection.
func (s *Supervisor) requestReExec(accepted func()) error {
	if len(s.reexecArgv) == 0 {
		return errors.New("re-exec is not configured for this supervisor")
	}
	fi, err := os.Stat(s.reexecArgv[0])
	if err != nil {
		return fmt.Errorf("re-exec target: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("re-exec target %s is not an executable file", s.reexecArgv[0])
	}
	accepted()
	fmt.Fprintf(os.Stderr, "supervisor: re-exec requested; stopping every component, then running %s\n",
		strings.Join(s.reexecArgv, " "))
	s.reexecRequested.Store(true)
	s.stopAll()
	return nil
}

type state struct {
	proc        Process
	wanted      bool // false = deliberately stopped via the socket
	pid         int
	kick        chan struct{} // wakes a stopped supervise loop
	cur         *exec.Cmd
	cancelStart context.CancelFunc // cancels this attempt's dependency gate
	foreign     bool               // last exit left Process.Socket served by another process
}

// foreignSettle is how long a restarted component must stay up before a
// foreign-socket finding is cleared. A heuristic: it does not prove the child
// holds the socket, only that it did not exit the way it does when another
// process owns it (within a second or so). A foreign owner that outlives the
// child's start would be missed until the next exit re-checks the socket. A var
// so tests can shorten it.
var foreignSettle = 5 * time.Second

func supervise(ctx context.Context, st *state, opts Options) {
	backoff := opts.MinBackoff
	for ctx.Err() == nil {
		if !st.stillWanted() {
			// Deliberately stopped via the control socket: wait for a start.
			select {
			case <-ctx.Done():
				return
			case <-st.kick:
				continue
			}
		}
		start := time.Now()
		err := runOnce(ctx, st, opts)
		if ctx.Err() != nil {
			return
		}
		if !st.stillWanted() {
			continue // the exit was a socket-requested stop, not a crash
		}
		// A process that stayed up a while and then died is a fresh failure,
		// not part of a crash loop; reset its backoff.
		if time.Since(start) > 30*time.Second {
			backoff = opts.MinBackoff
		}
		fmt.Fprintf(os.Stderr, "supervisor: %s exited (%v), restarting in %v\n", st.proc.Name, err, backoff)
		noteForeignSocket(st)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > opts.MaxBackoff {
			backoff = opts.MaxBackoff
		}
	}
}

// noteForeignSocket is the live-socket check, applied on a restart:
// the child just exited, so a socket it should own that still has a listener
// belongs to another process. Say so once, loudly, and flag the component so
// the node is not reported healthy; the retry loop carries on and clears the
// flag once a child holds the socket itself.
func noteForeignSocket(st *state) {
	stMu.Lock()
	sock, name, was := st.proc.Socket, st.proc.Name, st.foreign
	stMu.Unlock()
	if sock == "" {
		return
	}
	live := controlSocketLive(sock)
	stMu.Lock()
	st.foreign = live
	stMu.Unlock()
	if live && !was {
		fmt.Fprintln(os.Stderr, foreignSocketMessage(name, sock))
	}
}

// foreignSocketMessage is the one loud line. The remedy is to stop only the
// process owning the socket: `stratum uninstall` would also delete node state
// (etcd data, PKI, binaries), which is no recovery from an orphan.
func foreignSocketMessage(name, sock string) string {
	return fmt.Sprintf("supervisor: %s exited but %s is still served by another process — an orphan from an earlier run. "+
		"This node is NOT healthy: the %s serving it is not the one stratum installed. "+
		"Find the process that owns %s and stop only that process; the supervisor will then start %s",
		name, sock, name, sock, name)
}

func (st *state) stillWanted() bool { return st.get().wanted }

func (st *state) get() state { stMu.Lock(); defer stMu.Unlock(); return *st }

var stMu sync.Mutex

func runOnce(ctx context.Context, st *state, opts Options) error {
	startCtx, cancel := context.WithCancel(ctx)
	stMu.Lock()
	if !st.wanted {
		stMu.Unlock()
		cancel()
		return context.Canceled
	}
	st.cancelStart = cancel
	// Snapshot the exec definition under stMu: reload rewrites st.proc.Path/Args/
	// Env under the same lock (control.go), so reading it here unlocked would be a
	// data race. p is a value copy and reload swaps the slice headers rather than
	// mutating backing arrays, so using p.Args/Env outside the lock below is safe.
	p := st.proc
	stMu.Unlock()
	defer func() {
		cancel()
		stMu.Lock()
		st.cancelStart = nil
		stMu.Unlock()
	}()
	if p.WaitFor != nil {
		if err := p.WaitFor(startCtx); err != nil {
			return fmt.Errorf("waiting for dependency: %w", err)
		}
	}
	args := p.Args
	if p.ArgsFn != nil {
		args = p.ArgsFn()
	}
	cmd := exec.Command(p.Path, args...)
	cmd.Env = append(os.Environ(), p.Env...)
	// Own process group, so a stop signals the component's children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Serialize the final permission check, filesystem setup, start and PID
	// publication with stop. A stopped attempt cannot start after acknowledgement,
	// even if its dependency gate ignores cancellation or a later start is issued.
	stMu.Lock()
	if !st.wanted || startCtx.Err() != nil {
		stMu.Unlock()
		return context.Canceled
	}

	if opts.LogDir != "" {
		f, err := os.OpenFile(filepath.Join(opts.LogDir, p.Name+".log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			stMu.Unlock()
			return err
		}
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}

	if err := startLabelled(cmd, p.ExecLabel); err != nil {
		stMu.Unlock()
		return fmt.Errorf("start: %w", err)
	}
	st.pid, st.cur = cmd.Process.Pid, cmd
	stMu.Unlock()
	settle := time.AfterFunc(foreignSettle, func() {
		stMu.Lock()
		if st.cur == cmd {
			st.foreign = false
		}
		stMu.Unlock()
	})
	defer settle.Stop()
	if opts.OnStart != nil {
		opts.OnStart(p.Name, cmd.Process.Pid)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	defer func() {
		stMu.Lock()
		st.pid, st.cur = 0, nil
		stMu.Unlock()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		stop(cmd)
		<-done
		return ctx.Err()
	}
}

// stop asks the process group to terminate, then kills it if it will not.
func stop(cmd *exec.Cmd) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}
}

// ErrNotSupervised is returned when a caller asks about an unknown component.
var ErrNotSupervised = errors.New("not a supervised component")

// threadExecAttr is the calling thread's SELinux exec context; a var so a test
// can make setting it fail.
var threadExecAttr = "/proc/thread-self/attr/exec"

// startLabelled starts cmd in the SELinux context label. The exec context is
// per thread, so it is set on a locked thread that is never unlocked and so
// exits with its goroutine, taking the context with it. A context the kernel
// refuses leaves the component starting as it would have, with a warning.
func startLabelled(cmd *exec.Cmd, label string) error {
	if label == "" {
		return cmd.Start()
	}
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := os.WriteFile(threadExecAttr, []byte(label), 0); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: WARN: %s keeps this process's SELinux context; setting %s: %v\n",
				filepath.Base(cmd.Path), label, err)
		}
		errc <- cmd.Start()
	}()
	return <-errc
}
