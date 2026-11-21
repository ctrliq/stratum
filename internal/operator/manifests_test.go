package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/k8sutil"
)

type manifestReactor struct {
	patches []clienttesting.PatchActionImpl
	deletes []clienttesting.DeleteAction

	fakeDyn   *dynamicfake.FakeDynamicClient
	fakeDisco *discoveryfake.FakeDiscovery
	fakeCs    *k8sfake.Clientset

	d   *manifestDeployer
	dir string

	failNextPatchOf  string
	failNextDeleteOf string
	deleteNotFound   bool
}

func deployFixture(t *testing.T, objs ...runtime.Object) *manifestReactor {
	t.Helper()
	r := &manifestReactor{}
	fakeDyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Version: "v1", Resource: "nodes"}:                                                   "NodeList",
			{Version: "v1", Resource: "namespaces"}:                                              "NamespaceList",
			{Version: "v1", Resource: "serviceaccounts"}:                                         "ServiceAccountList",
			{Version: "v1", Resource: "secrets"}:                                                 "SecretList",
			{Version: "v1", Resource: "services"}:                                                "ServiceList",
			{Version: "v1", Resource: "configmaps"}:                                              "ConfigMapList",
			{Group: "apps", Version: "v1", Resource: "deployments"}:                              "DeploymentList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               "RoleList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
			{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
			{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}:                 "StorageClassList",
			{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"}:                    "RuntimeClassList",
			{Group: "example.com", Version: "v1", Resource: "widgets"}:                           "WidgetList",
		}, objs...)
	fakeDyn.PrependReactor("patch", "configmaps", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		r.patches = append(r.patches, pa)
		if r.failNextPatchOf != "" && pa.GetName() == r.failNextPatchOf {
			r.failNextPatchOf = ""
			return true, nil, apierrors.NewInternalError(errors.New("apply failed"))
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), obj); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	fakeDyn.PrependReactor("patch", "widgets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		r.patches = append(r.patches, pa)
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), obj); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	fakeDyn.PrependReactor("patch", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		r.patches = append(r.patches, pa)
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), obj); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	fakeDyn.PrependReactor("patch", "storageclasses", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		r.patches = append(r.patches, pa)
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), obj); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	fakeDyn.PrependReactor("patch", "runtimeclasses", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		r.patches = append(r.patches, pa)
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), obj); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	fakeDyn.PrependReactor("delete", "runtimeclasses", func(a clienttesting.Action) (bool, runtime.Object, error) {
		r.deletes = append(r.deletes, a.(clienttesting.DeleteAction))
		return true, nil, nil
	})
	fakeDyn.PrependReactor("delete", "configmaps", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteAction)
		r.deletes = append(r.deletes, da)
		if r.failNextDeleteOf != "" && da.GetName() == r.failNextDeleteOf {
			r.failNextDeleteOf = ""
			return true, nil, apierrors.NewInternalError(errors.New("delete failed"))
		}
		if r.deleteNotFound {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, da.GetName())
		}
		return true, nil, nil
	})
	fakeDyn.PrependReactor("delete", "widgets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteAction)
		r.deletes = append(r.deletes, da)
		if r.deleteNotFound {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "widgets"}, da.GetName())
		}
		return true, nil, nil
	})

	fakeDisco := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: "apps", Version: "v1"}, {Group: "storage.k8s.io", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"}, meta.RESTScopeRoot)
	r.dir = t.TempDir()
	fakeCs := k8sfake.NewSimpleClientset()
	r.d = &manifestDeployer{
		dyn:    fakeDyn,
		disco:  fakeDisco,
		mapper: mapper,
		dir:    r.dir,
		state:  map[string]manifestFileState{},
		now:    time.Now,
		cs:     fakeCs,
		node:   "cp1",
	}
	r.fakeDyn, r.fakeDisco, r.fakeCs = fakeDyn, fakeDisco, fakeCs
	return r
}

func (r *manifestReactor) deployerFor(dir, node string) *manifestDeployer {
	return &manifestDeployer{
		dyn:    r.fakeDyn,
		disco:  r.d.disco,
		mapper: r.d.mapper,
		dir:    dir,
		state:  map[string]manifestFileState{},
		now:    r.d.now,
		cs:     r.fakeCs,
		node:   node,
	}
}

func (r *manifestReactor) reconcile(ctx context.Context, vip, selfIP string) {
	r.d.reconcileManifests(ctx, vip, selfIP)
}

func (r *manifestReactor) setDir(dir string) { r.d.dir = dir }

func (r *manifestReactor) setDisco(d *discoveryfake.FakeDiscovery) {
	r.d.disco = d
	r.fakeDisco = d
}

func (r *manifestReactor) setMapper(m meta.RESTMapper) { r.d.mapper = m }

func (r *manifestReactor) restMapper() meta.RESTMapper { return r.d.mapper }

func (r *manifestReactor) stateFor(path string) (manifestFileState, bool) {
	st, ok := r.d.state[path]
	return st, ok
}

func (r *manifestReactor) ageLastApply(path string, back time.Duration) {
	st, ok := r.d.state[path]
	if !ok {
		return
	}
	st.lastApply = st.lastApply.Add(back)
	r.d.state[path] = st
}

func (r *manifestReactor) restart() { r.d.state = map[string]manifestFileState{} }

func (r *manifestReactor) resolveAPIServer(ctx context.Context, vip, selfIP string) (addons.APIServer, error) {
	return r.d.resolveAPIServer(ctx, vip, selfIP)
}

func (r *manifestReactor) reset() { r.patches, r.deletes = nil, nil }

func (r *manifestReactor) patchNames() []string {
	names := make([]string, len(r.patches))
	for i, p := range r.patches {
		names[i] = p.GetName()
	}
	return names
}

func (r *manifestReactor) deleteNames() []string {
	names := make([]string, len(r.deletes))
	for i, d := range r.deletes {
		names[i] = d.GetName()
	}
	return names
}

func writeManifest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func configMap(name, ns string) string {
	out := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n"
	if ns != "" {
		out += "  namespace: " + ns + "\n"
	}
	return out + "data:\n  key: \"" + name + "\"\n"
}

func record(t *testing.T, dir string) map[string][]objRef {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".applied.json"))
	if err != nil {
		t.Fatalf("read prune record: %v", err)
	}
	var rec map[string][]objRef
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("parse prune record: %v", err)
	}
	return rec
}

func seedRecord(t *testing.T, dir string, rec map[string][]objRef) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeManifest(t, filepath.Join(dir, ".applied.json"), string(b))
}

