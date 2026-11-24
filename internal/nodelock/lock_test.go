package nodelock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func redirect(t *testing.T, v *string, value string) {
	t.Helper()
	old := *v
	*v = value
	t.Cleanup(func() { *v = old })
}

// Redirects the node lock to a temp file so tests never touch /run.
func lockPathFor(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upgrade.lock")
	redirect(t, &version.NodeUpgradeLockPath, path)
	return path
}

func TestAcquirePathIsTheReleasedLockPath(t *testing.T) {
	if version.NodeUpgradeLockPath != version.RunDir+"-upgrade.lock" {
		t.Errorf("node lock path = %q, want %q: every released binary opens that path",
			version.NodeUpgradeLockPath, version.RunDir+"-upgrade.lock")
	}
	if version.NodeUpgradeLockPath != "/run/stratum-upgrade.lock" {
		t.Errorf("node lock path = %q, want %q: the path released binaries hold",
			version.NodeUpgradeLockPath, "/run/stratum-upgrade.lock")
	}
}

func TestAcquireHoldsTheRedirectedPath(t *testing.T) {
	path := lockPathFor(t)
	release, err := Acquire("upgrade")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	probe, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		t.Errorf("a second flock on %s succeeded: Acquire is not holding that file", path)
	}
}

func TestAcquireWritesOperationAndPid(t *testing.T) {
	path := lockPathFor(t)
	release, err := Acquire("upgrade")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	assertHoldsOperationAndPid(t, path, "upgrade")
	release()

	stale := strings.Repeat("stale-holder-op 999999 leftover\n", 4)
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err = Acquire("rollback")
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	defer release()
	assertHoldsOperationAndPid(t, path, "rollback")
}

func assertHoldsOperationAndPid(t *testing.T, path, op string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	fields := strings.Fields(string(content))
	want := []string{op, strconv.Itoa(os.Getpid())}
	if len(fields) != len(want) || fields[0] != want[0] || fields[1] != want[1] {
		t.Errorf("lock file = %q, want %q", strings.TrimSpace(string(content)), strings.Join(want, " "))
	}
}

func TestAcquireRefusesASecondOperation(t *testing.T) {
	lockPathFor(t)
	holder := holdNodeLockInChild(t, "upgrade")

	for _, op := range []string{"upgrade", "rollback"} {
		_, err := Acquire(op)
		if err == nil {
			t.Fatalf("%s: expected a refusal while another operation holds the node", op)
		}
		for _, want := range []string{
			"stratum " + op + ":",
			"already running on this node",
			fmt.Sprintf("pid %d", holder.Process.Pid),
			"nothing was changed",
			"upgrade", // the holder operation, named
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s error missing %q: %v", op, want, err)
			}
		}
	}
}

// The holder runs a different operation, so the assertion cannot pass on the caller's own name.
func TestAcquireRefusalNamesTheHolderOperation(t *testing.T) {
	lockPathFor(t)
	holdNodeLockInChild(t, "rollback")

	_, err := Acquire("certs")
	if err == nil {
		t.Fatal("expected a refusal while a rollback holds the node")
	}
	if !strings.Contains(err.Error(), "rollback") {
		t.Errorf("refusal does not name the holder operation: %v", err)
	}
}

// A refusal that drops the pid would leave an upgrade beside a released binary unfenced.
func TestAcquireRefusesALegacyPidOnlyHolder(t *testing.T) {
	lockPathFor(t)
	holder := holdRawFlockInChild(t)

	_, err := Acquire("upgrade")
	if err == nil {
		t.Fatal("expected a refusal while a released binary's lock holder holds the node")
	}
	for _, want := range []string{
		"already running on this node",
		fmt.Sprintf("pid %d", holder.Process.Pid),
		"nothing was changed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestAcquireExactlyOneWinner(t *testing.T) {
	lockPathFor(t)
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
			release, err := Acquire("upgrade")
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

func TestAcquireReleaseUnlocks(t *testing.T) {
	path := lockPathFor(t)
	release, err := Acquire("upgrade")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	probe, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("release did not drop the lock: %v", err)
	}
	_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)

	release, err = Acquire("rollback")
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	release()
}

// The fd is found via /proc/self/fd because Acquire hands back only the release closure.
func TestAcquireFdIsCloseOnExec(t *testing.T) {
	path := lockPathFor(t)
	release, err := Acquire("upgrade")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot inspect /proc/self/fd: %v", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + entry.Name())
		if err != nil || link != path {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 {
			t.Fatalf("fcntl F_GETFD on fd %d: %v", fd, errno)
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			t.Errorf("lock fd %d is not close-on-exec: a restarted unit would inherit the lock", fd)
		}
		return
	}
	t.Fatalf("no open fd for the lock file %s", path)
}
