// Package pki generates the cluster PKI (root CA, front-proxy CA, leaf certs,
// and the service-account keypair) entirely in Go (crypto/x509), with no openssl
// dependency. Output is written to
// version.PKIDir with 0600 keys; every step is idempotent (skip if present).
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
	// CAValidity is the lifetime of the root and front-proxy CAs, and the
	// controller-manager's CSR signing duration, set explicitly so the certs it
	// signs (workers' kubelet client certs) do not expire after the one-year
	// default.
	CAValidity = 10 * 365 * 24 * time.Hour
)

// LeafValidity is the lifetime of issued leaf certs. Short (~1yr) so
// they rotate routinely via `stratum certs rotate` + the renew timer, rather than
// the old write-once 10-year certs. A var (not const) so tests can shorten it
// and a future --cert-validity flag can override it.
var LeafValidity = 365 * 24 * time.Hour

// CA holds a signing certificate + key.
type CA struct {
	Cert *x509.Certificate
	Key  *rsa.PrivateKey
}

// CertSpec describes a leaf certificate to issue.
type CertSpec struct {
	Name       string   // file basename (e.g. "apiserver" -> apiserver.crt/.key)
	CN         string   // Subject CommonName
	O          []string // Subject Organization(s) (groups)
	DNS        []string // DNS SANs
	IPs        []string // IP SANs
	ServerAuth bool
	ClientAuth bool
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

// EnsureDir creates the PKI dir (0700). MkdirAll does not tighten a
// pre-existing dir (host.Dirs may have run first), so the mode is enforced
// with an explicit Chmod.
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

// EtcdCAExists reports whether the etcd CA certificate and key are on disk,
// the same state etcdLeafSigner branches on: once the CA exists a rotation
// reads the live etcd leaves before it picks their signer.
func EtcdCAExists() bool { return exists(EtcdCAName) }

// writeSeams is the seam set the pki writers hand the atomic single-file
// writer. Production leaves it zero — every step is then the writer's own —
// and a test fills it to inject a failure into the real writer: a directory
// sync that never completes, the window a power loss leaves behind.
var writeSeams atomicfile.Seams

func writeCertPEM(name string, der []byte) error {
	// Replaces, never truncates: a component holding the old certificate open
	// must keep reading the old bytes while the reissued one lands.
	return atomicfile.WriteFileWith(path(name, "crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644, writeSeams)
}

func writeKeyPEM(name string, key *rsa.PrivateKey) error {
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return host.WriteFileAtomically(path(name, "key"), b, 0o600)
}

// writePair writes a credential pair — a certificate and its key, or a
// signing key and its public half — as two atomic single-file writes. When
// the first write reports ErrNotDurable the new file is already in place and
// only its durability failed; stopping there strands a mismatched or
// incomplete pair that the next start refuses to repair: EnsureCA refuses a
// partial CA pair, Issue skips a leaf whose SANs already match, and
// EnsureSAKeypair skips whenever the key exists. The second half is written
// anyway, from the same in-memory material, so the pair on disk ends up
// complete and matching, and the durability error still goes out — a power
// loss can still bring the replaced half's old bytes back.
func writePair(first, second func() error) error {
	firstErr := first()
	if errors.Is(firstErr, atomicfile.ErrNotDurable) {
		// The first half is in place; write the second anyway and report
		// both, so the pair on disk is complete and matching.
		return errors.Join(firstErr, second())
	}
	if firstErr != nil {
		return firstErr
	}
	return second()
}

// EnsureCA loads an existing CA or creates one only when neither file exists.
// A partial pair may be the only recovery material for a cluster; never
// overwrite its certificate or key while trying to repair it implicitly.
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

// NewCA always generates a fresh self-signed CA and writes it to {name}.crt/.key,
// overwriting any existing files. Used to stage a rotation CA (e.g. "ca-new")
// without disturbing the live signer. EnsureCA wraps this with skip-if-exists.
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

// Constraints limit the names a CA may sign for, as critical X.509 name
// constraints, and stop it from signing other CAs.
type Constraints struct {
	DNS []string
	IPs []*net.IPNet
}

// GenerateCA makes a fresh self-signed CA in memory, for a CA whose home is not
// PKIDir. A nil c leaves it unconstrained.
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

// PEM encodes the CA's certificate and key.
func (ca *CA) PEM() (certPEM, keyPEM []byte) {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ca.Key)})
}

// ParseCA reads a CA from the PEM that PEM writes.
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

// Issue signs a leaf cert per spec with the given CA, skipping if it already
// exists (idempotent — used during initial bring-up).
func (ca *CA) Issue(s CertSpec) error { return ca.issue(s, false) }

// Reissue signs a leaf cert per spec, overwriting any existing crt/key with a
// fresh key + serial + validity window. This is the rotation path.
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
		// An existing leaf that no longer covers what its spec requires is
		// reissued. Skip-if-exists alone meant a SAN added to
		// the spec never reached a node that already had the cert: a cluster
		// created with a custom service CIDR kept an apiserver.crt without its
		// Service IP across the upgrade that fixed the spec, and TLS to
		// kubernetes.default kept failing. The same applies to a joined control
		// plane that predates the VIP SAN. A superset is left alone, so a SAN
		// that drops out of the spec never churns certificates.
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

// missingSANs lists the IP and DNS SANs the spec requires that the existing
// certificate lacks. An unreadable or unparsable certificate is an error, not a
// reason to overwrite it silently.
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
			continue // signLeaf skips it too
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

// SubjectDrift lists, as "name (subject)", the leaves on disk whose subject's
// CN or O differs from the one LeafSpecs issues. A missing leaf is not drift.
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

// signLeaf generates a fresh key and signs a leaf certificate per spec with this
// CA, returning the DER cert + key. It is the single crypto path shared by the
// on-disk issue/Reissue and the in-memory IssuePEM — one place decides key size,
// validity window, key usages, and SAN handling.
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

// LoadCA loads a CA keypair ({name}.crt/.key) from PKIDir. Exported so callers
// outside this package can sign a short-lived leaf in memory — the operator's
// admission-webhook serving cert (internal/operator), which is re-minted on
// demand from the live cluster root rather than persisted alongside the
// rotation-managed leaves.
func LoadCA(name string) (*CA, error) { return loadCA(name) }

