package pki

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// Bounds the whole handshake attempt; the probe runs inside interactive checks.
const probeTimeout = 5 * time.Second

// A handshake, not the bundle: a failed restart leaves it saying migrated; error is indeterminate.
func EtcdRejectsRootSignedClient() (bool, error) {
	return EtcdRejectsRootSignedClientAt(net.JoinHostPort("127.0.0.1", fmt.Sprint(version.EtcdClientPort)))
}

// Against an explicit address ("<nodeIP>:2379"), so tests and the orchestrator can probe a peer.
func EtcdRejectsRootSignedClientAt(addr string) (bool, error) {
	cert, err := mintRootSignedClientCert()
	if err != nil {
		return false, err
	}
	// GetClientCertificate, not Certificates: TLS 1.2 filters Certificates against AcceptableCAs.
	var peerOK bool
	cfg := &tls.Config{
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert, nil
		},
		InsecureSkipVerify: true, //nolint:gosec // deliberate: peer identity is checked below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			peerOK = peerIsEtcdAt(addr, rawCerts)
			return nil // probing our cert, not judging theirs
		},
		// TLS 1.2 so an untrusted client cert is refused in-handshake; TLS 1.3 hides the alert.
		MaxVersion: tls.VersionTLS12,
	}
	conn, err := dialProbeTLS(addr, cfg)
	if err != nil {
		if !peerOK {
			return false, fmt.Errorf("probe %s: the listener did not present this node's etcd certificate, so its answer proves nothing: %w", addr, err)
		}
		if isCertRejection(err) {
			return true, nil // etcd refused the root-signed cert: the etcd trust hole closed here
		}
		return false, fmt.Errorf("probe etcd at %s: %w", addr, err)
	}
	_ = conn.Close()
	if !peerOK {
		return false, fmt.Errorf("probe %s: the listener did not present this node's etcd certificate", addr)
	}
	// Handshake completed with a root-signed client cert: etcd still accepts it.
	return false, nil
}

// The weaker peer check is safe: forgery cannot advance a phase; Migrate* re-guard on the node.
func peerIsEtcdAt(addr string, rawCerts [][]byte) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return peerIsLocalEtcd(rawCerts)
	}
	leaf, err := parseLeaf(rawCerts)
	if err != nil {
		return false
	}
	return leafIsEtcdServerFor(leaf, host) && issuerIsKnownCA(leaf)
}

func parseLeaf(rawCerts [][]byte) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("pki: peer presented no certificate")
	}
	return x509.ParseCertificate(rawCerts[0])
}

// Matches what LeafSpecs mints; VerifyHostname would accept the "localhost" SAN every leaf carries.
func leafIsEtcdServerFor(leaf *x509.Certificate, host string) bool {
	if leaf.Subject.CommonName != EtcdCN {
		return false
	}
	serverAuth := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			serverAuth = true
			break
		}
	}
	if !serverAuth {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, san := range leaf.IPAddresses {
			// Loopback is in every etcd leaf's SANs, so it identifies no node.
			if san.Equal(ip) && !san.IsLoopback() {
				return true
			}
		}
		return false
	}
	for _, san := range leaf.DNSNames {
		if san == host && san != "localhost" {
			return true
		}
	}
	return false
}

// Signature-checked, not name-compared: an attacker controls the Issuer field, not the signature.
func issuerIsKnownCA(leaf *x509.Certificate) bool {
	for _, name := range []string{EtcdCAName, "ca"} {
		ca, err := readCertOptional(name)
		if err != nil || ca == nil {
			continue
		}
		if leaf.CheckSignatureFrom(ca) == nil {
			return true
		}
	}
	return false
}

// The presented leaf is byte-identical to this node's etcd.crt, what etcd serves in every state.
func peerIsLocalEtcd(rawCerts [][]byte) bool {
	if len(rawCerts) == 0 {
		return false
	}
	want, err := os.ReadFile(path("etcd", "crt"))
	if err != nil {
		return false
	}
	blk, _ := pem.Decode(want)
	if blk == nil {
		return false
	}
	return bytes.Equal(blk.Bytes, rawCerts[0])
}

// A peer alert means etcd refused our cert; unreachable or timeout is indeterminate.
func isCertRejection(err error) bool {
	// No *tls.CertificateVerificationError branch: with InsecureSkipVerify it is unreachable.
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "remote error" {
		return false // unreachable, timeout, version mismatch: indeterminate
	}
	s := err.Error()
	// The alerts etcd sends for an untrusted or absent client cert.
	return strings.Contains(s, "remote error: tls:") &&
		(strings.Contains(s, "bad certificate") ||
			strings.Contains(s, "unknown certificate authority") ||
			strings.Contains(s, "certificate required") ||
			strings.Contains(s, "unknown ca"))
}

// A throwaway client certificate the cluster root signs, held only in memory.
func RootSignedClientCert() (tls.Certificate, error) {
	c, err := mintRootSignedClientCert()
	if err != nil {
		return tls.Certificate{}, err
	}
	return *c, nil
}

// An in-memory client leaf signed by the cluster root CA, impersonating a kubelet.
func mintRootSignedClientCert() (*tls.Certificate, error) {
	if !exists("ca") {
		return nil, fmt.Errorf("no cluster root CA at %s (run this on a control-plane node)", version.PKIDir)
	}
	ca, err := loadCA("ca")
	if err != nil {
		return nil, fmt.Errorf("load root CA: %w", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject: pkix.Name{
			CommonName:   "system:node:stratum-probe",
			Organization: []string{"system:nodes"},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
