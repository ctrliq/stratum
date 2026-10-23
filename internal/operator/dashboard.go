package operator

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/pki"
)

const sessionKeyBytes = 32

// dashboardSecrets keeps the Secrets the dashboard stack mounts: a serving
// certificate from the dashboard CA, renewed before it ages out, when its names
// change, or when that CA is replaced; and a session key, created once.
type dashboardSecrets struct {
	cs     kubernetes.Interface
	signer func(ctx context.Context, dns, ips []string) (*pki.CA, error)
	vip    string
	domain string
	now    func() time.Time

	tokenChecked map[string]time.Time
}

const tokenCheckInterval = 5 * time.Minute

func newDashboardSecrets(cs kubernetes.Interface, vip, domain string) *dashboardSecrets {
	return &dashboardSecrets{
		cs:     cs,
		signer: func(ctx context.Context, dns, ips []string) (*pki.CA, error) { return dashboardCA(ctx, cs, dns, ips) },
		vip:    vip,
		domain: netcfg.DomainOr(domain),
		now:    time.Now,

		tokenChecked: map[string]time.Time{},
	}
}

// reconcileDashboard runs only while the dashboard stack is staged and its
// namespace exists; the deployer creates the namespace and prunes it with the
// stack.
func reconcileDashboard(ctx context.Context, d *dashboardSecrets, manifestsDir string) {
	if manifestsDir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(manifestsDir, addons.DashboardStack)); err != nil {
		return
	}
	if _, err := d.cs.CoreV1().Namespaces().Get(ctx, addons.DashboardNamespace, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("dashboard: %v", err)
		}
		return
	}
	if err := d.ensureSessionKey(ctx); err != nil {
		log.Printf("dashboard: session key: %v", err)
	}
	if err := d.ensureServingCert(ctx); err != nil {
		log.Printf("dashboard: serving certificate: %v", err)
	}
	for _, name := range slices.Sorted(maps.Keys(addons.DashboardTokenSecrets)) {
		if err := d.checkToken(ctx, name); err != nil {
			log.Printf("dashboard: token %s: %v", name, err)
		}
	}
}

// checkToken replaces a sign-in token Secret whose token the apiserver no
// longer accepts, as after a service-account key rotation; the token controller
// fills the new one. A Secret the stack does not carry is left alone.
func (d *dashboardSecrets) checkToken(ctx context.Context, name string) error {
	if last, ok := d.tokenChecked[name]; ok && d.now().Sub(last) < tokenCheckInterval {
		return nil
	}
	secrets := d.cs.CoreV1().Secrets(addons.DashboardNamespace)
	cur, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	token := cur.Data[corev1.ServiceAccountTokenKey]
	if len(token) == 0 {
		return nil
	}
	review, err := d.cs.AuthenticationV1().TokenReviews().Create(ctx,
		&authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: string(token)}}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	d.tokenChecked[name] = d.now()
	if review.Status.Authenticated {
		return nil
	}
	log.Printf("dashboard: %s/%s holds a token the apiserver rejects; replacing it", addons.DashboardNamespace, name)
	if err := secrets.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &cur.UID}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	fresh := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addons.DashboardNamespace, Labels: withoutLegacyTokenMarks(cur.Labels),
			Annotations: map[string]string{corev1.ServiceAccountNameKey: addons.DashboardTokenSecrets[name]}},
		Type: corev1.SecretTypeServiceAccountToken,
	}
	_, err = secrets.Create(ctx, fresh, metav1.CreateOptions{FieldManager: fieldManager})
	delete(d.tokenChecked, name)
	return err
}

func dashboardSecretMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: addons.DashboardNamespace,
		Labels:    map[string]string{"app.kubernetes.io/managed-by": fieldManager, "app.kubernetes.io/name": addons.DashboardName},
	}
}

