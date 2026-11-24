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

// makeCA returns a self-signed CA cert (PEM) and its "sha256:<hex>" pin.
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

// A TLS server with a self-signed cert, like an on-path attacker; counts requests.
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

// Shortens the retry budgets; retry behaviour is asserted by request count, not wall clock.
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

// Built from the standard library, not the code under test, so the fixtures check it independently.
func signatureFor(kubeconfig string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"` + testID + `"}`))
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(header + "." + base64.RawURLEncoding.EncodeToString([]byte(kubeconfig))))
	return header + ".." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

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
	// The attacker's CA must never be in what the caller writes to ca-bundle.crt.
	if containsPin(t, out, attackerPin) {
		t.Errorf("attacker CA present in the returned bundle (%d bytes)", len(out))
	}
}

func TestDiscoverCARejectsSignedAttackerBundle(t *testing.T) {
	attackerPEM, _ := makeCA(t)
	_, genuinePin := makeCA(t)

	url, _ := clusterInfoServer(t, signedData(kubeconfigWith(attackerPEM)))

	if _, err := DiscoverCA(url, testToken, genuinePin); err == nil {
		t.Fatal("accepted a signed bundle carrying no CA matching the pin")
	}
}

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
	// Without this the test passes on any error.
	if !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("got %v; want the explicit scheme guard", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("made %d request(s) to a plaintext endpoint; the scheme check must precede network I/O", n)
	}
}

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

// Pins the bound, not the rejection: the oversized-body test passes with the LimitReader removed.
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

	// Slack for socket and server buffering; the point is stopped-near-limit, not an exact count.
	const ceiling = 16 * chunk
	if got > ceiling {
		t.Errorf("server pushed %d bytes; the client did not stop near the %d-byte limit "+
			"(an unbounded read would drain all %d)", got, maxClusterInfoBytes, total)
	}
}

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
	for _, want := range []string{
		"exceeds the " + strconv.Itoa(maxClusterInfoBytes) + "-byte limit",
		"bootstrap-token signatures", "never expire",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("size-limit error missing %q\ngot: %v", want, err)
		}
	}
}

func TestDiscoverCARejectsTamperedSignedBundleImmediately(t *testing.T) {
	caPEM, pin := makeCA(t)
	attackerPEM, _ := makeCA(t)

	kc := kubeconfigWith(caPEM)
	data := signedData(kc)
	data[clusterinfo.KubeConfigKey] = kubeconfigWith(append(append([]byte{}, attackerPEM...), caPEM...))
	url, requests := clusterInfoServer(t, data)

	if _, err := DiscoverCA(url, testToken, pin); err == nil {
		t.Fatal("accepted a bundle whose signature does not cover it")
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("made %d requests; an invalid signature is the MITM signal and must never be retried", n)
	}
}

func TestDiscoverCAMissingSignatureCarriesRemediation(t *testing.T) {
	caPEM, pin := makeCA(t)
	url, requests := clusterInfoServer(t, map[string]string{
		clusterinfo.KubeConfigKey: kubeconfigWith(caPEM),
	})

	_, err := DiscoverCA(url, testToken, pin)
	if err == nil {
		t.Fatal("unsigned cluster-info accepted")
	}
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

func TestWriteBootstrapKubeconfigReplacesAtomically(t *testing.T) {
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	version.SetRoots(t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(func() { version.SetRoots(data, conf, run) })

	// A truncating writer that dies mid-write would leave residue behind.
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
