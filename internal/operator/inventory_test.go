package operator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

func cpNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{k8sutil.ControlPlaneLabel: "true"},
	}}
}

func kubeCM(name string) objRef {
	return objRef{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: name}
}

func inventoryConfigMap(inv pruneInventory) (*corev1.ConfigMap, error) {
	b, err := json.Marshal(inv)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
		Data:       map[string]string{inventoryDataKey: string(b)},
	}, nil
}

func seedInventory(t *testing.T, cs *k8sfake.Clientset, inv pruneInventory) {
	t.Helper()
	cm, err := inventoryConfigMap(inv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ConfigMaps(inventoryNamespace).Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed inventory ConfigMap: %v", err)
	}
}

func clusterInventory(t *testing.T, cs *k8sfake.Clientset) pruneInventory {
	t.Helper()
	inv, err := loadInventory(context.Background(), cs)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	return inv
}

func TestInventoryMigrationUnionsDisjointInventories(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	dirA, dirB := t.TempDir(), t.TempDir()
	seedRecord(t, dirA, map[string][]objRef{"cilium": {kubeCM("agent"), kubeCM("config")}})
	seedRecord(t, dirB, map[string][]objRef{"coredns": {kubeCM("core")}})

	if err := syncInventory(ctx, cs, dirA, "cp1"); err != nil {
		t.Fatalf("publish cp1's local record: %v", err)
	}
	if err := syncInventory(ctx, cs, dirB, "cp2"); err != nil {
		t.Fatalf("publish cp2's local record: %v", err)
	}

	if _, err := cs.CoreV1().ConfigMaps(inventoryNamespace).Get(ctx, inventoryConfigMapName, metav1.GetOptions{}); err != nil {
		t.Fatalf("get %s/%s: %v", inventoryNamespace, inventoryConfigMapName, err)
	}

	inv := clusterInventory(t, cs)
	if got := inv.Nodes["cp1"]["cilium"]; len(got) != 2 {
		t.Errorf("cp1's entry under cilium: %+v, want agent and config", got)
	}
	if got := inv.Nodes["cp2"]["coredns"]; len(got) != 1 || got[0].Name != "core" {
		t.Errorf("cp2's entry under coredns: %+v, want core", got)
	}
	for _, name := range []string{"agent", "config", "core"} {
		found := false
		for _, ref := range inv.union() {
			if ref.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("union of the cluster inventory lacks %s; union: %+v", name, inv.union())
		}
	}
	if !inv.migrationComplete([]string{"cp1", "cp2"}) {
		t.Errorf("migration still open with both control planes contributed: %+v", inv)
	}

	if err := syncInventory(ctx, cs, dirA, "cp1"); err != nil {
		t.Fatalf("republish cp1's local record: %v", err)
	}
	inv = clusterInventory(t, cs)
	if got := inv.Nodes["cp1"]["cilium"]; len(got) != 2 {
		t.Errorf("cp1's entry after the republish: %+v, want agent and config once each", got)
	}
	if got := inv.Nodes["cp2"]["coredns"]; len(got) != 1 || got[0].Name != "core" {
		t.Errorf("cp2's entry after cp1's republish: %+v, want core intact", got)
	}
}

func TestInventoryPublishReunionsOnAConflict(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	seedInventory(t, cs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"cilium": {kubeCM("agent")}},
	}})

	conflicted := false
	gvr := corev1.SchemeGroupVersion.WithResource("configmaps")
	cs.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		stored, err := cs.Tracker().Get(gvr, inventoryNamespace, inventoryConfigMapName)
		if err != nil {
			return true, nil, err
		}
		cm := stored.(*corev1.ConfigMap).DeepCopy()
		concurrent := pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp2": {"coredns": {kubeCM("core")}},
		}}
		b, err := json.Marshal(concurrent)
		if err != nil {
			return true, nil, err
		}
		cm.Data[inventoryDataKey] = string(b)
		if err := cs.Tracker().Update(gvr, cm, inventoryNamespace); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: "", Resource: "configmaps"}, inventoryConfigMapName,
			errors.New("the object has been modified; please apply your changes to the latest version"))
	})

	err := publishInventory(ctx, cs, "cp1", map[string][]objRef{"cilium": {kubeCM("agent"), kubeCM("config")}})
	if err != nil {
		t.Fatalf("publish with a concurrent writer: %v; the conflict must be retried, not fail the pass", err)
	}

	inv := clusterInventory(t, cs)
	if got := inv.Nodes["cp1"]["cilium"]; len(got) != 2 {
		t.Errorf("cp1's entry after the retry: %+v, want agent and config — the local record must survive the retry", got)
	}
	if got := inv.Nodes["cp2"]["coredns"]; len(got) != 1 || got[0].Name != "core" {
		t.Errorf("cp2's entry after the retry: %+v, want core — the concurrent writer's entry must not be clobbered", got)
	}
}

