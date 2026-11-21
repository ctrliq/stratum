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

// VIPServes is a var so tests here and in other packages can stand in for the check.
var VIPServes = checkVIPServes

var vipDialTimeout = 5 * time.Second

// A repoint onto a VIP that fails this leaves clients dialing an address that serves nothing.
func checkVIPServes(vip string) error {
	ca, err := clusterCA()
	if err != nil {
		return err
	}
	return tlsServes(net.JoinHostPort(vip, strconv.Itoa(version.APIServerPort)), vip, ca)
}

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

var errNoClusterCA = errors.New("no cluster CA in this node's kubeconfigs")

// Every Stratum node carries at least the kubelet's kubeconfig.
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

func repointKubeconfigs() []string {
	files := []string{version.AdminConf, version.SuperAdminConf(), version.BootstrapKubeconfig, version.SchedulerKubeconfig()}
	for _, f := range []string{"admin.kubeconfig", "controller-manager.kubeconfig",
		"scheduler.kubeconfig", "kubelet.kubeconfig"} {
		files = append(files, filepath.Join(version.KubeconfigDir, f))
	}
	return files
}

// The VIP set-up steps would not fix a local CA failure, so it gets its own refusal.
func vipCheckFailed(vip string, err error) error {
	if errors.Is(err, errNoClusterCA) {
		return fmt.Errorf("ha-repoint: cannot check that the VIP %s serves this cluster, so nothing was changed: %w", vip, err)
	}
	return vipNotServing(vip, err)
}

func vipNotServing(vip string, err error) error {
	return fmt.Errorf("ha-repoint: the VIP %s does not answer as this cluster's apiserver on port %d: %w\n"+
		"Nothing was changed. Before repointing onto a VIP:\n%s",
		vip, version.APIServerPort, err, vipBringUpSteps(vip))
}

// Shared so the ha-repoint refusal and the HA join cannot give different steps.
func vipBringUpSteps(vip string) string {
	return fmt.Sprintf("  1. set spec.vip.address: %s in /etc/stratum/config.yaml on every controller;\n"+
		"  2. restart Stratum on each controller (sudo systemctl restart stratum), which starts kube-vip and reissues the apiserver certificate with the VIP;\n"+
		"  3. check that the VIP answers: curl --cacert /var/lib/stratum/pki/ca.crt https://%s/healthz",
		vip, net.JoinHostPort(vip, strconv.Itoa(version.APIServerPort)))
}
