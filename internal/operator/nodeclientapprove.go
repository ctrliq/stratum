// Kubelet client-cert CSR approver for bootstrap-token joins: a join token may
// claim a new node name, never one already registered or claimed by a certificate.
package operator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

const (
	bootstrapUserPrefix = "system:bootstrap:"
	bootstrappersGroup  = "system:bootstrappers"
	nodeUserPrefix      = "system:node:"
	nodesGroup          = "system:nodes"
)

// nodeClientPassTimeout bounds one approval pass below the CSR loop's 30s tick,
// so a slow or flooded apiserver cannot hold the serving approver behind it.
const nodeClientPassTimeout = 25 * time.Second

// The annotations a pending bootstrap CSR carries, so `kubectl describe csr`
// shows which node it asks for and why it waits.
const (
	nodeClientPendingAnnotation   = "stratum.ciq.com/node-client-pending"
	nodeClientRequestedAnnotation = "stratum.ciq.com/requested-node"
)

func reconcileNodeClientCSRs(parent context.Context, cs kubernetes.Interface, now time.Time) {
	ctx, cancel := context.WithTimeout(parent, nodeClientPassTimeout)
	defer cancel()
	if err := k8sutil.DisableNodeClientAutoApprove(ctx, cs.RbacV1().ClusterRoleBindings()); err != nil {
		log.Printf("stratum-operator: disable upstream node-client auto-approval: %v", err)
		return
	}
	list, err := cs.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{
		FieldSelector: "spec.signerName=" + certv1.KubeAPIServerClientKubeletSignerName,
	})
	if err != nil {
		log.Printf("stratum-operator: list CSRs: %v", err)
		return
	}
	p, err := newNodeClientPass(ctx, cs, now)
	if err != nil {
		log.Printf("stratum-operator: node-client approval pass: %v", err)
		return
	}
	var undecided []*certv1.CertificateSigningRequest
	for i := range list.Items {
		csr := &list.Items[i]
		if !bootstrapNodeClientCSR(csr) {
			continue
		}
		name, named := requestedNodeName(csr)
		if csrDecided(csr) {
			if !csrApproved(csr) || !named {
				continue
			}
			if err := p.holdApproved(ctx, csr, name); err != nil {
				log.Printf("stratum-operator: reserve approved CSR %s node name: %v", csr.Name, err)
				return
			}
			p.claims[name]++
			continue
		}
		undecided = append(undecided, csr)
		if named {
			p.claims[name]++
		}
	}
	for _, csr := range undecided {
		name, _ := requestedNodeName(csr)
		verdict, reason := p.verdict(ctx, csr)
		if verdict == approve {
			owned, err := reserveNodeClientName(ctx, cs, csr, name)
			switch {
			case err != nil:
				verdict, reason = pending, "reserve node name "+name+": "+err.Error()
			case !owned:
				verdict, reason = pending, "node "+name+" was claimed by another certificate in the meantime"
			}
		}
		switch verdict {
		case approve:
			decideCSR(ctx, cs, csr, certv1.CertificateApproved, "StratumNodeClientApprove",
				"bootstrap node-client CSR for an unregistered node name, validated by stratum-operator")
			log.Printf("stratum-operator: approved node-client CSR %s: %s -> node %s", csr.Name, csr.Spec.Username, name)
		case deny:
			decideCSR(ctx, cs, csr, certv1.CertificateDenied, "StratumNodeClientDeny", reason)
			log.Printf("stratum-operator: DENIED node-client CSR %s (%s): %s", csr.Name, csr.Spec.Username, reason)
		default:
			recordNodeClientPending(ctx, cs, csr, name, reason)
		}
	}
}

// nodeClientPass is what one approval pass reads once: the registered Nodes,
// the persistent name claims, and each join token's verdict.
type nodeClientPass struct {
	cs         kubernetes.Interface
	now        time.Time
	claims     map[string]int
	registered map[string]bool
	owners     map[string]string
	tokens     map[string]tokenVerdict
}

type tokenVerdict struct {
	verdict verdict
	reason  string
}

