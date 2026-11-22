// Package pki generates and rotates the cluster PKI under version.PKIDir.
package pki

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	rsaBits = 2048
	// CA lifetime; also the controller-manager's signing duration, set past its one-year default.
	CAValidity = 10 * 365 * 24 * time.Hour
)

// LeafValidity is a var so tests and a future --cert-validity flag can shorten it.
var LeafValidity = 365 * 24 * time.Hour

type CA struct {
	Cert *x509.Certificate
	Key  *rsa.PrivateKey
}

type CertSpec struct {
	Name       string
	CN         string
	O          []string
	DNS        []string
	IPs        []string
	ServerAuth bool
	ClientAuth bool
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

// MkdirAll does not tighten a pre-existing dir, so the mode is enforced here.
func EnsureDir() error {
	if err := os.MkdirAll(version.PKIDir, 0o700); err != nil {
		return err
	}
	return os.Chmod(version.PKIDir, 0o700)
}

func path(name, ext string) string { return filepath.Join(version.PKIDir, name+"."+ext) }

func exists(name string) bool {
	_, e1 := os.Stat(path(name, "crt"))
	_, e2 := os.Stat(path(name, "key"))
	return e1 == nil && e2 == nil
}

// EtcdCAExists reports whether the etcd CA certificate and key are on disk.
func EtcdCAExists() bool { return exists(EtcdCAName) }

// A test seam into the real atomic writer; production leaves it zero.
var writeSeams atomicfile.Seams

func writeCertPEM(name string, der []byte) error {
	// Replaces, never truncates: a reader holding the old certificate keeps its bytes.
	return atomicfile.WriteFileWith(path(name, "crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644, writeSeams)
}

func writeKeyPEM(name string, key *rsa.PrivateKey) error {
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return host.WriteFileAtomically(path(name, "key"), b, 0o600)
}

// Writes the second half even when the first lands but reports ErrNotDurable.
func writePair(first, second func() error) error {
	firstErr := first()
	if errors.Is(firstErr, atomicfile.ErrNotDurable) {
		return errors.Join(firstErr, second())
	}
	if firstErr != nil {
		return firstErr
	}
	return second()
}

// A partial pair may be the only recovery material; never overwrite it implicitly.
func EnsureCA(name, cn string) (*CA, error) {
	_, certErr := os.Stat(path(name, "crt"))
	_, keyErr := os.Stat(path(name, "key"))
	if certErr != nil && !errors.Is(certErr, os.ErrNotExist) {
		return nil, fmt.Errorf("CA %s certificate: %w", name, certErr)
	}
	if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		return nil, fmt.Errorf("CA %s key: %w", name, keyErr)
	}
	if certErr == nil && keyErr == nil {
		return loadCA(name)
	}
	if certErr == nil || keyErr == nil {
		return nil, fmt.Errorf("CA %s has an incomplete certificate/key pair; refusing to overwrite recovery material", name)
	}
	return NewCA(name, cn)
}

// Stages rotation material; EnsureCA is the skip-if-exists wrapper.
func NewCA(name, cn string) (*CA, error) {
	ca, err := GenerateCA(cn, nil)
	if err != nil {
		return nil, err
	}
	if err := writePair(
		func() error { return writeCertPEM(name, ca.Cert.Raw) },
		func() error { return writeKeyPEM(name, ca.Key) },
	); err != nil {
		return nil, err
	}
	return ca, nil
}

// Enforced as critical X.509 name constraints that also forbid sub-CAs.
type Constraints struct {
	DNS []string
	IPs []*net.IPNet
}

// A nil c leaves the CA unconstrained.
func GenerateCA(cn string, c *Constraints) (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if c != nil {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.MaxPathLenZero = true
		tmpl.PermittedDNSDomainsCritical = true
		tmpl.PermittedDNSDomains = c.DNS
		tmpl.PermittedIPRanges = c.IPs
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

func (ca *CA) PEM() (certPEM, keyPEM []byte) {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ca.Key)})
}

func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("pki: bad CA PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, errors.New("pki: the certificate is not a CA")
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

func loadCA(name string) (*CA, error) {
	cpem, err := os.ReadFile(path(name, "crt"))
	if err != nil {
		return nil, err
	}
	kpem, err := os.ReadFile(path(name, "key"))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(cpem)
	kb, _ := pem.Decode(kpem)
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("pki: bad PEM for CA %s", name)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

func (ca *CA) Issue(s CertSpec) error { return ca.issue(s, false) }

func (ca *CA) Reissue(s CertSpec) error { return ca.issue(s, true) }

func (ca *CA) issue(s CertSpec, force bool) error { return ca.issueWithWriter(os.Stdout, s, force) }

func (ca *CA) issueWithWriter(w io.Writer, s CertSpec, force bool) error {
	if exists(s.Name) && !force {
		missing, err := missingSANs(s)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		// A superset is left alone, so a SAN dropping out of the spec never churns certs.
		fmt.Fprintf(w, "stratum: %s.crt does not cover %s; reissuing it\n", s.Name, strings.Join(missing, ", "))
	}
	der, key, err := ca.signLeaf(s)
	if err != nil {
		return err
	}
	return writePair(
		func() error { return writeCertPEM(s.Name, der) },
		func() error { return writeKeyPEM(s.Name, key) },
	)
}

// An unparsable certificate is an error, never a silent overwrite.
func missingSANs(s CertSpec) ([]string, error) {
	b, err := os.ReadFile(path(s.Name, "crt"))
	if err != nil {
		return nil, fmt.Errorf("read %s.crt to check its SANs: %w", s.Name, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s.crt: no PEM certificate", s.Name)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s.crt: %w", s.Name, err)
	}
	var missing []string
	for _, want := range s.IPs {
		ip := net.ParseIP(want)
		if ip == nil {
			continue
		}
		if !slices.ContainsFunc(c.IPAddresses, ip.Equal) {
			missing = append(missing, "IP:"+want)
		}
	}
	for _, want := range s.DNS {
		if !slices.Contains(c.DNSNames, want) {
			missing = append(missing, "DNS:"+want)
		}
	}
	return missing, nil
}

// A missing leaf is not drift.
func SubjectDrift(host, nodeIP string) ([]string, error) {
	var drifted []string
	for _, s := range append(LeafSpecs(host, nodeIP, nil), frontProxyClientSpec) {
		c, err := readCertOptional(s.Name)
		if err != nil {
			return nil, fmt.Errorf("read %s.crt to check its subject: %w", s.Name, err)
		}
		if c != nil && (c.Subject.CommonName != s.CN || !slices.Equal(c.Subject.Organization, s.O)) {
			drifted = append(drifted, fmt.Sprintf("%s (%s)", s.Name, c.Subject))
		}
	}
	return drifted, nil
}

func (ca *CA) signLeaf(s CertSpec) ([]byte, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, nil, err
	}
	var eku []x509.ExtKeyUsage
	if s.ServerAuth {
		eku = append(eku, x509.ExtKeyUsageServerAuth)
	}
	if s.ClientAuth {
		eku = append(eku, x509.ExtKeyUsageClientAuth)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: s.CN, Organization: s.O},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(LeafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  eku,
		DNSNames:     s.DNS,
	}
	for _, ip := range s.IPs {
		if p := net.ParseIP(ip); p != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, p)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, err
	}
	return der, key, nil
}

// Loads the live CA for a caller that mints short-lived leaves in memory.
func LoadCA(name string) (*CA, error) { return loadCA(name) }

// For process-local, re-mintable certs; neither idempotent nor persisted.
func (ca *CA) IssuePEM(s CertSpec) (certPEM, keyPEM []byte, err error) {
	der, key, err := ca.signLeaf(s)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// Trusts both roots across a rotation; right caBundle for a webhook on this CA.
func ReadCABundle(base string) ([]byte, error) {
	return os.ReadFile(bundleCrtPath(base))
}

func EnsureSAKeypair() error {
	if _, err := os.Stat(filepath.Join(version.PKIDir, "sa.key")); err == nil {
		return nil
	}
	return NewSAKeypair("sa")
}

// Stages rotation material; EnsureSAKeypair is the skip-if-exists wrapper.
func NewSAKeypair(name string) error {
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	return writePair(
		func() error {
			return atomicfile.WriteFileWith(filepath.Join(version.PKIDir, name+".key"),
				pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600, writeSeams)
		},
		func() error {
			return atomicfile.WriteFileWith(filepath.Join(version.PKIDir, name+".pub"),
				pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0o644, writeSeams)
		},
	)
}

// Signed by the front-proxy CA, a separate trust domain, not the root.
var frontProxyClientSpec = CertSpec{Name: "front-proxy-client", CN: "front-proxy-client", ClientAuth: true}

// Apiserver-cert names from the cluster network; nil or empty is the default.
type APISANs struct {
	IPs    []string
	Domain string
}

func (s *APISANs) ips() []string {
	if s == nil {
		return nil
	}
	return s.IPs
}

func (s *APISANs) domain() string {
	var domain string
	if s != nil {
		domain = s.Domain
	}
	return netcfg.DomainOr(domain)
}

// Bound to cluster-admin by a ClusterRoleBinding of the same name.
const ClusterAdminsGroup = "stratum:cluster-admins"

// The one identity etcd's peer port accepts (--peer-cert-allowed-cn).
const EtcdCN = "etcd"

// nodeIP may be "<v4>,<v6>" under dual-stack; sans carries the cluster-network names.
func LeafSpecs(host, nodeIP string, sans *APISANs) []CertSpec {
	nodeIPs := strings.Split(nodeIP, ",")
	etcdIPs := append([]string{"127.0.0.1"}, nodeIPs...)
	svcIP, found := netcfg.Default().ServiceIPv4, false
	var extra []string
	for _, ip := range sans.ips() {
		// The first IPv4 extra is the kubernetes Service IP; later IPv4 entries are plain SANs.
		if parsed := net.ParseIP(ip); !found && parsed != nil && parsed.To4() != nil {
			svcIP, found = ip, true
			continue
		}
		extra = append(extra, ip)
	}
	apiIPs := append([]string{svcIP, "127.0.0.1"}, nodeIPs...)
	apiIPs = append(apiIPs, extra...)
	return []CertSpec{
		{Name: "etcd", CN: EtcdCN, DNS: []string{"localhost"}, IPs: etcdIPs, ServerAuth: true, ClientAuth: true},
		{Name: "apiserver", CN: "kube-apiserver",
			DNS: []string{"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc." + sans.domain(), host},
			IPs: apiIPs, ServerAuth: true, ClientAuth: true},
		{Name: "apiserver-etcd-client", CN: "kube-apiserver-etcd-client", IPs: []string{"127.0.0.1"}, ClientAuth: true},
		{Name: "apiserver-kubelet-client", CN: "kube-apiserver-kubelet-client", DNS: []string{host}, ClientAuth: true},
		{Name: "admin", CN: "admin", O: []string{ClusterAdminsGroup}, DNS: []string{"localhost"}, ClientAuth: true},
		{Name: "super-admin", CN: "super-admin", O: []string{"system:masters"}, DNS: []string{"localhost"}, ClientAuth: true},
		{Name: "controller-manager", CN: "system:kube-controller-manager", DNS: []string{"localhost"}, ClientAuth: true},
		{Name: "scheduler", CN: "system:kube-scheduler", DNS: []string{"localhost"}, ClientAuth: true},
		{Name: "kubelet", CN: "system:node:" + host, O: []string{"system:nodes"}, DNS: []string{host}, IPs: nodeIPs, ServerAuth: true, ClientAuth: true},
		// No kube-proxy identity: Cilium's kube-proxy replacement is the only dataplane.
		{Name: "kube-vip", CN: "system:kube-vip", DNS: []string{"localhost"}, ClientAuth: true},
	}
}

// Issued by etcd-ca, so a root-signed kubelet cert no longer authenticates to etcd.
var etcdLeafNames = map[string]bool{"etcd": true, "apiserver-etcd-client": true}

// Callers that need every leaf regardless of signer keep using LeafSpecs.
func SplitLeafSpecs(host, nodeIP string, sans *APISANs) (root, etcd []CertSpec) {
	for _, sp := range LeafSpecs(host, nodeIP, sans) {
		if etcdLeafNames[sp.Name] {
			etcd = append(etcd, sp)
			continue
		}
		root = append(root, sp)
	}
	return root, etcd
}

// Deliberately excludes the CAs and the SA keypair: they are not leaf-rotatable.
func LeafNames(host, nodeIP string) []string {
	names := make([]string, 0, 10)
	for _, s := range LeafSpecs(host, nodeIP, nil) {
		names = append(names, s.Name)
	}
	return append(names, frontProxyClientSpec.Name)
}

func GenerateAll(host, nodeIP string, sans *APISANs) error {
	return GenerateAllWithWriter(os.Stdout, host, nodeIP, sans)
}

func GenerateAllWithWriter(w io.Writer, host, nodeIP string, sans *APISANs) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	ca, err := EnsureCA("ca", "stratum-ca")
	if err != nil {
		return fmt.Errorf("root CA: %w", err)
	}
	fpca, err := EnsureCA("front-proxy-ca", "front-proxy-ca")
	if err != nil {
		return fmt.Errorf("front-proxy CA: %w", err)
	}
	etcdCA, err := EnsureCA(EtcdCAName, etcdCACommonName)
	if err != nil {
		return fmt.Errorf("etcd CA: %w", err)
	}
	rootLeaves, etcdLeaves := SplitLeafSpecs(host, nodeIP, sans)
	if !bundleExists("ca") {
		if err := WriteCABundle("ca", "ca"); err != nil {
			return fmt.Errorf("ca bundle: %w", err)
		}
	}
	if err := ensureKubeletClientTrust(); err != nil {
		return fmt.Errorf("kubelet client-CA bundle: %w", err)
	}
	kubeletClientCA, err := kubeletClientSigner(ca)
	if err != nil {
		return err
	}
	for _, l := range rootLeaves {
		signer := ca
		if l.Name == "apiserver-kubelet-client" {
			signer = kubeletClientCA
		}
		if err := signer.issueWithWriter(w, l, false); err != nil {
			return fmt.Errorf("issue %s: %w", l.Name, err)
		}
	}
	// The etcd bundle decides who signs the etcd leaves, so it must exist first.
	if err := EnsureEtcdTrustBundle(); err != nil {
		return fmt.Errorf("etcd-ca bundle: %w", err)
	}
	// A joiner minting etcd-ca leaves while the cluster is root-signed is a unilateral issue-new.
	etcdSigner := ca
	if ReadEtcdTrust().EtcdSignerName() == EtcdCAName {
		etcdSigner = etcdCA
	}
	for _, l := range etcdLeaves {
		if err := etcdSigner.issueWithWriter(w, l, false); err != nil {
			return fmt.Errorf("issue %s: %w", l.Name, err)
		}
	}
	if err := fpca.issueWithWriter(w, frontProxyClientSpec, false); err != nil {
		return fmt.Errorf("issue front-proxy-client: %w", err)
	}
	if err := EnsureSAKeypair(); err != nil {
		return fmt.Errorf("sa keypair: %w", err)
	}
	// Bundles are what components trust (--*-ca-file flags); rotation makes them old‖new.
	if !bundleExists("front-proxy-ca") {
		if err := WriteCABundle("front-proxy-ca", "front-proxy-ca"); err != nil {
			return fmt.Errorf("front-proxy-ca bundle: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(version.PKIDir, "sa-bundle.pub")); err != nil {
		if err := WriteSABundle("sa"); err != nil {
			return fmt.Errorf("sa bundle: %w", err)
		}
	}
	return nil
}

// Reissues leaves from the existing CAs; the caller must regenerate kubeconfigs.
func RotateLeaves(host, nodeIP string, sans *APISANs) error {
	if !exists("ca") || !exists("front-proxy-ca") {
		return fmt.Errorf("pki: cannot rotate — CA not present at %s (run `stratum up` first)", version.PKIDir)
	}
	ca, err := loadCA("ca")
	if err != nil {
		return fmt.Errorf("load root CA: %w", err)
	}
	fpca, err := loadCA("front-proxy-ca")
	if err != nil {
		return fmt.Errorf("load front-proxy CA: %w", err)
	}
	// A leaf rotation must never move a trust anchor.
	etcdSigner, err := etcdLeafSigner(ca)
	if err != nil {
		return err
	}
	kubeletClientCA, err := kubeletClientReissuer(ca)
	if err != nil {
		return err
	}
	rootLeaves, etcdLeaves := SplitLeafSpecs(host, nodeIP, sans)
	for _, l := range rootLeaves {
		signer := ca
		if l.Name == "apiserver-kubelet-client" {
			signer = kubeletClientCA
		}
		if err := signer.Reissue(l); err != nil {
			return fmt.Errorf("reissue %s: %w", l.Name, err)
		}
	}
	for _, l := range etcdLeaves {
		if err := etcdSigner.Reissue(l); err != nil {
			return fmt.Errorf("reissue %s: %w", l.Name, err)
		}
	}
	if err := fpca.Reissue(frontProxyClientSpec); err != nil {
		return fmt.Errorf("reissue front-proxy-client: %w", err)
	}
	return nil
}

// Reads the live leaves, not the bundle: mid-migration the bundle still carries a root.
func etcdLeafSigner(root *CA) (*CA, error) {
	// A renewal must not change a leaf's trust domain; only issue-new moves it.
	if !exists(EtcdCAName) {
		return root, nil
	}
	anyPresent, allUnderEtcdCA := false, true
	for name := range etcdLeafNames {
		ok, err := leafChainsTo(name, EtcdCAName)
		if errors.Is(err, os.ErrNotExist) {
			continue // nothing to preserve for this one
		}
		if err != nil {
			// Reading a broken etcd.crt as "nothing to preserve" would let the renew timer promote it.
			return nil, fmt.Errorf("pki: cannot determine the current signer of %s: %w", name, err)
		}
		anyPresent = true
		if !ok {
			allUnderEtcdCA = false
		}
	}
	if !anyPresent || !allUnderEtcdCA {

		return root, nil
	}
	etcdCA, err := loadCA(EtcdCAName)
	if err != nil {
		return nil, fmt.Errorf("load etcd CA: %w", err)
	}
	return etcdCA, nil
}

// Phases: stage and widen the bundles to old‖new, promote, then collapse to new.

func bundleCrtPath(base string) string { return filepath.Join(version.PKIDir, base+"-bundle.crt") }
func saBundlePath() string             { return filepath.Join(version.PKIDir, "sa-bundle.pub") }

func bundleExists(base string) bool {
	_, err := os.Stat(bundleCrtPath(base))
	return err == nil
}

// Atomic: a trust bundle cut in half by a crash stops the components that read it.
func concatFiles(dst string, mode os.FileMode, src ...string) error {
	var buf []byte
	for _, s := range src {
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if len(buf) > 0 && buf[len(buf)-1] != '\n' {
			buf = append(buf, '\n')
		}
		buf = append(buf, b...)
	}
	return atomicfile.WriteFile(dst, buf, mode)
}

// Concatenates the named certs in order; a single name yields a one-cert bundle.
func WriteCABundle(base string, certNames ...string) error {
	src := make([]string, len(certNames))
	for i, n := range certNames {
		src[i] = path(n, "crt")
	}
	return concatFiles(bundleCrtPath(base), 0o644, src...)
}

// The apiserver verifies tokens against every key in the file, so old‖new works.
func WriteSABundle(pubNames ...string) error {
	src := make([]string, len(pubNames))
	for i, n := range pubNames {
		src[i] = filepath.Join(version.PKIDir, n+".pub")
	}
	return concatFiles(saBundlePath(), 0o644, src...)
}

// The old certificate in the bundle is what lets nodes verify the new root by it.
func RootCertPresent() bool { return certTrusted("ca") }

// No bundle means the plain cert counts as trusted; with one, a copied-in file is not the root.
func certTrusted(name string) bool {
	b, err := os.ReadFile(path(name, "crt"))
	if err != nil {
		return false
	}
	crt, _ := pem.Decode(b)
	if crt == nil || crt.Type != "CERTIFICATE" {
		return false
	}
	if _, err := x509.ParseCertificate(crt.Bytes); err != nil {
		return false
	}
	bundle, err := os.ReadFile(bundleCrtPath(name))
	if os.IsNotExist(err) {
		return true
	}
	for err == nil {
		var blk *pem.Block
		if blk, bundle = pem.Decode(bundle); blk == nil {
			return false
		}
		if bytes.Equal(blk.Bytes, crt.Bytes) {
			return true
		}
	}
	return false
}

// Base itself, or the staged pair when base's key is gone; PromoteCA/PromoteSA restore base.
func Signer(base string) string {
	if _, err := os.Stat(path(base, "key")); err == nil {
		return base
	}
	if _, err := os.Stat(path(base+"-new", "key")); err == nil {
		return base + "-new"
	}
	return base
}

var ErrNoRootCert = errors.New("cannot rotate the CA without the certificate of the root the cluster trusts: " +
	"nothing could verify the new root by it, so every component would fail TLS once trust-new restarts it. " +
	"The certificate is public: copy ca.crt from another control plane, or, when this node's ca-bundle.crt " +
	"holds a single certificate, copy that file to ca.crt, then run the rotation again")

var ErrNoOldPublicHalf = errors.New("cannot rotate the CA without the old front-proxy CA certificate and service-account public key: " +
	"the trust bundles must carry them, or what the old signers issued stops verifying at the trust-new restart, " +
	"and the rotation shares them with the other control planes. Both are public: copy the file from another control plane, " +
	"or, when this node's front-proxy-ca-bundle.crt or sa-bundle.pub holds a single entry, copy that file to front-proxy-ca.crt or sa.pub, " +
	"then run the rotation again")

func OldPublicHalvesPresent() bool {
	return certTrusted("front-proxy-ca") && (fileExists(filepath.Join(version.PKIDir, "sa.pub")) || fileExists(path("sa", "key")))
}

func restoreSAPub() error {
	pubFile := filepath.Join(version.PKIDir, "sa.pub")
	if fileExists(pubFile) {
		return nil
	}
	key, err := readPrivateKey(path("sa", "key"))
	if err != nil {
		return fmt.Errorf("pki: %s is missing and cannot be derived from sa.key (%v): %w", pubFile, err, ErrNoOldPublicHalf)
	}
	return writePublicKey(pubFile, key)
}

// The staged key came from the cluster; derive its public half rather than mint a new pair.
func stageSAKeypair() error {
	keyFile := filepath.Join(version.PKIDir, "sa-new.key")
	if !fileExists(keyFile) {
		return NewSAKeypair("sa-new")
	}
	pubFile := filepath.Join(version.PKIDir, "sa-new.pub")
	if fileExists(pubFile) {
		return nil
	}
	key, err := readPrivateKey(keyFile)
	if err != nil {
		return fmt.Errorf("%s is missing and cannot be derived from %s: %w", pubFile, keyFile, err)
	}
	return writePublicKey(pubFile, key)
}

func writePublicKey(file string, key crypto.Signer) error {
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return fmt.Errorf("pki: derive %s: %w", file, err)
	}
	return atomicfile.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
}

// Stages new material and rewrites the trust bundles as old‖new; does not promote.
func RotateCAMaterial(force bool) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	if !RootCertPresent() {
		return fmt.Errorf("pki: %s is missing, or is not a certificate in ca-bundle.crt: %w", path("ca", "crt"), ErrNoRootCert)
	}
	if !certTrusted("front-proxy-ca") {
		return fmt.Errorf("pki: %s is missing, or is not a certificate in front-proxy-ca-bundle.crt: %w", path("front-proxy-ca", "crt"), ErrNoOldPublicHalf)
	}
	if !fileExists(filepath.Join(version.PKIDir, "sa.pub")) && !fileExists(path("sa", "key")) {
		return fmt.Errorf("pki: %s and %s are both missing: %w", filepath.Join(version.PKIDir, "sa.pub"), path("sa", "key"), ErrNoOldPublicHalf)
	}
	split, err := KubeletClientSplit()
	if err != nil {
		return err
	}
	if err := refuseWhileKubeletMigrating("rotate the CA"); err != nil {
		return err
	}
	if split && !certTrusted(KubeletClientCAName) {
		return fmt.Errorf("pki: %s is missing, or is not a certificate in %s: %w", path(KubeletClientCAName, "crt"), KubeletClientBundlePath(), ErrNoOldPublicHalf)
	}
	if (!exists("ca") || !exists("front-proxy-ca") || !fileExists(path("sa", "key")) || (split && !exists(KubeletClientCAName))) && !force {
		return fmt.Errorf("pki: cannot rotate CA — existing CA/SA missing at %s (use --force to rotate without them)", version.PKIDir)
	}
	if err := PrepareStaging(force); err != nil {
		return err
	}
	if same := liveSignersStaged(); len(same) > 0 {
		return fmt.Errorf("pki: refusing to stage the CA rotation (nothing has been changed): the live %s in %s is byte-identical to its staged replacement, "+
			"so the trust bundles would carry the new signer twice and drop the old one. This node took a promoted control plane's signers as its own; "+
			"copy ca.crt, ca.key, front-proxy-ca.crt, front-proxy-ca.key, sa.key and sa.pub back from a control plane `stratum certs check` shows at trust-new, then run trust-new again",
			strings.Join(same, ", "), version.PKIDir)
	}
	if err := restoreSAPub(); err != nil {
		return err
	}
	if err := removeCAMarkers(); err != nil {
		return err
	}
	if err := setForcedMarker(force); err != nil {
		return err
	}
	if !exists("ca-new") {
		if _, err := NewCA("ca-new", "stratum-ca"); err != nil {
			return fmt.Errorf("stage root CA: %w", err)
		}
	}
	if !exists("front-proxy-ca-new") {
		if _, err := NewCA("front-proxy-ca-new", "front-proxy-ca"); err != nil {
			return fmt.Errorf("stage front-proxy CA: %w", err)
		}
	}
	if err := stageSAKeypair(); err != nil {
		return fmt.Errorf("stage sa keypair: %w", err)
	}
	if split {
		if !exists(KubeletClientCAName + "-new") {
			if _, err := NewCA(KubeletClientCAName+"-new", kubeletClientCACommonName); err != nil {
				return fmt.Errorf("stage kubelet-client CA: %w", err)
			}
		}
		if err := WriteCABundle(KubeletClientBundleBase, KubeletClientCAName+"-new", KubeletClientCAName); err != nil {
			return fmt.Errorf("kubelet client-CA bundle: %w", err)
		}
	}
	caSrc, fpSrc, saSrc := []string{"ca-new", "ca"}, []string{"front-proxy-ca-new", "front-proxy-ca"}, []string{"sa-new", "sa"}
	if err := writeRootBundle(caSrc...); err != nil {
		return fmt.Errorf("ca bundle: %w", err)
	}
	if err := WriteCABundle("front-proxy-ca", fpSrc...); err != nil {
		return fmt.Errorf("front-proxy-ca bundle: %w", err)
	}
	if err := syncEtcdBundleToRoot(caSrc...); err != nil {
		return fmt.Errorf("etcd-ca bundle: %w", err)
	}
	return WriteSABundle(saSrc...)
}

// Equal live and staged files happen only when another node's promoted signers were copied in.
func liveSignersStaged() []string {
	var same []string
	for _, name := range []string{RootCAName + ".crt", RootCAName + ".key", frontProxyCAName + ".crt", frontProxyCAName + ".key",
		saPairName + ".key", saPairName + ".pub", KubeletClientCAName + ".crt", KubeletClientCAName + ".key"} {
		live, err := os.ReadFile(filepath.Join(version.PKIDir, name))
		if err != nil {
			continue
		}
		staged, err := os.ReadFile(filepath.Join(version.PKIDir, StagedTrustName(name)))
		if err != nil {
			continue
		}
		if bytes.Equal(live, staged) {
			same = append(same, name)
		}
	}
	return same
}

// Refuses a node that has promoted and not finalized; removes obsolete backups when idle.
func PrepareStaging(force bool) error {
	if rotationPromoted() {
		way := "Finish the rotation with `stratum certs rotate-ca --phase=finalize` once every node has completed sign-new, " +
			"or abandon it with the rollback below"
		switch {
		case !CAPromoted():
			way = "This node's sign-new stopped before it recorded the promotion: run `stratum certs rotate-ca --phase=sign-new --yes` on it again to finish, " +
				"and finalize only after that. Or abandon the rotation with the rollback below"
		case len(CAStagedFiles()) > 0:
			way = "New CA material is staged on this node again, so finalize would refuse and sign-new would skip: do not finalize this node. Abandon the rotation with the rollback below"
		}
		return fmt.Errorf("pki: refusing to stage a CA rotation (nothing has been changed): this node has already promoted its new CA, and staging again would replace the staged "+
			"material and clear the promotion record, so a later sign-new would overwrite the %s backups with the new CA and a rollback could no longer restore the original. "+
			"%s: run `stratum certs rotate-ca --rollback --cluster --check` from a control plane, then `stratum certs rotate-ca --rollback --yes` on every control plane and then every worker", RootCAName+".pre-carotate.*", way)
	}
	return removeObsoleteBackups(force)
}

// Past promotion and not finalized: its backups are the only way back, so nothing stages over it.
func rotationPromoted() bool {
	return fileExists(filepath.Join(version.PKIDir, "ca.pre-carotate.crt")) && ((CAPromoted() && !CAFinalized()) || SignNewResumable())
}

// Older releases could leave backups behind; a later rollback would restore that stale CA.
func removeObsoleteBackups(force bool) error {
	rot := ReadCARotation()
	if rot.Err != nil {
		return nil
	}
	// A crash mid-staging leaves half a pair; any staged file means not idle.
	if len(CAStagedFiles()) > 0 || rot.BundleCAs != 1 || (CAPromoted() && !CAFinalized()) {
		return nil
	}
	// An unreadable live file keeps every backup and refuses, never deletes a key's only copy.
	var obsolete []string
	var errs []error
	keepAll := false
	for _, f := range carotateScratch {
		if !strings.Contains(f, ".pre-carotate.") {
			continue
		}
		bak, err := os.ReadFile(filepath.Join(version.PKIDir, f))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		live, err := os.ReadFile(filepath.Join(version.PKIDir, strings.Replace(f, ".pre-carotate", "", 1)))
		switch {
		case err != nil && force && os.IsNotExist(err):
			keepAll = true
		case err != nil:
			errs = append(errs, err)
		case bytes.Equal(bak, live):
			keepAll = true
		default:
			obsolete = append(obsolete, f)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pki: cannot tell which backups of an earlier CA rotation are obsolete, so none were removed (nothing has been changed): %w", err)
	}
	if keepAll {
		return nil
	}
	for _, f := range obsolete {
		if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pki: remove the backups of an earlier CA rotation: %w", err)
	}
	return nil
}

func promotePair(name string) error {
	for _, ext := range []string{"crt", "key"} {
		if b, err := os.ReadFile(path(name, ext)); err == nil {
			mode := os.FileMode(0o644)
			if ext == "key" {
				mode = 0o600
			}
			if err := os.WriteFile(filepath.Join(version.PKIDir, name+".pre-carotate."+ext), b, mode); err != nil {
				return err
			}
		}
		if err := os.Rename(path(name+"-new", ext), path(name, ext)); err != nil {
			return err
		}
	}
	return nil
}

// Bundles still carry old‖new, so old-signed peers keep validating; RotateLeaves must follow.
func PromoteCA() error {
	// A missing staged file after the first rename would leave the root consumed, half done.
	if err := requireStaged("ca-new.crt", "ca-new.key", "front-proxy-ca-new.crt", "front-proxy-ca-new.key", "sa-new.key", "sa-new.pub"); err != nil {
		return err
	}
	roots := []string{"ca-new"}
	if _, err := os.Stat(path("ca", "crt")); err == nil {
		roots = append(roots, "ca")
	}
	if err := EnsureKubeletClientTrusts(roots...); err != nil {
		return err
	}
	split, err := KubeletClientSplit()
	if err != nil {
		return err
	}
	if split {
		if err := requireStaged(KubeletClientCAName+"-new.crt", KubeletClientCAName+"-new.key"); err != nil {
			return err
		}
		if err := ensureKubeletClientStaged(); err != nil {
			return err
		}
	}
	if err := promotePair("ca"); err != nil {
		return fmt.Errorf("promote root CA: %w", err)
	}
	if err := promotePair("front-proxy-ca"); err != nil {
		return fmt.Errorf("promote front-proxy CA: %w", err)
	}
	if split {
		if err := promotePair(KubeletClientCAName); err != nil {
			return fmt.Errorf("promote kubelet-client CA: %w", err)
		}
	}
	return nil
}

func requireStaged(files ...string) error {
	var missing []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(version.PKIDir, f)); err != nil {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("pki: staged CA material is incomplete (missing %s) — nothing was promoted; run `stratum certs rotate-ca --rollback --yes` and rotate again", strings.Join(missing, ", "))
	}
	return nil
}

// sa-bundle.pub still holds both keys, so old-signed tokens verify until finalize.
func PromoteSA() error {
	if err := requireStaged("sa-new.key", "sa-new.pub"); err != nil {
		return err
	}
	for _, ext := range []string{"key", "pub"} {
		cur := filepath.Join(version.PKIDir, "sa."+ext)
		if b, err := os.ReadFile(cur); err == nil {
			mode := os.FileMode(0o644)
			if ext == "key" {
				mode = 0o600
			}
			if err := os.WriteFile(filepath.Join(version.PKIDir, "sa.pre-carotate."+ext), b, mode); err != nil {
				return err
			}
		}
		if err := os.Rename(filepath.Join(version.PKIDir, "sa-new."+ext), cur); err != nil {
			return fmt.Errorf("promote sa %s: %w", ext, err)
		}
	}
	return nil
}

// Every rotation-only file, removed on finalize and rollback.
var carotateScratch = []string{
	"ca-new.crt", "ca-new.key", "front-proxy-ca-new.crt", "front-proxy-ca-new.key",
	"sa-new.key", "sa-new.pub",
	KubeletClientCAName + "-new.crt", KubeletClientCAName + "-new.key",
	KubeletClientCAName + ".pre-carotate.crt", KubeletClientCAName + ".pre-carotate.key",
	caPromotedMarker,
	"ca.pre-carotate.crt", "ca.pre-carotate.key",
	"front-proxy-ca.pre-carotate.crt", "front-proxy-ca.pre-carotate.key",
	"sa.pre-carotate.key", "sa.pre-carotate.pub",
	caForcedMarker,
}

// The only proof finalize accepts that this node promoted; the backups are not.
const caPromotedMarker = "carotate-promoted"

// Call only after the promotion and the leaf reissue under it succeeded.
func MarkCAPromoted() error {
	if err := os.WriteFile(filepath.Join(version.PKIDir, caPromotedMarker), nil, 0o600); err != nil {
		return fmt.Errorf("record promotion: %w", err)
	}
	return nil
}

func CAPromoted() bool {
	_, err := os.Stat(filepath.Join(version.PKIDir, caPromotedMarker))
	return err == nil
}

// A stale carotate-finalized left behind would let a later finalize pass unchecked.
func removeCAMarkers() error {
	for _, m := range []string{caFinalizedMarker, caPromotedMarker} {
		if err := os.Remove(filepath.Join(version.PKIDir, m)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", m, err)
		}
	}
	return nil
}

// Tells a finalize retry from a node that never ran sign-new: both look idle on disk.
const caFinalizedMarker = "carotate-finalized"

func CAFinalized() bool {
	_, err := os.Stat(filepath.Join(version.PKIDir, caFinalizedMarker))
	return err == nil
}

// A crash mid-staging leaves a crt without its key, which CARotationStaged misses.
func CAStagedFiles() []string {
	var out []string
	for _, f := range carotateScratch {
		if !strings.Contains(f, "-new.") {
			continue
		}
		if _, err := os.Stat(filepath.Join(version.PKIDir, f)); err == nil {
			out = append(out, f)
		}
	}
	return out
}

func StagedSignersMissing() ([]string, error) {
	split, err := KubeletClientSplit()
	if err != nil {
		return nil, err
	}
	want := []string{"ca-new.crt", "ca-new.key", "front-proxy-ca-new.crt", "front-proxy-ca-new.key", "sa-new.key"}
	if split {
		want = append(want, KubeletClientCAName+"-new.crt", KubeletClientCAName+"-new.key")
	}
	var missing []string
	for _, f := range want {
		if !fileExists(filepath.Join(version.PKIDir, f)) {
			missing = append(missing, f)
		}
	}
	return missing, nil
}

// A surviving backup or marker would outlive the rotation.
func removeScratch() error {
	var errs []error
	for _, f := range carotateScratch {
		if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Safe only once every node trusts and uses the new material.
func FinalizeCA() error {
	split, err := KubeletClientSplit()
	if err != nil {
		return err
	}
	if err := writeRootBundle("ca"); err != nil {
		return err
	}
	if split {
		if err := WriteCABundle(KubeletClientBundleBase, KubeletClientCAName); err != nil {
			return err
		}
	}
	if err := WriteCABundle("front-proxy-ca", "front-proxy-ca"); err != nil {
		return err
	}
	if err := syncEtcdBundleToRoot("ca"); err != nil {
		return err
	}
	if err := WriteSABundle("sa"); err != nil {
		return err
	}
	// A failure in between leaves carotate-promoted, so a retry is accepted either way.
	if err := os.WriteFile(filepath.Join(version.PKIDir, caFinalizedMarker), nil, 0o600); err != nil {
		return fmt.Errorf("record finalize: %w", err)
	}
	if err := removeScratch(); err != nil {
		return fmt.Errorf("finalize: remove rotation scratch: %w", err)
	}
	return nil
}

type restoreFile struct {
	name, ext string
}

// Restores the old material; the caller reissues leaves once sign-new was reached, then cleans up.
func RollbackCA() error {
	if err := RollbackCheck(); err != nil {
		return err
	}
	split, err := KubeletClientSplit()
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	// Marker removal first, so an unremovable marker fails before anything changes.
	if err := removeCAMarkers(); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	// A missing backup means sign-new was not reached for this file; other errors are fatal.
	restore := func(name, ext string) error {
		bak := filepath.Join(version.PKIDir, name+".pre-carotate."+ext)
		b, err := os.ReadFile(bak)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("restore %s.%s: %w", name, ext, err)
		}
		mode := os.FileMode(0o644)
		if ext == "key" {
			mode = 0o600
		}
		if err := atomicfile.WriteFileWith(path(name, ext), b, mode, writeSeams); err != nil {
			return fmt.Errorf("restore %s.%s: %w", name, ext, err)
		}
		return nil
	}
	for _, pair := range []struct{ first, second restoreFile }{
		{restoreFile{"ca", "crt"}, restoreFile{"ca", "key"}},
		{restoreFile{"front-proxy-ca", "crt"}, restoreFile{"front-proxy-ca", "key"}},
		{restoreFile{"sa", "key"}, restoreFile{"sa", "pub"}},
		{restoreFile{KubeletClientCAName, "crt"}, restoreFile{KubeletClientCAName, "key"}},
	} {
		if err := writePair(
			func() error { return restore(pair.first.name, pair.first.ext) },
			func() error { return restore(pair.second.name, pair.second.ext) },
		); err != nil {
			return fmt.Errorf("rollback: %w", err)
		}
	}
	if err := writeRootBundle("ca"); err != nil {
		return err
	}
	if split {
		if err := WriteCABundle(KubeletClientBundleBase, KubeletClientCAName); err != nil {
			return err
		}
	}
	if err := WriteCABundle("front-proxy-ca", "front-proxy-ca"); err != nil {
		return err
	}
	// Without this the etcd bundle keeps trusting the dropped new root, and nothing breaks.
	if err := syncEtcdBundleToRoot("ca"); err != nil {
		return err
	}
	return WriteSABundle("sa")
}

var ErrRollbackUnsafe = errors.New("rolling back would not leave this node a working old signer. " +
	"Do not roll back any node of this rotation; finish it forward")

// sign-new is safe only once every node trusts the new CA, which one node cannot see.
const signNewForward = "first run `stratum certs rotate-ca --rollback --cluster --check` from a control plane: no node may sign with the new CA while another has not completed trust-new, and the check says what to do about one that has not. " +
	"Then run `stratum certs rotate-ca --phase=sign-new --yes` on each control plane and then each worker where it has not completed, " +
	"then finish with \"Finishing a rotation by hand\" in the certificate-rotation documentation"

// Rolling back any node of a forced rotation, even one that kept its keys, splits the cluster.
const caForcedMarker = "carotate-forced"

func setForcedMarker(force bool) error {
	m := filepath.Join(version.PKIDir, caForcedMarker)
	if force {
		if err := os.WriteFile(m, nil, 0o600); err != nil {
			return fmt.Errorf("record a forced rotation: %w", err)
		}
		return nil
	}
	if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", caForcedMarker, err)
	}
	return nil
}

func ForcedRotation() bool { return fileExists(filepath.Join(version.PKIDir, caForcedMarker)) }

// All of RollbackCA's refusal logic, read-only; --check answers exactly what --yes there would do.
func RollbackCheck() error {
	if ForcedRotation() {
		return fmt.Errorf("pki: refusing to roll back (nothing has been changed): this rotation was started with --force: %w. "+
			"If `stratum certs rotate-ca --cluster` halted before sign-new, run `stratum certs rotate-ca --cluster --force --yes` again "+
			"from the control plane that started it, with the --restart-workloads or --skip-workload-check the first run had: it resumes the staged rotation. Otherwise %s", ErrRollbackUnsafe, signNewForward)
	}
	if bad := rollbackMismatchedPairs(); len(bad) > 0 {
		return fmt.Errorf("pki: refusing to roll back (nothing has been changed): the restored %s would not be a matching certificate and key: %w. To finish it, %s",
			strings.Join(bad, ", "), ErrRollbackUnsafe, signNewForward)
	}
	// The check must report this read too, or a cluster-wide check would clear this node.
	if _, err := KubeletClientSplit(); err != nil {
		return fmt.Errorf("pki: refusing to roll back (nothing has been changed): the kubelet client-CA trust cannot be read, so the rollback could not tell which bundle to restore: %w: %w. "+
			"Repair or remove %s, then run the check again", err, ErrRollbackUnsafe, KubeletClientBundlePath())
	}
	if t := readEtcdTrust(rollbackFile("ca", "crt")); t.Err != nil {
		return fmt.Errorf("pki: refusing to roll back (nothing has been changed): etcd's trust cannot be read against the root the rollback restores, so the rollback could not re-derive its bundle: %w: %w. "+
			"Repair %s, then run the check again", t.Err, ErrRollbackUnsafe, bundleCrtPath(EtcdBundleBase))
	}
	return nil
}

func RollbackUnsafePairs() []string { return rollbackMismatchedPairs() }

var rollbackPairs = []struct{ name, public string }{
	{"ca", "crt"}, {"front-proxy-ca", "crt"}, {"sa", "pub"}, {KubeletClientCAName, "crt"},
}

func rollbackFile(name, ext string) string {
	if bak := filepath.Join(version.PKIDir, name+".pre-carotate."+ext); fileExists(bak) {
		return bak
	}
	return path(name, ext)
}

func rollbackMismatchedPairs() []string {
	var bad []string
	for _, p := range rollbackPairs {
		if p.name == KubeletClientCAName && !fileExists(path(p.name, "crt")) {
			continue
		}
		if !keyMatches(rollbackFile(p.name, p.public), rollbackFile(p.name, "key")) {
			bad = append(bad, p.name)
		}
	}
	return bad
}

// Lets the cluster-wide check confirm every control plane returns to the same signers.
func RollbackTarget() string {
	var parts []string
	for _, p := range rollbackPairs {
		if p.name == KubeletClientCAName && !fileExists(path(p.name, "crt")) {
			continue
		}
		fp := "unreadable"
		if pub, err := readPublicKey(rollbackFile(p.name, p.public)); err == nil {
			if der, err := x509.MarshalPKIXPublicKey(pub); err == nil {
				sum := sha256.Sum256(der)
				fp = hex.EncodeToString(sum[:8])
			}
		}
		parts = append(parts, p.name+"="+fp)
	}
	return strings.Join(parts, ",")
}

func keyMatches(pubFile, keyFile string) bool {
	pub, err := readPublicKey(pubFile)
	if err != nil {
		return false
	}
	key, err := readPrivateKey(keyFile)
	if err != nil {
		return false
	}
	eq, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	return ok && eq.Equal(pub)
}

func readPublicKey(file string) (crypto.PublicKey, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", file)
	}
	switch blk.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		return cert.PublicKey, nil
	case "PUBLIC KEY":
		return x509.ParsePKIXPublicKey(blk.Bytes)
	}
	return nil, fmt.Errorf("%s: unexpected PEM block %q", file, blk.Type)
}

