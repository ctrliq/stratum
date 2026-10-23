package kubeconfig

import (
	"bytes"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// useTempDirs points the kubeconfig and PKI paths at a fresh temp tree with a
// generated PKI, for one test.
func useTempDirs(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatalf("pki.GenerateAll: %v", err)
	}
}

func TestGenerateAll(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")

	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	const server = "https://127.0.0.1:6443"
	if err := GenerateAll("cp1", server, false); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{"admin", "controller-manager", "scheduler", "kubelet"} {
		if _, err := os.Stat(filepath.Join(version.KubeconfigDir, f+".kubeconfig")); err != nil {
			t.Errorf("missing %s.kubeconfig: %v", f, err)
		}
	}

	// admin.conf must be a usable kubeconfig with embedded creds + CA.
	cfg, err := clientcmd.LoadFromFile(version.AdminConf)
	if err != nil {
		t.Fatalf("load admin.conf: %v", err)
	}
	cl, ok := cfg.Clusters[version.ClusterName]
	if !ok {
		t.Fatalf("admin.conf missing cluster %q", version.ClusterName)
	}
	if cl.Server != server {
		t.Errorf("server = %q, want %q", cl.Server, server)
	}
	if len(cl.CertificateAuthorityData) == 0 {
		t.Error("admin.conf has no embedded CA data")
	}
	ai, ok := cfg.AuthInfos["admin"]
	if !ok {
		t.Fatal("admin.conf missing admin authinfo")
	}
	if len(ai.ClientCertificateData) == 0 || len(ai.ClientKeyData) == 0 {
		t.Error("admin.conf authinfo missing embedded client cert/key")
	}
}

func TestKubeconfigPermsRestrictive(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatal(err)
	}
	// kubeconfigs embed private keys, so they must not be world/group readable.
	fi, err := os.Stat(filepath.Join(version.KubeconfigDir, "admin.kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("admin.kubeconfig perms = %o, want 600", fi.Mode().Perm())
	}
}

// TestGenerateAllWithoutSuperAdminLeaf pins that a control plane an earlier
// release started, with no super-admin leaf yet, still regenerates its
// kubeconfigs, as CA rotation's trust-new does, and gets no super-admin.conf.
func TestGenerateAllWithoutSuperAdminLeaf(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".crt", ".key"} {
		if err := os.Remove(filepath.Join(version.PKIDir, "super-admin"+ext)); err != nil {
			t.Fatal(err)
		}
	}
	if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatalf("GenerateAll without a super-admin leaf: %v", err)
	}
	if _, err := os.Stat(version.AdminConf); err != nil {
		t.Errorf("admin.conf: %v", err)
	}
	if _, err := os.Stat(version.SuperAdminConf()); !os.IsNotExist(err) {
		t.Errorf("super-admin.conf: err %v, want none without its leaf", err)
	}
}

// TestGenerateAllSuperAdmin pins that super-admin.conf is written 0600 beside
// admin.conf, against the same server, carrying the super-admin leaf, and that
// admin.conf carries the admin leaf rather than it.
func TestGenerateAllSuperAdmin(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	const server = "https://10.0.0.100:6443"
	if err := GenerateAll("cp1", server, false); err != nil {
		t.Fatal(err)
	}
	for conf, user := range map[string]string{version.SuperAdminConf(): "super-admin", version.AdminConf: "admin"} {
		cfg, err := clientcmd.LoadFromFile(conf)
		if err != nil {
			t.Fatalf("load %s: %v", conf, err)
		}
		if got := cfg.Clusters[version.ClusterName].Server; got != server {
			t.Errorf("%s server = %q, want %q", conf, got, server)
		}
		ai := cfg.AuthInfos[user]
		if ai == nil {
			t.Fatalf("%s has no %s user", conf, user)
		}
		crt, err := os.ReadFile(filepath.Join(version.PKIDir, user+".crt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(ai.ClientCertificateData) != string(crt) {
			t.Errorf("%s does not embed the %s leaf", conf, user)
		}
		fi, err := os.Stat(conf)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perms = %o, want 600", conf, fi.Mode().Perm())
		}
	}
}

