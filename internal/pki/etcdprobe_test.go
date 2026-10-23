package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// fakeEtcd stands up a listener configured exactly as etcd's client listener is
// in v3.5.16 — RequireAndVerifyClientCert with ClientCAs from --trusted-ca-file —
// serving this node's real etcd.crt. trustRoot mirrors whether the cluster root
// is still in etcd's trust bundle, i.e. whether drop-root has run.
func fakeEtcd(t *testing.T, trustRoot bool) string {
	t.Helper()
	srv, err := tls.LoadX509KeyPair(path("etcd", "crt"), path("etcd", "key"))
	if err != nil {
		t.Fatalf("load etcd leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(loadCert(t, EtcdCAName))
	if trustRoot {
		pool.AddCert(loadCert(t, "ca"))
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{srv},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// TestProbeDetectsMigratedEtcd pins the failure mode that would make this
// control useless: under TLS 1.2 the Go client filters Config.Certificates
// against the CertificateRequest's AcceptableCAs, so after drop-root — when etcd
// advertises only the etcd CA — a root-signed leaf never reached the wire. etcd
// answered with a bare handshake_failure rather than an unknown-CA alert, and the
// probe reported INDETERMINATE on precisely the clusters that were fixed. CIS 2.7
// could report OPEN but never CLOSED.
func TestProbeDetectsMigratedEtcd(t *testing.T) {
	genInto(t)
	migrateAll(t)

	rejected, err := EtcdRejectsRootSignedClientAt(fakeEtcd(t, false))
	if err != nil {
		t.Fatalf("probe against a migrated etcd must be conclusive, got: %v", err)
	}
	if !rejected {
		t.Fatal("probe says a migrated etcd accepts cluster-root-signed clients — the etcd trust hole would be reported OPEN on a fixed cluster")
	}
}

// TestProbeDetectsVulnerableEtcd: while the root is still trusted, the same
// certificate must be ACCEPTED and the control must report the cluster exposed.
func TestProbeDetectsVulnerableEtcd(t *testing.T) {
	genInto(t)
	migrateAll(t)

	rejected, err := EtcdRejectsRootSignedClientAt(fakeEtcd(t, true))
	if err != nil {
		t.Fatalf("probe against a root-trusting etcd must be conclusive, got: %v", err)
	}
	if rejected {
		t.Fatal("probe says etcd rejects root-signed clients while it still trusts the root — this would report the etcd trust hole closed on a vulnerable cluster")
	}
}

// TestProbeRejectsImpostorListener closes the forgery hole: the etcd client port
// is above 1024, so whenever etcd is down any unprivileged local process can bind
// it. A listener that merely refuses client certificates would otherwise satisfy
// a control that gates a CIS audit and exits non-zero.
func TestProbeRejectsImpostorListener(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// An impostor: valid TLS, refuses every client cert, but is not our etcd.
	// Its leaf is a *different* etcd CA's, so even a chain check would not help —
	// only comparing against this node's actual etcd.crt catches it.
	dir := t.TempDir()
	realPKI := version.PKIDir
	version.PKIDir = dir
	if _, err := NewCA("etcd-ca", "impostor-etcd-ca"); err != nil {
		t.Fatal(err)
	}
	impostor, err := loadCA("etcd-ca")
	if err != nil {
		t.Fatal(err)
	}
	if err := impostor.Issue(CertSpec{Name: "etcd", CN: "etcd", IPs: []string{"127.0.0.1"}, ServerAuth: true}); err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(dir, "etcd.crt"), filepath.Join(dir, "etcd.key"))
	if err != nil {
		t.Fatal(err)
	}
	version.PKIDir = realPKI // probe reads OUR etcd.crt again

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    x509.NewCertPool(), // trusts nobody: always "rejects"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
		}
	}()

	rejected, err := EtcdRejectsRootSignedClientAt(ln.Addr().String())
	if rejected {
		t.Fatal("an impostor listener satisfied CIS 2.7 — the audit control is forgeable by any local process")
	}
	if err == nil {
		t.Fatal("probe should report INDETERMINATE against an unidentified listener, not a silent pass")
	}
	if !strings.Contains(err.Error(), "etcd certificate") {
		t.Errorf("error should say the peer was not identified, got: %v", err)
	}
}

// TestProbeIndeterminateWhenNothingListening: a closed port is not evidence that
// etcd rejects anything. It must never read as a pass.
func TestProbeIndeterminateWhenNothingListening(t *testing.T) {
	genInto(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing is listening here now

	rejected, err := EtcdRejectsRootSignedClientAt(addr)
	if rejected {
		t.Fatal("a closed port reported as 'etcd rejects root-signed clients'")
	}
	if err == nil {
		t.Fatal("probe must report INDETERMINATE when it cannot reach etcd")
	}
}

// TestIsCertRejectionDiscriminates is the direct test for the classifier the
// whole control rests on. Nothing else reaches it: the closed-port test fails at
// the dial, so peerOK is false and the probe returns before isCertRejection is
// ever called — meaning the function could be widened to `return true` and every
// other test in this package would still pass, on the one function that decides
// whether CIS 2.7 reports the etcd trust hole closed.
//
// The production logic is an ALLOWLIST of the four alerts etcd sends for an
// untrusted or absent client cert, plus a requirement that the alert came from
// the PEER. Both halves are load-bearing and both are pinned here:
//
//   - handshake failure must stay INDETERMINATE. It is what a TLS1.3-only or
//     otherwise mismatched server answers, and it is also what the client-side
//     AcceptableCAs filtering produces when the leaf is supplied through
//     Config.Certificates instead of GetClientCertificate — the exact bug
//     TestProbeDetectsMigratedEtcd pins. Reading it as a rejection would report
//     the vulnerability closed on a cluster nobody ever probed.
//   - a peer alert is the only evidence. A string that merely CONTAINS the alert
//     text, or an alert this side sent, proves nothing about what etcd decided.
func TestIsCertRejectionDiscriminates(t *testing.T) {
	peerAlert := func(alert string) error { return &net.OpError{Op: "remote error", Err: errors.New("tls: " + alert)} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// The four alerts etcd sends when it will not accept the client cert.
		{"bad certificate", peerAlert("bad certificate"), true},
		{"unknown certificate authority", peerAlert("unknown certificate authority"), true},
		{"certificate required", peerAlert("certificate required"), true},
		{"unknown ca", peerAlert("unknown ca"), true},
		// Still a rejection once the caller has wrapped it, which is why this
		// unwraps with errors.As rather than type-asserting.
		{"wrapped peer alert", fmt.Errorf("probe etcd at %s: %w", "127.0.0.1:2379", peerAlert("bad certificate")), true},

		// Not rejections. Every one of these must degrade to INDETERMINATE.
		{"handshake failure", peerAlert("handshake failure"), false},
		{"protocol version", peerAlert("protocol version not supported"), false},
		{"internal error", peerAlert("internal error"), false},
		{"timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, false},
		// We sent the alert, not the peer: says nothing about etcd's decision.
		{"local alert", &net.OpError{Op: "local error", Err: errors.New("tls: bad certificate")}, false},
		// An alert reached only through an outer operation the probe did not make.
		// errors.As matches the OUTERMOST net.OpError, so this is judged on Op
		// "read" and degrades to indeterminate — the conservative direction, and
		// the reason the type/Op guard is not redundant with the text match below
		// it. The probe pins TLS 1.2 precisely so the alert lands on the handshake
		// instead of arriving like this.
		{"alert behind an outer op", &net.OpError{Op: "read", Net: "tcp", Err: &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}}, false},
		// The alert text without an alert behind it.
		{"text only", errors.New("remote error: tls: bad certificate"), false},
		// Deliberately absent from the production logic: this says WE could not
		// verify THEIR server cert, which is the opposite claim.
		{"our verification of their cert", &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCertRejection(c.err); got != c.want {
				verdict := "INDETERMINATE"
				if c.want {
					verdict = "a certificate rejection"
				}
				t.Errorf("isCertRejection(%v) = %v, want %v — %q must read as %s",
					c.err, got, c.want, c.err, verdict)
			}
		})
	}
}

// TestProbeNeedsRootCAKey: on a worker there is no ca.key to mint with, so the
// probe must say so rather than concluding anything.
func TestProbeNeedsRootCAKey(t *testing.T) {
	version.PKIDir = t.TempDir() // no ca.crt/ca.key here
	if _, err := EtcdRejectsRootSignedClient(); err == nil {
		t.Fatal("probe concluded without a root CA on this node")
	}
}

// TestEtcdTrustFileNeverUnionsTheClusterRoot pins the rule against an over-broad
// trust set: this file verifies the etcd SERVER on remote MemberAdd/MemberList calls, so
// including the Kubernetes root would let any root-signed ServerAuth leaf holding
// that node's IP SAN impersonate an etcd endpoint during HA join or rejoin.
func TestEtcdTrustFileNeverUnionsTheClusterRoot(t *testing.T) {
	genInto(t)
	migrateAll(t)

	got, err := EtcdTrustFile()
	if err != nil {
		t.Fatal(err)
	}
	if want := EtcdBundlePath(); got != want {
		t.Fatalf("EtcdTrustFile() = %s, want %s", got, want)
	}
	pem, err := EtcdTrustPEM()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("no usable certs")
	}
	// A root-signed server leaf must NOT verify against the etcd trust set.
	if _, err := loadCert(t, "apiserver").Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName: "kubernetes",
	}); err == nil {
		t.Error("a cluster-root-signed server cert verifies against the etcd trust set — the root is still trusted for etcd server identity")
	}

	// Fallback only when the etcd bundle is genuinely absent.
	if err := os.Remove(EtcdBundlePath()); err != nil {
		t.Fatal(err)
	}
	got, err = EtcdTrustFile()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "ca-bundle.crt" {
		t.Errorf("fallback = %s, want ca-bundle.crt", got)
	}
}
