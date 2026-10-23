package operator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func TestPrunePreservesConcurrentMigrationContribution(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": {}}})
	legacy := kubeCM("only-legacy-remembers")
	seedRecord(t, r.dir, map[string][]objRef{"legacy": {legacy}})
	snapshot := clusterInventory(t, r.fakeCs)
	// The background publisher finishes after the manifest pass's snapshot.
	// Retiring the local file makes the fresh cluster entry the only evidence.
	if err := syncInventory(ctx, r.fakeCs, r.dir, "cp1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, appliedRecordName)); !os.IsNotExist(err) {
		t.Fatalf("local evidence not retired: %v", err)
	}
	current := map[string][]objRef{"current": {kubeCM("kept")}}
	r.d.pruneClusterInventory(ctx, snapshot, current, nil)
	inv := clusterInventory(t, r.fakeCs)
	if !containsRef(inv.union(), legacy) {
		t.Fatalf("concurrent contribution lost: inventory %+v, deletes %v", inv, r.deleteNames())
	}
	// A later pass can now discover and delete the no-longer-desired object.
	r.d.pruneClusterInventory(ctx, inv, current, nil)
	if got := r.deleteNames(); len(got) != 1 || got[0] != legacy.Name {
		t.Fatalf("deletes %v, want exactly %s", got, legacy.Name)
	}
	if containsRef(clusterInventory(t, r.fakeCs).union(), legacy) {
		t.Fatal("successfully pruned reference remains recorded")
	}
}

func TestClusterInventoryRelinquishesLooseOnlyObjects(t *testing.T) {
	for _, stackOwned := range []bool{false, true} {
		t.Run(map[bool]string{false: "loose only", true: "also stack owned"}[stackOwned], func(t *testing.T) {
			ctx := context.Background()
			r := deployFixture(t)
			moved := kubeCM("moved")
			seedInventory(t, r.fakeCs, pruneInventory{Recovery: true, Nodes: map[string]map[string][]objRef{
				"cp1": {}, "cp2": {"old-stack": {moved}},
			}})
			loose := filepath.Join(r.dir, "loose.yaml")
			writeManifest(t, loose, configMap(moved.Name, "kube-system"))
			writeManifest(t, filepath.Join(r.dir, "current", "kept.yaml"), configMap("kept", "kube-system"))
			stackFile := filepath.Join(r.dir, "current", "moved.yaml")
			if stackOwned {
				writeManifest(t, stackFile, configMap(moved.Name, "kube-system"))
			}
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			if got := r.deleteNames(); len(got) != 0 {
				t.Fatalf("transfer deleted %v", got)
			}
			if owned := containsRef(clusterInventory(t, r.fakeCs).union(), moved); owned != stackOwned {
				t.Fatalf("prune ownership = %t, want %t", owned, stackOwned)
			}
			if err := os.Remove(loose); err != nil {
				t.Fatal(err)
			}
			if stackOwned {
				if err := os.Remove(stackFile); err != nil {
					t.Fatal(err)
				}
			}
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			got := r.deleteNames()
			if stackOwned {
				if len(got) != 1 || got[0] != moved.Name {
					t.Fatalf("stack removal deleted %v, want only %s", got, moved.Name)
				}
			} else if len(got) != 0 {
				t.Fatalf("loose removal deleted %v; loose files are apply-only", got)
			}
		})
	}
}

