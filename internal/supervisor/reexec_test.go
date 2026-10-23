package supervisor

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runSleeper runs a supervisor with one long-running component and returns its
// control socket, the component's pid, and Run's result channel.
func runSleeper(t *testing.T, ctx context.Context, reexec []string) (string, int, chan error) {
	t.Helper()
	sock := filepath.Join(shortSockDir(t), "s.sock")
	procs := []Process{{Name: "sleeper", Path: "/bin/sleep", Args: []string{"60"}}}
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, procs, Options{LogDir: t.TempDir(), ControlSocket: sock, ReExec: reexec})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := Dial(sock, "status", ""); err == nil && len(st) == 1 && st[0].Pid != 0 {
			return sock, st[0].Pid, errc
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the supervised component never started")
	return "", 0, nil
}

func executable(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stratum")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReExecStopsEveryComponentFirst: the supervisor execs its re-exec command
// only once every component has exited, so the new process does not start
// over components the old one still runs.
func TestReExecStopsEveryComponentFirst(t *testing.T) {
	target := executable(t, 0o755)
	argv := []string{target, "up", "--worker"}
	var gotArgv []string
	childAlive := true
	var pid int
	saved := execProcess
	t.Cleanup(func() { execProcess = saved })
	execProcess = func(path string, a []string, env []string) error {
		gotArgv = append([]string{path}, a...)
		childAlive = syscall.Kill(pid, 0) == nil
		return errors.New("stub exec")
	}

	sock, p, errc := runSleeper(t, context.Background(), argv)
	pid = p
	if err := DialReExec(sock); err != nil {
		t.Fatalf("reexec: %v", err)
	}
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "stub exec") {
			t.Fatalf("Run = %v, want the exec's error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor never re-exec'd")
	}
	if want := append([]string{target}, argv...); !slices.Equal(gotArgv, want) {
		t.Errorf("exec(%q), want exec(%q)", gotArgv, want)
	}
	if childAlive {
		t.Errorf("component %d was still running when the supervisor re-exec'd", pid)
	}
}

// TestReExecRefusedLeavesComponentsRunning: a request the supervisor cannot
// carry out is refused before anything stops.
func TestReExecRefusedLeavesComponentsRunning(t *testing.T) {
	saved := execProcess
	t.Cleanup(func() { execProcess = saved })
	execProcess = func(string, []string, []string) error {
		t.Error("exec'd on a refused request")
		return errors.New("stub exec")
	}
	for name, reexec := range map[string][]string{
		"not configured":    nil,
		"not an executable": {executable(t, 0o644), "up"},
		"missing":           {filepath.Join(t.TempDir(), "gone"), "up"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			sock, pid, errc := runSleeper(t, ctx, reexec)
			err := DialReExec(sock)
			if err == nil || errors.Is(err, ErrReExecUnsupported) {
				t.Errorf("reexec = %v, want a refusal that is not ErrReExecUnsupported", err)
			}
			time.Sleep(200 * time.Millisecond)
			if syscall.Kill(pid, 0) != nil {
				t.Errorf("component %d stopped after a refused re-exec", pid)
			}
			cancel()
			<-errc
		})
	}
}

// TestDialReExecRecognisesAnOlderSupervisor: a supervisor that predates the
// request answers "unknown op"; the caller must be able to tell that apart.
func TestDialReExecRecognisesAnOlderSupervisor(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "old.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = bufio.NewReader(c).ReadBytes('\n')
		_, _ = c.Write([]byte(`{"ok":false,"error":"unknown op \"reexec\""}` + "\n"))
	}()
	if err := DialReExec(sock); !errors.Is(err, ErrReExecUnsupported) {
		t.Fatalf("err = %v, want ErrReExecUnsupported", err)
	}
}

// TestReExecAcceptsBeforeStopping: the request is acknowledged before any
// component stops, so a supervisor with nothing left to wait for cannot exec,
// and close the requester's connection, before the reply is written.
func TestReExecAcceptsBeforeStopping(t *testing.T) {
	accepted := false
	s := &Supervisor{
		reexecArgv: []string{executable(t, 0o755), "up"},
		stopAll: func() {
			if !accepted {
				t.Error("components were stopped before the request was acknowledged")
			}
		},
	}
	if err := s.requestReExec(func() { accepted = true }); err != nil {
		t.Fatal(err)
	}
	if !s.reexecRequested.Load() || !accepted {
		t.Errorf("requested=%v accepted=%v, want both", s.reexecRequested.Load(), accepted)
	}
}

// TestReExecNotDoneWhileShuttingDown: a supervisor told to shut down while a
// re-exec is pending stops, and does not start a new stratum up.
func TestReExecNotDoneWhileShuttingDown(t *testing.T) {
	saved := execProcess
	t.Cleanup(func() { execProcess = saved })
	execProcess = func(string, []string, []string) error {
		t.Error("re-exec'd a supervisor that was shutting down")
		return errors.New("stub exec")
	}
	sock := filepath.Join(shortSockDir(t), "s.sock")
	procs := []Process{{Name: "stubborn", Path: "/bin/sh", Args: []string{"-c", `trap "" TERM; sleep 60`}}}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, procs, Options{LogDir: t.TempDir(), ControlSocket: sock, ReExec: []string{executable(t, 0o755), "up"}})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, err := Dial(sock, "status", ""); err == nil && len(st) == 1 && st[0].Pid != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervised component never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := DialReExec(sock); err != nil {
		t.Fatalf("reexec: %v", err)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor never stopped")
	}
}
