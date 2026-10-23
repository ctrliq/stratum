package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// KubeletServingCurrent is the kubelet's bootstrap-issued serving pair, kept by
// its certificate manager as one cert-and-key file in the kubelet's cert dir.
const KubeletServingCurrent = "kubelet-server-current.pem"

// kubeletDefaultCertDir is the kubelet's --cert-dir default. It is a fixed
// path, not derived from --root-dir.
const kubeletDefaultCertDir = "/var/lib/kubelet/pki"

// KubeletCertDir is the cert dir a kubelet launched with args keeps its
// certificate manager's pairs in: --cert-dir, else the kubelet's default. A
// repeated flag resolves to its last value, as the kubelet's flag parser does.
func KubeletCertDir(args []string) string {
	certDir := kubeletDefaultCertDir
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--cert-dir="); ok {
			certDir = v
		} else if a == "--cert-dir" && i+1 < len(args) {
			certDir = args[i+1]
		}
	}
	return certDir
}

// KubeletServingRoots is the trust the apiserver verifies kubelet serving certs
// against (--kubelet-certificate-authority): this node's ca-bundle.crt.
func KubeletServingRoots() ([]*x509.Certificate, error) {
	return bundleCerts(bundleCrtPath("ca"))
}

// ReadKubeletServingLeaf reads the certificate of the serving pair at path. An
// absent file is reported as os.ErrNotExist.
func ReadKubeletServingLeaf(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	leaf, err := FirstCertificate(b)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", path, err)
	}
	return leaf, nil
}

// FirstCertificate parses the first CERTIFICATE block in b, skipping any other
// PEM block before it, such as the private key of a cert-and-key file.
func FirstCertificate(b []byte) (*x509.Certificate, error) {
	for {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			return nil, errors.New("no PEM certificate")
		}
		if blk.Type == "CERTIFICATE" {
			return x509.ParseCertificate(blk.Bytes)
		}
	}
}

// SignedByAny reports whether one of cas signed leaf.
func SignedByAny(leaf *x509.Certificate, cas []*x509.Certificate) bool {
	for _, ca := range cas {
		if leaf.CheckSignatureFrom(ca) == nil {
			return true
		}
	}
	return false
}

// HealKubeletServingCert moves a bootstrap-issued kubelet serving cert that no
// CA in roots signed out of the kubelet's way, so the kubelet requests a new
// one from the current signer when it next starts. The apiserver would refuse
// that cert anyway, so moving it loses nothing. Anything it cannot judge, an
// absent or unparseable cert or no roots, is left alone. It returns where the
// cert went, or "" when it moved nothing.
func HealKubeletServingCert(certDir string, roots []*x509.Certificate) (string, error) {
	if len(roots) == 0 {
		return "", nil
	}
	current := filepath.Join(certDir, KubeletServingCurrent)
	leaf, err := ReadKubeletServingLeaf(current)
	if err != nil {
		return "", nil
	}
	if SignedByAny(leaf, roots) {
		return "", nil
	}
	aside := fmt.Sprintf("%s.untrusted-%d", current, time.Now().Unix())
	if err := os.Rename(current, aside); err != nil {
		return "", fmt.Errorf("pki: move the untrusted kubelet serving cert %s aside: %w", current, err)
	}
	return aside, nil
}

// ErrKubeletServingUntrusted marks a kubelet that answered with a serving
// certificate no CA in the roots signed, as opposed to one that did not answer
// or whose certificate failed verification for another reason.
var ErrKubeletServingUntrusted = errors.New("the kubelet serves a certificate none of the trusted CAs signed")

// ProbeKubeletServing TLS-dials the kubelet serving endpoint at addr (host:port)
// and verifies the certificate it presents against roots for that host, as the
// apiserver does under --kubelet-certificate-authority. It presents no client
// certificate and sends nothing past the handshake.
func ProbeKubeletServing(ctx context.Context, addr string, roots []*x509.Certificate) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	for _, ca := range roots {
		pool.AddCert(ca)
	}
	d := tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			return fmt.Errorf("%s: %w: %w", addr, ErrKubeletServingUntrusted, err)
		}
		return fmt.Errorf("probe the kubelet at %s: %w", addr, err)
	}
	return conn.Close()
}

// KubeletProbe is ProbeKubeletServing's shape, so a caller can stand in for it.
type KubeletProbe func(ctx context.Context, addr string, roots []*x509.Certificate) error

// KubeletTarget is a node whose kubelet serving endpoint ProbeKubelets dials.
type KubeletTarget struct {
	Name, IP string
}

// ErrNoKubeletAddress marks a node that reports no address to probe its kubelet at.
var ErrNoKubeletAddress = errors.New("the node reports no InternalIP to reach its kubelet at")

// kubeletProbeTimeout bounds one probe; ProbeKubelets runs kubeletProbeWidth at once.
const (
	kubeletProbeTimeout = 5 * time.Second
	kubeletProbeWidth   = 16
)

// ProbeKubelets probes each target's kubelet at IP:KubeletPort against roots
// and returns one result per target, in order: nil when the kubelet serves a
// cert roots verify for that IP, an error wrapping ErrKubeletServingUntrusted
// when none of roots signed it, ErrNoKubeletAddress for a target with no IP,
// and any other error for a kubelet it could not verify.
func ProbeKubelets(ctx context.Context, targets []KubeletTarget, roots []*x509.Certificate, probe KubeletProbe) []error {
	results := make([]error, len(targets))
	sem := make(chan struct{}, kubeletProbeWidth)
	var wg sync.WaitGroup
	for i, t := range targets {
		if t.IP == "" {
			results[i] = ErrNoKubeletAddress
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, kubeletProbeTimeout)
			defer cancel()
			results[i] = probe(pctx, net.JoinHostPort(t.IP, strconv.Itoa(version.KubeletPort)), roots)
		})
	}
	wg.Wait()
	return results
}
