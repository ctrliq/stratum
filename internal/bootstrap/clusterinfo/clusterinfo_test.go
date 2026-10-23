package clusterinfo

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestAuthenticatedKubeletClientCAMode(t *testing.T) {
	tok := mustToken(t, testID+"."+testSecret)
	for _, tc := range []struct {
		name, marker string
		wantSplit    bool
		wantErr      bool
	}{
		{name: "split", marker: `{"mode":"split"}`, wantSplit: true},
		{name: "legacy", marker: `{"mode":"legacy"}`},
		{name: "older cluster-info has no marker"},
		{name: "unknown mode", marker: `{"mode":"other"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := clientcmdapi.NewConfig()
			cfg.Clusters[""] = &clientcmdapi.Cluster{Server: "https://apiserver"}
			if tc.marker != "" {
				cfg.Extensions[KubeletClientCAModeExtension] = &runtime.Unknown{Raw: []byte(tc.marker)}
			}
			raw, err := clientcmd.Write(*cfg)
			if err != nil {
				t.Fatal(err)
			}
			auth, err := Authenticate(signed(string(raw), testID, testSecret), tok)
			if err != nil {
				t.Fatal(err)
			}
			got, err := auth.KubeletClientCASplit()
			if (err != nil) != tc.wantErr || got != tc.wantSplit {
				t.Fatalf("split=%t, err=%v; want split=%t, error=%t", got, err, tc.wantSplit, tc.wantErr)
			}
			if tc.marker == `{"mode":"split"}` {
				cfg.Extensions[KubeletClientCAModeExtension] = &runtime.Unknown{Raw: []byte(`{"mode":"legacy"}`)}
				tampered, err := clientcmd.Write(*cfg)
				if err != nil {
					t.Fatal(err)
				}
				data := signed(string(raw), testID, testSecret)
				data[KubeConfigKey] = string(tampered)
				if _, err := Authenticate(data, tok); err == nil {
					t.Fatal("accepted a mode change without a new cluster-info signature")
				}
			}
		})
	}
}

// makeCA returns a self-signed CA cert (PEM) and its
// "sha256:<hex of SubjectPublicKeyInfo>" pin.
func makeCA(t *testing.T) (pemBytes []byte, pin string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PinFor(cert)
}

// testCert returns a parsed certificate plus its pin, for tests that need the
// x509 value rather than the PEM makeCA hands back.
func testCert(t *testing.T) (*x509.Certificate, string) {
	t.Helper()
	caPEM, pin := makeCA(t)
	blk, _ := pem.Decode(caPEM)
	if blk == nil {
		t.Fatal("makeCA produced un-decodable PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c, pin
}

// kubeconfigWith builds a cluster-info kubeconfig carrying caPEM, shaped like the
// one token.ensureClusterInfo publishes.
func kubeconfigWith(caPEM []byte) string {
	return "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(caPEM) +
		"\n    server: https://10.0.0.1:6443\n  name: \"\"\ncontexts: null\ncurrent-context: \"\"\nkind: Config\nusers: null\n"
}

// clusterEntry is one cluster stanza of a cluster-info kubeconfig under test.
type clusterEntry struct {
	server string
	caPEM  []byte
}

// kubeconfigWithEntries builds a cluster-info kubeconfig with one cluster entry
// per entry, shaped like the one token.ensureClusterInfo publishes. It is
// kubeconfigWith extended to a second entry, which is the only way to reach the
// ambiguity Parse must refuse. An entry with a nil caPEM carries no
// certificate-authority-data at all.
func kubeconfigWithEntries(entries ...clusterEntry) string {
	s := "apiVersion: v1\nclusters:\n"
	for i, e := range entries {
		s += "- cluster:\n"
		if len(e.caPEM) > 0 {
			s += "    certificate-authority-data: " + base64.StdEncoding.EncodeToString(e.caPEM) + "\n"
		}
		s += "    server: " + e.server + "\n"
		s += "  name: \"c" + strconv.Itoa(i) + "\"\n"
	}
	return s + "contexts: null\ncurrent-context: \"\"\nkind: Config\nusers: null\n"
}

// signed returns the ConfigMap data map for kubeconfig, signed for tokenID.
func signed(kubeconfig, tokenID, tokenSecret string) map[string]string {
	return map[string]string{
		KubeConfigKey:                   kubeconfig,
		JWSSignatureKeyPrefix + tokenID: computeDetachedSignature(kubeconfig, tokenID, tokenSecret),
	}
}

// mustToken builds a Token for tests. ParseToken is the only constructor, which
// is the point of the type.
func mustToken(t *testing.T, s string) Token {
	t.Helper()
	tok, err := ParseToken(s)
	if err != nil {
		t.Fatalf("fixture token %q: %v", s, err)
	}
	return tok
}

const (
	testID     = "abcdef"
	testSecret = "0123456789abcdef"
)

// TestGoldenVectorMatchesUpstreamSigner is the interop contract with stock
// kube-controller-manager output, and it is the reason this package can carry no
// dependency.
//
// PROVENANCE — read before touching the constant. `want` was produced by
// k8s.io/cluster-bootstrap/token/jws.ComputeDetachedSignature at v0.31.1, the
// exact function the bootstrapsigner calls, run against the inputs below. It is
// hardcoded on purpose. Regenerating it from computeDetachedSignature (this
// package's own code) would make the test assert that our implementation equals
// itself, which proves nothing at all and silently deletes the only evidence we
// have that stratum and the Kubernetes bootstrapsigner agree on the wire format.
//
// If this ever fails, the wire format has drifted. Do not update the constant to
// match — find out why.
func TestGoldenVectorMatchesUpstreamSigner(t *testing.T) {
	const (
		content = "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: AAAA\n    server: https://10.0.0.1:6443\n  name: \"\"\nkind: Config\n"
		want    = "eyJhbGciOiJIUzI1NiIsImtpZCI6ImFiY2RlZiJ9..Z7JMpYpPuYGTtKFWePl2ZzN6L7cLZ5U315XnpdeoU0w"
	)
	if got := computeDetachedSignature(content, testID, testSecret); got != want {
		t.Errorf("detached JWS drifted from the upstream signer\n got  %q\n want %q", got, want)
	}
}

// TestDetachedSignatureShape locks the RFC 7515 Appendix F encoding: three
// dot-separated parts with an elided payload, and a protected header of exactly
// {"alg":"HS256","kid":"<id>"}.
func TestDetachedSignatureShape(t *testing.T) {
	sig := computeDetachedSignature("content", testID, testSecret)
	parts := strings.Split(sig, ".")
	if len(parts) != 3 {
		t.Fatalf("compact JWS must have 3 parts, got %d in %q", len(parts), sig)
	}
	if parts[1] != "" {
		t.Errorf("payload must be detached (empty middle part), got %q", parts[1])
	}
	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("protected header is not base64url: %v", err)
	}
	if want := `{"alg":"HS256","kid":"abcdef"}`; string(hdr) != want {
		t.Errorf("protected header = %s, want %s", hdr, want)
	}
}

func TestParseToken(t *testing.T) {
	tests := []struct {
		name, token        string
		wantID, wantSecret string
		wantErr            bool
	}{
		{"valid", "abcdef.0123456789abcdef", "abcdef", "0123456789abcdef", false},
		{"empty", "", "", "", true},
		{"no separator", "abcdef0123456789abcdef", "", "", true},
		{"id too short", "abcde.0123456789abcdef", "", "", true},
		{"secret too short", "abcdef.0123456789abcde", "", "", true},
		{"uppercase", "ABCDEF.0123456789abcdef", "", "", true},
		{"json metacharacter in id", `ab"de.0123456789abcdef`, "", "", true},
		{"trailing newline", "abcdef.0123456789abcdef\n", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := ParseToken(tt.token)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tok.id != tt.wantID || tok.secret != tt.wantSecret {
				t.Errorf("got (%q, %q), want (%q, %q)", tok.id, tok.secret, tt.wantID, tt.wantSecret)
			}
		})
	}
}

