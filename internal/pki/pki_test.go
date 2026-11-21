package pki

import (
	"bytes"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/version"
)

// The SA pair test's distinct second-write failure, so the joined error proves both.
var errSecondSync = errors.New("second dir sync failed")

const (
	tHost = "cp1"
	tIP   = "10.0.0.5"
)

func genInto(t *testing.T) {
	t.Helper()
	version.PKIDir = t.TempDir()
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
}

func loadCert(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.PKIDir, name+".crt"))
	if err != nil {
		t.Fatalf("read %s.crt: %v", name, err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s.crt: bad PEM", name)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse %s.crt: %v", name, err)
	}
	return c
}

func hasEKU(c *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, e := range c.ExtKeyUsage {
		if e == want {
			return true
		}
	}
	return false
}

func hasStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestSubjectDrift(t *testing.T) {
	genInto(t)
	if got, err := SubjectDrift(tHost, tIP); err != nil || len(got) != 0 {
		t.Fatalf("SubjectDrift on a fresh node = %v, %v; want none", got, err)
	}
	ca, err := loadCA("ca")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []CertSpec{
		{Name: "admin", CN: "admin", O: []string{"system:masters"}, ClientAuth: true},
		{Name: "kube-vip", CN: "kube-vip", O: []string{"system:masters"}, ClientAuth: true},
	} {
		if err := ca.Reissue(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "scheduler.crt")); err != nil {
		t.Fatal(err)
	}
	got, err := SubjectDrift(tHost, tIP)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"admin (CN=admin,O=system:masters)", "kube-vip (CN=kube-vip,O=system:masters)"}
	if !slices.Equal(got, want) {
		t.Errorf("SubjectDrift = %q, want %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "admin.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SubjectDrift(tHost, tIP); err == nil {
		t.Error("SubjectDrift with an unparsable admin.crt: no error, want one")
	}
}

// Pins the certificate matrix Node/RBAC authz and conformance depend on.
func TestCertMatrix(t *testing.T) {
	genInto(t)

	t.Run("apiserver SANs + serverAuth", func(t *testing.T) {
		c := loadCert(t, "apiserver")
		if c.Subject.CommonName != "kube-apiserver" {
			t.Errorf("CN = %q", c.Subject.CommonName)
		}
		for _, dns := range []string{"kubernetes", "kubernetes.default.svc", "kubernetes.default.svc." + netdefault.ClusterDomain, tHost} {
			if !hasStr(c.DNSNames, dns) {
				t.Errorf("missing DNS SAN %q (have %v)", dns, c.DNSNames)
			}
		}
		var ips []string
		for _, ip := range c.IPAddresses {
			ips = append(ips, ip.String())
		}
		for _, ip := range []string{netdefault.APIServerSvcIP, "127.0.0.1", tIP} {
			if !hasStr(ips, ip) {
				t.Errorf("missing IP SAN %q (have %v)", ip, ips)
			}
		}
		if !hasEKU(c, x509.ExtKeyUsageServerAuth) {
			t.Error("apiserver cert lacks serverAuth EKU")
		}
	})

	t.Run("kubelet node identity", func(t *testing.T) {
		c := loadCert(t, "kubelet")
		if c.Subject.CommonName != "system:node:"+tHost {
			t.Errorf("CN = %q, want system:node:%s", c.Subject.CommonName, tHost)
		}
		if !hasStr(c.Subject.Organization, "system:nodes") {
			t.Errorf("O = %v, want system:nodes", c.Subject.Organization)
		}
	})

	t.Run("apiserver-kubelet-client carries no group", func(t *testing.T) {
		c := loadCert(t, "apiserver-kubelet-client")
		if len(c.Subject.Organization) != 0 {
			t.Errorf("O = %v, want none: the apiserver's kubelet client is authorized by an RBAC binding to its user, never by system:masters", c.Subject.Organization)
		}
		if c.Subject.CommonName != "kube-apiserver-kubelet-client" {
			t.Errorf("CN = %q, want kube-apiserver-kubelet-client, the user the kubelet API admin binding names", c.Subject.CommonName)
		}
		if !hasEKU(c, x509.ExtKeyUsageClientAuth) {
			t.Error("apiserver-kubelet-client lacks clientAuth EKU")
		}
	})

	t.Run("kube-vip carries no group", func(t *testing.T) {
		c := loadCert(t, "kube-vip")
		if len(c.Subject.Organization) != 0 {
			t.Errorf("O = %v, want none: kube-vip is authorized by a Role on its lease, never by system:masters", c.Subject.Organization)
		}
		if c.Subject.CommonName != "system:kube-vip" {
			t.Errorf("CN = %q, want system:kube-vip, the user the kube-vip RoleBinding names", c.Subject.CommonName)
		}
		if !hasEKU(c, x509.ExtKeyUsageClientAuth) {
			t.Error("kube-vip lacks clientAuth EKU")
		}
	})

	t.Run("groups (O) for client identities", func(t *testing.T) {
		for name, wantO := range map[string]string{
			"admin":       ClusterAdminsGroup,
			"super-admin": "system:masters",
		} {
			c := loadCert(t, name)
			if !slices.Equal(c.Subject.Organization, []string{wantO}) {
				t.Errorf("%s O = %v, want %q", name, c.Subject.Organization, wantO)
			}
			if !hasEKU(c, x509.ExtKeyUsageClientAuth) {
				t.Errorf("%s lacks clientAuth EKU", name)
			}
		}
	})
}

func TestLeavesSignedByCA(t *testing.T) {
	genInto(t)
	ca := loadCert(t, "ca")
	for _, leaf := range []string{"apiserver", "kubelet", "admin", "super-admin", "controller-manager", "scheduler", "kube-vip"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(ca); err != nil {
			t.Errorf("%s not signed by ca: %v", leaf, err)
		}
	}
	// A kubelet cert is root-signed; as long as etcd trusted the root it was full etcd read/write.
	etcdCA := loadCert(t, "etcd-ca")
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(etcdCA); err != nil {
			t.Errorf("%s not signed by etcd-ca: %v", leaf, err)
		}
		if err := loadCert(t, leaf).CheckSignatureFrom(ca); err == nil {
			t.Errorf("%s unexpectedly chains to the cluster root CA", leaf)
		}
	}
	// The kubelets' one trusted client; nothing else the root signs reaches :10250.
	kca := loadCert(t, KubeletClientCAName)
	if err := loadCert(t, "apiserver-kubelet-client").CheckSignatureFrom(kca); err != nil {
		t.Errorf("apiserver-kubelet-client not signed by %s: %v", KubeletClientCAName, err)
	}
	if err := loadCert(t, "apiserver-kubelet-client").CheckSignatureFrom(ca); err == nil {
		t.Error("apiserver-kubelet-client unexpectedly chains to the cluster root CA")
	}
	fp := loadCert(t, "front-proxy-ca")
	if err := loadCert(t, "front-proxy-client").CheckSignatureFrom(fp); err != nil {
		t.Errorf("front-proxy-client not signed by front-proxy-ca: %v", err)
	}
	if err := loadCert(t, "front-proxy-client").CheckSignatureFrom(ca); err == nil {
		t.Error("front-proxy-client unexpectedly chains to the main CA")
	}
}

