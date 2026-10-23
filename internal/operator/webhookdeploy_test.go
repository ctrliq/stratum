package operator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func TestBuildWebhookConfig(t *testing.T) {
	// kube-system is the default privileged set.
	cfg := buildWebhookConfig([]byte("CABUNDLE"), []string{"kube-system"})
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
	}
	wh := cfg.Webhooks[0]

	// Fail prevents replaying old signatures during an admission outage.
	if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregistrationv1.Fail {
		t.Errorf("failurePolicy = %v, want Fail", wh.FailurePolicy)
	}
	if wh.SideEffects == nil || *wh.SideEffects != admissionregistrationv1.SideEffectClassNone {
		t.Errorf("sideEffects = %v, want None", wh.SideEffects)
	}
	if !slices.Equal(wh.AdmissionReviewVersions, []string{"v1"}) {
		t.Errorf("admissionReviewVersions = %v", wh.AdmissionReviewVersions)
	}
	if string(wh.ClientConfig.CABundle) != "CABUNDLE" {
		t.Errorf("caBundle not threaded through")
	}
	if wh.ClientConfig.Service == nil {
		t.Fatal("clientConfig.service is nil")
	}
	if wh.ClientConfig.Service.Name != webhookServiceName || wh.ClientConfig.Service.Namespace != webhookNamespace {
		t.Errorf("service ref = %+v", wh.ClientConfig.Service)
	}
	if wh.ClientConfig.Service.Path == nil || *wh.ClientConfig.Service.Path != webhookPath {
		t.Errorf("service path = %v, want %s", wh.ClientConfig.Service.Path, webhookPath)
	}

	// Rules cover both CRs on CREATE + UPDATE.
	if len(wh.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(wh.Rules))
	}
	r := wh.Rules[0]
	if !slices.Equal(r.Resources, []string{"helmcharts", "helmchartconfigs"}) {
		t.Errorf("resources = %v", r.Resources)
	}
	// The rule must match the CRDs' group/version, which k8sutil declares once.
	if !slices.Equal(r.APIGroups, []string{k8sutil.HelmChartGroup}) {
		t.Errorf("apiGroups = %v", r.APIGroups)
	}
	if !slices.Equal(r.APIVersions, []string{k8sutil.HelmChartVersion}) {
		t.Errorf("apiVersions = %v", r.APIVersions)
	}
	ops := r.Operations
	if !slices.Contains(ops, admissionregistrationv1.Create) || !slices.Contains(ops, admissionregistrationv1.Update) {
		t.Errorf("operations = %v, want CREATE+UPDATE", ops)
	}

	// The default privileged set is exempt so add-on bring-up never waits on
	// the webhook.
	if wh.NamespaceSelector == nil || len(wh.NamespaceSelector.MatchExpressions) != 1 {
		t.Fatalf("namespaceSelector = %+v", wh.NamespaceSelector)
	}
	me := wh.NamespaceSelector.MatchExpressions[0]
	if me.Key != corev1.LabelMetadataName || me.Operator != metav1.LabelSelectorOpNotIn || !slices.Equal(me.Values, []string{"kube-system"}) {
		t.Errorf("namespaceSelector must exempt exactly kube-system: %+v", me)
	}
}

