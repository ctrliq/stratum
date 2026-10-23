package operator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

func labelled(apiVersion, kind, ns, name, stack string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{group + "/stack": stack})
	return u
}

func TestUnrecordedStackLabelsDoNotAuthorizeDeletion(t *testing.T) {
	for _, stack := range []string{"dashboard", "local-path"} {
		t.Run(stack, func(t *testing.T) {
			r := deployFixture(t,
				labelled("v1", "Namespace", "", "unrelated-tenant", stack),
				labelled("v1", "Namespace", "", "stratum-dashboard", stack),
				labelled("v1", "Secret", "unrelated-tenant", "tenant-secret", stack),
				labelled("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "tenant-binding", stack),
			)
			seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": {}}})
			writeManifest(t, filepath.Join(r.dir, "current", "keep.yaml"), configMap("keep", "kube-system"))
			r.reconcile(t.Context(), "10.0.0.1", "10.0.0.11")
			for _, obj := range []struct {
				gvr      schema.GroupVersionResource
				ns, name string
			}{
				{schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "unrelated-tenant"},
				{schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "stratum-dashboard"},
				{schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "unrelated-tenant", "tenant-secret"},
				{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, "", "tenant-binding"},
			} {
				if _, err := r.fakeDyn.Resource(obj.gvr).Namespace(obj.ns).Get(t.Context(), obj.name, metav1.GetOptions{}); err != nil {
					t.Errorf("unrecorded %s %s/%s must survive a client-writable label: %v", obj.gvr.Resource, obj.ns, obj.name, err)
				}
			}
		})
	}
}

func TestBundledApplyRequiresDurableOwnership(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(fmt.Sprintf("inventory-exists=%t", cluster), func(t *testing.T) {
			r := deployFixture(t)
			if cluster {
				seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": {}}})
			}
			writeManifest(t, filepath.Join(r.dir, "dashboard", "bundle.yaml"), configMap("dashboard", "kube-system"))
			r.fakeCs.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("cannot persist ownership")
			})
			r.fakeCs.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewServiceUnavailable("cannot persist ownership")
			})
			r.reconcile(t.Context(), "10.0.0.1", "10.0.0.11")
			if got := r.patchNames(); len(got) != 0 {
				t.Fatalf("applied resources without durable ownership: %v", got)
			}
		})
	}
}

func TestBundledOwnershipPreservesLegacyHistoryAndHandoffs(t *testing.T) {
	r := deployFixture(t)
	seedRecord(t, r.dir, map[string][]objRef{"old-stack": {kubeCM("legacy"), kubeCM("released")}})
	seedInventory(t, r.fakeCs, pruneInventory{
		Nodes:   map[string]map[string][]objRef{"cp2": {"other": {kubeCM("other-node")}}},
		Unowned: []inventoryObject{{Resource: schema.GroupResource{Resource: "configmaps"}, Namespace: "kube-system", Name: "released"}},
	})
	for range 2 {
		if err := r.d.recordBundledOwnership(t.Context(), "dashboard", kubeCM("new")); err != nil {
			t.Fatal(err)
		}
	}
	inv := clusterInventory(t, r.fakeCs)
	if !slices.Contains(inv.Nodes["cp1"]["old-stack"], kubeCM("legacy")) || !slices.Contains(inv.Nodes["cp2"]["other"], kubeCM("other-node")) {
		t.Fatalf("registering bundle ownership lost migration history: %+v", inv.Nodes)
	}
	if refs := inv.Nodes["cp1"]["dashboard"]; len(refs) != 1 || !slices.Contains(refs, kubeCM("new")) {
		t.Fatalf("repeated registration must preserve one durable ref: %+v", refs)
	}
	if slices.Contains(inv.union(), kubeCM("released")) || len(inv.Unowned) != 1 {
		t.Fatal("legacy migration revived apply-only ownership")
	}
	if err := r.d.recordBundledOwnership(t.Context(), "dashboard", kubeCM("released")); err != nil {
		t.Fatal(err)
	}
	inv = clusterInventory(t, r.fakeCs)
	if len(inv.Unowned) != 0 || !slices.Contains(inv.Nodes["cp1"]["dashboard"], kubeCM("released")) {
		t.Fatal("a trusted current declaration failed to reclaim its own released object")
	}
}