func assertApplied(t *testing.T, r *manifestReactor, name string) []clienttesting.PatchActionImpl {
	t.Helper()
	var found []clienttesting.PatchActionImpl
	for _, p := range r.patches {
		if p.GetPatchType() != types.ApplyPatchType {
			t.Errorf("patch of %s: got patch type %v, want ApplyPatchType", name, p.GetPatchType())
			continue
		}
		// the literal must not track fieldManager: a rename would hide it.
		if p.PatchOptions.FieldManager != "stratum-operator" {
			t.Errorf("patch of %s: field manager %q, want stratum-operator", name, p.PatchOptions.FieldManager)
		}
		if p.PatchOptions.Force == nil || !*p.PatchOptions.Force {
			t.Errorf("patch of %s: want force set", name)
		}
		if p.GetName() == name {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no patch recorded for %s; applied %v", name, r.patchNames())
	}
	return found
}

func stackLabel(t *testing.T, p clienttesting.PatchActionImpl) (string, bool) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(p.GetPatch(), obj); err != nil {
		t.Fatalf("parse patch payload: %v", err)
	}
	labels := obj.GetLabels()
	// CoreDNS spreading and already-applied objects depend on this key.
	v, ok := labels["stratum.ciq.com/stack"]
	return v, ok
}

func cp(name, ip string) runtime.Object {
	addrs := []any{}
	if ip != "" {
		addrs = append(addrs, map[string]any{"type": "InternalIP", "address": ip})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": name, "labels": map[string]any{k8sutil.ControlPlaneLabel: "true"}},
		"status":   map[string]any{"addresses": addrs},
	}}
}

func TestReconcileManifestsPrunesWhatAStackStopsDeclaring(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	stack := filepath.Join(dir, "alpha", "stack.yaml")
	writeManifest(t, stack, configMap("a", "kube-system")+"---\n"+configMap("b", "kube-system"))

	ctx := context.Background()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	assertApplied(t, r, "a")
	assertApplied(t, r, "b")
	for _, name := range []string{"a", "b"} {
		patches := assertApplied(t, r, name)
		p := patches[0]
		if p.GetNamespace() != "kube-system" {
			t.Errorf("patch of %s: namespace %q, want kube-system", name, p.GetNamespace())
		}
		if stack, ok := stackLabel(t, p); !ok || stack != "alpha" {
			t.Errorf("patch of %s: stack label %q (present %v), want alpha", name, stack, ok)
		}
	}
	rec := record(t, dir)
	if len(rec["alpha"]) != 2 {
		t.Fatalf("record under alpha: %+v, want a and b", rec["alpha"])
	}
	for _, ref := range rec["alpha"] {
		if ref.GVK != "v1,ConfigMap" || ref.Namespace != "kube-system" {
			t.Errorf("recorded ref %+v, want v1,ConfigMap kube-system", ref)
		}
	}

	r.reset()
	writeManifest(t, stack, configMap("a", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	if len(r.deletes) != 1 || r.deletes[0].GetName() != "b" {
		t.Fatalf("deletes %v, want exactly b", r.deleteNames())
	}
	if r.deletes[0].GetNamespace() != "kube-system" {
		t.Errorf("delete namespace %q, want kube-system", r.deletes[0].GetNamespace())
	}
	if got := len(assertApplied(t, r, "a")); got != 1 {
		t.Errorf("a applied %d times in pass 2, want once", got)
	}
	rec = record(t, dir)
	if len(rec["alpha"]) != 1 || rec["alpha"][0].Name != "a" {
		t.Fatalf("record under alpha: %+v, want only a", rec["alpha"])
	}
}

func TestReconcileManifestsPrunesNothingAfterAFailedFile(t *testing.T) {
	ctx := context.Background()
	seed := map[string][]objRef{"alpha": {{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: "old"}}}

	t.Run("an apply that errors prunes nothing", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"),
			configMap("a", "kube-system")+"---\n"+configMap("b", "kube-system"))
		r.failNextPatchOf = "b"

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v, want none after a failed apply", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after failed pass: %+v, want old alone", got["alpha"])
		}

		r.reset()
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		assertApplied(t, r, "a")
		assertApplied(t, r, "b")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want old", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 2 {
			t.Errorf("record after clean pass: %+v, want a and b", got["alpha"])
		}
	})

	t.Run("a file that does not parse prunes nothing", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))
		writeManifest(t, filepath.Join(dir, "beta", "broken.yaml"), "foo: [unclosed")

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v, want none after an unparsable file", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after failed pass: %+v, want old alone", got["alpha"])
		}

		r.reset()
		writeManifest(t, filepath.Join(dir, "beta", "broken.yaml"), configMap("repaired", "kube-system"))
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		assertApplied(t, r, "repaired")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want old", r.deleteNames())
		}
		if got := record(t, dir); len(got["beta"]) != 1 || got["beta"][0].Name != "repaired" {
			t.Errorf("record after repair: %+v, want repaired under beta", got)
		}
	})

	t.Run("an unresolved apiserver sentinel prunes nothing", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", ""))
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "cilium.yaml"),
			configMap("cilium-config", "kube-system")+"  k8s-api-server-host: \"__STRATUM_APISERVER_HOST__\"\n")

		r.reconcile(ctx, "", "10.0.0.11")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v, want none after a failed apiserver resolution", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after failed pass: %+v, want old alone", got["alpha"])
		}

		r.reset()
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		assertApplied(t, r, "cilium-config")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want old", r.deleteNames())
		}
	})

	t.Run("an empty stack file prunes nothing", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), "")
		writeManifest(t, filepath.Join(dir, "beta", "other.yaml"), configMap("b", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v, want none while a stack file is empty", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after empty-file pass: %+v, want old alone", got["alpha"])
		}

		r.reset()
		if err := os.RemoveAll(filepath.Join(dir, "alpha")); err != nil {
			t.Fatal(err)
		}
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want old once the stack's directory is gone", r.deleteNames())
		}
	})

	t.Run("an empty directory makes no API call", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		seedRecord(t, dir, seed)
		r.fakeDyn.ClearActions()
		r.fakeDisco.ClearActions()

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if actions := r.fakeDyn.Actions(); len(actions) != 0 {
			t.Errorf("dynamic client actions %v; an empty directory must not touch the API", actions)
		}
		if actions := r.fakeDisco.Actions(); len(actions) != 0 {
			t.Errorf("discovery client actions %v; an empty directory must not touch the API", actions)
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after empty pass: %+v, want old alone", got["alpha"])
		}
	})
}

func TestPruneKeepsAFailedDeleteInTheRecord(t *testing.T) {
	ctx := context.Background()
	seed := map[string][]objRef{
		"alpha": {
			{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: "a"},
			{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: "b"},
		},
	}

	t.Run("a server error keeps b in the record for a retry", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))
		r.failNextDeleteOf = "b"

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "b" {
			t.Fatalf("deletes %v, want exactly one delete of b", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 2 {
			t.Errorf("record after failed delete: %+v, want a and b", got["alpha"])
		}

		r.reset()
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "b" {
			t.Fatalf("deletes %v, want the retried delete of b", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "a" {
			t.Errorf("record after retried delete: %+v, want only a", got["alpha"])
		}
	})

	t.Run("NotFound is done: b is dropped at once", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))
		r.deleteNotFound = true

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "b" {
			t.Fatalf("deletes %v, want exactly one delete of b", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "a" {
			t.Errorf("record after NotFound delete: %+v, want only a", got["alpha"])
		}
	})
}

