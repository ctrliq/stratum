// Kubelet serving-cert CSR approver.
//
// A kubelet that runs serverTLSBootstrap requests a `kubernetes.io/kubelet-serving`
// CSR — every kubelet under `stratum server --cis`, and also every token-joined
// worker, which has no local PKI to pin and so bootstraps its serving cert this
// way. Kubernetes deliberately does NOT auto-approve that CSR (unlike
// the kubelet CLIENT CSR), because serving-cert SANs (node IPs/DNS) are spoofable
// and must be validated against the node's real addresses.
// An unapproved kubelet has no serving cert at all, so the apiserver cannot
// complete a TLS handshake with it → cluster-wide kubectl logs/exec/top +
// metrics outage. This reconciler is what makes serverTLSBootstrap
// safe: it approves a serving CSR ONLY when the requester is the node itself and
// every SAN it asks for is an address that node legitimately owns.
package operator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log"
	"slices"
	"strings"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func reconcileServingCSRs(ctx context.Context, cs kubernetes.Interface) {
	list, err := cs.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("stratum-operator: list CSRs: %v", err)
		return
	}
	for i := range list.Items {
		csr := &list.Items[i]
		if csr.Spec.SignerName != certv1.KubeletServingSignerName || csrDecided(csr) {
			continue
		}
		verdict, reason := servingCSRVerdict(ctx, cs, csr)
		switch verdict {
		case approve:
			decideCSR(ctx, cs, csr, certv1.CertificateApproved, "StratumKubeletServingApprove",
				"node identity + SANs validated by stratum-operator")
			log.Printf("stratum-operator: approved kubelet-serving CSR %s (%s)", csr.Name, csr.Spec.Username)
		case deny:
			decideCSR(ctx, cs, csr, certv1.CertificateDenied, "StratumKubeletServingDeny", reason)
			log.Printf("stratum-operator: DENIED kubelet-serving CSR %s (%s): %s", csr.Name, csr.Spec.Username, reason)
		default: // pending — leave it (a startup race self-heals; don't wedge the node)
			log.Printf("stratum-operator: kubelet-serving CSR %s pending: %s", csr.Name, reason)
		}
	}
}

type verdict int

const (
	pending verdict = iota
	approve
	deny
)

func csrDecided(csr *certv1.CertificateSigningRequest) bool {
	if len(csr.Status.Certificate) > 0 {
		return true
	}
	for _, c := range csr.Status.Conditions {
		if c.Type == certv1.CertificateApproved || c.Type == certv1.CertificateDenied {
			return true
		}
	}
	return false
}

// servingCSRVerdict decides a kubelet-serving CSR. It is deliberately strict:
// deny on any positive mismatch (wrong requester, wrong usages, a SAN the node
// doesn't own — the anti-spoofing core), but leave PENDING when we can't yet
// prove validity (Node not registered / addresses not populated), so a kubelet
// that races ahead of its Node.status retries instead of being permanently denied.
func servingCSRVerdict(ctx context.Context, cs kubernetes.Interface, csr *certv1.CertificateSigningRequest) (verdict, string) {
	const nodePrefix = "system:node:"
	if !strings.HasPrefix(csr.Spec.Username, nodePrefix) {
		return deny, "requester " + csr.Spec.Username + " is not a node identity"
	}
	nodeName := strings.TrimPrefix(csr.Spec.Username, nodePrefix)
	if !slices.Contains(csr.Spec.Groups, "system:nodes") {
		return deny, "requester not in system:nodes"
	}
	if !usagesAreServingOnly(csr.Spec.Usages) {
		return deny, "unexpected key usages for a serving cert (client-auth not allowed)"
	}
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return deny, "bad CSR PEM"
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return deny, "unparseable CSR: " + err.Error()
	}
	if req.Subject.CommonName != nodePrefix+nodeName {
		return deny, "CSR CN " + req.Subject.CommonName + " != " + nodePrefix + nodeName
	}
	node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return pending, "node " + nodeName + " not found yet"
	}
	dns, ips := nodeAddresses(node)
	if len(req.DNSNames) == 0 && len(req.IPAddresses) == 0 {
		return pending, "CSR carries no SANs to validate"
	}
	for _, d := range req.DNSNames {
		if !slices.Contains(dns, d) {
			return pending, "DNS SAN " + d + " not (yet) among node addresses"
		}
	}
	for _, ip := range req.IPAddresses {
		if !slices.Contains(ips, ip.String()) {
			return pending, "IP SAN " + ip.String() + " not (yet) among node addresses"
		}
	}
	return approve, ""
}

// usagesAreServingOnly requires server auth and permits only the signing/encipherment
// usages that accompany it — any client-auth (or other) usage under the serving
// signer is rejected.
func usagesAreServingOnly(us []certv1.KeyUsage) bool {
	sawServer := false
	for _, u := range us {
		switch u {
		case certv1.UsageServerAuth:
			sawServer = true
		case certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment:
		default:
			return false
		}
	}
	return sawServer
}

func nodeAddresses(n *corev1.Node) (dns, ips []string) {
	for _, a := range n.Status.Addresses {
		switch a.Type {
		case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
			dns = append(dns, a.Address)
		case corev1.NodeInternalIP, corev1.NodeExternalIP:
			ips = append(ips, a.Address)
		}
	}
	return dns, ips
}

func decideCSR(ctx context.Context, cs kubernetes.Interface, csr *certv1.CertificateSigningRequest, t certv1.RequestConditionType, reason, msg string) {
	csr.Status.Conditions = append(csr.Status.Conditions, certv1.CertificateSigningRequestCondition{
		Type:    t,
		Status:  corev1.ConditionTrue,
		Reason:  reason,
		Message: msg,
	})
	if _, err := cs.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
		log.Printf("stratum-operator: update CSR %s approval: %v", csr.Name, err)
	}
}
