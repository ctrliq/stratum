package operator

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

	"helm.sh/helm/v4/pkg/action"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
)

const (
	flowNS      = "kube-system"
	flowName    = "app"
	flowRelease = "app"
)

var flowPolicy = helmPolicy{privilegedNamespaces: []string{flowNS}}

func flowChart(t *testing.T, version string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(makeChartTGZ(t, flowRelease, version))
}

// The operator keys its unwrittenIncomplete and cleanUninstalls records by namespace/name/uid.
var flowHCUID atomic.Int64

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

// The missing kubeconfig makes any Helm work bypassing newActionConfigFn fail.
func reconcileFlow(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")
	reconcileHelmChart(context.Background(), dyn, kubeconfig, getFlowHC(t, dyn), "", nil, flowPolicy)
}

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

func TestHelmFlowDeletionHoldsAfterUnrecordedUninstall(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	useFakeHelm(t, fakeHelmOptions{})
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

func TestHelmFlowDeletionAfterFailedActionConfig(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	useFakeHelm(t, fakeHelmOptions{})
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

	reconcileHelmChart(context.Background(), dyn, filepath.Join(t.TempDir(), "missing-kubeconfig"), getFlowHC(t, dyn), "", nil, helmPolicy{})

	if st := readFlowStatus(t, dyn); st.phase != hcPhaseFailed || !strings.HasPrefix(st.message, "refusing to install: spec.targetNamespace") {
		t.Fatalf("status = %+v, want the targetNamespace refusal in place of the hold", st)
	}
}

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

func fakeKubeClient(t *testing.T, h *fakeHelm) *kubefake.FailingKubeClient {
	t.Helper()
	kc, ok := h.cfg.KubeClient.(*kubefake.FailingKubeClient)
	if !ok {
		t.Fatalf("fake Helm kube client is %T, want *kubefake.FailingKubeClient", h.cfg.KubeClient)
	}
	return kc
}

func pastBackoff(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		if err := unstructured.SetNestedField(hc.Object, past, "status", "lastAttempt"); err != nil {
			t.Fatal(err)
		}
	})
}

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

// Query 2 reads history; a failure must not count as empty: it would uninstall a serving release.
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

func TestHelmFlowUnwrittenIncompleteLeftBehind(t *testing.T) {
	t.Cleanup(func() { wantNoUnwrittenIncomplete(t, "at the end of a test that left an entry behind") })
	shortIncompleteWriteBackoff(t)
	useFakeHelm(t, fakeHelmOptions{})
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

func TestHelmFlowRecoveryHoldFieldRecordedAndCleared(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	spec := parseHelmSpec(getFlowHC(t, dyn))
	vals, err := mergedValues(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	st, _, _ := unstructured.NestedMap(getFlowHC(t, dyn).Object, "status")
	hold, ok := incompleteHoldFrom(st)
	if !ok {
		t.Fatalf("no hold recorded in status.%s after the incomplete recovery; status = %+v", holdField, st)
	}
	if hold.path != holdPathRecover {
		t.Errorf("hold path = %q, want %q", hold.path, holdPathRecover)
	}
	if want := specHash(spec, vals); hold.specHash != want {
		t.Errorf("hold specHash = %q, want the current spec's %q", hold.specHash, want)
	}
	if !strings.Contains(hold.unverified, "app-cm") ||
		!strings.Contains(hold.unverified, "were not deleted because their ownership could not be verified") {
		t.Errorf("hold objects = %q, want Helm's list of the objects it could not verify, naming app-cm", hold.unverified)
	}
	if want := incompleteHoldMessage(hold); want != str(st["message"], "") {
		t.Errorf("status message = %q, want the message the hold rebuilds to %q", str(st["message"], ""), want)
	}

	pastBackoff(t, dyn)
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Errorf("reinstalled while the hold recorded in status.%s stands; history = %+v", holdField, hist)
	}

	kc.DummyResources = nil
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(hc.Object, flowChart(t, "1.1.0"), "spec", "chartContent"); err != nil {
			t.Fatal(err)
		}
	})
	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 1, "1.1.0")
	stAfter, _, _ := unstructured.NestedMap(getFlowHC(t, dyn).Object, "status")
	if _, ok := incompleteHoldFrom(stAfter); ok {
		t.Errorf("status.%s still recorded after the clean reinstall; status = %+v", holdField, stAfter)
	}
}