func TestReconcileManifestsNeverPrunesLooseFiles(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	loose := filepath.Join(dir, "loose.yaml")
	writeManifest(t, loose, configMap("loose", ""))
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))

	ctx := context.Background()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	loosePatches := assertApplied(t, r, "loose")
	if stack, ok := stackLabel(t, loosePatches[0]); ok {
		t.Errorf("loose object carries stack label %q, want none", stack)
	}
	aPatches := assertApplied(t, r, "a")
	if stack, ok := stackLabel(t, aPatches[0]); !ok || stack != "alpha" {
		t.Errorf("stack object a: stack label %q (present %v), want alpha", stack, ok)
	}
	if got := clusterInventory(t, r.fakeCs).Nodes["cp1"]["alpha"]; len(got) != 1 || got[0].Name != "a" {
		t.Errorf("record under alpha: %+v, want a", got)
	}

	r.reset()
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v, want none after a loose file is removed", r.deleteNames())
	}
	if got := clusterInventory(t, r.fakeCs).Nodes["cp1"]["alpha"]; len(got) != 1 || got[0].Name != "a" {
		t.Errorf("record under alpha after pass 2: %+v, want a", got)
	}
}

func TestReconcileManifestsReappliesOnlyOnChangeOrResync(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	stack := filepath.Join(dir, "alpha", "stack.yaml")
	writeManifest(t, stack, configMap("a", "kube-system")+"---\n"+configMap("b", "kube-system"))
	ctx := context.Background()

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "a")
	assertApplied(t, r, "b")

	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; an unchanged file inside the resync window must not re-apply",
			r.patchNames(), r.deleteNames())
	}
	if got := record(t, dir)["alpha"]; len(got) != 2 {
		t.Errorf("record under alpha after the unchanged pass: %+v, want a and b — the unchanged file still feeds the keep-set", got)
	}

	if _, ok := r.stateFor(stack); !ok {
		t.Fatal("pass 1 left no apply state for the stack file")
	}
	r.ageLastApply(stack, -manifestResync-time.Minute)
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 2 {
		t.Errorf("patched %v, want a and b re-applied after the resync window", r.patchNames())
	}
	assertApplied(t, r, "a")
	assertApplied(t, r, "b")

	writeManifest(t, stack, configMap("a", "kube-system")+"  extra: \"changed\"\n---\n"+configMap("b", "kube-system"))
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 2 {
		t.Errorf("patched %v, want a and b re-applied on changed bytes", r.patchNames())
	}
	assertApplied(t, r, "a")
	assertApplied(t, r, "b")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v, want none — both objects are still declared", r.deleteNames())
	}
}

func TestReconcileManifestsARestartedDeployerReappliesOnce(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	stack := filepath.Join(dir, "alpha", "stack.yaml")
	writeManifest(t, stack, configMap("a", "kube-system"))
	ctx := context.Background()

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "a")

	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Fatalf("patched %v, deleted %v; the same process must not re-apply inside the resync window",
			r.patchNames(), r.deleteNames())
	}

	r.restart()
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 1 {
		t.Errorf("patched %v after the restart, want the file applied exactly once", r.patchNames())
	}
	assertApplied(t, r, "a")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v after the restart, want none — the file still declares a", r.deleteNames())
	}
	if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "a" {
		t.Errorf("record after the restart: %+v, want a — the fresh pass keeps the inventory", got)
	}

	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; the pass after the restart must be quiet again", r.patchNames(), r.deleteNames())
	}
}

func TestPruneReadsTheRecordFormatThisReleaseWrites(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	golden, err := os.ReadFile(filepath.Join("testdata", "applied.json"))
	if err != nil {
		t.Fatalf("read golden record: %v", err)
	}
	var goldenRec map[string][]objRef
	if err := json.Unmarshal(golden, &goldenRec); err != nil {
		t.Fatalf("parse golden record: %v", err)
	}
	if len(goldenRec["alpha"]) != 2 {
		t.Fatalf("golden record under alpha: %+v, want two refs in today's shape (gvk, namespace, name)", goldenRec["alpha"])
	}
	for _, ref := range goldenRec["alpha"] {
		if ref.GVK != "v1,ConfigMap" || ref.Namespace != "kube-system" || ref.Name == "" {
			t.Errorf("golden ref %+v, want v1,ConfigMap in kube-system with a name", ref)
		}
	}

	writeManifest(t, filepath.Join(dir, ".applied.json"), string(golden))
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))
	r.reconcile(context.Background(), "10.0.0.1", "10.0.0.11")

	if len(r.deletes) != 1 || r.deletes[0].GetName() != "b" {
		t.Fatalf("deletes %v, want exactly b pruned from the golden record", r.deleteNames())
	}
	assertApplied(t, r, "a")
	if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "a" {
		t.Errorf("record after the pass: %+v, want only a", got)
	}
}

func TestReconcileManifestsCorruptRecordRequiresExplicitRecovery(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	local := filepath.Join(r.dir, appliedRecordName)
	corrupt := `{"alpha": [{"gvk":"v1,ConfigMap","namespace":"kube-system","name":"ghost"}`
	writeManifest(t, local, corrupt)
	writeManifest(t, filepath.Join(r.dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))
	ctx := context.Background()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Fatalf("patches %v, deletes %v; corrupt migration evidence must suspend the pass", r.patchNames(), r.deleteNames())
	}
	if got, err := os.ReadFile(local); err != nil || string(got) != corrupt {
		t.Fatalf("local evidence = %q, %v; want original corrupt bytes", got, err)
	}
	seedInventory(t, r.fakeCs, pruneInventory{Recovery: true})
	if err := syncInventory(ctx, r.fakeCs, r.dir, "cp1"); err != nil {
		t.Fatalf("explicit recovery: %v", err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "a")
	if len(r.deletes) != 0 {
		t.Fatalf("deletes %v; recovery must not invent references from truncated JSON", r.deleteNames())
	}
}

func TestPruneRecordWriteFailureIsRetriedNextPass(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	seed := map[string][]objRef{"alpha": {{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: "old"}}}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	seedBytes := string(b)
	writeManifest(t, filepath.Join(dir, ".applied.json"), seedBytes)
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))
	tmp := filepath.Join(dir, ".applied.json.tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	assertApplied(t, r, "a")
	if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
		t.Fatalf("deletes %v, want exactly the pruned old", r.deleteNames())
	}
	if got, err := os.ReadFile(filepath.Join(dir, ".applied.json")); err != nil || string(got) != seedBytes {
		t.Errorf("record after the failed write: %q (%v), want the seed %q unchanged", got, err, seedBytes)
	}

	r.reset()
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 {
		t.Errorf("patched %v; a failed record write must not force a re-apply inside the resync window", r.patchNames())
	}
	if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
		t.Fatalf("deletes %v, want the stale old pruned again", r.deleteNames())
	}
	rec := record(t, dir)
	if len(rec["alpha"]) != 1 || rec["alpha"][0].Name != "a" {
		t.Errorf("record once the write succeeds: %+v, want only a", rec["alpha"])
	}
}

