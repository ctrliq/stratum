package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/certificate/csr"
	"k8s.io/client-go/util/keyutil"

	"github.com/ctrliq/stratum/internal/pki"
)

// kubeletRenewTimeout bounds the CSR round trip: create, auto-approval by the
// controller-manager, and signing.
var kubeletRenewTimeout = 3 * time.Minute

// errNoKubeletClientCert marks a kubelet with no client cert to renew with:
// none on disk, or none in the file. sign-new authenticates the renewal with
// that cert, so it cannot repair such a worker; rejoinWorker says what can.
var errNoKubeletClientCert = errors.New("the kubelet has no usable client cert")

const rejoinWorker = "remove this worker (`stratum uninstall`) and join it again with a token from `stratum token create`"

// kubeletRenewOps are the side effects of renewKubeletClientCert and
// renewKubeletServingCert: request sends a CSR and returns the issued
// certificate PEM, store installs a certificate and key as the kubelet's
// current credential of that kind, client or serving.
type kubeletRenewOps struct {
	request kubeletRequest
	store   func(certPEM, keyPEM []byte) error
}

// renewKubeletClientCert obtains a client certificate under the new CA for the
// node identity in certPath and installs it, without ever removing the current
// one first. The CSR is authenticated by the current certificate, which the
// apiservers still accept until finalize, and approved through the
// selfnodeclient binding, so no bootstrap token is involved. The new CA is the
// trust anchor's newest (newestCA). A current certificate it already signed is
// left as it is and reported as not renewed, so a sign-new that renewed and then
// failed to restart the kubelet can be run again. Every check on the issued
// certificate runs before store, so a refusal leaves the kubelet's credential as
// it was.
func renewKubeletClientCert(ctx context.Context, certPath string, anchor []*x509.Certificate, ops kubeletRenewOps) (renewed bool, err error) {
	current, err := readKubeletClientLeaf(certPath)
	if err != nil {
		return false, err
	}
	if err := checkNodeClientSubject("the current kubelet client cert "+certPath, current.Subject); err != nil {
		return false, err
	}
	newCA, err := newestCA(anchor)
	if err != nil {
		return false, err
	}
	if current.CheckSignatureFrom(newCA) == nil {
		return false, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, fmt.Errorf("generate kubelet client key: %w", err)
	}
	subject := pkix.Name{CommonName: current.Subject.CommonName, Organization: []string{nodesGroup}}
	csrPEM, err := cert.MakeCSR(key, &subject, nil, nil)
	if err != nil {
		return false, fmt.Errorf("build kubelet client CSR: %w", err)
	}
	certPEM, err := ops.request(ctx, csrPEM, key)
	if err != nil {
		return false, fmt.Errorf("request a kubelet client cert for %s: %w", subject.CommonName, err)
	}
	if err := checkRenewedClientCert(certPEM, key, subject.CommonName, anchor, newCA); err != nil {
		return false, err
	}
	keyPEM, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		return false, fmt.Errorf("encode kubelet client key: %w", err)
	}
	if err := ops.store(certPEM, keyPEM); err != nil {
		return false, fmt.Errorf("install the renewed kubelet client cert: %w", err)
	}
	return true, nil
}

// newestCA is the CA in the trust anchor that this rotation introduced: the one
// whose validity starts last, since trust-new mints it after the CA it
// replaces. The bundle's order is not relied on, as a worker's comes from
// cluster-info. Two CAs that start in the same second cannot be told apart and
// are refused.
func newestCA(anchor []*x509.Certificate) (*x509.Certificate, error) {
	var newest *x509.Certificate
	tied := false
	for _, ca := range anchor {
		switch {
		case newest == nil || ca.NotBefore.After(newest.NotBefore):
			newest, tied = ca, false
		case ca.NotBefore.Equal(newest.NotBefore) && !ca.Equal(newest):
			tied = true
		}
	}
	if newest == nil {
		return nil, fmt.Errorf("the trust anchor holds no CA")
	}
	if tied {
		return nil, fmt.Errorf("cannot tell which CA in the trust anchor is the new one: more than one starts at %s", newest.NotBefore.UTC().Format(time.RFC3339))
	}
	return newest, nil
}

const (
	nodeUserPrefix = "system:node:"
	nodesGroup     = "system:nodes"
)

func checkNodeClientSubject(what string, s pkix.Name) error {
	if !strings.HasPrefix(s.CommonName, nodeUserPrefix) || len(s.CommonName) == len(nodeUserPrefix) ||
		len(s.Organization) != 1 || s.Organization[0] != nodesGroup {
		return fmt.Errorf("%s is for %q in %v, not a node identity (%s<name> in %s)", what, s.CommonName, s.Organization, nodeUserPrefix, nodesGroup)
	}
	return nil
}