func (d *dashboardSecrets) ensureSessionKey(ctx context.Context) error {
	secrets := d.cs.CoreV1().Secrets(addons.DashboardNamespace)
	cur, err := secrets.Get(ctx, addons.DashboardSessionSecret, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if exists && len(cur.Data["key"]) >= sessionKeyBytes {
		return nil
	}
	key := make([]byte, sessionKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	s := &corev1.Secret{ObjectMeta: dashboardSecretMeta(addons.DashboardSessionSecret), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"key": key}}
	if !exists {
		_, err = secrets.Create(ctx, s, metav1.CreateOptions{FieldManager: fieldManager})
		return err
	}
	s.ResourceVersion = cur.ResourceVersion
	_, err = secrets.Update(ctx, s, metav1.UpdateOptions{FieldManager: fieldManager})
	return err
}

// withoutLegacyTokenMarks drops the legacy-token cleaner's labels, so a
// replacement token is not born invalid.
func withoutLegacyTokenMarks(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if !strings.HasPrefix(k, "kubernetes.io/legacy-token-") {
			out[k] = v
		}
	}
	return out
}

// dashboardCA is the CA the dashboard's serving certificate chains to: its own,
// kept in kube-system so the dashboard pod never holds it. Users install it as a
// trust root, so it is name-constrained to the dashboard's names and the
// subnets its addresses are in, and may sign no other CA: its key cannot vouch
// for any other site. A CA that does not cover dns and ips is replaced.
func dashboardCA(ctx context.Context, cs kubernetes.Interface, dns, ips []string) (*pki.CA, error) {
	secrets := cs.CoreV1().Secrets(dashboardCANamespace)
	where := dashboardCANamespace + "/" + addons.DashboardCASecret
	cur, err := secrets.Get(ctx, addons.DashboardCASecret, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get %s: %w", where, err)
	}
	if exists {
		ca, err := pki.ParseCA(cur.Data[corev1.TLSCertKey], cur.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			return nil, fmt.Errorf("%s does not hold a usable CA (delete it to have a new one made): %w", where, err)
		}
		reason := caShortfall(ca.Cert, dns, ips)
		if reason == "" {
			return ca, nil
		}
		log.Printf("dashboard: replacing the dashboard CA in %s: %s; browsers that trusted the old one must trust the new one", where, reason)
	}
	ca, err := pki.GenerateCA(addons.DashboardCASecret, caConstraints(dns, ips))
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM := ca.PEM()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: addons.DashboardCASecret, Namespace: dashboardCANamespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": fieldManager, "app.kubernetes.io/name": addons.DashboardName}},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
	if exists {
		s.ResourceVersion = cur.ResourceVersion
		if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return nil, fmt.Errorf("replace %s: %w", where, err)
		}
		return ca, nil
	}
	_, err = secrets.Create(ctx, s, metav1.CreateOptions{FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		return dashboardCA(ctx, cs, dns, ips)
	}
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", where, err)
	}
	log.Printf("dashboard: created the dashboard CA in %s", where)
	return ca, nil
}

// caConstraints permits exactly the dashboard's names, and for its addresses the
// subnet each sits in (a /24 or /64; loopback alone), so a node that joins the
// same subnet does not need a new CA.
func caConstraints(dns, ips []string) *pki.Constraints {
	c := &pki.Constraints{DNS: slices.Clone(dns)}
	seen := map[string]bool{}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		bits, size := 24, 32
		if ip.To4() == nil {
			bits, size = 64, 128
		}
		if ip.IsLoopback() {
			bits = size
		}
		n := &net.IPNet{IP: ip.Mask(net.CIDRMask(bits, size)), Mask: net.CIDRMask(bits, size)}
		if !seen[n.String()] {
			seen[n.String()] = true
			c.IPs = append(c.IPs, n)
		}
	}
	return c
}