func TestBuildWebhookServiceHasNoSelector(t *testing.T) {
	svc := buildWebhookService()
	if len(svc.Spec.Selector) != 0 {
		t.Errorf("service must have NO selector (endpoints are operator-managed): %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != webhookServicePort {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	if got := svc.Spec.Ports[0].TargetPort.IntValue(); got != webhookPort {
		t.Errorf("targetPort = %d, want %d", got, webhookPort)
	}
}

func TestBuildEndpointSlice(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		es := buildEndpointSlice("cp-1", "10.0.0.5")
		if es.AddressType != discoveryv1.AddressTypeIPv4 {
			t.Errorf("addressType = %v", es.AddressType)
		}
		if es.Labels[discoveryv1.LabelServiceName] != webhookServiceName {
			t.Errorf("missing service-name label: %v", es.Labels)
		}
		if len(es.Endpoints) != 1 || !slices.Equal(es.Endpoints[0].Addresses, []string{"10.0.0.5"}) {
			t.Errorf("endpoints = %+v", es.Endpoints)
		}
		if len(es.Ports) != 1 || es.Ports[0].Port == nil || *es.Ports[0].Port != webhookPort {
			t.Errorf("ports = %+v", es.Ports)
		}
	})
	t.Run("IPv6", func(t *testing.T) {
		es := buildEndpointSlice("cp-1", "fd00::5")
		if es.AddressType != discoveryv1.AddressTypeIPv6 {
			t.Errorf("addressType = %v, want IPv6", es.AddressType)
		}
	})
	t.Run("dual-stack uses the primary (first) address", func(t *testing.T) {
		es := buildEndpointSlice("cp-1", "10.0.0.5,fd00::5")
		if es.AddressType != discoveryv1.AddressTypeIPv4 || es.Endpoints[0].Addresses[0] != "10.0.0.5" {
			t.Errorf("dual-stack primary = %v/%v", es.AddressType, es.Endpoints[0].Addresses)
		}
	})
}