func TestInventoryMigrationSuspendsUntilEveryControlPlaneContributed(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing contribution keeps the local record and the migration open", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(cpNode("cp1"), cpNode("cp2"))
		dir := t.TempDir()
		seedRecord(t, dir, map[string][]objRef{"cilium": {kubeCM("agent")}})

		if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
			t.Fatalf("publish cp1's local record: %v", err)
		}
		inv := clusterInventory(t, cs)
		if len(inv.Nodes["cp1"]["cilium"]) != 1 {
			t.Fatalf("cp1's entry: %+v, want agent", inv.Nodes["cp1"]["cilium"])
		}
		if inv.migrationComplete([]string{"cp1", "cp2"}) {
			t.Errorf("migration complete with cp2 uncontributed: %+v", inv)
		}
		if _, err := os.Stat(filepath.Join(dir, ".applied.json")); err != nil {
			t.Errorf("stat the local record: %v; the local record must not be retired while the migration is open", err)
		}
	})

	t.Run("the last contribution completes the migration and retires the local records", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(cpNode("cp1"), cpNode("cp2"))
		dirA, dirB := t.TempDir(), t.TempDir()
		seedRecord(t, dirA, map[string][]objRef{"cilium": {kubeCM("agent")}})
		seedRecord(t, dirB, map[string][]objRef{"coredns": {kubeCM("core")}})
		if err := syncInventory(ctx, cs, dirA, "cp1"); err != nil {
			t.Fatalf("publish cp1's local record: %v", err)
		}

		if err := syncInventory(ctx, cs, dirB, "cp2"); err != nil {
			t.Fatalf("publish cp2's local record: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dirB, ".applied.json")); !os.IsNotExist(err) {
			t.Errorf("stat cp2's local record: %v; the completed migration must retire it", err)
		}
		if _, err := os.Stat(filepath.Join(dirA, ".applied.json")); err != nil {
			t.Fatalf("stat cp1's local record: %v; cp1 has not synced since the migration closed", err)
		}

		if err := syncInventory(ctx, cs, dirA, "cp1"); err != nil {
			t.Fatalf("cp1's sync after the migration closed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dirA, ".applied.json")); !os.IsNotExist(err) {
			t.Errorf("stat cp1's local record: %v; the completed migration must retire it", err)
		}
		inv := clusterInventory(t, cs)
		if !inv.migrationComplete([]string{"cp1", "cp2"}) {
			t.Errorf("migration open after both nodes contributed: %+v", inv)
		}
		if got := inv.Nodes["cp2"]["coredns"]; len(got) != 1 || got[0].Name != "core" {
			t.Errorf("cp2's entry after the retirement: %+v, want core — the record must outlive its local file", got)
		}
	})

	t.Run("an explicit recovery entry closes the migration without the missing node", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(cpNode("cp1"))
		dir := t.TempDir()
		seedRecord(t, dir, map[string][]objRef{"cilium": {kubeCM("agent")}})
		seedInventory(t, cs, pruneInventory{
			Nodes:    map[string]map[string][]objRef{"cp1": {"cilium": {kubeCM("agent")}}},
			Recovery: true,
		})

		inv := clusterInventory(t, cs)
		if !inv.migrationComplete([]string{"cp1", "cp2"}) {
			t.Errorf("migration open despite the recovery entry: %+v", inv)
		}
		if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
			t.Fatalf("cp1's sync under recovery: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".applied.json")); !os.IsNotExist(err) {
			t.Errorf("stat the local record: %v; a closed migration must retire it", err)
		}
	})

	t.Run("an unreadable local record waits for explicit recovery", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset(cpNode("cp1"), cpNode("cp2"))
		seedInventory(t, cs, pruneInventory{})
		dir := t.TempDir()
		writeManifest(t, filepath.Join(dir, ".applied.json"), `{"cilium": [{"gvk":"v1,ConfigMap"`)

		if err := syncInventory(ctx, cs, dir, "cp1"); err == nil {
			t.Fatal("unreadable local inventory must keep migration open")
		}
		inv := clusterInventory(t, cs)
		if _, ok := inv.Nodes["cp1"]; ok {
			t.Errorf("inventory after the sync: %+v, want cp1's contribution still missing", inv)
		}
	})
}

