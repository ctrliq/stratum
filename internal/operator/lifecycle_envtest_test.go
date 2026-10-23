package operator

import (
	"os"
	"path/filepath"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/ctrliq/stratum/internal/addons"
)

// The lifecycle integration check must retire a bundle through recorded intent,
// not a hand deletion that the desired-generation gate deliberately refuses.
// Exercise both cases against real storage and server-side apply with the actual
// local-path bundle, without waiting for a kubelet, CNI, or Helm installation.
func TestLocalPathPrunesOnlyAfterRecordedRemoval(t *testing.T) {
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
	if _, err := cs.CoreV1().Nodes().Create(t.Context(), cpNode("cp1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bundles := map[string][]byte{
		"local-path/local-path.yaml": addons.RenderLocalPath(),
		"kept/config.yaml":           []byte(configMap("kept", "kube-system")),
	}
	for rel, data := range bundles {
		writeManifest(t, filepath.Join(dir, rel), string(data))
	}
	staged := StagedGeneration{
		PublicationID: "storage-enabled", Digest: DigestBundles(bundles),
		Stacks: []string{"kept", "local-path"}, Inputs: ClusterInputs{StorageDefault: addons.StorageLocalPath},
	}
	before, err := RecordStagedGeneration(t.Context(), cs, staged, "")
	if err != nil {
		t.Fatal(err)
	}
	deployer, err := newManifestDeployer(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	deployer.node = "cp1"
	deployer.reconcileManifests(t.Context(), "", "")
	if _, err := cs.StorageV1().StorageClasses().Get(t.Context(), "local-path", metav1.GetOptions{}); err != nil {
		t.Fatalf("initial bundle must create the StorageClass: %v", err)
	}

	// Reproduce the failed CI scenario: a local deletion leaves desired intent
	// unchanged. It cannot authorize cluster-wide removal.
	if err := os.RemoveAll(filepath.Join(dir, "local-path")); err != nil {
		t.Fatal(err)
	}
	deployer.reconcileManifests(t.Context(), "", "")
	if _, err := cs.StorageV1().StorageClasses().Get(t.Context(), "local-path", metav1.GetOptions{}); err != nil {
		t.Fatalf("unrecorded disk deletion must preserve the StorageClass: %v", err)
	}
	inv, err := loadInventory(t.Context(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Desired.PublicationID != before.ID {
		t.Fatal("a refused pass changed the desired publication")
	}

	// A config-driven stratum up stages this reduced set and records storage
	// disabled. A fresh operator must prune from the retained cluster inventory.
	delete(bundles, "local-path/local-path.yaml")
	staged.PublicationID = "storage-disabled"
	staged.Digest = DigestBundles(bundles)
	staged.Stacks = []string{"kept"}
	staged.Inputs.StorageDefault = addons.StorageNone
	if _, err := RecordStagedGeneration(t.Context(), cs, staged, before.ID); err != nil {
		t.Fatal(err)
	}
	deployer, err = newManifestDeployer(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	deployer.node = "cp1"
	deployer.reconcileManifests(t.Context(), "", "")
	if _, err := cs.StorageV1().StorageClasses().Get(t.Context(), "local-path", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("recorded removal must prune the StorageClass, got %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "kept", metav1.GetOptions{}); err != nil {
		t.Fatalf("remaining stack must survive pruning: %v", err)
	}
}
