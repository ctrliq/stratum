package operator

// Regression guards for the operator's Helm flows. Each test drives
// reconcileHelmChart end to end against an in-memory Helm backend and pins what
// stratum makes observable: the HelmChart's status and finalizer, and the
// release revisions left in Helm's storage. The tests must pass unchanged
// across a Helm major-version migration, so this file imports no helm.sh
// package; every Helm-typed construct lives in helmflows_helpers_test.go.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"helm.sh/helm/v3/pkg/action"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
)

// The flow tests use a HelmChart in a privileged namespace, so it installs as
// the operator and needs no requester stamp, and its release takes the
// HelmChart's own namespace and name.
const (
	flowNS      = "kube-system"
	flowName    = "app"
	flowRelease = "app"
)

var flowPolicy = helmPolicy{privilegedNamespaces: []string{flowNS}}

// flowChart is spec.chartContent for an in-memory chart named app.
func flowChart(t *testing.T, version string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(makeChartTGZ(t, flowRelease, version))
}

// flowHCUID numbers the flow HelmCharts, so each carries a metadata.uid no
// other chart shares. The operator keys its process-wide unwrittenIncomplete
// and cleanUninstalls records by namespace/name/uid (helmchart.go), and without
// a UID every flow test's chart shares one key into them.
var flowHCUID atomic.Int64

// newFlowHC is an adopted HelmChart (it already carries the finalizer, so a
// reconcile goes straight to Helm work) in a fake dynamic client. Its metadata.uid
// is unique per call, and the operator's record of unwritten "uninstall
// incomplete:" statuses is emptied now and when the test ends, so an entry one
// test leaves behind can reach no other test.
func newFlowHC(t *testing.T, spec map[string]interface{}) dynamic.Interface {
	t.Helper()
	wantNoUnwrittenIncomplete(t, "at the start of a flow test")
	forgetUnwrittenIncomplete(t)
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":       flowName,
			"namespace":  flowNS,
			"uid":        fmt.Sprintf("flow-hc-%d", flowHCUID.Add(1)),
			"finalizers": []interface{}{helmFinalizer},
		},
		"spec": spec,
	}}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList", secretGVR: "SecretList"},
		hc,
	)
}

// wantNoUnwrittenIncomplete fails unless the operator's process-wide record of
// unwritten "uninstall incomplete:" statuses is empty. newFlowHC calls it
// before anything in a new test can add an entry, so a failure names an
// earlier test as the leaker.
func wantNoUnwrittenIncomplete(t *testing.T, when string) {
	t.Helper()
	unwrittenIncompleteMu.Lock()
	defer unwrittenIncompleteMu.Unlock()
	if len(unwrittenIncomplete) == 0 {
		return
	}
	keys := slices.Sorted(maps.Keys(unwrittenIncomplete))
	t.Fatalf("%s: unwrittenIncomplete holds entries for %v, want none: an earlier test left them behind, and the shared fixture must clear the record when that test ends", when, keys)
}

func getFlowHC(t *testing.T, dyn dynamic.Interface) *unstructured.Unstructured {
	t.Helper()
	hc, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Get(context.Background(), flowName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get helmchart: %v", err)
	}
	return hc
}

// reconcileFlow runs one reconcile of the stored HelmChart, as one tick of the
// operator's loop would. The kubeconfig does not exist, so any Helm work that
// bypasses newActionConfigFn fails instead of reaching a real cluster.
func reconcileFlow(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")
	reconcileHelmChart(context.Background(), dyn, kubeconfig, getFlowHC(t, dyn), "", nil, flowPolicy)
}

// flowStatus is the subset of HelmChart .status the flows pin.
type flowStatus struct {
	phase, message, chartVersion string
	revision                     int64
}

func readFlowStatus(t *testing.T, dyn dynamic.Interface) flowStatus {
	t.Helper()
	st, _, _ := unstructured.NestedMap(getFlowHC(t, dyn).Object, "status")
	return flowStatus{
		phase:        str(st["phase"], ""),
		message:      str(st["message"], ""),
		chartVersion: str(st["chartVersion"], ""),
		revision:     asInt(st["revision"]),
	}
}