func TestSyncInventoryAfterRetirementWritesNothing(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(cpNode("cp1"))
	dir := t.TempDir()
	seedRecord(t, dir, map[string][]objRef{"cilium": {kubeCM("agent")}})

	if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, appliedRecordName)); !os.IsNotExist(err) {
		t.Fatalf("stat the local record: %v; the completed migration must retire it", err)
	}

	cs.ClearActions()
	if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
		t.Fatalf("sync after retirement: %v", err)
	}
	for _, action := range cs.Actions() {
		if action.Matches("get", "configmaps") || action.Matches("list", "nodes") {
			continue
		}
		t.Errorf("a sync after retirement issued %s %s: the retired steady state must write nothing",
			action.GetVerb(), action.GetResource().Resource)
	}

	inv := clusterInventory(t, cs)
	if got := inv.Nodes["cp1"]["cilium"]; len(got) != 1 || got[0].Name != "agent" {
		t.Errorf("cp1's entry after the retired sync: %+v, want agent — the retired record's contribution must outlive it", got)
	}
	if !inv.migrationComplete([]string{"cp1"}) {
		t.Errorf("migration open after the retired sync: %+v", inv)
	}
}

func TestSyncInventoryPublishesAnEmptyEntryWhileTheMigrationIsOpen(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(cpNode("cp1"), cpNode("cp2"))
	dir := t.TempDir()

	if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
		t.Fatalf("sync without a local record: %v", err)
	}
	inv := clusterInventory(t, cs)
	if _, ok := inv.Nodes["cp1"]; !ok {
		t.Errorf("inventory after the sync: %+v, want cp1 contributed — an empty entry is a contribution", inv)
	}
	if inv.migrationComplete([]string{"cp1", "cp2"}) {
		t.Errorf("migration complete with cp2 uncontributed: %+v", inv)
	}
}

func TestWriteInventorySkipsANoOpUpdate(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	seedInventory(t, cs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"cilium": {kubeCM("agent")}},
	}})

	updates := 0
	cs.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})

	if err := publishInventory(ctx, cs, "cp1", map[string][]objRef{"cilium": {kubeCM("agent")}}); err != nil {
		t.Fatalf("no-op publish: %v", err)
	}
	if updates != 0 {
		t.Errorf("no-op publish issued %d updates; an identical document must not be written", updates)
	}

	if err := publishInventory(ctx, cs, "cp1", map[string][]objRef{"cilium": {kubeCM("config")}}); err != nil {
		t.Fatalf("publish a new reference: %v", err)
	}
	if updates != 1 {
		t.Errorf("publishing a new reference issued %d updates, want 1", updates)
	}
	inv := clusterInventory(t, cs)
	if got := inv.Nodes["cp1"]["cilium"]; len(got) != 2 {
		t.Errorf("cp1's entry: %+v, want agent and config", got)
	}
}