func TestReconcileManifestsEmptyDirReturnsBeforePruning(t *testing.T) {
	ctx := context.Background()
	seed := map[string][]objRef{"alpha": {{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: "old"}}}

	t.Run("a directory holding only the record prunes nothing", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		b, err := json.Marshal(seed)
		if err != nil {
			t.Fatal(err)
		}
		seedBytes := string(b)
		writeManifest(t, filepath.Join(dir, ".applied.json"), seedBytes)
		r.fakeDyn.ClearActions()
		r.fakeDisco.ClearActions()

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		if actions := r.fakeDyn.Actions(); len(actions) != 0 {
			t.Errorf("dynamic client actions %v; an early return must not touch the API", actions)
		}
		if actions := r.fakeDisco.Actions(); len(actions) != 0 {
			t.Errorf("discovery client actions %v; an early return must not touch the API", actions)
		}
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; nothing declared on disk must not prune the recorded old", r.deleteNames())
		}
		if got, err := os.ReadFile(filepath.Join(dir, ".applied.json")); err != nil || string(got) != seedBytes {
			t.Errorf("record after the pass: %q (%v), want the seed %q unchanged", got, err, seedBytes)
		}
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "old" {
			t.Errorf("record after the pass: %+v, want old kept", got)
		}
	})

	t.Run("a missing directory is a silent no-op", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := filepath.Join(t.TempDir(), "absent")
		r.setDir(dir)
		r.fakeDyn.ClearActions()
		r.fakeDisco.ClearActions()

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		if actions := r.fakeDyn.Actions(); len(actions) != 0 {
			t.Errorf("dynamic client actions %v; a missing directory must not touch the API", actions)
		}
		if actions := r.fakeDisco.Actions(); len(actions) != 0 {
			t.Errorf("discovery client actions %v; a missing directory must not touch the API", actions)
		}
		if _, err := os.Stat(filepath.Join(dir, ".applied.json")); !os.IsNotExist(err) {
			t.Errorf("stat the record: %v; a missing directory must stay missing", err)
		}
	})
}

func TestReconcileManifestsRebuildsItsMapperForANewCRD(t *testing.T) {
	widgetYAML := "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w1\n  namespace: kube-system\n"
	disco := func(withWidget bool) *discoveryfake.FakeDiscovery {
		res := []*metav1.APIResourceList{{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true}},
		}}
		if withWidget {
			res = append(res, &metav1.APIResourceList{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
			})
		}
		fd := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
		fd.Resources = res
		return fd
	}

	t.Run("a kind the mapper predates is applied after the rebuild", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		r.setDisco(disco(true))
		writeManifest(t, filepath.Join(dir, "alpha", "widget.yaml"), widgetYAML)

		r.reconcile(context.Background(), "10.0.0.1", "10.0.0.11")
		patches := assertApplied(t, r, "w1")
		if stack, ok := stackLabel(t, patches[0]); !ok || stack != "alpha" {
			t.Errorf("widget w1: stack label %q (present %v), want alpha", stack, ok)
		}
		if _, err := r.restMapper().RESTMapping(schema.GroupKind{Group: "example.com", Kind: "Widget"}, "v1"); err != nil {
			t.Errorf("mapper after the pass: %v; a no-match must have rebuilt it from discovery", err)
		}
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "w1" {
			t.Errorf("record under alpha: %+v, want w1", got)
		}
	})

	t.Run("a kind discovery does not know applies nothing and prunes nothing", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		r.setDisco(disco(false))
		seedRecord(t, dir, map[string][]objRef{"alpha": {{GVK: "v1,ConfigMap", Namespace: "kube-system", Name: "old"}}})
		writeManifest(t, filepath.Join(dir, "alpha", "widget.yaml"), widgetYAML)

		r.reconcile(context.Background(), "10.0.0.1", "10.0.0.11")
		if len(r.patches) != 0 || len(r.deletes) != 0 {
			t.Errorf("patched %v, deleted %v; a pass that cannot resolve a kind must apply and prune nothing",
				r.patchNames(), r.deleteNames())
		}
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "old" {
			t.Errorf("record after the failed pass: %+v, want old alone (unchanged)", got)
		}
	})
}

func TestPruneKeepsAnObjectMovedBetweenStacks(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	ctx := context.Background()

	writeManifest(t, filepath.Join(dir, "cilium", "stack.yaml"), configMap("shared", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := record(t, dir)["cilium"]; len(got) != 1 || got[0].Name != "shared" {
		t.Fatalf("record under cilium: %+v, want shared", got)
	}

	r.reset()
	if err := os.Remove(filepath.Join(dir, "cilium", "stack.yaml")); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, filepath.Join(dir, "coredns", "stack.yaml"), configMap("shared", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	patches := assertApplied(t, r, "shared")
	if stack, ok := stackLabel(t, patches[0]); !ok || stack != "coredns" {
		t.Errorf("shared applied under stack %q (present %v), want coredns", stack, ok)
	}
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v; an object a pass applies under its new stack must survive that pass", r.deleteNames())
	}
	rec := record(t, dir)
	if got := rec["coredns"]; len(got) != 1 || got[0].Name != "shared" {
		t.Errorf("record under coredns: %+v, want shared — the new stack owns it", got)
	}
	if got := rec["cilium"]; len(got) != 0 {
		t.Errorf("record under cilium: %+v, want empty — the old stack no longer owns it", got)
	}
}

func TestPruneMatchesByCanonicalIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("a GVK version flip does not orphan the recorded object", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.com", Version: "v1"}})
		mapper.Add(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Group: "example.com", Version: "v2", Kind: "Widget"}, meta.RESTScopeNamespace)
		r.setMapper(mapper)
		writeManifest(t, filepath.Join(dir, "alpha", "widget.yaml"),
			"apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w1\n  namespace: kube-system\n")
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "w1" {
			t.Fatalf("record under alpha: %+v, want w1", got)
		}

		r.reset()
		writeManifest(t, filepath.Join(dir, "alpha", "widget.yaml"),
			"apiVersion: example.com/v2\nkind: Widget\nmetadata:\n  name: w1\n  namespace: kube-system\n")
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		assertApplied(t, r, "w1")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; a version flip of the same object must not prune it", r.deleteNames())
		}
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "w1" {
			t.Errorf("record under alpha: %+v, want w1 still recorded", got)
		}
	})

	t.Run("a namespace made explicit is the same object", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		writeManifest(t, filepath.Join(dir, "alpha", "cm.yaml"), configMap("cmap", ""))
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "cmap" {
			t.Fatalf("record under alpha: %+v, want cmap", got)
		}

		r.reset()
		writeManifest(t, filepath.Join(dir, "alpha", "cm.yaml"), configMap("cmap", "default"))
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		assertApplied(t, r, "cmap")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; an object whose namespace was made explicit must not be pruned", r.deleteNames())
		}
		if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "cmap" {
			t.Errorf("record under alpha: %+v, want cmap still recorded", got)
		}
	})
}

