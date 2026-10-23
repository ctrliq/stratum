package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/keyutil"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

// errNoKubeletServingCert marks a kubelet that holds no bootstrap-issued
// serving cert yet. Rotation must stop until one is issued: a kubelet without
// a serving cert cannot satisfy the apiserver's kubelet TLS verification.
var errNoKubeletServingCert = errors.New("the kubelet has no serving cert yet")

// kubeletServingCertPath is a worker's serving pair, in the --cert-dir that
// checkWorkerReissueConfig requires the running kubelet to use.
func kubeletServingCertPath() string {
	return filepath.Join(filepath.Dir(kubeletClientCertPath()), pki.KubeletServingCurrent)
}

// kubeletBootstrapsServing reports whether this node's kubelet gets its serving
// cert through a kubelet-serving CSR rather than a static PKI leaf, which leaf
// rotation already reissues under the new CA. It decides the way the kubelet's
// config was written, and refuses when the node state cannot be read rather
// than guess "static leaf".
var kubeletBootstrapsServing = func() (bool, error) {
	st, err := nodestate.LoadState()
	if err != nil {
		return false, fmt.Errorf("read the node state to tell how the kubelet gets its serving cert: %w", err)
	}
	return runtime.ServingBootstrapped(st, version.PKIDir)
}

// renewKubeletServingCert is renewKubeletClientCert for the kubelet's serving
// cert: it obtains one signed by the newest CA in anchor, for the same node and
// the same SANs, and installs it without removing the current one first. The
// CSR goes to the kubernetes.io/kubelet-serving signer, so the operator's
// approver must approve it. A current cert the new CA already signed is left
// alone and reported as not renewed.
func renewKubeletServingCert(ctx context.Context, certPath string, anchor []*x509.Certificate, ops kubeletRenewOps) (renewed bool, err error) {
	current, err := pki.ReadKubeletServingLeaf(certPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: %s does not exist", errNoKubeletServingCert, certPath)
	}
	if err != nil {
		return false, fmt.Errorf("read the current kubelet serving cert %s: %w", certPath, err)
	}
	if err := checkNodeClientSubject("the current kubelet serving cert "+certPath, current.Subject); err != nil {
		return false, err
	}
	if len(current.DNSNames) == 0 && len(current.IPAddresses) == 0 {
		return false, fmt.Errorf("the current kubelet serving cert %s carries no SANs to request again", certPath)
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
		return false, fmt.Errorf("generate kubelet serving key: %w", err)
	}
	subject := pkix.Name{CommonName: current.Subject.CommonName, Organization: []string{nodesGroup}}
	csrPEM, err := cert.MakeCSR(key, &subject, current.DNSNames, current.IPAddresses)
	if err != nil {
		return false, fmt.Errorf("build kubelet serving CSR: %w", err)
	}
	certPEM, err := ops.request(ctx, csrPEM, key)
	if err != nil {
		return false, fmt.Errorf("request a kubelet serving cert for %s: %w", subject.CommonName, err)
	}
	if err := checkRenewedServingCert(certPEM, key, current, anchor, newCA); err != nil {
		return false, err
	}
	keyPEM, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		return false, fmt.Errorf("encode kubelet serving key: %w", err)
	}
	if err := ops.store(certPEM, keyPEM); err != nil {
		return false, fmt.Errorf("install the renewed kubelet serving cert: %w", err)
	}
	return true, nil
}