func TestBundledLabelsDoNotOverrideWholePassAndLooseOwnership(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t,
		labelled("v1", "ConfigMap", "default", "moved", "local-path"),
		labelled("v1", "ConfigMap", "kube-system", "loose", "dashboard"),
	)
	seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": {}}})
	writeManifest(t, filepath.Join(r.dir, "another-stack", "keep.yaml"), configMap("moved", ""))
	loose := filepath.Join(r.dir, "loose.yaml")
	writeManifest(t, loose, configMap("loose", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := r.deleteNames(); len(got) != 0 {
		t.Fatalf("whole-pass declarations were swept: %v", got)
	}
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	// Even a stale label must not revive pruning ownership after a handoff.
	r.d.node = "cp2"
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := r.deleteNames(); len(got) != 0 {
		t.Fatalf("cleanup revived apply-only ownership after a leader change: %v", got)
	}
}

func TestLegacyBundledOwnershipPersistsLooseHandoff(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t, labelled("v1", "ConfigMap", "kube-system", "moved", "local-path"))
	seedRecord(t, r.dir, map[string][]objRef{"local-path": {kubeCM("moved")}})
	loose := filepath.Join(r.dir, "loose.yaml")
	// A copied live manifest can carry the old bookkeeping label explicitly.
	writeManifest(t, loose, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: moved\n  namespace: kube-system\n  labels:\n    stratum.ciq.com/stack: local-path\n")
	writeManifest(t, filepath.Join(r.dir, "current", "keep.yaml"), configMap("keep", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := r.deleteNames(); len(got) != 0 {
		t.Fatalf("label discovery revived legacy pruning ownership after handoff: %v", got)
	}
}

func TestLegacyLooseHandoffFailurePreservesHistory(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t, labelled("v1", "ConfigMap", "kube-system", "orphan", "local-path"))
	previous := map[string][]objRef{"local-path": {kubeCM("moved"), kubeCM("orphan")}}
	seedRecord(t, r.dir, previous)
	writeManifest(t, filepath.Join(r.dir, "loose.yaml"), configMap("moved", "kube-system"))
	r.fakeCs.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("inventory publication failed")
	})
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := r.deleteNames(); len(got) != 0 {
		t.Fatalf("deletion proceeded without a durable handoff: %v", got)
	}
	refs := record(t, r.dir)["local-path"]
	for _, ref := range previous["local-path"] {
		if !slices.Contains(refs, ref) {
			t.Fatalf("failed publication lost the only local evidence of %s", ref.Name)
		}
	}
}

func TestDesiredEmptyGenerationPrunesRecordedBundledObjects(t *testing.T) {
	r := deployFixture(t, labelled("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "stratum-dashboard-admin", "dashboard"))
	seedInventory(t, r.fakeCs, pruneInventory{
		Nodes: map[string]map[string][]objRef{"cp1": {"dashboard": {{GVK: "rbac.authorization.k8s.io/v1,ClusterRoleBinding", Name: "stratum-dashboard-admin"}}}},
		Desired: &desiredGeneration{
			Generation: 1, Digest: DigestBundles(nil), Stacks: []string{"dashboard"},
		},
	})
	ctx := context.Background()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	gvr := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	if _, err := r.fakeDyn.Resource(gvr).Get(ctx, "stratum-dashboard-admin", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("explicit desired-empty generation retained a recorded binding: %v", err)
	}
}

func TestRecordedBundledCleanupRetriesAfterLabelRemoval(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		name := "local record"
		if cluster {
			name = "cluster inventory"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := deployFixture(t, labelled("v1", "ConfigMap", "kube-system", "orphan", "local-path"))
			if cluster {
				seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": {"local-path": {kubeCM("orphan")}}}})
			} else {
				seedRecord(t, r.dir, map[string][]objRef{"local-path": {kubeCM("orphan")}})
			}
			writeManifest(t, filepath.Join(r.dir, "current", "keep.yaml"), configMap("keep", "kube-system"))
			r.failNextDeleteOf = "orphan"
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			if got := r.deleteNames(); len(got) != 1 || got[0] != "orphan" {
				t.Fatalf("first delete attempts %v, want orphan", got)
			}
			var refs []objRef
			if cluster {
				refs = clusterInventory(t, r.fakeCs).union()
			} else {
				refs = record(t, r.dir)["local-path"]
			}
			if !slices.Contains(refs, kubeCM("orphan")) {
				t.Fatal("failed deletion lost its trusted history")
			}
			// The label no longer finds the object. Its recorded retry must.
			resource := r.fakeDyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace("kube-system")
			obj, err := resource.Get(ctx, "orphan", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			obj.SetLabels(nil)
			if _, err := resource.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			r.reset()
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			if got := r.deleteNames(); len(got) != 1 || got[0] != "orphan" {
				t.Fatalf("retry deletes %v, want orphan despite its missing label", got)
			}
		})
	}
}

func TestRecordedBundledCleanupRespectsReconcileGates(t *testing.T) {
	for _, gate := range []string{"failed apply", "unresolved sentinel", "unreadable intent", "stale intent", "migration open", "empty directory"} {
		t.Run(gate, func(t *testing.T) {
			ctx := context.Background()
			r := deployFixture(t, labelled("v1", "ConfigMap", "kube-system", "orphan", "local-path"))
			seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": {"local-path": {kubeCM("orphan")}}}})
			if gate != "empty directory" {
				writeManifest(t, filepath.Join(r.dir, "current", "keep.yaml"), configMap("keep", "kube-system"))
			}
			switch gate {
			case "failed apply":
				r.failNextPatchOf = "keep"
			case "unresolved sentinel":
				writeManifest(t, filepath.Join(r.dir, "coredns", "dns.yaml"), configMap("dns", "kube-system")+"  dns: __STRATUM_DNS_IP__\n")
			case "unreadable intent":
				r.fakeCs.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("inventory unavailable")
				})
			case "stale intent":
				if _, err := RecordDesiredGeneration(ctx, r.fakeCs, "sha256:other", "newer staging", []string{"current"}); err != nil {
					t.Fatal(err)
				}
			case "migration open":
				if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode("cp2"), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			if got := r.deleteNames(); len(got) != 0 {
				t.Fatalf("cleanup bypassed %s: deleted %v", gate, got)
			}
		})
	}
}

