package operator

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func dashboardFixture(t *testing.T) (*dashboardSecrets, *fake.Clientset, string, func(string) *pki.CA) {
	t.Helper()
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	newCA := func(cn string) *pki.CA {
		ca, err := pki.NewCA("ca", cn)
		if err != nil {
			t.Fatal(err)
		}
		return ca
	}
	ca := newCA("stratum-ca")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, addons.DashboardStack), 0o755); err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardNamespace}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}, {Type: corev1.NodeHostName, Address: "cp1"},
		}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardName, Namespace: addons.DashboardNamespace,
			Annotations: map[string]string{addons.DashboardHostsKey: "Dash.Example.com"}}},
	)
	d := newDashboardSecrets(cs, "10.0.0.100", "")
	d.domain = "cluster.local"
	d.signer = func(context.Context, []string, []string) (*pki.CA, error) { return ca, nil }
	return d, cs, dir, func(cn string) *pki.CA { ca = newCA(cn); return ca }
}

func servingLeaf(t *testing.T, cs *fake.Clientset) (*x509.Certificate, *corev1.Secret) {
	t.Helper()
	s, err := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(context.Background(), addons.DashboardTLSSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(s.Data[corev1.TLSCertKey])
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, s
}

func TestReconcileDashboardWritesItsSecrets(t *testing.T) {
	d, cs, dir, _ := dashboardFixture(t)
	ctx := context.Background()
	reconcileDashboard(ctx, d, dir)

	leaf, s := servingLeaf(t, cs)
	if s.Type != corev1.SecretTypeTLS {
		t.Errorf("type %s, want %s", s.Type, corev1.SecretTypeTLS)
	}
	wantDNS := []string{"dash.example.com", "localhost", "stratum-dashboard", "stratum-dashboard.stratum-dashboard",
		"stratum-dashboard.stratum-dashboard.svc", "stratum-dashboard.stratum-dashboard.svc.cluster.local"}
	if got := slices.Sorted(slices.Values(leaf.DNSNames)); !slices.Equal(got, wantDNS) {
		t.Errorf("DNS names %v, want %v", got, wantDNS)
	}
	var ips []string
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	if slices.Sort(ips); !slices.Equal(ips, []string{"10.0.0.100", "10.0.0.11", "127.0.0.1"}) {
		t.Errorf("IP SANs %v", ips)
	}

	key, err := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(ctx, addons.DashboardSessionSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(key.Data["key"]) != sessionKeyBytes {
		t.Fatalf("session key is %d bytes", len(key.Data["key"]))
	}

	reconcileDashboard(ctx, d, dir)
	again, _ := servingLeaf(t, cs)
	if again.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Error("an unchanged cluster reissued the certificate")
	}
	key2, _ := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(ctx, addons.DashboardSessionSecret, metav1.GetOptions{})
	if string(key2.Data["key"]) != string(key.Data["key"]) {
		t.Error("the session key was replaced, which signs everyone out")
	}
}

func TestReconcileDashboardRenews(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(d *dashboardSecrets, cs *fake.Clientset, rotate func(string) *pki.CA)
	}{
		{"a new node", func(_ *dashboardSecrets, cs *fake.Clientset, _ func(string) *pki.CA) {
			_, _ = cs.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.21"}}}}, metav1.CreateOptions{})
		}},
		{"a CA rotation", func(_ *dashboardSecrets, _ *fake.Clientset, rotate func(string) *pki.CA) { rotate("stratum-ca-new") }},
		{"age", func(d *dashboardSecrets, _ *fake.Clientset, _ func(string) *pki.CA) {
			d.now = func() time.Time { return time.Now().Add(pki.LeafValidity * 3 / 4) }
		}},
		{"a damaged key", func(_ *dashboardSecrets, cs *fake.Clientset, _ func(string) *pki.CA) {
			s, _ := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(context.Background(), addons.DashboardTLSSecret, metav1.GetOptions{})
			s.Data[corev1.TLSPrivateKeyKey] = nil
			_, _ = cs.CoreV1().Secrets(addons.DashboardNamespace).Update(context.Background(), s, metav1.UpdateOptions{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dir, rotate := dashboardFixture(t)
			reconcileDashboard(context.Background(), d, dir)
			before, _ := servingLeaf(t, cs)
			tc.change(d, cs, rotate)
			reconcileDashboard(context.Background(), d, dir)
			after, _ := servingLeaf(t, cs)
			if after.SerialNumber.Cmp(before.SerialNumber) == 0 {
				t.Fatal("the certificate was not reissued")
			}
		})
	}
}

func TestReconcileDashboardWaitsForTheStack(t *testing.T) {
	d, cs, _, _ := dashboardFixture(t)
	reconcileDashboard(context.Background(), d, t.TempDir())
	if l, _ := cs.CoreV1().Secrets(addons.DashboardNamespace).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatalf("wrote %d Secrets with no dashboard stack staged", len(l.Items))
	}
}

func TestReconcileDashboardReplacesARejectedToken(t *testing.T) {
	d, cs, dir, _ := dashboardFixture(t)
	ctx := context.Background()
	accepted := map[string]bool{"good": true}
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		tr.Status.Authenticated = accepted[tr.Spec.Token]
		return true, tr, nil
	})
	secret := func(name, token string) {
		_, err := cs.CoreV1().Secrets(addons.DashboardNamespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addons.DashboardNamespace,
				Annotations: map[string]string{corev1.ServiceAccountNameKey: addons.DashboardTokenSecrets[name]}},
			Type: corev1.SecretTypeServiceAccountToken,
			Data: map[string][]byte{corev1.ServiceAccountTokenKey: []byte(token)},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	secret("stratum-dashboard-admin-token", "good")
	secret("stratum-dashboard-viewer-token", "signed-by-the-old-key")
	viewerOld, _ := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(ctx, "stratum-dashboard-viewer-token", metav1.GetOptions{})
	viewerOld.Labels = map[string]string{"kubernetes.io/legacy-token-invalid-since": "2026-01-01", "stratum.ciq.com/stack": "dashboard"}
	if _, err := cs.CoreV1().Secrets(addons.DashboardNamespace).Update(ctx, viewerOld, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	reconcileDashboard(ctx, d, dir)

	get := func(name string) *corev1.Secret {
		s, err := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return s
	}
	if got := string(get("stratum-dashboard-admin-token").Data[corev1.ServiceAccountTokenKey]); got != "good" {
		t.Errorf("a valid token was replaced: %q", got)
	}
	viewer := get("stratum-dashboard-viewer-token")
	if len(viewer.Data[corev1.ServiceAccountTokenKey]) != 0 || viewer.Type != corev1.SecretTypeServiceAccountToken ||
		viewer.Annotations[corev1.ServiceAccountNameKey] != "stratum-dashboard-viewer" {
		t.Errorf("the rejected token was not replaced by an empty token Secret for its ServiceAccount: %+v", viewer)
	}
	if _, marked := viewer.Labels["kubernetes.io/legacy-token-invalid-since"]; marked || viewer.Labels["stratum.ciq.com/stack"] != "dashboard" {
		t.Errorf("replacement labels %v: want the stack label kept and the legacy-token mark dropped", viewer.Labels)
	}
}

func TestReconcileDashboardLeavesAbsentTokensAlone(t *testing.T) {
	d, cs, dir, _ := dashboardFixture(t)
	reconcileDashboard(context.Background(), d, dir)
	for name := range addons.DashboardTokenSecrets {
		if _, err := cs.CoreV1().Secrets(addons.DashboardNamespace).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			t.Errorf("%s was created by the operator; the stack owns it", name)
		}
	}
}