func wantDeployed(t *testing.T, dyn dynamic.Interface, revision int64, chartVersion string) {
	t.Helper()
	got := readFlowStatus(t, dyn)
	want := flowStatus{phase: hcPhaseDeployed, message: "release deployed", chartVersion: chartVersion, revision: revision}
	if got != want {
		t.Fatalf("helmchart status = %+v, want %+v", got, want)
	}
}

func wantLatestRelease(t *testing.T, h *fakeHelm, want helmRecord) {
	t.Helper()
	hist := h.history(t, flowNS, flowRelease)
	if len(hist) == 0 {
		t.Fatalf("release %s/%s: no revisions stored, want latest %+v", flowNS, flowRelease, want)
	}
	if got := hist[len(hist)-1]; got != want {
		t.Fatalf("release %s/%s latest revision = %+v, want %+v (history %+v)", flowNS, flowRelease, got, want, hist)
	}
}

func TestHelmFlowFirstInstall(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 1, "1.0.0")
	wantLatestRelease(t, h, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
}

func TestHelmFlowUpgradeOnSpecChange(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	reconcileFlow(t, dyn)
	wantDeployed(t, dyn, 1, "1.0.0")

	// A new chart version changes the spec hash, so the next tick upgrades.
	hc := getFlowHC(t, dyn)
	if err := unstructured.SetNestedField(hc.Object, flowChart(t, "1.1.0"), "spec", "chartContent"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update helmchart spec: %v", err)
	}
	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 2, "1.1.0")
	wantLatestRelease(t, h, helmRecord{revision: 2, status: "deployed", chartVersion: "1.1.0"})
}

// A Kubernetes create that fails during install surfaces as phase Failed with
// an "install: " message carrying the cause, whether the failing create is the
// chart's own objects or the target namespace (spec.createNamespace).
func TestHelmFlowInstallFailure(t *testing.T) {
	cause := errors.New("injected create failure for the Helm flow test")
	for _, tc := range []struct {
		name string
		opts fakeHelmOptions
		spec map[string]interface{}
	}{
		{
			name: "chart objects",
			opts: fakeHelmOptions{createErr: cause, renderedObject: true},
			spec: map[string]interface{}{},
		},
		{
			name: "target namespace",
			opts: fakeHelmOptions{createErr: cause},
			spec: map[string]interface{}{"targetNamespace": "stratum-addons", "createNamespace": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeHelm(t, tc.opts)
			tc.spec["chartContent"] = flowChart(t, "1.0.0")
			dyn := newFlowHC(t, tc.spec)

			reconcileFlow(t, dyn)

			got := readFlowStatus(t, dyn)
			if got.phase != hcPhaseFailed {
				t.Errorf("phase = %q, want %q (status %+v)", got.phase, hcPhaseFailed, got)
			}
			if !strings.HasPrefix(got.message, "install: ") {
				t.Errorf("message = %q, want prefix %q", got.message, "install: ")
			}
			if !strings.Contains(got.message, cause.Error()) {
				t.Errorf("message = %q, want it to contain the injected cause %q", got.message, cause.Error())
			}
		})
	}
}

// Deleting the HelmChart uninstalls the release it recorded and only then
// drops the finalizer.
func TestHelmFlowDeletionUninstallsAndRemovesFinalizer(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	hc := getFlowHC(t, dyn)
	deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	hc.SetDeletionTimestamp(&deleted)
	if err := unstructured.SetNestedMap(hc.Object, map[string]interface{}{
		"phase": hcPhaseDeployed, "releaseNamespace": flowNS, "releaseName": flowRelease,
	}, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark helmchart deleted: %v", err)
	}

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Errorf("release %s/%s still stored after deletion: %+v", flowNS, flowRelease, hist)
	}
	if hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Error("finalizer kept after a successful uninstall")
	}
}

