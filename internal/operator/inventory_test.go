package operator

// The prune inventory's contract: the prune inventory lives in cluster state.
//
// A ConfigMap in kube-system — name stratum-addon-inventory — holds the prune
// inventory as canonical records: each operator instance publishes, under its
// node's name, the prune record its manifests directory carries (today the
// per-node .applied.json file: a map from stack to the object references that
// stack applied). publishInventory unions a node's local record into its entry
// and writes the ConfigMap back with resourceVersion conflict retry: a
// conflict re-reads the ConfigMap, re-unions, and retries, so a concurrent
// writer's entry survives instead of being clobbered and the publish never
// fails the pass over a lost race. syncInventory is the per-instance step,
// run by every operator instance, leader or not: it publishes the local
// record of the given manifests directory and, once the migration is
// complete, retires the local file, so a stale record can never re-introduce
// an object pruning already deleted. The migration is complete when every
// control-plane node has contributed an entry, or when an explicit recovery
// entry closes it by hand (the escape hatch for a decommissioned node whose
// contribution can no longer be expected). While the migration is open,
// pruning suspends; once it closes, the union of the cluster inventory — not
// any local file — is the record pruning is driven by.
//
// The inventory is read and written through the typed clientset, and the
// control-plane node list the migration gate needs comes from the same
// clientset's Nodes labelled stratum.ciq.com/control-plane=true.

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

// cpNode builds a labelled control-plane Node for the inventory's fake
// clientset: the node list the migration gate reads comes from cluster state.
func cpNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{k8sutil.ControlPlaneLabel: "true"},
	}}
}

// kubeCM is a ConfigMap object reference in kube-system, the namespace every
// bundled stack applies to.
func kubeCM(name string) objRef {
	return objRef{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: name}
}

// inventoryConfigMap builds the inventory ConfigMap a seed or a concurrent
// writer puts into the fake clientset: one JSON document under the
// inventory's data key.
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

// seedInventory puts the given inventory into the fake clientset as the
// stratum-addon-inventory ConfigMap.
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

// clusterInventory reads the inventory back through loadInventory, failing
// the test when the read errors.
func clusterInventory(t *testing.T, cs *k8sfake.Clientset) pruneInventory {
	t.Helper()
	inv, err := loadInventory(context.Background(), cs)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	return inv
}

// TestInventoryMigrationUnionsDisjointInventories: two old controllers ran
// with node-local records — each instance's .applied.json listed only the
// stacks its own directory ever applied. Both instances publish their record
// as their node's entry, and the cluster inventory then holds the union: both
// nodes' stacks, both nodes' objects, so a later leader pruning against the
// cluster inventory sees everything either old controller applied. The
// records land in the stratum-addon-inventory ConfigMap in kube-system, and
// re-publishing a node's record unions without duplicating or dropping.
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

	// The inventory lives in the named ConfigMap in kube-system, not in a
	// local file or a per-stack Secret.
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

	// Republishing a node's record is idempotent: the union neither
	// duplicates the entries nor drops the other node's.
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

// TestInventoryPublishReunionsOnAConflict: another operator instance commits
// its entry between this instance's read and its write, so the write hits a
// resourceVersion conflict. The publish must re-read the ConfigMap, re-union
// its local record into the fresh state, and retry — both entries survive.
// Clobbering (the second write based on the stale read) would erase the
// concurrent writer's contribution; failing the pass would stall the
// migration on a mere lost race.
func TestInventoryPublishReunionsOnAConflict(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	seedInventory(t, cs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"cilium": {kubeCM("agent")}},
	}})

	// One shot: the first update of the inventory ConfigMap loses a race.
	// The reactor commits the concurrent writer's entry into the tracker
	// first — exactly what the other instance's successful write would have
	// done — and then rejects the update with a conflict.
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

