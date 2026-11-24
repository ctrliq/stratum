package certs

// A second in-process Acquire refuses as it would against another process.

import (
	"context"
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

func redirectVar(t *testing.T, v *string, value string) {
	t.Helper()
	old := *v
	*v = value
	t.Cleanup(func() { *v = old })
}

func holdNodeLock(t *testing.T) (release func()) {
	t.Helper()
	release, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatalf("take the node lock as an upgrade would: %v", err)
	}
	t.Cleanup(release)
	return release
}

func assertNodeLockRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the operation-naming node-lock refusal, got nil")
	}
	for _, want := range []string{
		"already running on this node",
		"nothing was changed",
		"upgrade",
		strconv.Itoa(os.Getpid()),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
}

func readLeaf(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.PKIDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func TestRotateRefusesWhileNodeLockHeld(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	holdNodeLock(t)

	before := readLeaf(t, "kube-vip.crt")
	_, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}) // force: the full reissue path, not the no-op timer path
	if err == nil {
		t.Fatal("expected Rotate to refuse while the node maintenance lock is held")
	}
	assertNodeLockRefusal(t, err)
	if after := readLeaf(t, "kube-vip.crt"); !bytes.Equal(before, after) {
		t.Error("Rotate reissued the kube-vip leaf while another operation held the node — the lock must be taken before any change")
	}
}

func TestRotateWithHeldLockCapabilityDoesNotReacquire(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	release := holdNodeLock(t)
	defer release()

	before := readLeaf(t, "kube-vip.crt")
	if _, err := RotateContext(context.Background(), io.Discard, withHeldLock(t, Options{Quiet: true})); err != nil {
		t.Fatalf("Rotate with the held-lock capability refused — a nested call must not reacquire: %v", err)
	}
	if after := readLeaf(t, "kube-vip.crt"); bytes.Equal(before, after) {
		t.Fatal("Rotate with the held-lock capability did not reissue the kube-vip leaf; the test proves nothing")
	}
}

func withHeldLock(t *testing.T, o Options) Options {
	t.Helper()
	f := reflect.ValueOf(&o).Elem().FieldByName("NodeLockHeld")
	if !f.IsValid() || f.Kind() != reflect.Bool {
		t.Fatalf("certs.Options has no NodeLockHeld bool — the held-lock capability nested calls pass instead of reacquiring is missing (have %v)", reflect.TypeOf(o))
	}
	f.SetBool(true)
	return o
}

func TestRotateReleasesTheNodeLock(t *testing.T) {
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))

	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	release, err := nodelock.Acquire("rollback")
	if err != nil {
		t.Fatalf("the node lock is still held after a finished rotation: %v", err)
	}
	release()
}

func stagedCPNode(t *testing.T) {
	t.Helper()
	kubeVIPNode(t)
	// The apiserver manifest is what makes isWorker() false and the CP phase run.
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

func stagedRotationFiles(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"ca-new.crt", "ca-new.key", "ca-bundle.crt"} {
		out[name] = readLeaf(t, name)
	}
	return out
}

func assertStagedRotationUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for name, want := range before {
		if got := readLeaf(t, name); !bytes.Equal(want, got) {
			t.Errorf("%s changed during a refused rotation", name)
		}
	}
}

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

func TestRotateCARefusesWhileNodeLockHeld(t *testing.T) {
	stagedCPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	holdNodeLock(t)

	before := stagedRotationFiles(t)
	err := RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
	assertNodeLockRefusal(t, err)
	assertStagedRotationUnchanged(t, before)
}

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
