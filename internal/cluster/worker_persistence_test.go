package cluster

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Node.yaml saves re-read under the etcd data lock and refuse while it is held.

func workerScratchHost(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.PKIDir, filepath.Join(tmp, "pki")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	// hostDirs is stubbed here, so the PKI dir the CA writes stage into needs this.
	if err := os.MkdirAll(version.PKIDir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// A unit test may not run modprobe/sysctl/swapoff or the firewalld binding.
func stubWorkerStartHost(t *testing.T) {
	t.Helper()
	oldPrereqs, oldTrust := hostPrereqs, hostTrustPodCIDRs
	t.Cleanup(func() { hostPrereqs, hostTrustPodCIDRs = oldPrereqs, oldTrust })
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	hostTrustPodCIDRs = func(string) {}
}

func TestWorkerRestartSavesGoThroughTheDataLock(t *testing.T) {
	const server = "https://10.0.0.1:6443"
	const podCIDR = "10.245.0.0/16"
	base := &NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8",
		KubeletNodeIP: "192.0.2.8", Server: server}

	t.Run("refuses while another operation holds the lock", func(t *testing.T) {
		workerScratchHost(t)
		if err := SaveState(base); err != nil {
			t.Fatal(err)
		}
		release, err := etcddata.Lock(version.EtcdDataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		stubWorkerStartHost(t)

		optOut := true
		if _, err := PrepareWorker(context.Background(), WorkerOptions{PodCIDR: podCIDR, SELinuxOptOut: &optOut}); err == nil {
			t.Fatal("the worker restart saved node.yaml while another operation held the data lock")
		}
		got, err := LoadState()
		if err != nil || got.SELinuxOptOut || got.PodCIDR != "" {
			t.Fatalf("node.yaml = %+v, %v; want it untouched while the lock was held", got, err)
		}
	})

	t.Run("keeps a field written while the restart ran", func(t *testing.T) {
		workerScratchHost(t)
		if err := SaveState(base); err != nil {
			t.Fatal(err)
		}
		stubWorkerStartHost(t)
		// A writer between the start's load and the save: the interleaving it must survive.
		hostPrereqs = func(host.PrereqOptions) error {
			cur, err := LoadState()
			if err != nil {
				return err
			}
			cur.ClusterDNSIP = "10.96.0.10"
			return SaveState(cur)
		}

		if _, err := PrepareWorker(context.Background(), WorkerOptions{PodCIDR: podCIDR}); err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}
		got, err := LoadState()
		if err != nil {
			t.Fatal(err)
		}
		if got.PodCIDR != podCIDR {
			t.Errorf("podCIDR = %q, want the restart's record", got.PodCIDR)
		}
		if got.ClusterDNSIP != "10.96.0.10" {
			t.Errorf("clusterDNSIP = %q, want the field the other writer recorded — the restart's save dropped it", got.ClusterDNSIP)
		}
	})
}

func TestWorkerRestartNodeIPSaveGoesThroughTheDataLock(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("lock held=%t", held), func(t *testing.T) {
			workerScratchHost(t)
			stubWorkerStartHost(t)
			saved := &NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8", KubeletNodeIP: "192.0.2.8"}
			if err := SaveState(saved); err != nil {
				t.Fatal(err)
			}
			if held {
				release, err := etcddata.Lock(version.EtcdDataDir)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			} else {
				hostPrereqs = func(host.PrereqOptions) error {
					return nodestate.Mutate(func(cur *NodeState) error {
						cur.ClusterDNSIP = "10.96.0.10"
						return nil
					})
				}
			}
			_, err := PrepareWorker(t.Context(), WorkerOptions{NodeIP: "192.0.2.9"})
			got := mustLoadState(t)
			if held {
				if err == nil || got.NodeIP != saved.NodeIP || got.KubeletNodeIP != saved.KubeletNodeIP {
					t.Fatalf("held lock: state = %+v, err = %v; want refusal without changing the address", got, err)
				}
			} else if err != nil || got.NodeIP != "192.0.2.9" || got.KubeletNodeIP != "192.0.2.9" || got.ClusterDNSIP != "10.96.0.10" {
				t.Fatalf("state = %+v, err = %v; want new address with the concurrent DNS update kept", got, err)
			}
		})
	}
}

