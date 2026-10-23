package bootstrap

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	testToken  = "abcdef.0123456789abcdef"
	testID     = "abcdef"
	testSecret = "0123456789abcdef"
)

// makeCA returns a self-signed CA cert (PEM) and its
// "sha256:<hex of SubjectPublicKeyInfo>" hash.
func makeCA(t *testing.T) (pemBytes []byte, hash string) {
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
	pemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return pemBytes, "sha256:" + hex.EncodeToString(sum[:])
}

func kubeconfigWith(caPEM []byte) string {
	return "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(caPEM) +
		"\n    server: https://10.0.0.1:6443\n  name: \"\"\ncontexts: null\ncurrent-context: \"\"\nkind: Config\nusers: null\n"
}

// clusterInfoServer serves a cluster-info ConfigMap over TLS with a self-signed
// certificate — exactly what an on-path attacker presents, and exactly what
// fetchClusterInfoData's InsecureSkipVerify transport accepts. It counts
// requests so tests can assert that no request was made at all.
func clusterInfoServer(t *testing.T, data map[string]string) (url string, requests *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "data": data,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests
}

// TestMain shortens the signature-wait budget so the unsigned-cluster-info tests
// do not each burn the production 30s. Retry behaviour is asserted by REQUEST
// COUNT (see TestDiscoverCAMissingSignatureCarriesRemediation, which requires
// more than one attempt, and TestDiscoverCARejectsTamperedSignedBundleImmediately,
// which requires exactly one), so shortening the wall-clock budget does not
// weaken what is checked.
func TestMain(m *testing.M) {
	signatureWaitInterval = 5 * time.Millisecond
	signatureWaitTimeout = 60 * time.Millisecond
	reachWaitInterval = 5 * time.Millisecond
	reachWaitTimeout = 200 * time.Millisecond
	os.Exit(m.Run())
}

func signedData(kubeconfig string) map[string]string {
	return map[string]string{
		clusterinfo.KubeConfigKey:                  kubeconfig,
		clusterinfo.JWSSignatureKeyPrefix + testID: signatureFor(kubeconfig),
	}
}