func TestPruneKeepsLooseObjectsWithoutOwningThem(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	ctx := context.Background()

	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("m", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "m" {
		t.Fatalf("record under alpha: %+v, want m", got)
	}

	r.reset()
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("kept", "kube-system"))
	writeManifest(t, filepath.Join(dir, "loose.yaml"), configMap("m", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	loosePatches := assertApplied(t, r, "m")
	if stack, ok := stackLabel(t, loosePatches[0]); ok {
		t.Errorf("loose object m carries stack label %q, want none", stack)
	}
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v; an object a pass applies from a loose file must survive that pass", r.deleteNames())
	}
	rec := clusterInventory(t, r.fakeCs).Nodes["cp1"]
	if got := rec["alpha"]; len(got) != 1 || got[0].Name != "kept" {
		t.Errorf("record under alpha: %+v, want kept", got)
	}
	for stack, refs := range rec {
		for _, ref := range refs {
			if ref.Name == "m" {
				t.Errorf("record under %s lists %s; a loose object must never be prune-owned", stack, ref.Name)
			}
		}
	}

	r.reset()
	if err := os.Remove(filepath.Join(dir, "loose.yaml")); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v after the loose file is removed; the record never owned m", r.deleteNames())
	}
}

func TestPruneIsDrivenByTheClusterInventory(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dir := r.dir
	seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"alpha": {kubeCM("old")}},
		"cp2": {"alpha": {kubeCM("kept")}},
	}})
	for _, name := range []string{"cp1", "cp2"} {
		if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode(name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("kept", "kube-system"))
	seedRecord(t, dir, map[string][]objRef{"alpha": {kubeCM("local-only")}})

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	assertApplied(t, r, "kept")
	if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
		t.Fatalf("deletes %v, want exactly old — the object the cluster inventory remembers and no file declares", r.deleteNames())
	}

	inv := clusterInventory(t, r.fakeCs)
	if got := inv.Nodes["cp1"]["alpha"]; len(got) != 1 || got[0].Name != "kept" {
		t.Errorf("cp1's entry after the pass: %+v, want kept — the pass's keep-set is this node's entry", got)
	}
	if got := inv.Nodes["cp2"]["alpha"]; len(got) != 1 || got[0].Name != "kept" {
		t.Errorf("cp2's entry after the pass: %+v, want kept — another node's entry is not ours to rewrite", got)
	}
	for _, ref := range inv.union() {
		if ref.Name == "old" {
			t.Errorf("the union still lists old: %+v; a pruned object must leave every entry", inv.union())
		}
		if ref.Name == "local-only" {
			t.Errorf("the union lists local-only: %+v; the pass must not prune from the local record", inv.union())
		}
	}

	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; the pass after the prune must do nothing", r.patchNames(), r.deleteNames())
	}
}

func TestPruneSuspendsWhileAControlPlaneHasNotContributed(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing contribution suspends pruning", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {"alpha": {kubeCM("old")}},
		}})
		for _, name := range []string{"cp1", "cp2"} {
			if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode(name), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("kept", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		assertApplied(t, r, "kept")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; pruning must suspend while cp2 has not contributed", r.deleteNames())
		}

		if err := publishInventory(ctx, r.fakeCs, "cp2", nil); err != nil {
			t.Fatalf("publish cp2's contribution: %v", err)
		}
		r.reset()
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want exactly old once cp2 has contributed", r.deleteNames())
		}
		inv := clusterInventory(t, r.fakeCs)
		if _, ok := inv.Nodes["cp2"]; !ok {
			t.Errorf("inventory after the pass: %+v, want cp2's entry kept", inv)
		}
	})

	t.Run("an explicit recovery entry closes the migration", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		seedInventory(t, r.fakeCs, pruneInventory{
			Nodes: map[string]map[string][]objRef{
				"cp1": {"alpha": {kubeCM("old")}},
			},
			Recovery: true,
		})
		for _, name := range []string{"cp1", "cp2"} {
			if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode(name), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("kept", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		assertApplied(t, r, "kept")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want exactly old — the recovery entry closes the migration without cp2", r.deleteNames())
		}
	})

	t.Run("an inventory that cannot be read applies and prunes nothing", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
			Data:       map[string]string{inventoryDataKey: `{"nodes": {"cp1":`},
		}
		if _, err := r.fakeCs.CoreV1().ConfigMaps(inventoryNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"cp1", "cp2"} {
			if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode(name), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("kept", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		noPatchOf(t, r, "configmaps", "kept")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; an unreadable inventory must prune nothing, like a corrupt local record", r.deleteNames())
		}
	})
}

func TestPruneKeepsTheKeepSetAcrossALeaderChange(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dirA, dirB := r.dir, t.TempDir()
	for _, name := range []string{"cp1", "cp2"} {
		if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode(name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	writeManifest(t, filepath.Join(dirA, "alpha", "stack.yaml"),
		configMap("a", "kube-system")+"---\n"+configMap("kept", "kube-system"))
	if err := syncInventory(ctx, r.fakeCs, dirA, "cp1"); err != nil {
		t.Fatalf("publish cp1's contribution: %v", err)
	}
	dA := r.deployerFor(dirA, "cp1")
	dA.reconcileManifests(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "a")
	assertApplied(t, r, "kept")
	if len(r.deletes) != 0 {
		t.Fatalf("deletes %v; cp1's pass must suspend while cp2 has not contributed", r.deleteNames())
	}
	inv := clusterInventory(t, r.fakeCs)
	if got := inv.Nodes["cp1"]["alpha"]; len(got) != 2 {
		t.Errorf("cp1's entry after its pass: %+v, want a and kept — the suspended pass still records what it applied", got)
	}

	writeManifest(t, filepath.Join(dirA, "alpha", "stack.yaml"), configMap("kept", "kube-system"))
	writeManifest(t, filepath.Join(dirB, "alpha", "stack.yaml"), configMap("kept", "kube-system"))
	if err := syncInventory(ctx, r.fakeCs, dirB, "cp2"); err != nil {
		t.Fatalf("publish cp2's contribution: %v", err)
	}
	r.reset()
	dB := r.deployerFor(dirB, "cp2")
	dB.reconcileManifests(ctx, "10.0.0.1", "10.0.0.11")

	assertApplied(t, r, "kept")
	if len(r.deletes) != 1 || r.deletes[0].GetName() != "a" {
		t.Fatalf("deletes %v, want exactly a — remembered by cp1's cluster entry, unknown to cp2's own record", r.deleteNames())
	}
	inv = clusterInventory(t, r.fakeCs)
	for _, ref := range inv.union() {
		if ref.Name == "a" {
			t.Errorf("the union still lists a: %+v; the pruned object must leave the inventory", inv.union())
		}
	}
	if got := inv.Nodes["cp2"]["alpha"]; len(got) != 1 || got[0].Name != "kept" {
		t.Errorf("cp2's entry after its pass: %+v, want kept", got)
	}

	r.reset()
	dA.reconcileManifests(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v; cp1's pass must keep the keep-set cp2's pass left", r.deleteNames())
	}
	inv = clusterInventory(t, r.fakeCs)
	foundKept := false
	for _, ref := range inv.union() {
		if ref.Name != "kept" {
			t.Errorf("the union lists %s: %+v; the inventory holds what the files declare, nothing else", ref.Name, inv.union())
			continue
		}
		foundKept = true
	}
	if !foundKept {
		t.Errorf("the union after both passes: %+v, want kept", inv.union())
	}
}

func TestDesiredWithoutARecordAppliesAsToday(t *testing.T) {
	ctx := context.Background()

	t.Run("no inventory ConfigMap at all", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		writeManifest(t, filepath.Join(r.dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		assertApplied(t, r, "a")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; a cluster without an inventory must apply as today", r.deleteNames())
		}
	})

	t.Run("an inventory without a desired record", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode("cp1"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {"alpha": {kubeCM("old")}},
		}})
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("a", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		assertApplied(t, r, "a")
		if len(r.deletes) != 1 || r.deletes[0].GetName() != "old" {
			t.Fatalf("deletes %v, want exactly old — without a desired record the pass prunes as today", r.deleteNames())
		}
	})
}

