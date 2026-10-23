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

// The manifest deployer's pins share one fixture: a manifestDeployer value
// under test, built from the fake dynamic client, a static DefaultRESTMapper,
// a temp dir and an empty apply state, with reactors that record every apply
// and delete instead of consulting the tracker — the fake's tracker applies a
// patch only to an object that already exists, so the apply reactor returns
// the patched object itself. The deployer value carries the seams the old
// manifestDyn/manifestMapper/manifestState package variables held: its dynamic
// client (fakeDyn), its RESTMapper (rebuilt in place from the fixture's
// discovery client on a no-match, which setDisco steers), its per-file apply
// state (read and aged through the reactor) and its clock (time.Now; the
// resync window is aged through ageLastApply, never the wall clock). The
// deployer also carries the inventory seams — its typed clientset (fakeCs)
// and this instance's node name — so a pass reads and writes the prune
// inventory in cluster state. Stacks are files under the temp dir; record
// reads the prune record as a map from stack to object references. It is the
// one fixture line a ConfigMap-backed record changes. Tests touch the
// deployer only through the reactor, so the tests themselves stay.

type manifestReactor struct {
	patches []clienttesting.PatchActionImpl
	deletes []clienttesting.DeleteAction

	fakeDyn   *dynamicfake.FakeDynamicClient
	fakeDisco *discoveryfake.FakeDiscovery
	fakeCs    *k8sfake.Clientset

	// d is the deployer value under test; dir is its manifests directory, the
	// temp dir the tests lay their stacks and the prune record in.
	d   *manifestDeployer
	dir string

	failNextPatchOf  string // one-shot: the next patch of this object errors
	failNextDeleteOf string // one-shot: the next delete of this object errors
	deleteNotFound   bool   // every delete returns NotFound
}

// deployFixture builds the deployer from the fake dynamic client, a
// DefaultRESTMapper and a temp dir, wires the recording reactors, and returns
// the reactor. Each fixture owns its deployer, so tests sharing no fixture
// share no deployer state either.
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
	// Deployments and StorageClasses record like the others: the sentinel rows
	// apply the CoreDNS Deployment and the local-path StorageClass.
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

// deployerFor builds a second deployer value on the same fake cluster — its
// own manifests directory, apply state and node identity, sharing the dynamic
// client, the discovery client, the RESTMapper and the typed clientset. The
// leader-change test drives two deployers that stand for two operator
// instances of one cluster.
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

// reconcile runs one manifest pass: the deployer applies and prunes its own
// manifests directory.
func (r *manifestReactor) reconcile(ctx context.Context, vip, selfIP string) {
	r.d.reconcileManifests(ctx, vip, selfIP)
}

// setDir points the deployer at a different manifests directory — the
// missing-directory case, whose path no fixture-created temp dir is.
func (r *manifestReactor) setDir(dir string) { r.d.dir = dir }

// setDisco swaps the discovery client the mapper rebuild consults, so a test
// can let discovery know a kind the static mapper predates.
func (r *manifestReactor) setDisco(d *discoveryfake.FakeDiscovery) {
	r.d.disco = d
	r.fakeDisco = d
}

// setMapper swaps the RESTMapper the deployer resolves against — the
// canonical-identity tests need one mapper that resolves two versions of a
// kind to the same resource, which the fixture's static mapper does not hold.
func (r *manifestReactor) setMapper(m meta.RESTMapper) { r.d.mapper = m }

// restMapper reads the mapper a pass would use: a no-match rebuild replaces
// it, so this must follow the live seam, not the fixture's original.
func (r *manifestReactor) restMapper() meta.RESTMapper { return r.d.mapper }

// stateFor reads the per-file apply state a pass recorded for path.
func (r *manifestReactor) stateFor(path string) (manifestFileState, bool) {
	st, ok := r.d.state[path]
	return st, ok
}

// ageLastApply backdates a file's last apply, ageing it past the resync window
// without touching its bytes.
func (r *manifestReactor) ageLastApply(path string, back time.Duration) {
	st, ok := r.d.state[path]
	if !ok {
		return
	}
	st.lastApply = st.lastApply.Add(back)
	r.d.state[path] = st
}

// restart drops the deployer's per-file apply state — the state a fresh
// deployer value starts with after an operator restart — while the on-disk
// prune record and the cluster keep whatever earlier passes left.
func (r *manifestReactor) restart() { r.d.state = map[string]manifestFileState{} }

