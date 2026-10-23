package operator

// webhookdeploy.go — delivery of the admission webhook in stratum's host-process
// operator model.
//
// The operator is a supervised host process on the control-plane node(s), not an
// in-cluster Deployment, so there is no pod for a Service selector to find. The
// webhook is delivered instead by:
//
//   - a serving cert minted IN MEMORY from the cluster CA (webhookdeploy mints it
//     with pki.IssuePEM, re-minting periodically so a CA rotation is picked up
//     without a restart); the MutatingWebhookConfiguration's caBundle is the
//     cluster CA trust bundle, so the apiserver trusts the cert;
//   - a Service (kube-system/stratum-operator-webhook) with NO selector, whose
//     endpoints are supplied by...
//   - a per-node EndpointSlice this operator instance owns: it registers its own
//     node IP on start and removes it on shutdown. Every control-plane operator
//     runs the (stateless) webhook and registers its own slice, so admission is
//     highly available and needs no leader coupling — unlike a single
//     leader-managed endpoint, a lost leader does not take stamping down.
//
// Admission fails closed for tenant writes; the configured privileged
// namespaces are exempt. Endpoint publication stops when serving fails, and
// bring-up retries.

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/pki"
)

const (
	webhookNamespace   = "kube-system"
	webhookServiceName = "stratum-operator-webhook"
	webhookConfigName  = "stratum-operator-requester"
	// webhookServicePort is the port the apiserver dials on the Service; it maps
	// to the operator's webhookPort on the backing node.
	webhookServicePort = 443
	// webhookEnsureInterval re-asserts the delivery objects (Service, webhook
	// config, this node's EndpointSlice) so a deleted one is restored promptly
	// rather than leaving admission unrouted for a cert-refresh window.
	webhookEnsureInterval = time.Minute
	// webhookRetryInterval backs off the initial bring-up when the CA is not yet
	// readable or the apiserver is not yet up.
	webhookRetryInterval       = 30 * time.Second
	webhookHeartbeatAnnotation = group + "/webhook-heartbeat"
	webhookEndpointTTL         = 3 * webhookEnsureInterval
)

// certRefreshInterval re-mints the serving cert and re-asserts the caBundle so
// a CA rotation (which swaps the live signer) is picked up without a restart.
// It is a variable so tests can shrink the interval; production keeps the
// twelve-hour default.
var certRefreshInterval = 12 * time.Hour

// webhookListen binds the webhook's port on every address. It is a variable
// so tests can serve on a free loopback port instead.
var webhookListen = func() (net.Listener, error) { return net.Listen("tcp", fmt.Sprintf(":%d", webhookPort)) }

// webhookDNSNames are the serving-cert SANs — the names the apiserver uses to
// reach the Service, which is what it validates the cert against (it verifies the
// Service DNS name even when routed through an endpoint IP). The full name ends in
// the cluster's DNS domain; an empty domain names the default one.
func webhookDNSNames(domain string) []string {
	svc := webhookServiceName + "." + webhookNamespace + ".svc"
	return []string{svc, svc + "." + netcfg.DomainOr(domain)}
}

// certCache holds the current serving cert behind a read/write lock so the TLS
// server's GetCertificate callback returns the latest across a re-mint. domain is
// the cluster's DNS domain every cert it holds names, so the first mint and each
// re-mint agree; it is set at construction and never changes, so the lock does
// not guard it.
type certCache struct {
	mu     sync.RWMutex
	cert   *tls.Certificate
	domain string
}

func (c *certCache) set(cert *tls.Certificate) {
	c.mu.Lock()
	c.cert = cert
	c.mu.Unlock()
}

func (c *certCache) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return nil, fmt.Errorf("webhook serving cert not ready")
	}
	return c.cert, nil
}

