package operator

// defaulting_envtest_test.go — the schema-default migration guard, run against
// a real apiserver (envtest) with the bundled HelmChart CRD.
//
// A requester stamp binds the spec exactly as admission saw it (requesterBinding).
// The apiserver re-applies structural schema defaulting on every decode of a
// custom resource — including reads from etcd — so once the HelmChart CRD gains
// a new defaulted field, an object signed under the old schema is served with a
// spec the stamp never covered. This guard runs the whole pipeline for real:
// admission through a registered webhook, verification of the stored object,
// the schema change, the old object read under it, and refusal of tampered
// identity and spec.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/k8sutil"
	envtest "sigs.k8s.io/controller-runtime/pkg/envtest"
)

// writeHelmChartCRDForEnvtest renders the bundled HelmChart CRD into a temp
// file for envtest: the first document of addons.RenderHelmCRDs(), written at
// test time rather than kept as a testdata copy that could drift from the
// bundled source — the schema-default guard stamps objects under these
// defaults and then grows the schema in memory, so a drifted copy would guard
// a CRD the release does not ship. The bundled file also carries the
// HelmChartConfig CRD, which this guard does not exercise.
func writeHelmChartCRDForEnvtest(t *testing.T) string {
	t.Helper()
	docs := bytes.Split(addons.RenderHelmCRDs(), []byte("\n---\n"))
	if len(docs) == 0 {
		t.Fatal("the bundled helmchart-crds.yaml has no documents")
	}
	path := filepath.Join(t.TempDir(), "helmchart-crd.yaml")
	if err := os.WriteFile(path, docs[0], 0o644); err != nil {
		t.Fatalf("write the bundled HelmChart CRD for envtest: %v", err)
	}
	return path
}