func TestDesiredStaleIntentRefusesThePass(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dir := r.dir
	content := configMap("a", "kube-system")
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), content)
	seedInventory(t, r.fakeCs, pruneInventory{Desired: &desiredGeneration{
		Generation: 3,
		Digest:     "sha256:not-the-staged-tree",
		Provenance: "stratum up on cp2",
	}})

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; a staged tree that does not match the recorded desired generation must not be applied this pass",
			r.patchNames(), r.deleteNames())
	}
	if logs := buf.String(); !strings.Contains(logs, "sha256:not-the-staged-tree") {
		t.Errorf("refusal logs %q; the pass must log why it refused, naming the record it read", logs)
	}
	inv := clusterInventory(t, r.fakeCs)
	if inv.Desired == nil || inv.Desired.Generation != 3 || inv.Desired.Digest != "sha256:not-the-staged-tree" {
		t.Errorf("desired record after the refused pass: %+v, want it untouched — the deployer reads the record, it does not write it", inv.Desired)
	}

	staged := DigestBundles(map[string][]byte{"alpha/stack.yaml": []byte(content)})
	if _, err := RecordDesiredGeneration(ctx, r.fakeCs, staged, "stratum up on cp1", []string{"alpha"}); err != nil {
		t.Fatalf("re-stage the matching set: %v", err)
	}
	log.SetOutput(prevOut)
	buf.Reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	assertApplied(t, r, "a")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v; the matching pass applies as usual", r.deleteNames())
	}
	inv = clusterInventory(t, r.fakeCs)
	if inv.Desired == nil || inv.Desired.Generation != 4 || inv.Desired.Digest != staged {
		t.Errorf("desired record after the matching staging: %+v, want generation 4 with the staged digest", inv.Desired)
	}
}

func TestDesiredHandDroppedStackIsApplyOnly(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dir := r.dir
	alpha := configMap("a", "kube-system")
	beta := configMap("b", "kube-system")
	dropped := configMap("dropped", "kube-system")
	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), alpha)
	writeManifest(t, filepath.Join(dir, "beta", "beta.yaml"), beta)
	writeManifest(t, filepath.Join(dir, "extra", "dropped.yaml"), dropped)
	staged := DigestBundles(map[string][]byte{
		"alpha/stack.yaml": []byte(alpha),
		"beta/beta.yaml":   []byte(beta),
	})
	seedInventory(t, r.fakeCs, pruneInventory{Desired: &desiredGeneration{
		Generation: 2,
		Digest:     staged,
		Provenance: "stratum up on cp1",
		Stacks:     []string{"alpha", "beta"},
	}})

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	if strings.Contains(buf.String(), "does not match the recorded desired generation") {
		t.Errorf("the pass refused the staged tree over a hand-dropped stack directory: %s", buf.String())
	}
	assertApplied(t, r, "a")
	assertApplied(t, r, "b")
	assertApplied(t, r, "dropped")
}

func TestDesiredEmptyGenerationPrunesTheLastStack(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode("cp1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	seedInventory(t, r.fakeCs, pruneInventory{
		Nodes: map[string]map[string][]objRef{
			"cp1": {"alpha": {kubeCM("last-stack"), kubeCM("second")}},
		},
		Desired: &desiredGeneration{
			Generation: 9,
			Digest:     DigestBundles(nil),
			Provenance: "stratum up with the operator disabled",
		},
	})

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	if len(r.patches) != 0 {
		t.Errorf("patched %v; an empty staged tree applies nothing", r.patchNames())
	}
	if len(r.deletes) != 2 {
		t.Fatalf("deletes %v, want exactly last-stack and second — the desired-empty generation prunes the last stack's recorded objects", r.deleteNames())
	}
	got := map[string]bool{}
	for _, d := range r.deletes {
		got[d.GetName()] = true
	}
	for _, name := range []string{"last-stack", "second"} {
		if !got[name] {
			t.Errorf("delete of %s missing from %v", name, r.deleteNames())
		}
	}
	inv := clusterInventory(t, r.fakeCs)
	if got := inv.Nodes["cp1"]["alpha"]; len(got) != 0 {
		t.Errorf("cp1's entry after the pass: %+v, want empty — nothing is declared, so nothing stays recorded", got)
	}
	if inv.Desired == nil || inv.Desired.Generation != 9 || inv.Desired.Digest != DigestBundles(nil) {
		t.Errorf("desired record after the pass: %+v, want it untouched — a pass does not re-record the generation it read", inv.Desired)
	}
}

func TestDesiredRollbackReStagingAnOlderSetApplies(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dir := r.dir
	if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode("cp1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stack := filepath.Join(dir, "alpha", "stack.yaml")
	digestOf := func(content string) string {
		return DigestBundles(map[string][]byte{"alpha/stack.yaml": []byte(content)})
	}

	newer := configMap("v2", "kube-system")
	writeManifest(t, stack, newer)
	gen, err := RecordDesiredGeneration(ctx, r.fakeCs, digestOf(newer), "stage the v2 set", []string{"alpha"})
	if err != nil {
		t.Fatalf("stage the v2 set: %v", err)
	}
	if gen != 1 {
		t.Fatalf("generation after the first staging = %d, want 1", gen)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "v2")
	if len(r.deletes) != 0 {
		t.Fatalf("deletes %v after the first pass, want none", r.deleteNames())
	}

	older := configMap("v1", "kube-system")
	writeManifest(t, stack, older)
	if gen, err = RecordDesiredGeneration(ctx, r.fakeCs, digestOf(older), "roll back to the v1 set", []string{"alpha"}); err != nil {
		t.Fatalf("stage the v1 set: %v", err)
	}
	if gen != 2 {
		t.Fatalf("generation after the rollback staging = %d, want 2", gen)
	}
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "v1")
	if len(r.deletes) != 1 || r.deletes[0].GetName() != "v2" {
		t.Fatalf("deletes %v, want exactly v2 pruned by the rolled-back pass", r.deleteNames())
	}

	writeManifest(t, stack, newer)
	if gen, err = RecordDesiredGeneration(ctx, r.fakeCs, digestOf(newer), "re-stage the v2 set", []string{"alpha"}); err != nil {
		t.Fatalf("re-stage the v2 set: %v", err)
	}
	if gen != 3 {
		t.Fatalf("generation after re-staging the earlier digest = %d, want 3 — no monotonic version comparison", gen)
	}
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "v2")
	if len(r.deletes) != 1 || r.deletes[0].GetName() != "v1" {
		t.Fatalf("deletes %v, want exactly v1 pruned", r.deleteNames())
	}
	inv := clusterInventory(t, r.fakeCs)
	if inv.Desired == nil || inv.Desired.Generation != 3 || inv.Desired.Digest != digestOf(newer) {
		t.Errorf("desired record after the re-staging: %+v, want generation 3 with the v2 digest", inv.Desired)
	}
}