// checkRenewedServingCert accepts an issued serving cert only when it is for
// the same node and SANs as current, carries the key this run generated, is
// valid now, and was signed by newCA.
func checkRenewedServingCert(certPEM []byte, key *ecdsa.PrivateKey, current *x509.Certificate, anchor []*x509.Certificate, newCA *x509.Certificate) error {
	issued, err := pki.FirstCertificate(certPEM)
	if err != nil {
		return fmt.Errorf("the issued kubelet serving cert: %w", err)
	}
	if issued.Subject.CommonName != current.Subject.CommonName {
		return fmt.Errorf("the issued kubelet serving cert is for %q, not %q", issued.Subject.CommonName, current.Subject.CommonName)
	}
	if !slices.Equal(issued.DNSNames, current.DNSNames) || !slices.EqualFunc(issued.IPAddresses, current.IPAddresses, net.IP.Equal) {
		return fmt.Errorf("the issued kubelet serving cert names %v %v, not the current cert's %v %v",
			issued.DNSNames, issued.IPAddresses, current.DNSNames, current.IPAddresses)
	}
	if !slices.Contains(issued.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return fmt.Errorf("the issued kubelet serving cert is not valid for server auth")
	}
	pub, ok := issued.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return fmt.Errorf("the issued kubelet serving cert does not carry the key this request generated")
	}
	if now := time.Now(); now.Before(issued.NotBefore) || now.After(issued.NotAfter) {
		return fmt.Errorf("the issued kubelet serving cert is not valid now (valid %s to %s)", issued.NotBefore.UTC().Format(time.RFC3339), issued.NotAfter.UTC().Format(time.RFC3339))
	}
	if issued.CheckSignatureFrom(newCA) == nil {
		return nil
	}
	if pki.SignedByAny(issued, anchor) {
		return fmt.Errorf("the issued kubelet serving cert was signed by the CA this rotation replaces, not the new one (%s): "+
			"finish --phase=%s on every control plane, so every controller-manager signs with the new CA", describe([]*x509.Certificate{newCA}), PhaseSignNew)
	}
	return fmt.Errorf("the issued kubelet serving cert was signed by a CA this node does not trust (issuer %q)", issued.Issuer.String())
}

// requestServingCert is requestKubeletServingCert, a var so a test can stand
// in for the operator's approval and the controller-manager's signature.
var requestServingCert = requestKubeletServingCert

// requestKubeletServingCert sends csrPEM to the kubelet-serving signer as the
// kubelet, using kubeconfigPath's client cert and trust.
func requestKubeletServingCert(kubeconfigPath string) kubeletRequest {
	return requestKubeletCert(kubeconfigPath, certificatesv1.KubeletServingSignerName,
		[]certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageServerAuth})
}

// storeKubeletServingCert installs the pair the way the kubelet's certificate
// manager does: a timestamped kubelet-server-*.pem, then a swap of the
// kubelet-server-current.pem symlink.
func storeKubeletServingCert(certPath string) func(certPEM, keyPEM []byte) error {
	return func(certPEM, keyPEM []byte) error {
		dir := filepath.Dir(certPath)
		store, err := certificate.NewFileStore("kubelet-server", dir, dir, "", "")
		if err != nil {
			return err
		}
		_, err = store.Update(certPEM, keyPEM)
		return err
	}
}

// renewServingCert renews this node's bootstrap-issued kubelet serving cert
// under the newest CA in anchor, authenticating as the kubelet through
// kubeconfigPath, and reports whether it installed a new one. A node whose
// kubelet pins a static serving leaf has nothing to renew. A bootstrap kubelet
// with no serving cert refuses the phase. A failure leaves the current cert in
// place.
func renewServingCert(w io.Writer, o CARotateOptions, phase, kubeconfigPath, certPath string, anchor []*x509.Certificate) (bool, error) {
	bootstraps, err := kubeletBootstrapsServing()
	if err != nil || !bootstraps {
		return false, err
	}
	castep(w, o, "Renew kubelet serving cert under the new CA")
	ctx, cancel := context.WithTimeout(context.Background(), kubeletRenewTimeout)
	defer cancel()
	renewed, err := renewKubeletServingCert(ctx, certPath, anchor, kubeletRenewOps{
		request: requestServingCert(kubeconfigPath),
		store:   storeKubeletServingCert(certPath),
	})
	switch {
	case errors.Is(err, errNoKubeletServingCert):
		return false, fmt.Errorf("%w — a kubelet-serving CSR must be approved before %s can continue; start the stratum operator or approve the CSR manually, then retry this phase", err, phase)
	case err != nil:
		return false, fmt.Errorf("%w — the kubelet keeps its current serving cert. The stratum operator approves kubelet-serving CSRs: "+
			"check it is running (spec.operator: false turns it off) and why `stratum kubectl get csr` shows the request pending or denied "+
			"(a SAN the node no longer reports among its addresses stays pending), then run `stratum certs rotate-ca --phase=%s --yes` on this node again",
			err, phase)
	case !renewed:
		castep(w, o, "The kubelet serving cert is already signed by the new CA; nothing to renew")
	}
	return renewed, nil
}