func TestLooseOwnershipSurvivesLateMigrationAndLeaderChange(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode("cp3"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := kubeCM("moved")
	seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {}, "cp2": {"old-stack": {moved}},
	}})
	loose := filepath.Join(r.dir, "loose.yaml")
	writeManifest(t, loose, configMap(moved.Name, "kube-system"))
	writeManifest(t, filepath.Join(r.dir, "current", "kept.yaml"), configMap("kept", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	// A late controller contributes its historical ownership after the handoff.
	if err := publishInventory(ctx, r.fakeCs, "cp3", map[string][]objRef{"old-stack": {moved}}); err != nil {
		t.Fatal(err)
	}
	r.d.node = "cp2"
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := r.deleteNames(); len(got) != 0 {
		t.Fatalf("late contribution revived loose ownership: deleted %v", got)
	}
	// A later stack explicitly claiming the object restores ordinary pruning.
	stackFile := filepath.Join(r.dir, "current", "moved.yaml")
	writeManifest(t, stackFile, configMap(moved.Name, "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if err := os.Remove(stackFile); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := r.deleteNames(); len(got) != 1 || got[0] != moved.Name {
		t.Fatalf("reclaimed stack removal deleted %v, want only %s", got, moved.Name)
	}
}

func TestLooseOwnershipSurvivesRemovalOfItsOriginalAPIVersion(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	v1 := schema.GroupVersion{Group: "example.com", Version: "v1"}
	v2 := schema.GroupVersion{Group: "example.com", Version: "v2"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{v1, v2})
	mapper.Add(v1.WithKind("Widget"), meta.RESTScopeNamespace)
	mapper.Add(v2.WithKind("Widget"), meta.RESTScopeNamespace)
	r.setMapper(mapper)
	old := objRef{GVK: "example.com/v1,Widget", Name: "moved"}
	seedInventory(t, r.fakeCs, pruneInventory{Recovery: true, Nodes: map[string]map[string][]objRef{
		"cp1": {}, "cp2": {"old-stack": {old}},
	}})
	r.d.pruneClusterInventory(ctx, clusterInventory(t, r.fakeCs), nil, []objRef{old})

	// The original API version disappears before another controller migrates
	// its record of the same resource, now using v2 and an explicit namespace.
	mapper = meta.NewDefaultRESTMapper([]schema.GroupVersion{v2})
	mapper.Add(v2.WithKind("Widget"), meta.RESTScopeNamespace)
	r.setMapper(mapper)
	newRef := objRef{GVK: "example.com/v2,Widget", Namespace: "default", Name: old.Name}
	if err := publishInventory(ctx, r.fakeCs, "cp2", map[string][]objRef{"old-stack": {newRef}}); err != nil {
		t.Fatal(err)
	}
	r.d.pruneClusterInventory(ctx, clusterInventory(t, r.fakeCs), nil, nil)
	if got := r.deleteNames(); len(got) != 0 {
		t.Fatalf("an API version change revived pruning ownership: deleted %v", got)
	}
}

func TestManifestInventoryUsesRegisteredNodeIdentity(t *testing.T) {
	saved := inventoryHostname
	t.Cleanup(func() { inventoryHostname = saved })
	// host.Hostname supplies this short form even if the kernel hostname is
	// fully qualified; the Node registers under the same short form.
	inventoryHostname = func() (string, error) { return "registered-cp", nil }
	d, err := newManifestDeployer(&rest.Config{Host: "https://127.0.0.1"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := deployFixture(t)
	d.cs = r.fakeCs
	if _, err := r.fakeCs.CoreV1().Nodes().Create(context.Background(), cpNode("registered-cp"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := syncInventory(context.Background(), d.cs, d.dir, d.node); err != nil {
		t.Fatal(err)
	}
	closed, err := migrationClosed(context.Background(), d.cs)
	if err != nil || !closed {
		t.Fatalf("migration closed = %t, %v; published node %q, want registered-cp", closed, err, d.node)
	}
}

func TestManifestInventoryPropagatesNodeIdentityFailure(t *testing.T) {
	saved := inventoryHostname
	t.Cleanup(func() { inventoryHostname = saved })
	want := errors.New("hostname unavailable")
	inventoryHostname = func() (string, error) { return "", want }
	if _, err := newManifestDeployer(&rest.Config{Host: "https://127.0.0.1"}, t.TempDir()); !errors.Is(err, want) {
		t.Fatalf("constructor error = %v, want %v", err, want)
	}
}
