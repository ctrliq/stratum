package certs

// The node-maintenance fence for the certs operations. Rotate, RotateCA and
// MigrateEtcdCA each restart control-plane components and rewrite certificate
// material on this host, so each must take the node maintenance lock
// (internal/nodelock, the /run/stratum-upgrade.lock flock the upgrade already
// fences at) before any change, and release it on every exit. The CLI and the
// MCP tool call exactly these functions, so a refusal here is the refusal
// their invocations print; pkg/cli adds nothing.
//
// Callers that already hold the lock must not refuse against themselves:
// RotateCA's leaf reissue and the daily renewal tick reach Rotate from code
// that took the lock, so Rotate takes a held-lock capability
// (Options.NodeLockHeld) instead of reacquiring. A lock held in-process is
// enough to test all of this: flock conflicts between open file descriptions,
// so a second Acquire in the same process refuses exactly as it would against
// a second process. The cross-process cases — a released binary's raw holder,
// a killed holder — are internal/nodelock's own tests.
//
// The lock file lives at version.NodeUpgradeLockPath (/run/stratum-upgrade.lock)
// — a path a test run must neither create nor hold. The fence tests below
// redirect it per test; the TestMain redirects it for the rest of the package,
// whose fixtures predate the fence and reach Rotate without one
// (internal/etcdsnap/main_test.go and internal/ha/main_test.go are the same
// fixture for restore and rejoin).

import (
	"io"

	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "certs-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "certs tests: temp dir for the node lock: %v\n", err)
		os.Exit(1)
	}
	version.NodeUpgradeLockPath = filepath.Join(dir, "node.lock")
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		fmt.Fprintf(os.Stderr, "certs tests: register the cis profile: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// redirectVar points one of the version package's path vars at value for the
// test and restores it afterwards.
func redirectVar(t *testing.T, v *string, value string) {
	t.Helper()
	old := *v
	*v = value
	t.Cleanup(func() { *v = old })
}

// holdNodeLock takes the node maintenance lock the way an `upgrade` run does —
// the operation every released binary records — and releases it when the test
// ends. The release is also returned for tests that must hand the node over
// mid-test; releasing twice is harmless.
func holdNodeLock(t *testing.T) (release func()) {
	t.Helper()
	release, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatalf("take the node lock as an upgrade would: %v", err)
	}
	t.Cleanup(release)
	return release
}

// assertNodeLockRefusal fails unless err is the operation-naming lock refusal:
// the substrings the released wording pins, plus the holder's operation and
// pid, which is what tells an operator WHAT holds the node.
func assertNodeLockRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the operation-naming node-lock refusal, got nil")
	}
	for _, want := range []string{
		"already running on this node",
		"nothing was changed",
		"upgrade", // the holder operation, named
		strconv.Itoa(os.Getpid()),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
}

// readLeaf reads one leaf certificate from the fixture PKI, for the
// byte-identical assertions that prove nothing was reissued.
func readLeaf(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.PKIDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// TestRotateRefusesWhileNodeLockHeld is the leaf-renewal fence: while an
// upgrade (or any other maintenance operation) holds the node, `certs rotate`
// — the CLI's, the MCP tool's and the daily tick's entry point — refuses,
// names the holder, and reissues nothing. A forced rotation is asserted, so
// the run would reach RotateLeaves if the lock were not in the way: the leaf
// must be byte-identical afterwards, which is what "before any change" means
// here.
func TestRotateRefusesWhileNodeLockHeld(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	holdNodeLock(t)

	before := readLeaf(t, "kube-vip.crt")
	_, err := Rotate(io.Discard, Options{Quiet: true}) // force: the full reissue path, not the no-op timer path
	if err == nil {
		t.Fatal("expected Rotate to refuse while the node maintenance lock is held")
	}
	assertNodeLockRefusal(t, err)
	if after := readLeaf(t, "kube-vip.crt"); !bytes.Equal(before, after) {
		t.Error("Rotate reissued the kube-vip leaf while another operation held the node — the lock must be taken before any change")
	}
}

// TestRotateWithHeldLockCapabilityDoesNotReacquire pins the nesting: a caller
// that already holds the node lock passes the capability (Options.NodeLockHeld)
// and Rotate proceeds instead of refusing against its own fence.
func TestRotateWithHeldLockCapabilityDoesNotReacquire(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	release := holdNodeLock(t)
	defer release()

	before := readLeaf(t, "kube-vip.crt")
	if _, err := Rotate(io.Discard, withHeldLock(t, Options{Quiet: true})); err != nil {
		t.Fatalf("Rotate with the held-lock capability refused — a nested call must not reacquire: %v", err)
	}
	if after := readLeaf(t, "kube-vip.crt"); bytes.Equal(before, after) {
		t.Fatal("Rotate with the held-lock capability did not reissue the kube-vip leaf; the test proves nothing")
	}
}

// withHeldLock returns o with the held-lock capability set — the
// Options.NodeLockHeld field nested callers pass instead of reacquiring. It is
// read through reflect rather than written as a literal so this file compiles,
// and the refusal tests above stay runnable, until the wiring adds the field;
// the probe fails naming exactly what is missing.
func withHeldLock(t *testing.T, o Options) Options {
	t.Helper()
	f := reflect.ValueOf(&o).Elem().FieldByName("NodeLockHeld")
	if !f.IsValid() || f.Kind() != reflect.Bool {
		t.Fatalf("certs.Options has no NodeLockHeld bool — the held-lock capability nested calls pass instead of reacquiring is missing (have %v)", reflect.TypeOf(o))
	}
	f.SetBool(true)
	return o
}

// TestRotateReleasesTheNodeLock: a finished rotation hands the node on. Two
// rotations in a row — or a rotation followed by any other operation's
// Acquire — must both get past the lock, which a release that did nothing
// would fail.
func TestRotateReleasesTheNodeLock(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))

	if _, err := Rotate(io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	release, err := nodelock.Acquire("rollback")
	if err != nil {
		t.Fatalf("the node lock is still held after a finished rotation: %v", err)
	}
	release()
}