// TestWriteEmbedsBundle: Write embeds the (possibly multi-cert) ca-bundle.crt so
// a kubeconfig trusts old‖new across a CA rotation, and falls back to ca.crt when
// no bundle exists (clusters predating the bundle files).
func TestWriteEmbedsBundle(t *testing.T) {
	countCerts := func(pemBytes []byte) int {
		n := 0
		rest := pemBytes
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			n++
		}
		return n
	}
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	// stage a CA rotation → ca-bundle.crt now has old‖new
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.LoadFromFile(version.AdminConf)
	if err != nil {
		t.Fatal(err)
	}
	if got := countCerts(cfg.Clusters[version.ClusterName].CertificateAuthorityData); got != 2 {
		t.Errorf("kubeconfig embeds %d CA certs during rotation, want 2 (bundle)", got)
	}

	// fallback: no bundle → embed the single ca.crt
	if err := os.Remove(filepath.Join(version.PKIDir, "ca-bundle.crt")); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatalf("GenerateAll without bundle (fallback): %v", err)
	}
	cfg, _ = clientcmd.LoadFromFile(version.AdminConf)
	if got := countCerts(cfg.Clusters[version.ClusterName].CertificateAuthorityData); got != 1 {
		t.Errorf("fallback kubeconfig embeds %d CA certs, want 1 (ca.crt)", got)
	}
}

// TestReadWithClusterCA rewrites the embedded CA data (the worker-trust refresh path)
// while preserving the server endpoint, and leaves the original file untouched —
// the caller stages the returned bytes and commits them itself.
func TestReadWithClusterCA(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	const lb = "https://127.0.0.1:7445"
	if err := GenerateAll("cp1", lb, false); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig")
	before, err := os.ReadFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	newCA := []byte("-----BEGIN CERTIFICATE-----\nCA-A\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nCA-B\n-----END CERTIFICATE-----\n")
	got, err := ReadWithClusterCA(kc, newCA)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(got)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Clusters[version.ClusterName]
	if string(c.CertificateAuthorityData) != string(newCA) {
		t.Error("ReadWithClusterCA did not replace the embedded CA data")
	}
	if c.Server != lb {
		t.Errorf("ReadWithClusterCA changed the server endpoint: %q, want %q", c.Server, lb)
	}
	// Returning bytes must not be a write: the atomic-commit caller relies on it.
	after, err := os.ReadFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("ReadWithClusterCA modified the file on disk; it must only return bytes")
	}
}

// TestCurrentServer round-trips the endpoint — the guard that cert rotation
// preserves a post-ha-repoint LB endpoint instead of reverting to :6443.
func TestCurrentServer(t *testing.T) {
	version.PKIDir = t.TempDir()
	version.KubeconfigDir = t.TempDir()
	version.K8sConfDir = t.TempDir()
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	const lb = "https://127.0.0.1:7445"
	if err := GenerateAll("cp1", lb, false); err != nil {
		t.Fatal(err)
	}
	got, err := CurrentServer(version.AdminConf)
	if err != nil {
		t.Fatal(err)
	}
	if got != lb {
		t.Errorf("CurrentServer=%q, want %q", got, lb)
	}
}

// TestGenerateAllKubeVIP pins that kube-vip.kubeconfig is written by
// GenerateAll on a VIP node, so every caller that rewrites the others (CA
// rotation, leaf rotation) rewrites it too. It targets the local apiserver even
// when the others target the VIP, and carries the current kube-vip leaf and
// trust bundle.
func TestGenerateAllKubeVIP(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	kv := version.KubeVipKubeconfig()

	if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kv); !os.IsNotExist(err) {
		t.Fatalf("no-VIP node got kube-vip.kubeconfig (stat err %v)", err)
	}

	old := filepath.Join(version.KubeconfigDir, "kube-vip.kubeconfig")
	if err := os.WriteFile(old, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	const vipServer = "https://10.0.0.100:6443"
	if err := GenerateAll("cp1", vipServer, true); err != nil {
		t.Fatal(err)
	}
	legacy, err := clientcmd.LoadFromFile(old)
	if err != nil {
		t.Fatalf("the legacy kube-vip.kubeconfig a rollback's manifest names is gone: %v", err)
	}
	if legacy.AuthInfos["system:kube-vip"] == nil || legacy.Clusters[version.ClusterName].Server != "https://127.0.0.1:6443" {
		t.Error("the legacy kube-vip.kubeconfig is not the current kube-vip config")
	}
	cfg, err := clientcmd.LoadFromFile(kv)
	if err != nil {
		t.Fatalf("load kube-vip.kubeconfig: %v", err)
	}
	if got := cfg.Clusters[version.ClusterName].Server; got != "https://127.0.0.1:6443" {
		t.Errorf("kube-vip server = %q, want the local apiserver, never the VIP it manages", got)
	}
	ai := cfg.AuthInfos["system:kube-vip"]
	if ai == nil {
		t.Fatal("kube-vip.kubeconfig has no system:kube-vip user")
	}
	crt, err := os.ReadFile(filepath.Join(version.PKIDir, "kube-vip.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ai.ClientCertificateData) != string(crt) {
		t.Error("kube-vip.kubeconfig does not embed the current kube-vip leaf")
	}
	fi, err := os.Stat(kv)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("kube-vip.kubeconfig perms = %o, want 600", fi.Mode().Perm())
	}
}