// A deleting HelmChart whose release is gone keeps its finalizer when its
// status shows an uninstall that never recorded its result, as after an
// operator restart between Helm's purge and the status write: Helm kept no
// record of what it left.
func TestHelmFlowDeletionHoldsAfterUnrecordedUninstall(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	useFakeHelm(t, fakeHelmOptions{}) // no release: Helm purged it
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeletedWithStatus(t, dyn, map[string]interface{}{
		"phase": hcPhaseUninstalling, "message": uninstallingMessage, "releaseNamespace": flowNS, "releaseName": flowRelease,
	})

	reconcileFlow(t, dyn)

	st := readFlowStatus(t, dyn)
	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("finalizer removed after an unrecorded uninstall; status = %+v", st)
	}
	if st.phase != hcPhaseFailed || !strings.HasPrefix(st.message, uninstallIncompleteDelete) {
		t.Errorf("status = %+v, want Failed with %q", st, uninstallIncompleteDelete)
	}
}

// A clean uninstall whose finalizer removal fails is not taken for an
// unrecorded one: the next reconcile removes the finalizer, even when the
// status write recording the clean result failed too.
func TestHelmFlowDeletionRetriesFinalizerRemovalAfterCleanUninstall(t *testing.T) {
	for _, failStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("status write fails: %v", failStatus), func(t *testing.T) {
			forgetUnwrittenIncomplete(t)
			t.Cleanup(cleanUninstalls.Clear)
			h := useFakeHelm(t, fakeHelmOptions{})
			h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
			dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
			markFlowHCDeletedWithStatus(t, dyn, map[string]interface{}{
				"phase": hcPhaseDeployed, "releaseNamespace": flowNS, "releaseName": flowRelease,
			})
			failedFinalizer, failedStatus := false, false
			dyn.(*dynfake.FakeDynamicClient).PrependReactor("update", helmChartGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
				unavailable := apierrors.NewServiceUnavailable("etcd leader change")
				if action.GetSubresource() == "" && !failedFinalizer {
					failedFinalizer = true
					return true, nil, unavailable
				}
				obj := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured)
				if msg, _, _ := unstructured.NestedString(obj.Object, "status", "message"); failStatus && strings.HasPrefix(msg, "release uninstalled") {
					failedStatus = true
					return true, nil, unavailable
				}
				return false, nil, nil
			})

			reconcileFlow(t, dyn)
			if !failedFinalizer || failedStatus != failStatus || !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
				t.Fatalf("the writes did not fail as arranged; status = %+v", readFlowStatus(t, dyn))
			}
			reconcileFlow(t, dyn)

			if hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
				t.Fatalf("finalizer kept after a clean uninstall; status = %+v", readFlowStatus(t, dyn))
			}
		})
	}
}

// A failed action config runs no uninstall, so it leaves no marker: the next
// reconcile, finding the release already gone, removes the finalizer.
func TestHelmFlowDeletionAfterFailedActionConfig(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	useFakeHelm(t, fakeHelmOptions{}) // no release: removed by hand
	failed := false
	prev := newActionConfigFn
	newActionConfigFn = func(ctx context.Context, kubeconfig, namespace string, imp *rest.ImpersonationConfig) (*action.Configuration, error) {
		if !failed {
			failed = true
			return nil, errors.New("apiserver unreachable")
		}
		return prev(ctx, kubeconfig, namespace, imp)
	}
	t.Cleanup(func() { newActionConfigFn = prev })
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeletedWithStatus(t, dyn, map[string]interface{}{
		"phase": hcPhaseDeployed, "releaseNamespace": flowNS, "releaseName": flowRelease,
	})

	reconcileFlow(t, dyn)
	if !failed || !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("the action config did not fail as arranged; status = %+v", readFlowStatus(t, dyn))
	}
	reconcileFlow(t, dyn)

	if hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("finalizer kept after an uninstall that never ran; status = %+v", readFlowStatus(t, dyn))
	}
}

