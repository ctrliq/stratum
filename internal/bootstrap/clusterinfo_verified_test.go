package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type verifiedFixture struct {
	caPEM   []byte
	serving tls.Certificate
}

func newFixture(t *testing.T, ips []net.IP) verifiedFixture {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "stratum-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kube-apiserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
	}, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return verifiedFixture{
		caPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		serving: tls.Certificate{Certificate: [][]byte{srvDER, caDER}, PrivateKey: srvKey},
	}
}

// sawClientCert, if non-nil, records whether the client offered a certificate.
func serve(t *testing.T, f verifiedFixture, publish []byte, sawClientCert *bool) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sawClientCert != nil {
			*sawClientCert = len(r.TLS.PeerCertificates) > 0
		}
		kc := clientcmdapi.NewConfig()
		kc.Clusters[""] = &clientcmdapi.Cluster{Server: "https://apiserver", CertificateAuthorityData: publish}
		raw, err := clientcmd.Write(*kc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"kubeconfig": string(raw)},
		}); err != nil {
			t.Errorf("encode cluster-info: %v", err)
		}
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{f.serving},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequestClientCert,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetchClusterCAVerifiedReturnsPeerChain(t *testing.T) {
	f := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	url := serve(t, f, f.caPEM, nil)
	ca, peer, err := FetchClusterCAVerified(url, f.caPEM)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(f.caPEM) {
		t.Error("returned bundle is not what cluster-info published")
	}
	if len(peer) == 0 {
		t.Fatal("no peer chain returned")
	}
	if peer[0].Subject.CommonName != "kube-apiserver" {
		t.Errorf("peer[0] = %q, want the serving leaf", peer[0].Subject.CommonName)
	}
}

func TestFetchClusterCAVerifiedSendsNoClientCert(t *testing.T) {
	f := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	var sawClientCert bool
	url := serve(t, f, f.caPEM, &sawClientCert)
	if _, _, err := FetchClusterCAVerified(url, f.caPEM); err != nil {
		t.Fatal(err)
	}
	if sawClientCert {
		t.Error("a client certificate was offered; the request would authenticate as system:node:<host> and be denied by RBAC")
	}
}

func TestFetchClusterCAVerifiedRejectsUntrustedServer(t *testing.T) {
	genuine := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	attacker := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	url := serve(t, attacker, attacker.caPEM, nil)
	_, _, err := FetchClusterCAVerified(url, genuine.caPEM)
	if err == nil {
		t.Fatal("attacker-signed serving cert accepted")
	}
	if !strings.Contains(err.Error(), "does not trust") {
		t.Errorf("err = %v, want it to name the trust failure", err)
	}
}

func TestFetchClusterCAVerifiedDistinguishesSANMismatch(t *testing.T) {
	f := newFixture(t, nil) // signed by the trusted CA, but no IP SAN
	url := serve(t, f, f.caPEM, nil)
	_, _, err := FetchClusterCAVerified(url, f.caPEM)
	if err == nil {
		t.Fatal("serving cert with no matching SAN accepted")
	}
	// Both branches contain "SAN mismatch"; matching that alone would pass whichever branch ran.
	if !strings.Contains(err.Error(), "presented a TRUSTED certificate that is not valid for that endpoint") {
		t.Errorf("err = %v, want the trusted-cert-wrong-endpoint diagnosis", err)
	}
	if strings.Contains(err.Error(), "neither trusted") {
		t.Errorf("a trusted cert was misreported as a trust failure: %v", err)
	}
}

func TestFetchClusterCAVerifiedNeedsUsableRoots(t *testing.T) {
	f := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	url := serve(t, f, f.caPEM, nil)
	_, _, err := FetchClusterCAVerified(url, []byte("not a bundle"))
	if err == nil {
		t.Fatal("fetch proceeded with an empty trust store")
	}
	if !strings.Contains(err.Error(), "nothing to verify") {
		t.Errorf("err = %v, want it to say there is nothing to verify against", err)
	}
}

func TestFetchClusterCAVerifiedUntrustedPlusSANMismatch(t *testing.T) {
	genuine := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	attacker := newFixture(t, nil) // untrusted CA AND no IP SAN
	url := serve(t, attacker, attacker.caPEM, nil)
	_, _, err := FetchClusterCAVerified(url, genuine.caPEM)
	if err == nil {
		t.Fatal("untrusted serving cert accepted")
	}
	if !strings.Contains(err.Error(), "neither trusted by this node nor valid") {
		t.Errorf("err = %v, want it reported as a trust failure", err)
	}
	if strings.Contains(err.Error(), "TRUSTED") {
		t.Errorf("untrusted certificate announced as TRUSTED: %v", err)
	}
}