func TestSAKeypairAndKeyPerms(t *testing.T) {
	genInto(t)
	for _, f := range []string{"sa.key", "sa.pub", "ca.key"} {
		if _, err := os.Stat(filepath.Join(version.PKIDir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	for _, f := range []string{"ca.key", "kubelet.key", "sa.key"} {
		fi, err := os.Stat(filepath.Join(version.PKIDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perms = %o, want 600", f, fi.Mode().Perm())
		}
	}
}

func TestRotateLeavesTightensLooseKey(t *testing.T) {
	genInto(t)
	p := filepath.Join(version.PKIDir, "front-proxy-client.key")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("front-proxy-client.key perms = %o after rotation, want 600", fi.Mode().Perm())
	}
	if os.SameFile(before, fi) {
		t.Error("rotation rewrote the loose key in place; an open descriptor on it reads the new key")
	}
}

func TestIdempotent(t *testing.T) {
	genInto(t)
	before, err := os.ReadFile(filepath.Join(version.PKIDir, "apiserver.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(version.PKIDir, "apiserver.crt"))
	if string(before) != string(after) {
		t.Error("apiserver cert changed on second GenerateAll (not idempotent)")
	}
}

func TestRotateLeaves(t *testing.T) {
	genInto(t)

	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join(version.PKIDir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		return b
	}
	caBefore := read("ca.crt")
	caKeyBefore := read("ca.key")
	saKeyBefore := read("sa.key")
	fpcaBefore := read("front-proxy-ca.crt")
	apiBefore := loadCert(t, "apiserver")

	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatalf("RotateLeaves: %v", err)
	}
	apiAfter := loadCert(t, "apiserver")

	if apiBefore.SerialNumber.Cmp(apiAfter.SerialNumber) == 0 {
		t.Error("apiserver serial unchanged after rotation") // the real proof of reissue
	}
	// x509 NotAfter has 1s resolution, so a same-second reissue is legitimately equal.
	if apiAfter.NotAfter.Before(apiBefore.NotAfter) {
		t.Errorf("apiserver NotAfter went backwards: before=%s after=%s", apiBefore.NotAfter, apiAfter.NotAfter)
	}
	pool := x509.NewCertPool()
	pool.AddCert(loadCert(t, "ca"))
	if _, err := apiAfter.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("rotated apiserver does not verify against original CA: %v", err)
	}
	if !contains(apiAfter.DNSNames, tHost) {
		t.Errorf("rotated apiserver lost hostname SAN: %v", apiAfter.DNSNames)
	}
	for _, tc := range []struct {
		name   string
		before []byte
	}{{"ca.crt", caBefore}, {"ca.key", caKeyBefore}, {"sa.key", saKeyBefore}, {"front-proxy-ca.crt", fpcaBefore}} {
		if string(read(tc.name)) != string(tc.before) {
			t.Errorf("%s changed during leaf rotation (must not)", tc.name)
		}
	}
}

func TestLeafValidityApplied(t *testing.T) {
	orig := LeafValidity
	LeafValidity = 24 * time.Hour
	defer func() { LeafValidity = orig }()
	genInto(t)

	leaf := loadCert(t, "apiserver")
	if d := leaf.NotAfter.Sub(leaf.NotBefore); d > 48*time.Hour {
		t.Errorf("apiserver validity %s exceeds the short LeafValidity window", d)
	}
	ca := loadCert(t, "ca")
	if d := ca.NotAfter.Sub(ca.NotBefore); d < 365*24*time.Hour {
		t.Errorf("CA validity %s is not long-lived", d)
	}
	got, err := CertNotAfter("apiserver")
	if err != nil || !got.Equal(leaf.NotAfter) {
		t.Errorf("CertNotAfter=%v err=%v, want %v", got, err, leaf.NotAfter)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func readPKI(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.PKIDir, f))
	if err != nil {
		t.Fatalf("read %s: %v", f, err)
	}
	return b
}

func loadBundle(t *testing.T, base string) []*x509.Certificate {
	t.Helper()
	b := readPKI(t, base+"-bundle.crt")
	var certs []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatalf("parse %s-bundle: %v", base, err)
		}
		certs = append(certs, c)
	}
	return certs
}

func poolFromBundle(t *testing.T, base string) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	for _, c := range loadBundle(t, base) {
		p.AddCert(c)
	}
	return p
}

func countPEMBlocks(t *testing.T, f string) int {
	t.Helper()
	b := readPKI(t, f)
	n := 0
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		n++
	}
	return n
}

func TestGenerateAllWritesBundles(t *testing.T) {
	genInto(t)
	if got := len(loadBundle(t, "ca")); got != 1 {
		t.Errorf("fresh ca-bundle has %d certs, want 1", got)
	}
	if got := len(loadBundle(t, "front-proxy-ca")); got != 1 {
		t.Errorf("fresh front-proxy-ca-bundle has %d certs, want 1", got)
	}
	if got := countPEMBlocks(t, "sa-bundle.pub"); got != 1 {
		t.Errorf("fresh sa-bundle has %d keys, want 1", got)
	}
	if !loadBundle(t, "ca")[0].Equal(loadCert(t, "ca")) {
		t.Error("ca-bundle[0] != live ca.crt on fresh install")
	}
}