// A refused spec.targetNamespace ends an incomplete-uninstall hold, as
// hardening.md says: containment runs before the hold, whatever the spec hash,
// and its refusal replaces the "uninstall incomplete:" message.
func TestRefusedTargetNamespaceEndsTheHold(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0"), "targetNamespace": "elsewhere"})
	hc := getFlowHC(t, dyn)
	if err := unstructured.SetNestedMap(hc.Object, map[string]interface{}{
		"phase": hcPhaseFailed, "message": uninstallIncompleteRecover + "ConfigMap/x", "releaseNamespace": flowNS, "releaseName": flowRelease,
	}, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The HelmChart's namespace is not privileged under this policy.
	reconcileHelmChart(context.Background(), dyn, filepath.Join(t.TempDir(), "missing-kubeconfig"), getFlowHC(t, dyn), "", nil, helmPolicy{})

	if st := readFlowStatus(t, dyn); st.phase != hcPhaseFailed || !strings.HasPrefix(st.message, "refusing to install: spec.targetNamespace") {
		t.Fatalf("status = %+v, want the targetNamespace refusal in place of the hold", st)
	}
}

// markFlowHCDeleted gives the stored HelmChart a deletion timestamp and the
// release identity the controller records on adoption, so the next reconcile
// takes the deletion path and uninstalls that release.
func markFlowHCDeleted(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	hc := getFlowHC(t, dyn)
	deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	hc.SetDeletionTimestamp(&deleted)
	if err := unstructured.SetNestedMap(hc.Object, map[string]interface{}{
		"phase": hcPhaseDeployed, "releaseNamespace": flowNS, "releaseName": flowRelease,
	}, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark helmchart deleted: %v", err)
	}
}

// markFlowHCDeletedWithStatus sets the flow HelmChart's deletion timestamp and
// status.
func markFlowHCDeletedWithStatus(t *testing.T, dyn dynamic.Interface, status map[string]interface{}) {
	t.Helper()
	hc := getFlowHC(t, dyn)
	deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	hc.SetDeletionTimestamp(&deleted)
	if err := unstructured.SetNestedMap(hc.Object, status, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark helmchart deleted: %v", err)
	}
}

// A release left pending-upgrade is rolled back to the previous revision, and
// the next tick upgrades it cleanly.
func TestHelmFlowRecoverPendingUpgradeRollsBack(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	want := flowStatus{phase: hcPhasePending, message: "recovering pending release", revision: 2}
	if got := readFlowStatus(t, dyn); got != want {
		t.Fatalf("helmchart status after recovery = %+v, want %+v", got, want)
	}
	wantLatestRelease(t, h, helmRecord{revision: 3, status: "deployed", chartVersion: "1.0.0"})

	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 4, "1.1.0")
	wantLatestRelease(t, h, helmRecord{revision: 4, status: "deployed", chartVersion: "1.1.0"})
}

// fakeKubeClient is the fake Helm backend's Kubernetes client, whose errors a
// test sets to make one Helm call fail.
func fakeKubeClient(t *testing.T, h *fakeHelm) *kubefake.FailingKubeClient {
	t.Helper()
	kc, ok := h.cfg.KubeClient.(*kubefake.FailingKubeClient)
	if !ok {
		t.Fatalf("fake Helm kube client is %T, want *kubefake.FailingKubeClient", h.cfg.KubeClient)
	}
	return kc
}

// pastBackoff moves the HelmChart's last attempt out of the failed backoff, so
// the next reconcile retries it.
func pastBackoff(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		if err := unstructured.SetNestedField(hc.Object, past, "status", "lastAttempt"); err != nil {
			t.Fatal(err)
		}
	})
}