// IssuePEM signs a leaf per spec and returns its cert + key as PEM, writing
// nothing to disk. Unlike Issue/Reissue it is neither idempotent nor persisted:
// it is for process-local, re-mintable certs (the webhook serving cert) where a
// file under PKIDir — next to the rotation scratch — would be the wrong home.
func (ca *CA) IssuePEM(s CertSpec) (certPEM, keyPEM []byte, err error) {
	der, key, err := ca.signLeaf(s)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// ReadCABundle returns the {base}-bundle.crt trust bundle (new‖old during a CA
// rotation). This is the correct caBundle for an admission webhook whose serving
// cert is signed by this CA: it trusts both the old and new roots across a
// rotation, so a serving cert signed by either validates.
func ReadCABundle(base string) ([]byte, error) {
	return os.ReadFile(bundleCrtPath(base))
}

// EnsureSAKeypair writes the service-account signing keypair (sa.key/sa.pub),
// skipping if already present.
func EnsureSAKeypair() error {
	if _, err := os.Stat(filepath.Join(version.PKIDir, "sa.key")); err == nil {
		return nil
	}
	return NewSAKeypair("sa")
}

// NewSAKeypair always generates a fresh SA signing keypair to {name}.key/{name}.pub,
// overwriting. Used to stage a rotation key ("sa-new"). EnsureSAKeypair wraps this
// with skip-if-exists for the live "sa" pair.
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

// frontProxyClientSpec is the one leaf signed by the front-proxy CA (a separate
// trust domain), not the root CA.
var frontProxyClientSpec = CertSpec{Name: "front-proxy-client", CN: "front-proxy-client", ClientAuth: true}

// APISANs are the apiserver-certificate names that come from the cluster rather
// than the node: the extra IPs and the cluster DNS domain. A nil *APISANs, or an
// empty field, is the default for that part.
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

// ClusterAdminsGroup is the admin leaf's group, bound to cluster-admin by a
// ClusterRoleBinding of the same name rather than built into the apiserver.
const ClusterAdminsGroup = "stratum:cluster-admins"

// EtcdCN is the etcd leaf's Subject CommonName, the one identity etcd's peer
// port accepts (--peer-cert-allowed-cn).
const EtcdCN = "etcd"

// LeafSpecs is the full set of root-CA-signed control-plane leaves for a node,
// the single source of truth shared by GenerateAll (initial) and RotateLeaves.
// The front-proxy-client leaf (front-proxy-CA-signed) is handled separately.
// LeafSpecs builds the control-plane leaves for a node. nodeIP may be a single
// IP or a comma-joined "<v4>,<v6>" under dual-stack (each family becomes a SAN on
// the node-bound certs). sans are the apiserver-cert names that come from the
// cluster network: the IPs are the `kubernetes` Service IP, plus the IPv6
// service IP and "::1" under dual-stack (netcfg.Config.APIServerExtraIPs), and
// the domain is the cluster domain.
//
// When sans carries no IPv4 service IP (nil, or IPv6-only extras), the
// default 10.96.0.1 is used, so a caller that passes nothing still gets the SAN
// every default cluster needs. A custom service CIDR reaches the cert only
// through sans — the default is a fallback, never an override. The
// same holds for the cluster domain.
func LeafSpecs(host, nodeIP string, sans *APISANs) []CertSpec {
	nodeIPs := strings.Split(nodeIP, ",") // "v4" or "v4,v6"
	etcdIPs := append([]string{"127.0.0.1"}, nodeIPs...)
	svcIP, found := netcfg.Default().ServiceIPv4, false
	var extra []string
	for _, ip := range sans.ips() {
		// The FIRST IPv4 extra is the kubernetes Service IP; any later IPv4 (a
		// VIP, appended after it) is an ordinary extra SAN.
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
		// No kube-proxy identity: Cilium's kube-proxy replacement is the only
		// service dataplane (kube-proxy is not built or shipped). kube-vip's election client
		// takes the slot instead.
		{Name: "kube-vip", CN: "system:kube-vip", DNS: []string{"localhost"}, ClientAuth: true},
	}
}

// etcdLeafNames are the leaves that belong to etcd's trust domain rather than the
// Kubernetes one. Both are issued by "etcd-ca" so that a kubelet client
// certificate — signed by the cluster root — no longer authenticates to etcd.
//
// There is deliberately no separate "etcd-peer" leaf: the single "etcd" leaf is
// both the server cert (--cert-file) and the peer cert (--peer-cert-file), see
// internal/staticpod/args.go. One write moves both roles at once.
var etcdLeafNames = map[string]bool{"etcd": true, "apiserver-etcd-client": true}

// SplitLeafSpecs partitions LeafSpecs by signing CA: the cluster root signs the
// Kubernetes leaves, "etcd-ca" signs etcd's. Callers that need every leaf
// regardless of signer (LeafNames, `certs check`) keep using LeafSpecs.
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

// LeafNames returns every rotatable leaf basename (root-CA leaves + front-proxy-client).
// Deliberately excludes the CAs and the SA keypair — those are not leaf-rotatable.
func LeafNames(host, nodeIP string) []string {
	names := make([]string, 0, 10)
	for _, s := range LeafSpecs(host, nodeIP, nil) { // names don't depend on IP SANs
		names = append(names, s.Name)
	}
	return append(names, frontProxyClientSpec.Name)
}

// GenerateAll produces the full control-plane PKI for a node with the given
// short hostname + node IP. Idempotent (skip-if-exists) — safe to
// re-run.
func GenerateAll(host, nodeIP string, sans *APISANs) error {
	return GenerateAllWithWriter(os.Stdout, host, nodeIP, sans)
}

// GenerateAllWithWriter generates node PKI and reports reissue progress to w.
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
	// The etcd trust bundle must exist BEFORE the etcd leaves are minted, because
	// it decides who signs them. On an HA join it arrived with the shared certs,
	// so it carries the CLUSTER's trust state rather than this node's guess.
	if err := EnsureEtcdTrustBundle(); err != nil {
		return fmt.Errorf("etcd-ca bundle: %w", err)
	}
	// Issue is skip-if-exists, so on an existing node the live (root-signed) etcd
	// leaves are left alone — moving them is phase issue-new, safe only once every
	// control plane trusts the etcd CA. On a JOINING node they do not exist yet,
	// and minting them under the etcd CA while the rest of the cluster is still
	// root-signed is exactly that unilateral issue-new: the peers reject the new
	// member, it never enters raft, and it is left behind as a voting member.
	// EtcdSignerName answers which CA the live members actually accept.
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
	// Trust-bundle files: what components actually trust (systemd points the
	// --*-ca-file / --service-account-key-file flags here, not at the plain
	// certs). On a fresh install these are single-cert copies of the live CA/SA;
	// CA rotation later makes them old‖new. Skip-if-exists so re-runs don't churn.
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

// RotateLeaves force-reissues every control-plane leaf from the EXISTING CAs,
// with fresh keys + a new LeafValidity window. It loads (never regenerates) the
// root + front-proxy CAs and never touches ca.*/front-proxy-ca.*/sa.* — those
// are the heavier cluster-wide "CA rotation" flow. Kubeconfigs (which embed
// client certs) must be regenerated by the caller afterward.
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
	// The etcd leaves are reissued by whichever CA signs them TODAY — see
	// etcdLeafSigner. A leaf rotation must never move a trust anchor.
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

// etcdLeafSigner returns the CA that currently signs the etcd leaves, so that a
// LEAF rotation preserves the trust anchor instead of silently moving it. This is
// what keeps the RotateLeaves callers — the renew timer (certs.Rotate), a
// `stratum up` re-run, `rotate-ca --phase=sign-new`, and the rotation
// ROLLBACK path — from re-signing etcd's certs with the cluster root and locking
// each apiserver out of its own etcd.
//
// Reading the live etcd.crt (rather than "etcd-ca exists ⇒ use it") is what makes
// the mid-migration states safe: at phase trust-both etcd-ca is on disk but the
// leaves are still root-signed cluster-wide, and reissuing them under etcd-ca
// there would be a unilateral phase issue-new on one node — breaking peer TLS
// against every control plane that has not migrated yet.
func etcdLeafSigner(root *CA) (*CA, error) {
	// PROVENANCE RULE: a renewal must not change a leaf's trust domain.
	// RotateLeaves exists to issue fresh keys and a new validity window; moving a
	// leaf between CAs is a MIGRATION operation, and issue-new is its only writer.
	// So this reads the artifact it is about to overwrite and nothing else.
	//
	// It deliberately does NOT consult ReadEtcdTrust(). Between issue-new and
	// drop-root the bundle still carries a root while the leaves have already
	// moved — that gap IS the migration's safety margin — so a bundle-derived
	// signer would regress a migrated node's etcd leaf back to the root.
	//
	// Where no leaf exists yet — fresh install, HA join — there is nothing to
	// preserve and bundle state is the right input; that decision lives in
	// GenerateAll, not here.
	if !exists(EtcdCAName) {
		return root, nil // pre-migration node: etcd's leaves belong to the root
	}
	anyPresent, allUnderEtcdCA := false, true
	for name := range etcdLeafNames {
		ok, err := leafChainsTo(name, EtcdCAName)
		if errors.Is(err, os.ErrNotExist) {
			continue // nothing to preserve for this one
		}
		if err != nil {
			// Fail closed. Treating a truncated or unreadable etcd.crt as "nothing
			// to preserve" let the unattended renew timer promote both leaves.
			return nil, fmt.Errorf("pki: cannot determine the current signer of %s: %w", name, err)
		}
		anyPresent = true
		if !ok {
			allUnderEtcdCA = false
		}
	}
	if !anyPresent || !allUnderEtcdCA {
		// No provenance to preserve, or provenance says root. Either way a renewal
		// keeps them on the root; only issue-new moves them.
		return root, nil
	}
	etcdCA, err := loadCA(EtcdCAName)
	if err != nil {
		return nil, fmt.Errorf("load etcd CA: %w", err)
	}
	return etcdCA, nil
}

// ── CA rotation ──────────────────────────────────────────────────────────────
//
// CA rotation splits the "trust set" from the "active signer". Trust files
// ({base}-bundle.crt, sa-bundle.pub) hold what components accept; the plain
// files (ca.crt/ca.key, sa.key) are the active signer. A rotation runs in three
// phases, each applied to every node before the next:
//
//	trust-new  RotateCAMaterial     stage {ca,front-proxy-ca,sa}-new + bundles=new‖old
//	sign-new   PromoteCA/PromoteSA  swap the live signer to the new material; RotateLeaves
//	finalize   FinalizeCA           collapse bundles to new-only; drop old trust
//
// Bundle-only crypto: the new CA is a fresh self-signed root and the old‖new
// bundle is the trust bridge (no cross-signing — it is functionally invisible
// here since we push the bundle to every node and embed it in every kubeconfig).

func bundleCrtPath(base string) string { return filepath.Join(version.PKIDir, base+"-bundle.crt") }
func saBundlePath() string             { return filepath.Join(version.PKIDir, "sa-bundle.pub") }

func bundleExists(base string) bool {
	_, err := os.Stat(bundleCrtPath(base))
	return err == nil
}

// concatFiles writes the concatenation of src (in order) to dst, separating
// blocks with a newline so PEM blocks never run together. The write is atomic
// through the shared writer: a trust bundle cut in half by a crash stops the
// components that point their --*-ca-file at it.
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

// WriteCABundle writes {base}-bundle.crt = concatenation of the given cert
// basenames (each {name}.crt), in order (new-first during rotation). Passing a
// single name yields a one-cert bundle (the fresh-install / finalize case).
func WriteCABundle(base string, certNames ...string) error {
	src := make([]string, len(certNames))
	for i, n := range certNames {
		src[i] = path(n, "crt")
	}
	return concatFiles(bundleCrtPath(base), 0o644, src...)
}

// WriteSABundle writes sa-bundle.pub = concatenation of the given pubkey
// basenames (each {name}.pub). The apiserver accepts every key in the file, so
// old‖new lets it verify tokens signed by either key during a rotation.
func WriteSABundle(pubNames ...string) error {
	src := make([]string, len(pubNames))
	for i, n := range pubNames {
		src[i] = filepath.Join(version.PKIDir, n+".pub")
	}
	return concatFiles(saBundlePath(), 0o644, src...)
}

// RootCertPresent reports whether this node still holds the certificate of the
// root CA the cluster trusts. A rotation carries that certificate into the new
// trust bundle, which is what lets every node and client verify the new root
// by the old one; the old key is not needed for that.
func RootCertPresent() bool { return certTrusted("ca") }

// certTrusted reports whether {name}.crt is a certificate in {name}-bundle.crt,
// the bundle this node trusts now, or simply a certificate when there is no
// bundle: a file copied in from elsewhere must not replace the trusted root.
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

// Signer names the key pair that signs for base ("ca" or "sa"): base itself,
// or, when base's key is gone and a rotation has staged base-new, the staged
// pair. A forced rotation goes ahead without a lost key, and from trust-new to
// sign-new the components that sign (the controller-manager, the apiserver's
// service-account tokens, the operator's webhook) would otherwise have no key
// at all; what the staged pair signs is trusted everywhere trust-new has run.
// PromoteCA and PromoteSA make base the signer again.
func Signer(base string) string {
	if _, err := os.Stat(path(base, "key")); err == nil {
		return base
	}
	if _, err := os.Stat(path(base+"-new", "key")); err == nil {
		return base + "-new"
	}
	return base
}

// ErrNoRootCert is RotateCAMaterial's refusal when ca.crt is gone.
var ErrNoRootCert = errors.New("cannot rotate the CA without the certificate of the root the cluster trusts: " +
	"nothing could verify the new root by it, so every component would fail TLS once trust-new restarts it. " +
	"The certificate is public: copy ca.crt from another control plane, or, when this node's ca-bundle.crt " +
	"holds a single certificate, copy that file to ca.crt, then run the rotation again")

// ErrNoOldPublicHalf is RotateCAMaterial's refusal when front-proxy-ca.crt, or
// sa.pub with no sa.key to derive it from, is gone.
var ErrNoOldPublicHalf = errors.New("cannot rotate the CA without the old front-proxy CA certificate and service-account public key: " +
	"the trust bundles must carry them, or what the old signers issued stops verifying at the trust-new restart, " +
	"and the rotation shares them with the other control planes. Both are public: copy the file from another control plane, " +
	"or, when this node's front-proxy-ca-bundle.crt or sa-bundle.pub holds a single entry, copy that file to front-proxy-ca.crt or sa.pub, " +
	"then run the rotation again")

// OldPublicHalvesPresent reports whether RotateCAMaterial would pass its
// ErrNoOldPublicHalf checks, for a caller that must refuse before it starts.
func OldPublicHalvesPresent() bool {
	return certTrusted("front-proxy-ca") && (fileExists(filepath.Join(version.PKIDir, "sa.pub")) || fileExists(path("sa", "key")))
}

// restoreSAPub writes sa.pub from sa.key when only the public half is gone.
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

// stageSAKeypair stages sa-new.key and sa-new.pub. NewSAKeypair writes the key
// before the public half, so a trust-new stopped between the two leaves the key
// alone: the public half is derived from it rather than a new pair minted,
// since on a follower the staged key came from the cluster and must stay the
// one every control plane holds.
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

// RotateCAMaterial (phase trust-new) stages a fresh root CA, front-proxy CA, and
// SA keypair (ca-new.*, front-proxy-ca-new.*, sa-new.*) and rewrites the trust
// bundles as new‖old so every component trusts both once restarted. It does NOT
// promote the signer — the live ca.*/sa.* keep signing until PromoteCA/PromoteSA.
// Idempotent: re-running reuses already-staged *-new material. force allows
// proceeding when an old private key (ca.key, front-proxy-ca.key or sa.key) is
// gone, and only force does: a rotation that goes ahead without one is recorded
// as forced, so no node of it is rolled back. The old public halves are carried
// into the bundles and are required even with force: without the root's
// certificate no component could verify the new root, and every one would fail
// TLS from the restart that follows. A lost sa.pub is derived from sa.key.
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
	// A new rotation starts: neither marker may carry over from the last one.
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
	// etcd's bundle is a trust bundle too: widen it with the same roots.
	if err := syncEtcdBundleToRoot(caSrc...); err != nil {
		return fmt.Errorf("etcd-ca bundle: %w", err)
	}
	return WriteSABundle(saSrc...)
}

