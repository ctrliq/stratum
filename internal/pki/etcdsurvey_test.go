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

// The survey's contract, stated as tests.
//
// Two channels, from one TLS 1.2 handshake:
//
//	AcceptableCAs   the trust axis — has the etcd anchor, still trusts a root
//	the served leaf the leaf axis  — which CA signed this member's etcd cert
//
// Neither alone decides a state. Every test below moves exactly one axis and
// asserts the state it produces, which is what pins that both are being read.

// remoteEtcd stands up a listener configured exactly as etcd's client listener
// is, on a non-loopback address, so the survey exercises its PEER path rather
// than the byte-identical local comparison.
//
// trustRoot / trustEtcdCA select the client CA pool (--trusted-ca-file), and
// leaf selects which CA signed the certificate the member serves. Those are
// independent on a real cluster mid-migration, and the point of the survey is
// that it reads both.
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
	// Hand back the node's real IP as the address. The dial seam below sends the
	// connection to the loopback listener, but the probe identity-checks the
	// address it was ASKED for — so this exercises the remote peer path, which
	// dialling 127.0.0.1 would silently bypass in favour of the local one.
	redirectProbeDial(t, ln.Addr().String())
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort(tIP, port)
}

// redirectProbeDial points the probes' one network call at a loopback listener
// while leaving the address they judge untouched. An optional timeout overrides
// the production probeTimeout, so tests that exercise timeout paths stay fast.
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

// surveyAt is ProbeEtcdTrustAt, named for what the tests are asserting.
func surveyAt(t *testing.T, addr string) EtcdRemoteTrust {
	t.Helper()
	return ProbeEtcdTrustAt(addr)
}

// TestSurveyReadsBothAxes walks the four states, moving one axis at a time.
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
		// Trust and leaves disagree: this member's etcd would reject its own
		// apiserver. On disk EtcdTrust.State calls this not-migrated because a
		// follower passes through it mid-phase; as a SURVEYED resting state it is
		// a broken node and must not be classified.
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

// rootSignedEtcdLeaf mints an etcd server leaf under the CLUSTER ROOT, i.e. what
// an unmigrated member serves, and returns its basename.
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

// TestSurveyIsUnknownWhenEtcdIsUnreachable. "etcd is down on that node" is not a
// gap in the survey — it is the answer, and the answer is unknown, which
// refuses. A survey that guessed here would let drop-root proceed against a
// member nobody could reach.
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

// TestSurveyRejectsAnImpostorListener is the peer-identity check, and it is the
// one the remote form could not inherit: peerIsLocalEtcd compares the served
// leaf byte-for-byte against THIS node's etcd.crt, which by construction cannot
// work against a peer. The replacement identifies the leaf structurally, and it
// must still refuse a listener that is not that node's etcd.
func TestSurveyRejectsAnImpostorListener(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// An impostor serving a certificate signed by a CA this node does not hold.
	// On a real node this is any local process binding 2379 while etcd is down.
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

// TestSurveyRefusesALeafFromAnUnknownCA. A member serving a certificate this
// node cannot place — a root a rotation replaced and then discarded, say —
// leaves the leaf axis unreadable. Unreadable is unknown, not "probably fine".
//
// A hand-built EtcdRemoteTrust would exercise none of the probe — it would
// pass against an implementation that classifies the shape as not-migrated.
// This stands up a listener serving a foreign leaf that is
// otherwise structurally valid — right CN, right SAN — so the ONLY thing that
// can reject it is the issuer check.
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

// TestSurveyTreatsAnUnnameableAnchorAsUnknown. An acceptable-CA Subject the
// surveying node cannot name may well be a Kubernetes root — which is exactly
// what drop-root must not be run against. Refuse rather than classify.
func TestSurveyTreatsAnUnnameableAnchorAsUnknown(t *testing.T) {
	p := EtcdRemoteTrust{TrustsEtcdAnchor: true, LeafUnderEtcdCA: true, UnknownAnchors: 1}
	if got := p.State(); got != EtcdStateUnknown {
		t.Errorf("an unnameable trust anchor resolved to %s — the node would be treated as migrated", got)
	}
	if p.Migrated() {
		t.Error("Migrated() is true for a node with an anchor we cannot name")
	}
}

// stalledListener stands up a RAW TCP listener that accepts and never writes:
// exactly what a just-restarted etcd looks like while its listener is open but
// it is still replaying its WAL. The probe's read blocks until its (shortened)
// deadline.
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

// TestSurveyStalledListenerIsTransient pins the resume rule:
// a TCP connect that succeeds and then a handshake that never answers is etcd
// STARTING UP, not a node that cannot be classified. It is still unknown —
// never a plausible-looking state — but it is marked transient so the
// orchestrator may re-probe instead of refusing on the first attempt.
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

// TestSurveyRefusedIsTransient. Nothing is listening YET is the same
// just-restarted situation seen a moment earlier, so it is transient too —
// still unknown, still refusing, but re-probeable.
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

// TestSurveyDefiniteUnknownsAreNotTransient. Transiency is reserved for etcd
// not serving yet; every other unknown is definite and must stay non-transient,
// because a retry that re-asked an impostor would eventually read the same
// impostor. The two live cases reuse the impostor and foreign-CA stands from
// the tests above; the synthetic ones cover the states only a full probe can
// otherwise reach.
func TestSurveyDefiniteUnknownsAreNotTransient(t *testing.T) {
	genInto(t)
	migrateAll(t)

	// An impostor: a structurally valid etcd leaf signed by a root this node
	// does not hold.
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
	// Surveyed now: the next remoteEtcd points the one dial seam at its own
	// listener, whatever address is asked for.
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
	// Each answer came from its own listener: the impostor trusts only a root,
	// the foreign stand only an etcd CA.
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

	// The remaining definite unknowns are reachable only through State()'s
	// other branches; none of them sets Err, so transiency is false by
	// construction — pinned here so it stays that way.
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

// TestSurveyTrustBothAfterStall is the resume scenario end to end at the probe
// level: the first connection stalls (etcd replaying its WAL), a later one is
// answered by an etcd that has rejoined raft and widened its trust — and the
// same survey that refused before now classifies trust-both.
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

// TestSurveyDialTimeoutIsNotTransient pins the line the transient kinds stop
// at: a dial that never connected — host down, route gone — is reported as
// unreachable but is NOT transient, because nothing on that host is going to
// start answering on its own. Only refused and answered-but-stalled dials,
// which a restarting etcd produces, carry errEtcdNotServing.
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