// A recovery uninstall that fails is reported, and the release it leaves
// uninstalling stays on the recovery path: the next tick finishes the
// uninstall instead of upgrading a partly removed release.
func TestHelmFlowRecoverFailedUninstallIsReportedAndRetried(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := fakeKubeClient(t, h)
	kc.BuildError = errors.New("build refused")
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	got := readFlowStatus(t, dyn)
	if got.phase != hcPhaseFailed || !strings.Contains(got.message, "recover pending-install release: uninstall: failed to delete release") {
		t.Fatalf("helmchart status after a failed recovery uninstall = %+v, want Failed naming the uninstall error", got)
	}
	wantLatestRelease(t, h, helmRecord{revision: 1, status: "uninstalling", chartVersion: "1.0.0"})

	kc.BuildError = nil
	pastBackoff(t, dyn)
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("uninstalling release not purged by the next recovery: %+v", hist)
	}
	reconcileFlow(t, dyn)
	wantDeployed(t, dyn, 1, "1.0.0")
}

// A recovery that cannot read the release history is reported and retried; it
// is never taken for a history with nothing to roll back to, which would
// uninstall a release that is serving. The first storage query is the
// release lookup, the second the history read.
func TestHelmFlowRecoverFailedHistoryReadNeverUninstalls(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	)
	h.failQuery(2, errors.New("list refused"))
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	got := readFlowStatus(t, dyn)
	if got.phase != hcPhaseFailed || !strings.Contains(got.message, "recover pending-upgrade release: read release history") || !strings.Contains(got.message, "list refused") {
		t.Fatalf("helmchart status after a failed history read = %+v, want Failed naming the history read", got)
	}
	want := []helmRecord{
		{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	}
	if hist := h.history(t, flowNS, flowRelease); !slices.Equal(hist, want) {
		t.Fatalf("release history after a failed history read = %+v, want it untouched %+v", hist, want)
	}

	pastBackoff(t, dyn)
	reconcileFlow(t, dyn)

	wantLatestRelease(t, h, helmRecord{revision: 3, status: "deployed", chartVersion: "1.0.0"})
}

// A recovery rollback that fails is reported instead of being taken for done.
func TestHelmFlowRecoverFailedRollbackIsReported(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	)
	fakeKubeClient(t, h).UpdateError = errors.New("update refused")
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	got := readFlowStatus(t, dyn)
	if got.phase != hcPhaseFailed || !strings.Contains(got.message, "recover pending-upgrade release: roll back to revision 1") || !strings.Contains(got.message, "update refused") {
		t.Fatalf("helmchart status after a failed recovery rollback = %+v, want Failed naming the rollback error", got)
	}
}

// A rollback that was itself cut short leaves a pending-rollback revision on
// top of the failed upgrade. Recovery rolls back to the last revision that was
// deployed, not to the revision just below, which is the upgrade that never
// finished.
func TestHelmFlowRecoverPendingRollbackTargetsLastDeployed(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "failed", chartVersion: "1.1.0"},
		helmRecord{revision: 3, status: "pending-rollback", chartVersion: "1.0.0"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.2.0")})

	reconcileFlow(t, dyn)

	want := flowStatus{phase: hcPhasePending, message: "recovering pending release", revision: 3}
	if got := readFlowStatus(t, dyn); got != want {
		t.Fatalf("helmchart status after recovery = %+v, want %+v", got, want)
	}
	wantLatestRelease(t, h, helmRecord{revision: 4, status: "deployed", chartVersion: "1.0.0"})
}

// A rollback that failed marks the upgrade it started from superseded although
// that upgrade never deployed. Recovery prefers an earlier deployed revision
// to a superseded one, so it rolls back past it to the revision that actually
// served.
func TestHelmFlowRecoverSkipsSupersededThatNeverDeployed(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "superseded", chartVersion: "1.1.0", description: "Preparing upgrade"},
		helmRecord{revision: 3, status: "pending-rollback", chartVersion: "1.0.0"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.2.0")})

	reconcileFlow(t, dyn)

	wantLatestRelease(t, h, helmRecord{revision: 4, status: "deployed", chartVersion: "1.0.0"})
}