// TestRequesterStampAcrossSchemaDefaultChange: a requester stamp written under
// one CRD schema must keep verifying after the schema gains a new default
// field, and it must keep refusing anything the webhook did not sign.
//
// Through a real apiserver:
//  1. create a HelmChart through the registered stamping webhook and verify the
//     stored object (the stamp binds the spec with the current defaults in it);
//  2. grow the CRD schema with a new default and read the old object back —
//     the served spec now carries the new default the stamp never covered, and
//     the versioned binding must still verify with the identity still bound;
//  3. ordinary creation under the new current defaults keeps verifying;
//  4. with the webhook deregistered, a direct spec or identity mutation — the
//     write the webhook never saw — is refused by verification.
func TestRequesterStampAcrossSchemaDefaultChange(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make test-envtest`")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{writeHelmChartCRDForEnvtest(t)},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest control plane: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	admin, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	gvr := schema.GroupVersionResource{Group: k8sutil.HelmChartGroup, Version: k8sutil.HelmChartVersion, Resource: "helmcharts"}
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	// The stamping webhook: the production admission handler over TLS,
	// registered the way the operator deploys it (buildWebhookConfig's rules,
	// fail closed), pointed at this test's listener instead of the Service.
	signer := testSigner()
	signers := newRequesterSignerHolder()
	signers.publish(signer)
	hookURL, caBundle := stampWebhookServe(t, signers)
	stampWebhookRegister(t, admin, hookURL, caBundle)

	const ns = "team-a"
	if _, err := admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	alice := stampImpersonated(t, cfg, requesterInfo{Username: "alice", UID: "alice-uid", Groups: []string{"team-a", "system:authenticated"}})

	// The v2 binding's schema source, wired exactly as the operator does when
	// it loads its signer (requestersig.go ensureRequesterSigner): the webhook
	// signs bindingV2, whose projection comes from the process-global
	// schemaDefaultStripper, so until it is wired the v2 signature covers the
	// full spec and the migration phase below has nothing canonical to verify.
	strip, err := fetchSchemaDefaultStripper(t.Context(), admin)
	if err != nil {
		t.Fatalf("wire schema-default stripper: %v", err)
	}
	setSchemaDefaultStripper(t, strip)

	// RBAC before the canary loop: envtest's apiserver authorizes with RBAC
	// (controller-runtime starts it --authorization-mode=RBAC) and the
	// impersonated writer alice holds no grants. A Role in team-a bound to
	// alice is the faithful grant, not an AlwaysAllow authorization override:
	// production tenants create HelmCharts under real namespace grants — grants
	// the reconciler then re-evaluates on every impersonated install
	// (requester.go impersonationConfig) — so admission must be exercised
	// through authorization, not around it.
	if _, err := admin.RbacV1().Roles(ns).Create(t.Context(), &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "helmcharts-writer", Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{k8sutil.HelmChartGroup},
			Resources: []string{"helmcharts"},
			Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
		}},
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create helmcharts-writer Role in %s: %v", ns, err)
	}
	if _, err := admin.RbacV1().RoleBindings(ns).Create(t.Context(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "helmcharts-writer", Namespace: ns},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "alice"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "helmcharts-writer"},
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("bind alice to the helmcharts-writer Role: %v", err)
	}
	// The RBAC authorizer reads an informer cache, so the grant is not
	// necessarily visible the instant Create returns; poll the authorizer
	// itself until alice's create is allowed before any canary runs.
	stampAwaitRBAC(t, admin, ns)

	spec := func() map[string]any {
		return map[string]any{"chart": "app", "repo": "https://charts.example.com", "valuesContent": "replicas: 1"}
	}

	// The mutating webhook plugin's informer syncs the configuration
	// asynchronously, so canary creates until one comes back stamped: an
	// unstamped create means the plugin is not calling the webhook yet.
	stampWaitWebhookStamping(t, alice, gvr, ns, spec)

	// Current defaults: a create through the real admission pipeline is
	// stamped, and the stored object — spec already carrying the schema's
	// defaults — verifies.
	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "app", spec()), signer,
		"alice", "alice-uid", "create under the bundled schema")
	for _, name := range []string{"persisted-migrated", "persisted-unmigrated"} {
		stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, name, spec()), signer,
			"alice", "alice-uid", "create before the later default")
	}
	// A stamp actually written in the legacy-only rollout phase also survives
	// an additive default under the new verifier.
	legacySigner := *signer
	legacySigner.bindingVersion = ""
	signers.publish(&legacySigner)
	legacy := stampCreateChart(t, alice, gvr, ns, "legacy", spec())
	if legacy.GetAnnotations()[annRequesterSigV2] != "" {
		t.Fatal("legacy rollout signer emitted v2")
	}
	signers.publish(signer)

	// Grow the schema: a new defaulted field lands after the object exists.
	stampApplySchemaDefault(t, dyn, crdGVR, "helmcharts.stratum.ciq.com", "priority", int64(3))

	// The signer owner re-points the schema source at the CRD as the cluster
	// now serves it on every owner tick (requestersig.go
	// maintainRequesterSigner), so the versioned binding picks a schema change
	// up without a restart. The test refreshes it here, standing in for that
	// tick: without it the v2 projection still carries the new default and the
	// migrated read below would refuse for the wrong reason.
	strip, err = fetchSchemaDefaultStripper(t.Context(), admin)
	if err != nil {
		t.Fatalf("refresh schema-default stripper: %v", err)
	}
	setSchemaDefaultStripper(t, strip)

	// The guard: read the old object under the later schema. Reading
	// re-applies structural defaulting, so the served spec gains "priority",
	// which the stamp never covered. The versioned binding must be invariant
	// to that: the stamp still verifies, and it still returns the identity
	// recorded under the old defaults.
	stored := stampAwaitServedDefault(t, dyn, gvr, ns, "app", "priority")
	info, authentic, reason := verifiedRequester(stored, signer)
	if !authentic {
		t.Errorf("the HelmChart created before the schema gained a default must still verify when read under the later schema; refused: %s", reason)
	} else if info.Username != "alice" || info.UID != "alice-uid" {
		t.Errorf("read after the schema change verified identity %+v, want the bound identity alice/alice-uid", info)
	}

	legacy = stampAwaitServedDefault(t, dyn, gvr, ns, "legacy", "priority")
	if _, ok, reason := verifiedRequester(legacy, signer); !ok {
		t.Fatalf("legacy stamp after additive default: %s", reason)
	}

	// A status write stores the defaulted spec without adding the new field to
	// its authenticated policy. Before changing that default again, the intended
	// requester must make a real spec change under the still-compatible schema.
	// Keep an unmigrated control to pin the fail-closed rollout boundary.
	for _, name := range []string{"persisted-migrated", "persisted-unmigrated"} {
		persisted := stampAwaitServedDefault(t, dyn, gvr, ns, name, "priority")
		policy := persisted.GetAnnotations()[annRequesterFieldsV2]
		if requesterPolicyFields(policy)["priority"] {
			t.Fatal("a later served default must not already be an admitted field")
		}
		persisted.Object["status"] = map[string]any{"phase": "Pending"}
		persisted, err = dyn.Resource(gvr).Namespace(ns).UpdateStatus(t.Context(), persisted, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("persist the later default through status on %s: %v", name, err)
		}
		if persisted.GetAnnotations()[annRequesterFieldsV2] != policy {
			t.Fatal("a status write must preserve the admitted-field policy")
		}
		stampVerifyStored(t, persisted, signer, "alice", "alice-uid", "status write under the later default")
		if name == "persisted-migrated" {
			persisted.Object["spec"].(map[string]any)["valuesContent"] = "replicas: 2"
			persisted, err = alice.Resource(gvr).Namespace(ns).Update(t.Context(), persisted, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("authenticate the spec-changing migration: %v", err)
			}
			if !requesterPolicyFields(persisted.GetAnnotations()[annRequesterFieldsV2])["priority"] {
				t.Fatal("authenticated migration must bind the materialized default")
			}
			stampVerifyStored(t, persisted, signer, "alice", "alice-uid", "authenticated migration before changing the default")
		}
	}

	// Ordinary current-default creation keeps verifying: a create under the
	// new schema is stamped with the new default already in its spec.
	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "second", spec()), signer,
		"alice", "alice-uid", "create under the schema with the new default")
	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "third", spec()), signer,
		"alice", "alice-uid", "second create under the new schema")
	// The object created under priority=3 must bind that actual value even when
	// priority's schema default changes and a second new default invalidates its
	// legacy full-spec MAC. Its authenticated v2 field set preserves coverage.
	stampApplySchemaDefault(t, dyn, crdGVR, "helmcharts.stratum.ciq.com", "priority", int64(4))
	stampApplySchemaDefault(t, dyn, crdGVR, "helmcharts.stratum.ciq.com", "weight", int64(1))
	strip, err = fetchSchemaDefaultStripper(t.Context(), admin)
	if err != nil {
		t.Fatal(err)
	}
	setSchemaDefaultStripper(t, strip)
	evolved := stampAwaitServedDefault(t, dyn, gvr, ns, "second", "weight")
	stampVerifyStored(t, evolved, signer, "alice", "alice-uid", "sequential schema changes")
	if priority, _, _ := unstructured.NestedInt64(evolved.Object, "spec", "priority"); priority != 3 {
		t.Fatalf("persisted admitted priority changed to %d, want 3", priority)
	}
	migrated := stampAwaitServedDefault(t, dyn, gvr, ns, "persisted-migrated", "weight")
	stampVerifyStored(t, migrated, signer, "alice", "alice-uid", "migrated materialized default after changing the schema default")
	if priority, _, _ := unstructured.NestedInt64(migrated.Object, "spec", "priority"); priority != 3 {
		t.Fatalf("migrated persisted priority changed to %d, want 3", priority)
	}
	unmigrated := stampAwaitServedDefault(t, dyn, gvr, ns, "persisted-unmigrated", "weight")
	if priority, _, _ := unstructured.NestedInt64(unmigrated.Object, "spec", "priority"); priority != 3 {
		t.Fatalf("status must have materialized the earlier default, got %d, want 3", priority)
	}
	if _, ok, reason := verifiedRequester(unmigrated, signer); ok {
		t.Fatal("an unmigrated materialized default must fail closed after changing the schema default")
	} else if !strings.Contains(reason, "failed signature verification") {
		t.Fatalf("unmigrated default must fail signature verification, got: %s", reason)
	}
	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "fourth", spec()), signer,
		"alice", "alice-uid", "create after sequential schema changes")

	// Post-webhook mutation attempts: delete the MutatingWebhookConfiguration
	// HERE — the delete in stampWebhookRegister's t.Cleanup is teardown only,
	// it never fires mid-test — so the following writes reach etcd exactly as
	// the reconciler would read them, mutations the webhook never saw.
	// stampWaitWebhookGone then probes that deregistration is observable
	// before any tamper runs. The tamper refusals are the reconciler's
	// backstop (verifiedRequester), not admission.
	if err := admin.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(t.Context(), webhookConfigName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deregister stamping webhook: %v", err)
	}
	stampWaitWebhookGone(t, dyn, gvr, ns, spec)

	// Tampered spec: the stored stamp no longer matches the desired state.
	if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), "fourth", types.MergePatchType,
		[]byte(`{"spec":{"chart":"other"}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("tamper spec patch: %v", err)
	}
	stampRefuseTampered(t, dyn, gvr, ns, "fourth", signer, "tampered spec")

	// A value equal to the new current default cannot replace a value that
	// admission already bound. The legacy MAC is stale because weight was added.
	if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), "second", types.MergePatchType,
		[]byte(`{"spec":{"priority":4}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	stampRefuseTampered(t, dyn, gvr, ns, "second", signer, "tampered admitted default")
	if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), "persisted-migrated", types.MergePatchType,
		[]byte(`{"spec":{"priority":4}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	stampRefuseTampered(t, dyn, gvr, ns, "persisted-migrated", signer, "tampered materialized default after authenticated migration")

	// Tampered identity: the requester annotation changed under the signature.
	if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), "third", types.MergePatchType,
		fmt.Appendf(nil, `{"metadata":{"annotations":{%q:"mallory"}}}`, annRequester), metav1.PatchOptions{}); err != nil {
		t.Fatalf("tamper identity patch: %v", err)
	}
	stampRefuseTampered(t, dyn, gvr, ns, "third", signer, "tampered identity")
}

