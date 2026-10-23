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

// probeTimeout bounds the whole handshake attempt. An interactive check
// runs the probe, so this stays short.
const probeTimeout = 5 * time.Second

// EtcdRejectsRootSignedClient reports whether the etcd RUNNING on this node
// rejects a cluster-root-signed client certificate — i.e. whether the etcd trust hole is
// actually closed on this process rather than merely on disk.
//
// It mints a throwaway leaf from the cluster root CA with the same subject a
// kubelet gets (CN=system:node:<host>, O=system:nodes) and offers it to
// 127.0.0.1:2379. That is precisely the reported attack: read a kubelet client
// cert off any worker, point it at a control plane's client port. The certificate
// is generated in memory and never written to disk.
//
// Why this exists rather than reading the trust bundle: etcd builds its client CA
// pool once at listener construction and never reloads it, so a drop-root whose
// restart failed leaves a bundle that says "migrated" and a process that still
// accepts every kubelet certificate in the cluster. Only a handshake distinguishes
// them.
//
// Returns (rejected, nil) when the answer is definite. A non-nil error means
// INDETERMINATE — etcd unreachable, or no root CA key on this node (a worker) —
// and callers must not report it as either pass or fail.
func EtcdRejectsRootSignedClient() (bool, error) {
	return EtcdRejectsRootSignedClientAt(net.JoinHostPort("127.0.0.1", fmt.Sprint(version.EtcdClientPort)))
}

// EtcdRejectsRootSignedClientAt is EtcdRejectsRootSignedClient against an explicit
// address, so the handshake can be exercised in tests against a stand-in listener
// configured exactly as etcd's is — and so the cluster
// orchestrator can assert the migration landed on every control plane rather than only
// on the one it runs from. A remote caller passes "<nodeIP>:2379".
//
// The peer check is address-aware: against the local node it still requires the
// served leaf to be byte-identical to this node's etcd.crt, and against a peer it
// falls back to identifying the leaf structurally (see peerIsEtcdAt).
func EtcdRejectsRootSignedClientAt(addr string) (bool, error) {
	cert, err := mintRootSignedClientCert()
	if err != nil {
		return false, err
	}
	// Two non-obvious requirements, both load-bearing:
	//
	// GetClientCertificate rather than Certificates. Under TLS 1.2 the Go client
	// filters Config.Certificates against the CertificateRequest's AcceptableCAs,
	// and after drop-root etcd advertises only the etcd CA — so a root-signed leaf
	// is dropped client-side, etcd never sees a certificate, and it answers with a
	// bare handshake_failure instead of an unknown-CA alert. The probe would then
	// be permanently indeterminate on exactly the clusters that are fixed.
	// GetClientCertificate bypasses that filter: we WANT etcd to judge this cert.
	//
	// VerifyPeerCertificate to identify who answered. The client port is above
	// 1024, so if etcd is down any local process can bind it, and a listener that
	// simply refuses client certs would otherwise satisfy a control that gates a
	// CIS audit. InsecureSkipVerify stops a half-migrated server cert from masking
	// the answer, so this is the only peer check there is.
	var peerOK bool
	cfg := &tls.Config{
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert, nil
		},
		InsecureSkipVerify: true, //nolint:gosec // deliberate: peer identity is checked below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			peerOK = peerIsEtcdAt(addr, rawCerts)
			return nil // never fail here: we are probing OUR cert, not judging theirs
		},
		// Pin TLS 1.2 so an untrusted client cert is refused during the handshake.
		// Under TLS 1.3 the client finishes before the server validates, and the
		// alert only surfaces on a later read — far harder to classify and easy to
		// mistake for a healthy connection. A TLS1.3-only etcd yields a version
		// error, which is not a cert rejection, so it degrades to INDETERMINATE.
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

// peerIsEtcdAt identifies the listener that answered at addr as that node's etcd.
//
// Against the local node the byte-identical comparison below is available and is
// kept, because it is the strongest check there is: it admits exactly one
// certificate. Against a PEER it is not available — this node does not hold the
// peer's etcd.crt — so the leaf is identified structurally instead: it must be an
// etcd server leaf (CN and ServerAuth EKU, as minted by LeafSpecs) carrying the
// address we dialled in its SANs, issued by a CA whose Subject this node knows.
//
// That is deliberately weaker, and the gap is worth naming rather than hiding.
// Forging it needs a ServerAuth leaf with CN=etcd and the target's IP SAN signed
// by a CA the cluster trusts. On a MIGRATED cluster only the etcd CA qualifies,
// and its key lives only on control planes. On an UNMIGRATED cluster the cluster
// root also qualifies — which is not a weakness this check introduces, it is
// the etcd trust hole itself, the condition the caller is about to fix. The orchestrator is
// safe under it either way: the only forgery that buys anything is one that
// advances a node's apparent phase, and every phase transition is independently
// re-guarded on the node itself by the pki.Migrate* primitives.
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

// parseLeaf returns the peer's end-entity certificate.
func parseLeaf(rawCerts [][]byte) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("pki: peer presented no certificate")
	}
	return x509.ParseCertificate(rawCerts[0])
}

// leafIsEtcdServerFor reports whether leaf is the etcd server certificate for
// host, matching what LeafSpecs mints: CN "etcd", ServerAuth, and the node's
// address among the SANs. VerifyHostname is not used — it would also accept the
// "localhost" DNS SAN every etcd leaf in the cluster carries, which would make
// every node's certificate valid for every other node.
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

// issuerIsKnownCA reports whether leaf was signed by a CA this node holds — the
// etcd CA or the cluster root. Signature-checked, not name-compared: an attacker
// controls the Issuer field of a certificate they mint, but not the signature.
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

// peerIsLocalEtcd reports whether the leaf the listener presented is byte-identical
// to this node's etcd.crt. That file is what the running etcd serves in every
// migration state, so an exact DER match is both sufficient and free of chain
// assumptions.
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

// isCertRejection distinguishes "etcd refused our certificate" from "etcd was not
// reachable". Rejection arrives as a TLS alert from the peer; anything else
// (connection refused, timeout, no route) is indeterminate and must not be read
// as a pass.
func isCertRejection(err error) bool {
	// Deliberately no *tls.CertificateVerificationError branch: with
	// InsecureSkipVerify set it is unreachable, and its meaning is the OPPOSITE of
	// what this function reports — it says WE could not verify THEIR server cert,
	// which must never be read as "etcd refused ours" on a control that gates an
	// audit. A peer alert is the only evidence of rejection.
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "remote error" {
		return false // unreachable, timeout, version mismatch: indeterminate
	}
	s := err.Error()
	// Go renders a peer alert as "remote error: tls: <alert>". The alerts etcd
	// sends for an untrusted or absent client cert are bad certificate, unknown
	// certificate authority, and certificate required.
	return strings.Contains(s, "remote error: tls:") &&
		(strings.Contains(s, "bad certificate") ||
			strings.Contains(s, "unknown certificate authority") ||
			strings.Contains(s, "certificate required") ||
			strings.Contains(s, "unknown ca"))
}

// RootSignedClientCert is a throwaway client certificate the cluster root
// signs, held only in memory.
func RootSignedClientCert() (tls.Certificate, error) {
	c, err := mintRootSignedClientCert()
	if err != nil {
		return tls.Certificate{}, err
	}
	return *c, nil
}

// mintRootSignedClientCert generates an in-memory client leaf signed by the
// cluster root CA, impersonating a kubelet.
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
