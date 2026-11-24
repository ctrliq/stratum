package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
)

const clusterInfoPath = "/api/v1/namespaces/kube-public/configmaps/cluster-info"

func FetchClusterCAVerified(server string, roots []byte) ([]byte, []*x509.Certificate, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		return nil, nil, fmt.Errorf("cluster-info: on-disk trust bundle holds no usable certificate, so there is nothing to verify the apiserver against")
	}
	// No client credential: this ConfigMap is anonymous-only; a kubelet cert would get a 403.
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}},
	}
	resp, err := client.Get(server + clusterInfoPath)
	if err != nil {
		return nil, nil, classifyFetchError(server, pool, err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil {
		return nil, nil, fmt.Errorf("cluster-info: %s is not an HTTPS endpoint, so the apiserver was never authenticated", server)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("cluster-info: fetch from %s: HTTP %d", server, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClusterInfoBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("cluster-info: read response from %s: %w", server, err)
	}
	if len(body) > maxClusterInfoBytes {
		return nil, nil, fmt.Errorf("cluster-info: response from %s exceeds %d bytes, which is not a cluster-info ConfigMap", server, maxClusterInfoBytes)
	}
	ca, err := caFromClusterInfo(body)
	if err != nil {
		return nil, nil, err
	}
	return ca, resp.TLS.PeerCertificates, nil
}

// Reads with clusterinfo.Parse: an ambiguous ConfigMap is refused, never answered by map order.
func caFromClusterInfo(body []byte) ([]byte, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse cluster-info ConfigMap: %w", err)
	}
	// The authenticated path keys off the same constant; the two must not drift.
	raw, ok := cm.Data[clusterinfo.KubeConfigKey]
	if !ok || raw == "" {
		return nil, fmt.Errorf("cluster-info ConfigMap missing data.%s", clusterinfo.KubeConfigKey)
	}
	_, ca, err := clusterinfo.Parse([]byte(raw))
	if err != nil {
		return nil, err
	}
	if len(ca) == 0 {
		return nil, fmt.Errorf("cluster-info has no CA")
	}
	return ca, nil
}

// x509.Verify checks the hostname before chains, so HostnameError says nothing about trust.
func classifyFetchError(server string, roots *x509.CertPool, err error) error {
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		if !peerChainsTo(roots, err) {
			return fmt.Errorf("cluster-info: the apiserver at %s presented a certificate that is neither trusted by this node nor valid for that endpoint — treat this as a trust failure (possible MITM), not as a SAN mismatch: %w", server, err)
		}
		return fmt.Errorf("cluster-info: the apiserver at %s presented a TRUSTED certificate that is not valid for that endpoint (SAN mismatch, not a trust failure): the cert covers DNS %v / IPs %v — the fix is to point this node's kubeconfig endpoint at a name or address the cert already covers (the cert's SAN set is fixed at issue time and stratum has no supported way to widen it): %w",
			server, hostErr.Certificate.DNSNames, hostErr.Certificate.IPAddresses, err)
	}
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return fmt.Errorf("cluster-info: the apiserver at %s presented a certificate signed by a CA this node does not trust (possible MITM, or this node missed the trust-new phase): %w", server, err)
	}
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &invalidErr) {
		return fmt.Errorf("cluster-info: the apiserver certificate at %s failed chain validation against this node's on-disk trust bundle: %w", server, err)
	}
	return fmt.Errorf("cluster-info: fetch from %s: %w", server, err)
}

func peerChainsTo(roots *x509.CertPool, err error) bool {
	var cve *tls.CertificateVerificationError
	if !errors.As(err, &cve) || len(cve.UnverifiedCertificates) == 0 {
		return false
	}
	intermediates := x509.NewCertPool()
	for _, c := range cve.UnverifiedCertificates[1:] {
		intermediates.AddCert(c)
	}
	_, verifyErr := cve.UnverifiedCertificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return verifyErr == nil
}