// stampCreateChart creates one HelmChart through dyn (the impersonated writer
// in the stamped cases) and returns the stored object.
func stampCreateChart(t *testing.T, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string, spec map[string]any) *unstructured.Unstructured {
	t.Helper()
	hc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       "HelmChart",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       spec,
	}}
	created, err := dyn.Resource(gvr).Namespace(ns).Create(t.Context(), hc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create HelmChart %s/%s: %v", ns, name, err)
	}
	return created
}

// stampVerifyStored asserts a stored object's stamp verifies and carries the
// migration stamp set (both signatures plus the key id), returning the identity
// verification attributed.
func stampVerifyStored(t *testing.T, obj *unstructured.Unstructured, signer *requesterSigner, wantUser, wantUID, what string) {
	t.Helper()
	info, authentic, reason := verifiedRequester(obj, signer)
	if !authentic {
		t.Errorf("%s: the stored object must verify; refused: %s", what, reason)
		return
	}
	if info.Username != wantUser || info.UID != wantUID {
		t.Errorf("%s: verified identity %+v, want %s/%s", what, info, wantUser, wantUID)
	}
	ann := obj.GetAnnotations()
	if ann[annRequesterSigV2] == "" || ann[annRequesterKeyID] == "" {
		t.Errorf("%s: the stamp must carry the versioned signature and key id; annotations = %v", what, ann)
	}
}