func TestPrimaryIP(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.5":           "10.0.0.5",
		"10.0.0.5,fd00::5":   "10.0.0.5",
		" 10.0.0.5 ,fd00::5": "10.0.0.5",
		"fd00::5":            "fd00::5",
	} {
		if got := primaryIP(in); got != want {
			t.Errorf("primaryIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"cp-1":            "cp-1",
		"CP-1.Example":    "cp-1.example",
		"node_with_under": "node-with-under",
		"--weird--":       "weird",
		"":                "node",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureWebhookServiceCreateThenReassert(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if err := ensureWebhookService(ctx, cs); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc, err := cs.CoreV1().Services(webhookNamespace).Get(ctx, webhookServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service not created: %v", err)
	}
	// Simulate an externally-set ClusterIP; a re-assert must preserve it.
	svc.Spec.ClusterIP = "10.96.0.42"
	if _, err := cs.CoreV1().Services(webhookNamespace).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := ensureWebhookService(ctx, cs); err != nil {
		t.Fatalf("re-assert: %v", err)
	}
	svc, err = cs.CoreV1().Services(webhookNamespace).Get(ctx, webhookServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Spec.ClusterIP != "10.96.0.42" {
		t.Errorf("re-assert clobbered the immutable ClusterIP: %q", svc.Spec.ClusterIP)
	}
}

func TestEnsureEndpointSliceLifecycle(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if err := ensureEndpointSlice(ctx, cs, "cp-1", "10.0.0.5"); err != nil {
		t.Fatalf("create: %v", err)
	}
	name := endpointSliceName("cp-1")
	es, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("slice not created: %v", err)
	}
	if es.Endpoints[0].Addresses[0] != "10.0.0.5" {
		t.Errorf("endpoint address = %v", es.Endpoints[0].Addresses)
	}
	// Re-ensure with a changed IP updates in place.
	if err := ensureEndpointSlice(ctx, cs, "cp-1", "10.0.0.9"); err != nil {
		t.Fatalf("update: %v", err)
	}
	es, _ = cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(ctx, name, metav1.GetOptions{})
	if es.Endpoints[0].Addresses[0] != "10.0.0.9" {
		t.Errorf("endpoint not updated: %v", es.Endpoints[0].Addresses)
	}
	// Deregister removes it.
	if err := deleteEndpointSlice(ctx, cs, "cp-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("slice still present after deregister")
	}
}

func TestEnsureEndpointSliceReplacesOnAddressFamilyChange(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if err := ensureEndpointSlice(ctx, cs, "cp-1", "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	// AddressType is immutable; flipping the node to IPv6 must replace the slice.
	if err := ensureEndpointSlice(ctx, cs, "cp-1", "fd00::5"); err != nil {
		t.Fatalf("family change: %v", err)
	}
	es, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(ctx, endpointSliceName("cp-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if es.AddressType != discoveryv1.AddressTypeIPv6 || es.Endpoints[0].Addresses[0] != "fd00::5" {
		t.Errorf("slice not replaced for new family: %v/%v", es.AddressType, es.Endpoints[0].Addresses)
	}
}

func TestPruneWebhookEndpoints(t *testing.T) {
	now := time.Now().UTC()
	live := buildEndpointSlice("live", "192.0.2.1")
	live.Annotations[webhookHeartbeatAnnotation] = now.Format(time.RFC3339Nano)
	stale := buildEndpointSlice("stale", "192.0.2.2")
	stale.Annotations[webhookHeartbeatAnnotation] = now.Add(-webhookEndpointTTL - time.Second).Format(time.RFC3339Nano)
	stale.UID = "old-uid"
	stale.ResourceVersion = "42"
	legacy := buildEndpointSlice("legacy", "192.0.2.3")
	legacy.Annotations = nil
	foreign := buildEndpointSlice("foreign", "192.0.2.4")
	foreign.Labels[discoveryv1.LabelServiceName] = "another-service"
	foreign.Annotations = nil
	cs := fake.NewSimpleClientset(live, stale, legacy, foreign)
	if err := pruneWebhookEndpoints(context.Background(), cs, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{live.Name, foreign.Name} {
		if _, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("removed healthy/unowned endpoint %s: %v", name, err)
		}
	}
	for _, name := range []string{stale.Name, legacy.Name} {
		if _, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("stale endpoint remains: %s", name)
		}
	}
	for _, action := range cs.Actions() {
		if d, ok := action.(ktesting.DeleteAction); ok && d.GetName() == stale.Name {
			p := d.GetDeleteOptions().Preconditions
			if p == nil || p.UID == nil || *p.UID != stale.UID || p.ResourceVersion == nil || *p.ResourceVersion != stale.ResourceVersion {
				t.Fatal("pruning can race a renewed heartbeat")
			}
		}
	}
}

func TestEndpointHeartbeatRenewalPreventsPruning(t *testing.T) {
	old := buildEndpointSlice("cp1", "192.0.2.1")
	old.Annotations[webhookHeartbeatAnnotation] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	cs := fake.NewSimpleClientset(old)
	if err := ensureEndpointSlice(context.Background(), cs, "cp1", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := pruneWebhookEndpoints(context.Background(), cs, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).Get(context.Background(), old.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("renewed endpoint pruned: %v", err)
	}
}

func TestPruningConcurrentRenewalIsNotAnError(t *testing.T) {
	old := buildEndpointSlice("cp1", "192.0.2.1")
	old.Annotations = nil
	cs := fake.NewSimpleClientset(old)
	cs.PrependReactor("delete", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, old.Name, fmt.Errorf("heartbeat renewed"))
	})
	if err := pruneWebhookEndpoints(context.Background(), cs, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestMintServingCertUsesTheStagedSignerForALostKey: between a forced
// trust-new and sign-new, ca.key is gone and the staged root signs the webhook.
func TestMintServingCertUsesTheStagedSignerForALostKey(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	if _, err := pki.NewCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	staged, err := pki.NewCA("ca-new", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := mintServingCert("")
	if err != nil {
		t.Fatalf("mintServingCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.CheckSignatureFrom(staged.Cert); err != nil {
		t.Errorf("the serving cert is not signed by ca-new: %v", err)
	}
}

// TestWebhookServingCertNamesTheClusterDomain: the apiserver may reach the
// webhook by its Service's full name, which on a custom-domain cluster ends in
// that domain rather than the default one. An operator started without a
// domain names the default.
func TestWebhookServingCertNamesTheClusterDomain(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	if _, err := pki.NewCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	const svc = "stratum-operator-webhook.kube-system.svc"
	for _, tc := range []struct {
		domain string
		want   string
	}{
		{"fuzz.internal", svc + ".fuzz.internal"},
		{"", svc + ".cluster.local"},
	} {
		pair, err := mintServingCert(tc.domain)
		if err != nil {
			t.Fatalf("mintServingCert(%q): %v", tc.domain, err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(leaf.DNSNames, tc.want) {
			t.Errorf("domain %q: DNS names %v lack %s", tc.domain, leaf.DNSNames, tc.want)
		}
		// The domain-free Service name stays whatever the domain.
		if !slices.Contains(leaf.DNSNames, svc) {
			t.Errorf("domain %q: DNS names %v lack %s", tc.domain, leaf.DNSNames, svc)
		}
		if tc.domain == "" {
			continue
		}
		for _, name := range leaf.DNSNames {
			if strings.HasSuffix(name, ".svc.cluster.local") {
				t.Errorf("domain %q: DNS names %v still name the default domain (%s)", tc.domain, leaf.DNSNames, name)
			}
		}
	}
}

// TestWebhookCertRefreshNamesTheClusterDomain: the periodic re-mint that picks
// up a CA rotation names the cluster's domain, as the first mint did, so a
// custom-domain cluster's webhook does not fall back to the default domain on
// its first refresh.
func TestWebhookCertRefreshNamesTheClusterDomain(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	ca, err := pki.NewCA("ca", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	oldInterval := certRefreshInterval
	certRefreshInterval = 10 * time.Millisecond
	t.Cleanup(func() { certRefreshInterval = oldInterval })

	// maintainWebhook never mints the first cert (runWebhookOnce does, before
	// calling it), so a cert in this empty cache can only come from a refresh.
	cache := &certCache{domain: "fuzz.internal"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maintainWebhook(ctx, fake.NewSimpleClientset(), "cp1", "192.0.2.1", cache, func(ctx context.Context) error {
		poll := time.NewTicker(time.Millisecond)
		defer poll.Stop()
		for {
			if _, err := cache.get(nil); err == nil {
				return errors.New("serving cert refreshed")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-poll.C:
			}
		}
	}, nil)

	pair, err := cache.get(nil)
	if err != nil {
		t.Fatalf("no refresh re-minted the serving cert within 5s: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Errorf("the refreshed cert is not signed by the cluster CA: %v", err)
	}
	const want = "stratum-operator-webhook.kube-system.svc.fuzz.internal"
	if !slices.Contains(leaf.DNSNames, want) {
		t.Errorf("refreshed DNS names %v lack %s", leaf.DNSNames, want)
	}
	for _, name := range leaf.DNSNames {
		if strings.HasSuffix(name, ".svc.cluster.local") {
			t.Errorf("refreshed DNS names %v still name the default domain (%s)", leaf.DNSNames, name)
		}
	}
}

// TestWebhookServesACertNamingTheClusterDomain: the domain runWebhook is given
// reaches the cert the webhook's first bring-up mints and serves, not only the
// refresh, so from its first handshake a custom-domain cluster's webhook
// verifies for its Service's full name in that domain.
func TestWebhookServesACertNamingTheClusterDomain(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	ca, err := pki.NewCA("ca", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	bound := make(chan string, 1)
	oldListen := webhookListen
	webhookListen = func() (net.Listener, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			bound <- l.Addr().String()
		}
		return l, err
	}
	t.Cleanup(func() { webhookListen = oldListen })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWebhook(ctx, fake.NewSimpleClientset(), "cp1", "192.0.2.1", "fuzz.internal", newRequesterSignerHolder(), nil)
	}()
	t.Cleanup(func() { cancel(); <-done })

	var addr string
	select {
	case addr = <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook never bound its port")
	}

	// The webhook requires a client certificate this test does not have, so
	// the handshake is stopped once the served certificate has been verified.
	const want = "stratum-operator-webhook.kube-system.svc.fuzz.internal"
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	verified := false
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: want, MinVersion: tls.VersionTLS12,
		VerifyConnection: func(tls.ConnectionState) error {
			verified = true
			return errors.New("served certificate verified")
		}})
	if err == nil {
		conn.Close()
	}
	if !verified {
		t.Fatalf("the webhook's served certificate does not verify for %s: %v", want, err)
	}
}