// TestInventoryMigrationSuspendsUntilEveryControlPlaneContributed: pruning
// waits until every control-plane node in cluster state has published its
// inventory entry, so opening the migration before a node has contributed
// would prune the objects only that node's record remembers. The local record
// stays in place while the migration is open — it is the instance's
// contribution — and is retired once the migration closes, because a retired
// record can never re-introduce an object the cluster inventory already
// pruned. An explicit recovery entry closes the migration without the missing
// node: the escape hatch for a control plane that was decommissioned before
// it could contribute.
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

		// cp2's publish is the last contribution: its own sync sees the
		// completed migration and retires its local record at once.
		if err := syncInventory(ctx, cs, dirB, "cp2"); err != nil {
			t.Fatalf("publish cp2's local record: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dirB, ".applied.json")); !os.IsNotExist(err) {
			t.Errorf("stat cp2's local record: %v; the completed migration must retire it", err)
		}
		if _, err := os.Stat(filepath.Join(dirA, ".applied.json")); err != nil {
			t.Fatalf("stat cp1's local record: %v; cp1 has not synced since the migration closed", err)
		}

		// cp1's next sync retires its record too — the cluster inventory now
		// holds both nodes' entries.
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
		cs := k8sfake.NewSimpleClientset(cpNode("cp1")) // cp2 was decommissioned
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

// TestSyncInventoryAfterRetirementWritesNothing: once the migration has
// closed and the local record is retired, the cluster inventory is the
// record — a later sync can publish nothing new, union nothing, and retire
// nothing, so it must not write the inventory ConfigMap at all. A retired
// instance that still published would rewrite the ConfigMap with identical
// bytes every sync interval for the process lifetime, churning the
// resourceVersion and emitting watch events for a no-op on every control
// plane. The gate's reads — the control-plane node list and the inventory
// Get — remain, and the entry the retired record contributed must still hold
// what it held.
func TestSyncInventoryAfterRetirementWritesNothing(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(cpNode("cp1"))
	dir := t.TempDir()
	seedRecord(t, dir, map[string][]objRef{"cilium": {kubeCM("agent")}})

	// The only control-plane node's first publish completes the migration and
	// retires the local record at once.
	if err := syncInventory(ctx, cs, dir, "cp1"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, appliedRecordName)); !os.IsNotExist(err) {
		t.Fatalf("stat the local record: %v; the completed migration must retire it", err)
	}

	// The steady state: every later sync reads the migration gate and writes
	// nothing.
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

// TestSyncInventoryPublishesAnEmptyEntryWhileTheMigrationIsOpen: a node whose
// manifests directory carries no local record — a fresh instance that has
// never applied anything — must still contribute its entry while the
// migration waits on it, because the gate counts a node as contributed once
// an entry exists under its name, whatever the entry holds. Skipping the
// publish for a record-less instance would leave such a node an expected
// contributor forever and suspend cluster-wide pruning until the recovery
// entry closed the migration by hand.
func TestSyncInventoryPublishesAnEmptyEntryWhileTheMigrationIsOpen(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset(cpNode("cp1"), cpNode("cp2"))
	dir := t.TempDir() // no .applied.json: this instance never applied anything

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

// TestWriteInventorySkipsANoOpUpdate: writing the inventory the ConfigMap
// already holds must not issue an Update. A no-op write bumps the
// resourceVersion and emits a watch event for identical bytes — churn every
// idempotent publish would otherwise repeat for the migration's whole
// duration — so the write is skipped when the mutated document is
// byte-identical to the stored one. A document the mutation actually changed
// still writes.
func TestWriteInventorySkipsANoOpUpdate(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	seedInventory(t, cs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"cilium": {kubeCM("agent")}},
	}})

	updates := 0
	cs.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil // count the attempt, then pass it through
	})

	// The same record published again unions to the identical document.
	if err := publishInventory(ctx, cs, "cp1", map[string][]objRef{"cilium": {kubeCM("agent")}}); err != nil {
		t.Fatalf("no-op publish: %v", err)
	}
	if updates != 0 {
		t.Errorf("no-op publish issued %d updates; an identical document must not be written", updates)
	}

	// A genuinely new reference still writes.
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

