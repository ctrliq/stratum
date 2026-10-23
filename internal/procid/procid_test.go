package procid

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// writeProc creates one fake /proc entry: a directory named pid holding a
// cmdline file built from args and, when exe is not empty, an exe symlink to
// it. The symlink's target is only ever read, never followed, so a target
// need not exist; a BuildInfo fixture must point at a real file.
func writeProc(t *testing.T, root, pid, exe string, args ...string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmdline := strings.Join(args, "\x00")
	if len(args) > 0 {
		cmdline += "\x00"
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o600); err != nil {
		t.Fatal(err)
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
}

// collect walks root and returns every Process the walk yields, failing the
// test if the walk errors.
func collect(t *testing.T, root string) []Process {
	t.Helper()
	var got []Process
	if err := Walk(root, func(p Process) (bool, error) {
		got = append(got, p)
		return true, nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return got
}

// TestWalkYieldsLiveProcesses: one /proc walk yields pid, argv and the exe
// target of every live process, in /proc order. Entries that are not
// processes — non-numeric names, numeric names that are not directories,
// kernel threads and zombies with an empty cmdline — never reach the caller.
func TestWalkYieldsLiveProcesses(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "123", "/tmp/stratum-linux-amd64", "/usr/local/bin/stratum", "up", "--worker")
	writeProc(t, root, "456", "", "/usr/bin/kubelet") // an exe no readlink can resolve
	writeProc(t, root, "42", "")                      // kernel thread: empty cmdline
	writeProc(t, root, "acpi", "", "/sbin/acpid")     // a non-numeric name
	if err := os.WriteFile(filepath.Join(root, "789"), []byte("/usr/bin/other\x00"), 0o600); err != nil {
		t.Fatal(err) // a numeric name that is not a directory
	}

	got := collect(t, root)
	if len(got) != 2 {
		t.Fatalf("walk yielded %d processes (%+v), want 2", len(got), got)
	}
	if got[0].PID != 123 || strings.Join(got[0].Argv, " ") != "/usr/local/bin/stratum up --worker" {
		t.Errorf("first process = %+v, want pid 123 running `stratum up --worker`", got[0])
	}
	if got[0].Exe != "/tmp/stratum-linux-amd64" {
		t.Errorf("exe of pid 123 = %q, want the symlink target", got[0].Exe)
	}
	if got[1].PID != 456 || strings.Join(got[1].Argv, " ") != "/usr/bin/kubelet" {
		t.Errorf("second process = %+v, want pid 456 running /usr/bin/kubelet", got[1])
	}
	if got[1].Exe != "" {
		t.Errorf("exe of pid 456 = %q, want an unresolvable exe left empty", got[1].Exe)
	}
}

// TestWalkSkipsExitedProcesses: a process that exits between the /proc
// listing and the cmdline read fails with ENOENT or ESRCH. It is gone, not an
// unreadable process table, so the walk skips it. Any other read error fails
// the walk: a caller deciding what is safe to stop must not read an
// unreadable table as an empty one.
func TestWalkSkipsExitedProcesses(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "123", "", "/usr/bin/kubelet")
	writeProc(t, root, "888", "", "/usr/bin/gone")
	writeProc(t, root, "999", "", "/usr/bin/dying")
	saved := readProcFile
	t.Cleanup(func() { readProcFile = saved })
	readProcFile = func(path string) ([]byte, error) {
		switch {
		case strings.Contains(path, "/888/"):
			return nil, &os.PathError{Op: "read", Path: path, Err: syscall.ENOENT}
		case strings.Contains(path, "/999/"):
			return nil, &os.PathError{Op: "read", Path: path, Err: syscall.ESRCH}
		default:
			return os.ReadFile(path)
		}
	}
	got := collect(t, root)
	if len(got) != 1 || got[0].PID != 123 {
		t.Fatalf("walk yielded %+v, want only the live pid 123", got)
	}

	root = t.TempDir()
	writeProc(t, root, "123", "", "/usr/bin/kubelet")
	writeProc(t, root, "777", "", "/usr/bin/secret")
	readProcFile = func(path string) ([]byte, error) {
		if strings.Contains(path, "/777/") {
			return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EACCES}
		}
		return os.ReadFile(path)
	}
	if err := Walk(root, func(Process) (bool, error) { return true, nil }); err == nil {
		t.Fatal("an unreadable cmdline was skipped; the walk must fail closed")
	}
}