// The same failed rollback, started from a recovery after an upgrade was cut
// short, can also leave no deployed revision: Helm records the upgrade it
// started from superseded and only then the rollback failed, and a crash
// between the two writes leaves the rollback pending. The upgrade never served,
// so recovery passes over it to the revision that completed.
func TestHelmFlowRecoverSkipsUnservedSupersededWithNothingDeployed(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "superseded", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "superseded", chartVersion: "1.1.0", description: "Preparing upgrade"},
		helmRecord{revision: 3, status: "pending-rollback", chartVersion: "1.0.0", description: "Rollback to 1"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.2.0")})

	reconcileFlow(t, dyn)

	wantLatestRelease(t, h, helmRecord{revision: 4, status: "deployed", chartVersion: "1.0.0"})
}

// A superseded revision with no other proof it served, and nothing completed
// before it, leaves recovery nothing to roll back to, so it uninstalls.
func TestHelmFlowRecoverWithOnlyUnservedSupersededUninstalls(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "superseded", chartVersion: "1.0.0", description: "Preparing upgrade"},
		helmRecord{revision: 2, status: "pending-rollback", chartVersion: "1.0.0", description: "Rollback to 1"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release with nothing that served still stored after recovery: %+v", hist)
	}
}

// An upgrade cut short after Helm marked the previous revision superseded but
// before it marked the new one deployed leaves no deployed revision, yet the
// superseded one served. Recovery rolls back to it instead of uninstalling a
// working release.
func TestHelmFlowRecoverFallsBackToSuperseded(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "superseded", chartVersion: "1.0.0", description: "Install complete"},
		helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	want := flowStatus{phase: hcPhasePending, message: "recovering pending release", revision: 2}
	if got := readFlowStatus(t, dyn); got != want {
		t.Fatalf("helmchart status after recovery = %+v, want %+v", got, want)
	}
	wantLatestRelease(t, h, helmRecord{revision: 3, status: "deployed", chartVersion: "1.0.0"})
}

// A pending upgrade with no earlier revision that was ever deployed or
// superseded has no good state to roll back to, so it is uninstalled like a
// first install.
func TestHelmFlowRecoverWithNothingDeployedUninstalls(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "failed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release with nothing deployed still stored after recovery: %+v", hist)
	}
	reconcileFlow(t, dyn)
	wantDeployed(t, dyn, 1, "1.1.0")
}

// A first install left pending-install has nothing to roll back to, so it is
// uninstalled with its history, and the next tick installs it afresh.
func TestHelmFlowRecoverPendingInstallUninstalls(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	want := flowStatus{phase: hcPhasePending, message: "recovering pending release", revision: 1}
	if got := readFlowStatus(t, dyn); got != want {
		t.Fatalf("helmchart status after recovery = %+v, want %+v", got, want)
	}
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("pending first install still stored after recovery: %+v", hist)
	}

	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 1, "1.0.0")
	wantLatestRelease(t, h, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
}

// Every newFlowHC HelmChart carries a metadata.uid of its own. The operator
// keys its process-wide records of unwritten incomplete statuses and of clean
// uninstalls by namespace/name/uid, so charts without UIDs would share one
// entry between tests.
func TestHelmFlowChartsCarryUniqueUIDs(t *testing.T) {
	dyn1 := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	dyn2 := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	uid1, uid2 := getFlowHC(t, dyn1).GetUID(), getFlowHC(t, dyn2).GetUID()
	if uid1 == "" || uid2 == "" {
		t.Fatalf("flow HelmCharts carry UIDs %q and %q, want two non-empty metadata.uid values", uid1, uid2)
	}
	if uid1 == uid2 {
		t.Fatalf("two flow HelmCharts carry the same metadata.uid %q, want one per chart", uid1)
	}
}

