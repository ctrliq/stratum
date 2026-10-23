package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	stratumpki "github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func healTestCert(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "system:node:w1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: isCA, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestKubeletStartHealsAnUntrustedServingCert runs the kubelet's own WaitFor,
// with the runtime wait stubbed, on the argv kubeletProcess launches a worker
// with: a serving pair no CA in ca-bundle.crt signed is moved out of the
// kubelet's cert dir before the start, and one the bundle's CA signed stays.
func TestKubeletStartHealsAnUntrustedServingCert(t *testing.T) {
	savedPKI, savedReady := version.PKIDir, kubeletRuntimeReady
	t.Cleanup(func() { version.PKIDir, kubeletRuntimeReady = savedPKI, savedReady })
	kubeletRuntimeReady = func(context.Context) error { return nil }
	version.PKIDir = t.TempDir()
	oldCA, oldKey, _ := healTestCert(t, nil, nil, true)
	newCA, newKey, newPEM := healTestCert(t, nil, nil, true)
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), newPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	pki := func(name string) string { return filepath.Join(version.PKIDir, name) }
	kc := func(name string) string { return filepath.Join(t.TempDir(), name) }
	proc := kubeletProcess(&NodeState{Role: "worker"}, pki, kc)
	certDir := pki("kubelet")
	current := filepath.Join(certDir, stratumpki.KubeletServingCurrent)
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		ca        *x509.Certificate
		key       *ecdsa.PrivateKey
		wantMoved bool
	}{
		{"signed by a CA the bundle dropped", oldCA, oldKey, true},
		{"signed by the bundle's CA", newCA, newKey, false},
	} {
		_, _, leaf := healTestCert(t, tc.ca, tc.key, false)
		_ = os.Remove(current)
		if err := os.WriteFile(current, leaf, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := proc.WaitFor(t.Context()); err != nil {
			t.Fatalf("%s: WaitFor: %v", tc.name, err)
		}
		_, err := os.Stat(current)
		if moved := errors.Is(err, os.ErrNotExist); moved != tc.wantMoved {
			t.Errorf("%s: moved = %v, want %v", tc.name, moved, tc.wantMoved)
		}
	}
}