// mintServingCert signs a fresh in-memory serving cert from the cluster CA, naming
// the Service in the cluster's DNS domain.
func mintServingCert(domain string) (*tls.Certificate, error) {
	ca, err := pki.LoadCA(pki.Signer("ca"))
	if err != nil {
		return nil, fmt.Errorf("load cluster CA: %w", err)
	}
	certPEM, keyPEM, err := ca.IssuePEM(pki.CertSpec{
		Name:       webhookServiceName,
		CN:         webhookServiceName,
		DNS:        webhookDNSNames(domain),
		ServerAuth: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sign serving cert: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load serving keypair: %w", err)
	}
	return &pair, nil
}

// runWebhook brings up and maintains the admission webhook for the lifetime of
// ctx: it serves admission over HTTPS, keeps the delivery objects (Service,
// MutatingWebhookConfiguration, this node's EndpointSlice) in place, and on
// shutdown deregisters this node's endpoint so the apiserver stops routing to a
// dead server. Bring-up failures are logged and retried; tenant admission remains
// unavailable until serving and delivery recover. domain is the cluster's DNS
// domain the serving cert names.
func runWebhook(ctx context.Context, cs kubernetes.Interface, nodeName, nodeIP, domain string, signers *requesterSignerHolder, privileged []string) {
	for ctx.Err() == nil {
		runWebhookOnce(ctx, cs, nodeName, nodeIP, domain, signers, privileged)
		select {
		case <-ctx.Done():
			return
		case <-time.After(webhookRetryInterval):
		}
	}
}

func runWebhookOnce(ctx context.Context, cs kubernetes.Interface, nodeName, nodeIP, domain string, signers *requesterSignerHolder, privileged []string) {
	cache := &certCache{domain: domain}

	// Block (retrying) until the first serving cert is minted — the server cannot
	// start without one. A missing CA here means this node is not a control-plane
	// node with PKI, in which case the webhook simply never comes up on it.
	if !mintInto(ctx, cache) {
		return // ctx cancelled during bring-up
	}

	// Bind before publishing a Ready endpoint. A bind failure never registers.
	listener, err := webhookListen()
	if err != nil {
		log.Printf("webhook listen: %v", err)
		return
	}
	defer listener.Close()
	maintainWebhook(ctx, cs, nodeName, nodeIP, cache, func(ctx context.Context) error { return runWebhookServer(ctx, listener, cache.get, signers) }, privileged)
}

// maintainWebhook serves the webhook and keeps the delivery objects in place
// until ctx is cancelled or serving stops. privileged is the configured
// privileged-namespace set, threaded through to buildWebhookConfig's
// NamespaceSelector. It is a required parameter rather than variadic so a
// refactor that stops forwarding the set fails to compile instead of silently
// omitting the namespace exemption; a caller that wants no exemption passes
// nil explicitly.
func maintainWebhook(ctx context.Context, cs kubernetes.Interface, nodeName, nodeIP string, cache *certCache, serve func(context.Context) error, privileged []string) {
	// A stopped listener must stop endpoint publication as well. The supervisor
	// loop retries the complete server/delivery lifecycle after deregistration.
	serverCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() {
		err := serve(serverCtx)
		if err != nil && serverCtx.Err() == nil {
			log.Printf("admission webhook stopped: %v", err)
		}
		stopServer()
		serverDone <- err
	}()
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := deleteEndpointSlice(delCtx, cs, nodeName); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("deregister webhook endpoint: %v", err)
		}
	}()

	// Assert the delivery objects, then keep them fresh. The delivery objects are
	// re-asserted on a short interval (not just the cert-refresh tick) so a deleted
	// Service, EndpointSlice, or webhook config is restored within a minute rather
	// than being absent for up to a cert-refresh window.
	ensureWebhookObjects(serverCtx, cs, nodeName, nodeIP, privileged)

	ensure := time.NewTicker(webhookEnsureInterval)
	defer ensure.Stop()
	certRefresh := time.NewTicker(certRefreshInterval)
	defer certRefresh.Stop()
	for {
		select {
		case <-serverCtx.Done():
			return
		case err := <-serverDone:
			log.Printf("stratum-operator: admission webhook server stopped: %v", err)
			return

		case <-ensure.C:
			ensureWebhookObjects(serverCtx, cs, nodeName, nodeIP, privileged)
		case <-certRefresh.C:
			// Re-mint so a CA rotation (new live signer) is picked up. A transient
			// failure just keeps the existing (still long-lived) cert.
			if cert, err := mintServingCert(cache.domain); err != nil {
				log.Printf("stratum-operator: refresh webhook serving cert: %v", err)
			} else {
				cache.set(cert)
			}
		}
	}
}

// mintInto mints a serving cert naming the cache's domain into the cache,
// retrying on failure until it succeeds or ctx is cancelled. Returns false only
// if ctx was cancelled first.
func mintInto(ctx context.Context, cache *certCache) bool {
	for {
		cert, err := mintServingCert(cache.domain)
		if err == nil {
			cache.set(cert)
			return true
		}
		log.Printf("stratum-operator: mint webhook serving cert: %v (retrying in %s)", err, webhookRetryInterval)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(webhookRetryInterval):
		}
	}
}