// resolveAPIServer resolves the Cilium apiserver settings through the
// deployer's dynamic client.
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

// record reads the prune record as a map from stack to object references.
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

// assertApplied checks that every recorded patch is an apply-patch by
// stratum-operator with force set, and returns the patches for name.
func assertApplied(t *testing.T, r *manifestReactor, name string) []clienttesting.PatchActionImpl {
	t.Helper()
	var found []clienttesting.PatchActionImpl
	for _, p := range r.patches {
		if p.GetPatchType() != types.ApplyPatchType {
			t.Errorf("patch of %s: got patch type %v, want ApplyPatchType", name, p.GetPatchType())
			continue
		}
		// The released ownership name must remain stable across refactors;
		// deriving the expectation from fieldManager would hide its rename.
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

// cp builds a labelled control-plane Node for the apiserver-resolution row.
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

// TestReconcileManifestsPrunesWhatAStackStopsDeclaring: an object a stack stops
// declaring is deleted on the next pass, and everything it still declares stays
// applied and in the record.
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

	// Pass 2: the stack no longer declares b.
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

// TestReconcileManifestsPrunesNothingAfterAFailedFile: a pass that failed to
// read intent — an apply error, a file that does not parse, an unresolvable
// Cilium apiserver — or that found no file at all, deletes nothing and leaves
// the record alone; the next clean pass applies what failed and prunes.
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

		// The clean pass applies the failed file again — it got no entry in
		// the deployer's apply state — and prunes the object the files stopped declaring.
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
		r := deployFixture(t, cp("cp1", "")) // labelled control plane with no address
		dir := r.dir
		seedRecord(t, dir, seed)
		writeManifest(t, filepath.Join(dir, "alpha", "cilium.yaml"),
			configMap("cilium-config", "kube-system")+"  k8s-api-server-host: \"__STRATUM_APISERVER_HOST__\"\n")

		// No VIP: the sentinel must resolve from the labelled Nodes, and cp1
		// reports no address, so the file is skipped this pass.
		r.reconcile(ctx, "", "10.0.0.11")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v, want none after a failed apiserver resolution", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after failed pass: %+v, want old alone", got["alpha"])
		}

		// A VIP resolves the sentinel without the Nodes: the file applies and
		// the object the files stopped declaring is pruned.
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
		// `helm template > stack.yaml` truncates the file before it writes.
		writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), "")
		writeManifest(t, filepath.Join(dir, "beta", "other.yaml"), configMap("b", "kube-system"))

		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(r.deletes) != 0 {
			t.Errorf("deletes %v, want none while a stack file is empty", r.deleteNames())
		}
		if got := record(t, dir); len(got["alpha"]) != 1 || got["alpha"][0].Name != "old" {
			t.Errorf("record after empty-file pass: %+v, want old alone", got["alpha"])
		}

		// Removing the stack's directory is how a stack is deleted.
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

// TestPruneKeepsAFailedDeleteInTheRecord: a delete that errors stays in the
// record so the next pass retries it; one answered NotFound is treated as done
// and dropped at once.
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

// TestReconcileManifestsNeverPrunesLooseFiles: a loose file in the manifests
// directory itself is applied without a stack label, and its removal deletes
// nothing — the operator drop-a-file-and-go path stays apply-only.
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

	// Pass 2: the loose file is gone, the stack keeps a file so the pass still
	// runs its prune — and deletes nothing.
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

// TestReconcileManifestsReappliesOnlyOnChangeOrResync: a file whose bytes are
// unchanged is re-applied only after the five-minute resync window — but it
// still feeds the prune record's keep-set on every pass, so an unchanged stack
// is never pruned.
func TestReconcileManifestsReappliesOnlyOnChangeOrResync(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	stack := filepath.Join(dir, "alpha", "stack.yaml")
	writeManifest(t, stack, configMap("a", "kube-system")+"---\n"+configMap("b", "kube-system"))
	ctx := context.Background()

	// Pass 1: the fresh file applies.
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "a")
	assertApplied(t, r, "b")

	// Pass 2, nothing changed and well inside the resync window: nothing is
	// applied or deleted, yet every object of alpha stays in the record.
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; an unchanged file inside the resync window must not re-apply",
			r.patchNames(), r.deleteNames())
	}
	if got := record(t, dir)["alpha"]; len(got) != 2 {
		t.Errorf("record under alpha after the unchanged pass: %+v, want a and b — the unchanged file still feeds the keep-set", got)
	}

	// Pass 3: the file's last apply is aged past the resync window, so the
	// whole file re-applies even though its bytes are the same.
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

	// Pass 4: changed bytes re-apply the file at once.
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