// checkServingCertSurvives refuses a finalize that would drop the CA the
// kubelet serving cert at path was signed by: the apiservers would then reject
// it, and kubectl logs, exec and port-forward to this node would fail. A
// missing or unreadable cert also refuses: the worker cannot prove its kubelet
// will serve a trusted certificate after finalize restarts it.
func checkServingCertSurvives(dropped []*x509.Certificate, path string) error {
	if len(dropped) == 0 {
		return nil
	}
	leaf, err := pki.ReadKubeletServingLeaf(path)
	if err != nil {
		return fmt.Errorf("cannot verify the kubelet serving cert %s before finalize drops old CA trust: %w — restore a valid serving cert and rerun --phase=%s before retrying finalize", path, err, PhaseSignNew)
	}
	for _, ca := range dropped {
		if leaf.CheckSignatureFrom(ca) == nil {
			return fmt.Errorf("the kubelet serving cert %s is still signed by a CA this finalize drops (%s), so the apiservers would refuse it and kubectl logs/exec to this node would fail — "+
				"run --phase=%s first, or `stratum certs rotate-ca --rollback --yes`", path, describe([]*x509.Certificate{ca}), PhaseSignNew)
		}
	}
	return nil
}

// probeKubelet is pki.ProbeKubeletServing, a var so a test can stand in for the
// kubelets.
var probeKubelet pki.KubeletProbe = pki.ProbeKubeletServing

// checkKubeletServingTrusted is the kubelet arm of FinalizePreflight. When the
// cluster's profile bootstraps control-plane serving certs (cpBootstrap) the
// control planes are skipped:
// their kubelets bootstrap their serving certs and renew them in their own
// finalize, since before then not every controller-manager signs with the new
// CA. A worker whose kubelet cannot be checked is reported but does not
// refuse, since its own finalize checks its cert on disk; a control plane has
// no such check and a kubelet that restarted best-effort may still serve the
// old-CA cert, so one that cannot be checked refuses.
func checkKubeletServingTrusted(ctx context.Context, w io.Writer, nodes []corev1.Node, cpBootstrap bool, newCA *x509.Certificate, probe pki.KubeletProbe) error {
	var targets []pki.KubeletTarget
	var probed []*corev1.Node
	for i := range nodes {
		n := &nodes[i]
		if cpBootstrap && isControlPlane(n) {
			continue
		}
		targets = append(targets, pki.KubeletTarget{Name: n.Name, IP: nodeInternalIP(n)})
		probed = append(probed, n)
	}
	results := pki.ProbeKubelets(ctx, targets, []*x509.Certificate{newCA}, probe)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the kubelet serving-cert check was interrupted: %w", err)
	}
	for i, err := range results {
		n := probed[i]
		switch {
		case errors.Is(err, pki.ErrKubeletServingUntrusted):
			return fmt.Errorf("node %s serves a kubelet cert the new CA (%s) did not sign, so finalize would break kubectl logs/exec to it — %s",
				n.Name, describe([]*x509.Certificate{newCA}), untrustedServingRemedy(n))
		case err != nil && isControlPlane(n):
			return fmt.Errorf("could not check that the kubelet on control plane %s serves a cert the new CA (%s) signed (%v), and nothing else checks it "+
				"before finalize drops the old CA, after which an old-CA cert would break kubectl logs/exec to it — make its kubelet reachable on its InternalIP at :%d; "+
				"if it is, %s", n.Name, describe([]*x509.Certificate{newCA}), err, version.KubeletPort, untrustedServingRemedy(n))
		case errors.Is(err, pki.ErrNoKubeletAddress):
			fmt.Fprintf(w, "  WARN: node %s reports no InternalIP; its kubelet serving cert was not checked\n", n.Name)
		case err != nil:
			fmt.Fprintf(w, "  WARN: could not check the kubelet serving cert on node %s: %v\n", n.Name, err)
		}
	}
	return nil
}

func isControlPlane(n *corev1.Node) bool { return n.Labels[k8sutil.ControlPlaneLabel] == "true" }

// untrustedServingRemedy is what clears an old-CA serving cert on n. A worker
// renews it at sign-new. A control plane's kubelet bootstraps it only under a
// profile that declares bootstrap serving certs, which renews it in finalize;
// otherwise it serves the static kubelet.crt leaf that sign-new reissued, and a
// kubelet that did not restart onto it at sign-new needs a restart, since sign-new cannot run again once it promoted.
func untrustedServingRemedy(n *corev1.Node) string {
	if isControlPlane(n) {
		return "restart Stratum on that control plane (`sudo systemctl restart stratum`) so its kubelet serves its new-CA cert, then retry finalize"
	}
	return fmt.Sprintf("run `stratum certs rotate-ca --phase=%s --yes` on that node again, then retry finalize", PhaseSignNew)
}