func TestRotateCAMaterialBundles(t *testing.T) {
	genInto(t)
	caBefore, caKeyBefore, saKeyBefore := readPKI(t, "ca.crt"), readPKI(t, "ca.key"), readPKI(t, "sa.key")

	if err := RotateCAMaterial(false); err != nil {
		t.Fatalf("RotateCAMaterial: %v", err)
	}
	if !CARotationStaged() {
		t.Error("CARotationStaged() false after staging")
	}
	if string(readPKI(t, "ca.crt")) != string(caBefore) {
		t.Error("ca.crt changed before promote")
	}
	if string(readPKI(t, "ca.key")) != string(caKeyBefore) {
		t.Error("ca.key changed before promote")
	}
	if string(readPKI(t, "sa.key")) != string(saKeyBefore) {
		t.Error("sa.key changed before promote")
	}
	if got := len(loadBundle(t, "ca")); got != 2 {
		t.Errorf("ca-bundle has %d certs, want 2", got)
	}
	if got := len(loadBundle(t, "front-proxy-ca")); got != 2 {
		t.Errorf("front-proxy-ca-bundle has %d certs, want 2", got)
	}
	if got := countPEMBlocks(t, "sa-bundle.pub"); got != 2 {
		t.Errorf("sa-bundle has %d keys, want 2", got)
	}
	if n, _ := CABundleCertCount("ca"); n != 2 {
		t.Errorf("CABundleCertCount=%d, want 2", n)
	}
	nc := loadCert(t, "ca-new")
	if !nc.IsCA {
		t.Error("ca-new is not a CA")
	}
	if err := nc.CheckSignatureFrom(nc); err != nil {
		t.Errorf("ca-new not self-signed: %v", err)
	}
	stagedBefore := readPKI(t, "ca-new.crt")
	if err := RotateCAMaterial(false); err != nil {
		t.Fatalf("RotateCAMaterial rerun: %v", err)
	}
	if string(readPKI(t, "ca-new.crt")) != string(stagedBefore) {
		t.Error("ca-new regenerated on rerun (not idempotent)")
	}
}