// liveSignersStaged names the live signer files that are byte-identical to
// their staged replacement. Promotion renames the staged files away, so the
// two never match on a node that staged its own rotation or fetched another
// control plane's staged material; they match only on a node whose live files
// were overwritten with signers another node had already promoted.
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

// PrepareStaging is what must happen before any CA material is staged on this
// node, including the material a follower control plane fetches from the
// initiator: it refuses a node that has promoted and not finalized, and removes
// the obsolete backups of an idle one. It runs ahead of the fetch because the
// fetch's own writes stage a root, which makes the node look mid-rotation and
// keeps its backups. It changes nothing when it refuses. force is the
// rotation's --force: a forced recovery may have lost an old key, which must not
// block staging.
func PrepareStaging(force bool) error {
	if rotationPromoted() {
		way := "Finish the rotation with `stratum certs rotate-ca --phase=finalize` once every node has completed sign-new, " +
			"or abandon it with the rollback below"
		switch {
		case !CAPromoted():
			// Promoted but unmarked: sign-new stopped before it recorded the
			// promotion. It resumes, and what is staged is the rest of that
			// promotion; finalize refuses until sign-new has recorded it.
			way = "This node's sign-new stopped before it recorded the promotion: run `stratum certs rotate-ca --phase=sign-new --yes` on it again to finish, " +
				"and finalize only after that. Or abandon the rotation with the rollback below"
		case len(CAStagedFiles()) > 0:
			// Staged files beside a recorded promotion are a repeated staging:
			// finalize refuses them and sign-new skips as applied, so the only way
			// out is the rollback, which restores from the backups this refusal
			// protects.
			way = "New CA material is staged on this node again, so finalize would refuse and sign-new would skip: do not finalize this node. Abandon the rotation with the rollback below"
		}
		return fmt.Errorf("pki: refusing to stage a CA rotation (nothing has been changed): this node has already promoted its new CA, and staging again would replace the staged "+
			"material and clear the promotion record, so a later sign-new would overwrite the %s backups with the new CA and a rollback could no longer restore the original. "+
			"%s: run `stratum certs rotate-ca --rollback --cluster --check` from a control plane, then `stratum certs rotate-ca --rollback --yes` on every control plane and then every worker", RootCAName+".pre-carotate.*", way)
	}
	return removeObsoleteBackups(force)
}