// stampRefuseTampered asserts verification refuses a stored object with a
// signature-verification failure — the stamp is still present but no longer
// matches, which is the tamper being caught (not a dropped stamp).
func stampRefuseTampered(t *testing.T, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string, signer *requesterSigner, what string) {
	t.Helper()
	stored, err := dyn.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get tampered %s/%s: %v", ns, name, err)
	}
	_, authentic, reason := verifiedRequester(stored, signer)
	if authentic {
		t.Errorf("%s: the tampered object verified; verification must refuse it", what)
		return
	}
	if !strings.Contains(reason, "failed signature verification") {
		t.Errorf("%s: refusal %q does not name a signature-verification failure; the stamp may have been dropped rather than caught", what, reason)
	}
}

// stampApplySchemaDefault updates the CRD so spec gains `field` with a default,
// the schema change the old object's stamp must survive.
func stampApplySchemaDefault(t *testing.T, dyn dynamic.Interface, crdGVR schema.GroupVersionResource, crdName, field string, def any) {
	t.Helper()
	crd, err := dyn.Resource(crdGVR).Get(t.Context(), crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", crdName, err)
	}
	// Navigate with NestedFieldNoCopy: NestedMap and NestedSlice deep-copy, and
	// the mutation must land on the CRD object that is written back.
	val, found, err := unstructured.NestedFieldNoCopy(crd.Object, "spec", "versions")
	if err != nil || !found {
		t.Fatalf("CRD %s has no versions: found=%v err=%v", crdName, found, err)
	}
	versions, ok := val.([]any)
	if !ok || len(versions) == 0 {
		t.Fatalf("CRD %s versions is %T, want a non-empty list", crdName, val)
	}
	v0, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("CRD %s version 0 is %T, not an object", crdName, versions[0])
	}
	pv, found, err := unstructured.NestedFieldNoCopy(v0, "schema", "openAPIV3Schema", "properties", "spec", "properties")
	if err != nil || !found {
		t.Fatalf("CRD %s has no spec properties to grow: found=%v err=%v", crdName, found, err)
	}
	props, ok := pv.(map[string]any)
	if !ok {
		t.Fatalf("CRD %s spec properties is %T, not an object", crdName, pv)
	}
	props[field] = map[string]any{"type": "integer", "default": def}
	if _, err := dyn.Resource(crdGVR).Update(t.Context(), crd, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("apply schema default %s to %s: %v", field, crdName, err)
	}
}