// TestReconcileManifestsARestartedDeployerReappliesOnce: the per-file apply
// state lives only for the running process, in the deployer value, so an
// operator restart, a fresh deployer with empty state, re-applies every file
// once even though no byte changed. The restart loses no prune inventory: the
// record is on disk, and the fresh pass keeps it exact and then goes quiet
// again.
func TestReconcileManifestsARestartedDeployerReappliesOnce(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	stack := filepath.Join(dir, "alpha", "stack.yaml")
	writeManifest(t, stack, configMap("a", "kube-system"))
	ctx := context.Background()

	// Pass 1 of the first process applies the fresh file.
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	assertApplied(t, r, "a")

	// The same process's next pass is inside the resync window: nothing moves.
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Fatalf("patched %v, deleted %v; the same process must not re-apply inside the resync window",
			r.patchNames(), r.deleteNames())
	}

	// A restart: the fresh deployer's apply state starts empty, so the
	// unchanged file is applied again — exactly once.
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

	// The process after the restart is quiet again: its state now tracks the
	// file, so the resync window once more applies nothing.
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; the pass after the restart must be quiet again", r.patchNames(), r.deleteNames())
	}
}

// TestPruneReadsTheRecordFormatThisReleaseWrites: the golden record
// testdata/applied.json is today's .applied.json, generated once from a real
// reconcile pass. It stands for every record an earlier control plane may have
// written; a release that renames or reshapes the record type breaks the
// hand-off between control planes. The test seeds the golden as the on-disk
// record and checks that pruning still reads it: the object the stack stops
// declaring is deleted, the one it keeps stays.
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

	// The record is exactly what the release wrote; the stack now declares
	// only a, so the recorded b must be pruned.
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

// A corrupt legacy record remains recovery evidence until an administrator
// explicitly accepts the missing contribution in the cluster inventory.
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

// TestPruneRecordWriteFailureIsRetriedNextPass: a prune whose record write
// fails leaves the on-disk record byte for byte as it was — the tmp+rename
// write never touches it — so the next pass re-reads the stale record and
// re-issues the prune. The manifests themselves are not re-applied inside the
// resync window: change detection reads the deployer's apply state, never the record.
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
	// The record's tmp name is taken by a directory, so the record write fails
	// for any user; .applied.json itself stays readable.
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
	// No state corruption: the failed write left the seeded record untouched.
	if got, err := os.ReadFile(filepath.Join(dir, ".applied.json")); err != nil || string(got) != seedBytes {
		t.Errorf("record after the failed write: %q (%v), want the seed %q unchanged", got, err, seedBytes)
	}

	// The next pass re-issues the prune from the stale record, and the write
	// it couldn't do before now lands.
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

// TestReconcileManifestsEmptyDirReturnsBeforePruning: today a manifests
// directory with no YAML — or no directory at all — returns before pruning,
// so objects left in the record survive with nothing on disk declaring them.
// This is the baseline the desired-empty generation replaces.
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

// TestReconcileManifestsRebuildsItsMapperForANewCRD: a kind the cached
// RESTMapper predates (a CRD installed after the operator started) is applied
// on the pass that first needs it, because applyManifestObject rebuilds the
// mapper from discovery once on a no-match error and retries. When discovery
// does not know the kind either, the pass applies nothing from that file and
// prunes nothing.
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
		r.setDisco(disco(true)) // discovery knows what the static mapper does not
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

