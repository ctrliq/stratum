package ha

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/tools/clientcmd"
)

// VIPServes reports whether the VIP serves this cluster's apiserver, as
// checkVIPServes does. Exported, and a var, so a test of a command that
// repoints, in this package or another, can stand in for a VIP that does or
// does not answer.
var VIPServes = checkVIPServes

// vipDialTimeout bounds the check that the VIP answers.
var vipDialTimeout = 5 * time.Second

// checkVIPServes reports whether this cluster's apiserver answers on the VIP:
// a TLS handshake with <vip>:6443 that trusts only the cluster CA this node's
// kubeconfigs carry and requires the certificate to cover the VIP. A repoint
// onto a VIP that fails it leaves every client of this node, or every Cilium
// agent, dialing an address that does not serve the cluster.
func checkVIPServes(vip string) error {
	ca, err := clusterCA()
	if err != nil {
		return err
	}
	return tlsServes(net.JoinHostPort(vip, strconv.Itoa(version.APIServerPort)), vip, ca)
}

// tlsServes performs the handshake checkVIPServes describes against addr,
// verifying the certificate for host against ca.
func tlsServes(addr, host string, ca *x509.CertPool) error {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: vipDialTimeout}, "tcp", addr, &tls.Config{
		RootCAs:    ca,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return err
	}
	return conn.Close()
}

// errNoClusterCA marks a check that could not start because no CA could be
// read from this node's kubeconfigs: a local problem, not the VIP's.
var errNoClusterCA = errors.New("no cluster CA in this node's kubeconfigs")

// clusterCA is the cluster CA from the first of this node's kubeconfigs that
// carries one: every Stratum node has at least the kubelet's. When none
// does, the error matches errNoClusterCA and carries why each kubeconfig
// present yielded no CA.
func clusterCA() (*x509.CertPool, error) {
	var loaded []string
	var problems []error
	for _, f := range repointKubeconfigs() {
		cfg, err := clientcmd.LoadFromFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", f, err))
			continue
		}
		loaded = append(loaded, f)
		for name, c := range cfg.Clusters {
			data := c.CertificateAuthorityData
			if len(data) == 0 && c.CertificateAuthority != "" {
				if data, err = os.ReadFile(c.CertificateAuthority); err != nil {
					problems = append(problems, fmt.Errorf("%s: cluster %s: read its certificate authority: %w", f, name, err))
					continue
				}
			}
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(data) {
				return pool, nil
			}
			problems = append(problems, fmt.Errorf("%s: cluster %s: its certificate authority holds no PEM certificate", f, name))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %w", errNoClusterCA, errors.Join(problems...))
	}
	return nil, fmt.Errorf("%w (read %v); is this a Stratum node?", errNoClusterCA, loaded)
}

// repointKubeconfigs are the kubeconfigs ha-repoint rewrites, those this node
// has and those it does not.
func repointKubeconfigs() []string {
	files := []string{version.AdminConf, version.SuperAdminConf(), version.BootstrapKubeconfig, version.SchedulerKubeconfig()}
	for _, f := range []string{"admin.kubeconfig", "controller-manager.kubeconfig",
		"scheduler.kubeconfig", "kubelet.kubeconfig"} {
		files = append(files, filepath.Join(version.KubeconfigDir, f))
	}
	return files
}

// vipCheckFailed is the refusal for a VIPServes error. One that never reached
// the VIP, because this node's cluster CA could not be read, says so and no
// more: the steps that put a VIP in place would not fix it.
func vipCheckFailed(vip string, err error) error {
	if errors.Is(err, errNoClusterCA) {
		return fmt.Errorf("ha-repoint: cannot check that the VIP %s serves this cluster, so nothing was changed: %w", vip, err)
	}
	return vipNotServing(vip, err)
}

// vipNotServing is the refusal when the VIP does not answer as the cluster's
// apiserver: what is missing, and how to put it in place first.
func vipNotServing(vip string, err error) error {
	return fmt.Errorf("ha-repoint: the VIP %s does not answer as this cluster's apiserver on port %d: %w\n"+
		"Nothing was changed. Before repointing onto a VIP:\n%s",
		vip, version.APIServerPort, err, vipBringUpSteps(vip))
}

// vipBringUpSteps are the steps that put the VIP in place on a cluster whose
// controllers do not serve it yet, one per line. ha-repoint's refusal and the
// HA join's both give them, so the two cannot tell an operator different
// things.
func vipBringUpSteps(vip string) string {
	return fmt.Sprintf("  1. set spec.vip.address: %s in /etc/stratum/config.yaml on every controller;\n"+
		"  2. restart Stratum on each controller (sudo systemctl restart stratum), which starts kube-vip and reissues the apiserver certificate with the VIP;\n"+
		"  3. check that the VIP answers: curl --cacert /var/lib/stratum/pki/ca.crt https://%s/healthz",
		vip, net.JoinHostPort(vip, strconv.Itoa(version.APIServerPort)))
}
