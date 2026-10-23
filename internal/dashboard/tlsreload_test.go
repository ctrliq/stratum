package dashboard

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, cn string, mod time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for f, b := range map[string][]byte{
		cert:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyFile: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}),
	} {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	return cert, keyFile
}

func servedCN(t *testing.T, r *certReloader) string {
	t.Helper()
	c, err := r.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestCertReloaderPicksUpARenewedPair(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	cert, key := writePair(t, dir, "first", t0)
	var logs bytes.Buffer
	r, err := newCertReloader(cert, key, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.now = func() time.Time { return now }
	if got := servedCN(t, r); got != "first" {
		t.Fatalf("served %q, want first", got)
	}

	writePair(t, dir, "second", t0.Add(time.Minute))
	if got := servedCN(t, r); got != "first" {
		t.Errorf("served %q inside the recheck interval, want first", got)
	}
	now = now.Add(r.recheck)
	if got := servedCN(t, r); got != "second" {
		t.Errorf("served %q after the files changed, want second", got)
	}

	if err := os.WriteFile(cert, []byte("half written"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(r.recheck)
	if got := servedCN(t, r); got != "second" {
		t.Errorf("served %q after a bad update, want the last good pair", got)
	}
	now = now.Add(r.recheck)
	servedCN(t, r)
	if n := strings.Count(logs.String(), "could not reload the TLS certificate"); n != 1 {
		t.Errorf("a pair that keeps failing was logged %d times, want once:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "reloaded the TLS certificate"); n != 1 {
		t.Errorf("the successful reload was logged %d times, want once:\n%s", n, logs.String())
	}
}

func TestNewCertReloaderRefusesABadPair(t *testing.T) {
	dir := t.TempDir()
	if _, err := newCertReloader(filepath.Join(dir, "none.crt"), filepath.Join(dir, "none.key"), slog.Default()); err == nil {
		t.Fatal("a missing pair was accepted")
	}
}

func TestLoadSessionSecret(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	if err := os.WriteFile(good, make([]byte, minSessionKey), 0o400); err != nil {
		t.Fatal(err)
	}
	if b, err := loadSessionSecret(good); err != nil || len(b) != minSessionKey {
		t.Fatalf("loadSessionSecret(good) = %d bytes, %v", len(b), err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("tiny"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionSecret(short); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := loadSessionSecret(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing key file was accepted")
	}
	a, err := loadSessionSecret("")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := loadSessionSecret("")
	if len(a) != 32 || string(a) == string(b) {
		t.Error("the default secret is not 32 fresh random bytes")
	}
}