// newNodeClientPass reads the Nodes and the claims, and claims every
// registered Node's name that holds none yet.
func newNodeClientPass(ctx context.Context, cs kubernetes.Interface, now time.Time) (*nodeClientPass, error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	owners, err := nodeClientClaims(ctx, cs)
	if err != nil {
		return nil, fmt.Errorf("list node-name claims: %w", err)
	}
	p := &nodeClientPass{cs: cs, now: now, claims: map[string]int{}, registered: map[string]bool{},
		owners: owners, tokens: map[string]tokenVerdict{}}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		p.registered[n.Name] = true
		if _, ok := owners[n.Name]; ok {
			continue
		}
		if _, err := reserveNodeClaim(ctx, cs, n.Name, registeredNodeOwner(n), n.Name); err != nil {
			return nil, fmt.Errorf("claim registered node %s: %w", n.Name, err)
		}
		owners[n.Name] = registeredNodeOwner(n)
	}
	return p, nil
}

// holdApproved records the claim of an approved bootstrap CSR whose name holds
// none: one approved before claims existed, or one an administrator approved
// after deleting the old claim.
func (p *nodeClientPass) holdApproved(ctx context.Context, csr *certv1.CertificateSigningRequest, name string) error {
	if _, ok := p.owners[name]; ok {
		return nil
	}
	if _, err := reserveNodeClientName(ctx, p.cs, csr, name); err != nil {
		return err
	}
	p.owners[name] = string(csr.UID)
	return nil
}

// recordNodeClientPending writes the node a pending CSR asks for and why it
// waits onto the CSR, and logs it, only when either changed.
func recordNodeClientPending(ctx context.Context, cs kubernetes.Interface, csr *certv1.CertificateSigningRequest, name, reason string) {
	if csr.Annotations[nodeClientPendingAnnotation] == reason && csr.Annotations[nodeClientRequestedAnnotation] == name {
		return
	}
	log.Printf("stratum-operator: node-client CSR %s (%s) for node %q pending: %s", csr.Name, csr.Spec.Username, name, reason)
	upd := csr.DeepCopy()
	if upd.Annotations == nil {
		upd.Annotations = map[string]string{}
	}
	upd.Annotations[nodeClientPendingAnnotation] = reason
	upd.Annotations[nodeClientRequestedAnnotation] = name
	if _, err := cs.CertificatesV1().CertificateSigningRequests().Update(ctx, upd, metav1.UpdateOptions{}); err != nil {
		log.Printf("stratum-operator: record why node-client CSR %s is pending: %v", csr.Name, err)
	}
}

func csrApproved(csr *certv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certv1.CertificateDenied || c.Type == certv1.CertificateFailed {
			return false
		}
	}
	for _, c := range csr.Status.Conditions {
		if c.Type == certv1.CertificateApproved {
			return true
		}
	}
	return false
}

// bootstrapNodeClientCSR excludes renewals by an existing node, which
// stratum-csr-renewal leaves to the upstream approver.
func bootstrapNodeClientCSR(csr *certv1.CertificateSigningRequest) bool {
	return csr.Spec.SignerName == certv1.KubeAPIServerClientKubeletSignerName &&
		strings.HasPrefix(csr.Spec.Username, bootstrapUserPrefix) &&
		slices.Contains(csr.Spec.Groups, bootstrappersGroup)
}

func requestedNodeName(csr *certv1.CertificateSigningRequest) (string, bool) {
	req, err := parseCSR(csr)
	if err != nil || !strings.HasPrefix(req.Subject.CommonName, nodeUserPrefix) {
		return "", false
	}
	return strings.TrimPrefix(req.Subject.CommonName, nodeUserPrefix), true
}

func parseCSR(csr *certv1.CertificateSigningRequest) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return nil, errors.New("bad CSR PEM")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

