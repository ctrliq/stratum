package nodelock

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

const holdLockEnv = "STRATUM_TEST_HOLD_NODE_LOCK"

const holdLockOpEnv = "STRATUM_TEST_HOLD_NODE_LOCK_OP"

// Helper process: holds the node lock as a second process, until killed or the parent dies.
func TestHelperHoldNodeLock(t *testing.T) {
	path := os.Getenv(holdLockEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	version.NodeUpgradeLockPath = path
	op := os.Getenv(holdLockOpEnv)
	if op == "" {
		op = "upgrade"
	}
	release, err := Acquire(op)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	// Keeps the *os.File reachable, or its finalizer would close the fd and drop the lock.
	defer release()
	fmt.Println("held")
	// Not select {}: a parked cgo-free binary hits the deadlock detector; a syscall counts as running.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func holdNodeLockInChild(t *testing.T, op string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldNodeLock$")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+version.NodeUpgradeLockPath, holdLockOpEnv+"="+op)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// cmd keeps the write end open until Wait, so the holder holds until killed or the parent dies.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("lock holder did not take the lock: %q", line)
	}
	return cmd
}

const holdRawFlockEnv = "STRATUM_TEST_HOLD_RAW_NODE_LOCK"

// Helper process: holds a raw flock, pid only, like a released binary, until killed or parent dies.
func TestHelperHoldRawNodeLock(t *testing.T) {
	path := os.Getenv(holdRawFlockEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }()
	fmt.Println("held")
	// Not select {}: a parked cgo-free binary hits the deadlock detector; a syscall counts as running.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func holdRawFlockInChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldRawNodeLock$")
	cmd.Env = append(os.Environ(), holdRawFlockEnv+"="+version.NodeUpgradeLockPath)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// cmd keeps the write end open until Wait, so the holder holds until killed or the parent dies.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("raw lock holder did not take the lock: %q", line)
	}
	return cmd
}
