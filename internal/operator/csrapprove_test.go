package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"testing"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func csrPEM(t *testing.T, cn string, dns, ips []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, DNSNames: dns}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func makeNode(name, ip, host string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: ip},
			{Type: corev1.NodeHostName, Address: host},
		}},
	}
}

func servingCSR(user string, groups []string, usages []certv1.KeyUsage, req []byte) *certv1.CertificateSigningRequest {
	return &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "csr-x"},
		Spec: certv1.CertificateSigningRequestSpec{
			SignerName: certv1.KubeletServingSignerName,
			Username:   user, Groups: groups, Usages: usages, Request: req,
		},
	}
}

func TestServingCSRVerdict(t *testing.T) {
	const node = "stratum-w1"
	user := "system:node:" + node
	grp := []string{"system:nodes"}
	serving := []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageServerAuth}
	cs := fake.NewSimpleClientset(makeNode(node, "10.0.0.5", node))
	ctx := context.Background()

	cases := []struct {
		name string
		csr  *certv1.CertificateSigningRequest
		want verdict
	}{
		{"valid: CN + SANs match the node", servingCSR(user, grp, serving, csrPEM(t, user, []string{node}, []string{"10.0.0.5"})), approve},
		{"spoofed IP the node doesn't own → never approved", servingCSR(user, grp, serving, csrPEM(t, user, []string{node}, []string{"10.0.0.99"})), pending},
		{"CN mismatch → deny", servingCSR(user, grp, serving, csrPEM(t, "system:node:other", []string{node}, []string{"10.0.0.5"})), deny},
		{"requester not a node → deny", servingCSR("dave", grp, serving, csrPEM(t, user, []string{node}, []string{"10.0.0.5"})), deny},
		{"requester not in system:nodes → deny", servingCSR(user, nil, serving, csrPEM(t, user, []string{node}, []string{"10.0.0.5"})), deny},
		{"client-auth usage under serving signer → deny", servingCSR(user, grp, []certv1.KeyUsage{certv1.UsageClientAuth, certv1.UsageServerAuth}, csrPEM(t, user, []string{node}, []string{"10.0.0.5"})), deny},
		{"unregistered node → pending (self-heals)", servingCSR("system:node:ghost", grp, serving, csrPEM(t, "system:node:ghost", nil, []string{"10.0.0.5"})), pending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := servingCSRVerdict(ctx, cs, c.csr)
			if got != c.want {
				t.Errorf("verdict = %v (%q), want %v", got, reason, c.want)
			}
		})
	}
}