// TestAuthenticate covers the verification failure paths. Each one must fail
// closed: an unauthenticated payload never reaches TrustBundle.
func TestAuthenticate(t *testing.T) {
	caPEM, _ := makeCA(t)
	kc := kubeconfigWith(caPEM)
	good := signed(kc, testID, testSecret)

	t.Run("valid signature", func(t *testing.T) {
		if _, err := Authenticate(good, mustToken(t, testID+"."+testSecret)); err != nil {
			t.Errorf("valid signature rejected: %v", err)
		}
	})

	t.Run("missing signature is NoSignatureError", func(t *testing.T) {
		_, err := Authenticate(map[string]string{KubeConfigKey: kc}, mustToken(t, testID+"."+testSecret))
		if err == nil {
			t.Fatal("unsigned cluster-info accepted")
		}
		var missing *NoSignatureError
		if !errors.As(err, &missing) {
			t.Errorf("want *NoSignatureError so the caller can retry it, got %T: %v", err, err)
		}
	})

	t.Run("wrong token secret", func(t *testing.T) {
		if _, err := Authenticate(good, mustToken(t, testID+".ffffffffffffffff")); err == nil {
			t.Error("signature accepted under the wrong token secret")
		}
	})

	t.Run("signature for a different token id", func(t *testing.T) {
		other := signed(kc, "zzzzzz", testSecret)
		if _, err := Authenticate(other, mustToken(t, testID+"."+testSecret)); err == nil {
			t.Error("accepted a ConfigMap carrying no signature for our token id")
		}
	})

	t.Run("kubeconfig mutated after signing", func(t *testing.T) {
		tampered := map[string]string{
			KubeConfigKey:                  kc + "\n# appended by an attacker\n",
			JWSSignatureKeyPrefix + testID: good[JWSSignatureKeyPrefix+testID],
		}
		if _, err := Authenticate(tampered, mustToken(t, testID+"."+testSecret)); err == nil {
			t.Error("accepted a kubeconfig that changed after it was signed")
		}
	})

	t.Run("alg swapped to none", func(t *testing.T) {
		// REGRESSION GUARD, not a test of a defended risk. Under
		// recompute-and-compare we never parse the received header, so alg
		// confusion is structurally impossible and this fails as an ordinary
		// byte mismatch. It starts doing real work the day someone switches the
		// implementation to parse-the-received-header — which is exactly the
		// change that must not be made silently.
		parts := strings.Split(good[JWSSignatureKeyPrefix+testID], ".")
		none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"` + testID + `"}`))
		forged := map[string]string{
			KubeConfigKey:                  kc,
			JWSSignatureKeyPrefix + testID: none + ".." + parts[2],
		}
		if _, err := Authenticate(forged, mustToken(t, testID+"."+testSecret)); err == nil {
			t.Error("accepted a token whose protected header claimed alg=none")
		}
	})

	t.Run("missing kubeconfig key", func(t *testing.T) {
		if _, err := Authenticate(map[string]string{JWSSignatureKeyPrefix + testID: "x"}, mustToken(t, testID+"."+testSecret)); err == nil {
			t.Error("accepted a ConfigMap with no kubeconfig")
		}
	})
}

// TestAuthenticateRejectsZeroToken closes the input-side blind spot. An outside
// package cannot build a populated Token, but it can write Token{} — and an
// empty secret would key the HMAC with the empty string, which anyone can
// compute, turning the whole control into a no-op for attacker-supplied bytes.
func TestAuthenticateRejectsZeroToken(t *testing.T) {
	kc := kubeconfigWith(func() []byte { p, _ := makeCA(t); return p }())
	// A signature the attacker can forge without knowing any secret.
	forged := map[string]string{
		KubeConfigKey:         kc,
		JWSSignatureKeyPrefix: computeDetachedSignature(kc, "", ""),
	}
	if _, err := Authenticate(forged, Token{}); err == nil {
		t.Error("zero Token accepted: an empty secret makes the MAC forgeable by anyone")
	}
}

// TestTrustBundleZeroValueRejected is the compiler's blind spot: an outside
// package cannot build a POPULATED Authenticated, but it can write
// Authenticated{}. That zero value must fail closed rather than return an empty
// trust bundle.
func TestTrustBundleZeroValueRejected(t *testing.T) {
	_, err := (Authenticated{}).TrustBundle("sha256:whatever")
	if err == nil {
		t.Fatal("TrustBundle on an unauthenticated zero value returned no error")
	}
	// Assert the SPECIFIC error: without this, deleting the guard still passes,
	// because clientcmd.Load("") succeeds and execution falls through to
	// "cluster-info has no CA" — a different branch entirely.
	if !strings.Contains(err.Error(), "unauthenticated value") {
		t.Errorf("got %v; want the explicit unauthenticated-value guard, not an incidental downstream error", err)
	}
}

func TestTrustBundle(t *testing.T) {
	caPEM, pin := makeCA(t)
	otherPEM, otherPin := makeCA(t)

	authOf := func(t *testing.T, caData []byte) Authenticated {
		t.Helper()
		kc := kubeconfigWith(caData)
		a, err := Authenticate(signed(kc, testID, testSecret), mustToken(t, testID+"."+testSecret))
		if err != nil {
			t.Fatalf("fixture failed to authenticate: %v", err)
		}
		return a
	}

	t.Run("single cert round-trips", func(t *testing.T) {
		out, err := authOf(t, caPEM).TrustBundle(pin)
		if err != nil {
			t.Fatal(err)
		}
		if got := countCerts(t, out); got != 1 {
			t.Errorf("bundle has %d certs, want 1", got)
		}
	})

	// Staged CA rotation: cluster-info carries old‖new while the token pins only
	// one of them. Both directions must be accepted, and BOTH certs must survive
	// into the returned bundle — narrowing to the pinned one is what would break
	// a rotating cluster, and this is the assertion that catches it.
	t.Run("rotation bundle keeps both certs, pinned either way", func(t *testing.T) {
		bundle := append(append([]byte{}, caPEM...), otherPEM...)
		for name, p := range map[string]string{"pinned to first": pin, "pinned to second": otherPin} {
			t.Run(name, func(t *testing.T) {
				out, err := authOf(t, bundle).TrustBundle(p)
				if err != nil {
					t.Fatalf("rotation bundle rejected: %v", err)
				}
				got := pinSet(t, out)
				if len(got) != 2 || !got[pin] || !got[otherPin] {
					t.Errorf("bundle narrowed to %v; both CAs must survive a rotation", got)
				}
			})
		}
	})

	t.Run("pin matching neither cert is rejected", func(t *testing.T) {
		if _, err := authOf(t, caPEM).TrustBundle("sha256:deadbeef"); err == nil {
			t.Error("accepted a bundle matching no pin")
		}
	})

	// A CERTIFICATE-typed block that does not parse is a hard error, not a
	// skipped block.
	t.Run("unparseable CERTIFICATE block is a hard error", func(t *testing.T) {
		junk := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})
		bundle := append(append([]byte{}, junk...), caPEM...)
		if _, err := authOf(t, bundle).TrustBundle(pin); err == nil {
			t.Error("a malformed CERTIFICATE block was skipped; it must fail closed")
		}
	})

	// An authenticated bundle with zero parseable certificates is an error.
	// DiscoverCA must never return an empty trust bundle — an empty clientCAFile
	// is not "trust nothing", it is a kubelet that fails to start or a trust
	// store silently narrowed to nothing.
	t.Run("zero parseable certificates is an error", func(t *testing.T) {
		only := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")})
		out, err := authOf(t, only).TrustBundle(pin)
		if err == nil {
			t.Fatalf("accepted a bundle with no certificate, returned %d bytes", len(out))
		}
		if len(out) != 0 {
			t.Errorf("returned %d bytes alongside an error", len(out))
		}
		// Pin the branch by name: checkPin also fails closed on an empty slice,
		// so a bare err != nil check passes with the zero-cert guard deleted.
		if !strings.Contains(err.Error(), "carries no parseable certificate") {
			t.Errorf("got %v; want the explicit zero-certificate guard", err)
		}
	})

	// Canonical re-encoding: non-CERTIFICATE blocks and trailing bytes in the
	// authenticated field are dropped rather than travelling on into
	// ca-bundle.crt.
	t.Run("drops non-CERTIFICATE blocks and trailing bytes", func(t *testing.T) {
		noise := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("ignored")})
		bundle := append(append(append([]byte{}, noise...), caPEM...), []byte("trailing junk, not PEM\n")...)
		out, err := authOf(t, bundle).TrustBundle(pin)
		if err != nil {
			t.Fatal(err)
		}
		if countCerts(t, out) != 1 {
			t.Errorf("bundle = %q, want exactly the one certificate", out)
		}
		if strings.Contains(string(out), "PUBLIC KEY") || strings.Contains(string(out), "trailing junk") {
			t.Errorf("non-certificate material survived into the trust bundle: %q", out)
		}
	})
}

func countCerts(t *testing.T, bundle []byte) int {
	t.Helper()
	return len(pinSet(t, bundle))
}

func pinSet(t *testing.T, bundle []byte) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	rest := bundle
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return out
		}
		if blk.Type != "CERTIFICATE" {
			t.Errorf("re-encoded bundle contains a %q block", blk.Type)
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatalf("re-encoded bundle has an unparseable cert: %v", err)
		}
		out[PinFor(c)] = true
	}
}

// TestMalformedPinIsDiagnosedAsMalformed pins the DIAGNOSIS, not just the refusal.
//
// Every input below was already refused before this test existed — no cert can
// match a malformed pin — but all of them were refused with "CA hash mismatch:
// ... (possible MITM)", which sends an operator hunting an attacker over a
// copy-paste error. The failure was closed; the message was wrong.
func TestMalformedPinIsDiagnosedAsMalformed(t *testing.T) {
	cert, good := testCert(t)
	upper := "sha256:" + strings.ToUpper(strings.TrimPrefix(good, "sha256:"))

	for _, tc := range []struct {
		name, pin string
	}{
		{"empty", ""},
		{"missing sha256: prefix", strings.TrimPrefix(good, "sha256:")},
		{"uppercase hex", upper},
		{"leading space", " " + good},
		{"trailing newline", good + "\n"},
		{"truncated", good[:len(good)-1]},
		{"wrong algorithm", "sha512:" + strings.TrimPrefix(good, "sha256:")},
		{"not hex", "sha256:" + strings.Repeat("z", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPin([]*x509.Certificate{cert}, tc.pin)
			if err == nil {
				t.Fatalf("accepted malformed pin %q", tc.pin)
			}
			if strings.Contains(err.Error(), "possible MITM") {
				t.Errorf("malformed pin diagnosed as an attack:\n  %v", err)
			}
			if tc.pin != "" && !strings.Contains(err.Error(), "malformed") {
				t.Errorf("error does not say the pin is malformed:\n  %v", err)
			}
		})
	}

	// A well-formed pin that simply does not match MUST still read as a mismatch —
	// that is the case where "possible MITM" is the right thing to say.
	other := strings.Repeat("a", 64)
	err := checkPin([]*x509.Certificate{cert}, "sha256:"+other)
	if err == nil {
		t.Fatal("accepted a well-formed pin that matches nothing")
	}
	if !strings.Contains(err.Error(), "possible MITM") {
		t.Errorf("a genuine mismatch lost its MITM warning:\n  %v", err)
	}
}

// TestParseRefusesAmbiguousClusterInfo pins the one reading rule for the
// cluster-info kubeconfig. token.ensureClusterInfo writes the document with
// exactly one cluster entry, but three readers decode it back — TrustBundle
// on the joining node, the verified re-fetch in bootstrap, RepinCA on the
// control plane — and a second entry, e.g. a VIP beside a node
// address, must make them refuse together rather than each answer by Go map
// order.
func TestParseRefusesAmbiguousClusterInfo(t *testing.T) {
	caPEM, pin := makeCA(t)
	otherPEM, _ := makeCA(t)
	const (
		server      = "https://10.0.0.1:6443"
		otherServer = "https://10.0.0.2:6443"
	)

	t.Run("one entry returns its server and CA", func(t *testing.T) {
		gotServer, ca, err := Parse([]byte(kubeconfigWithEntries(clusterEntry{server: server, caPEM: caPEM})))
		if err != nil {
			t.Fatal(err)
		}
		if gotServer != server {
			t.Errorf("server = %q, want %q", gotServer, server)
		}
		if string(ca) != string(caPEM) {
			t.Errorf("CA = %q, want the document's certificate-authority-data", ca)
		}
	})

	t.Run("two identical entries accepted", func(t *testing.T) {
		gotServer, ca, err := Parse([]byte(kubeconfigWithEntries(
			clusterEntry{server: server, caPEM: caPEM},
			clusterEntry{server: server, caPEM: caPEM},
		)))
		if err != nil {
			t.Fatalf("two agreeing entries rejected: %v", err)
		}
		if gotServer != server || string(ca) != string(caPEM) {
			t.Errorf("got (%q, %q), want (%q, %q)", gotServer, ca, server, caPEM)
		}
	})

	t.Run("two entries with different CA data refused", func(t *testing.T) {
		_, _, err := Parse([]byte(kubeconfigWithEntries(
			clusterEntry{server: server, caPEM: caPEM},
			clusterEntry{server: server, caPEM: otherPEM},
		)))
		if err == nil {
			t.Fatal("differing CA bundles accepted; the answer would depend on map order")
		}
		if !strings.Contains(err.Error(), "refusing to guess") {
			t.Errorf("err = %v, want it to say it refuses to guess", err)
		}
	})

	t.Run("two entries with different servers refused", func(t *testing.T) {
		_, _, err := Parse([]byte(kubeconfigWithEntries(
			clusterEntry{server: server, caPEM: caPEM},
			clusterEntry{server: otherServer, caPEM: caPEM},
		)))
		if err == nil {
			t.Fatal("differing servers accepted; the answer would depend on map order")
		}
		if !strings.Contains(err.Error(), "refusing to guess") {
			t.Errorf("err = %v, want it to say it refuses to guess", err)
		}
	})

	// TrustBundle reads the same document on the join path, so the same
	// ambiguity must be refused there too — and the refusal must come from the
	// ambiguity guard, not from an incidental pin mismatch against whichever
	// entry map order happened to visit first.
	t.Run("TrustBundle refuses the differing-CA document", func(t *testing.T) {
		kc := kubeconfigWithEntries(
			clusterEntry{server: server, caPEM: caPEM},
			clusterEntry{server: server, caPEM: otherPEM},
		)
		a, err := Authenticate(signed(kc, testID, testSecret), mustToken(t, testID+"."+testSecret))
		if err != nil {
			t.Fatalf("fixture failed to authenticate: %v", err)
		}
		_, err = a.TrustBundle(pin)
		if err == nil {
			t.Fatal("TrustBundle accepted a document whose entries disagree on the CA")
		}
		if !strings.Contains(err.Error(), "refusing to guess") {
			t.Errorf("err = %v, want the ambiguity refusal, not an incidental downstream error", err)
		}
	})
}
