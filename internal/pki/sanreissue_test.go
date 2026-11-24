package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestIssueReissuesALeafMissingARequiredSAN(t *testing.T) {
	orig := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = orig })
	ca, err := EnsureCA("ca", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	spec := CertSpec{Name: "apiserver", CN: "kube-apiserver", DNS: []string{"kubernetes"},
		IPs: []string{"10.96.0.1", "127.0.0.1"}, ServerAuth: true}
	if err := ca.Issue(spec); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path("apiserver", "crt"))

	if err := ca.Issue(spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, readFile(t, path("apiserver", "crt"))) {
		t.Fatal("a leaf that already covers its spec was reissued")
	}

	grown := spec
	grown.IPs = []string{"10.43.0.1", "127.0.0.1"}
	if err := ca.Issue(grown); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, path("apiserver", "crt"))
	if slices.Equal(before, after) {
		t.Fatal("a leaf missing a required IP SAN was not reissued")
	}
	if c := parse(t, after); !slices.ContainsFunc(c.IPAddresses, net.ParseIP("10.43.0.1").Equal) {
		t.Errorf("reissued apiserver.crt IPs = %v, want 10.43.0.1 among them", c.IPAddresses)
	}

	shrunk := grown
	shrunk.IPs = []string{"127.0.0.1"}
	if err := ca.Issue(shrunk); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after, readFile(t, path("apiserver", "crt"))) {
		t.Error("a leaf that is a superset of its spec was reissued")
	}

	if err := os.WriteFile(path("apiserver", "crt"), []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ca.Issue(spec); err == nil {
		t.Error("a corrupt existing apiserver.crt was silently accepted or overwritten")
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parse(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