// ensureWebhookObjects asserts the Service, the MutatingWebhookConfiguration
// (with the current CA bundle), and this node's EndpointSlice. Each is
// best-effort and independently logged so one failing does not block the others.
func ensureWebhookObjects(ctx context.Context, cs kubernetes.Interface, nodeName, nodeIP string, privileged []string) {
	if err := ensureWebhookService(ctx, cs); err != nil {
		log.Printf("stratum-operator: ensure webhook Service: %v", err)
	}
	caBundle, err := pki.ReadCABundle("ca")
	if err != nil {
		log.Printf("stratum-operator: read CA bundle for webhook: %v", err)
	} else if err := ensureWebhookConfig(ctx, cs, caBundle, privileged); err != nil {
		log.Printf("stratum-operator: ensure MutatingWebhookConfiguration: %v", err)
	}
	if err := ensureEndpointSlice(ctx, cs, nodeName, nodeIP); err != nil {
		log.Printf("stratum-operator: ensure webhook EndpointSlice: %v", err)
	}
	if err := pruneWebhookEndpoints(ctx, cs, time.Now()); err != nil {
		log.Printf("stratum-operator: prune webhook endpoints: %v", err)
	}
}

// buildWebhookService is the pure Service builder (no selector: endpoints come
// from the operator-managed EndpointSlices).
func buildWebhookService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookServiceName,
			Namespace: webhookNamespace,
			Labels:    map[string]string{group + "/managed-by": fieldManager},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "https",
				Port:       webhookServicePort,
				TargetPort: intstr.FromInt32(webhookPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func ensureWebhookService(ctx context.Context, cs kubernetes.Interface) error {
	svcs := cs.CoreV1().Services(webhookNamespace)
	want := buildWebhookService()
	cur, err := svcs.Get(ctx, webhookServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = svcs.Create(ctx, want, metav1.CreateOptions{})
		// Another control-plane operator won the create race; it converges next tick.
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	// Preserve the immutable ClusterIP; re-assert the ports and CLEAR any selector
	// on the fetched object. A stray selector would make kube-proxy publish
	// selector-matched pod endpoints alongside the operator-managed EndpointSlices,
	// routing admission to unintended pods.
	cur.Spec.Ports = want.Spec.Ports
	cur.Spec.Selector = nil
	_, err = svcs.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// buildWebhookConfig is the pure MutatingWebhookConfiguration builder. It is
// split out so the policy (failurePolicy, rules, namespace exemption) is
// unit-testable without a cluster. privileged is the configured set, normalized
// once here.
func buildWebhookConfig(caBundle []byte, privileged []string) *admissionregistrationv1.MutatingWebhookConfiguration {
	scope := admissionregistrationv1.NamespacedScope
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:   webhookConfigName,
			Labels: map[string]string{group + "/managed-by": fieldManager},
		},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name: "requester.stratum.ciq.com",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Namespace: webhookNamespace,
					Name:      webhookServiceName,
					Path:      ptr.To(webhookPath),
					Port:      ptr.To[int32](webhookServicePort),
				},
				CABundle: caBundle,
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{
					admissionregistrationv1.Create,
					admissionregistrationv1.Update,
				},
				Rule: admissionregistrationv1.Rule{
					// k8sutil owns the HelmChart group/version pair so the
					// rule cannot drift off the CRDs' group.
					APIGroups:   []string{k8sutil.HelmChartGroup},
					APIVersions: []string{k8sutil.HelmChartVersion},
					Resources:   []string{"helmcharts", "helmchartconfigs"},
					Scope:       &scope,
				},
			}},
			// Fail closes replay on CREATE: name/spec bindings cannot distinguish
			// recreation while the webhook is unavailable.
			FailurePolicy: ptr.To(admissionregistrationv1.Fail),
			MatchPolicy:   ptr.To(admissionregistrationv1.Equivalent),
			// Exempt exactly the configured privileged namespaces, normalized
			// once here: the same set whose HelmCharts may install as the
			// operator, so a privileged namespace's bring-up never waits on the
			// webhook and one option governs install authorization and
			// admission exemption. An empty set omits the selector — the webhook
			// then matches every namespace and unstamped writes fail closed,
			// matching the reconciler's deny-all reading of an empty set.
			NamespaceSelector:       privilegedNamespaceSelector(normalizePrivilegedNamespaces(privileged)),
			SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
			TimeoutSeconds:          ptr.To[int32](5),
			AdmissionReviewVersions: []string{"v1"},
			ReinvocationPolicy:      ptr.To(admissionregistrationv1.NeverReinvocationPolicy),
		}},
	}
}