// caShortfall says why ca cannot sign for dns and ips, or "" when it can.
func caShortfall(ca *x509.Certificate, dns, ips []string) string {
	if !ca.PermittedDNSDomainsCritical || !ca.MaxPathLenZero {
		return "it is not name-constrained"
	}
	for _, d := range dns {
		if !slices.Contains(ca.PermittedDNSDomains, d) {
			return "it does not cover " + d
		}
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil || !slices.ContainsFunc(ca.PermittedIPRanges, func(n *net.IPNet) bool { return n.Contains(ip) }) {
			return "it does not cover " + s
		}
	}
	return ""
}

const dashboardCANamespace = "kube-system"

// servingNames are the names and addresses users reach the dashboard on: its
// Service, every node (it is a NodePort), the VIP, the publicURL host, and
// localhost for a port-forward.
func (d *dashboardSecrets) servingNames(ctx context.Context) (dns, ips []string, err error) {
	svc := addons.DashboardName + "." + addons.DashboardNamespace
	dns = []string{"localhost", addons.DashboardName, svc, svc + ".svc", svc + ".svc." + d.domain}
	ips = []string{"127.0.0.1"}
	add := func(h string) {
		if h == "" {
			return
		}
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip.String())
		} else {
			dns = append(dns, strings.ToLower(h))
		}
	}
	add(d.vip)
	nodes, err := d.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				add(a.Address)
			}
		}
	}
	s, err := d.cs.CoreV1().Services(addons.DashboardNamespace).Get(ctx, addons.DashboardName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	if err == nil {
		for _, h := range strings.Split(s.Annotations[addons.DashboardHostsKey], ",") {
			add(strings.TrimSpace(h))
		}
	}
	slices.Sort(dns)
	slices.Sort(ips)
	return slices.Compact(dns), slices.Compact(ips), nil
}

func (d *dashboardSecrets) ensureServingCert(ctx context.Context) error {
	dns, ips, err := d.servingNames(ctx)
	if err != nil {
		return err
	}
	ca, err := d.signer(ctx, dns, ips)
	if err != nil {
		return fmt.Errorf("load the dashboard CA: %w", err)
	}
	secrets := d.cs.CoreV1().Secrets(addons.DashboardNamespace)
	cur, err := secrets.Get(ctx, addons.DashboardTLSSecret, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if exists {
		reason := d.renewReason(cur.Data[corev1.TLSCertKey], ca, dns, ips)
		if _, err := tls.X509KeyPair(cur.Data[corev1.TLSCertKey], cur.Data[corev1.TLSPrivateKeyKey]); reason == "" && err != nil {
			reason = "its key does not load with the certificate: " + err.Error()
		}
		if reason == "" {
			return nil
		}
		log.Printf("dashboard: renewing %s/%s: %s", addons.DashboardNamespace, addons.DashboardTLSSecret, reason)
	}
	certPEM, keyPEM, err := ca.IssuePEM(pki.CertSpec{Name: addons.DashboardName, CN: addons.DashboardName, DNS: dns, IPs: ips, ServerAuth: true})
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: dashboardSecretMeta(addons.DashboardTLSSecret),
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
			"ca.crt":                pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}),
		},
	}
	if !exists {
		_, err = secrets.Create(ctx, s, metav1.CreateOptions{FieldManager: fieldManager})
		return err
	}
	s.ResourceVersion = cur.ResourceVersion
	_, err = secrets.Update(ctx, s, metav1.UpdateOptions{FieldManager: fieldManager})
	return err
}

// renewReason says why the certificate in certPEM has to be replaced, or ""
// when it is current.
func (d *dashboardSecrets) renewReason(certPEM []byte, ca *pki.CA, dns, ips []string) string {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "no certificate in the Secret"
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "the certificate does not parse"
	}
	if leaf.CheckSignatureFrom(ca.Cert) != nil {
		return "it is not signed by the current dashboard CA"
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if d.now().After(leaf.NotBefore.Add(life * 2 / 3)) {
		return "it is past two thirds of its life"
	}
	var have []string
	for _, ip := range leaf.IPAddresses {
		have = append(have, ip.String())
	}
	slices.Sort(have)
	gotDNS := slices.Clone(leaf.DNSNames)
	slices.Sort(gotDNS)
	if !slices.Equal(gotDNS, dns) || !slices.Equal(have, ips) {
		return "the names it serves changed"
	}
	return ""
}