// stagedCPNode is a control-plane fixture with a CA rotation already staged:
// the state from which a fresh trust-new must refuse, whether or not the node
// lock is free.
func stagedCPNode(t *testing.T) {
	t.Helper()
	kubeVIPNode(t)
	// A control-plane node renders the apiserver static pod; its presence is
	// what makes isWorker() false and the CP phase run.
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staticpod.Path("kube-apiserver"), []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"crt", "key"} {
		if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new."+ext), cert, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// stagedRotationFiles reads the files a staged rotation consists of, for the
// byte-identical assertion that a refusal left them alone.
func stagedRotationFiles(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"ca-new.crt", "ca-new.key", "ca-bundle.crt"} {
		out[name] = readLeaf(t, name)
	}
	return out
}

// assertStagedRotationUnchanged fails unless every staged file is byte-identical.
func assertStagedRotationUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for name, want := range before {
		if got := readLeaf(t, name); !bytes.Equal(want, got) {
			t.Errorf("%s changed during a refused rotation", name)
		}
	}
}

// TestStagedRotationRefusalSurvivesTheNodeLock pins the pre-existing guard: a
// trust-new on a node with a rotation already staged refuses with the staged
// message while the node lock is FREE — the node-lock wiring must not swallow
// or reorder the staged-rotation refusal that stops a second, divergent CA.
// Green before the wiring lands, so any change it reports is the wiring's.
func TestStagedRotationRefusalSurvivesTheNodeLock(t *testing.T) {
	stagedCPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))

	before := stagedRotationFiles(t)
	err := RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
	if err == nil {
		t.Fatal("expected trust-new to refuse on a staged rotation")
	}
	for _, want := range []string{"already staged on", "--rollback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("staged-rotation refusal missing %q: %v", want, err)
		}
	}
	assertStagedRotationUnchanged(t, before)
}

// TestRotateCARefusesWhileNodeLockHeld is the CA-rotation fence: the node-side
// phase entry takes the node maintenance lock at its top — before the staged
// guard, which is the first pre-change refusal on the control-plane path — so
// while an upgrade holds the node, the refusal is the operation-naming lock
// refusal and the staged rotation is untouched.
func TestRotateCARefusesWhileNodeLockHeld(t *testing.T) {
	stagedCPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	holdNodeLock(t)

	before := stagedRotationFiles(t)
	err := RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
	assertNodeLockRefusal(t, err)
	assertStagedRotationUnchanged(t, before)
}

// TestRotateCAReleasesTheNodeLockOnAnErrorExit: a phase that fails after the
// lock — here a worker whose kubelet kubeconfig cannot be read — must still
// hand the node on, or every later maintenance operation on it refuses until
// the process dies.
func TestRotateCAReleasesTheNodeLockOnAnErrorExit(t *testing.T) {
	base := t.TempDir()
	redirectVar(t, &version.K8sConfDir, filepath.Join(base, "kubernetes"))    // no manifests: a worker
	redirectVar(t, &version.KubeconfigDir, filepath.Join(base, "kubeconfig")) // no kubelet.kubeconfig
	redirectVar(t, &version.PKIDir, filepath.Join(base, "pki"))
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(base, "upgrade.lock"))

	err := RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
	if err == nil || !strings.Contains(err.Error(), "read kubelet kubeconfig endpoint") {
		t.Fatalf("want the worker trust-refresh transport failure past the lock, got: %v", err)
	}
	release, err := nodelock.Acquire("rollback")
	if err != nil {
		t.Fatalf("the node lock is still held after a failed phase: %v", err)
	}
	release()
}

// TestMigrateEtcdCARefusesWhileNodeLockHeld is the etcd-CA-migration fence:
// the per-node phase entry takes the node maintenance lock before it decides
// anything about the phase, so while an upgrade holds the node the refusal is
// the operation-naming lock refusal. Once the holder lets go, the same
// invocation gets past the lock — to this node's no-etcd refusal, which is
// where the phase's own work starts — proving the lock was released, not
// dropped.
func TestMigrateEtcdCARefusesWhileNodeLockHeld(t *testing.T) {
	t.Setenv("NODE_IP", "10.0.0.9")                                               // hostNodeIP's fallback with no node state
	redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes")) // no etcd manifest
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))

	release := holdNodeLock(t)
	err := MigrateEtcdCA(io.Discard, EtcdCAOptions{Phase: pki.EtcdPhaseTrustBoth, Quiet: true})
	assertNodeLockRefusal(t, err)
	release()

	err = MigrateEtcdCA(io.Discard, EtcdCAOptions{Phase: pki.EtcdPhaseTrustBoth, Quiet: true})
	if err == nil {
		t.Fatal("expected the phase to refuse for its own reasons once the lock is free")
	}
	if strings.Contains(err.Error(), "already running on this node") {
		t.Fatalf("the lock was not released: %v", err)
	}
	if !strings.Contains(err.Error(), "runs no etcd") {
		t.Errorf("want the phase past the lock at the no-etcd refusal, got: %v", err)
	}
}
