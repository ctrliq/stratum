package operator

// A supervised host process, not a pod: no Service selector can find it.

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
	webhookServicePort = 443
	// Restores a deleted delivery object promptly, not a cert-refresh window later.
	webhookEnsureInterval = time.Minute
	// Backs off bring-up while the CA is unreadable or the apiserver is not up.
	webhookRetryInterval       = 30 * time.Second
	webhookHeartbeatAnnotation = group + "/webhook-heartbeat"
	webhookEndpointTTL         = 3 * webhookEnsureInterval
)

// Re-mints the serving cert so a CA rotation needs no restart; variable for tests.
var certRefreshInterval = 12 * time.Hour

// A variable so tests can serve on a free loopback port.
var webhookListen = func() (net.Listener, error) { return net.Listen("tcp", fmt.Sprintf(":%d", webhookPort)) }

// The apiserver validates the cert against the Service DNS name even over an endpoint IP.
func webhookDNSNames(domain string) []string {
	svc := webhookServiceName + "." + webhookNamespace + ".svc"
	return []string{svc, svc + "." + netcfg.DomainOr(domain)}
}

// domain is set at construction and never changes, so the lock does not guard it.
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

	// A missing CA here means this node has no PKI; the webhook never comes up.
	if !mintInto(ctx, cache) {
		return
	}

	listener, err := webhookListen()
	if err != nil {
		log.Printf("webhook listen: %v", err)
		return
	}
	defer listener.Close()
	maintainWebhook(ctx, cs, nodeName, nodeIP, cache, func(ctx context.Context) error { return runWebhookServer(ctx, listener, cache.get, signers) }, privileged)
}

// privileged is required so stopping its forwarding fails to compile, not silently.
func maintainWebhook(ctx context.Context, cs kubernetes.Interface, nodeName, nodeIP string, cache *certCache, serve func(context.Context) error, privileged []string) {
	// A stopped listener must stop endpoint publication as well.
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
			if cert, err := mintServingCert(cache.domain); err != nil {
				log.Printf("stratum-operator: refresh webhook serving cert: %v", err)
			} else {
				cache.set(cert)
			}
		}
	}
}

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

// Each object is asserted independently so one failure does not block the others.
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

// No selector: endpoints come from the operator-managed EndpointSlices.
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
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	// A stray selector would publish pod endpoints alongside the managed slices.
	cur.Spec.Ports = want.Spec.Ports
	cur.Spec.Selector = nil
	_, err = svcs.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

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
					// k8sutil owns the group/version pair so the rule cannot drift off the CRDs' group.
					APIGroups:   []string{k8sutil.HelmChartGroup},
					APIVersions: []string{k8sutil.HelmChartVersion},
					Resources:   []string{"helmcharts", "helmchartconfigs"},
					Scope:       &scope,
				},
			}},
			// Fail closes replay on CREATE: recreation is indistinguishable while the webhook is down.
			FailurePolicy:           ptr.To(admissionregistrationv1.Fail),
			MatchPolicy:             ptr.To(admissionregistrationv1.Equivalent),
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

// One slice per node, so instances never contend on a shared object.
func endpointSliceName(nodeName string) string {
	return webhookServiceName + "-" + sanitizeName(nodeName)
}

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
	// AddressType is immutable; a family flip must replace the slice.
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

// Preconditions keep a stale observation from deleting a slice renewed since.
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

// The EndpointSlice addressType is single-family, so one slice carries the primary address.
func primaryIP(nodeIP string) string {
	for _, p := range strings.Split(nodeIP, ",") {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return nodeIP
}

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
