// Package kubeconfig writes the control-plane kubeconfigs with client-go's clientcmd/api.
package kubeconfig

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Entry describes one kubeconfig to write.
type Entry struct {
	File     string // basename under version.KubeconfigDir, or an absolute path
	User     string // context user name
	CertName string // PKI basename for the client cert/key (e.g. "admin")
}

// Embeds the CA trust bundle (old‖new during a rotation), falling back to ca.crt.
func Write(e Entry, server string) error {
	ca, _, err := pki.ReadRootTrust()
	if err != nil {
		return err
	}
	crt, err := os.ReadFile(filepath.Join(version.PKIDir, e.CertName+".crt"))
	if err != nil {
		return fmt.Errorf("read %s.crt: %w", e.CertName, err)
	}
	key, err := os.ReadFile(filepath.Join(version.PKIDir, e.CertName+".key"))
	if err != nil {
		return fmt.Errorf("read %s.key: %w", e.CertName, err)
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[version.ClusterName] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: ca,
	}
	cfg.AuthInfos[e.User] = &clientcmdapi.AuthInfo{
		ClientCertificateData: crt,
		ClientKeyData:         key,
	}
	cfg.Contexts["default"] = &clientcmdapi.Context{
		Cluster:  version.ClusterName,
		AuthInfo: e.User,
	}
	cfg.CurrentContext = "default"

	out := e.File
	if !filepath.IsAbs(out) {
		out = filepath.Join(version.KubeconfigDir, out)
	}
	if err := os.MkdirAll(version.KubeconfigDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	// clientcmd.WriteToFile truncates the file in place; a truncated kubeconfig stops its component.
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(out, b, 0o600)
}

// Cert rotation uses this to keep the endpoint ha-repoint recorded (the per-node LB).
func CurrentServer(file string) (string, error) {
	cfg, err := clientcmd.LoadFromFile(file)
	if err != nil {
		return "", err
	}
	if c, ok := cfg.Clusters[version.ClusterName]; ok && c.Server != "" {
		return c.Server, nil
	}
	for _, c := range cfg.Clusters {
		if c.Server != "" {
			return c.Server, nil
		}
	}
	return "", fmt.Errorf("kubeconfig %s has no cluster server URL", file)
}

// Returns bytes so a caller can stage several kubeconfigs before committing any.
func ReadWithClusterCA(file string, ca []byte) ([]byte, error) {
	cfg, err := clientcmd.LoadFromFile(file)
	if err != nil {
		return nil, err
	}
	for _, c := range cfg.Clusters {
		c.CertificateAuthorityData = ca
		c.CertificateAuthority = ""
	}
	return clientcmd.Write(*cfg)
}

var kubeconfigLeaves = []string{"admin", "controller-manager", "kubelet"}

func entryForLeaf(leaf, host string) (Entry, bool) {
	for _, s := range pki.LeafSpecs(host, "", nil) {
		if s.Name != leaf {
			continue
		}
		return Entry{File: s.Name + ".kubeconfig", User: s.CN, CertName: s.Name}, true
	}
	return Entry{}, false
}

func NodeServerURL(server, vip string) string {
	if server != "" {
		return server
	}
	if vip != "" {
		return "https://" + net.JoinHostPort(vip, strconv.Itoa(version.APIServerPort))
	}
	return fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
}

// kube-vip's kubeconfig targets the local apiserver, never the VIP it manages.
func GenerateAll(host, server string, vip bool) error {
	var entries []Entry
	for _, leaf := range kubeconfigLeaves {
		e, ok := entryForLeaf(leaf, host)
		if !ok {
			return fmt.Errorf("pki has no %s leaf: the %s kubeconfig has no client certificate to embed", leaf, leaf)
		}
		entries = append(entries, e)
	}
	if leafIssued("super-admin") {
		entries = append(entries, Entry{File: version.SuperAdminConf(), User: "super-admin", CertName: "super-admin"})
	}
	for _, e := range entries {
		if err := Write(e, server); err != nil {
			return fmt.Errorf("kubeconfig %s: %w", e.File, err)
		}
	}
	if err := WriteScheduler(server); err != nil {
		return err
	}
	if vip {
		if err := WriteKubeVip(); err != nil {
			return err
		}
	}
	// admin.conf is the operator's --kubeconfig, so the copy goes through the atomic writer too.
	if err := os.MkdirAll(version.K8sConfDir, 0o755); err != nil {
		return err
	}
	src := filepath.Join(version.KubeconfigDir, "admin.kubeconfig")
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(version.AdminConf, data, 0o600)
}

func leafIssued(name string) bool {
	for _, ext := range []string{".crt", ".key"} {
		if _, err := os.Stat(filepath.Join(version.PKIDir, name+ext)); err != nil {
			return false
		}
	}
	return true
}

// legacyKubeVipKubeconfig is the path older kube-vip manifests name; a rollback needs it.
func legacyKubeVipKubeconfig() string {
	return filepath.Join(version.KubeconfigDir, "kube-vip.kubeconfig")
}

func WriteKubeVip() error {
	server := fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
	for _, f := range []string{version.KubeVipKubeconfig(), legacyKubeVipKubeconfig()} {
		if err := Write(Entry{File: f, User: "system:kube-vip", CertName: "kube-vip"}, server); err != nil {
			return fmt.Errorf("kubeconfig %s: %w", f, err)
		}
	}
	return nil
}

// The path older kube-scheduler manifests name; a rollback needs it.
func legacySchedulerKubeconfig() string {
	return filepath.Join(version.KubeconfigDir, "scheduler.kubeconfig")
}

func WriteScheduler(server string) error {
	for _, f := range []string{version.SchedulerKubeconfig(), legacySchedulerKubeconfig()} {
		if err := Write(Entry{File: f, User: "system:kube-scheduler", CertName: "scheduler"}, server); err != nil {
			return fmt.Errorf("kubeconfig %s: %w", f, err)
		}
	}
	return nil
}

// Writes both paths against the server the kubeconfig at the legacy path names.
func RefreshScheduler() error {
	server, err := CurrentServer(legacySchedulerKubeconfig())
	if err != nil {
		return fmt.Errorf("read kube-scheduler's server: %w", err)
	}
	return WriteScheduler(server)
}

// Atomic and 0600: the kubelet authenticates with this file, and a truncated one stops a worker.
func SetServer(file, server string) error {
	cfg, err := clientcmd.LoadFromFile(file)
	if err != nil {
		return err
	}
	for _, c := range cfg.Clusters {
		c.Server = server
	}
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(file, b, 0o600)
}
