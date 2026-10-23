package operator

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// Previously installed cross-namespace releases must remain owned even after
// policy is tightened. A failed cleanup keeps the finalizer for retry.
func TestDeletionPathUsesRecordedIdentityNotSpec(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":              "app",
			"namespace":         "team-a",
			"finalizers":        []interface{}{helmFinalizer},
			"deletionTimestamp": "2026-01-01T00:00:00Z",
		},
		"spec": map[string]interface{}{
			"chart":           "app",
			"targetNamespace": "team-a",
			"releaseName":     "app",
		},
		"status": map[string]interface{}{
			"releaseNamespace": "kube-system",
			"releaseName":      "coredns",
		},
	}}

	signer := testSigner()
	owner := requesterInfo{Username: "alice"}
	hc.SetAnnotations(map[string]string{annRequester: "alice", annRequesterSig: signer.sign(requesterBinding(hc), owner)})

	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"},
		hc.DeepCopy(),
	)

	var releaseLookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			_, _ = io.WriteString(w, `{"gitVersion":"v1.35.0"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			releaseLookups.Add(1)
			if r.URL.Path != "/api/v1/namespaces/kube-system/secrets" || !strings.Contains(r.URL.Query().Get("labelSelector"), "name=coredns") {
				t.Errorf("uninstall targeted spec instead of recorded release: %s", r.URL)
			}
		} else {
			t.Errorf("unexpected Helm request: %s", r.URL)
		}
		http.Error(w, "cleanup unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\ncurrent-context: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reconcileHelmChart(context.Background(), dyn, kubeconfig, hc, "", nil, helmPolicy{signer: signer})
	if releaseLookups.Load() == 0 {
		t.Fatal("cleanup never looked up the recorded release")
	}

	got, err := dyn.Resource(helmChartGVR).Namespace("team-a").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinalizer(got, helmFinalizer) {
		t.Fatal("abandoned recorded release after policy change")
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase != hcPhaseUninstalling {
		t.Fatalf("phase = %q; want cleanup attempted", phase)
	}
	ns, _, _ := unstructured.NestedString(got.Object, "status", "releaseNamespace")
	name, _, _ := unstructured.NestedString(got.Object, "status", "releaseName")
	if ns != "kube-system" || name != "coredns" {
		t.Fatalf("ownership changed: %s/%s", ns, name)
	}
}

// Same-namespace cleanup also keeps ownership when Helm cannot connect.
func TestDeletionPathStillUninstallsItsOwnRelease(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":              "app",
			"namespace":         "team-a",
			"finalizers":        []interface{}{helmFinalizer},
			"deletionTimestamp": "2026-01-01T00:00:00Z",
		},
		"spec":   map[string]interface{}{"chart": "app"},
		"status": map[string]interface{}{"releaseNamespace": "team-a", "releaseName": "app"},
	}}

	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"},
		hc.DeepCopy(),
	)

	reconcileHelmChart(context.Background(), dyn, t.TempDir()+"/missing-kubeconfig", hc, "", nil,
		helmPolicy{privilegedNamespaces: []string{"kube-system"}})

	got, err := dyn.Resource(helmChartGVR).Namespace("team-a").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get helmchart: %v", err)
	}
	// The uninstall itself cannot succeed with no cluster, so the controller keeps
	// the finalizer and retries — which is exactly the contract: a release it owns
	// is never abandoned just because helm failed.
	if fin := got.GetFinalizers(); len(fin) == 0 {
		t.Error("finalizer dropped without a successful uninstall — a release this HelmChart " +
			"owns would be orphaned")
	}
}

// TestFailedStatusDoesNotRepointTheUninstallTarget covers the other half of the
// identity fix. The recorded release is what the deletion path uninstalls, and
// a Failed write carries the CURRENT spec — so if every status write stamped the
// identity, a tenant could repoint the spec, let the containment check write
// Failed, and thereby move the uninstall target without ever deploying.
func TestFailedStatusDoesNotRepointTheUninstallTarget(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata":   map[string]interface{}{"name": "app", "namespace": "team-a"},
		"spec":       map[string]interface{}{"chart": "app"},
		"status": map[string]interface{}{
			"phase": hcPhaseDeployed, "releaseNamespace": "team-a", "releaseName": "app",
		},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"},
		hc.DeepCopy(),
	)

	// The refused spec: what the tenant repointed to after the release existed.
	refused := helmSpec{releaseName: "coredns", targetNamespace: "kube-system"}
	setHelmStatus(context.Background(), dyn, "team-a", "app", hcPhaseFailed, refused, 0, "",
		"refusing to install: targetNamespace outside this HelmChart's namespace")

	got, err := dyn.Resource(helmChartGVR).Namespace("team-a").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get helmchart: %v", err)
	}
	gotNS, _, _ := unstructured.NestedString(got.Object, "status", "releaseNamespace")
	gotName, _, _ := unstructured.NestedString(got.Object, "status", "releaseName")
	if gotNS != "team-a" || gotName != "app" {
		t.Errorf("recorded identity moved to %s/%s on a Failed write; it must stay at the "+
			"deployed team-a/app or a refused spec can choose what gets uninstalled", gotNS, gotName)
	}
}

// TestConvergedRefreshesStaleInsecureTLSStatus covers the effective
// insecure-TLS status: it depends on the operator's --allow-insecure-helm-repos
// flag, which is deliberately not part of specHash, so a settled release takes
// the converged early-return and would otherwise keep reporting whatever it
// reported when it last did real work.
func TestConvergedRefreshesStaleInsecureTLSStatus(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			hc := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
				"kind":       "HelmChart",
				"metadata": map[string]interface{}{
					"name": "app", "namespace": "team-a",
					// Already adopted: without this, reconcile adds the finalizer and
					// returns before it ever reaches the converged hot path.
					"finalizers": []interface{}{helmFinalizer},
				},
				"spec": map[string]interface{}{
					"chart": "app", "repo": "https://example.invalid", "insecureSkipTLSVerify": true,
				},
			}}

			// Reproduce the hash the reconciler will compute, so it takes the hot path.
			spec := parseHelmSpec(hc)
			vals, err := mergedValues(spec, "")
			if err != nil {
				t.Fatal(err)
			}
			_ = unstructured.SetNestedMap(hc.Object, map[string]interface{}{
				"phase":    hcPhaseDeployed,
				"specHash": specHash(spec, vals),
				"revision": int64(1),
				// Stale: recorded while the operator still allowed insecure repos.
				"insecureTLSVerify": true,
				"releaseNamespace":  "team-a",
				"releaseName":       "original-app",
			}, "status")

			if missing {
				unstructured.RemoveNestedField(hc.Object, "status", "insecureTLSVerify")
			}
			// wantStatus is the EXPECTED status AFTER the refresh: the starting status
			// with insecureTLSVerify flipped to false. unstructured.NestedMap returns a
			// DEEP COPY, so mutating it here does NOT touch hc — the fake client below is
			// seeded from hc.DeepCopy(), which in the missing case has no
			// insecureTLSVerify field at all (that absence is what exercises the !present
			// guard; the missing=true case fails without it).
			wantStatus, _, _ := unstructured.NestedMap(hc.Object, "status")
			wantStatus["insecureTLSVerify"] = false

			relSecret := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]interface{}{"name": "sh.helm.release.v1.app.v1", "namespace": "team-a"},
			}}

			dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(),
				map[schema.GroupVersionResource]string{
					helmChartGVR: "HelmChartList",
					secretGVR:    "SecretList",
				},
				hc.DeepCopy(), relSecret,
			)

			// Operator has since restarted WITHOUT --allow-insecure-helm-repos.
			reconcileHelmChart(context.Background(), dyn, t.TempDir()+"/missing-kubeconfig", hc, "", nil,
				helmPolicy{privilegedNamespaces: []string{"kube-system"}, allowInsecureRepos: false})

			got, err := dyn.Resource(helmChartGVR).Namespace("team-a").Get(context.Background(), "app", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get helmchart: %v", err)
			}
			insecure, present, _ := unstructured.NestedBool(got.Object, "status", "insecureTLSVerify")
			if insecure || !present {
				t.Error("status.insecureTLSVerify is still true after the operator dropped " +
					"--allow-insecure-helm-repos — a converged release reports a security posture " +
					"it no longer has")
			}
			after, _, _ := unstructured.NestedMap(got.Object, "status")
			if !reflect.DeepEqual(wantStatus, after) {
				t.Fatalf("refresh changed more than insecureTLSVerify: want=%v after=%v", wantStatus, after)
			}
		})
	}

}

func TestPendingInstallKeepsOwnershipAfterFailure(t *testing.T) {
	ctx := context.Background()
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart",
		"metadata": map[string]interface{}{"name": "app", "namespace": "team-a"},
	}}
	dyn := dynfake.NewSimpleDynamicClient(runtime.NewScheme(), hc)
	spec := helmSpec{releaseName: "app", targetNamespace: "team-a"}
	if err := writeHelmStatus(ctx, dyn, "team-a", "app", hcPhasePending, spec, "hash", 0, "", "reconciling", true); err != nil {
		t.Fatal(err)
	}
	setHelmStatus(ctx, dyn, "team-a", "app", hcPhaseFailed, spec, 0, "", "install failed after persisting release")
	got, err := dyn.Resource(helmChartGVR).Namespace("team-a").Get(ctx, "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := unstructured.NestedMap(got.Object, "status")
	ns, name, ok := uninstallTarget(status)
	if !ok || ns != "team-a" || name != "app" {
		t.Fatalf("failed install lost cleanup target: %v", status)
	}
	// An old CR without CEL immutability cannot repoint recorded ownership.
	spec.releaseName = "other"
	if err := writeHelmStatus(ctx, dyn, "team-a", "app", hcPhasePending, spec, "hash", 0, "", "reconciling", true); err == nil {
		t.Fatal("accepted changed release identity")
	}
}

// If the ownership write fails, reconciliation must stop before even looking
// up a Helm release. The missing kubeconfig makes an accidental lookup produce
// a second (Failed) status write, which this test detects.
func TestReconcileStopsWhenPendingOwnershipCannotBeSaved(t *testing.T) {
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart",
		// kube-system (a privileged namespace) so the requester backstop is
		// bypassed and this test exercises only the Pending-ownership-write path.
		"metadata": map[string]interface{}{"name": "app", "namespace": "kube-system", "finalizers": []interface{}{helmFinalizer}},
		"spec":     map[string]interface{}{"chartContent": base64.StdEncoding.EncodeToString(makeChartTGZ(t, "app", "1.0.0"))},
	}}
	dyn := dynfake.NewSimpleDynamicClient(runtime.NewScheme(), hc.DeepCopy())
	var phases []string
	dyn.PrependReactor("update", "helmcharts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		object := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
		phases = append(phases, phase)
		return true, nil, fmt.Errorf("status storage unavailable")
	})
	reconcileHelmChart(context.Background(), dyn, t.TempDir()+"/missing-kubeconfig", hc, "", nil,
		helmPolicy{privilegedNamespaces: []string{"kube-system"}})
	if !reflect.DeepEqual(phases, []string{hcPhasePending}) {
		t.Fatalf("continued after ownership failure: status writes %v", phases)
	}
}
