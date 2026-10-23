package certs

import (
	"context"
	"crypto"
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
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/pki"
)

func nodeClientCert(t *testing.T, ca *testCA, pub crypto.PublicKey, subject pkix.Name) []byte {
	t.Helper()
	return nodeClientCertValid(t, ca, pub, subject, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
}

func nodeClientCertValid(t *testing.T, ca *testCA, pub crypto.PublicKey, subject pkix.Name, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      subject,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func parseCSRPEM(t *testing.T, b []byte) *x509.CertificateRequest {
	t.Helper()
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("not a PEM CSR: %q", b)
	}
	req, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

var nodeW1 = pkix.Name{CommonName: "system:node:w1", Organization: []string{"system:nodes"}}

func TestRenewKubeletClientCert(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	oldCA := caStarting(t, "ca", start.Add(-48*time.Hour))
	newCA := caStarting(t, "ca", start.Add(-time.Hour))
	strangerCA := caStarting(t, "stranger", start.Add(-2*time.Hour))
	twinCA := caStarting(t, "ca", start.Add(-time.Hour))
	type validity struct{ notBefore, notAfter time.Time }
	signWith := func(ca *testCA, subject *pkix.Name, otherKey bool, v *validity) func(*testing.T, []byte) []byte {
		return func(t *testing.T, csrPEM []byte) []byte {
			req := parseCSRPEM(t, csrPEM)
			s, pub := req.Subject, req.PublicKey
			if subject != nil {
				s = *subject
			}
			if otherKey {
				k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				pub = &k.PublicKey
			}
			if v != nil {
				return nodeClientCertValid(t, ca, pub, s, v.notBefore, v.notAfter)
			}
			return nodeClientCert(t, ca, pub, s)
		}
	}
	other := pkix.Name{CommonName: "system:node:w2", Organization: []string{"system:nodes"}}
	notNode := pkix.Name{CommonName: "admin", Organization: []string{"system:masters"}}
	masters := pkix.Name{CommonName: "system:node:w1", Organization: []string{"system:masters"}}
	extraGroup := pkix.Name{CommonName: "system:node:w1", Organization: []string{"system:nodes", "system:masters"}}
	noName := pkix.Name{CommonName: "system:node:", Organization: []string{"system:nodes"}}
	notNodeUser := pkix.Name{CommonName: "admin", Organization: []string{"system:nodes"}}
	expired := &validity{time.Now().Add(-2 * time.Hour), time.Now().Add(-time.Hour)}
	future := &validity{time.Now().Add(time.Hour), time.Now().Add(2 * time.Hour)}
	for _, tc := range []struct {
		name        string
		current     *pkix.Name
		currentCA   *testCA
		noCurrent   bool
		currentPEM  []byte
		anchor      []*testCA
		sign        func(*testing.T, []byte) []byte
		requestErr  error
		storeErr    error
		wantErr     string
		wantRenewed bool
	}{
		{name: "renewed under the new CA", sign: signWith(newCA, nil, false, nil), wantRenewed: true},
		{name: "new CA listed first", anchor: []*testCA{newCA, oldCA}, sign: signWith(newCA, nil, false, nil), wantRenewed: true},
		{name: "already signed by the new CA", currentCA: newCA},
		{name: "same CA listed twice", anchor: []*testCA{oldCA, newCA, newCA}, sign: signWith(newCA, nil, false, nil), wantRenewed: true},
		{name: "current cert missing", noCurrent: true, wantErr: "has no usable client cert"},
		{name: "current cert holds no certificate", currentPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{0}}), wantErr: "has no usable client cert"},
		{name: "current cert not a node", current: &notNode, wantErr: "not a node identity"},
		{name: "current cert names no node", current: &noName, wantErr: "not a node identity"},
		{name: "current cert not a node user", current: &notNodeUser, wantErr: "not a node identity"},
		{name: "two CAs start together", anchor: []*testCA{oldCA, newCA, twinCA}, wantErr: "cannot tell which CA"},
		{name: "request fails", requestErr: errors.New("Unauthorized"), wantErr: "Unauthorized"},
		{name: "signed by the replaced CA", sign: signWith(oldCA, nil, false, nil), wantErr: "the CA this rotation replaces"},
		{name: "signed by an untrusted CA", sign: signWith(strangerCA, nil, false, nil), wantErr: "does not trust"},
		{name: "issued for another node", sign: signWith(newCA, &other, false, nil), wantErr: `not "system:node:w1"`},
		{name: "issued with another group", sign: signWith(newCA, &masters, false, nil), wantErr: "not a node identity"},
		{name: "issued with an extra group", sign: signWith(newCA, &extraGroup, false, nil), wantErr: "not a node identity"},
		{name: "issued for another key", sign: signWith(newCA, nil, true, nil), wantErr: "does not carry the key"},
		{name: "issued already expired", sign: signWith(newCA, nil, false, expired), wantErr: "not valid now"},
		{name: "issued not yet valid", sign: signWith(newCA, nil, false, future), wantErr: "not valid now"},
		{name: "store fails", sign: signWith(newCA, nil, false, nil), storeErr: errors.New("disk full"), wantErr: "disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certPath := filepath.Join(t.TempDir(), "kubelet-client-current.pem")
			switch {
			case tc.currentPEM != nil:
				if err := os.WriteFile(certPath, tc.currentPEM, 0o600); err != nil {
					t.Fatal(err)
				}
			case !tc.noCurrent:
				subject := nodeW1
				if tc.current != nil {
					subject = *tc.current
				}
				k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signer := oldCA
				if tc.currentCA != nil {
					signer = tc.currentCA
				}
				if err := os.WriteFile(certPath, nodeClientCert(t, signer, &k.PublicKey, subject), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			anchor := []*x509.Certificate{oldCA.cert, newCA.cert}
			if tc.anchor != nil {
				anchor = nil
				for _, ca := range tc.anchor {
					anchor = append(anchor, ca.cert)
				}
			}
			var requested, stored bool
			var storedCert, storedKey []byte
			renewed, err := renewKubeletClientCert(t.Context(), certPath, anchor, kubeletRenewOps{
				request: func(_ context.Context, csrPEM []byte, key crypto.Signer) ([]byte, error) {
					requested = true
					req := parseCSRPEM(t, csrPEM)
					if req.Subject.CommonName != "system:node:w1" || len(req.Subject.Organization) != 1 || req.Subject.Organization[0] != "system:nodes" {
						t.Errorf("CSR subject = %v, want the current node identity", req.Subject)
					}
					if len(req.DNSNames)+len(req.IPAddresses)+len(req.EmailAddresses)+len(req.URIs) != 0 {
						t.Errorf("CSR carries SANs, which selfnodeclient approval refuses")
					}
					if !key.Public().(*ecdsa.PublicKey).Equal(req.PublicKey) {
						t.Errorf("CSR key differs from the signer handed to request")
					}
					if tc.requestErr != nil {
						return nil, tc.requestErr
					}
					return tc.sign(t, csrPEM), nil
				},
				store: func(c, k []byte) error {
					stored, storedCert, storedKey = true, c, k
					return tc.storeErr
				},
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("renew: %v", err)
				}
				if renewed != tc.wantRenewed {
					t.Fatalf("renewed = %v, want %v", renewed, tc.wantRenewed)
				}
				if !tc.wantRenewed {
					if requested || stored {
						t.Fatalf("requested=%v stored=%v for a cert the new CA already signed; want neither", requested, stored)
					}
					return
				}
				leaf, err := pki.FirstCertificate(storedCert)
				if err != nil || leaf.CheckSignatureFrom(newCA.cert) != nil {
					t.Fatalf("stored cert not signed by the new CA: %v", err)
				}
				blk, _ := pem.Decode(storedKey)
				if blk == nil {
					t.Fatal("stored key is not PEM")
				}
				k, err := x509.ParseECPrivateKey(blk.Bytes)
				if err != nil || !k.PublicKey.Equal(leaf.PublicKey) {
					t.Fatalf("stored key does not match the stored cert: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			if tc.storeErr == nil && stored {
				t.Fatal("stored a certificate the checks refused")
			}
			if renewed {
				t.Fatal("reported renewed alongside an error")
			}
			if errors.Is(err, errNoKubeletClientCert) != (tc.noCurrent || tc.currentPEM != nil) {
				t.Fatalf("errors.Is(err, errNoKubeletClientCert) = %v for %v", !(tc.noCurrent || tc.currentPEM != nil), err)
			}
			if (tc.noCurrent || tc.currentPEM != nil || tc.current != nil || tc.anchor != nil) && requested {
				t.Fatal("sent a CSR before the current identity and the new CA were checked")
			}
		})
	}
}

func TestStoreKubeletClientCertSwapsCurrentAndKeepsThePrevious(t *testing.T) {
	ca := newTestCA(t, "ca")
	dir := t.TempDir()
	certPath := filepath.Join(dir, "kubelet-client-current.pem")
	previous := filepath.Join(dir, "kubelet-client-2026-01-01-00-00-00.pem")
	if err := os.WriteFile(previous, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previous, certPath); err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := nodeClientCert(t, ca, &k.PublicKey, nodeW1)
	if err := storeKubeletClientCert(certPath)(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		t.Fatalf("store: %v", err)
	}
	fi, err := os.Lstat(certPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink after the swap: %v", certPath, err)
	}
	target, err := os.Readlink(certPath)
	if err != nil || target == previous {
		t.Fatalf("current still points at the previous pair: %q, %v", target, err)
	}
	if leaf, err := readKubeletClientLeaf(certPath); err != nil || leaf.CheckSignatureFrom(ca.cert) != nil {
		t.Fatalf("current does not hold the stored cert: %v", err)
	}
	if b, err := os.ReadFile(previous); err != nil || string(b) != "previous" {
		t.Fatalf("previous pair changed: %q, %v", b, err)
	}
}
