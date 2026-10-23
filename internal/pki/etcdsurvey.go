// Remote, PROCESS-derived survey of a control plane's etcd migration state.
//
// ReadEtcdTrust answers the same question from files, and says so: "It is still
// FILE-derived and therefore describes intent, not the running process: etcd
// never reloads its CA pool." That caveat is survivable for a node acting on
// itself, because the node is about to restart etcd anyway. It is not survivable
// for an ORCHESTRATOR deciding whether the cluster is ready for drop-root: a
// bundle written on some other node whose restart never took reads as "ready"
// and is not, and dropping the root on the strength of it is how a cluster loses
// quorum.
//
// So the cluster survey asks etcd rather than asking a disk. One TLS 1.2
// handshake to a peer's client port yields both halves of the state:
//
//	cri.AcceptableCAs   the CA pool the RUNNING etcd will accept clients from,
//	                    i.e. the trust axis — HasEtcdAnchor and TrustsRoot
//	the served leaf     which CA signed this node's etcd certificate,
//	                    i.e. the leaf axis — LeavesMigrated
//
// Neither alone decides a state; together they decide all four. Under TLS 1.2
// the server's CertificateRequest carries AcceptableCAs and Go hands it to the
// GetClientCertificate callback, and both it and the server's own Certificate
// arrive BEFORE the server judges any client certificate — so the survey learns
// everything it needs even when the handshake then fails. It therefore sends no
// client certificate at all and mints nothing.
//
// Anything indeterminate is EtcdStateUnknown, never a plausible-looking state:
// unreachable, TLS 1.3-only, a listener that is not this node's etcd, an anchor
// whose Subject this node cannot name, or trust and leaves that disagree. Per
// etcdca.go, unknown "is never a resting state and no guard may treat it as
// safe" — including the orchestrator's --resume.
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

// errEtcdNotServing marks a survey failure whose cause is etcd not yet serving
// TLS — a member that was just power-cycled has its listener open long before it
// is back in raft. It is the ONLY survey failure the orchestrator may retry:
// every other unknown (impostor listener, foreign issuer, unnameable anchors)
// is definite and must stay one.
var errEtcdNotServing = errors.New("etcd is not serving yet")

// EtcdStalled is the words a survey detail carries when etcd accepted the
// connection but did not answer TLS, as opposed to refusing it: the
// orchestrator gives the two opposite remedies and tells them apart by it.
const EtcdStalled = "did not answer TLS"

// transientErr carries a dial/handshake failure plus a human sub-cause, and
// matches errEtcdNotServing for errors.Is — which is how Transient() and the
// orchestrator's retry recognise it — without printing the sentinel a second
// time in the detail string.
type transientErr struct{ error }

func (transientErr) Is(target error) bool { return target == errEtcdNotServing }

// EtcdRemoteTrust is the process-derived counterpart to EtcdTrust. Field names
// deliberately echo it so the two can be read side by side.
type EtcdRemoteTrust struct {
	Addr string
	// TrustsEtcdAnchor / TrustsRoot are read from the live client CA pool.
	TrustsEtcdAnchor bool
	TrustsRoot       bool
	// UnknownAnchors counts acceptable-CA Subjects this node cannot name. A
	// rotation-replaced root is NOT one of these: EnsureCA mints every cluster
	// root with the same Subject, so a stale root is indistinguishable from the
	// live one here — which is exactly EtcdTrust.TrustsRoot's semantics.
	UnknownAnchors int
	// LeafUnderEtcdCA / LeafUnderRoot describe the etcd certificate the peer
	// actually served, signature-checked against this node's CAs.
	LeafUnderEtcdCA bool
	LeafUnderRoot   bool
	// Err set means every other field is meaningless and State() is unknown.
	Err error
}

// State names the migration phase the RUNNING etcd at Addr is in, using the same
// vocabulary as EtcdTrust.State so a mixed local/remote report reads uniformly.
func (p EtcdRemoteTrust) State() string {
	switch {
	case p.Err != nil:
		return EtcdStateUnknown
	case p.UnknownAnchors > 0:
		// An anchor we cannot name is an anchor we cannot reason about, and it
		// may well be a Kubernetes root. Refuse rather than classify.
		return EtcdStateUnknown
	case !p.TrustsEtcdAnchor:
		// Whatever else is true, this etcd has never been handed the etcd CA.
		return EtcdStateNotMigrated
	case p.TrustsRoot && p.LeafUnderRoot:
		return EtcdStateTrustBoth
	case p.TrustsRoot && p.LeafUnderEtcdCA:
		return EtcdStateIssueNew
	case !p.TrustsRoot && p.LeafUnderEtcdCA:
		return EtcdStateMigrated
	default:
		// Trust and leaves disagree — most sharply, an etcd-CA-only pool serving
		// a root-signed leaf, a node whose own etcd would reject its own
		// apiserver. EtcdTrust.State classifies the on-disk form of this as
		// not-migrated because a follower reaches it transiently mid-phase
		// (etcdca.go, the --fetch-shared case). As a SURVEYED resting state it is
		// a broken node, and the orchestrator must not drive phases against a
		// node it cannot classify.
		return EtcdStateUnknown
	}
}

