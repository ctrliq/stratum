// Package supervisor runs and restarts the host processes that make up a SKE node.
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

type Process struct {
	Name string
	Path string
	Args []string
	// Evaluated on each (re)start instead of Args; etcd membership may change between starts.
	ArgsFn func() []string
	Env    []string
	// Gates each start; without it apiserver clients crash-loop while the apiserver bootstraps RBAC.
	WaitFor func(context.Context) error
	// If the component exits while this socket is live, an orphan owns it: report Foreign.
	Socket string
	// SELinux context to exec in, instead of the one inherited from this process.
	ExecLabel string
}

// Options tunes restart behaviour.
type Options struct {
	LogDir     string
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Serves the control protocol internal/comps speaks.
	ControlSocket string
	// Called after each successful start.
	OnStart func(name string, pid int)
	// Command a "reexec" request replaces this process with; without it the request is refused.
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

// Starts every process and supervises them until ctx is cancelled; exits restart with backoff.
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

// Indirected so a test can stub the re-exec without replacing the test binary.
var execProcess = syscall.Exec

// Supervisor tracks the running components for the control socket.
type Supervisor struct {
	mu              sync.Mutex
	procs           map[string]*state
	reexecArgv      []string
	stopAll         context.CancelFunc
	reexecRequested atomic.Bool
}

// Validates the re-exec target and acknowledges the caller before anything stops.
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

// Staying up this long clears the finding without proving the child owns the socket; var for tests.
var foreignSettle = 5 * time.Second

func supervise(ctx context.Context, st *state, opts Options) {
	backoff := opts.MinBackoff
	for ctx.Err() == nil {
		if !st.stillWanted() {
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
		// A long-lived process that died is a fresh failure; reset the backoff.
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

// On restart, a socket the exited child should own but that is still live belongs to an orphan.
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

// Uninstall would delete node state; the remedy is stopping the orphan's owner only.
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
	// A value copy: reload swaps slice headers, never backing arrays, so p is safe outside the lock.
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
	// Serialize start and PID publication with stop; a stopped attempt must not start.
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

var ErrNotSupervised = errors.New("not a supervised component")

// The calling thread's SELinux exec context; a var so a test can force a failure.
var threadExecAttr = "/proc/thread-self/attr/exec"

// The exec context is per thread: set it on a locked thread that exits with the goroutine.
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