// signatureFor produces the detached JWS the bootstrapsigner publishes, built
// here from the standard library rather than from the production code under
// test, so these fixtures are an independent check on it. The authority on the
// encoding is TestGoldenVectorMatchesUpstreamSigner in the clusterinfo package,
// which pins real upstream output; if this and the implementation ever disagree,
// that test says which one is wrong.
func signatureFor(kubeconfig string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"` + testID + `"}`))
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(header + "." + base64.RawURLEncoding.EncodeToString([]byte(kubeconfig))))
	return header + ".." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestDiscoverCARejectsUnpinnedCAsInBundle is the regression test for accepting
// an unpinned CA from an authenticated bundle.
//
// DiscoverCA must not trust a fetched bundle on a first-match pin alone:
// trusted that way, `[attacker-CA ‖ genuine-CA]` — pinned to the genuine
// cert — is accepted and the attacker's CA is written to ca-bundle.crt, the
// kubelet's x509.clientCAFile.
//
// The bundle is unsigned because that is the actual attack: with no JWS
// published and none verified, an on-path attacker needs no credentials at all.
func TestDiscoverCARejectsUnpinnedCAsInBundle(t *testing.T) {
	attackerPEM, attackerPin := makeCA(t)
	genuinePEM, genuinePin := makeCA(t)
	bundle := append(append([]byte{}, attackerPEM...), genuinePEM...)

	url, _ := clusterInfoServer(t, map[string]string{
		clusterinfo.KubeConfigKey: kubeconfigWith(bundle),
	})

	out, err := DiscoverCA(url, testToken, genuinePin)
	if err == nil {
		t.Fatal("unsigned cluster-info accepted: an on-path attacker can inject a trusted client CA")
	}
	// Belt and braces: whatever the resolution, the attacker's CA must never be
	// in what the caller writes to ca-bundle.crt.
	if containsPin(t, out, attackerPin) {
		t.Errorf("attacker CA present in the returned bundle (%d bytes)", len(out))
	}
}

// TestDiscoverCARejectsSignedAttackerBundle is the same bypass one level up: a
// well-formed signature over a bundle that does not contain the pinned CA. The
// JWS authenticates the document; the pin is what binds it to the CA we expect.
// Both must hold.
func TestDiscoverCARejectsSignedAttackerBundle(t *testing.T) {
	attackerPEM, _ := makeCA(t)
	_, genuinePin := makeCA(t)

	url, _ := clusterInfoServer(t, signedData(kubeconfigWith(attackerPEM)))

	if _, err := DiscoverCA(url, testToken, genuinePin); err == nil {
		t.Fatal("accepted a signed bundle carrying no CA matching the pin")
	}
}

// TestDiscoverCAAcceptsSignedRotationBundle is the counterweight: the same
// two-CA shape, correctly signed, must be ACCEPTED with both certificates
// intact. This is the test that fails if someone later narrows the bundle to the
// pinned certificate and breaks staged CA rotation.
func TestDiscoverCAAcceptsSignedRotationBundle(t *testing.T) {
	oldPEM, oldPin := makeCA(t)
	newPEM, newPin := makeCA(t)
	bundle := append(append([]byte{}, oldPEM...), newPEM...)

	url, _ := clusterInfoServer(t, signedData(kubeconfigWith(bundle)))

	for name, pin := range map[string]string{"pinned to old CA": oldPin, "pinned to new CA": newPin} {
		t.Run(name, func(t *testing.T) {
			out, err := DiscoverCA(url, testToken, pin)
			if err != nil {
				t.Fatalf("mid-rotation bundle rejected: %v", err)
			}
			if !containsPin(t, out, oldPin) || !containsPin(t, out, newPin) {
				t.Error("bundle narrowed: a node joining mid-rotation needs both CAs")
			}
		})
	}
}

// TestDiscoverCAValidatesTokenBeforeAnyRequest: a malformed token must fail
// without contacting an endpoint we cannot yet verify, and — the property C2
// exists to guarantee — there is no token value that skips verification.
func TestDiscoverCAValidatesTokenBeforeAnyRequest(t *testing.T) {
	caPEM, pin := makeCA(t)
	url, requests := clusterInfoServer(t, signedData(kubeconfigWith(caPEM)))

	for _, tok := range []string{"", "not-a-token", "abcdef", ".", "abcdef."} {
		if _, err := DiscoverCA(url, tok, pin); err == nil {
			t.Errorf("token %q accepted", tok)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("made %d request(s) with a malformed token; validation must precede network I/O", n)
	}
}

// TestDiscoverCARequiresHTTPS: a plaintext endpoint would put the bootstrap
// token on the wire as a bearer credential during TLS bootstrap — and that token
// is the HMAC key this whole control rests on.
//
// The server is PLAINTEXT and serves valid, correctly-signed cluster-info, so
// the join would succeed were it not for the scheme guard — only requireHTTPS
// can fail this request.
func TestDiscoverCARequiresHTTPS(t *testing.T) {
	caPEM, pin := makeCA(t)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "data": signedData(kubeconfigWith(caPEM)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	_, err = DiscoverCA(srv.URL, testToken, pin)
	if err == nil {
		t.Fatal("accepted an http:// server; the bootstrap token would go out in cleartext")
	}
	// Pin the guard's own message: without this the test passes on any error.
	if !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("got %v; want the explicit scheme guard", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("made %d request(s) to a plaintext endpoint; the scheme check must precede network I/O", n)
	}
}

// TestDiscoverCARefusesRedirects: cluster-info is one fixed endpoint. Following a
// redirect would let a server we have not authenticated steer this pre-auth
// request to an arbitrary host, or downgrade it to plaintext.
func TestDiscoverCARefusesRedirects(t *testing.T) {
	caPEM, pin := makeCA(t)
	var targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"data": signedData(kubeconfigWith(caPEM))})
		_, _ = w.Write(b)
	}))
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := DiscoverCA(redirector.URL, testToken, pin)
	if err == nil {
		t.Fatal("followed a redirect from an unauthenticated server")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("got %v; want the redirect surfaced as a non-200 status", err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target was contacted %d time(s)", n)
	}
}

// TestDiscoverCAStopsReadingAtTheLimit pins the BOUND, not merely the rejection.
//
// TestDiscoverCARejectsOversizedBody below sends maxClusterInfoBytes+64, so the
// `len(body) > max` check fires whether or not the read is bounded — removing the
// io.LimitReader leaves that test green. The property that actually matters is
// that a hostile endpoint streaming gigabytes cannot make the joiner buffer them
// before deciding: the transport skips TLS verification and DiscoverCA re-fetches
// on a retry loop, so an unbounded read is a remote memory-exhaustion primitive.
//
// This serves far more than the limit and asserts on how much the server managed
// to push. With the LimitReader the client stops at max+1 and the handler's next
// Write fails; without it, io.ReadAll drains everything.
func TestDiscoverCAStopsReadingAtTheLimit(t *testing.T) {
	const chunk = 1 << 20
	const total = 64 * chunk // far beyond any bound the code might use

	var mu sync.Mutex
	served := 0

	_, pin := makeCA(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf := make([]byte, chunk)
		for i := range buf {
			buf[i] = 'A'
		}
		for sent := 0; sent < total; sent += chunk {
			n, err := w.Write(buf)
			mu.Lock()
			served += n
			mu.Unlock()
			if err != nil {
				return // client stopped reading: this is the bounded case
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	if _, err := DiscoverCA(srv.URL, testToken, pin); err == nil {
		t.Fatal("accepted an unbounded cluster-info body")
	}

	mu.Lock()
	got := served
	mu.Unlock()

	// Generous slack for socket and http server buffering — the point is to
	// separate "stopped near the limit" from "drained 64 MiB", not to pin an exact
	// byte count.
	const ceiling = 16 * chunk
	if got > ceiling {
		t.Errorf("server pushed %d bytes; the client did not stop near the %d-byte limit "+
			"(an unbounded read would drain all %d)", got, maxClusterInfoBytes, total)
	}
}

// TestDiscoverCARejectsOversizedBody: the body is attacker-controlled before
// authentication and the retry loop re-fetches, so the read must be bounded.
func TestDiscoverCARejectsOversizedBody(t *testing.T) {
	_, pin := makeCA(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"kubeconfig":"` + strings.Repeat("A", maxClusterInfoBytes+64) + `"}}`))
	}))
	defer srv.Close()

	_, err := DiscoverCA(srv.URL, testToken, pin)
	if err == nil {
		t.Fatal("accepted an oversized cluster-info body")
	}
	// The cap bounds a hostile endpoint, but the way a real cluster reaches it is
	// benign accumulation of bootstrap-token signatures. A bare read error would
	// make a token-lifecycle problem look like a network fault on the joiner, so
	// the message has to name the cause and where the lifecycle work lives.
	for _, want := range []string{
		"exceeds the " + strconv.Itoa(maxClusterInfoBytes) + "-byte limit",
		"bootstrap-token signatures", "never expire",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("size-limit error missing %q\ngot: %v", want, err)
		}
	}
}