func TestDashboardCertChainsToItsOwnCA(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, addons.DashboardStack), 0o755); err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardNamespace}})
	d := newDashboardSecrets(cs, "", "")
	ctx := context.Background()
	reconcileDashboard(ctx, d, dir)

	caSecret, err := cs.CoreV1().Secrets("kube-system").Get(ctx, addons.DashboardCASecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no dashboard CA in kube-system: %v", err)
	}
	ca, err := pki.ParseCA(caSecret.Data[corev1.TLSCertKey], caSecret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}
	leaf, s := servingLeaf(t, cs)
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Errorf("the serving cert is not signed by the dashboard CA: %v", err)
	}
	if string(s.Data["ca.crt"]) != string(caSecret.Data[corev1.TLSCertKey]) {
		t.Error("ca.crt in the serving Secret is not the dashboard CA")
	}
	if _, ok := s.Data["ca.key"]; ok {
		t.Error("the CA key reached the dashboard namespace")
	}
	if entries, _ := os.ReadDir(version.PKIDir); len(entries) != 0 {
		t.Errorf("the cluster PKI dir was touched: %v", entries)
	}

	reconcileDashboard(ctx, d, dir)
	again, err := cs.CoreV1().Secrets("kube-system").Get(ctx, addons.DashboardCASecret, metav1.GetOptions{})
	if err != nil || string(again.Data[corev1.TLSCertKey]) != string(caSecret.Data[corev1.TLSCertKey]) {
		t.Error("a second pass replaced the dashboard CA")
	}
	same, _ := servingLeaf(t, cs)
	if same.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Error("a second pass reissued the serving cert")
	}
}