type recordedClusterInputs struct {
	DNSAddress     string `json:"dnsAddress"`
	ClusterDomain  string `json:"clusterDomain"`
	DualStack      bool   `json:"dualStack"`
	StorageDefault string `json:"storageDefault"`
}

func seedClusterInputs(t *testing.T, cs *k8sfake.Clientset, inputs recordedClusterInputs) {
	t.Helper()
	b, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	configMaps := cs.CoreV1().ConfigMaps(inventoryNamespace)
	ctx := context.Background()
	cm, err := configMaps.Get(ctx, inventoryConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
			Data:       map[string]string{"cluster-inputs": string(b)},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed the cluster inputs: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("read the inventory ConfigMap: %v", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["cluster-inputs"] = string(b)
	if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seed the cluster inputs: %v", err)
	}
}

func readClusterInputs(t *testing.T, cs *k8sfake.Clientset) recordedClusterInputs {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(inventoryNamespace).Get(context.Background(), inventoryConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the inventory ConfigMap: %v", err)
	}
	var inputs recordedClusterInputs
	if err := json.Unmarshal([]byte(cm.Data["cluster-inputs"]), &inputs); err != nil {
		t.Fatalf("parse the cluster inputs: %v", err)
	}
	return inputs
}

const sentinelCorednsStack = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: coredns
  namespace: kube-system
spec:
  selector:
    matchLabels:
      k8s-app: kube-dns
  template:
    metadata:
      labels:
        k8s-app: kube-dns
    spec:
      containers:
      - name: coredns
        image: mirror.example/stratum/coredns:1.12.0
        env:
        - name: STRATUM_DNS_ADDRESS
          value: "__STRATUM_DNS_IP__"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns
  namespace: kube-system
data:
  Corefile: |
    kubernetes __STRATUM_CLUSTER_DOMAIN__ in-addr.arpa ip6.arpa {
        errors
    }
`

const sentinelCiliumStack = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cilium-config
  namespace: kube-system
data:
  enable-ipv6: "__STRATUM_ENABLE_IPV6__"
`

const sentinelLocalPathStack = `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: local-path
  annotations:
    storageclass.kubernetes.io/is-default-class: "__STRATUM_STORAGE_DEFAULT__"
provisioner: rancher.io/local-path
`

func patchBody(t *testing.T, p clienttesting.PatchActionImpl) string {
	t.Helper()
	return string(p.GetPatch())
}

func patchesOf(t *testing.T, r *manifestReactor, resource, name string) []clienttesting.PatchActionImpl {
	t.Helper()
	var found []clienttesting.PatchActionImpl
	for _, p := range r.patches {
		if p.GetResource().Resource == resource && p.GetName() == name {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no %s patch recorded for %s; applied %v", resource, name, r.patchNames())
	}
	return found
}

func noPatchOf(t *testing.T, r *manifestReactor, resource, name string) {
	t.Helper()
	for _, p := range r.patches {
		if p.GetResource().Resource == resource && p.GetName() == name {
			t.Fatalf("%s %s was applied (%s); a pass that cannot resolve its sentinels must apply nothing from that file", resource, name, p.GetPatch())
		}
	}
}

func noTokenInPatches(t *testing.T, r *manifestReactor) {
	t.Helper()
	for _, p := range r.patches {
		if strings.Contains(string(p.GetPatch()), "__STRATUM_") {
			t.Errorf("patch of %s %s still carries a sentinel: %s", p.GetResource(), p.GetName(), p.GetPatch())
		}
	}
}

func TestSentinelClusterInputsApplyFromClusterState(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)

	writeManifest(t, filepath.Join(r.dir, "coredns", "coredns.yaml"), strings.NewReplacer(
		`"__STRATUM_DNS_IP__"`, `"10.96.0.10"`,
		`__STRATUM_CLUSTER_DOMAIN__`, "cluster.local",
	).Replace(sentinelCorednsStack))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	for _, p := range patchesOf(t, r, "deployments", "coredns") {
		if body := patchBody(t, p); !strings.Contains(body, "10.96.0.10") {
			t.Errorf("the staged legacy render applied %s; want the baked 10.96.0.10", body)
		}
	}

	writeManifest(t, filepath.Join(r.dir, "coredns", "coredns.yaml"), sentinelCorednsStack)
	writeManifest(t, filepath.Join(r.dir, "cilium", "cilium.yaml"), sentinelCiliumStack)
	writeManifest(t, filepath.Join(r.dir, "local-path", "local-path.yaml"), sentinelLocalPathStack)
	writeManifest(t, filepath.Join(r.dir, "beta", "stack.yaml"), configMap("kept", "kube-system"))
	seedClusterInputs(t, r.fakeCs, recordedClusterInputs{
		DNSAddress:     "10.43.0.10",
		ClusterDomain:  "k8s.example.internal",
		DualStack:      true,
		StorageDefault: "local-path",
	})

	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	noTokenInPatches(t, r)
	for _, p := range patchesOf(t, r, "deployments", "coredns") {
		if body := patchBody(t, p); !strings.Contains(body, `"10.43.0.10"`) {
			t.Errorf("CoreDNS Deployment applied %s; want the cluster's DNS address 10.43.0.10, not this node's 10.96.0.10 and not the sentinel", body)
		}
		if strings.Contains(patchBody(t, p), "10.96.0.10") {
			t.Errorf("CoreDNS Deployment applied %s; the staging node's own 10.96.0.10 must not survive the re-stage", patchBody(t, p))
		}
	}
	for _, p := range patchesOf(t, r, "configmaps", "coredns") {
		if body := patchBody(t, p); !strings.Contains(body, "kubernetes k8s.example.internal") {
			t.Errorf("CoreDNS Corefile applied %s; want the cluster's domain k8s.example.internal, not cluster.local and not the sentinel", body)
		}
	}
	for _, p := range patchesOf(t, r, "configmaps", "cilium-config") {
		if body := patchBody(t, p); !strings.Contains(body, `"enable-ipv6":"true"`) {
			t.Errorf("Cilium config applied %s; want enable-ipv6 \"true\" from the cluster's dual-stack record", body)
		}
	}
	for _, p := range patchesOf(t, r, "storageclasses", "local-path") {
		if body := patchBody(t, p); !strings.Contains(body, `"storageclass.kubernetes.io/is-default-class":"true"`) {
			t.Errorf("local-path StorageClass applied %s; want the cluster's local-path default claimed", body)
		}
	}
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v; the convergence pass declares everything it applied", r.deleteNames())
	}

	if got := readClusterInputs(t, r.fakeCs); got.DNSAddress != "10.43.0.10" || got.ClusterDomain != "k8s.example.internal" || !got.DualStack || got.StorageDefault != "local-path" {
		t.Errorf("cluster inputs after the pass: %+v, want the recorded record untouched — a pass resolves the inputs, it does not rewrite them", got)
	}

	cp1Corefile := patchBody(t, patchesOf(t, r, "configmaps", "coredns")[0])

	dir2 := t.TempDir()
	writeManifest(t, filepath.Join(dir2, "coredns", "coredns.yaml"), sentinelCorednsStack)
	writeManifest(t, filepath.Join(dir2, "cilium", "cilium.yaml"), sentinelCiliumStack)
	writeManifest(t, filepath.Join(dir2, "local-path", "local-path.yaml"), sentinelLocalPathStack)
	writeManifest(t, filepath.Join(dir2, "beta", "stack.yaml"), configMap("kept", "kube-system"))
	d2 := r.deployerFor(dir2, "cp2")
	r.reset()
	d2.reconcileManifests(ctx, "10.0.0.1", "10.0.0.12")

	noTokenInPatches(t, r)
	for _, p := range patchesOf(t, r, "deployments", "coredns") {
		if body := patchBody(t, p); !strings.Contains(body, `"10.43.0.10"`) {
			t.Errorf("cp2's CoreDNS Deployment applied %s; want the cluster's DNS address, not cp2's own", body)
		}
	}
	for _, p := range patchesOf(t, r, "configmaps", "coredns") {
		if body := patchBody(t, p); !strings.Contains(body, "kubernetes k8s.example.internal") {
			t.Errorf("cp2's CoreDNS Corefile applied %s; want the cluster's domain", body)
		}
	}
	if cp2Corefile := patchBody(t, patchesOf(t, r, "configmaps", "coredns")[0]); cp1Corefile != cp2Corefile {
		t.Errorf("the two control planes applied different Corefile payloads:\ncp1: %s\ncp2: %s\n— the applied bytes must not depend on which control plane leads", cp1Corefile, cp2Corefile)
	}
}