// The desired bundle generation's contract.
//
// The staging path — writeBundles in internal/cluster — renders the bundle
// set and stages it into the manifests directory. What it staged is the
// cluster's desired state, and the deployer on any control plane must be able
// to tell what it is about to apply apart from what was staged: the staging
// path records a desired generation {generation, digest of the rendered
// bundle set, provenance} in the same stratum-addon-inventory ConfigMap that
// carries the prune inventory, through the apiserver — RBAC is the integrity
// boundary, so the record is as trustworthy as the cluster's own write access
// and separate from the operator binary version. The generation moves only
// when the rendered bytes change, and it has no monotonic rule: re-staging an
// earlier bundle set — an authorized rollback — is a new generation like any
// other. The deployer's half of the contract — refusing a staged tree whose
// digest does not match the record, pruning on an explicitly recorded empty
// set, applying as today when no record exists — is pinned in
// manifests_test.go.
//
// The digest is one algorithm shared by both sides, so it is pinned here as a
// pure function of the bundle set: a map from a bundle's path under the
// manifests directory to its rendered bytes.

// TestDesiredDigestIsDeterministicAndSensitive: the digest the staging path
// records and the digest the deployer computes over the staged tree must
// agree byte for byte for a pass to apply, so the algorithm cannot depend on
// map walk order (the stager digests a rendered map, the deployer re-reads
// the staged tree from disk), and it must move when any bundle's bytes or
// path move. The empty set — the desired-empty generation — digests to its
// own stable value, so an explicitly staged empty set is recordable and
// matchable like any other.
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
	// Twenty walks of the same map: a digest that read the map's iteration
	// order would flake here, and then no staged tree would ever match its
	// record.
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
		"cilium/other.yaml":      files["cilium/cilium.yaml"], // same bytes, different bundle path
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

// TestInventoryRecordsTheDesiredGeneration: the staging path records the
// desired bundle generation in the cluster inventory object — the same
// stratum-addon-inventory ConfigMap that carries the prune inventory — as
// {generation, digest, provenance}. The first staging of a set opens the
// record at generation 1. Re-staging the same bytes records nothing new: the
// generation moves only when the rendered bytes change, so every quiet
// `stratum up` leaves the record — and the provenance of the staging that
// produced the generation — exactly as it was.
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

// TestInventoryRecordsAnAuthorizedRollback: rollback is re-staging an earlier
// bundle set, and the record has no monotonic rule to refuse it — the digest
// an earlier generation held is a changed digest like any other, so the
// re-staging records the next generation. A comparison against the recorded
// history would turn every rollback into a manual intervention instead.
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

// TestInventoryDesiredRecordSurvivesAConcurrentWriter: two control planes can
// run `stratum up` at once, and the loser of the resourceVersion race must
// re-read the ConfigMap, re-apply its desired record onto the fresh state and
// retry — the winner's prune-inventory entry survives beside it. Clobbering
// would drop a node's inventory contribution; failing the staging would make
// every concurrent `stratum up` fail over a lost race.
func TestInventoryDesiredRecordSurvivesAConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewSimpleClientset()
	seedInventory(t, cs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"cilium": {kubeCM("agent")}},
	}})

	// One shot: the first update of the inventory ConfigMap loses a race. The
	// reactor commits the concurrent writer's entry into the tracker first —
	// exactly what the other staging's successful write would have done — and
	// then rejects the update with a conflict.
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
		// The concurrent writer is a union publisher: its write carries what
		// it read (cp1's entry) plus its own contribution, not a clobber.
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

// union returns every object reference the inventory holds — the union of all
// nodes' entries, stacks in a deterministic order. Once the migration is
// complete this union is what pruning drives against: everything any instance
// applied that nothing has yet pruned.
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
