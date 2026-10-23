package upgrade

import (
	"io"

	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// holdLockEnv makes the test binary a lock holder instead of a test run: see
// TestHelperHoldNodeLock.
const holdLockEnv = "STRATUM_TEST_HOLD_NODE_LOCK"

// TestHelperHoldNodeLock is not a test. Re-executed with holdLockEnv set to a
// lock path, it takes the node lock there, says so, and holds it until killed
// or its parent dies — a second process on the same node, which is what two
// upgrade Jobs are.
func TestHelperHoldNodeLock(t *testing.T) {
	path := os.Getenv(holdLockEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	version.NodeUpgradeLockPath = path
	release, err := lockNode("upgrade")
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

// holdNodeLockInChild starts a separate process holding the node lock at the
// redirected path and returns it once the lock is held.
func holdNodeLockInChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldNodeLock$")
	cmd.Env = append(os.Environ(), holdLockEnv+"="+version.NodeUpgradeLockPath)
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
// to a lock path, it takes a raw syscall.Flock there, records its pid the way
// lockNode does, and holds until killed or its parent dies — what a released
// binary's lockNode leaves behind.
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
// redirected node-lock path and returns it once the lock is held.
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

// TestNodeLockExcludesAHolderOfTheReleasedLockFile is the cross-version
// contract: every released binary fences at version.RunDir + "-upgrade.lock"
// with a flock, so a node mid-upgrade refuses the next binary's upgrade,
// --check and rollback too. The holder here takes a raw flock, the way a
// released binary's lockNode does, rather than running this build's lockNode —
// both sides of the fence must not move together. A new path or a new kind of
// lock needs a compatibility release first.
func TestNodeLockExcludesAHolderOfTheReleasedLockFile(t *testing.T) {
	if version.NodeUpgradeLockPath != version.RunDir+"-upgrade.lock" {
		t.Fatalf("node lock path = %q, want %q: every released binary opens that path",
			version.NodeUpgradeLockPath, version.RunDir+"-upgrade.lock")
	}
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, version.K8sVersion)
	installedBuild(t, "rc.3", "rc.3")
	redirect(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	holder := holdRawFlockInChild(t)

	for cmd, run := range map[string]func() error{
		"upgrade": func() error { return UpgradeNode(io.Discard, NodeOptions{}) },
		"check": func() error {
			_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{Check: true}) })
			return err
		},
		"rollback": func() error { return RollbackNode(io.Discard, RollbackOptions{}) },
	} {
		err := run()
		if err == nil {
			t.Fatalf("%s: expected a refusal while a released binary's lock holder holds the node", cmd)
		}
		for _, want := range []string{
			fmt.Sprintf("pid %d", holder.Process.Pid),
			"already running on this node",
			"nothing was changed",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s error missing %q: %v", cmd, want, err)
			}
		}
	}
}

// TestNodeLockRefusesASecondRun is the node-lock fence: while one process
// upgrades a node, a second upgrade or rollback on that node — whatever
// created it — refuses, names the holder, and changes nothing.
func TestNodeLockRefusesASecondRun(t *testing.T) {
	backupFixture(t)
	redirect(t, &version.K8sConfDir, t.TempDir()) // no node.yaml there
	nodeUp(t)
	holder := holdNodeLockInChild(t)

	for cmd, run := range map[string]func() error{
		"upgrade":  func() error { return UpgradeNode(io.Discard, NodeOptions{}) },
		"rollback": func() error { return RollbackNode(io.Discard, RollbackOptions{}) },
	} {
		err := run()
		if err == nil {
			t.Fatalf("%s: expected a refusal while another run holds the node", cmd)
		}
		for _, want := range []string{
			"stratum " + cmd + ":",
			"already running on this node",
			fmt.Sprintf("pid %d", holder.Process.Pid),
			"nothing was changed",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s error missing %q: %v", cmd, want, err)
			}
		}
	}
}

// TestNodeLockDiesWithItsHolder: a holder that is killed — the OOM-killed or
// deadline-killed Job — does not leave the node locked for the next run.
func TestNodeLockDiesWithItsHolder(t *testing.T) {
	backupFixture(t)
	redirect(t, &version.K8sConfDir, t.TempDir())
	nodeUp(t)
	holder := holdNodeLockInChild(t)
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	// Past the lock to the next step, the node-state load.
	if err := UpgradeNode(io.Discard, NodeOptions{}); err == nil || !strings.Contains(err.Error(), "node state") {
		t.Errorf("upgrade after the holder died should get past the lock, got: %v", err)
	}
}

// TestNodeLockReleases: a finished run hands the node on. Two runs in a row
// must both get past the lock, which a release that did nothing would fail.
func TestNodeLockReleases(t *testing.T) {
	backupFixture(t)
	redirect(t, &version.K8sConfDir, t.TempDir())
	nodeUp(t)
	for i := 0; i < 2; i++ {
		if err := UpgradeNode(io.Discard, NodeOptions{}); err == nil || !strings.Contains(err.Error(), "node state") {
			t.Fatalf("run %d should get past the lock, got: %v", i+1, err)
		}
	}
}

// TestNodeLockExactlyOneWinner: runs racing for one node leave exactly one
// holding it and every other refusing.
func TestNodeLockExactlyOneWinner(t *testing.T) {
	redirect(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	const n = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		releases []func()
		refused  int
		start    = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := lockNode("upgrade")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !strings.Contains(err.Error(), "already running on this node") {
					t.Errorf("unexpected lock error: %v", err)
				}
				refused++
				return
			}
			releases = append(releases, release)
		}()
	}
	close(start)
	wg.Wait()
	for _, r := range releases {
		r()
	}
	if len(releases) != 1 || refused != n-1 {
		t.Errorf("got %d holders and %d refusals, want 1 and %d", len(releases), refused, n-1)
	}
}

// TestNodeLockSkippedByDryRun: a preview writes nothing, so it must not be
// blocked by — or block — a real run.
func TestNodeLockSkippedByDryRun(t *testing.T) {
	backupFixture(t)
	probeFixture(t, false, nil)
	confDir := t.TempDir()
	redirect(t, &version.K8sConfDir, confDir)
	if err := os.WriteFile(filepath.Join(confDir, "node.yaml"), []byte("role: worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	holdNodeLockInChild(t)

	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	old := os.Stdout
	os.Stdout = devnull
	err := UpgradeNode(io.Discard, NodeOptions{DryRun: true})
	os.Stdout = old
	if err != nil {
		t.Errorf("dry-run must not take the node lock: %v", err)
	}
}
