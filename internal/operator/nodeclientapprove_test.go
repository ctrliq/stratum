package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

func nodeClientPEM(t *testing.T, cn string, orgs, dns []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn, Organization: orgs}, DNSNames: dns}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func joinToken(id string, expires time.Time) *corev1.Secret {
	data := map[string][]byte{
		"token-id": []byte(id), "token-secret": []byte("0123456789abcdef"),
		"usage-bootstrap-authentication": []byte("true"),
		"auth-extra-groups":              []byte(k8sutil.BootstrapGroup),
	}
	if !expires.IsZero() {
		data["expiration"] = []byte(expires.UTC().Format(time.RFC3339))
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + id, Namespace: metav1.NamespaceSystem},
		Type:       corev1.SecretTypeBootstrapToken,
		Data:       data,
	}
}

var nodeClientUsages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageClientAuth}

func bootstrapCSR(name, user string, usages []certv1.KeyUsage, req []byte) *certv1.CertificateSigningRequest {
	return &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("csr-" + name)},
		Spec: certv1.CertificateSigningRequestSpec{
			SignerName: certv1.KubeAPIServerClientKubeletSignerName,
			Username:   user,
			Groups:     []string{"system:bootstrappers", "system:bootstrappers:stratum:nodes", "system:authenticated"},
			Usages:     usages,
			Request:    req,
		},
	}
}

