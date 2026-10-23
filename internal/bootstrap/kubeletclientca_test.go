package bootstrap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
)

func selfSigned(t *testing.T, isCA bool) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubelet-client-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func serveKubeletClientCA(t *testing.T, status int, bundle []byte) (url string, roots []byte) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != kubeletClientCAPath || r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{clusterinfo.KubeletClientCAKey: string(bundle)}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestFetchKubeletClientCA(t *testing.T) {
	ca := selfSigned(t, true)
	for _, tc := range []struct {
		name      string
		status    int
		bundle    []byte
		wantFound bool
		wantErr   bool
	}{
		{name: "published", status: http.StatusOK, bundle: ca, wantFound: true},
		{name: "not published (404)", status: http.StatusNotFound},
		{name: "Role predates it (403)", status: http.StatusForbidden},
		{name: "a leaf, not a CA", status: http.StatusOK, bundle: selfSigned(t, false), wantErr: true},
		{name: "not PEM", status: http.StatusOK, bundle: []byte("garbage"), wantErr: true},
		{name: "trailing data", status: http.StatusOK, bundle: append(append([]byte{}, ca...), "junk"...), wantErr: true},
		{name: "leading data", status: http.StatusOK, bundle: append([]byte("junk\n"), ca...), wantErr: true},
		{name: "space before PEM", status: http.StatusOK, bundle: append([]byte(" "), ca...), wantErr: true},
		{name: "tab after newline before PEM", status: http.StatusOK, bundle: append([]byte("\n\t"), ca...), wantErr: true},
		{name: "whitespace ending in newline", status: http.StatusOK, bundle: append([]byte(" \t\n"), ca...), wantFound: true},
		{name: "malformed earlier BEGIN", status: http.StatusOK, bundle: append([]byte("-----BEGIN CERTIFICATE-----\nnot-base64\n-----END CERTIFICATE-----\n"), ca...), wantErr: true},
		{name: "data between blocks", status: http.StatusOK, bundle: append(append(append([]byte{}, ca...), "junk\n"...), ca...), wantErr: true},
		{name: "PEM headers", status: http.StatusOK, bundle: withPEMHeader(t, ca), wantErr: true},
		{name: "two CAs", status: http.StatusOK, bundle: append(append([]byte{}, ca...), selfSigned(t, true)...), wantFound: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, roots := serveKubeletClientCA(t, tc.status, tc.bundle)
			got, found, err := FetchKubeletClientCA(url, roots)
			if (err != nil) != tc.wantErr || found != tc.wantFound {
				t.Fatalf("FetchKubeletClientCA = found %v, err %v; want found %v, error %v", found, err, tc.wantFound, tc.wantErr)
			}
			if found && string(got) != string(tc.bundle) {
				t.Errorf("bundle = %q, want the published one", got)
			}
		})
	}
}

// The fetch is verified against the cluster CA the caller already trusts: a
// server that CA did not sign is refused, not read.
func TestFetchKubeletClientCARefusesAnUntrustedServer(t *testing.T) {
	url, _ := serveKubeletClientCA(t, http.StatusOK, selfSigned(t, true))
	if _, found, err := FetchKubeletClientCA(url, selfSigned(t, true)); err == nil || found {
		t.Fatalf("FetchKubeletClientCA = found %v, err %v; want a refusal", found, err)
	}
}

func TestFetchKubeletClientCARejectsPlainHTTPBeforeRequest(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if _, found, err := FetchKubeletClientCA(srv.URL, selfSigned(t, true)); err == nil || found {
		t.Fatalf("plain HTTP fetch = found %t, err %v; want refusal", found, err)
	}
	if called.Load() {
		t.Fatal("sent a request to a plaintext endpoint")
	}
}

func TestFetchKubeletClientCARefusesRedirectToPlainHTTP(t *testing.T) {
	var followed atomic.Bool
	ca := selfSigned(t, true)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
			clusterinfo.KubeletClientCAKey: string(ca),
		}})
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+kubeletClientCAPath, http.StatusFound)
	}))
	t.Cleanup(secure.Close)
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secure.Certificate().Raw})
	if _, found, err := FetchKubeletClientCA(secure.URL, roots); err == nil || found {
		t.Fatalf("redirected fetch = found %t, err %v; want refusal", found, err)
	}
	if followed.Load() {
		t.Fatal("followed an HTTPS redirect to a plaintext endpoint")
	}
}

func withPEMHeader(t *testing.T, b []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(b)
	blk.Headers = map[string]string{"Proc-Type": "4,ENCRYPTED"}
	return pem.EncodeToMemory(blk)
}