func TestPromoteAndRotateLeaves(t *testing.T) {
	genInto(t)
	oldCA := loadCert(t, "ca")

	if err := RotateCAMaterial(false); err != nil {
		t.Fatalf("RotateCAMaterial: %v", err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatalf("PromoteCA: %v", err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatalf("PromoteSA: %v", err)
	}
	newCA := loadCert(t, "ca")
	if oldCA.SerialNumber.Cmp(newCA.SerialNumber) == 0 {
		t.Fatal("ca.crt not swapped by PromoteCA")
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatalf("RotateLeaves: %v", err)
	}
	for _, leaf := range []string{"apiserver", "kubelet", "admin", "controller-manager", "scheduler", "kube-vip"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(newCA); err != nil {
			t.Errorf("%s not signed by new CA: %v", leaf, err)
		}
	}
	// Reissuing every leaf from the root would let a routine rotation re-sign etcd's client leaf.
	etcdCARot := loadCert(t, "etcd-ca")
	for _, leaf := range []string{"etcd", "apiserver-etcd-client"} {
		if err := loadCert(t, leaf).CheckSignatureFrom(etcdCARot); err != nil {
			t.Errorf("%s left etcd-ca during a ROOT rotation: %v", leaf, err)
		}
	}
	if _, err := loadCert(t, "apiserver").Verify(x509.VerifyOptions{
		Roots: poolFromBundle(t, "ca"), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("new-signed apiserver fails against old‖new bundle: %v", err)
	}
	found := false
	for _, c := range loadBundle(t, "ca") {
		if c.Equal(oldCA) {
			found = true
		}
	}
	if !found {
		t.Error("old CA missing from bundle during sign-new")
	}
	if _, err := os.Stat(filepath.Join(version.PKIDir, "sa.pre-carotate.key")); err != nil {
		t.Error("sa.pre-carotate.key missing after PromoteSA")
	}
}

func TestFinalizeDropsOld(t *testing.T) {
	genInto(t)
	oldCA := loadCert(t, "ca")
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeCA(); err != nil {
		t.Fatalf("FinalizeCA: %v", err)
	}
	if got := len(loadBundle(t, "ca")); got != 1 {
		t.Errorf("ca-bundle has %d certs after finalize, want 1", got)
	}
	if got := countPEMBlocks(t, "sa-bundle.pub"); got != 1 {
		t.Errorf("sa-bundle has %d keys after finalize, want 1", got)
	}
	oldPool := x509.NewCertPool()
	oldPool.AddCert(oldCA)
	if _, err := loadCert(t, "apiserver").Verify(x509.VerifyOptions{
		Roots: oldPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("apiserver still verifies against old CA after finalize")
	}
	for _, f := range []string{"ca-new.crt", "ca.pre-carotate.crt", "sa-new.key"} {
		if _, err := os.Stat(filepath.Join(version.PKIDir, f)); err == nil {
			t.Errorf("scratch %s not removed by finalize", f)
		}
	}
	if CARotationStaged() {
		t.Error("CARotationStaged() true after finalize")
	}
}

func TestRollbackRestoresOld(t *testing.T) {
	genInto(t)
	caBefore, caKeyBefore := readPKI(t, "ca.crt"), readPKI(t, "ca.key")
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if string(readPKI(t, "ca.crt")) == string(caBefore) {
		t.Fatal("PromoteCA did not change ca.crt")
	}
	if err := RollbackCA(); err != nil {
		t.Fatalf("RollbackCA: %v", err)
	}
	if string(readPKI(t, "ca.crt")) != string(caBefore) {
		t.Error("rollback did not restore old ca.crt")
	}
	if string(readPKI(t, "ca.key")) != string(caKeyBefore) {
		t.Error("rollback did not restore old ca.key")
	}
	if got := len(loadBundle(t, "ca")); got != 1 {
		t.Errorf("ca-bundle has %d certs after rollback, want 1", got)
	}
	if CARotationStaged() {
		t.Error("staged material remains after rollback")
	}
}

func TestRotateCAForce(t *testing.T) {
	genInto(t)
	old, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if err := RotateCAMaterial(false); err == nil {
		t.Error("RotateCAMaterial(false) should refuse with the old root key gone")
	}
	if err := RotateCAMaterial(true); err != nil {
		t.Fatalf("RotateCAMaterial(true): %v", err)
	}
	bundle := loadBundle(t, "ca")
	if len(bundle) != 2 {
		t.Fatalf("force ca-bundle has %d certs, want 2 (new‖old)", len(bundle))
	}
	blk, _ := pem.Decode(old)
	if blk == nil || !bytes.Equal(bundle[1].Raw, blk.Bytes) {
		t.Error("force ca-bundle does not carry the old root certificate")
	}
}

func TestRotateCARequiresForceForEveryLostKey(t *testing.T) {
	for _, key := range []string{"ca.key", "front-proxy-ca.key", "sa.key"} {
		t.Run(key, func(t *testing.T) {
			genInto(t)
			removePKI(t, key)
			if err := RotateCAMaterial(false); err == nil {
				t.Errorf("RotateCAMaterial(false) went ahead without %s", key)
			}
			if ForcedRotation() || fileExists(path("ca-new", "key")) {
				t.Error("the refused rotation staged material or recorded a marker")
			}
			if err := RotateCAMaterial(true); err != nil {
				t.Fatalf("RotateCAMaterial(true): %v", err)
			}
			if !ForcedRotation() {
				t.Error("a rotation without " + key + " is not recorded as forced")
			}
		})
	}
}

func TestRotateCARefusesWithoutRootCert(t *testing.T) {
	genInto(t)
	for _, f := range []string{"ca.crt", "ca.key"} {
		if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-bundle.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RotateCAMaterial(true); !errors.Is(err, ErrNoRootCert) {
		t.Fatalf("RotateCAMaterial(true) = %v, want ErrNoRootCert", err)
	}
	if exists("ca-new") {
		t.Error("a refused rotation staged ca-new")
	}
	after, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-bundle.crt"))
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("a refused rotation changed ca-bundle.crt (err %v)", err)
	}
}

func TestRotateCADerivesALostSAPub(t *testing.T) {
	genInto(t)
	before := readPKI(t, "sa.pub")
	removePKI(t, "sa.pub")
	if err := RotateCAMaterial(true); err != nil {
		t.Fatalf("RotateCAMaterial(true): %v", err)
	}
	if got := readPKI(t, "sa.pub"); !bytes.Equal(got, before) {
		t.Error("the derived sa.pub differs from the lost one")
	}
	want := append(readPKI(t, "sa-new.pub"), before...)
	if got := readPKI(t, "sa-bundle.pub"); !bytes.Equal(got, want) {
		t.Error("sa-bundle.pub is not sa-new.pub followed by the derived sa.pub")
	}
}

func TestRotateCARefusalWithoutForceWritesNothing(t *testing.T) {
	genInto(t)
	removePKI(t, "ca.key", "sa.pub")
	before := pkiSnapshot(t)
	if err := RotateCAMaterial(false); err == nil {
		t.Fatal("RotateCAMaterial(false) without ca.key = nil, want a refusal")
	}
	if !maps.Equal(before, pkiSnapshot(t)) {
		t.Error("a rotation refused for want of --force changed the PKI")
	}
}

func TestRotateCAResumesAHalfStagedSAKeypair(t *testing.T) {
	genInto(t)
	removePKI(t, "ca.key")
	if err := NewSAKeypair("sa-new"); err != nil {
		t.Fatal(err)
	}
	key, pub := readPKI(t, "sa-new.key"), readPKI(t, "sa-new.pub")
	removePKI(t, "sa-new.pub")
	if err := RotateCAMaterial(true); err != nil {
		t.Fatalf("RotateCAMaterial(true): %v", err)
	}
	if !bytes.Equal(readPKI(t, "sa-new.key"), key) {
		t.Error("the staged sa-new.key was replaced")
	}
	if !bytes.Equal(readPKI(t, "sa-new.pub"), pub) {
		t.Error("the derived sa-new.pub is not the staged key's public half")
	}
	want := append(append([]byte{}, pub...), readPKI(t, "sa.pub")...)
	if !bytes.Equal(readPKI(t, "sa-bundle.pub"), want) {
		t.Error("sa-bundle.pub is not sa-new.pub followed by sa.pub")
	}
}

func TestRotateCARefusesWithoutAnOldPublicHalf(t *testing.T) {
	for name, lost := range map[string][]string{
		"front-proxy-ca.crt": {"front-proxy-ca.crt"},
		"sa.pub and sa.key":  {"sa.pub", "sa.key"},
	} {
		t.Run(name, func(t *testing.T) {
			genInto(t)
			removePKI(t, lost...)
			before := pkiSnapshot(t)
			if err := RotateCAMaterial(true); !errors.Is(err, ErrNoOldPublicHalf) {
				t.Fatalf("RotateCAMaterial(true) = %v, want ErrNoOldPublicHalf", err)
			}
			if !maps.Equal(before, pkiSnapshot(t)) {
				t.Error("a refused rotation changed the PKI")
			}
		})
	}
}

func TestRotateCARefusesARootCertTheBundleDoesNotCarry(t *testing.T) {
	genInto(t)
	removePKI(t, "ca.key")
	node := version.PKIDir
	version.PKIDir = t.TempDir()
	if _, err := NewCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	other := readPKI(t, "ca.crt")
	version.PKIDir = node
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.crt"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	before := pkiSnapshot(t)
	if err := RotateCAMaterial(true); !errors.Is(err, ErrNoRootCert) {
		t.Fatalf("RotateCAMaterial(true) = %v, want ErrNoRootCert", err)
	}
	if !maps.Equal(before, pkiSnapshot(t)) {
		t.Error("a refused rotation changed the PKI")
	}
}

// The shared-cert fetch writes the live files too, so the bundles would carry the new signer twice.
func TestRotateCARefusesALiveSignerThatIsItsStagedReplacement(t *testing.T) {
	for name, pairs := range map[string][][2]string{
		"root CA":        {{"ca-new.crt", "ca.crt"}, {"ca-new.key", "ca.key"}},
		"front-proxy CA": {{"front-proxy-ca-new.crt", "front-proxy-ca.crt"}, {"front-proxy-ca-new.key", "front-proxy-ca.key"}},
		"SA keypair":     {{"sa-new.key", "sa.key"}, {"sa-new.pub", "sa.pub"}},
	} {
		t.Run(name, func(t *testing.T) {
			genInto(t)
			if err := RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			for _, p := range pairs {
				if err := os.WriteFile(filepath.Join(version.PKIDir, p[1]), readPKI(t, p[0]), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := pkiSnapshot(t)
			if err := RotateCAMaterial(false); err == nil {
				t.Fatal("RotateCAMaterial re-staged a node whose live signer is its staged replacement")
			}
			if !maps.Equal(before, pkiSnapshot(t)) {
				t.Error("the refused re-run changed the PKI")
			}
		})
	}
}

func TestRotateCAKeyPerms(t *testing.T) {
	genInto(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ca-new.key", "sa-new.key"} {
		fi, err := os.Stat(filepath.Join(version.PKIDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perms=%o, want 600", f, fi.Mode().Perm())
		}
	}
}

// One row set across all three trust readers; a disagreement is a contract break.
func TestBundleReadersAgreedInputs(t *testing.T) {
	genInto(t)
	ca, other := readPKI(t, "ca.crt"), readPKI(t, "front-proxy-ca.crt")
	badDER := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a DER certificate")})
	for _, row := range []struct {
		name string
		in   []byte
		want int // 0: the parser must refuse
	}{
		{"a clean bundle of two CA certificates", append(append([]byte{}, ca...), other...), 2},
		{"a single CA certificate", ca, 1},
		{"a block whose DER does not parse", badDER, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			if err := os.WriteFile(bundleCrtPath("ca"), row.in, 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "bundle.crt")
			if err := os.WriteFile(path, row.in, 0o644); err != nil {
				t.Fatal(err)
			}

			n, err := CABundleCertCount("ca")
			if row.want == 0 {
				if err == nil {
					t.Error("CABundleCertCount accepted an input parseTrustBundle refuses; the parsers would disagree")
				}
				if _, err := bundleCerts(path); err == nil {
					t.Error("bundleCerts accepted an input parseTrustBundle refuses; the parsers would disagree")
				}
				return
			}
			if err != nil {
				t.Fatalf("CABundleCertCount refused: %v", err)
			}
			if n != row.want {
				t.Errorf("CABundleCertCount = %d, want %d", n, row.want)
			}
			certs, err := bundleCerts(path)
			if err != nil {
				t.Fatalf("bundleCerts refused: %v", err)
			}
			if len(certs) != row.want {
				t.Errorf("bundleCerts parsed %d certificates, want %d", len(certs), row.want)
			}
		})
	}
}

func catBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// The three readers differ only by declared policy, never by ad-hoc parsing.
func TestOneBundleParser(t *testing.T) {
	genInto(t)
	ca, other := readPKI(t, "ca.crt"), readPKI(t, "front-proxy-ca.crt")
	leaf := readPKI(t, "apiserver.crt")
	blk, _ := pem.Decode(ca)
	if blk == nil {
		t.Fatal("ca.crt is not PEM")
	}
	// A headered block still carries its DER; the kubelet's parser drops it outright.
	headers := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: blk.Bytes})
	foreign := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a certificate")})
	two := catBytes(ca, other)
	for _, row := range []struct {
		name string
		in   []byte
		// want* of 0: that reader must refuse.
		wantCount, wantEtcd, wantKubelet int
	}{
		{"a clean bundle of two CA certificates", two, 2, 2, 2},
		{"a single CA certificate", ca, 1, 1, 1},
		{"newline-ended leading whitespace before the first block", catBytes([]byte(" \n"), two), 2, 2, 2},
		{"a space immediately before BEGIN", catBytes([]byte(" "), ca), 1, 0, 0},
		{"a tab immediately before BEGIN", catBytes([]byte("\t"), ca), 1, 0, 0},
		{"a lone CR immediately before BEGIN", catBytes([]byte("\r"), ca), 1, 0, 0},
		// Whitespace after the last block anchors no block; client-go's parser stops cleanly at it.
		{"a space after the final newline", catBytes(ca, []byte(" ")), 1, 1, 1},
		{"a tab after the final newline", catBytes(ca, []byte("\t")), 1, 1, 1},
		{"a lone CR after the final newline", catBytes(ca, []byte("\r")), 1, 1, 1},
		{"a newline then a space after the final block", catBytes(two, []byte("\n ")), 2, 2, 2},
		{"a CERTIFICATE block carrying PEM headers", headers, 1, 1, 0},
		{"a non-certificate PEM block after the CA", catBytes(ca, foreign), 0, 1, 0},
		{"a non-CA certificate", leaf, 0, 1, 0},
		{"embedded text before a block", catBytes([]byte("owned-by-nobody\n"), ca), 0, 1, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, pc := range []struct {
				what string
				pol  BundlePolicy
				want int
			}{
				{"the bundle counter's declared policy", CountPolicy, row.wantCount},
				{"the etcd trust reader's declared policy", EtcdPolicy, row.wantEtcd},
				{"the kubelet-verbatim policy the worker refresh keeps", KubeletPolicy, row.wantKubelet},
			} {
				certs, err := DecodePEMBundle(row.in, pc.pol)
				if pc.want == 0 {
					if err == nil {
						t.Errorf("%s accepted %q, which that policy must refuse", pc.what, row.name)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s refused %q: %v", pc.what, row.name, err)
				}
				if len(certs) != pc.want {
					t.Errorf("%s decoded %d certificates from %q, want %d", pc.what, len(certs), row.name, pc.want)
				}
			}
			if err := os.WriteFile(bundleCrtPath("ca"), row.in, 0o644); err != nil {
				t.Fatal(err)
			}
			n, err := CABundleCertCount("ca")
			bundleReaderAgrees(t, "CABundleCertCount", row.in, CountPolicy, n, err)
			path := filepath.Join(t.TempDir(), "etcd-bundle.crt")
			if err := os.WriteFile(path, row.in, 0o644); err != nil {
				t.Fatal(err)
			}
			etcdCerts, err := bundleCerts(path)
			bundleReaderAgrees(t, "bundleCerts", row.in, EtcdPolicy, len(etcdCerts), err)
		})
	}
}

// Refusal reasons may differ, wrapped with file names; acceptance must agree.
func bundleReaderAgrees(t *testing.T, reader string, in []byte, pol BundlePolicy, n int, err error) {
	t.Helper()
	certs, derr := DecodePEMBundle(in, pol)
	if (err == nil) != (derr == nil) {
		t.Errorf("%s disagreed with the shared decoder on accept/refuse: reader err = %v, decoder err = %v", reader, err, derr)
		return
	}
	if err == nil && n != len(certs) {
		t.Errorf("%s answered %d certificates, the shared decoder decoded %d under the reader's declared policy", reader, n, len(certs))
	}
}

func TestSigner(t *testing.T) {
	genInto(t)
	for _, base := range []string{"ca", "sa"} {
		if got := Signer(base); got != base {
			t.Errorf("Signer(%q) = %q before any rotation", base, got)
		}
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if got := Signer("ca"); got != "ca" {
		t.Errorf("Signer(ca) = %q with no staged pair to stand in", got)
	}
	if err := RotateCAMaterial(true); err != nil {
		t.Fatal(err)
	}
	if got := Signer("ca"); got != "ca-new" {
		t.Errorf("Signer(ca) = %q with ca.key gone and ca-new staged, want ca-new", got)
	}
	if got := Signer("sa"); got != "sa" {
		t.Errorf("Signer(sa) = %q with sa.key still there", got)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if got := Signer("ca"); got != "ca" {
		t.Errorf("Signer(ca) = %q after promotion", got)
	}
}

func pkiSnapshot(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(version.PKIDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			out[e.Name()] = string(readPKI(t, e.Name()))
		}
	}
	return out
}

func wantRollbackRefused(t *testing.T, stage string) {
	t.Helper()
	before := pkiSnapshot(t)
	forced := ForcedRotation()
	err := RollbackCA()
	if !errors.Is(err, ErrRollbackUnsafe) {
		t.Fatalf("%s: RollbackCA = %v, want ErrRollbackUnsafe", stage, err)
	}
	if resume := strings.Contains(err.Error(), "--cluster --force"); resume != forced {
		t.Errorf("%s: the refusal of a rotation forced=%v offers the --cluster --force resume=%v: %v", stage, forced, resume, err)
	}
	after := pkiSnapshot(t)
	if len(after) != len(before) {
		t.Fatalf("%s: a refused rollback changed the PKI file set", stage)
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("%s: a refused rollback changed %s", stage, name)
		}
	}
}

func removePKI(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.Remove(filepath.Join(version.PKIDir, n)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRollbackRefusesAForcedRotation(t *testing.T) {
	for _, lost := range [][]string{{"ca.key"}, nil} {
		t.Run(fmt.Sprintf("lost %v", lost), func(t *testing.T) {
			genInto(t)
			removePKI(t, lost...)
			if err := RotateCAMaterial(true); err != nil {
				t.Fatal(err)
			}
			if !ForcedRotation() {
				t.Fatal("RotateCAMaterial(true) did not record a forced rotation")
			}
			wantRollbackRefused(t, "before sign-new")
			if err := PromoteCA(); err != nil {
				t.Fatal(err)
			}
			if err := PromoteSA(); err != nil {
				t.Fatal(err)
			}
			wantRollbackRefused(t, "after sign-new")
		})
	}
}

func TestRollbackChecksThePairsItWouldRestore(t *testing.T) {
	for _, key := range []string{"ca.key", "front-proxy-ca.key", "sa.key"} {
		t.Run(key+" lost", func(t *testing.T) {
			genInto(t)
			removePKI(t, key)
			if err := RotateCAMaterial(true); err != nil {
				t.Fatal(err)
			}
			removePKI(t, caForcedMarker)
			wantRollbackRefused(t, "before sign-new")
			if err := PromoteCA(); err != nil {
				t.Fatal(err)
			}
			if err := PromoteSA(); err != nil {
				t.Fatal(err)
			}
			wantRollbackRefused(t, "after sign-new")
		})
	}
	t.Run("a public half lost after sign-new", func(t *testing.T) {
		genInto(t)
		if err := RotateCAMaterial(true); err != nil {
			t.Fatal(err)
		}
		if err := PromoteCA(); err != nil {
			t.Fatal(err)
		}
		if err := PromoteSA(); err != nil {
			t.Fatal(err)
		}
		removePKI(t, caForcedMarker, "sa.pre-carotate.pub")
		wantRollbackRefused(t, "after sign-new")
	})
	t.Run("a promotion that stopped between the certificate and the key", func(t *testing.T) {
		genInto(t)
		old := readPKI(t, "ca.crt")
		if err := RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.pre-carotate.crt"), old, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path("ca-new", "crt"), path("ca", "crt")); err != nil {
			t.Fatal(err)
		}
		if err := RollbackCA(); err != nil {
			t.Fatalf("RollbackCA = %v; the old key is still live and the old certificate backed up", err)
		}
		if !bytes.Equal(readPKI(t, "ca.crt"), old) || !keyMatches(path("ca", "crt"), path("ca", "key")) {
			t.Error("rollback did not restore the old root as a matching pair")
		}
	})
}

func TestSignNewResumes(t *testing.T) {
	genInto(t)
	removePKI(t, "ca.key")
	if err := RotateCAMaterial(true); err != nil {
		t.Fatal(err)
	}
	if SignNewResumable() {
		t.Fatal("resumable before anything was promoted")
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if !SignNewResumable() {
		t.Fatal("not resumable after the root was promoted and nothing recorded")
	}
	if err := FinishPromotion(); err != nil {
		t.Fatal(err)
	}
	if left := CAStagedFiles(); len(left) != 0 {
		t.Errorf("FinishPromotion left %v staged", left)
	}
	if !keyMatches(filepath.Join(version.PKIDir, "sa.pub"), path("sa", "key")) {
		t.Error("the promoted SA keypair does not match")
	}
	if err := MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	if SignNewResumable() {
		t.Error("still resumable after the promotion was recorded")
	}
}

// The assertion tests the writer's mode, not the caller's umask.
func umask022(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// A reader holding the old certificate open (the hard link below) keeps the old bytes.
func TestCertWritesReplaceTheFileNotTruncate(t *testing.T) {
	umask022(t)
	genInto(t)
	before := readPKI(t, "apiserver.crt")
	reader := path("apiserver", "crt") + ".reader"
	if err := os.Link(path("apiserver", "crt"), reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(reader) })

	if err := RotateLeaves(tHost, tIP, nil); err != nil {
		t.Fatalf("RotateLeaves: %v", err)
	}

	if bytes.Equal(readPKI(t, "apiserver.crt"), before) {
		t.Fatal("RotateLeaves did not reissue apiserver.crt")
	}
	if b, err := os.ReadFile(reader); err != nil || !bytes.Equal(b, before) {
		t.Errorf("the reissue rewrote apiserver.crt in place: the linked reader of the old certificate holds the new bytes (err %v)", err)
	}
	if fi, err := os.Stat(path("apiserver", "crt")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o644 {
		t.Errorf("reissued apiserver.crt perms = %o, want 644", fi.Mode().Perm())
	}
}

// A truncated sa.key stops every apiserver from starting.
func TestSAKeypairWritesReplaceTheFileNotTruncate(t *testing.T) {
	umask022(t)
	genInto(t)
	oldKey, oldPub := readPKI(t, "sa.key"), readPKI(t, "sa.pub")
	readers := map[string]string{}
	for _, name := range []string{"sa.key", "sa.pub"} {
		reader := filepath.Join(version.PKIDir, name+".reader")
		if err := os.Link(filepath.Join(version.PKIDir, name), reader); err != nil {
			t.Fatal(err)
		}
		readers[name] = reader
	}
	t.Cleanup(func() {
		for _, reader := range readers {
			os.Remove(reader)
		}
	})

	if err := NewSAKeypair("sa"); err != nil {
		t.Fatalf("NewSAKeypair: %v", err)
	}

	if bytes.Equal(readPKI(t, "sa.key"), oldKey) || bytes.Equal(readPKI(t, "sa.pub"), oldPub) {
		t.Fatal("NewSAKeypair did not mint a fresh pair")
	}
	for name, old := range map[string][]byte{"sa.key": oldKey, "sa.pub": oldPub} {
		if b, err := os.ReadFile(readers[name]); err != nil || !bytes.Equal(b, old) {
			t.Errorf("the rewrite truncated %s in place: the linked reader of the old file holds the new bytes (err %v)", name, err)
		}
	}
	for name, want := range map[string]os.FileMode{"sa.key": 0o600, "sa.pub": 0o644} {
		fi, err := os.Stat(filepath.Join(version.PKIDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s perms = %o, want %o", name, fi.Mode().Perm(), want)
		}
	}
}

// A write that failed half-way onto a live control plane is worse than a clean failure.
func TestCertWriteFailureLeavesNothingBehind(t *testing.T) {
	genInto(t)
	before := pkiSnapshot(t)
	if err := os.Remove(path("apiserver", "crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path("apiserver", "crt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path("apiserver", "crt")) })

	if err := RotateLeaves(tHost, tIP, nil); err == nil {
		t.Fatal("RotateLeaves wrote over a directory")
	}

	after := pkiSnapshot(t) // skips the directory standing in for apiserver.crt
	want := maps.Clone(before)
	delete(want, "apiserver.crt")
	if !maps.Equal(want, after) {
		t.Errorf("a failed certificate write changed the PKI: want %v, got %v", want, after)
	}
	entries, err := os.ReadDir(version.PKIDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("a failed certificate write left a temp file %s behind", e.Name())
		}
	}
}

// The real writer renames the file into place and then reports ErrNotDurable.
func failDirSync(t *testing.T) {
	t.Helper()
	writeSeams = atomicfile.Seams{SyncDir: func(string) error { return syscall.EIO }}
	t.Cleanup(func() { writeSeams = atomicfile.Seams{} })
}

// EnsureCA refuses a partial pair, so stopping there strands a certificate with no key.
func TestNewCACompletesThePairWhenTheCertificateLandsDurably(t *testing.T) {
	version.PKIDir = t.TempDir()
	failDirSync(t)
	if _, err := NewCA("ca", "stratum-ca"); !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("NewCA = %v, want the certificate write's ErrNotDurable", err)
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(version.PKIDir, "ca.crt"), filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatalf("the pair on disk does not load together: %v", err)
	}
	if _, err := EnsureCA("ca", "stratum-ca"); err != nil {
		t.Fatalf("the retry did not load the completed pair: %v", err)
	}
}

// Without completion, the skip-if-SANs-match path pairs the new cert with the old key.
func TestIssueCompletesThePairWhenTheCertificateLandsDurably(t *testing.T) {
	version.PKIDir = t.TempDir()
	ca, err := EnsureCA("ca", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	etcd := CertSpec{Name: "etcd", CN: "etcd", DNS: []string{"localhost"},
		IPs: []string{"127.0.0.1", tIP}, ServerAuth: true, ClientAuth: true}
	if err := ca.Issue(etcd); err != nil {
		t.Fatal(err)
	}
	failDirSync(t)
	if err := ca.Reissue(etcd); !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("Reissue = %v, want the certificate write's ErrNotDurable", err)
	}
	if _, err := tls.LoadX509KeyPair(path("etcd", "crt"), path("etcd", "key")); err != nil {
		t.Fatalf("the leaf pair on disk does not load together: %v", err)
	}
	if err := ca.Issue(etcd); err != nil {
		t.Fatalf("the retry did not recognize the completed leaf: %v", err)
	}
}

// EnsureSAKeypair skips whenever sa.key exists, so a stranded key never gains its public half.
func TestNewSAKeypairCompletesThePairWhenTheKeyLandsDurably(t *testing.T) {
	version.PKIDir = t.TempDir()
	var syncs atomic.Int32
	writeSeams = atomicfile.Seams{SyncDir: func(string) error {
		if syncs.Add(1) == 1 {
			return syscall.EIO
		}
		return errSecondSync
	}}
	t.Cleanup(func() { writeSeams = atomicfile.Seams{} })
	err := NewSAKeypair("sa")
	if !errors.Is(err, atomicfile.ErrNotDurable) || !errors.Is(err, syscall.EIO) || !errors.Is(err, errSecondSync) {
		t.Fatalf("NewSAKeypair = %v, want both writes' durability errors joined", err)
	}
	kb, err := os.ReadFile(filepath.Join(version.PKIDir, "sa.key"))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := os.ReadFile(filepath.Join(version.PKIDir, "sa.pub"))
	if err != nil {
		t.Fatalf("sa.pub was not written after the key landed: %v", err)
	}
	kblk, _ := pem.Decode(kb)
	k, err := x509.ParsePKCS1PrivateKey(kblk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pblk, _ := pem.Decode(pb)
	pubAny, err := x509.ParsePKIXPublicKey(pblk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok || !pub.Equal(&k.PublicKey) {
		t.Error("sa.pub does not carry the key's public half")
	}
	if err := EnsureSAKeypair(); err != nil {
		t.Fatalf("the retry did not accept the completed pair: %v", err)
	}
}

// A stopped rollback would pair the restored old certificate with the new signer's key.
func TestRollbackCompletesThePairWhenTheRestoreLandsDurably(t *testing.T) {
	genInto(t)
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	// The sa key's write is the fifth directory sync, the one that fails here.
	var syncs atomic.Int32
	writeSeams = atomicfile.Seams{SyncDir: func(string) error {
		if syncs.Add(1) == 5 {
			return syscall.EIO
		}
		return nil
	}}
	t.Cleanup(func() { writeSeams = atomicfile.Seams{} })
	if err := RollbackCA(); !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("RollbackCA = %v, want the sa restore's ErrNotDurable", err)
	}
	for _, pair := range []string{"ca", "front-proxy-ca"} {
		if _, err := tls.LoadX509KeyPair(path(pair, "crt"), path(pair, "key")); err != nil {
			t.Fatalf("the restored %s pair on disk does not load together: %v", pair, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(version.PKIDir, "sa.pub"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(version.PKIDir, "sa.pre-carotate.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("sa.pub after the rollback is not the backed-up public half; the sa pair's completion is unpinned")
	}
}

func TestReadRootTrustPrefersTheBundleElseCACert(t *testing.T) {
	readFile := func(t *testing.T, p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return b
	}

	t.Run("mid-rotation the bundle's old‖new wins over ca.crt", func(t *testing.T) {
		genInto(t)
		if err := RotateCAMaterial(false); err != nil {
			t.Fatalf("RotateCAMaterial: %v", err)
		}
		bundlePath := filepath.Join(version.PKIDir, RootTrustBundleName)
		want := readFile(t, bundlePath)
		b, source, err := ReadRootTrust()
		if err != nil {
			t.Fatalf("ReadRootTrust: %v", err)
		}
		if source != bundlePath {
			t.Errorf("source = %q, want the bundle %q: a joiner embedding ca.crt alone would refuse the staged root after finalize", source, bundlePath)
		}
		if !bytes.Equal(b, want) {
			t.Error("ReadRootTrust did not return the bundle's bytes")
		}
	})

	t.Run("a node predating the bundle files falls back to ca.crt", func(t *testing.T) {
		genInto(t)
		bundlePath := filepath.Join(version.PKIDir, RootTrustBundleName)
		caPath := filepath.Join(version.PKIDir, RootCAName+".crt")
		if err := os.Remove(bundlePath); err != nil {
			t.Fatalf("remove the bundle: %v", err)
		}
		want := readFile(t, caPath)
		b, source, err := ReadRootTrust()
		if err != nil {
			t.Fatalf("ReadRootTrust: %v", err)
		}
		if source != caPath {
			t.Errorf("source = %q, want ca.crt %q: the fallback keeps a bundle-less node joinable", source, caPath)
		}
		if !bytes.Equal(b, want) {
			t.Error("the fallback did not return ca.crt's bytes")
		}
	})

	t.Run("neither file readable is an error, never empty trust", func(t *testing.T) {
		if b, source, err := ReadRootTrustFrom(filepath.Join(t.TempDir(), "absent-pki")); err == nil {
			t.Errorf("ReadRootTrustFrom = (%q, %v, nil), want an error for a directory with neither file", source, b)
		}
	})

	t.Run("a bundle that reads but does not parse is returned as it is", func(t *testing.T) {
		genInto(t)
		bundlePath := filepath.Join(version.PKIDir, RootTrustBundleName)
		garbage := []byte("owned-by-nobody\n")
		if err := os.WriteFile(bundlePath, garbage, 0o644); err != nil {
			t.Fatal(err)
		}
		b, source, err := ReadRootTrust()
		if err != nil {
			t.Fatalf("ReadRootTrust: %v", err)
		}
		if source != bundlePath {
			t.Errorf("source = %q, want the bundle %q: the reader classifies files, it does not parse them", source, bundlePath)
		}
		if !bytes.Equal(b, garbage) {
			t.Errorf("the unparseable bundle was rewritten to %q, want it returned as it is — the consumer refuses it under its own policy", b)
		}
	})
}