// TestWalkStopsAndFailsOnVisitorRequest: a visitor's false stops the walk
// after the process it accepted, and a visitor error fails the walk — a
// scanner that cannot decide identity must abort rather than report a partial
// scan as a complete one.
func TestWalkStopsAndFailsOnVisitorRequest(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "1", "", "/bin/a")
	writeProc(t, root, "2", "", "/bin/b")
	writeProc(t, root, "3", "", "/bin/c")
	var got []int
	if err := Walk(root, func(p Process) (bool, error) {
		got = append(got, p.PID)
		return len(got) < 2, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("walk yielded %v, want it to stop after pid 2", got)
	}
	boom := errors.New("boom")
	if err := Walk(root, func(Process) (bool, error) { return true, boom }); !errors.Is(err, boom) {
		t.Fatalf("walk error = %v, want the visitor's error", err)
	}
}

// TestExited pins the skip-or-fail decision every scanner shares: ENOENT and
// ESRCH mean the process is gone; every other error is unknown.
func TestExited(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"process directory removed", &os.PathError{Op: "read", Path: "/proc/1/cmdline", Err: syscall.ENOENT}, true},
		{"process exiting", &os.PathError{Op: "read", Path: "/proc/1/cmdline", Err: syscall.ESRCH}, true},
		{"sentinel not-exist", os.ErrNotExist, true},
		{"unreadable", &os.PathError{Op: "read", Path: "/proc/1/cmdline", Err: syscall.EACCES}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Exited(tc.err); got != tc.want {
				t.Errorf("Exited(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBuildInfo: the build information of a process's executable tells the
// legacy-supervisor check which module the binary was built from. A valid
// non-Go ELF image is not an error — a native program may legitimately run an
// `up` argument — but a malformed or unreadable image must fail closed.
func TestBuildInfo(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := BuildInfo(self); err != nil || info == nil {
		t.Fatalf("Go executable (this test binary): info=%v err=%v", info, err)
	}
	// A minimal valid native ELF image, without Go sections.
	native := make([]byte, 64)
	copy(native, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(native[16:], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(native[18:], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(native[20:], 1)
	binary.LittleEndian.PutUint16(native[52:], 64)
	path := filepath.Join(t.TempDir(), "native")
	if err := os.WriteFile(path, native, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := BuildInfo(path); err != nil || info != nil {
		t.Fatalf("native executable: info=%v err=%v", info, err)
	}
	if err := os.WriteFile(path, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildInfo(path); err == nil {
		t.Fatal("malformed executable accepted")
	}
	if _, err := BuildInfo(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing executable accepted")
	}
}

// TestIsStratumBuild: identity by build information is the stratum module —
// as the main module, so a renamed binary keeps its identity, or as a
// dependency, so a distribution CLI embedding stratum still hosts it.
// Anything else, including a binary with no readable build information, is
// not stratum.
func TestIsStratumBuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want bool
	}{
		{"renamed stratum binary", &debug.BuildInfo{Main: debug.Module{Path: "github.com/ctrliq/stratum"}}, true},
		{"distribution CLI embedding stratum", &debug.BuildInfo{Main: debug.Module{Path: "example/embedder"}, Deps: []*debug.Module{{Path: "github.com/ctrliq/stratum"}}}, true},
		{"unrelated Go program", &debug.BuildInfo{Main: debug.Module{Path: "example/unrelated"}}, false},
		{"no build information", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStratumBuild(tc.info); got != tc.want {
				t.Errorf("IsStratumBuild(%v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestIsUpCommand follows Cobra root-command selection: leading flags consume
// their values, and the first non-flag token must be `up`. A resource name or
// flag value passed to another stratum command — `kubectl get pod up --watch`
// — must never read as the up subcommand.
func TestIsUpCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"node start", []string{"stratum", "up", "--worker"}, true},
		{"leading boolean flag", []string{"stratum", "--worker=true", "up"}, true},
		{"leading config flag with value", []string{"stratum", "--config=/etc/cluster.yaml", "up"}, true},
		{"leading separate value", []string{"stratum", "--config", "/etc/cluster.yaml", "up"}, true},
		{"single-letter flag with value", []string{"stratum", "-c", "/etc/cluster.yaml", "up"}, true},
		{"leading value named up", []string{"stratum", "--config", "up", "kubectl", "get", "pods"}, false},
		{"watched pod named up", []string{"stratum", "kubectl", "get", "pod", "up", "--watch"}, false},
		{"flag value up", []string{"stratum", "operator", "--node-name", "up"}, false},
		{"another subcommand", []string{"stratum", "uninstall", "--yes"}, false},
		{"no subcommand", []string{"stratum"}, false},
		{"empty argv", nil, false},
		{"subcommand after --", []string{"stratum", "--", "up"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUpCommand(tc.args); got != tc.want {
				t.Errorf("IsUpCommand(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestHasPayloadPath: containerd and kubelet are identified by exact payload
// path — argv[0] must be the staged binary under BinDir. A distribution-owned
// binary with the same basename, or anything else under BinDir, is not the
// component, and a caller deciding what to signal must refuse it.
func TestHasPayloadPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
		argv []string
		want bool
	}{
		{"owned containerd", "containerd", []string{filepath.Join(version.BinDir, "containerd")}, true},
		{"distro containerd", "containerd", []string{"/usr/bin/containerd"}, false},
		{"owned kubelet", "kubelet", []string{filepath.Join(version.BinDir, "kubelet"), "--config", "k.yaml"}, true},
		{"distro kubelet", "kubelet", []string{"/usr/bin/kubelet"}, false},
		{"another component's payload", "kubelet", []string{filepath.Join(version.BinDir, "containerd")}, false},
		{"a same-basename lookalike under BinDir", "containerd", []string{filepath.Join(version.BinDir, "containerd-helper")}, false},
		{"empty argv", "containerd", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasPayloadPath(tc.unit, Process{Argv: tc.argv}); got != tc.want {
				t.Errorf("HasPayloadPath(%q, %q) = %v, want %v", tc.unit, tc.argv, got, tc.want)
			}
		})
	}
}

// TestIsOperator: the operator is identified by its flag shape, not its
// executable name, which may be a release artifact. Every Stratum-owned flag
// must be present in any order; any program without the full shape — another
// program's operator, another subcommand, or the pkill command itself — is
// refused.
func TestIsOperator(t *testing.T) {
	operatorArgs := func(exe string) []string {
		return []string{exe, "operator", "--kubeconfig", version.AdminConf, "--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"}
	}
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"canonical name", operatorArgs("/usr/local/bin/stratum"), true},
		{"release artifact", operatorArgs("/tmp/stratum-linux-amd64"), true},
		{"flags in another order", []string{"/usr/local/bin/stratum", "operator", "--leader-elect", "--helm-cache", version.HelmCacheDir, "--manifests-dir", version.ManifestsDir, "--kubeconfig", version.AdminConf}, true},
		{"missing leader election", slices.DeleteFunc(operatorArgs("/usr/local/bin/stratum"), func(a string) bool { return a == "--leader-elect" }), false},
		{"another kubeconfig", []string{"/usr/local/bin/stratum", "operator", "--kubeconfig", "/etc/other.conf", "--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"}, false},
		{"another manifests dir", []string{"/usr/local/bin/stratum", "operator", "--kubeconfig", version.AdminConf, "--manifests-dir", "/tmp/m", "--helm-cache", version.HelmCacheDir, "--leader-elect"}, false},
		{"another program's operator", []string{"/usr/local/bin/other", "operator"}, false},
		{"another subcommand", []string{"/usr/local/bin/stratum", "up", "--kubeconfig", version.AdminConf, "--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"}, false},
		{"the pkill command itself", []string{"sudo", "pkill", "-TERM", "-f", "pattern"}, false},
		{"no subcommand", []string{"/usr/local/bin/stratum"}, false},
		{"empty argv", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOperator(Process{Argv: tc.args}); got != tc.want {
				t.Errorf("IsOperator(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestIsStratumTopLevel: the top-level `stratum up` process is this exact
// binary running the up subcommand — /proc/<pid>/exe equality with the
// running executable, so the same program built elsewhere, or started with an
// unrelated subcommand, never matches. The kernel's " (deleted)" marker — an
// upgrade replaced the binary underneath a running up — is tolerated on both
// sides, and an exe that cannot be resolved is not a match: better to leave a
// process alone than to signal one this call could not confirm.
func TestIsStratumTopLevel(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		p    Process
		want bool
	}{
		{"up with the same file as this process", Process{Argv: []string{"stratum", "up", "--worker"}, Exe: self}, true},
		{"up whose binary an upgrade unlinked", Process{Argv: []string{self, "up"}, Exe: self + " (deleted)"}, true},
		{"another program running up", Process{Argv: []string{"other", "up"}, Exe: "/usr/local/bin/other"}, false},
		{"up but the exe cannot be resolved", Process{Argv: []string{"stratum", "up"}}, false},
		{"this call's own uninstall subcommand", Process{Argv: []string{self, "uninstall", "--yes"}, Exe: self}, false},
		{"no subcommand", Process{Argv: []string{self}, Exe: self}, false},
		{"watched pod named up", Process{Argv: []string{"stratum", "kubectl", "get", "pod", "up", "--watch"}, Exe: self}, false},
		{"flag value up", Process{Argv: []string{"stratum", "operator", "--node-name", "up"}, Exe: self}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStratumTopLevel(tc.p); got != tc.want {
				t.Errorf("IsStratumTopLevel(%+v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

// TestStratumUpPredicatesAgree: two rules answer "is this the stratum up
// process" — IsUpCommand for the legacy-supervisor handoff check, and
// IsStratumTopLevel for uninstall — and both must decide the up subcommand
// through one shared argv scan, so the two rules cannot drift. Over this
// shared table of argv shapes (flags before and after up, the "--"
// terminator, a missing up) the two answers must be identical, and the answer
// itself is pinned to the flag-skipping scan IsUpCommand has always followed.
// The rules' deliberate difference is not argv-shaped and each is pinned by
// its own test: the legacy check accepts any stratum build's up, while
// uninstall matches its own binary exactly — here every process's exe is this
// test binary, the only file IsStratumTopLevel can confirm, so the table
// answers purely the up question.
func TestStratumUpPredicatesAgree(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"flags after up", []string{"stratum", "up", "--worker"}, true},
		{"flag with = before up", []string{"stratum", "--config=/etc/cluster.yaml", "up"}, true},
		{"flag with a separate value before up", []string{"stratum", "--config", "/etc/cluster.yaml", "up"}, true},
		{"single-letter flag with a value before up", []string{"stratum", "-c", "/etc/cluster.yaml", "up"}, true},
		{"boolean flag with = before up", []string{"stratum", "--worker=true", "up"}, true},
		{"terminator after up", []string{"stratum", "up", "--", "node1"}, true},
		{"terminator hides up", []string{"stratum", "--", "up"}, false},
		{"boolean flag reads the subcommand as its value", []string{"stratum", "--worker", "up"}, false},
		{"another subcommand", []string{"stratum", "uninstall", "--yes"}, false},
		{"no subcommand", []string{"stratum"}, false},
		{"empty argv", nil, false},
		{"watched pod named up", []string{"stratum", "kubectl", "get", "pod", "up", "--watch"}, false},
		{"flag value named up", []string{"stratum", "--config", "up", "kubectl", "get", "pods"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := IsUpCommand(tc.args)
			top := IsStratumTopLevel(Process{Argv: tc.args, Exe: self})
			if up != tc.want {
				t.Errorf("IsUpCommand(%q) = %v, want %v", tc.args, up, tc.want)
			}
			if top != tc.want {
				t.Errorf("IsStratumTopLevel(%q with this binary's exe) = %v, want %v", tc.args, top, tc.want)
			}
			if up != top {
				t.Errorf("the predicates disagree on %q: IsUpCommand = %v, IsStratumTopLevel = %v — up detection must stay one shared rule", tc.args, up, top)
			}
		})
	}
}

// TestIsRuntimeShim: runtime-v2 shims outlive containerd and are attributed
// by the runtime socket they serve, not by their executable path, which a
// distribution can install under the same name. A shim for another
// containerd, a non-shim binary pointed at stratum's socket, and a shim with
// no address at all must all be refused.
func TestIsRuntimeShim(t *testing.T) {
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"owned surviving shim", []string{"/usr/bin/containerd-shim-runc-v2", "-namespace", "k8s.io", "-address", sock}, true},
		{"owned shim, address as one token", []string{"/usr/bin/containerd-shim-runc-v2", "-address=" + sock}, true},
		{"shim for another containerd", []string{"/usr/bin/containerd-shim-runc-v2", "-address", "/run/containerd/containerd.sock"}, false},
		{"another binary on stratum's socket", []string{"/usr/bin/conmon", "-address", sock}, false},
		{"shim without an address", []string{"/usr/bin/containerd-shim-runc-v2", "-namespace", "k8s.io"}, false},
		{"empty argv", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRuntimeShim(Process{Argv: tc.args}); got != tc.want {
				t.Errorf("IsRuntimeShim(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestHasArgv0Basename: the static-pod scanners are deliberately loose — any
// process whose argv[0] basename is the component name counts, whatever path
// it runs from, because a distro-owned same-named binary is still that
// component running on this node. A different basename is not.
func TestHasArgv0Basename(t *testing.T) {
	for _, tc := range []struct {
		name string
		comp string
		argv []string
		want bool
	}{
		{"running from its own directory", "etcd", []string{"/usr/sbin/etcd", "--data-dir", "/var/lib/etcd"}, true},
		{"bare name", "etcd", []string{"etcd"}, true},
		{"a distro-owned same-named binary still counts", "kubelet", []string{"/usr/bin/kubelet"}, true},
		{"different basename", "etcd", []string{"/usr/bin/etcdctl"}, false},
		{"a lookalike suffix", "etcd", []string{"/usr/sbin/etcd-helper"}, false},
		{"empty argv", "etcd", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasArgv0Basename(tc.comp, Process{Argv: tc.argv}); got != tc.want {
				t.Errorf("HasArgv0Basename(%q, %q) = %v, want %v", tc.comp, tc.argv, got, tc.want)
			}
		})
	}
}