// TestGenerateAllScheduler pins that the scheduler's kubeconfig is written,
// against the server the others carry, in its own directory and at the legacy
// path an older kube-scheduler manifest names, and that RefreshScheduler
// rewrites both from the server at the legacy path.
func TestGenerateAllScheduler(t *testing.T) {
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(version.SchedulerKubeconfig()) == version.KubeconfigDir {
		t.Fatalf("the scheduler's kubeconfig sits in %s beside the others", version.KubeconfigDir)
	}
	legacy := filepath.Join(version.KubeconfigDir, "scheduler.kubeconfig")
	serverOf := func(t *testing.T, f string) string {
		t.Helper()
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			t.Fatalf("load %s: %v", f, err)
		}
		if cfg.AuthInfos["system:kube-scheduler"] == nil {
			t.Errorf("%s has no system:kube-scheduler user", f)
		}
		return cfg.Clusters[version.ClusterName].Server
	}

	const vipServer = "https://10.0.0.100:6443"
	if err := GenerateAll("cp1", vipServer, false); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{version.SchedulerKubeconfig(), legacy} {
		if got := serverOf(t, f); got != vipServer {
			t.Errorf("%s server = %q, want %q", f, got, vipServer)
		}
	}

	if err := os.Remove(version.SchedulerKubeconfig()); err != nil {
		t.Fatal(err)
	}
	if err := RefreshScheduler(); err != nil {
		t.Fatal(err)
	}
	if got := serverOf(t, version.SchedulerKubeconfig()); got != vipServer {
		t.Errorf("refreshed scheduler kubeconfig server = %q, want the legacy file's %q", got, vipServer)
	}

	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if err := RefreshScheduler(); err == nil {
		t.Error("RefreshScheduler with no legacy kubeconfig to read its server from succeeded")
	}
}

// TestWriteReplacesTheFileNotTruncate: Write renders the kubeconfig with
// clientcmd.Write and replaces the file atomically — a reader holding the old
// file (the hard link below) keeps the old bytes while the file at path gains
// the new server. clientcmd.WriteToFile writes in place with no temp file, so
// a crash mid-write leaves a truncated kubeconfig: on a worker that is a
// kubelet.kubeconfig that stops the kubelet from starting at all — the
// hazard the package's own comment names.
func TestWriteReplacesTheFileNotTruncate(t *testing.T) {
	useTempDirs(t)
	e := Entry{File: "admin.kubeconfig", User: "admin", CertName: "admin"}
	kc := filepath.Join(version.KubeconfigDir, e.File)
	if err := Write(e, "https://127.0.0.1:6443"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	reader := kc + ".reader"
	if err := os.Link(kc, reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(reader) })

	if err := Write(e, "https://10.0.0.9:6443"); err != nil {
		t.Fatal(err)
	}

	cfg, err := clientcmd.LoadFromFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[version.ClusterName].Server; got != "https://10.0.0.9:6443" {
		t.Errorf("server = %q, want the new endpoint", got)
	}
	if b, err := os.ReadFile(reader); err != nil || !bytes.Equal(b, before) {
		t.Errorf("the rewrite truncated the kubeconfig in place: the linked reader of the old file holds the new bytes (err %v)", err)
	}
	if fi, err := os.Stat(kc); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig perms = %o, want 600", fi.Mode().Perm())
	}
}

// TestSetServerReplacesTheFileNotTruncate: SetServer — the HA repoint
// primitive — replaces the kubeconfig atomically and keeps its 0600 mode: it
// rewrites the file the kubelet authenticates with, and a truncated one stops
// a worker nobody is logged in to.
func TestSetServerReplacesTheFileNotTruncate(t *testing.T) {
	useTempDirs(t)
	if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(version.KubeconfigDir, "admin.kubeconfig")
	before, err := os.ReadFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	reader := kc + ".reader"
	if err := os.Link(kc, reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(reader) })

	const lb = "https://127.0.0.1:7445"
	if err := SetServer(kc, lb); err != nil {
		t.Fatal(err)
	}

	cfg, err := clientcmd.LoadFromFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[version.ClusterName].Server; got != lb {
		t.Errorf("server = %q, want %q", got, lb)
	}
	if b, err := os.ReadFile(reader); err != nil || !bytes.Equal(b, before) {
		t.Errorf("the repoint truncated the kubeconfig in place: the linked reader of the old file holds the new bytes (err %v)", err)
	}
	if fi, err := os.Stat(kc); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig perms after SetServer = %o, want 600", fi.Mode().Perm())
	}
}