// TestDiscoverCARejectsTamperedSignedBundleImmediately: a signature that is
// present and does not verify is the MITM signal. It must fail fast, not sit in
// the missing-signature retry loop for the full budget.
func TestDiscoverCARejectsTamperedSignedBundleImmediately(t *testing.T) {
	caPEM, pin := makeCA(t)
	attackerPEM, _ := makeCA(t)

	kc := kubeconfigWith(caPEM)
	data := signedData(kc)
	// Signature stays; the document it covers is swapped underneath it.
	data[clusterinfo.KubeConfigKey] = kubeconfigWith(append(append([]byte{}, attackerPEM...), caPEM...))
	url, requests := clusterInfoServer(t, data)

	if _, err := DiscoverCA(url, testToken, pin); err == nil {
		t.Fatal("accepted a bundle whose signature does not cover it")
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("made %d requests; an invalid signature is the MITM signal and must never be retried", n)
	}
}

// TestDiscoverCAMissingSignatureCarriesRemediation: the joiner-side message is
// the actual control for a control plane that does not run the bootstrapsigner
// yet, so it must carry the whole remedy — the flag,
// where to verify it, and "every control plane".
func TestDiscoverCAMissingSignatureCarriesRemediation(t *testing.T) {
	caPEM, pin := makeCA(t)
	url, requests := clusterInfoServer(t, map[string]string{
		clusterinfo.KubeConfigKey: kubeconfigWith(caPEM),
	})

	_, err := DiscoverCA(url, testToken, pin)
	if err == nil {
		t.Fatal("unsigned cluster-info accepted")
	}
	// The signature is patched in a moment after the ConfigMap is written, so a
	// missing one must be waited out, not failed on the first read. Without this
	// assertion a one-attempt implementation passes the whole suite.
	if n := requests.Load(); n < 2 {
		t.Errorf("made %d request(s); a missing signature must be retried until the budget expires", n)
	}
	for _, want := range []string{
		version.ControllerManagerControllersFlag,
		version.ControllerManagerManifest,
		"EVERY control-plane",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("remediation message missing %q\ngot: %v", want, err)
		}
	}
}