// verdict leaves a claimed name PENDING rather than denied. An administrator
// may approve a verified same-name replacement explicitly.
func (p *nodeClientPass) verdict(ctx context.Context, csr *certv1.CertificateSigningRequest) (verdict, string) {
	if !usagesAreNodeClientOnly(csr.Spec.Usages) {
		return deny, "unexpected key usages for a kubelet client cert"
	}
	req, err := parseCSR(csr)
	if err != nil {
		return deny, "unparseable CSR: " + err.Error()
	}
	cn := req.Subject.CommonName
	nodeName := strings.TrimPrefix(cn, nodeUserPrefix)
	if !strings.HasPrefix(cn, nodeUserPrefix) || nodeName == "" {
		return deny, fmt.Sprintf("CSR CN %q is not a node identity", cn)
	}
	if !slices.Equal(req.Subject.Organization, []string{nodesGroup}) {
		return deny, "CSR organization must be exactly " + nodesGroup
	}
	if len(req.DNSNames)+len(req.IPAddresses)+len(req.EmailAddresses)+len(req.URIs) > 0 {
		return deny, "a kubelet client CSR carries no SANs"
	}
	if !slices.Contains(csr.Spec.Groups, k8sutil.BootstrapGroup) {
		return deny, "the requester is not a stratum join token (no " + k8sutil.BootstrapGroup + " group)"
	}
	if v, reason := p.token(ctx, strings.TrimPrefix(csr.Spec.Username, bootstrapUserPrefix)); v != approve {
		return v, reason
	}
	if p.claims[nodeName] > 1 {
		return pending, "another bootstrap CSR claims node " + nodeName
	}
	if p.registered[nodeName] {
		return pending, "node " + nodeName + " is already registered; a join token cannot take over its identity " +
			"(approve this CSR by hand if it comes from that node being re-provisioned)"
	}
	if owner, ok := p.owners[nodeName]; ok && owner != string(csr.UID) {
		return pending, "node name " + nodeName + " is held by an earlier certificate (ConfigMap kube-system/" +
			nodeClientClaimName(nodeName) + "); delete that claim and approve this CSR by hand if the old credential is unusable"
	}
	return approve, ""
}

func (p *nodeClientPass) token(ctx context.Context, id string) (verdict, string) {
	if t, ok := p.tokens[id]; ok {
		return t.verdict, t.reason
	}
	v, reason := bootstrapTokenVerdict(ctx, p.cs, id, p.now)
	p.tokens[id] = tokenVerdict{v, reason}
	return v, reason
}

// bootstrapTokenVerdict denies a CSR whose join token has since been deleted,
// disabled or has expired, or was never a stratum join token, so a request
// left pending cannot outlive its credential.
func bootstrapTokenVerdict(ctx context.Context, cs kubernetes.Interface, id string, now time.Time) (verdict, string) {
	sec, err := cs.CoreV1().Secrets(metav1.NamespaceSystem).Get(ctx, "bootstrap-token-"+id, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return deny, "join token " + id + " no longer exists"
	case err != nil:
		return pending, "look up join token " + id + ": " + err.Error()
	}
	if sec.Type != corev1.SecretTypeBootstrapToken ||
		string(sec.Data["token-id"]) != id || len(sec.Data["token-secret"]) == 0 ||
		string(sec.Data["usage-bootstrap-authentication"]) != "true" {
		return deny, "join token " + id + " is no longer enabled for bootstrap authentication"
	}
	if !slices.Contains(strings.Split(string(sec.Data["auth-extra-groups"]), ","), k8sutil.BootstrapGroup) {
		return deny, "join token " + id + " is not a stratum join token"
	}
	raw := string(sec.Data["expiration"])
	if raw == "" {
		return approve, ""
	}
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return deny, "join token " + id + " has an unparseable expiration"
	}
	if !now.Before(exp) {
		return deny, "join token " + id + " has expired"
	}
	return approve, ""
}

// usagesAreNodeClientOnly accepts exactly the usage sets the
// kube-apiserver-client-kubelet signer signs: {digital signature, client auth}
// and {digital signature, key encipherment, client auth}, repeats ignored. Any
// other set would be approved and then never signed, so it is denied here.
func usagesAreNodeClientOnly(us []certv1.KeyUsage) bool {
	var sig, client bool
	for _, u := range us {
		switch u {
		case certv1.UsageDigitalSignature:
			sig = true
		case certv1.UsageKeyEncipherment:
		case certv1.UsageClientAuth:
			client = true
		default:
			return false
		}
	}
	return sig && client
}
