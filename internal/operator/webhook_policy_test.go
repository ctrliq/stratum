package operator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestWebhookNamespaceSelectorDerivesFromPrivilegedNamespaces pins the
// privileged-set contract: the MutatingWebhookConfiguration's NamespaceSelector
// exempts exactly the normalized privileged set — the same
// Options.PrivilegedNamespaces whose HelmCharts may install cross-namespace —
// so one option governs install authorization and admission exemption.
// failurePolicy stays Fail for every set shape, so tenant writes keep failing
// closed.
func TestWebhookNamespaceSelectorDerivesFromPrivilegedNamespaces(t *testing.T) {
	cases := []struct {
		name string
		// privileged is the configured set, as the operator option carries it.
		privileged []string
		// wantValues is the expected NotIn values on
		// kubernetes.io/metadata.name; nil means the namespaceSelector is
		// omitted entirely.
		wantValues []string
	}{
		{"default set exempts kube-system", []string{"kube-system"}, []string{"kube-system"}},
		{"replaced set exempts that set", []string{"platform"}, []string{"platform"}},
		{"added namespace joins kube-system", []string{"kube-system", "platform"}, []string{"kube-system", "platform"}},
		{"values are the configured set in order", []string{"platform", "kube-system"}, []string{"platform", "kube-system"}},
		{"empty set omits the namespaceSelector", nil, nil},
		{"empty non-nil set also omits it", []string{}, nil},
		{"whitespace, empties and duplicates normalize once", []string{" kube-system ", "", "kube-system", "platform", "platform "}, []string{"kube-system", "platform"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := buildWebhookConfig([]byte("CABUNDLE"), tc.privileged)
			if len(cfg.Webhooks) != 1 {
				t.Fatalf("want 1 webhook, got %d", len(cfg.Webhooks))
			}
			wh := cfg.Webhooks[0]
			if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregistrationv1.Fail {
				t.Errorf("failurePolicy = %v, want Fail", wh.FailurePolicy)
			}
			if tc.wantValues == nil {
				// The selector must be OMITTED, not rendered as a valueless
				// NotIn (which fails apiserver validation): the webhook then
				// matches every namespace, so unstamped writes are denied
				// everywhere — the reconciler's empty-set deny-all reading.
				if wh.NamespaceSelector != nil {
					t.Errorf("namespaceSelector = %+v, want omitted", wh.NamespaceSelector)
				}
				return
			}
			if wh.NamespaceSelector == nil || len(wh.NamespaceSelector.MatchExpressions) != 1 {
				t.Fatalf("namespaceSelector = %+v, want one match expression", wh.NamespaceSelector)
			}
			me := wh.NamespaceSelector.MatchExpressions[0]
			if me.Key != corev1.LabelMetadataName || me.Operator != metav1.LabelSelectorOpNotIn {
				t.Errorf("matchExpression = %+v, want NotIn on %s", me, corev1.LabelMetadataName)
			}
			if !slices.Equal(me.Values, tc.wantValues) {
				t.Errorf("notIn values = %v, want %v (the normalized privileged set)", me.Values, tc.wantValues)
			}
		})
	}
}

// TestMaintainWebhookThreadsWithThePrivilegedSet pins the last hop of the
// privileged-set threading that the pure-builder tables cannot see: the
// privileged set handed to maintainWebhook (as runWebhook passes it) reaches
// the live MutatingWebhookConfiguration through ensureWebhookObjects and
// ensureWebhookConfig. The parameter is a required slice, so a refactor that
// stops forwarding it no longer compiles; this test additionally pins the hops
// the compiler cannot see — that the value maintainWebhook receives reaches
// buildWebhookConfig through ensureWebhookObjects and ensureWebhookConfig.
func TestMaintainWebhookThreadsWithThePrivilegedSet(t *testing.T) {
	// ensureWebhookObjects reads the CA bundle from the PKI dir; redirect it so
	// the ensure step renders the config instead of logging a read failure.
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), []byte("CABUNDLE"), 0o600); err != nil {
		t.Fatal(err)
	}

	cs := fake.NewSimpleClientset()
	var mu sync.Mutex
	var created *admissionregistrationv1.MutatingWebhookConfiguration
	sawCreate := make(chan struct{})
	cs.PrependReactor("create", "mutatingwebhookconfigurations", func(action ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		created = action.(ktesting.CreateAction).GetObject().(*admissionregistrationv1.MutatingWebhookConfiguration).DeepCopy()
		mu.Unlock()
		close(sawCreate)
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maintainWebhook(ctx, cs, "cp1", "192.0.2.1", &certCache{}, func(ctx context.Context) error {
		select {
		case <-sawCreate:
			return errors.New("webhook configuration delivered")
		case <-ctx.Done():
			return ctx.Err()
		}
	}, []string{"kube-system", "platform"})

	mu.Lock()
	defer mu.Unlock()
	if created == nil {
		t.Fatal("maintainWebhook never created the MutatingWebhookConfiguration: the privileged set did not reach buildWebhookConfig")
	}
	if len(created.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(created.Webhooks))
	}
	wh := created.Webhooks[0]
	if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregistrationv1.Fail {
		t.Errorf("failurePolicy = %v, want Fail", wh.FailurePolicy)
	}
	if wh.NamespaceSelector == nil || len(wh.NamespaceSelector.MatchExpressions) != 1 {
		t.Fatalf("namespaceSelector = %+v, want one match expression", wh.NamespaceSelector)
	}
	me := wh.NamespaceSelector.MatchExpressions[0]
	if me.Key != corev1.LabelMetadataName || me.Operator != metav1.LabelSelectorOpNotIn {
		t.Errorf("matchExpression = %+v, want NotIn on %s", me, corev1.LabelMetadataName)
	}
	if !slices.Equal(me.Values, []string{"kube-system", "platform"}) {
		t.Errorf("notIn values = %v, want [kube-system platform] (the set maintainWebhook was handed)", me.Values)
	}
}

// TestEnsureWebhookConfigSelfHealsSelector pins that a changed privileged set
// reaches an existing cluster through ensureWebhookConfig's wholesale webhook
// update, so a changed selector self-heals without migration code; the create
// path renders the same selector.
func TestEnsureWebhookConfigSelfHealsSelector(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if err := ensureWebhookConfig(ctx, cs, []byte("CABUNDLE"), []string{"kube-system"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The operator restarts configured with a replaced privileged set.
	if err := ensureWebhookConfig(ctx, cs, []byte("CABUNDLE"), []string{"platform"}); err != nil {
		t.Fatalf("re-assert with replaced set: %v", err)
	}
	cfg, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, webhookConfigName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sel := cfg.Webhooks[0].NamespaceSelector
	if sel == nil || len(sel.MatchExpressions) != 1 || !slices.Equal(sel.MatchExpressions[0].Values, []string{"platform"}) {
		t.Errorf("selector not healed to the replaced set: %+v", sel)
	}
	// A later empty set drops the selector from the live object too.
	if err := ensureWebhookConfig(ctx, cs, []byte("CABUNDLE"), nil); err != nil {
		t.Fatalf("re-assert with empty set: %v", err)
	}
	cfg, err = cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, webhookConfigName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhooks[0].NamespaceSelector != nil {
		t.Errorf("namespaceSelector = %+v, want omitted after the empty set", cfg.Webhooks[0].NamespaceSelector)
	}
}