// The digest is one algorithm shared by the staging path and the deployer.
func TestDesiredDigestIsDeterministicAndSensitive(t *testing.T) {
	files := map[string][]byte{
		"crds/stratum-crds.yaml": []byte("apiVersion: v1\nkind: ConfigMap\n"),
		"cilium/cilium.yaml":     []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cilium\n"),
		"coredns/coredns.yaml":   []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: coredns\n"),
		"node-mark/policy.yaml":  []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: mark\n"),
		"importer/stack.yaml":    []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: imported\n"),
	}
	first := DigestBundles(files)
	if first == "" {
		t.Fatal("DigestBundles = empty, want a digest of the bundle set")
	}
	for i := 0; i < 20; i++ {
		if got := DigestBundles(files); got != first {
			t.Fatalf("DigestBundles of the same set = %q on walk %d, want the stable %q", got, i, first)
		}
	}

	changed := map[string][]byte{
		"crds/stratum-crds.yaml": files["crds/stratum-crds.yaml"],
		"cilium/cilium.yaml":     []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cilium\n  extra: \"1\"\n"),
		"coredns/coredns.yaml":   files["coredns/coredns.yaml"],
		"node-mark/policy.yaml":  files["node-mark/policy.yaml"],
		"importer/stack.yaml":    files["importer/stack.yaml"],
	}
	if got := DigestBundles(changed); got == first {
		t.Error("a changed byte in one bundle did not change the digest")
	}
	moved := map[string][]byte{
		"crds/stratum-crds.yaml": files["crds/stratum-crds.yaml"],
		"cilium/other.yaml":      files["cilium/cilium.yaml"],
		"coredns/coredns.yaml":   files["coredns/coredns.yaml"],
		"node-mark/policy.yaml":  files["node-mark/policy.yaml"],
		"importer/stack.yaml":    files["importer/stack.yaml"],
	}
	if got := DigestBundles(moved); got == first {
		t.Error("the same bytes under a different bundle path did not change the digest")
	}

	empty := DigestBundles(map[string][]byte{})
	if empty == "" {
		t.Fatal("DigestBundles of the empty set = empty, want the desired-empty digest")
	}
	for i := 0; i < 20; i++ {
		if got := DigestBundles(nil); got != empty {
			t.Fatalf("DigestBundles(nil) = %q on walk %d, want the empty set's stable digest %q", got, i, empty)
		}
	}
	if got := DigestBundles(map[string][]byte{"a.yaml": []byte("x")}); got == empty {
		t.Error("a one-file set digests like the empty set")
	}
}

func TestInventoryRecordsTheDesiredGeneration(t *testing.T) {
	ctx := context.Background()

	t.Run("the first staging opens the record at generation 1", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		gen, err := RecordDesiredGeneration(ctx, cs, "sha256:aaa", "stratum up (v1.2.3) on cp1", nil)
		if err != nil {
			t.Fatalf("record the first desired generation: %v", err)
		}
		if gen != 1 {
			t.Errorf("generation = %d, want 1 — a cluster's first record opens the sequence", gen)
		}
		inv := clusterInventory(t, cs)
		if inv.Desired == nil {
			t.Fatal("the inventory holds no desired record")
		}
		if inv.Desired.Digest != "sha256:aaa" {
			t.Errorf("digest = %q, want sha256:aaa — the digest of the rendered bundle set", inv.Desired.Digest)
		}
		if inv.Desired.Provenance != "stratum up (v1.2.3) on cp1" {
			t.Errorf("provenance = %q, want the staging that produced the generation", inv.Desired.Provenance)
		}
	})

	t.Run("re-staging the same bytes keeps the generation and its provenance", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		if _, err := RecordDesiredGeneration(ctx, cs, "sha256:aaa", "stratum up on cp1", nil); err != nil {
			t.Fatal(err)
		}
		gen, err := RecordDesiredGeneration(ctx, cs, "sha256:aaa", "stratum up on cp2, same bytes", nil)
		if err != nil {
			t.Fatalf("re-record the same digest: %v", err)
		}
		if gen != 1 {
			t.Errorf("generation = %d, want 1 — the generation moves only when the rendered bytes change", gen)
		}
		inv := clusterInventory(t, cs)
		if inv.Desired.Provenance != "stratum up on cp1" {
			t.Errorf("provenance = %q, want the staging that produced generation 1 — an unchanged digest records nothing new", inv.Desired.Provenance)
		}
	})

	t.Run("changed bytes record the next generation", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		if _, err := RecordDesiredGeneration(ctx, cs, "sha256:aaa", "first staging", nil); err != nil {
			t.Fatal(err)
		}
		gen, err := RecordDesiredGeneration(ctx, cs, "sha256:bbb", "second staging", nil)
		if err != nil {
			t.Fatalf("record a changed digest: %v", err)
		}
		if gen != 2 {
			t.Errorf("generation = %d, want 2", gen)
		}
		inv := clusterInventory(t, cs)
		if inv.Desired.Digest != "sha256:bbb" || inv.Desired.Provenance != "second staging" {
			t.Errorf("record = {digest %q, provenance %q}, want the new staging's digest and provenance",
				inv.Desired.Digest, inv.Desired.Provenance)
		}
	})
}

