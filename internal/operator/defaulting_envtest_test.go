package operator

// Schema defaulting re-runs on every decode, so a new CRD default can invalidate an old stamp.

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

// Rendered at test time, not testdata: a drifted copy would guard a CRD the release does not ship.
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

	// The stamping webhook: the production handler, pointed at this test's listener.
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

	// Until the stripper is wired, the v2 signature covers the full spec, leaving nothing canonical.
	strip, err := fetchSchemaDefaultStripper(t.Context(), admin)
	if err != nil {
		t.Fatalf("wire schema-default stripper: %v", err)
	}
	setSchemaDefaultStripper(t, strip)

	// envtest authorizes with RBAC, so admission runs through real grants, not an override.
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
	stampAwaitRBAC(t, admin, ns)

	spec := func() map[string]any {
		return map[string]any{"chart": "app", "repo": "https://charts.example.com", "valuesContent": "replicas: 1"}
	}

	// The webhook plugin's informer sync is async, so canary until one create is stamped.
	stampWaitWebhookStamping(t, alice, gvr, ns, spec)

	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "app", spec()), signer,
		"alice", "alice-uid", "create under the bundled schema")
	for _, name := range []string{"persisted-migrated", "persisted-unmigrated"} {
		stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, name, spec()), signer,
			"alice", "alice-uid", "create before the later default")
	}
	// A stamp actually written in the legacy-only rollout phase must also survive the default.
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

	// The signer owner refreshes its schema source on every tick; the test refreshes it here.
	strip, err = fetchSchemaDefaultStripper(t.Context(), admin)
	if err != nil {
		t.Fatalf("refresh schema-default stripper: %v", err)
	}
	setSchemaDefaultStripper(t, strip)

	// Reading re-applies defaulting, so the served spec gains "priority" the stamp never covered.
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

	// Ordinary creation under the new schema keeps verifying.
	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "second", spec()), signer,
		"alice", "alice-uid", "create under the schema with the new default")
	stampVerifyStored(t, stampCreateChart(t, alice, gvr, ns, "third", spec()), signer,
		"alice", "alice-uid", "second create under the new schema")
	// The v2 field set keeps the bound priority when a second default invalidates the legacy MAC.
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

	// Deregister mid-test; the t.Cleanup delete is teardown only. Refusals below are the backstop.
	if err := admin.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(t.Context(), webhookConfigName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deregister stamping webhook: %v", err)
	}
	stampWaitWebhookGone(t, dyn, gvr, ns, spec)

	// Tampered spec: the stamp no longer matches the desired state.
	if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), "fourth", types.MergePatchType,
		[]byte(`{"spec":{"chart":"other"}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("tamper spec patch: %v", err)
	}
	stampRefuseTampered(t, dyn, gvr, ns, "fourth", signer, "tampered spec")

	// A value equal to the new default cannot replace one admission already bound.
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

	// Tampered identity: the annotation changed under the signature.
	if _, err := dyn.Resource(gvr).Namespace(ns).Patch(t.Context(), "third", types.MergePatchType,
		fmt.Appendf(nil, `{"metadata":{"annotations":{%q:"mallory"}}}`, annRequester), metav1.PatchOptions{}); err != nil {
		t.Fatalf("tamper identity patch: %v", err)
	}
	stampRefuseTampered(t, dyn, gvr, ns, "third", signer, "tampered identity")
}

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

// Distinguishes a caught tamper from a dropped stamp: the refusal must name signature verification.
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

func stampApplySchemaDefault(t *testing.T, dyn dynamic.Interface, crdGVR schema.GroupVersionResource, crdName, field string, def any) {
	t.Helper()
	crd, err := dyn.Resource(crdGVR).Get(t.Context(), crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", crdName, err)
	}
	// NestedMap and NestedSlice deep-copy; the mutation must land on the object written back.
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

// The RBAC authorizer reads an informer cache, so a fresh grant is not visible immediately.
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

// A Forbidden create is retriable: the authorizer's cache may lag the fresh grant.
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

// A re-stamp signs a new binding, so a changed signature proves the webhook is gone.
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

// nil privileged namespaces matches every namespace, as production does for a tenant-only set.
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

// The CA is the bundle the webhook configuration registers.
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
