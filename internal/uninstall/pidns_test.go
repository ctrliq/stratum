package uninstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/version"
)

func TestPIDNamespaceReadsTheRunsProcRoot(t *testing.T) {
	fx := &hostEffects{procRoot: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(fx.procRoot, "self", "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pid:[4026532199]", filepath.Join(fx.procRoot, "self", "ns", "pid")); err != nil {
		t.Fatal(err)
	}
	ns, err := pidNamespace(fx)
	if err != nil || ns != "pid:[4026532199]" {
		t.Errorf("pidNamespace = %q, %v; want the link's target", ns, err)
	}
	if _, err := pidNamespace(&hostEffects{procRoot: t.TempDir()}); err == nil {
		t.Error("a missing ns link read as a namespace")
	}
}

func TestHostPIDNamespaceGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ns     string
		err    error
		assume bool
		refuse string
	}{
		{"host namespace", initPIDNamespace, nil, false, ""},
		{"private namespace", "pid:[4026532199]", nil, false, "private PID namespace (pid:[4026532199])"},
		{"unreadable", "", errors.New("permission denied"), false, "could not tell whether this process can see the host's processes (permission denied)"},
		{"private namespace, operator checked", "pid:[4026532199]", nil, true, ""},
		{"unreadable, operator checked", "", errors.New("permission denied"), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{assumeNoForeignRuntimes: tc.assume, pidNamespaceFn: func() (string, error) { return tc.ns, tc.err }}
			err := hostPIDNamespaceGate(fx)
			if tc.refuse == "" {
				if err != nil {
					t.Errorf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refuse) || !strings.Contains(err.Error(), "--assume-no-foreign-runtimes") {
				t.Errorf("gate = %v; want a refusal containing %q that names --assume-no-foreign-runtimes", err, tc.refuse)
			}
		})
	}
	if err := hostPIDNamespaceGate(&hostEffects{}); !errors.Is(err, errUnsetEffect) {
		t.Errorf("an unset probe = %v, want the unset-effect refusal", err)
	}
}

func TestRunRefusesInAPrivatePIDNamespaceBeforeChangingAnything(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.pidNamespaceFn = func() (string, error) { return "pid:[4026532199]", nil }
	fx.disableSystemdUnit = func() error { *events = append(*events, "disable unit"); return nil }
	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "--assume-no-foreign-runtimes") {
		t.Fatalf("runUninstall = %v, want the private-namespace refusal", err)
	}
	if len(*events) != 0 {
		t.Errorf("events = %q, want none: the refusal comes before any change", *events)
	}
}

func TestRunWithTheOverrideProceedsInAPrivatePIDNamespace(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.pidNamespaceFn = func() (string, error) { return "pid:[4026532199]", nil }
	fx.assumeNoForeignRuntimes = true
	var removed []string
	fx.removeAll = func(p string) error { removed = append(removed, p); return nil }
	if err := runUninstall(io.Discard, true, fx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, Targets()) {
		t.Errorf("removed = %q, want every target %q", removed, Targets())
	}
}

func TestRunKeepsTheCNIDirWhileAnotherRuntimeRuns(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return []string{"k3s-server (pid 42)"}, nil, nil }
	var removed []string
	fx.removeAll = func(p string) error { removed = append(removed, p); return nil }
	var out strings.Builder
	err := runUninstall(&out, true, fx)
	if slices.Contains(removed, host.CNIDir) {
		t.Errorf("removed %s while another runtime runs", host.CNIDir)
	}
	if want := slices.DeleteFunc(slices.Clone(Targets()), func(p string) bool { return p == host.CNIDir }); !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want every other target %q", removed, want)
	}
	if !strings.Contains(out.String(), "left "+host.CNIDir+" in place") || !strings.Contains(out.String(), "k3s-server (pid 42)") {
		t.Errorf("output does not report the kept directory and why:\n%s", out.String())
	}
	if err == nil || !errors.Is(err, errRuntimeRunning) || strings.Contains(out.String(), "stratum uninstalled.") {
		t.Errorf("runUninstall = %v; want the run not reported clean", err)
	}
}

func TestRunKeepsTheCNIDirWhenItCannotCheckForAnotherRuntime(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return nil, nil, errors.New("read /proc: permission denied") }
	var removed []string
	fx.removeAll = func(p string) error { removed = append(removed, p); return nil }
	err := runUninstall(io.Discard, true, fx)
	if slices.Contains(removed, host.CNIDir) {
		t.Errorf("removed %s without knowing whether another runtime uses it", host.CNIDir)
	}
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("runUninstall = %v, want the failed check reported", err)
	}
}

// A runtime that keeps /etc/cni/net.d may bind-mount its configuration there.
// The unmount phase runs before the delete loop, so it must leave that mount
// alone too, and detach the run's own mounts as before.
func TestRunLeavesMountsUnderAKeptCNIDirMounted(t *testing.T) {
	_, fx := runNode(t, "worker")
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return []string{"k3s-server (pid 42)"}, nil, nil }
	foreign := filepath.Join(host.CNIDir, "10-foreign.conflist")
	own := filepath.Join(version.DataDir, "kubelet-volume")
	mounted := []string{foreign, own}
	writeMountinfo := func() {
		var b strings.Builder
		for i, mp := range mounted {
			fmt.Fprintf(&b, "%d 1 0:%d / %s rw - tmpfs tmpfs rw\n", 30+i, 50+i, mp)
		}
		if err := os.WriteFile(fx.mountInfoPath, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMountinfo()
	var unmounted []string
	fx.unmountFn = func(mp string) error {
		unmounted = append(unmounted, mp)
		mounted = slices.DeleteFunc(mounted, func(m string) bool { return m == mp })
		writeMountinfo()
		return nil
	}
	fx.unmountStateFn = func() error { return unmountState(fx) }
	var removed []string
	fx.removeAll = func(p string) error { removed = append(removed, p); return nil }
	err := runUninstall(io.Discard, true, fx)
	if slices.Contains(unmounted, foreign) {
		t.Errorf("unmounted %s under a CNI directory another runtime may use", foreign)
	}
	if !slices.Contains(unmounted, own) {
		t.Errorf("unmounted = %q, want the run's own mount %s detached", unmounted, own)
	}
	if slices.Contains(removed, host.CNIDir) {
		t.Errorf("removed %s while another runtime runs", host.CNIDir)
	}
	if err == nil || !errors.Is(err, errRuntimeRunning) {
		t.Errorf("runUninstall = %v; want the kept directory reported", err)
	}
}

func TestInitPIDNamespaceIsTheKernelsInitialInode(t *testing.T) {
	const procPIDInitIno = 0xEFFFFFFC
	if want := "pid:[" + strconv.Itoa(procPIDInitIno) + "]"; initPIDNamespace != want {
		t.Errorf("initPIDNamespace = %q, want %q (PROC_PID_INIT_INO)", initPIDNamespace, want)
	}
}