// Migrated reports the one state that means the etcd trust hole is closed on this member.
func (p EtcdRemoteTrust) Migrated() bool { return p.State() == EtcdStateMigrated }

// Transient reports whether this survey's failure is the "etcd not serving yet"
// kind, which a re-probe resolves once the member finishes starting. Every other
// failure — unparseable leaf, impostor listener, foreign issuer, no
// CertificateRequest, unnameable anchors — is a definite unknown and stays
// non-transient, so the orchestrator's refusal keeps standing behind it.
func (p EtcdRemoteTrust) Transient() bool {
	return p.Err != nil && errors.Is(p.Err, errEtcdNotServing)
}

// ProbeEtcdTrust surveys the control plane whose etcd client port is at nodeIP.
func ProbeEtcdTrust(nodeIP string) EtcdRemoteTrust {
	return ProbeEtcdTrustAt(net.JoinHostPort(nodeIP, fmt.Sprint(version.EtcdClientPort)))
}

// ProbeEtcdTrustAt is ProbeEtcdTrust against an explicit "host:port", so the
// handshake can be exercised against a stand-in listener configured exactly as
// etcd's is.
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
		// Send NO client certificate. The survey needs the CertificateRequest and
		// the server's own Certificate, both of which arrive before the server
		// judges ours; offering one would mean minting a root-signed leaf for
		// every node in the cluster on every survey, to learn nothing extra.
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
			// Structural half of the peer check only. The issuer half is the
			// leaf-axis read below, against the CAs already loaded here — asking
			// peerIsEtcdAt would re-read both CA files and re-verify the same
			// signatures, and would collapse "served a leaf from a CA I cannot
			// place" into the less precise "not that node's etcd".
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil
			}
			structural = leafIsEtcdServerFor(c, host)
			return nil // never fail here: identity is judged after the handshake
		},
		// Same pin, same reason, as EtcdRejectsRootSignedClientAt: under TLS 1.3
		// there is no CertificateRequest carrying AcceptableCAs at all, so the
		// trust axis would be unreadable. A TLS1.3-only etcd yields a version
		// error, which this reports as unknown rather than guessing.
		MaxVersion: tls.VersionTLS12,
	}

	conn, err := dialProbeTLS(addr, cfg)
	if conn != nil {
		_ = conn.Close()
	}
	// err is EXPECTED here and is not itself a failure: etcd runs with
	// --client-cert-auth, so a handshake offering no client certificate ends in a
	// "certificate required" alert. What matters is whether the two reads landed
	// before it did.
	// Order matters, because these are the diagnostics an operator reads while a
	// quorum-critical command has just refused. "Nobody answered" and "something
	// answered that is not your etcd" call for opposite responses, and the
	// earlier form of this function reported the second for both.
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
	// Checked last, so a reachable, identified etcd that simply does not request
	// a client certificate reports the reason written for it rather than being
	// mistaken for an unreachable one.
	if !sawRequest {
		out.Err = fmt.Errorf("pki: etcd at %s requested no client certificate, so its live client-CA pool is unreadable "+
			"(is --client-cert-auth set? is it TLS 1.3-only?): %v", addr, err)
		return out
	}
	return out
}

// notServingError builds the refusal for a peer that never served a
// certificate. The sub-cause it appends matters, because the three ways to fail
// here call for opposite operator responses: "not listening" and "accepted but
// did not answer" are what a just-restarted etcd looks like, and both are
// marked errEtcdNotServing so the orchestrator may re-probe; a dial that never
// connected is not coming back on its own, so it is reported for what it is
// and stays non-transient.
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

// dialProbeTLS is the one network call both probes make. It is a package var
// solely so tests can stand a listener on loopback and still have the probe
// dial — and therefore IDENTITY-CHECK — a peer address, which is the path that
// cannot be exercised otherwise: peerIsEtcdAt's remote branch is chosen by the
// address, and a test that dialled loopback would silently take the local one.
//
// It redirects the connection only. Everything the probes judge — the served
// certificate, its SANs, its issuer, the acceptable-CA set — still comes from
// the listener over a real TLS handshake.
var dialProbeTLS = func(addr string, cfg *tls.Config) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: probeTimeout}, "tcp", addr, cfg)
}
