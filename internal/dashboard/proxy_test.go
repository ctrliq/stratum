package dashboard

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// Tested directly: net/http canonicalises header keys on the wire, hiding a non-canonical key.
func TestRewriteRequestScrubsNonCanonicalHeaderKeys(t *testing.T) {
	out := httptest.NewRequest(http.MethodGet, "/k8s/api/v1/pods", nil)
	out = out.WithContext(context.WithValue(context.Background(), ctxToken, "the-session-token"))
	out.Header["impersonate-uid"] = []string{"1234"}
	out.Header["IMPERSONATE-USER"] = []string{"system:admin"}
	out.Header["Impersonate-Extra-Scopes"] = []string{"everything"}
	out.Header.Set("Authorization", "Bearer someone-elses-token")

	rewriteRequest(out, k8sPrefix, target{Scheme: "https", Host: "apiserver:6443"})

	for name := range out.Header {
		if strings.HasPrefix(strings.ToLower(name), "impersonate-") {
			t.Errorf("%q survived the scrub; Header.Del canonicalises before deleting, so it cannot remove this", name)
		}
	}
	if got := out.Header.Get("Authorization"); got != "Bearer the-session-token" {
		t.Errorf("Authorization = %q, want the session's token", got)
	}
	if out.URL.Path != "/api/v1/pods" || out.URL.Host != "apiserver:6443" || out.URL.Scheme != "https" {
		t.Errorf("retarget produced %s://%s%s", out.URL.Scheme, out.URL.Host, out.URL.Path)
	}
	if out.Host != "apiserver:6443" {
		t.Errorf("out.Host = %q, want the target's", out.Host)
	}
}

// With no session in the context there must be no Authorization at all.
func TestRewriteRequestAddsNoAuthorizationWithoutASession(t *testing.T) {
	out := httptest.NewRequest(http.MethodGet, "/k8s/api/v1/pods", nil)
	out.Header.Set("Authorization", "Bearer someone-elses-token")
	rewriteRequest(out, k8sPrefix, target{Scheme: "https", Host: "apiserver:6443"})
	if got := out.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

func TestIsStreaming(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		method    string
		upgrade   bool
		want      bool
	}{
		{name: "a plain list", url: "/api/v1/pods"},
		{name: "a watch", url: "/api/v1/pods?watch=true", want: true},
		{name: "a watch, numeric", url: "/api/v1/pods?watch=1", want: true},
		{name: "a log follow", url: "/api/v1/namespaces/d/pods/p/log?follow=true", want: true},
		{name: "a log follow, numeric", url: "/api/v1/namespaces/d/pods/p/log?follow=1", want: true},
		{name: "an exec upgrade", url: "/api/v1/namespaces/d/pods/p/exec", upgrade: true, want: true},
		{name: "a create wearing an Upgrade header", url: "/api/v1/namespaces/d/pods",
			method: http.MethodPost, upgrade: true},
		{name: "a create claiming to follow", url: "/api/v1/namespaces/d/pods?follow=true",
			method: http.MethodPost},
		{name: "a create claiming to follow, numeric", url: "/api/v1/namespaces/d/pods?follow=1",
			method: http.MethodPost},
		{name: "a create claiming to watch", url: "/api/v1/namespaces/d/pods?watch=true",
			method: http.MethodPost},
		{name: "a delete wearing an Upgrade header", url: "/api/v1/namespaces/d/pods/p",
			method: http.MethodDelete, upgrade: true},
		{name: "a patch claiming to watch", url: "/api/v1/namespaces/d/pods/p?watch=true",
			method: http.MethodPatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.method == "" {
				tc.method = http.MethodGet
			}
			r := httptest.NewRequest(tc.method, tc.url, nil)
			if tc.upgrade {
				r.Header.Set("Upgrade", "websocket")
			}
			if got := isStreaming(r); got != tc.want {
				why := "a deadline on a real stream cuts it off mid-session"
				if !tc.want {
					why = "the caller must not be able to lift its own deadline"
				}
				t.Fatalf("isStreaming = %t, want %t (%s)", got, tc.want, why)
			}
		})
	}
}

// A sessionless request must be anonymous to the apiserver, not the process's account.
func TestProxyTransportCarriesNoProcessCredentials(t *testing.T) {
	api := newFakeAPIServer(t)
	const processToken = "the-process-own-serviceaccount-token"
	tgt, err := localTarget(&rest.Config{Host: api.URL, BearerToken: processToken})
	if err != nil {
		t.Fatalf("localTarget: %v", err)
	}
	p := newProxy(tgt, false, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Straight at the proxy, with no session in the context.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, k8sPrefix+"/api/v1/pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxied GET: got %d", rec.Code)
	}
	if got := api.last(t).Header.Get("Authorization"); got != "" {
		t.Fatalf("the apiserver saw Authorization %q; the proxy transport is carrying the process's own credentials", got)
	}
}

// AnonymousClientConfig drops client certs; rest.CopyConfig would present the process certificate.
func TestProxyTransportPresentsNoClientCertificate(t *testing.T) {
	clientCert, clientPEM, clientKeyPEM := generateCert(t, "the-dashboard-process")

	var mu sync.Mutex
	var presented []string
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for _, c := range r.TLS.PeerCertificates {
			presented = append(presented, c.Subject.CommonName)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Pod","apiVersion":"v1"}`))
	}))
	// Observes whether a cert is offered; rejecting the handshake cannot tell the arms apart.
	api.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	api.StartTLS()
	defer api.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	cfg := func() *rest.Config {
		return &rest.Config{Host: api.URL, TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM, ServerName: "example.com",
			CertData: clientPEM, KeyData: clientKeyPEM,
		}}
	}

	// Control arm: proves the fixture really authenticates by certificate.
	rt, err := rest.TransportFor(cfg())
	if err != nil {
		t.Fatalf("control transport: %v", err)
	}
	if _, err := (&http.Client{Transport: rt}).Get(api.URL + "/api/v1/pods"); err != nil {
		t.Fatalf("control request: %v", err)
	}
	mu.Lock()
	control := append([]string(nil), presented...)
	presented = nil
	mu.Unlock()
	if len(control) != 1 || control[0] != clientCert.Subject.CommonName {
		t.Fatalf("control arm presented %v, want the process certificate; the fixture is not exercising mTLS", control)
	}

	tgt, err := localTarget(cfg())
	if err != nil {
		t.Fatalf("localTarget: %v", err)
	}
	p := newProxy(tgt, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, k8sPrefix+"/api/v1/pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxied GET: got %d, body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), presented...)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("the apiserver was handed client certificate(s) %v; a sessionless request authenticates as the dashboard process, not anonymously", got)
	}
}

func generateCert(t *testing.T, cn string) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return cert,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
