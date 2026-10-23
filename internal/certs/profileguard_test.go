package certs

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// unregisteredProfileCP is a control plane whose node state names the cis
// profile on a binary that registers no profile at all: the node a binary
// that does not carry the cis profile meets. It carries the apiserver and
// etcd manifests, so every certs operation takes its control-plane path.
func unregisteredProfileCP(t *testing.T) {
	t.Helper()
	kubeVIPNode(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	redirectVar(t, &version.AdminConf, "missing-admin.conf")
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if err := os.WriteFile(staticpod.Path(name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.Profile = profile.CISName
	if err := nodestate.SaveState(st); err != nil {
		t.Fatal(err)
	}
	prev := profile.All()
	if err := profile.SetRegistered(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

// hostFiles hashes every file under the PKI, kubeconfig and kubernetes
// directories, the files a certs operation writes.
func hostFiles(t *testing.T) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, dir := range []string{version.PKIDir, version.KubeconfigDir, version.K8sConfDir} {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = sha256.Sum256(b)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return out
}

// TestCertsOperationsRefuseANodeWhoseProfileThisBinaryDoesNotRegister: every
// certs entry — the leaf rotation, each CA rotation and migration phase and
// each cluster orchestrator — resolves the node's profile before its first
// write, and refuses with the cis note. Before the guard they ran on
// to the static-pod restart or the finalize preflight, which stop on the same
// profile only after the leaves were reissued and the kubeconfigs rewritten.
func TestCertsOperationsRefuseANodeWhoseProfileThisBinaryDoesNotRegister(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		op   string
		run  func(b *recordingBuilder) error
	}{
		{"rotate", "rotate", func(*recordingBuilder) error {
			_, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true})
			return err
		}},
		{"rotate-ca --phase=trust-new", "rotate-ca", func(*recordingBuilder) error {
			return RotateCA(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true})
		}},
		{"rotate-ca --rollback", "rotate-ca --rollback", func(*recordingBuilder) error {
			return RollbackCA(io.Discard, CARotateOptions{Quiet: true})
		}},
		{"rotate-ca --cluster", "rotate-ca --cluster", func(b *recordingBuilder) error {
			return RotateCAClusterContext(ctx, io.Discard, CARotateOptions{Quiet: true}, b.build)
		}},
		{"migrate-etcd-ca --phase=trust-both", "migrate-etcd-ca", func(*recordingBuilder) error {
			return MigrateEtcdCA(io.Discard, EtcdCAOptions{Phase: pki.EtcdPhaseTrustBoth, Quiet: true})
		}},
		{"migrate-etcd-ca --cluster", "migrate-etcd-ca --cluster", func(b *recordingBuilder) error {
			return MigrateEtcdCAClusterContext(ctx, io.Discard, EtcdCAOptions{Quiet: true}, b.build)
		}},
		{"migrate-kubelet-client-ca --phase=trust-both", "migrate-kubelet-client-ca", func(*recordingBuilder) error {
			return MigrateKubeletClientCA(io.Discard, KubeletCAOptions{Phase: pki.KubeletPhaseTrustBoth, Quiet: true})
		}},
		{"migrate-kubelet-client-ca --cluster", "migrate-kubelet-client-ca --cluster", func(b *recordingBuilder) error {
			return MigrateKubeletClientCAClusterContext(ctx, io.Discard, KubeletCAOptions{Quiet: true}, b.build)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unregisteredProfileCP(t)
			b := &recordingBuilder{cs: fake.NewSimpleClientset(thisCPNode(t)...)}
			before := hostFiles(t)

			err := tc.run(b)
			if !errors.Is(err, profile.ErrUnregistered) {
				t.Fatalf("err = %v, want the refusal wrapping profile.ErrUnregistered", err)
			}
			for _, want := range []string{profile.CISUnregisteredNote, `profile "cis"`, "nothing was changed", "certs " + tc.op + ":"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal is missing %q: %v", want, err)
				}
			}
			after := hostFiles(t)
			for p, sum := range before {
				if got, ok := after[p]; !ok || got != sum {
					t.Errorf("%s changed under a refused operation", p)
				}
			}
			for p := range after {
				if _, ok := before[p]; !ok {
					t.Errorf("%s was written by a refused operation", p)
				}
			}
			if b.calls != 0 {
				t.Errorf("the refused run built %d API client(s); the profile is resolved before the cluster is touched", b.calls)
			}
			release, err := nodelock.Acquire("rollback")
			if err != nil {
				t.Fatalf("the node lock is still held after the refusal: %v", err)
			}
			release()
		})
	}
}

// A node with no state yet runs no profile, so the guard lets the operation
// go on to resolve the node the way its first boot did.
func TestRequireNodeProfileAcceptsANodeWithoutState(t *testing.T) {
	redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
	if err := RequireNodeProfile("rotate"); err != nil {
		t.Fatalf("RequireNodeProfile with no node state = %v, want nil", err)
	}
}