// stampAwaitServedDefault re-reads the old object until the apiserver serves it
// with the new default (read-path structural defaulting of the updated schema)
// — the precondition the guard exists for.
func stampAwaitServedDefault(t *testing.T, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name, field string) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		stored, err := dyn.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s/%s: %v", ns, name, err)
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(stored.Object, "spec", field); found {
			return stored
		}
		if time.Now().After(deadline) {
			t.Fatalf("the apiserver never served %s/%s with the new default %q within 30s; the schema change did not reach the read path", ns, name, field)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stampAwaitRBAC polls a SubjectAccessReview until the RBAC authorizer reports
// alice's helmcharts create in ns as allowed: the authorizer reads an informer
// cache, so the RoleBinding's grant is not necessarily visible the instant its
// Create returns, and a canary create racing that lag would fail the guard for
// the wrong reason.
func stampAwaitRBAC(t *testing.T, admin kubernetes.Interface, ns string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		review, err := admin.AuthorizationV1().SubjectAccessReviews().Create(t.Context(),
			&authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: "alice",
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: ns,
					Verb:      "create",
					Group:     k8sutil.HelmChartGroup,
					Resource:  "helmcharts",
				},
			}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("subject access review for alice's helmcharts create in %s: %v", ns, err)
		}
		if review.Status.Allowed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the RBAC authorizer never allowed alice's helmcharts create in %s within 30s of creating the RoleBinding (last reason: %s)", ns, review.Status.Reason)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stampWaitWebhookStamping canary-creates until the mutating webhook plugin is