func readPrivateKey(file string) (crypto.Signer, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", file)
	}
	switch blk.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(blk.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(blk.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%s: unexpected PEM block %q", file, blk.Type)
}

func SignNewResumable() bool {
	if CAPromoted() || fileExists(path("ca-new", "crt")) || fileExists(path("ca-new", "key")) {
		return false
	}
	if !fileExists(filepath.Join(version.PKIDir, "ca.pre-carotate.crt")) {
		return false
	}
	n, err := CABundleCertCount("ca")
	return err == nil && n > 1
}

func FinishPromotion() error {
	if exists("front-proxy-ca-new") {
		if err := promotePair("front-proxy-ca"); err != nil {
			return fmt.Errorf("promote front-proxy CA: %w", err)
		}
	}
	if fileExists(path("sa-new", "key")) && fileExists(filepath.Join(version.PKIDir, "sa-new.pub")) {
		return PromoteSA()
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Separate from RollbackCA: the caller needs the backups to tell whether to reissue leaves.
func RemoveRotationScratch() error {
	if err := removeScratch(); err != nil {
		return fmt.Errorf("rollback: remove rotation scratch: %w", err)
	}
	return nil
}

func CARotationStaged() bool { return exists("ca-new") }

// Only pki spells the rotation file names; every rotation-state consumer reads this struct.
type CARotation struct {
	Staged   bool
	Promoted bool
	// Two while a rotation has widened trust to old‖new, one at rest.
	BundleCAs int
	// The staged root while its certificate is on disk, else the live root.
	Anchor string
	// Only os.ErrNotExist is a benign read; Staged, Promoted and Anchor stay meaningful.
	Err error
}

// A bundle read or parse failure sets Err, never a plausible-looking idle state.
func ReadCARotation() CARotation {
	rot := CARotation{
		Staged:   CARotationStaged(),
		Promoted: fileExists(filepath.Join(version.PKIDir, "ca.pre-carotate.crt")),
		Anchor:   "ca",
	}
	if fileExists(path("ca-new", "crt")) {
		rot.Anchor = "ca-new"
	}
	rot.BundleCAs, rot.Err = CABundleCertCount("ca")
	return rot
}

// Three readers consume trust bundles; their loaders differ, so the policies here declare it.

type BundleLeading int

const (
	LeadingTrim BundleLeading = iota
	// pem.Decode honours a BEGIN line only at offset 0 or after a newline, as the kubelet does.
	LeadingNewlineEnded
	// Matches x509.CertPool.AppendCertsFromPEM, which etcd loads --trusted-ca-file through.
	LeadingPemSkip
)

// Loaders differ: the kubelet's parser drops blocks the count and etcd readers accept.
type BundlePolicy struct {
	Leading BundleLeading
	// The kubelet's parser drops headered blocks outright.
	RefusePEMHeaders bool
	// The etcd pool skips them; the count and the kubelet refuse.
	SkipNonCertificateBlocks bool
	// The etcd pool never checks IsCA; the count and the kubelet refuse a non-CA.
	AcceptNonCA bool
	// A zero count must not authorize a rotation, and an empty clientCAFile trusts nothing.
	RefuseEmpty bool
}

// Refuses anything it cannot count completely; PEM headers are kept.
var CountPolicy = BundlePolicy{Leading: LeadingTrim, RefuseEmpty: true}

// The broadest: refusing material the etcd pool accepts would underreport etcd's trust.
var EtcdPolicy = BundlePolicy{
	Leading:                  LeadingPemSkip,
	SkipNonCertificateBlocks: true,
	AcceptNonCA:              true,
}

// The bytes go verbatim into the kubelet's clientCAFile; refuse what it would skip.
var KubeletPolicy = BundlePolicy{
	Leading:          LeadingNewlineEnded,
	RefusePEMHeaders: true,
	RefuseEmpty:      true,
}

// The one PEM decoder for trust bundles; readers differ only by declared policy.
func DecodePEMBundle(b []byte, pol BundlePolicy) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for i := 0; ; i++ {
		if pol.Leading != LeadingPemSkip {
			// pem.Decode silently skips anything undecodable, so leading bytes are validated.
			trimmed := bytes.TrimLeft(b, " \t\r\n")
			// Whitespace ending the input precedes no block, so the check below does not refuse it.
			if len(trimmed) == 0 {
				break
			}
			if pol.Leading == LeadingNewlineEnded {
				// Trimming a run ending in space, tab or a lone CR would validate a block the kubelet skips.
				if skipped := b[:len(b)-len(trimmed)]; len(skipped) > 0 && skipped[len(skipped)-1] != '\n' {
					return nil, fmt.Errorf("%q immediately before a PEM block, which the kubelet's PEM parser skips over; the bundle is written out verbatim, so it must be PEM end to end", skipped[len(skipped)-1:])
				}
			}
			b = trimmed
			if !bytes.HasPrefix(b, []byte("-----BEGIN ")) {
				return nil, fmt.Errorf("%d bytes that are not a PEM block", len(b))
			}
		}
		blk, rest := pem.Decode(b)
		if blk == nil {
			if pol.Leading != LeadingPemSkip {
				return nil, fmt.Errorf("a PEM block that does not decode")
			}
			// Non-whitespace left behind is trailing garbage, not an empty answer.
			if len(bytes.TrimSpace(b)) > 0 {
				return nil, fmt.Errorf("trailing garbage after block %d", i)
			}
			break
		}
		if pol.Leading != LeadingPemSkip {
			// pem.Decode skips forward to the next BEGIN line when one does not decode.
			if n := bytes.Count(b[:len(b)-len(rest)], []byte("-----BEGIN ")); n != 1 {
				return nil, fmt.Errorf("%d BEGIN lines in one block: bytes between PEM blocks that pem.Decode skips over; the bundle is written out verbatim, so it must be PEM end to end", n)
			}
		}
		if blk.Type != "CERTIFICATE" {
			if pol.SkipNonCertificateBlocks {
				b = rest
				continue
			}
			return nil, fmt.Errorf("a %q PEM block; a CA trust bundle may contain CERTIFICATE blocks only", blk.Type)
		}
		if pol.RefusePEMHeaders && len(blk.Headers) != 0 {
			return nil, fmt.Errorf("a CERTIFICATE block carrying PEM headers, which the kubelet's PEM parser skips; this would report success while the kubelet silently trusted fewer anchors than the bundle lists, or none if it is the only one")
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("block %d holds an unparseable certificate: %w", i, err)
		}
		if !pol.AcceptNonCA && !cert.IsCA {
			return nil, fmt.Errorf("a non-CA certificate (subject %q); every entry becomes a trust anchor", cert.Subject)
		}
		out = append(out, cert)
		b = rest
	}
	if pol.RefuseEmpty && len(out) == 0 {
		return nil, fmt.Errorf("the bundle holds no certificate")
	}
	return out, nil
}

// An incomplete count must not authorize a rotation decision.
func CABundleCertCount(base string) (int, error) {
	b, err := os.ReadFile(bundleCrtPath(base))
	if err != nil {
		return 0, err
	}
	certs, err := DecodePEMBundle(b, CountPolicy)
	if err != nil {
		return 0, fmt.Errorf("pki: malformed CA bundle %s: %w", bundleCrtPath(base), err)
	}
	return len(certs), nil
}

func CertNotAfter(name string) (time.Time, error) {
	pemBytes, err := os.ReadFile(path(name, "crt"))
	if err != nil {
		return time.Time{}, err
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return time.Time{}, fmt.Errorf("pki: bad PEM for %s", name)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

type TrustFileClass int

const (
	// Held byte-identically by every control plane; travels in the join bundle.
	TrustShared TrustFileClass = iota
	// Absent at rest, so an upload skips it instead of failing.
	TrustRotation
	// Never leaves the node.
	TrustNodeLocal
)

type TrustFile struct {
	Name       string
	CommonName string
	Class      TrustFileClass
	Rotatable  bool
	// A staged file's own StagedAs is empty: it IS the rotation's material.
	StagedAs string
}

const RootCAName = "ca"

const RootTrustBundleName = RootCAName + "-bundle.crt"

func RootTrustBundlePath() string { return bundleCrtPath(RootCAName) }

const (
	frontProxyCAName     = "front-proxy-ca"
	saPairName           = "sa"
	frontProxyClientName = "front-proxy-client"
)

// Manifest order is a cross-version contract pinned by ha's join-bundle tests.
var trustLayout = []TrustFile{
	// A joining control plane without the etcd CA mints a divergent anchor the cluster rejects.
	{Name: RootCAName + ".crt", CommonName: "stratum-ca", Class: TrustShared, Rotatable: true, StagedAs: StagedRootCAName + ".crt"},
	{Name: RootCAName + ".key", CommonName: "stratum-ca", Class: TrustShared, Rotatable: true, StagedAs: StagedRootCAName + ".key"},
	{Name: saPairName + ".key", Class: TrustShared, Rotatable: true, StagedAs: saPairName + "-new.key"},
	{Name: saPairName + ".pub", Class: TrustShared, Rotatable: true, StagedAs: saPairName + "-new.pub"},
	{Name: frontProxyCAName + ".crt", CommonName: frontProxyCAName, Class: TrustShared, Rotatable: true, StagedAs: frontProxyCAName + "-new.crt"},
	{Name: frontProxyCAName + ".key", CommonName: frontProxyCAName, Class: TrustShared, Rotatable: true, StagedAs: frontProxyCAName + "-new.key"},
	{Name: frontProxyClientName + ".crt", CommonName: frontProxyClientName, Class: TrustShared, Rotatable: true},
	{Name: frontProxyClientName + ".key", CommonName: frontProxyClientName, Class: TrustShared, Rotatable: true},
	{Name: EtcdCAName + ".crt", CommonName: etcdCACommonName, Class: TrustShared},
	{Name: EtcdCAName + ".key", CommonName: etcdCACommonName, Class: TrustShared},
	// Received by a joiner whenever present, so a mid-rotation joiner holds the same state.
	{Name: RootTrustBundleName, CommonName: "stratum-ca", Class: TrustRotation, Rotatable: true},
	{Name: frontProxyCAName + "-bundle.crt", CommonName: frontProxyCAName, Class: TrustRotation, Rotatable: true},
	{Name: saPairName + "-bundle.pub", Class: TrustRotation, Rotatable: true},
	// Carries the cluster's current etcd migration state, not this node's guess.
	{Name: EtcdBundleBase + "-bundle.crt", CommonName: etcdCACommonName, Class: TrustRotation, Rotatable: true},
	{Name: KubeletClientBundleBase + "-bundle.crt", Class: TrustRotation, Rotatable: true},
	{Name: KubeletClientCAName + ".crt", Class: TrustRotation, Rotatable: true, StagedAs: KubeletClientCAName + "-new.crt"},
	{Name: KubeletClientCAName + ".key", Class: TrustRotation, Rotatable: true, StagedAs: KubeletClientCAName + "-new.key"},
	{Name: KubeletClientCAName + "-new.crt", Class: TrustRotation},
	{Name: KubeletClientCAName + "-new.key", Class: TrustRotation},
	{Name: RootCAName + "-new.crt", CommonName: "stratum-ca", Class: TrustRotation},
	{Name: RootCAName + "-new.key", Class: TrustRotation},
	{Name: frontProxyCAName + "-new.crt", CommonName: frontProxyCAName, Class: TrustRotation},
	{Name: frontProxyCAName + "-new.key", Class: TrustRotation},
	{Name: saPairName + "-new.key", Class: TrustRotation},
	{Name: saPairName + "-new.pub", Class: TrustRotation},
	{Name: RootCAName + ".pre-carotate.crt", CommonName: "stratum-ca", Class: TrustNodeLocal},
	{Name: RootCAName + ".pre-carotate.key", Class: TrustNodeLocal},
	{Name: frontProxyCAName + ".pre-carotate.crt", CommonName: frontProxyCAName, Class: TrustNodeLocal},
	{Name: frontProxyCAName + ".pre-carotate.key", Class: TrustNodeLocal},
	{Name: saPairName + ".pre-carotate.key", Class: TrustNodeLocal},
	{Name: saPairName + ".pre-carotate.pub", Class: TrustNodeLocal},
	{Name: caPromotedMarker, Class: TrustNodeLocal},
	{Name: caFinalizedMarker, Class: TrustNodeLocal},
	{Name: caForcedMarker, Class: TrustNodeLocal},
}

// An unclassified file is invisible to every consumer that derives from the manifest.
func TrustLayout() []TrustFile { return slices.Clone(trustLayout) }

func StagedTrustName(name string) string {
	for _, f := range trustLayout {
		if f.Name == name {
			return f.StagedAs
		}
	}
	return ""
}

const StagedRootCAName = RootCAName + "-new"

func PreRotationBackupName(name string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + ".pre-carotate" + ext
}

func ReadRootTrust() ([]byte, string, error) { return ReadRootTrustFrom(version.PKIDir) }

// Only an unreadable bundle falls back; consumers refuse malformed material themselves.
func ReadRootTrustFrom(dir string) ([]byte, string, error) {
	bundle := filepath.Join(dir, RootTrustBundleName)
	if b, err := os.ReadFile(bundle); err == nil {
		return b, bundle, nil
	}
	caCert := filepath.Join(dir, RootCAName+".crt")
	b, err := os.ReadFile(caCert)
	if err != nil {
		return nil, "", fmt.Errorf("pki: read %s, falling back to %s: %w", bundle, caCert, err)
	}
	return b, caCert, nil
}