// rotationPromoted reports whether this node's rotation is past promotion and
// not yet finalized: the root's backup is there, and sign-new recorded its
// promotion or stopped before recording it (SignNewResumable). Its staged files
// are gone and its backups are the only way back, so nothing may stage over it.
func rotationPromoted() bool {
	return fileExists(filepath.Join(version.PKIDir, "ca.pre-carotate.crt")) && ((CAPromoted() && !CAFinalized()) || SignNewResumable())
}

// removeObsoleteBackups deletes the .pre-carotate files that no rotation on
// this node owns. Finalize and rollback remove them, but older releases ignored
// an unlink failure, so the backups of a completed A→B rotation can survive. The
// next rotation's rollback would restore A over B, the signer it started from.
//
// A backup is obsolete only when it differs from the live file it backs up and
// the node is idle: nothing staged (a half pair counts), one anchor in the trust bundle, and no
// promoted-not-finalized marker. A backup equal to its live file is a rollback
// that restored but has not finished its cleanup, and a retry needs every
// backup, the ones that still differ included, to repair and finish: one equal
// file keeps them all. A live file that is missing under force is a forced
// recovery whose backup may be the only copy of that key: it keeps them all and
// does not fail. Everything is also left alone when the bundle is unreadable.
//
// shortcut: a node that an older release staged beside such backups is not
// cleaned, because that state cannot be told from a promotion whose staging was
// repeated. Do not roll it back before sign-new: that restores the stale
// backups' CA. Nothing confirms that every node trusts this rotation's new CA:
// neither the cluster-wide rollback check nor the trusts-new= field of the
// per-node one identifies the intended signer. Never run trust-new on a control
// plane to fix it (it mints its own CA). Resume the open run from the control
// plane that holds the staged material; escalate when the run has ended or
// trust cannot be confirmed. Once the resumed run's sign-new promotes, it
// overwrites the backups with the live CA, and a rollback is safe after.
func removeObsoleteBackups(force bool) error {
	rot := ReadCARotation()
	if rot.Err != nil {
		return nil
	}
	// Any staged file counts, not only a complete pair (rot.Staged): a crash
	// mid-staging leaves half of one, and that node is not idle.
	if len(CAStagedFiles()) > 0 || rot.BundleCAs != 1 || (CAPromoted() && !CAFinalized()) {
		return nil
	}
	// Decide first, delete after: a live file that cannot be read says nothing
	// about whether its backup is obsolete, so one such file keeps every backup
	// and refuses, instead of deleting the only copy of a key.
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

// promotePair backs up the live {name}.crt/.key to {name}.pre-carotate.* and
// atomically renames {name}-new.* into place (same-fs rename is atomic).
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

// PromoteCA (phase sign-new) swaps the live root + front-proxy CA to the staged
// new material. The trust bundles still carry old‖new, so old-signed peers and
// tokens keep validating. Callers must RotateLeaves afterward so all leaves are
// reissued under the new signer.
func PromoteCA() error {
	// All of it or none of it: a missing staged file found after the first rename
	// would leave the root consumed and the front-proxy half-done.
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

// requireStaged errors, naming every file that is missing, unless all exist.
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

// PromoteSA (phase sign-new) swaps the live SA signing keypair to sa-new.*.
// sa-bundle.pub still contains both keys, so tokens signed by the old key remain
// verifiable until finalize drops it.
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

// carotateScratch is every rotation-only file, removed on finalize/rollback.
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

// caPromotedMarker records that sign-new finished its file work: the new CA and SA
// promoted and the leaves reissued under them. It is the only proof finalize
// accepts that this node promoted — the .pre-carotate backups are not, because
// a forced rotation backs up no lost key and a failed cleanup can leave one
// behind after a rollback. Removed with the other rotation scratch on finalize and rollback,
// and by trust-new.
const caPromotedMarker = "carotate-promoted"

// MarkCAPromoted writes the marker. Call it only after the promotion and the
// leaf reissue under it have succeeded.
func MarkCAPromoted() error {
	if err := os.WriteFile(filepath.Join(version.PKIDir, caPromotedMarker), nil, 0o600); err != nil {
		return fmt.Errorf("record promotion: %w", err)
	}
	return nil
}

// CAPromoted reports whether MarkCAPromoted ran in this node's current rotation.
func CAPromoted() bool {
	_, err := os.Stat(filepath.Join(version.PKIDir, caPromotedMarker))
	return err == nil
}

// removeCAMarkers drops both rotation markers. A failed removal is an error, not
// a skip: a stale carotate-finalized would let a later finalize pass unchecked.
func removeCAMarkers() error {
	for _, m := range []string{caFinalizedMarker, caPromotedMarker} {
		if err := os.Remove(filepath.Join(version.PKIDir, m)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", m, err)
		}
	}
	return nil
}

// caFinalizedMarker records that FinalizeCA collapsed trust to the promoted CA. It
// is what tells a finalize retry (restart failed) from a node that never ran
// sign-new: both look idle on disk. trust-new and rollback remove it.
const caFinalizedMarker = "carotate-finalized"

// CAFinalized reports whether this node's last rotation ended in FinalizeCA.
func CAFinalized() bool {
	_, err := os.Stat(filepath.Join(version.PKIDir, caFinalizedMarker))
	return err == nil
}

// CAStagedFiles lists the staged *-new files on disk, complete pair or not: a
// crash mid-staging leaves a crt without its key, which CARotationStaged misses.
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

// StagedSignersMissing lists the staged signer files a control plane that fetched another's rotation lacks.
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

// removeScratch deletes every rotation-only file. A file that will not go is an
// error, not a skip: a surviving backup or marker would outlive the rotation.
func removeScratch() error {
	var errs []error
	for _, f := range carotateScratch {
		if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// FinalizeCA (phase finalize) collapses the trust bundles to the live (new) CA/SA
// only and removes rotation scratch files. Only safe once every node trusts and
// uses the new material (the certs layer gates this).
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
	// The old root leaves etcd's trust at the same instant it leaves Kubernetes
	// trust — including under --force, where the root being dropped is the one
	// suspected of compromise.
	if err := syncEtcdBundleToRoot("ca"); err != nil {
		return err
	}
	if err := WriteSABundle("sa"); err != nil {
		return err
	}
	// Before the scratch goes: a failure in between leaves carotate-promoted, so a
	// retry is accepted either way.
	if err := os.WriteFile(filepath.Join(version.PKIDir, caFinalizedMarker), nil, 0o600); err != nil {
		return fmt.Errorf("record finalize: %w", err)
	}
	if err := removeScratch(); err != nil {
		return fmt.Errorf("finalize: remove rotation scratch: %w", err)
	}
	return nil
}

// restoreFile names one file of a credential pair the CA rollback reinstates.
type restoreFile struct {
	name, ext string
}

// RollbackCA reverts a partial rotation: restores the pre-rotation CA/SA from the
// .pre-carotate backups (present only if sign-new was reached), rewrites the trust
// bundles to the restored material only. It leaves the staged and backup files in
// place. If sign-new was reached the caller must also RotateLeaves (under the
// restored old CA), then RemoveRotationScratch, then restart.
func RollbackCA() error {
	if err := RollbackCheck(); err != nil {
		return err
	}
	split, err := KubeletClientSplit()
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	// First, so a marker that cannot be removed fails the rollback before it
	// changes anything.
	if err := removeCAMarkers(); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	// restore reinstates {name}.{ext} from its .pre-carotate backup. A missing
	// backup means sign-new was not reached for this file (nothing to undo) and is
	// a benign skip; any other read error, or a write failure, is fatal — this is
	// the disaster-recovery path, so a swallowed error must never report success.
	// The write is atomic through the shared writer: this path exists to repair
	// broken trust material, so it must not be able to leave more of it behind.
	// Pairs are restored through writePair, so a first half that lands
	// not-durably (the rename put the file in place, the directory sync
	// failed) still writes the matching second half: a rollback that stopped
	// between them would pair the restored old certificate with the new
	// signer's key.
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
	// The site that fails SILENTLY if it is missed: without this the etcd bundle
	// keeps trusting the NEW root — the one the rollback existed to un-trust —
	// and nothing breaks, because etcd's leaves went back to the old root which
	// is still in the stale bundle.
	if err := syncEtcdBundleToRoot("ca"); err != nil {
		return err
	}
	return WriteSABundle("sa")
}

// ErrRollbackUnsafe is RollbackCA's refusal when rolling back would not leave
// the node an old certificate and key that belong together.
var ErrRollbackUnsafe = errors.New("rolling back would not leave this node a working old signer. " +
	"Do not roll back any node of this rotation; finish it forward")

// signNewForward is the forward path every RollbackCA refusal gives. sign-new
// is safe only once every node trusts the new CA, which this node cannot see,
// so it sends the operator to the cluster-wide check first.
const signNewForward = "first run `stratum certs rotate-ca --rollback --cluster --check` from a control plane: no node may sign with the new CA while another has not completed trust-new, and the check says what to do about one that has not. " +
	"Then run `stratum certs rotate-ca --phase=sign-new --yes` on each control plane and then each worker where it has not completed, " +
	"then finish with \"Finishing a rotation by hand\" in the certificate-rotation documentation"

// caForcedMarker records that this rotation was staged with --force. A forced
// rotation may have gone ahead without an old key on some control plane, and
// rolling back any node of it, even one that kept its keys, splits the cluster:
// that node returns to the old root while another can only go forward. Written
// by RotateCAMaterial, removed with the rotation scratch on finalize.
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

// ForcedRotation reports whether the rotation in progress was staged with --force.
func ForcedRotation() bool { return fileExists(filepath.Join(version.PKIDir, caForcedMarker)) }

// RollbackCheck reports why RollbackCA would refuse on this node, and changes
// nothing: a rotation started with --force, or a pair the rollback would
// restore whose certificate and key no longer match (an old key lost mid-
// rotation). Every refusal wraps ErrRollbackUnsafe. It is the whole of
// RollbackCA's refusal logic, so `rotate-ca --rollback --check` on a node
// answers exactly what `--rollback --yes` there would do, and the cluster-wide
// check can ask every node before any node rolls back.
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
	// The same read RollbackCA makes before it changes anything: a kubelet
	// client-CA bundle it cannot read stops the rollback there, so the check
	// must report it too, or a cluster-wide check would clear this node.
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

// RollbackUnsafePairs names the pairs a rollback on this node would leave
// without a matching certificate and key, for `stratum certs check`.
func RollbackUnsafePairs() []string { return rollbackMismatchedPairs() }

// rollbackPairs are the pairs RollbackCA restores, each with its public half.
var rollbackPairs = []struct{ name, public string }{
	{"ca", "crt"}, {"front-proxy-ca", "crt"}, {"sa", "pub"}, {KubeletClientCAName, "crt"},
}

// rollbackFile is the file {name}.{ext} that would be live after RollbackCA:
// the .pre-carotate backup when there is one, the live file otherwise.
func rollbackFile(name, ext string) string {
	if bak := filepath.Join(version.PKIDir, name+".pre-carotate."+ext); fileExists(bak) {
		return bak
	}
	return path(name, ext)
}

// rollbackMismatchedPairs names the pairs RollbackCA would leave unusable: for
// each of the root CA, the front-proxy CA and the SA keypair, the certificate
// or public key and the private key that would be live afterwards must both
// exist and match.
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

// RollbackTarget names the signers a rollback on this node would leave live,
// as name=fingerprint of each pair's public key, so the cluster-wide check can
// tell that every control plane would return to the same ones. A control
// plane that finalized holds no backups, so its target is the new signers,
// and differs from that of one that has not finalized.
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

// keyMatches reports whether the private key in keyFile belongs to the public
// key in pubFile, a PEM certificate or PKIX public key.
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

// SignNewResumable reports whether this node's sign-new promoted the new root
// but stopped before recording it: nothing is staged any more, the root's
// backup is there, the trust bundle still carries old‖new, and no promotion
// marker. Such a node cannot run sign-new from staged material and must not be
// rolled back once a key was lost, so sign-new finishes it instead.
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

// FinishPromotion completes a sign-new that promoted the root and stopped: it
// promotes the front-proxy CA and the SA keypair if they are still staged.
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

// RemoveRotationScratch deletes the staged and backup files once a rollback is
// complete. It is separate from RollbackCA because the caller still needs the
// backups to tell it whether the leaves must be reissued, and a retry after a
// failed cleanup must find them.
func RemoveRotationScratch() error {
	if err := removeScratch(); err != nil {
		return fmt.Errorf("rollback: remove rotation scratch: %w", err)
	}
	return nil
}

// CARotationStaged reports whether a CA rotation is mid-flight (new CA staged
// but not yet finalized).
func CARotationStaged() bool { return exists("ca-new") }

// The on-disk states of a node's root-CA rotation. Every reader and every guard
// (ReadCARotation, RotateCAMaterial, SignNewResumable, certs.caPhaseApplied,
// checkCPFinalize, RollbackCA) reads these files, and a state not in this table
// is damage, not a rotation. "backup" is ca.pre-carotate.*, "staged" is the
// ca-new pair, "bundle" the number of anchors in ca-bundle.crt, and "promoted"
// and "finalized" the carotate-promoted and carotate-finalized markers.
//
//	state            staged  backup           bundle  promoted  finalized
//	idle             no      no               1       no        no, or the last rotation's
//	trust-new        yes     no               2       no        no
//	promoted, cut    no      yes, != live     2       no        no   sign-new stopped before its marker: sign-new resumes it, or rollback undoes it
//	promoted         no      yes, != live     2       yes       no   finalize, or rollback
//	finalizing       no      maybe, != live   1       maybe     yes  finalize collapsed trust; the scratch removal may have stopped, and a retry finishes it
//	rollback, cut    maybe   yes, == live     1       no        no   RollbackCA restored; the repair of the leaves or the cleanup stopped, and a retry finishes it
//	residue          no      yes, != live     1       no        any  not a rotation: backups an older finalize failed to unlink; the next trust-new removes them
//
// A promoted rotation's backups are its only way back, so nothing stages a new
// rotation over one (rotationPromoted). A backup that differs from its live file
// where no promotion can be under way is residue (removeObsoleteBackups).
//
// CARotation is the one answer to "what root-CA rotation state is this node
// in", read from the PKI directory pki owns.
//
// Four readers need that state — certs' RotationPhase, signerPromoted and
// checkCARotationState, and token.CACertHash — and each spelling pki's staged
// and backup file names for itself would leave those names in three packages
// and the phase answers free to drift apart. Every rotation-state consumer
// reads this struct instead; only pki spells the rotation file names.
type CARotation struct {
	// Staged reports whether a fresh root CA is staged but not yet promoted
	// (ca-new.crt and ca-new.key both on disk): the trust-new phase.
	Staged bool
	// Promoted reports whether the live root has a .pre-carotate backup:
	// sign-new promoted the staged root, and finalize or a completed
	// rollback has not yet removed the backup.
	Promoted bool
	// BundleCAs is the number of CA anchors in the live root trust bundle:
	// two while a rotation has widened trust to old‖new, one at rest.
	BundleCAs int
	// Anchor is the basename of the root CA certificate a new pin must be
	// computed from: the staged root while its certificate is on disk —
	// finalize renames it over the live ca.crt — and the live root
	// otherwise.
	Anchor string
	// Err is set when the trust bundle could not be read or parsed. Only
	// os.ErrNotExist is a benign read (no bundle written yet); every other
	// error is malformed or corrupt material a guard must refuse rather
	// than read as idle. Staged, Promoted and Anchor stay meaningful: they
	// come from the CA files, not the bundle.
	Err error
}

// ReadCARotation inspects this node's PKI directory and returns the rotation
// state every consumer shares: whether a new root is staged, whether the
// promotion backup is present, how many anchors the live trust bundle carries,
// and which root file a new pin must name. A bundle read or parse failure sets
// Err rather than degrading to a plausible-looking state.
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

// ── One PEM bundle decoder, declared per consumer ────────────────────────────
//
// Three readers consume PEM trust bundles — CABundleCertCount (the rotation
// counter), bundleCerts (the etcd trust reader) and certs.parseTrustBundle
// (the worker refresh, which writes its bytes verbatim into the kubelet's
// clientCAFile) — and they do not agree on what a bundle may carry, because
// each one's file is loaded by a different PEM parser. DecodePEMBundle is the
// one decoder they all share; each consumer passes the BundlePolicy its file's
// loader dictates, so the differences are declared here and tested in one
// place (TestOneBundleParser) instead of re-spelled as three ad-hoc loops.

// BundleLeading declares what bytes before a PEM block may be.
type BundleLeading int

const (
	// LeadingTrim allows any run of space, tab, CR or LF before a block and
	// reads the block from offset 0 after trimming it.
	LeadingTrim BundleLeading = iota
	// LeadingNewlineEnded allows the same run only when it ends in a newline:
	// pem.Decode honours a BEGIN line only at offset 0 or immediately after a
	// '\n', and client-go's cert.ParseCertsPEM — the parser the kubelet loads
	// clientCAFile with — behaves the same way, so a run ending in a space, a
	// tab or a lone CR would anchor a block the kubelet then skips.
	LeadingNewlineEnded
	// LeadingPemSkip leaves the bytes to pem.Decode, which skips anything
	// before a BEGIN line: the same skip x509.CertPool.AppendCertsFromPEM —
	// what etcd loads --trusted-ca-file with — performs.
	LeadingPemSkip
)

// BundlePolicy is one consumer's declared validation policy for a PEM trust
// bundle. The trust contexts differ in what their loaders actually accept, so
// the shared decoder keeps the differences explicit instead of demanding
// identical acceptance everywhere: the kubelet's parser drops blocks the count
// and etcd readers accept, and the etcd pool accepts blocks the kubelet's
// parser drops.
type BundlePolicy struct {
	// Leading is the declared treatment of bytes before each PEM block.
	Leading BundleLeading
	// RefusePEMHeaders refuses a CERTIFICATE block that carries PEM headers.
	// The kubelet's parser drops such blocks outright, so a bundle written
	// verbatim into its clientCAFile may not carry one; the count and etcd
	// readers read files nothing loads with that parser, so they keep them.
	RefusePEMHeaders bool
	// SkipNonCertificateBlocks passes PEM blocks of any other type over. The
	// etcd pool skips them; the count and the kubelet refuse, because a block
	// the loader would not load must not sit in trust material this code has
	// already accepted.
	SkipNonCertificateBlocks bool
	// AcceptNonCA keeps parseable leaf certificates in the answer. The etcd
	// pool never checks IsCA — a leaf in the pool still anchors a chain — so
	// the etcd reader must see it; the count and the kubelet refuse a non-CA,
	// because every entry becomes a trust anchor.
	AcceptNonCA bool
	// RefuseEmpty refuses a bundle that decodes to zero certificates instead
	// of returning an empty answer. The count refuses, because a zero count
	// must not authorize a rotation decision, and the kubelet refuses, because
	// an empty clientCAFile trusts nothing; the etcd reader answers the empty
	// set and lets its state mapping decide what an empty bundle means.
	RefuseEmpty bool
}

// CountPolicy is CABundleCertCount's declared policy. The counter feeds the
// rotation-phase decision (one anchor at rest, two mid-rotation), so it counts
// every parseable CA certificate and refuses anything it cannot count
// completely: a non-certificate block, a non-CA certificate, a corrupt block
// or an empty bundle must never widen or advance a rotation. PEM headers are
// kept: pki never writes them, a headered block still carries its DER, and
// this file is never loaded by the kubelet's parser.
var CountPolicy = BundlePolicy{Leading: LeadingTrim, RefuseEmpty: true}

// EtcdPolicy is bundleCerts's declared policy — deliberately the broadest, so
// the reader answers the trust etcd actually holds. etcd loads
// --trusted-ca-file through x509.CertPool.AppendCertsFromPEM, which skips
// non-certificate blocks and never checks IsCA, so refusing such material here
// would report narrower trust than etcd has — the exact failure direction the
// etcd trust-hole defect ran on. Only an unparseable CERTIFICATE block is
// refused, stricter than the pool's silent skip: a corrupt block read as
// absent makes the bundle look narrower than it is.
var EtcdPolicy = BundlePolicy{
	Leading:                  LeadingPemSkip,
	SkipNonCertificateBlocks: true,
	AcceptNonCA:              true,
}

// KubeletPolicy is the kubelet-verbatim policy the worker trust refresh keeps.
// The fetched bytes are written verbatim into the kubelet's clientCAFile,
// which client-go's cert.ParseCertsPEM loads: it honours a BEGIN line only at
// offset 0 or after a newline, drops headered blocks, and skips blocks of any
// other type — so exactly those inputs are refused here rather than validated
// by this code and then skipped by the kubelet.
var KubeletPolicy = BundlePolicy{
	Leading:          LeadingNewlineEnded,
	RefusePEMHeaders: true,
	RefuseEmpty:      true,
}

// DecodePEMBundle decodes every certificate in a PEM trust bundle under one
// declared policy. It is the one PEM decoder for trust bundles: the count, etcd
// and kubelet-verbatim readers call it under their own BundlePolicy, so they
// differ only by declared policy and a policy change is a review here, not a
// divergent rewrite in three packages.
func DecodePEMBundle(b []byte, pol BundlePolicy) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for i := 0; ; i++ {
		if pol.Leading != LeadingPemSkip {
			// The whole input must be PEM: pem.Decode silently skips anything
			// it cannot decode, so each block is anchored at offset 0 and the
			// bytes before it are validated rather than skipped over.
			trimmed := bytes.TrimLeft(b, " \t\r\n")
			// Whitespace that ends the input precedes no block, so it is not
			// the case the check below refuses: the kubelet's parser stops
			// cleanly at it.
			if len(trimmed) == 0 {
				break
			}
			if pol.Leading == LeadingNewlineEnded {
				// pem.Decode honours a BEGIN line only at offset 0 or
				// immediately after a '\n'. Space, tab and a lone CR are
				// whitespace but not '\n', so trimming a run that ends in one
				// of them would validate a block the kubelet's parser skips.
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
			// LeadingPemSkip: pem.Decode ran out of blocks it can skip to.
			// Anything non-whitespace left behind is trailing garbage, not an
			// empty answer.
			if len(bytes.TrimSpace(b)) > 0 {
				return nil, fmt.Errorf("trailing garbage after block %d", i)
			}
			break
		}
		if pol.Leading != LeadingPemSkip {
			// pem.Decode scans forward internally: a "-----BEGIN " line it
			// cannot complete makes it skip everything up to the next one. The
			// prefix check above does not catch that, so require that it
			// consumed exactly the block starting at offset 0.
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

// CABundleCertCount validates and counts CA certificates in {base}-bundle.crt
// (2 during a rotation, 1 at rest) under CountPolicy. Empty or partially
// malformed bundles are errors: an incomplete count must not authorize a new
// rotation.
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

// CertNotAfter parses the NotAfter of a cert basename in version.PKIDir.
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

// ── Trust layout ─────────────────────────────────────────────────────────────
//
// The manifest below is pki's one classification of the trust files it writes
// under version.PKIDir. Before it, the entry lists a joining control plane
// accepts were spelled by hand in internal/ha, and the "trust the bundle, else
// ca.crt" preference was spelled three times — kubeconfig.Write,
// token.ensureClusterInfo and certs.readTrustAnchor — so a new anchor, bundle
// form or fallback meant imitation edits across packages that no check tied
// together.

// TrustFileClass says who holds a trust file.
type TrustFileClass int

const (
	// TrustShared marks material every control plane must hold byte-identically:
	// it travels in the stratum-certs join bundle on every upload.
	TrustShared TrustFileClass = iota
	// TrustRotation marks material a joining control plane receives whenever it
	// is present — the trust bundles and any staged rotation material. It is
	// absent at rest, so an upload skips it instead of failing, and a node
	// joining mid-rotation lands on the same in-flight trust state as the rest
	// of the cluster.
	TrustRotation
	// TrustNodeLocal marks material that never leaves the node: files every
	// node derives from the shared ones, and the rotation's own residue.
	TrustNodeLocal
)

// TrustFile classifies one file pki writes under version.PKIDir.
type TrustFile struct {
	// Name is the basename under version.PKIDir.
	Name string
	// CommonName is the Subject CN of the anchor the file belongs to — the
	// certificate it carries or, for a bundle, the anchor it is named after —
	// and empty for files that carry no certificate: private keys, the SA
	// public half and its bundle, the rotation markers.
	CommonName string
	// Class places the file in the trust layout: held byte-identically by
	// every control plane (TrustShared), received by a joiner whenever present
	// (TrustRotation), or never leaving the node (TrustNodeLocal).
	Class TrustFileClass
	// Rotatable reports whether a rotation rewrites the file: a CA rotation
	// for the signers and the trust bundles (StagedAs naming the staged
	// replacement where one exists), a leaf rotation for the front-proxy
	// client, a re-sync from the root bundle for the kubelet's clientCAFile.
	Rotatable bool
	// StagedAs is the basename a CA rotation stages as this file's replacement
	// ("ca-new.key" for "ca.key"), or "" when nothing is staged for it. A
	// staged file's own StagedAs is empty: it IS the rotation's material, not
	// something a later rotation replaces.
	StagedAs string
}

// RootCAName is the live root CA's basename: RootCAName+".crt" is the
// certificate the cluster pins, RootCAName+".key" its signing key.
const RootCAName = "ca"

// RootTrustBundleName is the root CA's trust bundle: the live root alone at
// rest, old‖new while a rotation is mid-flight. Every trust consumer embeds it
// through ReadRootTrust.
const RootTrustBundleName = RootCAName + "-bundle.crt"

// RootTrustBundlePath is the path of RootTrustBundleName, the file a rotation
// rewrites and the static-pod components point at.
func RootTrustBundlePath() string { return bundleCrtPath(RootCAName) }

const (
	frontProxyCAName     = "front-proxy-ca"
	saPairName           = "sa"
	frontProxyClientName = "front-proxy-client"
)

// trustLayout is the manifest itself. Order is part of the contract: ha
// derives the join bundle's shared and rotation entry lists from it in
// manifest order, and those lists are pinned in order by
// TestSharedCertSetsAreTheJoinContract — an older joiner refuses an entry it
// does not know, so the names are a contract across versions, not an
// implementation detail.
//
// Leaves are deliberately absent: they are minted per node, never travel in
// the join bundle, and LeafSpecs/LeafNames already classify them. A new leaf
// must be adopted deliberately — the node-local list in ha's
// TestEveryPKIFileIsSharedOrNodeLocal is kept literal for exactly that — not
// inherited silently from a shared classification.
var trustLayout = []TrustFile{
	// Shared signers and the one shared leaf: byte-identical on every control
	// plane, in the join bundle on every upload. The etcd CA is REQUIRED, not
	// optional: a joining control plane that does not receive it mints a
	// divergent anchor whose etcd the cluster rejects.
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
	// Trust bundles and staged rotation material: received by a joiner
	// whenever present, so a node joining mid-rotation holds the same
	// in-flight trust state as the rest of the cluster.
	{Name: RootTrustBundleName, CommonName: "stratum-ca", Class: TrustRotation, Rotatable: true},
	{Name: frontProxyCAName + "-bundle.crt", CommonName: frontProxyCAName, Class: TrustRotation, Rotatable: true},
	{Name: saPairName + "-bundle.pub", Class: TrustRotation, Rotatable: true},
	// The etcd trust bundle carries the cluster's CURRENT etcd migration state
	// (etcd-ca alone, or etcd-ca‖root mid-migration) to a joining CP, so it
	// lands on the same anchor set as everyone else instead of guessing.
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
	// Node-local: derived from the shared material on each node, or the
	// rotation's residue until finalize or a rollback removes it.
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

// TrustLayout returns the trust manifest: every non-leaf trust file pki
// writes, classified by name, common name, who holds it, and whether a
// rotation rewrites it. The caller owns the returned slice. Adding a file to
// pki's writers without an entry here leaves it invisible to every consumer
// that derives from the manifest — ha's TestEveryPKIFileIsSharedOrNodeLocal
// fails until it is classified on purpose.
func TrustLayout() []TrustFile { return slices.Clone(trustLayout) }

// StagedTrustName returns the basename a CA rotation stages as the replacement
// of the manifest entry named name — "ca-new.crt" for "ca.crt" — or "" when
// nothing stages a replacement for it.
func StagedTrustName(name string) string {
	for _, f := range trustLayout {
		if f.Name == name {
			return f.StagedAs
		}
	}
	return ""
}

// StagedRootCAName is the basename, without extension, of the root CA pair a
// CA rotation stages in trust-new: StagedTrustName of the root's certificate
// and key, for a reader that loads the pair by its base name.
const StagedRootCAName = RootCAName + "-new"

// PreRotationBackupName returns the basename sign-new backs the live file
// name up to before it promotes the staged replacement over it —
// "ca.pre-carotate.crt" for "ca.crt" — and that rollback restores from.
func PreRotationBackupName(name string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + ".pre-carotate" + ext
}

// ReadRootTrust returns the cluster's root trust material and the path it read
// it from: the root trust bundle (RootTrustBundleName — old‖new mid-rotation)
// when the node has one, else the plain root certificate (RootCAName+".crt")
// on nodes that predate the bundle files. kubeconfig.Write,
// token.ensureClusterInfo and the worker trust refresh embed this material;
// before this reader each spelled the preference for itself.
func ReadRootTrust() ([]byte, string, error) { return ReadRootTrustFrom(version.PKIDir) }

// ReadRootTrustFrom is ReadRootTrust for a PKI directory other than
// version.PKIDir, which the trust-refresh tests lay out in a temporary
// directory. Only an unreadable bundle falls back: a bundle that reads but
// does not parse is returned as it is, because every consumer parses and
// refuses malformed material itself, under its own trust policy.
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
