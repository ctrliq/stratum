package certs

import (
	"io"

	"bytes"
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func kubeVIPNode(t *testing.T) (check func(phase string, wantCAs int)) {
	t.Helper()
	fakeAdminClientset(t)
	origPKI, origKC, origConf, origAdmin, origNet := version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath
	t.Cleanup(func() {
		version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath = origPKI, origKC, origConf, origAdmin, origNet
	})
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	version.NetConfigPath = filepath.Join(base, "netconfig") // absent: the IPv4 defaults
	// No manifests and no supervisor on this socket: restart paths find nothing to restart.
	origSock := version.ControlSocket
	version.ControlSocket = filepath.Join(base, "stratum.sock")
	t.Cleanup(func() { version.ControlSocket = origSock })

	const vip = "192.168.99.10"
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
		VIP: nodestate.VIP{Address: vip},
	}); err != nil {
		t.Fatal(err)
	}
	if err := pki.GenerateAll("cp1", "192.168.99.12", nil); err != nil {
		t.Fatal(err)
	}
	// The state `up` leaves on the initiator: every other kubeconfig targets the VIP.
	if err := kubeconfig.GenerateAll("cp1", "https://"+vip+":6443", true); err != nil {
		t.Fatal(err)
	}
	kv := version.KubeVipKubeconfig()
	read := func(f string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(version.PKIDir, f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return func(phase string, wantCAs int) {
		t.Helper()
		cfg, err := clientcmd.LoadFromFile(kv)
		if err != nil {
			t.Fatalf("%s: load kube-vip.kubeconfig: %v", phase, err)
		}
		cl := cfg.Clusters[version.ClusterName]
		if cl.Server != "https://127.0.0.1:6443" {
			t.Errorf("%s: kube-vip server = %q, want the local apiserver", phase, cl.Server)
		}
		if !bytes.Equal(cl.CertificateAuthorityData, read("ca-bundle.crt")) {
			t.Errorf("%s: kube-vip.kubeconfig does not trust the current bundle", phase)
		}
		if n := countPEM(cl.CertificateAuthorityData); n != wantCAs {
			t.Errorf("%s: kube-vip.kubeconfig trusts %d CA(s), want %d", phase, n, wantCAs)
		}
		if ai := cfg.AuthInfos["system:kube-vip"]; ai == nil || !bytes.Equal(ai.ClientCertificateData, read("kube-vip.crt")) {
			t.Errorf("%s: kube-vip.kubeconfig does not carry the current kube-vip leaf", phase)
		}
	}
}

func TestCARotationRewritesKubeVIPKubeconfig(t *testing.T) {
	check := kubeVIPNode(t)

	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := regenFromNodeState(); err != nil {
		t.Fatal(err)
	}
	check("trust-new", 2)

	if err := pki.PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteSA(); err != nil {
		t.Fatal(err)
	}
	hostname, nodeIP, extra, _, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.RotateLeaves(hostname, nodeIP, extra); err != nil {
		t.Fatal(err)
	}
	if err := regenFromNodeState(); err != nil {
		t.Fatal(err)
	}
	check("sign-new", 2)

	if err := pki.FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	if err := regenFromNodeState(); err != nil {
		t.Fatal(err)
	}
	check("finalize", 1)
}

func countPEM(b []byte) int {
	n := 0
	for {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			return n
		}
		n++
	}
}

func TestCARollbackRewritesKubeVIPKubeconfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		promoted bool
	}{
		{"after trust-new", false},
		{"after sign-new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := kubeVIPNode(t)
			original, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			if err := regenFromNodeState(); err != nil {
				t.Fatal(err)
			}
			if tc.promoted {
				if err := pki.PromoteCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.PromoteSA(); err != nil {
					t.Fatal(err)
				}
				hostname, nodeIP, extra, _, err := hostNodeIP()
				if err != nil {
					t.Fatal(err)
				}
				if err := pki.RotateLeaves(hostname, nodeIP, extra); err != nil {
					t.Fatal(err)
				}
				if err := regenFromNodeState(); err != nil {
					t.Fatal(err)
				}
			}
			check("before rollback", 2)
			if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
				t.Fatal(err)
			}
			check("rollback", 1)
			cfg, err := clientcmd.LoadFromFile(version.KubeVipKubeconfig())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cfg.Clusters[version.ClusterName].CertificateAuthorityData, original) {
				t.Error("rollback: kube-vip.kubeconfig does not trust the restored pre-rotation CA")
			}
		})
	}
}

