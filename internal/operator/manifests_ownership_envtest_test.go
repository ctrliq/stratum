package operator

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestBundledOwnershipSurvivesPartialApplyAndRestart(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make test-envtest`")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A writable bookkeeping label must not make this namespace or its data
	// belong to the deployer, even while it cleans up the same named stack.
	if _, err := cs.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated-tenant", Labels: map[string]string{group + "/stack": "dashboard"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ConfigMaps("unrelated-tenant").Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "tenant-data"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "dashboard", "partial.yaml")
	writeManifest(t, bundle, configMap("partially-applied", "kube-system")+"---\ninvalid: [unclosed\n")
	d, err := newManifestDeployer(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	d.reconcileManifests(t.Context(), "", "")
	obj, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "partially-applied", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("first document must apply before the later parse failure: %v", err)
	}
	// Prove cleanup uses trusted history even after the visible label changes.
	obj.Labels = nil
	if _, err := cs.CoreV1().ConfigMaps("kube-system").Update(t.Context(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, filepath.Join(dir, "remaining", "keep.yaml"), configMap("kept", "kube-system"))
	d, err = newManifestDeployer(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	d.reconcileManifests(t.Context(), "", "")
	if _, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "partially-applied", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("fresh deployer must prune the partial apply from durable history: %v", err)
	}
	ns, err := cs.CoreV1().Namespaces().Get(t.Context(), "unrelated-tenant", metav1.GetOptions{})
	if err != nil || ns.DeletionTimestamp != nil {
		t.Fatalf("labelled unrelated namespace must not be deleted: namespace=%+v, error=%v", ns, err)
	}
	if _, err := cs.CoreV1().ConfigMaps("unrelated-tenant").Get(t.Context(), "tenant-data", metav1.GetOptions{}); err != nil {
		t.Fatalf("unrelated namespace data must survive: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "kept", metav1.GetOptions{}); err != nil {
		t.Fatalf("the remaining stack must be applied: %v", err)
	}
}
