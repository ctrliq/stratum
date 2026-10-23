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

// clusterInfoPath is the kube-public cluster-info ConfigMap, shared by the
// initial-join fetch and the already-joined verified fetch.
const clusterInfoPath = "/api/v1/namespaces/kube-public/configmaps/cluster-info"

// maxClusterInfoBytes is defined once in bootstrap.go and shared: the same cap
// protects both fetches, and two copies is how one of them silently stops
// matching the other.

// FetchClusterCAVerified fetches the cluster-info CA trust bundle over a TLS
// connection VERIFIED against roots (an already-joined node's on-disk trust
// bundle), and returns the fetched bundle together with the verified peer chain
// so the caller can check the bundle against the connection it arrived on.
//
// Like DiscoverCA this sends no credential, and deliberately so — the two differ
// in what they authenticate, not in what they present. The cluster-info ConfigMap is
// granted to system:anonymous alone (token.ensureClusterInfo binds the
// stratum:cluster-info Role to exactly that one subject), and the apiserver runs
// --authorization-mode=Node,RBAC: presenting the kubelet's client certificate
// would authenticate the request as system:node:<host>, which has no grant on
// that ConfigMap and no Node-authorizer path to it, turning a request RBAC
// permits into a 403. Anonymous is both the working answer and the least
// privileged one for reading a ConfigMap published for anonymous read.
//
// Dropping InsecureSkipVerify enables TWO checks, not one: chain validation and
// SAN/hostname validation. An endpoint outside the apiserver leaf's SANs
// fails closed on purpose — such a mismatch is a real misconfiguration, and
// this path rewrites the file that decides who is
// cluster-admin on the kubelet. classifyFetchError keeps that case distinguishable
// from a trust failure.
func FetchClusterCAVerified(server string, roots []byte) ([]byte, []*x509.Certificate, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		return nil, nil, fmt.Errorf("cluster-info: on-disk trust bundle holds no usable certificate, so there is nothing to verify the apiserver against")
	}
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

// caFromClusterInfo pulls the CA bundle out of a cluster-info ConfigMap body.
// The reading rule is clusterinfo.Parse's, shared with TrustBundle and RepinCA:
// an ambiguous ConfigMap is refused rather than answered by whichever cluster
// entry the map happened to iterate first — these bytes become the node's trust
// store, so guessing between two differing bundles is not an acceptable outcome.
func caFromClusterInfo(body []byte) ([]byte, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse cluster-info ConfigMap: %w", err)
	}
	// clusterinfo.KubeConfigKey rather than a local "kubeconfig" literal: the
	// authenticated path keys off the same constant, and the two must not drift.
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

// classifyFetchError turns a TLS failure into a self-contained diagnosis. The
// cases call for opposite responses — repoint the endpoint, or treat the peer as
// hostile — and this string is the only record of which one happened: it reaches
// the operator through the failed carotate Job's pod log, which nodejob.Run
// deliberately retains (it deletes the Job only on success).
//
// The SAN case cannot be inferred from the error type alone. x509.Verify checks
// the hostname BEFORE it builds chains, so a HostnameError comes back with chain
// building never attempted and says nothing about whether the peer is trusted.
// Trust is therefore established separately, against the same roots, rather than
// assumed from which error surfaced first.
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

// peerChainsTo reports whether the certificates the failed handshake rejected
// would chain to roots on their own. tls.CertificateVerificationError carries
// them, so trust can be judged independently of which x509 error surfaced first.
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