func TestNodeClientCSRVerdict(t *testing.T) {
	const tok = "system:bootstrap:abc123"
	nodes := []string{"system:nodes"}
	now := time.Now()
	expired := joinToken("old111", now.Add(-time.Minute))
	malformed := joinToken("bad222", time.Time{})
	malformed.Data["expiration"] = []byte("tomorrow")
	disabled := joinToken("off333", now.Add(time.Hour))
	disabled.Data["usage-bootstrap-authentication"] = []byte("false")
	foreign := joinToken("kad444", now.Add(time.Hour))
	foreign.Data["auth-extra-groups"] = []byte("system:bootstrappers:kubeadm:default-node-token")
	cs := fake.NewSimpleClientset(makeNode("cp1", "10.0.0.1", "cp1"),
		joinToken("abc123", now.Add(time.Hour)), joinToken("forever", time.Time{}), expired, malformed, disabled, foreign)
	ctx := context.Background()
	notStratum := bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil))
	notStratum.Spec.Groups = []string{"system:bootstrappers", "system:authenticated"}

	cases := []struct {
		name   string
		csr    *certv1.CertificateSigningRequest
		claims map[string]int
		want   verdict
	}{
		{"new node name → approve", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, approve},
		{"registered node name → pending, never taken over", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:cp1", nodes, nil)), nil, pending},
		{"name claimed twice → pending", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), map[string]int{"w1": 2}, pending},
		{"CN not a node → deny", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "admin", nodes, nil)), nil, deny},
		{"empty node name → deny", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:", nodes, nil)), nil, deny},
		{"extra organization → deny", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", []string{"system:nodes", "system:masters"}, nil)), nil, deny},
		{"SANs on a client cert → deny", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, []string{"w1"})), nil, deny},
		{"client auth alone → deny", bootstrapCSR("a", tok, []certv1.KeyUsage{certv1.UsageClientAuth}, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"client auth with key encipherment, no signature → deny", bootstrapCSR("a", tok, []certv1.KeyUsage{certv1.UsageKeyEncipherment, certv1.UsageClientAuth}, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"signature and client auth (ECDSA) → approve", bootstrapCSR("a", tok, []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth}, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, approve},
		{"server-auth usage → deny", bootstrapCSR("a", tok, []certv1.KeyUsage{certv1.UsageClientAuth, certv1.UsageServerAuth}, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"client-auth without digital signature → deny", bootstrapCSR("a", tok, []certv1.KeyUsage{certv1.UsageClientAuth}, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"bad PEM → deny", bootstrapCSR("a", tok, nodeClientUsages, []byte("junk")), nil, deny},
		{"non-expiring token → approve", bootstrapCSR("a", "system:bootstrap:forever", nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, approve},
		{"token deleted since → deny", bootstrapCSR("a", "system:bootstrap:gone00", nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"token expired since → deny", bootstrapCSR("a", "system:bootstrap:old111", nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"token expiration unparseable → deny", bootstrapCSR("a", "system:bootstrap:bad222", nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"token authentication disabled → deny", bootstrapCSR("a", "system:bootstrap:off333", nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"requester not in the stratum join group → deny", notStratum, nil, deny},
		{"token not minted by stratum → deny", bootstrapCSR("a", "system:bootstrap:kad444", nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)), nil, deny},
		{"name held by an earlier certificate → pending", bootstrapCSR("a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w8", nodes, nil)), nil, pending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := newNodeClientPass(ctx, cs, now)
			if err != nil {
				t.Fatal(err)
			}
			p.owners["w8"] = "csr-earlier"
			if c.claims != nil {
				p.claims = c.claims
			}
			got, reason := p.verdict(ctx, c.csr)
			if got != c.want {
				t.Fatalf("verdict = %v (%s), want %v", got, reason, c.want)
			}
			if c.name == "token expiration unparseable → deny" && !strings.Contains(reason, "unparseable expiration") {
				t.Fatalf("reason = %q, want unparseable expiration", reason)
			}
		})
	}
}

func approvalOf(t *testing.T, cs *fake.Clientset, name string) certv1.RequestConditionType {
	t.Helper()
	csr, err := cs.CertificatesV1().CertificateSigningRequests().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range csr.Status.Conditions {
		if c.Type == certv1.CertificateApproved || c.Type == certv1.CertificateDenied {
			return c.Type
		}
	}
	return ""
}

func TestReconcileNodeClientCSRs(t *testing.T) {
	const tok = "system:bootstrap:abc123"
	nodes := []string{"system:nodes"}
	renewal := &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "renewal"},
		Spec: certv1.CertificateSigningRequestSpec{
			SignerName: certv1.KubeAPIServerClientKubeletSignerName,
			Username:   "system:node:cp1",
			Groups:     []string{"system:nodes"},
			Usages:     nodeClientUsages,
			Request:    nodeClientPEM(t, "system:node:cp1", nodes, nil),
		},
	}
	now := time.Now()
	decided := func(name, node string, created time.Time, t2 certv1.RequestConditionType) *certv1.CertificateSigningRequest {
		c := bootstrapCSR(name, tok, nodeClientUsages, nodeClientPEM(t, "system:node:"+node, nodes, nil))
		c.CreationTimestamp = metav1.NewTime(created)
		c.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: t2}}
		return c
	}
	serving := bootstrapCSR("serving", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w9", nodes, nil))
	serving.Spec.SignerName = certv1.KubeletServingSignerName
	approvedLate := decided("w7-approved-late", "w7", now.Add(-time.Hour), certv1.CertificateApproved)
	approvedLate.Status.Conditions[0].LastUpdateTime = metav1.NewTime(now.Add(-time.Minute))
	cs := fake.NewSimpleClientset(
		makeNode("cp1", "10.0.0.1", "cp1"),
		joinToken("abc123", now.Add(time.Hour)),
		joinToken("zzz999", now.Add(time.Hour)),
		approvedLate,
		bootstrapCSR("w7-second", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w7", nodes, nil)),
		bootstrapCSR("new", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", nodes, nil)),
		bootstrapCSR("takeover", tok, nodeClientUsages, nodeClientPEM(t, "system:node:cp1", nodes, nil)),
		bootstrapCSR("dup-a", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w2", nodes, nil)),
		bootstrapCSR("dup-b", "system:bootstrap:zzz999", nodeClientUsages, nodeClientPEM(t, "system:node:w2", nodes, nil)),
		bootstrapCSR("forged", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w3", []string{"system:masters"}, nil)),
		renewal,
		serving,
		decided("w4-earlier", "w4", now.Add(-time.Minute), certv1.CertificateApproved),
		bootstrapCSR("w4-second", "system:bootstrap:zzz999", nodeClientUsages, nodeClientPEM(t, "system:node:w4", nodes, nil)),
		decided("w5-stale", "w5", now.Add(-2*time.Hour), certv1.CertificateApproved),
		bootstrapCSR("w5-rejoin", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w5", nodes, nil)),
		decided("w6-denied", "w6", now.Add(-time.Minute), certv1.CertificateDenied),
		bootstrapCSR("w6-retry", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w6", nodes, nil)),
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: k8sutil.NodeClientAutoApproveBinding},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"},
			Subjects:   []rbacv1.Subject{{Kind: "Group", Name: k8sutil.BootstrapGroup, APIGroup: rbacv1.GroupName}},
		},
	)

	reconcileNodeClientCSRs(context.Background(), cs, now)

	if binding, err := cs.RbacV1().ClusterRoleBindings().Get(context.Background(), k8sutil.NodeClientAutoApproveBinding, metav1.GetOptions{}); err != nil || len(binding.Subjects) != 0 {
		t.Errorf("%s not reserved without subjects (binding %+v, err %v)", k8sutil.NodeClientAutoApproveBinding, binding, err)
	}

	for name, want := range map[string]certv1.RequestConditionType{
		"new":       certv1.CertificateApproved,
		"takeover":  "",
		"dup-a":     "",
		"dup-b":     "",
		"forged":    certv1.CertificateDenied,
		"renewal":   "",
		"serving":   "",
		"w4-second": "",
		"w5-rejoin": "",
		"w6-retry":  certv1.CertificateApproved,
		"w7-second": "",
	} {
		if got := approvalOf(t, cs, name); got != want {
			t.Errorf("CSR %s: decision %q, want %q", name, got, want)
		}
	}
}

func TestNodeClientClaimSurvivesCSRDeletion(t *testing.T) {
	const tok = "system:bootstrap:abc123"
	now := time.Now()
	newCSR := func(name string) *certv1.CertificateSigningRequest {
		return bootstrapCSR(name, tok, nodeClientUsages,
			nodeClientPEM(t, "system:node:w1", []string{"system:nodes"}, nil))
	}
	cs := fake.NewSimpleClientset(joinToken("abc123", now.Add(time.Hour)), newCSR("first"))
	reconcileNodeClientCSRs(context.Background(), cs, now)
	if got := approvalOf(t, cs, "first"); got != certv1.CertificateApproved {
		t.Fatalf("first CSR decision = %q, want approval", got)
	}
	claim, err := cs.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(context.Background(), nodeClientClaimName("w1"), metav1.GetOptions{})
	if err != nil || claim.Data["csrUID"] != "csr-first" {
		t.Fatalf("node-name claim = %+v, %v", claim, err)
	}
	if err := cs.CertificatesV1().CertificateSigningRequests().Delete(context.Background(), "first", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CertificatesV1().CertificateSigningRequests().Create(context.Background(), newCSR("second"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileNodeClientCSRs(context.Background(), cs, now.Add(11*time.Minute))
	if got := approvalOf(t, cs, "second"); got != "" {
		t.Fatalf("second CSR decision = %q; an issued certificate must retain its node name after CSR cleanup", got)
	}
}

// A lookup the pass cannot complete leaves every request pending: a transient
// apiserver error must never read as "no such node" or "token is fine".
func TestNodeClientPassFailsClosedOnLookupErrors(t *testing.T) {
	const tok = "system:bootstrap:abc123"
	now := time.Now()
	for _, tc := range []struct{ verb, resource string }{
		{"get", "secrets"}, {"list", "nodes"}, {"list", "configmaps"},
	} {
		t.Run(tc.verb+" "+tc.resource, func(t *testing.T) {
			cs := fake.NewSimpleClientset(joinToken("abc123", now.Add(time.Hour)),
				bootstrapCSR("w1", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", []string{"system:nodes"}, nil)))
			cs.PrependReactor(tc.verb, tc.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
			})
			reconcileNodeClientCSRs(context.Background(), cs, now)
			if got := approvalOf(t, cs, "w1"); got != "" {
				t.Fatalf("CSR decision = %q when %s %s fails; want it left pending", got, tc.verb, tc.resource)
			}
		})
	}
}

// A registered Node's name is claimed even though no bootstrap CSR approved it,
// so a request held pending for that name is not approved once the Node is
// deleted.
func TestRegisteredNodeNameStaysClaimedAfterNodeDeletion(t *testing.T) {
	const tok = "system:bootstrap:abc123"
	now := time.Now()
	cs := fake.NewSimpleClientset(makeNode("w1", "10.0.0.5", "w1"), joinToken("abc123", now.Add(time.Hour)),
		bootstrapCSR("takeover", tok, nodeClientUsages, nodeClientPEM(t, "system:node:w1", []string{"system:nodes"}, nil)))
	ctx := context.Background()
	reconcileNodeClientCSRs(ctx, cs, now)
	if err := cs.CoreV1().Nodes().Delete(ctx, "w1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileNodeClientCSRs(ctx, cs, now.Add(30*time.Minute))
	if got := approvalOf(t, cs, "takeover"); got != "" {
		t.Fatalf("CSR for a deleted Node's name decided %q; the registered name must stay claimed", got)
	}
	claim, err := cs.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(ctx, nodeClientClaimName("w1"), metav1.GetOptions{})
	if err != nil || !strings.HasPrefix(claim.Data["csrUID"], "node/") {
		t.Fatalf("registered node claim = %+v, %v", claim, err)
	}
}

// A pending CSR names its node and reason on the object, written once, and
// each join token is looked up once per pass however many requests use it.
func TestNodeClientPendingReasonIsRecordedOnce(t *testing.T) {
	const tok = "system:bootstrap:abc123"
	now := time.Now()
	nodes := []string{"system:nodes"}
	cs := fake.NewSimpleClientset(makeNode("cp1", "10.0.0.1", "cp1"), joinToken("abc123", now.Add(time.Hour)),
		bootstrapCSR("t1", tok, nodeClientUsages, nodeClientPEM(t, "system:node:cp1", nodes, nil)),
		bootstrapCSR("t2", tok, nodeClientUsages, nodeClientPEM(t, "system:node:cp1", nodes, nil)),
		bootstrapCSR("t3", tok, nodeClientUsages, nodeClientPEM(t, "system:node:cp1", nodes, nil)))
	ctx := context.Background()
	count := func(verb, resource string) int {
		n := 0
		for _, a := range cs.Actions() {
			if a.GetVerb() == verb && a.GetResource().Resource == resource && a.GetSubresource() == "" {
				n++
			}
		}
		return n
	}
	reconcileNodeClientCSRs(ctx, cs, now)
	if got := count("get", "secrets"); got != 1 {
		t.Errorf("join token looked up %d times for three requests, want once", got)
	}
	csr, err := cs.CertificatesV1().CertificateSigningRequests().Get(ctx, "t1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if csr.Annotations[nodeClientRequestedAnnotation] != "cp1" || !strings.Contains(csr.Annotations[nodeClientPendingAnnotation], "claims node cp1") {
		t.Fatalf("pending CSR annotations = %v", csr.Annotations)
	}
	updates := count("update", "certificatesigningrequests")
	reconcileNodeClientCSRs(ctx, cs, now.Add(time.Minute))
	if got := count("update", "certificatesigningrequests"); got != updates {
		t.Errorf("an unchanged pending reason was rewritten (%d updates, then %d)", updates, got)
	}
}