// Recorded bundle ownership survives add-on retirement, including a dashboard's
// namespace and privileged binding. Labels on unrelated objects grant no claim.
func TestBundledCleanupUsesTrustedHistory(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		name := "local record"
		if cluster {
			name = "cluster inventory"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := deployFixture(t,
				labelled("v1", "Namespace", "", "stratum-dashboard", "dashboard"),
				labelled("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "stratum-dashboard-admin", "dashboard"),
				labelled("v1", "Secret", "stratum-dashboard", "admin-token", "dashboard"),
				labelled("v1", "ConfigMap", "kube-system", "keep", "local-path"),
				labelled("v1", "ConfigMap", "kube-system", "orphan", "local-path"),
				labelled("v1", "Secret", "kube-system", "someone-elses", "addon"),
			)
			history := map[string][]objRef{
				"dashboard": {
					{GVK: "v1,Namespace", Name: "stratum-dashboard"},
					{GVK: "rbac.authorization.k8s.io/v1,ClusterRoleBinding", Name: "stratum-dashboard-admin"},
					{GVK: "v1,Secret", Namespace: "stratum-dashboard", Name: "admin-token"},
				},
				"local-path": {kubeCM("orphan")},
			}
			if cluster {
				seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{"cp1": history}})
			} else {
				seedRecord(t, r.dir, history)
			}
			writeManifest(t, filepath.Join(r.dir, "local-path", "keep.yaml"), configMap("keep", "kube-system"))
			r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
			for _, obj := range []struct {
				gvr      schema.GroupVersionResource
				ns, name string
				wantGone bool
			}{
				{schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "stratum-dashboard", true},
				{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, "", "stratum-dashboard-admin", true},
				{schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "stratum-dashboard", "admin-token", true},
				{schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "kube-system", "someone-elses", false},
			} {
				_, err := r.fakeDyn.Resource(obj.gvr).Namespace(obj.ns).Get(ctx, obj.name, metav1.GetOptions{})
				if gone := apierrors.IsNotFound(err); gone != obj.wantGone {
					t.Errorf("%s %s/%s: gone=%t, want %t (Get error: %v)", obj.gvr.Resource, obj.ns, obj.name, gone, obj.wantGone, err)
				}
			}
			if got := r.deleteNames(); !slices.Contains(got, "orphan") || slices.Contains(got, "keep") {
				t.Errorf("ConfigMap deletes %v, want orphan and not keep", got)
			}
		})
	}
}