// checkRenewedClientCert accepts an issued certificate only when it is for the
// same node, carries the key this run generated, is valid now, and was signed
// by newCA. One signed by another CA in the trust anchor is from the CA this
// rotation replaces: finalize would drop it and lock the kubelet out exactly as
// the old certificate would.
func checkRenewedClientCert(certPEM []byte, key *ecdsa.PrivateKey, cn string, anchor []*x509.Certificate, newCA *x509.Certificate) error {
	issued, err := pki.FirstCertificate(certPEM)
	if err != nil {
		return fmt.Errorf("the issued kubelet client cert: %w", err)
	}
	if issued.Subject.CommonName != cn {
		return fmt.Errorf("the issued kubelet client cert is for %q, not %q", issued.Subject.CommonName, cn)
	}
	if err := checkNodeClientSubject("the issued kubelet client cert", issued.Subject); err != nil {
		return err
	}
	pub, ok := issued.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return fmt.Errorf("the issued kubelet client cert does not carry the key this request generated")
	}
	if now := time.Now(); now.Before(issued.NotBefore) || now.After(issued.NotAfter) {
		return fmt.Errorf("the issued kubelet client cert is not valid now (valid %s to %s)", issued.NotBefore.UTC().Format(time.RFC3339), issued.NotAfter.UTC().Format(time.RFC3339))
	}
	if issued.CheckSignatureFrom(newCA) == nil {
		return nil
	}
	for _, ca := range anchor {
		if issued.CheckSignatureFrom(ca) == nil {
			return fmt.Errorf("the issued kubelet client cert was signed by the CA this rotation replaces (%s), not the new one (%s): "+
				"finish --phase=%s on every control plane, so every controller-manager signs with the new CA",
				describe([]*x509.Certificate{ca}), describe([]*x509.Certificate{newCA}), PhaseSignNew)
		}
	}
	return fmt.Errorf("the issued kubelet client cert was signed by a CA this worker does not trust (issuer %q)", issued.Issuer.String())
}

func readKubeletClientLeaf(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", errNoKubeletClientCert, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read the current kubelet client cert %s (it authenticates the renewal): %w", path, err)
	}
	leaf, err := pki.FirstCertificate(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errNoKubeletClientCert, path, err)
	}
	return leaf, nil
}

// kubeletRequest sends a CSR and returns the issued certificate PEM.
type kubeletRequest = func(ctx context.Context, csrPEM []byte, key crypto.Signer) ([]byte, error)

// requestKubeletClientCert sends csrPEM as the kubelet, using kubeconfigPath's
// client cert and trust, and waits for the issued certificate.
func requestKubeletClientCert(kubeconfigPath string) kubeletRequest {
	return requestKubeletCert(kubeconfigPath, certificatesv1.KubeAPIServerClientKubeletSignerName,
		[]certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth})
}

func requestKubeletCert(kubeconfigPath, signerName string, usages []certificatesv1.KeyUsage) kubeletRequest {
	return func(ctx context.Context, csrPEM []byte, key crypto.Signer) ([]byte, error) {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", kubeconfigPath, err)
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		name, uid, err := csr.RequestCertificateWithContext(ctx, cs, csrPEM, "", signerName, nil, usages, key)
		if err != nil {
			return nil, fmt.Errorf("create CSR: %w", err)
		}
		certPEM, err := csr.WaitForCertificate(ctx, cs, name, uid)
		if err != nil {
			return nil, fmt.Errorf("CSR %s was not approved and signed: %w%s", name, err, abandonCSR(cs, name))
		}
		return certPEM, nil
	}
}

// storeKubeletClientCert writes the pair the way the kubelet's own rotation
// does: a new timestamped kubelet-client-*.pem, then a swap of the
// kubelet-client-current.pem symlink. The previous file stays on disk.
func storeKubeletClientCert(certPath string) func(certPEM, keyPEM []byte) error {
	return func(certPEM, keyPEM []byte) error {
		dir := filepath.Dir(certPath)
		store, err := certificate.NewFileStore("kubelet-client", dir, dir, "", "")
		if err != nil {
			return err
		}
		_, err = store.Update(certPEM, keyPEM)
		return err
	}
}

// abandonCSR deletes a CSR the wait gave up on, since one left pending blocks
// FinalizePreflight until kube-controller-manager collects it a day later. The
// kubelet's own credentials may not delete it, so a failure only names it.
func abandonCSR(cs kubernetes.Interface, name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cs.CertificatesV1().CertificateSigningRequests().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Sprintf(" (it is still pending and blocks finalize until deleted: `stratum kubectl delete csr %s`)", name)
	}
	return " (the pending CSR was deleted)"
}