// dashboardNodePortHeldKey annotates the dashboard Service when its NodePort is
// held by another Service, naming the port and the holder.
const dashboardNodePortHeldKey = "stratum.ciq.com/nodeport-held-by"

// yieldDashboardNodePort serves the dashboard on a ClusterIP when another
// Service already holds a NodePort its Service asks for. obj is one object the
// deployer is about to apply; anything but the dashboard Service is left
// alone. The apiserver refuses a Service whose NodePort is allocated, and a
// refused object stops the apply of everything after it in the stack and
// suspends pruning cluster-wide for the pass, so applying it as rendered would
// leave the cluster with no dashboard Deployment and no pruning for as long as
// the port is held. Instead obj becomes a ClusterIP Service, annotated with the
// holder, and the message for the operator log is returned. The rendered
// Service, NodePort included, applies again on the first pass after the port
// is freed. A Service holds a NodePort through spec.ports, or through
// spec.healthCheckNodePort, which a LoadBalancer Service with
// externalTrafficPolicy Local takes from the same allocator.
func yieldDashboardNodePort(ctx context.Context, cs kubernetes.Interface, obj *unstructured.Unstructured) (string, error) {
	if !isDashboardService(obj) {
		return "", nil
	}
	ports, _, err := unstructured.NestedSlice(obj.Object, "spec", "ports")
	if err != nil {
		return "", err
	}
	want := map[int64]bool{}
	for _, p := range ports {
		if m, ok := p.(map[string]any); ok {
			if n, ok, _ := unstructured.NestedInt64(m, "nodePort"); ok && n != 0 {
				want[n] = true
			}
		}
	}
	if len(want) == 0 {
		return "", nil
	}
	svcs, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list Services: %w", err)
	}
	var held []string
	for _, svc := range svcs.Items {
		if svc.Namespace == addons.DashboardNamespace && svc.Name == addons.DashboardName {
			continue
		}
		taken := []int32{svc.Spec.HealthCheckNodePort}
		for _, p := range svc.Spec.Ports {
			taken = append(taken, p.NodePort)
		}
		for _, n := range taken {
			if n != 0 && want[int64(n)] {
				held = append(held, fmt.Sprintf("%d by %s/%s", n, svc.Namespace, svc.Name))
			}
		}
	}
	if len(held) == 0 {
		return "", nil
	}
	slices.Sort(held)
	for i, p := range ports {
		if m, ok := p.(map[string]any); ok {
			delete(m, "nodePort")
			ports[i] = m
		}
	}
	if err := unstructured.SetNestedSlice(obj.Object, ports, "spec", "ports"); err != nil {
		return "", err
	}
	if err := unstructured.SetNestedField(obj.Object, string(corev1.ServiceTypeClusterIP), "spec", "type"); err != nil {
		return "", err
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[dashboardNodePortHeldKey] = strings.Join(held, ", ")
	obj.SetAnnotations(ann)
	return fmt.Sprintf("NodePort %s is held, so the dashboard is served on a ClusterIP only; "+
		"set spec.dashboard.nodePort to a free port on every control plane and restart `stratum up`, "+
		"or free the port, which restores the NodePort on the next pass",
		strings.Join(held, ", ")), nil
}

// isDashboardService reports whether obj is the dashboard stack's Service.
func isDashboardService(obj *unstructured.Unstructured) bool {
	return obj.GetKind() == "Service" && obj.GetNamespace() == addons.DashboardNamespace && obj.GetName() == addons.DashboardName
}
