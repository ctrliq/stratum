package kube

import (
	"crypto/tls"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
)

func TestClusterCertificateRotationChangesTransportIdentity(t *testing.T) {
	isolateHome(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	certA, keyA := writeCertPair(t, dir, "client-a")
	certB, keyB := writeCertPair(t, dir, "client-b")
	config := writeReferencingKubeconfig(t, dir, "config", server.URL, ca, certA, keyA, "")
	conn := NewConnector(Options{Kubeconfig: config})
	first, err := conn.Cluster("")
	if err != nil {
		t.Fatal(err)
	}
	identity := func(cl *Cluster) string {
		t.Helper()
		client, err := rest.HTTPClientFor(cl.Config)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if got := identity(first); got != "client-a" {
		t.Fatalf("initial identity = %q", got)
	}
	rewriteSameMtime(t, certA, mustRead(t, certB))
	rewriteSameMtime(t, keyA, mustRead(t, keyB))
	rotated, err := conn.Cluster("")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == first {
		t.Fatal("certificate rotation reused the cached cluster")
	}
	if got := identity(rotated); got != "client-b" {
		t.Fatalf("rotated identity = %q, want client-b", got)
	}
	if got := identity(first); got != "client-a" {
		t.Fatalf("in-flight client's identity changed to %q", got)
	}
	if stable, err := conn.Cluster(""); err != nil || stable != rotated {
		t.Fatalf("stable inputs did not reuse rotated cluster: %p, %v", stable, err)
	}
}
