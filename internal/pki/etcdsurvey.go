// PROCESS-derived survey: one TLS 1.2 handshake reads the live client CA pool and the served leaf.
package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/ctrliq/stratum/internal/version"
)

// The one survey failure the orchestrator may retry: a restarting member opens its listener early.
var errEtcdNotServing = errors.New("etcd is not serving yet")

// Etcd accepted the connection but did not answer TLS, as opposed to refusing it.
const EtcdStalled = "did not answer TLS"

// Matches errEtcdNotServing for errors.Is without printing the sentinel twice in the detail.
type transientErr struct{ error }

func (transientErr) Is(target error) bool { return target == errEtcdNotServing }

// The process-derived counterpart to EtcdTrust, with field names that echo it.
type EtcdRemoteTrust struct {
	Addr string
	// Read from the live client CA pool.
	TrustsEtcdAnchor bool
	TrustsRoot       bool
	// A rotation-replaced root is not one of these: every cluster root shares a Subject.
	UnknownAnchors int
	// The served etcd certificate, signature-checked against this node's CAs.
	LeafUnderEtcdCA bool
	LeafUnderRoot   bool
	Err             error
}

// The phase the RUNNING etcd at Addr is in, sharing EtcdTrust.State's vocabulary.
func (p EtcdRemoteTrust) State() string {
	switch {
	case p.Err != nil:
		return EtcdStateUnknown
	case p.UnknownAnchors > 0:
		// An unnamed anchor may well be a Kubernetes root; refuse rather than classify.
		return EtcdStateUnknown
	case !p.TrustsEtcdAnchor:
		return EtcdStateNotMigrated
	case p.TrustsRoot && p.LeafUnderRoot:
		return EtcdStateTrustBoth
	case p.TrustsRoot && p.LeafUnderEtcdCA:
		return EtcdStateIssueNew
	case !p.TrustsRoot && p.LeafUnderEtcdCA:
		return EtcdStateMigrated
	default:
		// Trust and leaves disagree; as a surveyed resting state that is a broken node.
		return EtcdStateUnknown
	}
}

// Migrated reports the one state that means the etcd trust hole is closed on this member.
func (p EtcdRemoteTrust) Migrated() bool { return p.State() == EtcdStateMigrated }

// True only for the "etcd not serving yet" kind, which a re-probe resolves.
func (p EtcdRemoteTrust) Transient() bool {
	return p.Err != nil && errors.Is(p.Err, errEtcdNotServing)
}

func ProbeEtcdTrust(nodeIP string) EtcdRemoteTrust {
	return ProbeEtcdTrustAt(net.JoinHostPort(nodeIP, fmt.Sprint(version.EtcdClientPort)))
}

// Against an explicit "host:port", so tests can exercise the handshake on a stand-in listener.
func ProbeEtcdTrustAt(addr string) EtcdRemoteTrust {
	out := EtcdRemoteTrust{Addr: addr}

	etcdCA, err := readCertOptional(EtcdCAName)
	if err != nil {
		out.Err = fmt.Errorf("pki: read %s to survey %s: %w", EtcdCAName, addr, err)
		return out
	}
	root, err := readCertOptional("ca")
	if err != nil {
		out.Err = fmt.Errorf("pki: read cluster root to survey %s: %w", addr, err)
		return out
	}
	if etcdCA == nil && root == nil {
		out.Err = fmt.Errorf("pki: no CA on this node to survey %s against (run from a control plane)", addr)
		return out
	}

	var (
		sawRequest  bool
		sawPeerCert bool
		leaf        *x509.Certificate
		structural  bool
	)
	cfg := &tls.Config{
		// Send no client certificate: the reads land before the server judges ours.
		GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			sawRequest = true
			for _, rawSubject := range cri.AcceptableCAs {
				switch {
				case etcdCA != nil && bytes.Equal(rawSubject, etcdCA.RawSubject):
					out.TrustsEtcdAnchor = true
				case root != nil && bytes.Equal(rawSubject, root.RawSubject):
					out.TrustsRoot = true
				default:
					out.UnknownAnchors++
				}
			}
			return &tls.Certificate{}, nil
		},
		InsecureSkipVerify: true, //nolint:gosec // deliberate: peer identity is checked below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			sawPeerCert = len(rawCerts) > 0
			c, err := parseLeaf(rawCerts)
			if err != nil {
				return nil
			}
			leaf = c
			// Structural half only; the issuer half is the leaf-axis read below.
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil
			}
			structural = leafIsEtcdServerFor(c, host)
			return nil // identity is judged after the handshake
		},
		// TLS 1.2: under TLS 1.3 there is no CertificateRequest carrying AcceptableCAs.
		MaxVersion: tls.VersionTLS12,
	}

	conn, err := dialProbeTLS(addr, cfg)
	if conn != nil {
		_ = conn.Close()
	}
	// A handshake failure is expected: what matters is whether the reads landed first.
	if !sawPeerCert {
		out.Err = notServingError(addr, err)
		return out
	}
	if leaf == nil {
		out.Err = fmt.Errorf("pki: the listener at %s served a certificate this node could not parse: %v", addr, err)
		return out
	}
	if !structural {
		out.Err = fmt.Errorf("pki: the listener at %s is not that node's etcd — it served %q, not an etcd server certificate for that address, "+
			"so its answer proves nothing", addr, leaf.Subject.CommonName)
		return out
	}
	if etcdCA != nil && leaf.CheckSignatureFrom(etcdCA) == nil {
		out.LeafUnderEtcdCA = true
	}
	if root != nil && leaf.CheckSignatureFrom(root) == nil {
		out.LeafUnderRoot = true
	}
	if !out.LeafUnderEtcdCA && !out.LeafUnderRoot {
		out.Err = fmt.Errorf("pki: the etcd certificate served at %s was issued by a CA this node does not hold "+
			"(issuer %q) — the cluster's migration state cannot be determined from here", addr, leaf.Issuer.CommonName)
		return out
	}
	// Checked last, so a reachable etcd that requests no client cert gets its own reason.
	if !sawRequest {
		out.Err = fmt.Errorf("pki: etcd at %s requested no client certificate, so its live client-CA pool is unreadable "+
			"(is --client-cert-auth set? is it TLS 1.3-only?): %v", addr, err)
		return out
	}
	return out
}

// "Not listening" and "did not answer" are transient (a restarting etcd); a dead dial is not.
func notServingError(addr string, err error) error {
	var oe *net.OpError
	var ne net.Error
	switch {
	case errors.As(err, &oe) && oe.Op == "dial" && errors.Is(err, syscall.ECONNREFUSED):
		err = transientErr{fmt.Errorf("etcd is not listening: %w", err)}
	case errors.As(err, &oe) && oe.Op == "dial":
		err = fmt.Errorf("unreachable: %w", err)
	case errors.As(err, &ne) && ne.Timeout():
		err = transientErr{fmt.Errorf("etcd accepted the connection but %s (starting up or not yet back in raft): %w", EtcdStalled, err)}
	}
	return fmt.Errorf("pki: could not complete a TLS handshake with etcd at %s: %w", addr, err)
}

// A package var so tests can redirect the dial; everything judged still comes from the handshake.
var dialProbeTLS = func(addr string, cfg *tls.Config) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: probeTimeout}, "tcp", addr, cfg)
}