// actually calling the webhook: the first creates can race the plugin's
// informer sync, and an unstamped create would fail the guard for the wrong
// reason. Unstamped canaries are deleted; the stamped one stays. A Forbidden
// create is retriable like the unstamped one: the RBAC authorizer's informer
// cache may still not show a just-created grant, the same lag
// stampAwaitRBAC's SubjectAccessReview poll already rode out.
func stampWaitWebhookStamping(t *testing.T, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns string, spec func() map[string]any) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		name := "stamping-canary-" + strconv.Itoa(i)
		hc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvr.Group + "/" + gvr.Version,
			"kind":       "HelmChart",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec":       spec(),
		}}
		_, err := dyn.Resource(gvr).Namespace(ns).Create(t.Context(), hc, metav1.CreateOptions{})
		if apierrors.IsForbidden(err) {
			// The grant is not visible to the authorizer yet; retry.
			if time.Now().After(deadline) {
				t.Fatalf("alice still forbidden to create helmcharts in %s within 30s of the RoleBinding: %v", ns, err)
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("create canary HelmChart %s/%s: %v", ns, name, err)
		}
		stored, err := dyn.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get canary %s/%s: %v", ns, name, err)
		}
		if stored.GetAnnotations()[annRequester] != "" {
			return
		}
		if err := dyn.Resource(gvr).Namespace(ns).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete unstamped canary %s/%s: %v", ns, name, err)
		}
		if time.Now().After(deadline) {
			t.Fatal("admission never stamped a canary create within 30s; the MutatingWebhookConfiguration never took effect")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stampWaitWebhookGone waits until deregistering the webhook is observable: a
// spec-changing write must no longer be re-stamped. The probe is the stored
// signature — a re-stamp signs the new binding, so it differs; an unstamped
// write leaves it untouched.
func stampWaitWebhookGone(t *testing.T, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns string, spec func() map[string]any) {
	t.Helper()
	name := "deregister-canary"
	stampCreateChart(t, dyn, gvr, ns, name, spec())
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		before, err := dyn.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get deregister canary %s/%s: %v", ns, name, err)
		}
		sigBefore := before.GetAnnotations()[annRequesterSig]
		if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), name, types.MergePatchType,
			[]byte(`{"spec":{"chart":"probe-`+strconv.Itoa(i)+`"}}`), metav1.PatchOptions{}); err != nil {
			t.Fatalf("deregister probe patch: %v", err)
		}
		after, err := dyn.Resource(gvr).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get deregister canary after patch: %v", err)
		}
		if after.GetAnnotations()[annRequesterSig] == sigBefore {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("spec-changing writes were still re-stamped within 30s of deregistering the webhook")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stampWebhookServe serves the production admission handler over TLS on
// 127.0.0.1 and returns the registerable URL and CA bundle.
func stampWebhookServe(t *testing.T, signer signerSnapshot) (url string, caBundle []byte) {
	t.Helper()
	cert, caPEM := stampServingCert(t, "127.0.0.1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the stamping webhook: %v", err)
	}
	srv := &http.Server{Handler: admissionHandler(signer), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
	}()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return "https://" + listener.Addr().String() + webhookPath, caPEM
}

// stampWebhookRegister registers the production webhook configuration against
// the real apiserver, pointed at the test's serving URL. nil privileged
// namespaces means the webhook matches every namespace, as production does for
// a tenant-only exemption set.
func stampWebhookRegister(t *testing.T, cs kubernetes.Interface, url string, caBundle []byte) {
	t.Helper()
	cfg := buildWebhookConfig(caBundle, nil)
	cfg.Webhooks[0].ClientConfig = admissionregistrationv1.WebhookClientConfig{URL: &url, CABundle: caBundle}
	if _, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(t.Context(), cfg, metav1.CreateOptions{}); err != nil {
		t.Fatalf("register mutating webhook: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.Background(), webhookConfigName, metav1.DeleteOptions{})
	})
}

// stampImpersonated returns a dynamic client writing as exactly one identity,
// so the webhook stamps a deterministic requester.
func stampImpersonated(t *testing.T, cfg *rest.Config, info requesterInfo) dynamic.Interface {
	t.Helper()
	icfg := rest.CopyConfig(cfg)
	icfg.Impersonate = rest.ImpersonationConfig{UserName: info.Username, UID: info.UID, Groups: info.Groups}
	dyn, err := dynamic.NewForConfig(icfg)
	if err != nil {
		t.Fatalf("impersonated dynamic client: %v", err)
	}
	return dyn
}

// stampServingCert issues a self-signed CA and a serving leaf for the webhook
// URL's host; the CA is the bundle the webhook configuration registers.
func stampServingCert(t *testing.T, host string) (tls.Certificate, []byte) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host + " stamping webhook ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
	}
	caDER, err := x509.CreateCertificate(rand.Reader, root, root, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		IPAddresses:  []net.IP{net.ParseIP(host)},
		DNSNames:     []string{"localhost"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:    ca.NotBefore,
		NotAfter:     ca.NotAfter,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}
