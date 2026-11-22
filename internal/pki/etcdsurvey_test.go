package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// etcd's client listener on a non-loopback address, so the survey exercises its peer path.
func remoteEtcd(t *testing.T, trustEtcdCA, trustRoot bool, leaf string) string {
	t.Helper()
	srv, err := tls.LoadX509KeyPair(path(leaf, "crt"), path(leaf, "key"))
	if err != nil {
		t.Fatalf("load %s leaf: %v", leaf, err)
	}
	pool := x509.NewCertPool()
	if trustEtcdCA {
		pool.AddCert(loadCert(t, EtcdCAName))
	}
	if trustRoot {
		pool.AddCert(loadCert(t, "ca"))
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{srv},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MaxVersion:   tls.VersionTLS12,
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
	// The probe identity-checks the address it was asked for, so the remote peer path runs.
	redirectProbeDial(t, ln.Addr().String())
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort(tIP, port)
}

// Points the dial at to, not the address judged; an optional timeout keeps timeout tests fast.
func redirectProbeDial(t *testing.T, to string, timeout ...time.Duration) {
	t.Helper()
	d := probeTimeout
	if len(timeout) > 0 {
		d = timeout[0]
	}
	saved := dialProbeTLS
	dialProbeTLS = func(_ string, cfg *tls.Config) (*tls.Conn, error) {
		return tls.DialWithDialer(&net.Dialer{Timeout: d}, "tcp", to, cfg)
	}
	t.Cleanup(func() { dialProbeTLS = saved })
}

func surveyAt(t *testing.T, addr string) EtcdRemoteTrust {
	t.Helper()
	return ProbeEtcdTrustAt(addr)
}

func TestSurveyReadsBothAxes(t *testing.T) {
	genInto(t)
	migrateAll(t) // gives us an etcd CA and etcd-CA-signed leaves to serve

	for _, tc := range []struct {
		name                   string
		trustEtcdCA, trustRoot bool
		leafUnderEtcdCA        bool
		want                   string
	}{
		// etcd has never seen the etcd CA: whatever it serves, it is unmigrated.
		{"root-only trust, root-signed leaf", false, true, false, EtcdStateNotMigrated},
		// Trust widened, leaves not yet moved.
		{"both anchors, root-signed leaf", true, true, false, EtcdStateTrustBoth},
		// Leaves moved, root still an anchor: the gate's readiness condition.
		{"both anchors, etcd-CA-signed leaf", true, true, true, EtcdStateIssueNew},
		// The end state, and the only one that closes the etcd trust hole.
		{"etcd-CA only, etcd-CA-signed leaf", true, false, true, EtcdStateMigrated},
		// Trust and leaves disagree: on disk not-migrated, but surveyed a resting state is a broken node.
		{"etcd-CA only, root-signed leaf", true, false, false, EtcdStateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf := "etcd"
			if !tc.leafUnderEtcdCA {
				leaf = rootSignedEtcdLeaf(t)
			}
			got := surveyAt(t, remoteEtcd(t, tc.trustEtcdCA, tc.trustRoot, leaf)).State()
			if got != tc.want {
				t.Errorf("survey = %s, want %s", got, tc.want)
			}
		})
	}
}

// Mints an etcd server leaf under the CLUSTER ROOT, what an unmigrated member serves.
func rootSignedEtcdLeaf(t *testing.T) string {
	t.Helper()
	ca, err := loadCA("ca")
	if err != nil {
		t.Fatal(err)
	}
	_, etcdLeaves := SplitLeafSpecs(tHost, tIP, nil)
	for _, l := range etcdLeaves {
		if l.Name != "etcd" {
			continue
		}
		l.Name = "etcd-rootsigned"
		if err := ca.Issue(l); err != nil {
			t.Fatal(err)
		}
		return l.Name
	}
	t.Fatal("no etcd leaf spec")
	return ""
}