func TestSentinelClusterInputsFillTheNegatives(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	writeManifest(t, filepath.Join(r.dir, "cilium", "cilium.yaml"), sentinelCiliumStack)
	writeManifest(t, filepath.Join(r.dir, "local-path", "local-path.yaml"), sentinelLocalPathStack)
	seedClusterInputs(t, r.fakeCs, recordedClusterInputs{ClusterDomain: "cluster.local", StorageDefault: "none"})

	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

	noTokenInPatches(t, r)
	for _, p := range patchesOf(t, r, "configmaps", "cilium-config") {
		if body := patchBody(t, p); !strings.Contains(body, `"enable-ipv6":"false"`) {
			t.Errorf("Cilium config applied %s; want enable-ipv6 \"false\" — the cluster record is not dual-stack", body)
		}
	}
	for _, p := range patchesOf(t, r, "storageclasses", "local-path") {
		if body := patchBody(t, p); !strings.Contains(body, `"storageclass.kubernetes.io/is-default-class":"false"`) {
			t.Errorf("local-path StorageClass applied %s; want the default claim off — the cluster's storage default is none", body)
		}
	}
}

func TestSentinelUnresolvableSuspendsPruning(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing record skips the file and suspends pruning", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {
				"coredns": {kubeCM("old-coredns")},
				"beta":    {kubeCM("old-beta")},
			},
		}})
		writeManifest(t, filepath.Join(dir, "coredns", "coredns.yaml"), sentinelCorednsStack)
		writeManifest(t, filepath.Join(dir, "beta", "stack.yaml"), configMap("kept", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		noPatchOf(t, r, "deployments", "coredns")
		noPatchOf(t, r, "configmaps", "coredns")
		assertApplied(t, r, "kept")
		if len(r.deletes) != 0 {
			t.Fatalf("deletes %v; an unresolved input must suspend all pruning", r.deleteNames())
		}
		for _, d := range r.deletes {
			if d.GetName() == "old-coredns" {
				t.Fatalf("deletes %v; the skipped stack's recorded object must survive the pass that skipped it", r.deleteNames())
			}
		}
		if _, ok := r.stateFor(filepath.Join(dir, "coredns", "coredns.yaml")); ok {
			t.Errorf("the skipped file got an entry in the apply state; a skipped file must be retried next pass")
		}

		seedClusterInputs(t, r.fakeCs, recordedClusterInputs{
			DNSAddress:     "10.43.0.10",
			ClusterDomain:  "k8s.example.internal",
			DualStack:      true,
			StorageDefault: "local-path",
		})
		r.reset()
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		noTokenInPatches(t, r)
		for _, p := range patchesOf(t, r, "deployments", "coredns") {
			if body := patchBody(t, p); !strings.Contains(body, `"10.43.0.10"`) {
				t.Errorf("CoreDNS Deployment applied %s after the record landed; want the cluster's DNS address", body)
			}
		}
		for _, p := range patchesOf(t, r, "configmaps", "coredns") {
			if body := patchBody(t, p); !strings.Contains(body, "kubernetes k8s.example.internal") {
				t.Errorf("CoreDNS Corefile applied %s after the record landed; want the cluster's domain", body)
			}
		}
		if len(r.deletes) != 2 {
			t.Fatalf("deletes %v, want old-coredns and old-beta after the complete pass", r.deleteNames())
		}
	})

	t.Run("a record missing one value skips the file rather than applying it half-filled", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {"beta": {kubeCM("old-beta")}},
		}})
		writeManifest(t, filepath.Join(dir, "coredns", "coredns.yaml"), sentinelCorednsStack)
		writeManifest(t, filepath.Join(dir, "beta", "stack.yaml"), configMap("kept", "kube-system"))
		seedClusterInputs(t, r.fakeCs, recordedClusterInputs{ClusterDomain: "k8s.example.internal"})

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		noPatchOf(t, r, "deployments", "coredns")
		noPatchOf(t, r, "configmaps", "coredns")
		assertApplied(t, r, "kept")
		if len(r.deletes) != 0 {
			t.Fatalf("deletes %v; a partial input record must suspend all pruning", r.deleteNames())
		}
		noTokenInPatches(t, r)
	})

	t.Run("an inventory that cannot be read suspends the whole pass", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
			Data:       map[string]string{inventoryDataKey: `{"nodes": {"cp1":`},
		}
		if _, err := r.fakeCs.CoreV1().ConfigMaps(inventoryNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		writeManifest(t, filepath.Join(dir, "coredns", "coredns.yaml"), sentinelCorednsStack)
		writeManifest(t, filepath.Join(dir, "beta", "stack.yaml"), configMap("kept", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")

		noPatchOf(t, r, "deployments", "coredns")
		noPatchOf(t, r, "configmaps", "coredns")
		noPatchOf(t, r, "configmaps", "kept")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v; cluster state that cannot be read suspends pruning, as for the Cilium sentinel", r.deleteNames())
		}
		noTokenInPatches(t, r)
	})
}