// A re-join keeps the DNS address the learner recorded; the join's state lacks it.
func TestWorkerJoinStateSaveGoesThroughTheDataLock(t *testing.T) {
	const (
		server = "https://10.0.0.1:6443"
		tok    = "join-token"
		caHash = "sha256:aa"
	)
	ca := []byte("-----BEGIN CERTIFICATE-----\njoin CA\n-----END CERTIFICATE-----\n")

	t.Run("refuses while another operation holds the lock", func(t *testing.T) {
		workerScratchHost(t)
		stubJoinHost(t, "w1", "192.0.2.8", ca)
		recordTrust(t)
		release, err := etcddata.Lock(version.EtcdDataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash}); err == nil {
			t.Fatal("the worker join saved node.yaml while another operation held the data lock")
		}
		if _, err := os.Stat(nodestate.StatePath()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused join left node.yaml behind: %v", err)
		}
	})

	t.Run("a re-join keeps what it does not set", func(t *testing.T) {
		workerScratchHost(t)
		stubJoinHost(t, "w1", "192.0.2.8", ca)
		recordTrust(t)
		prev := &NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8",
			KubeletNodeIP: "192.0.2.8", Server: "https://10.9.9.9:6443",
			ClusterDNSIP: "10.96.0.10", PodCIDR: netdefault.PodCIDR}
		if err := SaveState(prev); err != nil {
			t.Fatal(err)
		}

		st, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash})
		if err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}
		got, err := LoadState()
		if err != nil {
			t.Fatal(err)
		}
		if got.Server != server {
			t.Errorf("server = %q, want the re-join's record", got.Server)
		}
		if got.ClusterDNSIP != "10.96.0.10" {
			t.Errorf("clusterDNSIP = %q, want the learned address kept — the join's save dropped it", got.ClusterDNSIP)
		}
		// RunWorker renders kubelet.conf from this state, so the saved resolver must win.
		if st.ClusterDNSIP != "10.96.0.10" {
			t.Errorf("returned clusterDNSIP = %q, want the learned address the save kept", st.ClusterDNSIP)
		}
	})
}

// The trust anchors the kubelet authenticates against must never be truncated.
func TestWorkerJoinWritesTheCABundles(t *testing.T) {
	const (
		server = "https://cp-8.example.test:6443"
		tok    = "join-token-8"
		caHash = "sha256:88"
	)
	ca := []byte("-----BEGIN CERTIFICATE-----\nthe discovered CA\n-----END CERTIFICATE-----\n")
	const previous = "the trust already on the node, from the previous start\n"

	// The same three paths the join writes, so the test follows a path move.
	caPaths := func() []string {
		return []string{
			filepath.Join(version.PKIDir, "ca.crt"),
			filepath.Join(version.PKIDir, "ca-bundle.crt"),
			pki.KubeletClientBundlePath(),
		}
	}

	t.Run("an induced failure truncates no trust file and leaves no stray temp", func(t *testing.T) {
		_, pkiDir := redirectJoinDirs(t)
		stubJoinHost(t, "w-8", "192.0.2.8", ca)
		recordTrust(t)
		for _, p := range caPaths() {
			if err := os.WriteFile(p, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// Fail at the fsync, after the temp file holds the bytes; a raw os.WriteFile bypasses this seam.
		oldWrite := writeTrustFile
		t.Cleanup(func() { writeTrustFile = oldWrite })
		writeTrustFile = func(path string, b []byte, perm os.FileMode) error {
			return atomicfile.WriteFileWith(path, b, perm, atomicfile.Seams{
				SyncFile: func(*os.File) error { return syscall.EFBIG },
			})
		}

		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash}); !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("PrepareWorker with a failing trust write = %v, want the injected error", err)
		}
		for _, p := range caPaths() {
			if got := mustRead(t, p); string(got) != previous {
				t.Errorf("%s holds %q after the failed write, want the trust already on the node — a truncated CA bundle breaks the kubelet's verification", p, got)
			}
		}
		entries, err := os.ReadDir(pkiDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 3 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("the failed join left %d entries in %s (%v), want only the three CA files — no stray temp file", len(entries), pkiDir, names)
		}
	})

	t.Run("a successful write keeps the mode and lands the discovered CA", func(t *testing.T) {
		_, pkiDir := redirectJoinDirs(t)
		stubJoinHost(t, "w-8", "192.0.2.8", ca)
		recordTrust(t)
		for _, p := range caPaths() {
			if err := os.WriteFile(p, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash}); err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}
		for _, p := range caPaths() {
			if got := mustRead(t, p); string(got) != string(ca) {
				t.Errorf("%s holds %q, want the discovered CA bytes", p, got)
			}
			if got := workerStatMode(t, p); got != 0o644 {
				t.Errorf("%s written as %v, want 0644 — the mode these writes have always used", p, got)
			}
		}
		entries, err := os.ReadDir(pkiDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 3 {
			t.Errorf("the join left %d entries in %s, want only the three CA files — no stray temp file", len(entries), pkiDir)
		}
	})
}

func workerStatMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}