// Unknown, which refuses; a guess would let drop-root proceed against an unreachable member.
func TestSurveyIsUnknownWhenEtcdIsUnreachable(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// A port with nothing on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := surveyAt(t, addr)
	if p.Err == nil {
		t.Fatal("survey of an unreachable etcd returned no error")
	}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("survey of an unreachable etcd = %s, want %s", got, EtcdStateUnknown)
	}
}

// The peer-identity check: it must refuse a listener that is not that node's etcd.
func TestSurveyRejectsAnImpostorListener(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// An impostor with a leaf signed by a CA this node does not hold.
	other := t.TempDir()
	saved := version.PKIDir
	version.PKIDir = other
	if err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	impostor := remoteEtcd(t, false, true, "etcd")
	version.PKIDir = saved

	p := surveyAt(t, impostor)
	if p.Err == nil {
		t.Fatal("survey accepted a listener serving a foreign certificate as that node's etcd")
	}
	if !strings.Contains(p.Err.Error(), "etcd certificate") {
		t.Errorf("refusal should say the listener is not that node's etcd, got: %v", p.Err)
	}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("survey of an impostor = %s, want %s", got, EtcdStateUnknown)
	}
}

// An unplaceable leaf is unknown, not "probably fine"; only the issuer check rejects it.
func TestSurveyRefusesALeafFromAnUnknownCA(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// A foreign CA, structurally identical etcd leaf.
	foreign := t.TempDir()
	saved := version.PKIDir
	version.PKIDir = foreign
	if err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	addr := remoteEtcd(t, true, false, "etcd")
	version.PKIDir = saved

	p := surveyAt(t, addr)
	if p.Err == nil {
		t.Fatal("survey placed a leaf signed by a CA this node does not hold")
	}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("survey of an unplaceable leaf = %s, want %s", got, EtcdStateUnknown)
	}
}

// An unnameable anchor may be a Kubernetes root, exactly what drop-root must not run against.
func TestSurveyTreatsAnUnnameableAnchorAsUnknown(t *testing.T) {
	p := EtcdRemoteTrust{TrustsEtcdAnchor: true, LeafUnderEtcdCA: true, UnknownAnchors: 1}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("an unnameable trust anchor resolved to %s — the node would be treated as migrated", got)
	}
	if p.Migrated() {
		t.Error("Migrated() is true for a node with an anchor we cannot name")
	}
}

// A raw TCP listener that accepts and never writes: a just-restarted etcd replaying its WAL.
func stalledListener(t *testing.T) string {
	t.Helper()
	var held []net.Conn
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			// Held open and never answered: that IS the stall.
			held = append(held, c)
		}
	}()
	redirectProbeDial(t, ln.Addr().String(), 200*time.Millisecond)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort(tIP, port)
}

// A connect that succeeds then never answers is etcd starting up: unknown, but transient.
func TestSurveyStalledListenerIsTransient(t *testing.T) {
	genInto(t)
	migrateAll(t)

	p := surveyAt(t, stalledListener(t))
	if p.Err == nil {
		t.Fatal("survey of a stalled listener returned no error")
	}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("survey of a stalled listener = %s, want %s", got, EtcdStateUnknown)
	}
	if !p.Transient() {
		t.Errorf("a stalled listener must be transient, got: %v", p.Err)
	}
	if !strings.Contains(p.Err.Error(), "could not complete a TLS handshake") {
		t.Errorf("detail lost its prefix: %v", p.Err)
	}
	if !strings.Contains(p.Err.Error(), "did not answer TLS") {
		t.Errorf("detail should name the stall: %v", p.Err)
	}
}

// Nothing listening yet is the same just-restarted situation, so it is transient too.
func TestSurveyRefusedIsTransient(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// A port with nothing on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := surveyAt(t, addr)
	if p.Err == nil {
		t.Fatal("survey of a closed port returned no error")
	}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("survey of a closed port = %s, want %s", got, EtcdStateUnknown)
	}
	if !p.Transient() {
		t.Errorf("connection refused must be transient, got: %v", p.Err)
	}
	if !strings.Contains(p.Err.Error(), "etcd is not listening") {
		t.Errorf("detail should name the refusal: %v", p.Err)
	}
}