func ensureWebhookConfig(ctx context.Context, cs kubernetes.Interface, caBundle []byte, privileged []string) error {
	cfgs := cs.AdmissionregistrationV1().MutatingWebhookConfigurations()
	want := buildWebhookConfig(caBundle, privileged)
	cur, err := cfgs.Get(ctx, webhookConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cfgs.Create(ctx, want, metav1.CreateOptions{})
		// Another control-plane operator won the create race; it converges next tick.
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	cur.Webhooks = want.Webhooks
	cur.Labels = want.Labels
	_, err = cfgs.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// endpointSliceName is this node's slice name — one slice per node so instances
// never contend on a shared object.
func endpointSliceName(nodeName string) string {
	return webhookServiceName + "-" + sanitizeName(nodeName)
}

// buildEndpointSlice is the pure EndpointSlice builder for one node's endpoint.
func buildEndpointSlice(nodeName, nodeIP string) *discoveryv1.EndpointSlice {
	ip := primaryIP(nodeIP)
	addrType := discoveryv1.AddressTypeIPv4
	if strings.Contains(ip, ":") {
		addrType = discoveryv1.AddressTypeIPv6
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:        endpointSliceName(nodeName),
			Annotations: map[string]string{webhookHeartbeatAnnotation: time.Now().UTC().Format(time.RFC3339Nano)},
			Namespace:   webhookNamespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: webhookServiceName,
				group + "/managed-by":        fieldManager,
			},
		},
		AddressType: addrType,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			NodeName:   ptr.To(nodeName),
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr.To("https"),
			Port:     ptr.To[int32](webhookPort),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
}

func ensureEndpointSlice(ctx context.Context, cs kubernetes.Interface, nodeName, nodeIP string) error {
	slices := cs.DiscoveryV1().EndpointSlices(webhookNamespace)
	want := buildEndpointSlice(nodeName, nodeIP)
	cur, err := slices.Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = slices.Create(ctx, want, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// AddressType is immutable; if it changed (a node that flipped IP family),
	// replace the slice rather than patch it.
	if cur.AddressType != want.AddressType {
		if err := slices.Delete(ctx, want.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		_, err = slices.Create(ctx, want, metav1.CreateOptions{})
		return err
	}
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[webhookHeartbeatAnnotation] = want.Annotations[webhookHeartbeatAnnotation]
	cur.Endpoints = want.Endpoints
	cur.Ports = want.Ports
	cur.Labels = want.Labels
	_, err = slices.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// Each serving instance renews its own heartbeat. Surviving instances remove
// stale peers after three missed renewals; graceful shutdown still deletes at
// once. Preconditions prevent a stale observation from deleting a renewed slice.
func pruneWebhookEndpoints(ctx context.Context, cs kubernetes.Interface, now time.Time) error {
	api := cs.DiscoveryV1().EndpointSlices(webhookNamespace)
	list, err := api.List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + webhookServiceName + "," + group + "/managed-by=" + fieldManager})
	if err != nil {
		return err
	}
	for _, slice := range list.Items {
		heartbeat, err := time.Parse(time.RFC3339Nano, slice.Annotations[webhookHeartbeatAnnotation])
		if err == nil && now.Sub(heartbeat) <= webhookEndpointTTL {
			continue
		}
		err = api.Delete(ctx, slice.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &slice.UID, ResourceVersion: &slice.ResourceVersion}})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

func deleteEndpointSlice(ctx context.Context, cs kubernetes.Interface, nodeName string) error {
	return cs.DiscoveryV1().EndpointSlices(webhookNamespace).Delete(ctx, endpointSliceName(nodeName), metav1.DeleteOptions{})
}

// primaryIP returns the first IP from a possibly comma-joined dual-stack node IP
// ("v4,v6"). The EndpointSlice addressType is single-family, so one slice carries
// the node's primary address; the apiserver reaches the webhook over it.
func primaryIP(nodeIP string) string {
	for _, p := range strings.Split(nodeIP, ",") {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return nodeIP
}

// sanitizeName lowercases and reduces s to an RFC-1123-safe object-name segment
// (a node's hostname is usually already valid; this guards the odd uppercase or
// underscore).
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "node"
	}
	if len(out) > 200 { // leave room under the 253-char object-name cap
		out = strings.Trim(out[:200], "-.")
	}
	return out
}
