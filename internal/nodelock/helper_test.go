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

// holdLockEnv makes the test binary a node-lock holder instead of a test run:
// see TestHelperHoldNodeLock.
const holdLockEnv = "STRATUM_TEST_HOLD_NODE_LOCK"

// holdLockOpEnv names the operation the helper holds the lock for. It exists so
// a test can hold the lock under one operation and refuse another; the default
// is "upgrade", the operation every released binary takes the lock for.
const holdLockOpEnv = "STRATUM_TEST_HOLD_NODE_LOCK_OP"

// TestHelperHoldNodeLock is not a test. Re-executed with holdLockEnv set to a
// lock path, it takes the node maintenance lock there, says so, and holds it
// until killed or its parent dies — a second process on the same node, which
// is what two maintenance operations running on one host are.
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
	// Deferred, so the closure — and the *os.File whose finalizer would
	// otherwise close the descriptor and drop the lock — stays reachable for
	// as long as this process holds it.
	defer release()
	fmt.Println("held")
	// Held until the parent closes stdin or dies, not parked on select {}:
	// with every goroutine parked, a test binary without cgo — what macOS
	// builds by default — dies of the runtime's deadlock detector milliseconds after
	// saying "held", and the kernel drops the lock with it. A blocked read
	// keeps a thread in a syscall, which that detector counts as running.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// holdNodeLockInChild starts a separate process holding the node maintenance
// lock at the redirected path for op, and returns it once the lock is held.
func holdNodeLockInChild(t *testing.T, op string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldNodeLock$")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+version.NodeUpgradeLockPath, holdLockOpEnv+"="+op)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The holder holds until it is killed, or until this process dies and
	// the pipe closes; cmd keeps the write end open until its Wait.
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

// holdRawFlockEnv makes the test binary a raw lock holder instead of a test
// run: see TestHelperHoldRawNodeLock.
const holdRawFlockEnv = "STRATUM_TEST_HOLD_RAW_NODE_LOCK"

// TestHelperHoldRawNodeLock is not a test. Re-executed with holdRawFlockEnv set
// to a lock path, it takes a raw syscall.Flock there, records only its pid the
// way a released binary's lockNode does, and holds until killed or its parent
// dies — the legacy pid-only holder every released binary leaves behind.
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
	// Held until the parent closes stdin or dies, not parked on select {}:
	// with every goroutine parked, a test binary without cgo — what macOS
	// builds by default — dies of the runtime's deadlock detector milliseconds after
	// saying "held", and the kernel drops the lock with it. A blocked read
	// keeps a thread in a syscall, which that detector counts as running.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// holdRawFlockInChild starts a separate process holding a raw flock at the
// redirected node-lock path, leaving a pid-only file, and returns it once the
// lock is held.
func holdRawFlockInChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldRawNodeLock$")
	cmd.Env = append(os.Environ(), holdRawFlockEnv+"="+version.NodeUpgradeLockPath)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The holder holds until it is killed, or until this process dies and
	// the pipe closes; cmd keeps the write end open until its Wait.
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