func TestParseClusterInfoBody(t *testing.T) {
	tests := []struct {
		name, body string
		want       string
		wantErr    bool
	}{
		{"valid", `{"data":{"kubeconfig":"apiVersion: v1"}}`, "apiVersion: v1", false},
		{"missing key", `{"data":{"other":"x"}}`, "", true},
		{"empty value", `{"data":{"kubeconfig":""}}`, "", true},
		{"bad json", `{not json`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseClusterInfoBody([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got[clusterinfo.KubeConfigKey] != tt.want {
				t.Errorf("got %q, want %q", got[clusterinfo.KubeConfigKey], tt.want)
			}
		})
	}
}

// TestParseClusterInfoBodyKeepsSignatures: the signature travels in the same
// data map as the kubeconfig, so the transport layer must not drop unknown keys.
func TestParseClusterInfoBodyKeepsSignatures(t *testing.T) {
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"data": map[string]string{
			"kubeconfig":            "clusters: []",
			"jws-kubeconfig-abcdef": "sig",
		},
	}
	body, _ := json.Marshal(cm)
	got, err := parseClusterInfoBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if got["jws-kubeconfig-abcdef"] != "sig" {
		t.Errorf("transport dropped the signature key: %v", got)
	}
}

func containsPin(t *testing.T, bundle []byte, pin string) bool {
	t.Helper()
	rest := bundle
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return false
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			continue
		}
		if clusterinfo.PinFor(c) == pin {
			return true
		}
	}
}

// TestWriteBootstrapKubeconfigReplacesAtomically pins the write rule the
// node-file record (version.NodeFileOwners) and make lint's write guard put on
// this file: the kubelet's TLS-bootstrap kubeconfig is start-critical, so it
// is rendered to bytes and written through the shared atomic writer — never
// truncated in place — at mode 0600, and the directory is left holding the
// written file alone.
func TestWriteBootstrapKubeconfigReplacesAtomically(t *testing.T) {
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	version.SetRoots(t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(func() { version.SetRoots(data, conf, run) })

	// A longer file already at the path: a truncating writer that dies
	// mid-write would leave the residue behind; the replacement must be whole.
	if err := os.MkdirAll(filepath.Dir(version.BootstrapKubeconfig), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("stale kubeconfig padding that the replacement must not leave behind\n", 40)
	if err := os.WriteFile(version.BootstrapKubeconfig, []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}

	ca := []byte("fake-ca-bytes")
	if err := WriteBootstrapKubeconfig("https://192.0.2.10:6443", testToken, ca); err != nil {
		t.Fatalf("WriteBootstrapKubeconfig: %v", err)
	}
	b, err := os.ReadFile(version.BootstrapKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stale kubeconfig padding") {
		t.Error("the rewritten bootstrap kubeconfig carries residue from the file it replaced")
	}
	cfg, err := clientcmd.Load(b)
	if err != nil {
		t.Fatalf("the written bootstrap kubeconfig does not parse: %v", err)
	}
	cluster := cfg.Clusters[version.ClusterName]
	if cluster == nil || string(cluster.CertificateAuthorityData) != string(ca) || cluster.Server != "https://192.0.2.10:6443" {
		t.Errorf("the written cluster = %+v, want the server and CA the write was given", cluster)
	}
	user := cfg.AuthInfos["tls-bootstrap"]
	if user == nil || user.Token != testToken {
		t.Errorf("the written auth info = %+v, want the tls-bootstrap token", user)
	}
	fi, err := os.Stat(version.BootstrapKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("bootstrap.conf written as %04o, want 0600 — it carries the bootstrap token", perm)
	}
	entries, err := os.ReadDir(filepath.Dir(version.BootstrapKubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(version.BootstrapKubeconfig) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the kubeconfig directory holds %v, want the written file alone — no stray temp name", names)
	}
}