// A reconcile whose "uninstall incomplete:" status write keeps failing leaves
// an entry in the process-wide unwrittenIncomplete record. This test leaves
// that entry behind with nothing in the test body removing it: the shared
// fixture must clear the record when the test ends, and the cleanup below
// fails if the record is not empty again by then.
func TestHelmFlowUnwrittenIncompleteLeftBehind(t *testing.T) {
	t.Cleanup(func() { wantNoUnwrittenIncomplete(t, "at the end of a test that left an entry behind") })
	shortIncompleteWriteBackoff(t)
	useFakeHelm(t, fakeHelmOptions{}) // no release: Helm purged it
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeletedWithStatus(t, dyn, map[string]interface{}{
		"phase": hcPhaseUninstalling, "message": uninstallingMessage, "releaseNamespace": flowNS, "releaseName": flowRelease,
	})
	outage := failStatusWritesFromIncomplete(t, dyn)

	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("finalizer removed while the %q status write was failing; status = %+v", uninstallIncompletePrefix, readFlowStatus(t, dyn))
	}
	outage.wantEveryAttemptRejected(t, "after the uninstall")
	rec, ok := unwrittenIncompleteFor(getFlowHC(t, dyn))
	if !ok {
		t.Fatalf("no unwrittenIncomplete entry left behind for this HelmChart; status = %+v", readFlowStatus(t, dyn))
	}
	if !strings.HasPrefix(rec.msg, uninstallIncompletePrefix) {
		t.Errorf("left-behind message = %q, want the %q prefix", rec.msg, uninstallIncompletePrefix)
	}
}

// An unwrittenIncomplete entry for one HelmChart must not touch another's
// reconcile. Before each flow chart carried its own UID, every one of them
// shared the key planted here, so an entry one test left behind held the next
// test's chart in place of its own reconcile. The chart reconciled here must
// deploy as if the entry were not there, and the entry must still be waiting
// for the chart it belongs to.
func TestHelmFlowReconcileIgnoresAnotherChartsUnwrittenIncomplete(t *testing.T) {
	useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	leftoverKey := flowNS + "/" + flowName + "/"
	unwrittenIncompleteMu.Lock()
	unwrittenIncomplete[leftoverKey] = incompleteRecord{
		msg:  uninstallIncompleteRecover + "ConfigMap/some-other-chart",
		note: "planted by TestHelmFlowReconcileIgnoresAnotherChartsUnwrittenIncomplete",
	}
	unwrittenIncompleteMu.Unlock()

	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 1, "1.0.0")
	unwrittenIncompleteMu.Lock()
	_, stillThere := unwrittenIncomplete[leftoverKey]
	unwrittenIncompleteMu.Unlock()
	if !stillThere {
		t.Errorf("reconciling one HelmChart consumed the unwrittenIncomplete entry planted for another")
	}
}

// A recovery whose pre-uninstall marker write is rejected runs no uninstall:
// the marker is the evidence a later restart must find, so while its write
// fails the release stays untouched and nothing is recorded as incomplete.
func TestHelmFlowRecoveryMarkerWriteFailureNeverUninstalls(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	rejected := false
	dyn.(*dynfake.FakeDynamicClient).PrependReactor("update", helmChartGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		obj, ok := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		if msg, _, _ := unstructured.NestedString(obj.Object, "status", "message"); msg == recoveringUninstallMarker {
			rejected = true
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})

	reconcileFlow(t, dyn)

	if !rejected {
		t.Fatalf("the %q status write was not rejected as arranged; status = %+v", recoveringUninstallMarker, readFlowStatus(t, dyn))
	}
	want := []helmRecord{{revision: 1, status: "pending-install", chartVersion: "1.0.0"}}
	if hist := h.history(t, flowNS, flowRelease); !slices.Equal(hist, want) {
		t.Errorf("release history after a rejected marker write = %+v, want it untouched at %+v: no uninstall may run unmarked", hist, want)
	}
	if _, ok := unwrittenIncompleteFor(getFlowHC(t, dyn)); ok {
		t.Errorf("an incomplete record was parked although no uninstall ran; status = %+v", readFlowStatus(t, dyn))
	}
}
