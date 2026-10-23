package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type servingCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newServingCA(t *testing.T) servingCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "stratum-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return servingCA{cert, key}
}

func (ca servingCA) servingPair(t *testing.T, ip string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "system:node:w1", Organization: []string{"system:nodes"}},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pemBytes
}

func TestKubeletCertDir(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "/var/lib/kubelet/pki"},
		{[]string{"--root-dir=/data/kubelet"}, "/var/lib/kubelet/pki"},
		{[]string{"--root-dir", "/data/kubelet"}, "/var/lib/kubelet/pki"},
		{[]string{"--root-dir=/data/kubelet", "--cert-dir=/pki/kubelet"}, "/pki/kubelet"},
		{[]string{"--cert-dir", "/pki/kubelet"}, "/pki/kubelet"},
		{[]string{"--cert-dir=/a", "--cert-dir=/b"}, "/b"},
	} {
		if got := KubeletCertDir(tc.args); got != tc.want {
			t.Errorf("KubeletCertDir(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestHealKubeletServingCert(t *testing.T) {
	oldCA, newCA := newServingCA(t), newServingCA(t)
	_, oldPair := oldCA.servingPair(t, "192.0.2.20")
	_, newPair := newCA.servingPair(t, "192.0.2.20")
	for _, tc := range []struct {
		name      string
		current   []byte
		roots     []*x509.Certificate
		wantMoved bool
	}{
		{name: "signed by a dropped CA", current: oldPair, roots: []*x509.Certificate{newCA.cert}, wantMoved: true},
		{name: "signed by the cluster CA", current: newPair, roots: []*x509.Certificate{newCA.cert}},
		{name: "mid rotation, either CA trusted", current: oldPair, roots: []*x509.Certificate{oldCA.cert, newCA.cert}},
		{name: "no serving cert", roots: []*x509.Certificate{newCA.cert}},
		{name: "unparseable serving cert", current: []byte("garbage"), roots: []*x509.Certificate{newCA.cert}},
		{name: "no roots to judge by", current: oldPair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			current := filepath.Join(dir, KubeletServingCurrent)
			if tc.current != nil {
				target := filepath.Join(dir, "kubelet-server-2026-01-01-00-00-00.pem")
				if err := os.WriteFile(target, tc.current, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, current); err != nil {
					t.Fatal(err)
				}
			}
			aside, err := HealKubeletServingCert(dir, tc.roots)
			if err != nil {
				t.Fatalf("heal: %v", err)
			}
			_, statErr := os.Lstat(current)
			if !tc.wantMoved {
				if aside != "" {
					t.Fatalf("moved the serving cert to %s, want it left alone", aside)
				}
				if tc.current != nil && statErr != nil {
					t.Fatalf("serving cert gone: %v", statErr)
				}
				return
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("%s still present after heal: %v", current, statErr)
			}
			if !strings.HasPrefix(aside, current+".untrusted-") {
				t.Fatalf("aside = %q, want it next to %s", aside, current)
			}
			if b, err := os.ReadFile(aside); err != nil || string(b) != string(tc.current) {
				t.Fatalf("the moved cert does not hold the old pair: %v", err)
			}
		})
	}
}

func kubeletAt(t *testing.T, serving tls.Certificate) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serving}, ClientAuth: tls.VerifyClientCertIfGiven, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestProbeKubeletServing(t *testing.T) {
	oldCA, newCA := newServingCA(t), newServingCA(t)
	newServing, _ := newCA.servingPair(t, "127.0.0.1")
	oldServing, _ := oldCA.servingPair(t, "127.0.0.1")
	wrongIP, _ := newCA.servingPair(t, "192.0.2.99")

	if err := ProbeKubeletServing(t.Context(), kubeletAt(t, newServing), []*x509.Certificate{newCA.cert}); err != nil {
		t.Errorf("new-CA kubelet: %v", err)
	}
	err := ProbeKubeletServing(t.Context(), kubeletAt(t, oldServing), []*x509.Certificate{newCA.cert})
	if !errors.Is(err, ErrKubeletServingUntrusted) {
		t.Errorf("old-CA kubelet: err = %v, want ErrKubeletServingUntrusted", err)
	}
	err = ProbeKubeletServing(t.Context(), kubeletAt(t, wrongIP), []*x509.Certificate{newCA.cert})
	if err == nil || errors.Is(err, ErrKubeletServingUntrusted) {
		t.Errorf("SAN without the node IP: err = %v, want a verification error that is not ErrKubeletServingUntrusted, which only an unknown signer earns", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	err = ProbeKubeletServing(t.Context(), addr, []*x509.Certificate{newCA.cert})
	if err == nil || errors.Is(err, ErrKubeletServingUntrusted) {
		t.Errorf("unreachable kubelet: err = %v, want a dial error that is not ErrKubeletServingUntrusted", err)
	}
}

func TestProbeKubelets(t *testing.T) {
	targets := []KubeletTarget{{Name: "cp1", IP: "192.0.2.10"}, {Name: "w1", IP: "2001:db8::20"}, {Name: "w2"}}
	var mu sync.Mutex
	var dialled []string
	results := ProbeKubelets(t.Context(), targets, nil, func(_ context.Context, addr string, _ []*x509.Certificate) error {
		mu.Lock()
		dialled = append(dialled, addr)
		mu.Unlock()
		if addr == "[2001:db8::20]:10250" {
			return ErrKubeletServingUntrusted
		}
		return nil
	})
	if len(results) != 3 || results[0] != nil || !errors.Is(results[1], ErrKubeletServingUntrusted) || !errors.Is(results[2], ErrNoKubeletAddress) {
		t.Fatalf("results = %v, want one per target in order: nil, untrusted, no address", results)
	}
	slices.Sort(dialled)
	if want := []string{"192.0.2.10:10250", "[2001:db8::20]:10250"}; !slices.Equal(dialled, want) {
		t.Fatalf("dialled %v, want %v", dialled, want)
	}
}