func TestCAFromClusterInfoRefusesAmbiguity(t *testing.T) {
	body := func(t *testing.T, clusters map[string][]byte) []byte {
		t.Helper()
		kc := clientcmdapi.NewConfig()
		for name, ca := range clusters {
			kc.Clusters[name] = &clientcmdapi.Cluster{Server: "https://x", CertificateAuthorityData: ca}
		}
		raw, err := clientcmd.Write(*kc)
		if err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(map[string]any{"data": map[string]string{"kubeconfig": string(raw)}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := []byte("-----BEGIN CERTIFICATE-----\nA\n-----END CERTIFICATE-----\n"), []byte("-----BEGIN CERTIFICATE-----\nB\n-----END CERTIFICATE-----\n")

	if _, err := caFromClusterInfo(body(t, map[string][]byte{"one": a, "two": b})); err == nil {
		t.Error("two differing CA bundles accepted; the result would depend on map order")
	} else if !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("err = %v, want it to say it refuses to guess", err)
	}
	// Two entries agreeing is not ambiguous.
	got, err := caFromClusterInfo(body(t, map[string][]byte{"one": a, "two": a}))
	if err != nil {
		t.Fatalf("identical duplicate entries rejected: %v", err)
	}
	if string(got) != string(a) {
		t.Error("returned the wrong bundle")
	}
	if _, err := caFromClusterInfo(body(t, map[string][]byte{"one": nil})); err == nil {
		t.Error("a cluster-info with no CA was accepted")
	}
}

func TestFetchClusterCAVerifiedCapsResponse(t *testing.T) {
	f := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	// A handler that stops on its own produces "exceeds" even with the bound removed.
	var served atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<16)
		for served.Load() < 64*maxClusterInfoBytes {
			n, err := w.Write(chunk)
			served.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{f.serving}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	_, _, err := FetchClusterCAVerified(srv.URL, f.caPEM)
	if err == nil {
		t.Fatal("an oversized response was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want it to name the size cap", err)
	}
	// Slack for buffering, but nothing near what the handler was willing to send.
	if got := served.Load(); got > 16*maxClusterInfoBytes {
		t.Errorf("served %d bytes before the refusal; the read is not bounded near the %d-byte cap", got, maxClusterInfoBytes)
	}
}

func TestFetchClusterCAVerifiedRejectsPlaintext(t *testing.T) {
	f := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kc := clientcmdapi.NewConfig()
		kc.Clusters[""] = &clientcmdapi.Cluster{Server: "https://apiserver", CertificateAuthorityData: f.caPEM}
		raw, err := clientcmd.Write(*kc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"kubeconfig": string(raw)},
		}); err != nil {
			t.Errorf("encode cluster-info: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	if _, _, err := FetchClusterCAVerified(srv.URL, f.caPEM); err == nil {
		t.Fatal("a bundle fetched over plaintext was accepted")
	} else if !strings.Contains(err.Error(), "not an HTTPS endpoint") {
		t.Errorf("err = %v, want it to name the plaintext endpoint", err)
	}
}

func TestFetchClusterCAVerifiedReportsHTTPStatus(t *testing.T) {
	f := newFixture(t, []net.IP{net.ParseIP("127.0.0.1")})
	for _, tc := range []struct {
		name string
		code int
		want string
	}{
		{"forbidden", http.StatusForbidden, "HTTP 403"},
		{"not found", http.StatusNotFound, "HTTP 404"},
		{"unavailable", http.StatusServiceUnavailable, "HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				// The shape the apiserver returns: a Status object with no data key.
				if err := json.NewEncoder(w).Encode(map[string]any{
					"kind": "Status", "status": "Failure", "code": tc.code,
				}); err != nil {
					t.Errorf("encode status: %v", err)
				}
			}))
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{f.serving}, MinVersion: tls.VersionTLS12}
			srv.StartTLS()
			t.Cleanup(srv.Close)

			_, _, err := FetchClusterCAVerified(srv.URL, f.caPEM)
			if err == nil {
				t.Fatal("a non-200 response was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name the status %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "missing data.kubeconfig") {
				t.Errorf("an HTTP failure was misreported as a malformed ConfigMap: %v", err)
			}
		})
	}
}
