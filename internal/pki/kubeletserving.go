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

// The kubelet's bootstrap-issued serving pair, one cert-and-key file in its cert dir.
const KubeletServingCurrent = "kubelet-server-current.pem"

// The kubelet's --cert-dir default; a fixed path, not derived from --root-dir.
const kubeletDefaultCertDir = "/var/lib/kubelet/pki"

// --cert-dir from args, else the default; a repeated flag resolves to its last value.
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

// What the apiserver verifies kubelet serving certs against: this node's ca-bundle.crt.
func KubeletServingRoots() ([]*x509.Certificate, error) {
	return bundleCerts(bundleCrtPath("ca"))
}

// Reads the serving pair's certificate; an absent file is os.ErrNotExist.
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

// Parses the first CERTIFICATE block in b, skipping earlier PEM blocks such as a private key.
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

// Moves a serving cert no CA in roots signed aside; the apiserver would refuse it anyway.
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

// Marks a kubelet that answered with a serving cert none of the roots signed.
var ErrKubeletServingUntrusted = errors.New("the kubelet serves a certificate none of the trusted CAs signed")

// TLS-dials the kubelet serving endpoint and verifies its cert, as the apiserver does.
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

// Bounds one probe; ProbeKubelets runs kubeletProbeWidth at once.
const (
	kubeletProbeTimeout = 5 * time.Second
	kubeletProbeWidth   = 16
)

// Probes each target's kubelet at IP:KubeletPort: nil when roots verify it, else the error.
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