// TestWriteFailureLeavesNoScratch: a Write that fails (the destination
// occupied by a directory here) leaves the directory as it found it — no temp
// copy of the kubeconfig beside it.
func TestWriteFailureLeavesNoScratch(t *testing.T) {
	useTempDirs(t)
	occupied := filepath.Join(version.KubeconfigDir, "admin.kubeconfig")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(Entry{File: "admin.kubeconfig", User: "admin", CertName: "admin"}, "https://127.0.0.1:6443"); err == nil {
		t.Fatal("Write onto a directory succeeded")
	}
	entries, err := os.ReadDir(version.KubeconfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "admin.kubeconfig" || !entries[0].IsDir() {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("KubeconfigDir holds %v, want only the directory — the failed write left a temp file", names)
	}
}

// TestGenerateAllAdminConfCopyIsAtomic covers the admin.conf copy that closes
// GenerateAll: it is the operator's --kubeconfig and kubectl's on a control
// plane, so it is written like every kubeconfig above it. The copy replaces
// the file (a reader holding the old one keeps the old bytes) at 0600, and a
// copy that fails (the destination occupied by a directory here) leaves no
// temp copy beside it.
func TestGenerateAllAdminConfCopyIsAtomic(t *testing.T) {
	t.Run("replaces the file, not truncates, at 0600", func(t *testing.T) {
		useTempDirs(t)
		if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(version.AdminConf)
		if err != nil {
			t.Fatal(err)
		}
		reader := version.AdminConf + ".reader"
		if err := os.Link(version.AdminConf, reader); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(reader) })

		// A second GenerateAll against a different endpoint rewrites
		// admin.kubeconfig, so the copy's bytes differ and the test can
		// tell a replace from a truncate.
		if err := GenerateAll("cp1", "https://10.0.0.9:6443", false); err != nil {
			t.Fatal(err)
		}

		cfg, err := clientcmd.LoadFromFile(version.AdminConf)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Clusters[version.ClusterName].Server; got != "https://10.0.0.9:6443" {
			t.Errorf("admin.conf server = %q, want the new endpoint", got)
		}
		if b, err := os.ReadFile(reader); err != nil || !bytes.Equal(b, before) {
			t.Errorf("the copy rewrote admin.conf in place: the linked reader of the old file holds the new bytes (err %v)", err)
		}
		if fi, err := os.Stat(version.AdminConf); err != nil {
			t.Fatal(err)
		} else if fi.Mode().Perm() != 0o600 {
			t.Errorf("admin.conf perms = %o, want 600", fi.Mode().Perm())
		}
	})

	t.Run("a failed copy leaves no temp beside it", func(t *testing.T) {
		useTempDirs(t)
		if err := os.MkdirAll(version.AdminConf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := GenerateAll("cp1", "https://127.0.0.1:6443", false); err == nil {
			t.Fatal("GenerateAll copied admin.conf over a directory")
		}
		entries, err := os.ReadDir(version.K8sConfDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "admin.conf" || !entries[0].IsDir() {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("the state dir holds %v, want only the directory — the failed copy left a temp file", names)
		}
	})
}

func TestNodeServerURL(t *testing.T) {
	for _, tc := range []struct {
		name, server, vip, want string
	}{
		{"a record without a VIP derives the local apiserver", "", "", "https://127.0.0.1:6443"},
		{"a record without a recorded endpoint derives the VIP", "", "10.0.0.10", "https://10.0.0.10:6443"},
		{"a recorded endpoint wins over the VIP", "https://192.168.99.240:6443", "10.0.0.10", "https://192.168.99.240:6443"},
		{"a recorded endpoint wins without a VIP", "https://127.0.0.1:7445", "", "https://127.0.0.1:7445"},
		// An IPv6 VIP must be bracketed: unbracketed, url.Parse reads the
		// address's last groups as the port and every client refuses it.
		{"an IPv6 VIP is bracketed", "", "fd00::10", "https://[fd00::10]:6443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NodeServerURL(tc.server, tc.vip)
			if got != tc.want {
				t.Errorf("NodeServerURL(%q, %q) = %q, want %q", tc.server, tc.vip, got, tc.want)
			}
			if _, err := url.Parse(got); err != nil {
				t.Errorf("NodeServerURL(%q, %q) = %q, which no client can parse: %v", tc.server, tc.vip, got, err)
			}
		})
	}
}