func TestLeafRotationRewritesKubeVIPKubeconfig(t *testing.T) {
	check := kubeVIPNode(t)
	before, err := os.ReadFile(filepath.Join(version.PKIDir, "kube-vip.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("RotateContext: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(version.PKIDir, "kube-vip.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("the rotation did not reissue the kube-vip leaf; the test proves nothing")
	}
	check("certs rotate", 1)
}

func TestRegenKubeconfigsKeepsEachServer(t *testing.T) {
	serverOf := func(t *testing.T, f string) string {
		t.Helper()
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			t.Fatalf("load %s: %v", f, err)
		}
		return cfg.Clusters[version.ClusterName].Server
	}
	plain := []string{"admin.kubeconfig", "controller-manager.kubeconfig", "scheduler.kubeconfig", "kubelet.kubeconfig"}

	t.Run("VIP node", func(t *testing.T) {
		kubeVIPNode(t)
		if err := regenFromNodeState(); err != nil {
			t.Fatal(err)
		}
		for _, f := range plain {
			if got := serverOf(t, filepath.Join(version.KubeconfigDir, f)); got != "https://192.168.99.10:6443" {
				t.Errorf("%s server = %q, want the VIP admin.conf holds", f, got)
			}
		}
		if got := serverOf(t, version.SchedulerKubeconfig()); got != "https://192.168.99.10:6443" {
			t.Errorf("the scheduler's own kubeconfig server = %q, want the VIP admin.conf holds", got)
		}
		if got := serverOf(t, version.AdminConf); got != "https://192.168.99.10:6443" {
			t.Errorf("admin.conf server = %q, want the VIP", got)
		}
		if got := serverOf(t, version.KubeVipKubeconfig()); got != "https://127.0.0.1:6443" {
			t.Errorf("kube-vip server = %q, want the local apiserver", got)
		}
	})

	t.Run("node without a VIP", func(t *testing.T) {
		origPKI, origKC, origConf, origAdmin, origNet := version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath
		t.Cleanup(func() {
			version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath = origPKI, origKC, origConf, origAdmin, origNet
		})
		base := t.TempDir()
		version.PKIDir = filepath.Join(base, "pki")
		version.KubeconfigDir = filepath.Join(base, "kubeconfig")
		version.K8sConfDir = filepath.Join(base, "kubernetes")
		version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
		version.NetConfigPath = filepath.Join(base, "netconfig")
		if err := nodestate.SaveState(&nodestate.NodeState{
			Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
		}); err != nil {
			t.Fatal(err)
		}
		if err := pki.GenerateAll("cp1", "192.168.99.12", nil); err != nil {
			t.Fatal(err)
		}
		if err := kubeconfig.GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
			t.Fatal(err)
		}
		if err := regenFromNodeState(); err != nil {
			t.Fatal(err)
		}
		for _, f := range plain {
			if got := serverOf(t, filepath.Join(version.KubeconfigDir, f)); got != "https://127.0.0.1:6443" {
				t.Errorf("%s server = %q, want the local apiserver admin.conf holds", f, got)
			}
		}
		if got := serverOf(t, version.AdminConf); got != "https://127.0.0.1:6443" {
			t.Errorf("admin.conf server = %q, want the local apiserver", got)
		}
		if _, err := os.Stat(version.KubeVipKubeconfig()); err == nil {
			t.Error("a node without a VIP gained a kube-vip.kubeconfig")
		}
	})
}

func TestKubeconfigRewriteUsesTheVIPReadBeforeTheChange(t *testing.T) {
	kubeVIPNode(t)
	hostname, _, _, vip, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if vip == "" {
		t.Fatal("setup: the node has no VIP")
	}
	kv := version.KubeVipKubeconfig()
	if err := os.Remove(kv); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodestate.StatePath(), []byte("vip: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := regenKubeconfigs(hostname, vip); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kv); err != nil {
		t.Errorf("kube-vip.kubeconfig not rewritten from the VIP read before the change: %v", err)
	}
}
