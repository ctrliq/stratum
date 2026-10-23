package ha

import (
	"io"

	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// servingVIP stands in for a VIP that serves the cluster's apiserver.
func servingVIP(t *testing.T) {
	t.Helper()
	old := VIPServes
	VIPServes = func(string) error { return nil }
	t.Cleanup(func() { VIPServes = old })
}

// deadVIP stands in for a VIP that nothing serves.
func deadVIP(t *testing.T) error {
	t.Helper()
	err := errors.New("dial tcp 10.10.1.250:6443: connect: no route to host")
	old := VIPServes
	VIPServes = func(string) error { return err }
	t.Cleanup(func() { VIPServes = old })
	return err
}

// A node repointed onto a VIP nothing serves loses every API client, so
// ha-repoint refuses before it rewrites a kubeconfig, and says what to do
// first.
func TestNodeRepointRefusesAVIPThatDoesNotServe(t *testing.T) {
	cause := deadVIP(t)
	called := false
	_, err := nodeRepoint(io.Discard, "10.10.1.250", func(string) error { called = true; return nil })
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "spec.vip.address") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("nodeRepoint = %v, want a refusal naming spec.vip.address and wrapping the dial error", err)
	}
	if called {
		t.Error("kubeconfigs were rewritten onto a VIP that does not serve")
	}
}

// Cilium agents pointed at a dead VIP lose the apiserver on every node, so
// the cluster-wide repoint refuses the same way, before changing anything.
func TestRepointCiliumRefusesAVIPThatDoesNotServe(t *testing.T) {
	deadVIP(t)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: "kube-system"}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cilium-operator", Namespace: "kube-system"}}
	cs := fake.NewSimpleClientset(ds, dep)
	if _, err := RepointCiliumWith(t.Context(), io.Discard, cs, "10.10.1.250"); err == nil {
		t.Fatalf("RepointCiliumWith = %v, want a refusal", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" {
			t.Errorf("Cilium was changed: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// The check is a TLS handshake that trusts only the cluster CA and requires
// the certificate to cover the VIP: an address that answers with another CA's
// certificate, or one that does not name the VIP, does not serve the cluster.
func TestTLSServes(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	cluster := x509.NewCertPool()
	cluster.AddCert(srv.Certificate())
	if err := tlsServes(addr, "127.0.0.1", cluster); err != nil {
		t.Errorf("the cluster's apiserver on the VIP: %v, want it to serve", err)
	}
	if err := tlsServes(addr, "10.9.9.9", cluster); err == nil {
		t.Error("a certificate that does not cover the VIP was accepted")
	}
	if err := tlsServes(addr, "127.0.0.1", x509.NewCertPool()); err == nil {
		t.Error("a certificate from another CA was accepted")
	}
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	gone.Close()
	if err := tlsServes(gone.Listener.Addr().String(), "127.0.0.1", cluster); err == nil {
		t.Error("an address nothing listens on was accepted")
	}
}

// The cluster CA comes from whichever of the node's kubeconfigs exists: a
// worker has only the kubelet's.
func TestClusterCAFromTheNodesKubeconfigs(t *testing.T) {
	dir := t.TempDir()
	oldDir, oldAdmin, oldBoot := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig
	version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = dir, filepath.Join(dir, "absent.conf"), filepath.Join(dir, "absent-boot.conf")
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = oldDir, oldAdmin, oldBoot
	})
	if _, err := clusterCA(); err == nil {
		t.Fatal("found a cluster CA with no kubeconfigs")
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	kc := "apiVersion: v1\nkind: Config\nclusters:\n- name: stratum\n  cluster:\n    server: https://127.0.0.1:6443\n    certificate-authority-data: " +
		b64(ca) + "\ncontexts:\n- name: k\n  context:\n    cluster: stratum\ncurrent-context: k\n"
	if err := os.WriteFile(filepath.Join(dir, "kubelet.kubeconfig"), []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := clusterCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := tlsServes(srv.Listener.Addr().String(), "127.0.0.1", pool); err != nil {
		t.Errorf("the CA read from kubelet.kubeconfig does not verify the server it came from: %v", err)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// When no kubeconfig yields a CA, the error says why for each one that is
// present: one that does not parse, and one whose certificate-authority file
// cannot be read, are reported by name rather than counted as read.
func TestClusterCAReportsWhyNoCAWasFound(t *testing.T) {
	dir := t.TempDir()
	oldDir, oldAdmin, oldBoot := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig
	version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = dir, filepath.Join(dir, "absent.conf"), filepath.Join(dir, "absent-boot.conf")
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = oldDir, oldAdmin, oldBoot
	})

	broken := filepath.Join(dir, "kubelet.kubeconfig")
	if err := os.WriteFile(broken, []byte("clusters: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingCA := filepath.Join(dir, "no-such-ca.crt")
	kc := "apiVersion: v1\nkind: Config\nclusters:\n- name: stratum\n  cluster:\n    server: https://127.0.0.1:6443\n    certificate-authority: " + missingCA + "\n"
	if err := os.WriteFile(filepath.Join(dir, "admin.kubeconfig"), []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := clusterCA()
	if !errors.Is(err, errNoClusterCA) {
		t.Fatalf("clusterCA = %v, want errNoClusterCA", err)
	}
	for _, want := range []string{broken, missingCA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "is this a Stratum node") {
		t.Errorf("error asks whether this is a Stratum node though it found its kubeconfigs: %v", err)
	}
}

// A check that never reached the VIP, because this node's CA could not be
// read, is refused as such: not as a VIP that does not answer, and without
// the VIP set-up steps that would not fix it.
func TestRepointRefusesALocalCAProblemAsSuch(t *testing.T) {
	cause := fmt.Errorf("%w: kubelet.kubeconfig: permission denied", errNoClusterCA)
	old := VIPServes
	VIPServes = func(string) error { return cause }
	t.Cleanup(func() { VIPServes = old })

	called := false
	_, err := nodeRepoint(io.Discard, "10.10.1.250", func(string) error { called = true; return nil })
	if !errors.Is(err, errNoClusterCA) || !strings.Contains(err.Error(), "cannot check") {
		t.Fatalf("nodeRepoint = %v, want a refusal saying the VIP could not be checked", err)
	}
	if strings.Contains(err.Error(), "spec.vip.address") || strings.Contains(err.Error(), "does not answer") {
		t.Errorf("a local CA problem was reported as the VIP's: %v", err)
	}
	if called {
		t.Error("kubeconfigs were rewritten after the check failed")
	}
	cs := fake.NewSimpleClientset()
	if _, err := RepointCiliumWith(t.Context(), io.Discard, cs, "10.10.1.250"); !errors.Is(err, errNoClusterCA) || strings.Contains(err.Error(), "spec.vip.address") {
		t.Errorf("RepointCiliumWith = %v, want the same local-CA refusal", err)
	}
}