func dashboardCAFromSecret(t *testing.T, cs *fake.Clientset) (*pki.CA, *corev1.Secret) {
	t.Helper()
	sec, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), addons.DashboardCASecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.ParseCA(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}
	return ca, sec
}

func defaultDashboard(t *testing.T, objs ...runtime.Object) (*dashboardSecrets, *fake.Clientset, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, addons.DashboardStack), 0o755); err != nil {
		t.Fatal(err)
	}
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardNamespace}})
	cs := fake.NewClientset(objs...)
	return newDashboardSecrets(cs, "10.0.0.100", ""), cs, dir
}

func ipNode(name, ip string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
}

func TestDashboardCAOnlyVouchesForTheDashboard(t *testing.T) {
	d, cs, dir := defaultDashboard(t, ipNode("cp1", "10.0.0.11"))
	reconcileDashboard(context.Background(), d, dir)
	ca, _ := dashboardCAFromSecret(t, cs)
	if !ca.Cert.PermittedDNSDomainsCritical || !ca.Cert.MaxPathLenZero {
		t.Fatalf("the dashboard CA is not constrained: critical=%v maxPathLenZero=%v", ca.Cert.PermittedDNSDomainsCritical, ca.Cert.MaxPathLenZero)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	verify := func(c *x509.Certificate, name string, inter *x509.CertPool) error {
		_, err := c.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: inter})
		return err
	}

	leaf, _ := servingLeaf(t, cs)
	for _, name := range []string{"stratum-dashboard.stratum-dashboard.svc", "localhost", "10.0.0.11", "10.0.0.100"} {
		if err := verify(leaf, name, nil); err != nil {
			t.Errorf("the serving cert does not verify for %s: %v", name, err)
		}
	}

	issue := func(s pki.CertSpec) *x509.Certificate {
		certPEM, _, err := ca.IssuePEM(s)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(certPEM)
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := verify(issue(pki.CertSpec{CN: "bank", DNS: []string{"bank.example.com"}, ServerAuth: true}), "bank.example.com", nil); err == nil {
		t.Error("the dashboard CA vouched for bank.example.com")
	}
	if err := verify(issue(pki.CertSpec{CN: "far", IPs: []string{"192.168.50.5"}, ServerAuth: true}), "192.168.50.5", nil); err == nil {
		t.Error("the dashboard CA vouched for an address outside the cluster's subnets")
	}
}

