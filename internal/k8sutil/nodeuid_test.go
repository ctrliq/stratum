package k8sutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

func TestOwnNodeUIDLoadsRelativeCredentials(t *testing.T) {
	for _, inline := range []string{"", "stale-token"} {
		t.Run("inline="+inline, func(t *testing.T) { testNodeUIDCredentials(t, inline) })
	}
}

func testNodeUIDCredentials(t *testing.T, inline string) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || len(r.TLS.PeerCertificates) != 1 {
			t.Error("node read did not use the kubelet's certificate and token files")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"Node","metadata":{"uid":"immutable-uid"}}`)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()
	old := version.KubeconfigDir
	version.KubeconfigDir = t.TempDir()
	t.Cleanup(func() { version.KubeconfigDir = old })
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	key, err := x509.MarshalPKCS8PrivateKey(srv.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\n    certificate-authority: ca.crt\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user:\n    client-certificate: client.crt\n    client-key: client.key\n    tokenFile: token\n", srv.URL)
	if inline != "" {
		config += "    token: " + inline + "\n"
	}
	for name, data := range map[string][]byte{
		"kubelet.kubeconfig": []byte(config), "ca.crt": cert, "client.crt": cert,
		"client.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), "token": []byte("test-token\n"),
	} {
		if err := os.WriteFile(filepath.Join(version.KubeconfigDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	uid, err := OwnNodeUID(context.Background(), "cp-1")
	if err != nil || uid != "immutable-uid" {
		t.Fatalf("uid %q, error %v", uid, err)
	}
}

func TestOwnNodeUIDRejectsFIFOConfigAndCredentials(t *testing.T) {
	for _, input := range []string{"kubeconfig", "certificate"} {
		t.Run(input, func(t *testing.T) {
			old := version.KubeconfigDir
			version.KubeconfigDir = t.TempDir()
			t.Cleanup(func() { version.KubeconfigDir = old })
			path := filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig")
			if input == "certificate" {
				config := "apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: https://127.0.0.1:1\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user:\n    client-certificate: certificate\n    client-key: certificate\n"
				if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(version.KubeconfigDir, "certificate")
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			fifo, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer fifo.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := OwnNodeUID(ctx, "cp-1"); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO identity input was accepted")
				}
			case <-time.After(500 * time.Millisecond):
				t.Error("FIFO identity input blocked past the deadline")
				// The old implementation may open the shared cert/key twice.
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, _ = fifo.Write([]byte("invalid\n"))
				_ = fifo.Close()
				<-done
			}
		})
	}
}

func TestOwnNodeUIDUsesNodeAPIAndRequestContext(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(fmt.Sprint(hang), func(t *testing.T) {
			cancelled := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/nodes/cp-1" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				if hang {
					<-r.Context().Done()
					close(cancelled)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Node","metadata":{"name":"cp-1","uid":"immutable-uid"}}`)
			}))
			defer srv.Close()
			old := version.KubeconfigDir
			version.KubeconfigDir = t.TempDir()
			t.Cleanup(func() { version.KubeconfigDir = old })
			config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", srv.URL)
			if err := os.WriteFile(filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			uid, err := OwnNodeUID(ctx, "cp-1")
			if !hang {
				if err != nil || uid != "immutable-uid" {
					t.Fatalf("uid %q, error %v", uid, err)
				}
				return
			}
			if err == nil || uid != "" {
				t.Fatalf("hung API returned uid %q, error %v", uid, err)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("node API did not receive cancellation")
			}
		})
	}
}