func TestInventoryRecordsAnAuthorizedRollback(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	if _, err := RecordDesiredGeneration(ctx, cs, "sha256:old", "generation 1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordDesiredGeneration(ctx, cs, "sha256:new", "generation 2", nil); err != nil {
		t.Fatal(err)
	}
	gen, err := RecordDesiredGeneration(ctx, cs, "sha256:old", "rollback to the generation-1 bundle set", nil)
	if err != nil {
		t.Fatalf("re-stage the earlier bundle set: %v", err)
	}
	if gen != 3 {
		t.Errorf("generation = %d, want 3 — re-staging an earlier set is a new generation, not a refusal", gen)
	}
	inv := clusterInventory(t, cs)
	if inv.Desired.Digest != "sha256:old" || inv.Desired.Provenance != "rollback to the generation-1 bundle set" {
		t.Errorf("record = {digest %q, provenance %q}, want the rolled-back set and its staging",
			inv.Desired.Digest, inv.Desired.Provenance)
	}
}

func TestInventoryDesiredRecordSurvivesAConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	seedInventory(t, cs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"cilium": {kubeCM("agent")}},
	}})

	// One shot: the first update loses a race to a concurrent union publisher.
	conflicted := false
	gvr := corev1.SchemeGroupVersion.WithResource("configmaps")
	cs.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		stored, err := cs.Tracker().Get(gvr, inventoryNamespace, inventoryConfigMapName)
		if err != nil {
			return true, nil, err
		}
		cm := stored.(*corev1.ConfigMap).DeepCopy()
		concurrent := pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {"cilium": {kubeCM("agent")}},
			"cp2": {"coredns": {kubeCM("core")}},
		}}
		b, err := json.Marshal(concurrent)
		if err != nil {
			return true, nil, err
		}
		cm.Data[inventoryDataKey] = string(b)
		if err := cs.Tracker().Update(gvr, cm, inventoryNamespace); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: "", Resource: "configmaps"}, inventoryConfigMapName,
			errors.New("the object has been modified; please apply your changes to the latest version"))
	})

	gen, err := RecordDesiredGeneration(ctx, cs, "sha256:aaa", "stratum up on cp1", nil)
	if err != nil {
		t.Fatalf("record past a concurrent writer: %v; the conflict must be retried, not fail the staging", err)
	}
	if gen != 1 {
		t.Errorf("generation = %d, want 1 — the seeded inventory held no desired record", gen)
	}
	inv := clusterInventory(t, cs)
	if inv.Desired == nil || inv.Desired.Digest != "sha256:aaa" {
		t.Errorf("desired record after the retry: %+v, want the staged digest", inv.Desired)
	}
	if got := inv.Nodes["cp1"]["cilium"]; len(got) != 1 || got[0].Name != "agent" {
		t.Errorf("cp1's entry after the retry: %+v, want agent — the retried write re-applies onto the fresh state", got)
	}
	if got := inv.Nodes["cp2"]["coredns"]; len(got) != 1 || got[0].Name != "core" {
		t.Errorf("cp2's entry after the retry: %+v, want core — the concurrent writer must not be clobbered", got)
	}
}

// What pruning drives against once migration completes: everything any node applied, not pruned.
func (inv pruneInventory) union() []objRef {
	var out []objRef
	for _, node := range sortedStringKeys(inv.Nodes) {
		entry := inv.Nodes[node]
		for _, stack := range sortedStringKeys(entry) {
			out = append(out, entry[stack]...)
		}
	}
	return out
}