// TestPruneKeepsAnObjectMovedBetweenStacks: a release that moves an object
// from one stack to another — a bundle renamed from cilium/ to coredns/ —
// applies it under the new stack on the pass that follows, and that pass must
// not delete it. The keep-set is the whole pass's union, not one stack's
// files: the object the old stack's record remembers but its files no longer
// declare is still declared by the stack that now owns it. Today pruneStacks
// prunes each stack against that stack alone and deletes it.
func TestPruneKeepsAnObjectMovedBetweenStacks(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	ctx := context.Background()

	writeManifest(t, filepath.Join(dir, "cilium", "stack.yaml"), configMap("shared", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := record(t, dir)["cilium"]; len(got) != 1 || got[0].Name != "shared" {
		t.Fatalf("record under cilium: %+v, want shared", got)
	}

	// The release moves the object to the coredns stack: the pass applies it
	// under the new stack and keeps it, and the record hands ownership over.
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

// TestPruneMatchesByCanonicalIdentity: the keep-set identifies an object by
// the RESTMapper's GroupResource, its effective namespace and its name — a
// GVK's version string is not identity, and a namespace the files leave
// implicit is the default one. A pass that applies an object under a
// different apiVersion, or with a newly explicit namespace, keeps the
// recorded object instead of pruning it.
func TestPruneMatchesByCanonicalIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("a GVK version flip does not orphan the recorded object", func(t *testing.T) {
		r := deployFixture(t, cp("cp1", "10.0.0.11"))
		dir := r.dir
		// Two versions of the Widget kind map to one resource: the mapper, not
		// the apiVersion string, says what the object is.
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

		// The release bumps the apiVersion: same object, same GroupResource —
		// the pass applies it and keeps it recorded instead of pruning it.
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

		// The release writes out the namespace the files left implicit: the
		// object is still applied to the default namespace either way, and
		// identity says so.
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

// TestPruneKeepsLooseObjectsWithoutOwningThem: a loose file's objects join the
// pass's keep-set, so an object a stack releases into a loose file survives
// the hand-over — but loose files stay apply-only: the record never lists
// their objects, so removing the loose file later still deletes nothing.
func TestPruneKeepsLooseObjectsWithoutOwningThem(t *testing.T) {
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	ctx := context.Background()

	writeManifest(t, filepath.Join(dir, "alpha", "stack.yaml"), configMap("m", "kube-system"))
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if got := record(t, dir)["alpha"]; len(got) != 1 || got[0].Name != "m" {
		t.Fatalf("record under alpha: %+v, want m", got)
	}

	// The release moves m out of the stack into a loose file: the pass applies
	// the loose file and keeps m — the keep-set is the whole pass — and the
	// record stops owning m, because a loose object is nobody's to prune.
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
	// A loose-file handoff starts migration so label discovery cannot restore
	// historical ownership later. The cluster inventory is now authoritative.
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

	// Removing the loose file still deletes nothing: the record never owned m.
	r.reset()
	if err := os.Remove(filepath.Join(dir, "loose.yaml")); err != nil {
		t.Fatal(err)
	}
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.deletes) != 0 {
		t.Errorf("deletes %v after the loose file is removed; the record never owned m", r.deleteNames())
	}
}

// TestPruneIsDrivenByTheClusterInventory: with the migration complete, the
// prune record is the cluster inventory — the union of every operator
// instance's entry in the kube-system stratum-addon-inventory ConfigMap — and
// the pass's own local record no longer drives anything: a stale .applied.json
// a crash left behind lists an object the cluster inventory never knew, and
// the pass leaves it alone rather than pruning on a record it does not own.
// The object the cluster inventory remembers but no file declares is pruned;
// the pass's keep-set is recorded as this node's entry, the pruned reference
// leaves every entry it was in, and another node's entry is untouched. A
// second pass is quiet.
func TestPruneIsDrivenByTheClusterInventory(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dir := r.dir
	seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
		"cp1": {"alpha": {kubeCM("old")}},  // this node's entry, from the old controller
		"cp2": {"alpha": {kubeCM("kept")}}, // another node's entry
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

	// The next pass is quiet: the inventory now holds exactly what the files
	// declare, and the unchanged file is inside the resync window.
	r.reset()
	r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
	if len(r.patches) != 0 || len(r.deletes) != 0 {
		t.Errorf("patched %v, deleted %v; the pass after the prune must do nothing", r.patchNames(), r.deleteNames())
	}
}

// TestPruneSuspendsWhileAControlPlaneHasNotContributed: the migration opens
// only when every control-plane node in cluster state has published its
// inventory entry. Pruning before cp2's contribution would delete the objects
// only cp2's record remembers — the leader's own node may never have applied
// them. Once the missing node contributes, the same pass prunes; an explicit
// recovery entry closes the migration the same way, and an inventory that
// cannot be read prunes nothing at all.
func TestPruneSuspendsWhileAControlPlaneHasNotContributed(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing contribution suspends pruning", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {"alpha": {kubeCM("old")}}, // cp2 has not contributed
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

		// cp2's instance publishes its (empty) contribution: the migration
		// closes and the next pass prunes what the inventory remembers and
		// no file declares.
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
			Recovery: true, // cp2 was decommissioned before it could contribute
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

// TestPruneKeepsTheKeepSetAcrossALeaderChange: leadership moves between two
// operator instances — two deployer values on one fake cluster, each with its
// own manifests directory, apply state and node identity — and the keep-set
// stays correct, because the prune record is the cluster inventory, not the
// leading node's local file. The old leader's entry remembers an object its
// files declared; the new leader's own record never listed it, and today a
// fresh node-local record would orphan the object forever. Under the cluster
// inventory the new leader's pass prunes exactly that object, keeps what its
// files still declare, and a later pass by either identity changes nothing.
func TestPruneKeepsTheKeepSetAcrossALeaderChange(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)
	dirA, dirB := r.dir, t.TempDir()
	for _, name := range []string{"cp1", "cp2"} {
		if _, err := r.fakeCs.CoreV1().Nodes().Create(ctx, cpNode(name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 1: cp1 leads. Its instance contributes its (empty) entry, and its
	// first pass applies the stack while the migration is still open — cp2
	// has not contributed — so it prunes nothing, but what it applied is
	// recorded as cp1's entry.
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

	// Phase 2: the release drops a on every node's directory, cp2 contributes,
	// and leadership moves to cp2. Its first pass has no local record of a —
	// only the cluster inventory does — and must still prune a, never kept.
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

	// Phase 3: a later pass by the old identity changes nothing — the
	// keep-set is the cluster inventory's, whichever instance drives it.
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

// The desired bundle generation's deployer half. The staging path records
// {generation, digest, provenance} in the cluster inventory (pinned in
// inventory_test.go); the deployer applies only a staged tree whose digest
// matches that record:
//
//   - a staged tree whose digest differs from the recorded desired generation
//     is stale intent — a hand edit, a half-written staging, a leftover — and
//     is not applied this pass; the pass logs why and touches nothing.
//   - an explicitly recorded empty bundle set — the desired-empty generation —
//     replaces the early return an empty directory made: the pass prunes the
//     objects the inventory still remembers, because empty is now what the
//     cluster asked for.
//   - a cluster with no desired record has no intent to refuse: the deployer
//     applies the staged files exactly as earlier releases did, so an
//     operator upgraded ahead of its first recording `stratum up` keeps
//     deploying.
//
// The record lives in the stratum-addon-inventory ConfigMap the prune
// inventory already uses, read through the same typed clientset.

// TestDesiredWithoutARecordAppliesAsToday: the compatibility path. A cluster
// the desired-generation record does not reach — no inventory ConfigMap at
// all, or an inventory written before its first recording staging — has no
// intent to check the staged tree against, so the pass applies and prunes as
// today. Refusing when the record is merely absent would strand every cluster
// between its operator upgrade and its next `stratum up`.
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

// TestDesiredStaleIntentRefusesThePass: a staged tree whose digest differs
// from the recorded desired generation is not applied this pass — applying
// what nobody recorded would make the node's manifests directory, not the
// apiserver record, the source of truth. The pass applies and prunes nothing,
// leaves the record untouched, and logs why, naming the record it refused
// against. Once the staged tree matches the record again, the next pass
// applies it.
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

	// Capture the log: a silent refusal would leave the pass unexplained.
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

	// The tree re-staged to match: the next pass applies it as usual, and the
	// recording staging that matched it moved the generation to 4.
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

// TestDesiredHandDroppedStackIsApplyOnly: a stack directory the desired
// record does not name is a hand-dropped one — run.go's staging documents it
// as the operator's drop-a-file contract and deliberately never deletes it, so
// no re-stage can ever cover it. The digest gate therefore digests only the
// stacks the record names: the pass applies the hand-dropped file beside the
// recorded stacks instead of refusing the whole tree — refusing would stop
// Cilium, CoreDNS and the CRDs applying and pruning cluster-wide until a
// person removed the file the staging itself says it will never remove.
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

	// Capture the log: a refusal must be visible in the failure output.
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

// TestDesiredEmptyGenerationPrunesTheLastStack: an explicitly staged empty
// bundle set is a desired state, not a missing one. The staging path records
// the empty set's digest as a generation, and the pass over an empty staged
// tree then prunes the objects the inventory still remembers — the last
// stack's objects, which today's early return on an empty directory leaves
// behind forever. Without a desired record the early return stays (the
// compatibility path above): an empty directory prunes only when the record
// says empty is intended.
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
			Digest:     DigestBundles(nil), // the staging path recorded the empty set
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

// TestDesiredRollbackReStagingAnOlderSetApplies: authorized rollback, end to
// end, with RecordDesiredGeneration standing where the staging path stands.
// The first staged set applies; staging the older set back records the next
// generation — no monotonic comparison stands in the way — and the pass that
// follows applies the rolled-back set and prunes what only the newer set
// declared. Staging the first set again is the same authorized re-staging: it
// records the generation after that and applies again.
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

	// Generation 1: the newer bundle set is staged and applied.
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

	// Generation 2: the older set is staged back — the rollback — and its
	// pass applies it and prunes what only the v2 set declared.
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

	// Generation 3: the v2 set is staged again. The digest an earlier
	// generation held is no reason to refuse it — re-staging records the next
	// generation and applies.
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

// The apply-time cluster inputs. The bundled stacks carry the four remaining
// per-control-plane inputs — the storage default, the DNS address derived from
// the service CIDR, the cluster domain and dual-stack — as named sentinels
// instead of the staging node's local values, exactly as the apiserver sentinel
// does for Cilium's endpoint: writeBundles stages the same bytes on every
// control plane, and the deployer resolves each sentinel from cluster state at
// apply time.
//
// The cluster state the sentinels resolve from is the stratum-addon-inventory
// ConfigMap beside the prune inventory and the desired generation: a
// cluster-inputs document holding the four values the staging path recorded.
// Resolution is skip-not-guess: a sentinel the cluster state cannot resolve
// leaves its file unapplied this pass, with no entry in the deployer's apply
// state, so the pass retries it; the other files still apply and prune.
//
// The sentinel tokens are the ones the bundled templates carry — CoreDNS's
// __STRATUM_DNS_IP__ and __STRATUM_CLUSTER_DOMAIN__, plus
// __STRATUM_ENABLE_IPV6__ for Cilium's dual-stack toggle and
// __STRATUM_STORAGE_DEFAULT__ for the local-path StorageClass's
// is-default-class annotation.

// recordedClusterInputs is the cluster-inputs document the staging path
// records in the inventory ConfigMap: the four values every sentinel resolves
// from.
type recordedClusterInputs struct {
	DNSAddress     string `json:"dnsAddress"`
	ClusterDomain  string `json:"clusterDomain"`
	DualStack      bool   `json:"dualStack"`
	StorageDefault string `json:"storageDefault"`
}

// seedClusterInputs writes the cluster-inputs document into the inventory
// ConfigMap's "cluster-inputs" data key, beside the prune inventory's own
// "inventory" key — creating the ConfigMap when no seed put it there first and
// preserving whatever else it holds.
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

// readClusterInputs reads the cluster-inputs document back out of the
// inventory ConfigMap, failing the test when it is missing or unreadable.
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

// sentinelCorednsStack is the CoreDNS stack the new staging renders: the
// Deployment carries the DNS-address sentinel where the rendered Service IP
// goes, and the Corefile ConfigMap carries the cluster-domain sentinel where
// the rendered domain goes.
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

// sentinelCiliumStack is Cilium's dual-stack toggle as a sentinel: the staging
// no longer reads this node's DualStack to pick true or false.
const sentinelCiliumStack = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cilium-config
  namespace: kube-system
data:
  enable-ipv6: "__STRATUM_ENABLE_IPV6__"
`

// sentinelLocalPathStack is the local-path StorageClass's default claim as a
// sentinel: whether local-path IS the cluster's default storage is decided at
// apply time from cluster state, not by the staging node's config file.
const sentinelLocalPathStack = `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: local-path
  annotations:
    storageclass.kubernetes.io/is-default-class: "__STRATUM_STORAGE_DEFAULT__"
provisioner: rancher.io/local-path
`

// patchBody returns one applied object's patch payload as a string.
func patchBody(t *testing.T, p clienttesting.PatchActionImpl) string {
	t.Helper()
	return string(p.GetPatch())
}

// patchesOf returns the recorded apply patches for the named object on the
// named resource.
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

// noPatchOf fails the test when the pass applied the named object.
func noPatchOf(t *testing.T, r *manifestReactor, resource, name string) {
	t.Helper()
	for _, p := range r.patches {
		if p.GetResource().Resource == resource && p.GetName() == name {
			t.Fatalf("%s %s was applied (%s); a pass that cannot resolve its sentinels must apply nothing from that file", resource, name, p.GetPatch())
		}
	}
}

// noTokenInPatches fails the test when any applied payload still carries a
// sentinel token: a half-resolved file must never reach the apiserver.
func noTokenInPatches(t *testing.T, r *manifestReactor) {
	t.Helper()
	for _, p := range r.patches {
		if strings.Contains(string(p.GetPatch()), "__STRATUM_") {
			t.Errorf("patch of %s %s still carries a sentinel: %s", p.GetResource(), p.GetName(), p.GetPatch())
		}
	}
}

// TestSentinelClusterInputsApplyFromClusterState: the four remaining
// per-control-plane inputs are applied from cluster state, not from the
// staging node's local values. Two control planes whose historic node states
// disagree — cp1 recorded the legacy defaults (DNS 10.96.0.10, cluster.local,
// single-stack, local-path), cp2 a custom range with its own domain and
// dual-stack — stage the SAME sentinel bytes after the re-render, and
// whichever of them leads applies the cluster's recorded inputs: the DNS
// address 10.43.0.10, the domain k8s.example.internal, dual-stack on, and
// local-path claiming the default StorageClass.
//
// Today the deployer fills no sentinels at all, so the staged tokens reach
// the apiserver literally and the two control planes would disagree with the
// record: the assertions below fail.
func TestSentinelClusterInputsApplyFromClusterState(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t)

	// Phase 1, the pre-upgrade staging: cp1's directory holds the bytes its
	// own node state rendered — the legacy defaults, baked in. The pass
	// applies them as today; nothing here is cluster-derived yet.
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

	// The upgrade's `stratum up` restart re-renders the bundles: both control
	// planes now stage the same sentinel bytes, whatever their own node state
	// says, and the staging path records the cluster's inputs beside the
	// prune inventory. The two nodes' historic values — cp1's 10.96.0.10 and
	// cluster.local, cp2's own range and domain — are gone from the bytes.
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

	// Phase 2, cp1 leads: every sentinel resolves from the cluster's record.
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

	// The staging recorded what the pass read: the cluster-inputs document
	// survives the pass's inventory rewrite untouched.
	if got := readClusterInputs(t, r.fakeCs); got.DNSAddress != "10.43.0.10" || got.ClusterDomain != "k8s.example.internal" || !got.DualStack || got.StorageDefault != "local-path" {
		t.Errorf("cluster inputs after the pass: %+v, want the recorded record untouched — a pass resolves the inputs, it does not rewrite them", got)
	}

	// The payload cp1's pass sent for the Corefile, kept for the convergence
	// comparison below.
	cp1Corefile := patchBody(t, patchesOf(t, r, "configmaps", "coredns")[0])

	// Phase 3, cp2 leads: a different node, a different node IP, a historic
	// node state that disagreed with cp1's — and the SAME applied values. The
	// two passes sent byte-identical apply payloads for the same objects.
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

// TestSentinelClusterInputsFillTheNegatives: a record that says
// not-dual-stack fills Cilium's toggle with false, and a storage default that
// is not local-path leaves the local-path StorageClass not claiming default —
// the sentinel fills what the cluster recorded, it does not guess the
// staging node's own setting.
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

// Unresolved sentinels skip their file and suspend all pruning: a skipped
// file may declare an object moved from any formerly owning stack.
func TestSentinelUnresolvableSuspendsPruning(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing record skips the file and suspends pruning", func(t *testing.T) {
		r := deployFixture(t)
		dir := r.dir
		seedInventory(t, r.fakeCs, pruneInventory{Nodes: map[string]map[string][]objRef{
			"cp1": {
				"coredns": {kubeCM("old-coredns")}, // the stack whose sentinel cannot resolve
				"beta":    {kubeCM("old-beta")},    // a stack whose file carries no sentinel
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

		// The staging records the inputs: the next pass applies the CoreDNS
		// stack with the cluster's values, and prunes the object the stack's
		// record remembers but no file declares.
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
		// The record names a domain but no DNS address: one of the file's two
		// sentinels cannot be resolved.
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