func TestDashboardCAIsReplacedWhenItDoesNotCover(t *testing.T) {
	loose, err := pki.GenerateCA(addons.DashboardCASecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := loose.PEM()
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardCASecret, Namespace: "kube-system"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}}
	d, cs, dir := defaultDashboard(t, ipNode("cp1", "10.0.0.11"), old)
	ctx := context.Background()

	reconcileDashboard(ctx, d, dir)
	first, _ := dashboardCAFromSecret(t, cs)
	if first.Cert.Equal(loose.Cert) || !first.Cert.PermittedDNSDomainsCritical {
		t.Fatal("an unconstrained dashboard CA was kept")
	}
	leaf, _ := servingLeaf(t, cs)
	if err := leaf.CheckSignatureFrom(first.Cert); err != nil {
		t.Fatalf("the serving cert was not reissued under the new CA: %v", err)
	}

	if _, err := cs.CoreV1().Nodes().Create(ctx, ipNode("w1", "10.0.0.21"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileDashboard(ctx, d, dir)
	if same, _ := dashboardCAFromSecret(t, cs); !same.Cert.Equal(first.Cert) {
		t.Error("a node in a subnet the CA already covers replaced the CA")
	}

	if _, err := cs.CoreV1().Nodes().Create(ctx, ipNode("w2", "172.16.4.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileDashboard(ctx, d, dir)
	grown, _ := dashboardCAFromSecret(t, cs)
	if grown.Cert.Equal(first.Cert) {
		t.Fatal("a node in a new subnet kept a CA that cannot vouch for it")
	}
	leaf, _ = servingLeaf(t, cs)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "172.16.4.9", Roots: func() *x509.CertPool { p := x509.NewCertPool(); p.AddCert(grown.Cert); return p }()}); err != nil {
		t.Errorf("the serving cert does not verify for the new node: %v", err)
	}
}

func TestDashboardCAUnusableSecretNamesTheFix(t *testing.T) {
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardCASecret, Namespace: "kube-system"},
		Data: map[string][]byte{corev1.TLSCertKey: []byte("junk"), corev1.TLSPrivateKeyKey: []byte("junk")}}
	_, cs, _ := defaultDashboard(t, bad)
	_, err := dashboardCA(context.Background(), cs, []string{"localhost"}, []string{"127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "kube-system/"+addons.DashboardCASecret) || !strings.Contains(err.Error(), "delete it") {
		t.Fatalf("err = %v, want one naming the Secret and the fix", err)
	}
}

func TestDashboardCertNamesTheClusterDomain(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, addons.DashboardStack), 0o755); err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardNamespace}})
	reconcileDashboard(context.Background(), newDashboardSecrets(cs, "", "fuzz.internal"), dir)
	leaf, _ := servingLeaf(t, cs)
	if !slices.Contains(leaf.DNSNames, "stratum-dashboard.stratum-dashboard.svc.fuzz.internal") {
		t.Errorf("DNS names %v lack the Service name in the cluster's domain", leaf.DNSNames)
	}
}

// TestDashboardCertNamesTheDefaultDomainWhenNoneIsGiven: an operator handed no
// cluster domain names the dashboard's Service in the default domain, never in
// an empty one ("….svc.").
func TestDashboardCertNamesTheDefaultDomainWhenNoneIsGiven(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, addons.DashboardStack), 0o755); err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardNamespace}})
	d := newDashboardSecrets(cs, "", "")
	if d.domain != "cluster.local" {
		t.Errorf("domain = %q, want cluster.local", d.domain)
	}
	reconcileDashboard(context.Background(), d, dir)
	leaf, _ := servingLeaf(t, cs)
	if !slices.Contains(leaf.DNSNames, "stratum-dashboard.stratum-dashboard.svc.cluster.local") {
		t.Errorf("DNS names %v lack the Service name in the default domain", leaf.DNSNames)
	}
	for _, name := range leaf.DNSNames {
		if strings.HasSuffix(name, ".") {
			t.Errorf("DNS names %v include %q, a Service name in an empty domain", leaf.DNSNames, name)
		}
	}
}