// Transiency is reserved for etcd not serving yet; a retry would re-read the same impostor.
func TestSurveyDefiniteUnknownsAreNotTransient(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// A structurally valid etcd leaf signed by a root this node does not hold.
	other := t.TempDir()
	saved := version.PKIDir
	version.PKIDir = other
	if err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	impostor := remoteEtcd(t, false, true, "etcd")
	version.PKIDir = saved
	// Surveyed now: the next remoteEtcd points the one dial seam at its own listener.
	impostorProbe := surveyAt(t, impostor)
	// A foreign issuer under an otherwise placeable etcd leaf.
	foreign := t.TempDir()
	version.PKIDir = foreign
	if err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	foreignLeaf := remoteEtcd(t, true, false, "etcd")
	version.PKIDir = saved

	probeCases := map[string]EtcdRemoteTrust{
		"impostor listener": impostorProbe,
		"foreign issuer":    surveyAt(t, foreignLeaf),
	}
	// Each answer came from its own listener: the impostor trusts only a root.
	if p := probeCases["impostor listener"]; !p.TrustsRoot || p.TrustsEtcdAnchor {
		t.Errorf("impostor listener: not answered by the impostor: %+v", p)
	}
	if p := probeCases["foreign issuer"]; p.TrustsRoot || !p.TrustsEtcdAnchor {
		t.Errorf("foreign issuer: not answered by the foreign stand: %+v", p)
	}
	for name, p := range probeCases {
		if p.Err == nil || p.State() != EtcdStateUnknown {
			t.Errorf("%s: want a definite unknown, got err=%v state=%s", name, p.Err, p.State())
		}
		if p.Transient() {
			t.Errorf("%s: a definite unknown must not be transient", name)
		}
		if errors.Is(p.Err, errEtcdNotServing) {
			t.Errorf("%s: must not carry errEtcdNotServing", name)
		}
	}

	// The remaining definite unknowns never set Err, so transiency is false by construction.
	synthetic := map[string]EtcdRemoteTrust{
		"unnameable anchor":     {TrustsEtcdAnchor: true, LeafUnderEtcdCA: true, UnknownAnchors: 1},
		"trust/leaf split":      {TrustsEtcdAnchor: true, LeafUnderRoot: true},
		"no CertificateRequest": {TrustsRoot: true, LeafUnderEtcdCA: true},
	}
	for name, p := range synthetic {
		if p.Transient() || errors.Is(p.Err, errEtcdNotServing) {
			t.Errorf("%s: a definite unknown must not be transient", name)
		}
	}
}

// End to end: the first probe stalls, a later one is answered by an etcd that rejoined raft.
func TestSurveyTrustBothAfterStall(t *testing.T) {
	genInto(t)
	migrateAll(t)

	if p := surveyAt(t, stalledListener(t)); !p.Transient() {
		t.Fatalf("first probe should hit the stall transiently, got: %v", p.Err)
	}

	q := surveyAt(t, remoteEtcd(t, true, true, rootSignedEtcdLeaf(t)))
	if q.Err != nil {
		t.Fatalf("re-probe after the stall failed: %v", q.Err)
	}
	if got := q.State(); got != EtcdStateTrustBoth {
		t.Errorf("re-probe after the stall = %s, want %s", got, EtcdStateTrustBoth)
	}
}

// A never-connected dial is unreachable, not transient; refused and stalled dials are.
func TestSurveyDialTimeoutIsNotTransient(t *testing.T) {
	err := notServingError("10.0.0.5:2379", &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: os.ErrDeadlineExceeded, // net.Error with Timeout() true, Op "dial"
	})
	if err == nil {
		t.Fatal("no error built")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("detail should name the unreachable dial: %v", err)
	}
	if errors.Is(err, errEtcdNotServing) {
		t.Errorf("a dial timeout must not carry errEtcdNotServing")
	}
	p := EtcdRemoteTrust{Err: err}
	if p.Transient() {
		t.Errorf("a dial timeout must not be transient")
	}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("an unreachable etcd = %s, want %s", got, EtcdStateUnknown)
	}
}
