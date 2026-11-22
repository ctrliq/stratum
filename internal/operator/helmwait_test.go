package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"

	"helm.sh/helm/v4/pkg/kube"
	helmrelease "helm.sh/helm/v4/pkg/release/v1"
)

type waitRecorder struct {
	kube.Interface

	mu         sync.Mutex
	strategies []kube.WaitStrategy
}

// Wraps the one Kubernetes client every action config shares, so every waiter is recorded.
func recordWaits(h *fakeHelm) *waitRecorder {
	rec := &waitRecorder{Interface: h.cfg.KubeClient}
	h.cfg.KubeClient = rec
	return rec
}

func (w *waitRecorder) GetWaiter(ws kube.WaitStrategy) (kube.Waiter, error) {
	return w.GetWaiterWithOptions(ws)
}

// The upgrade worker calls this from its own goroutine, hence the lock.
func (w *waitRecorder) GetWaiterWithOptions(ws kube.WaitStrategy, opts ...kube.WaitOption) (kube.Waiter, error) {
	w.mu.Lock()
	w.strategies = append(w.strategies, ws)
	w.mu.Unlock()
	if ws == "" {
		return nil, errors.New("wait strategy not set (refused by waitRecorder, as Helm's real client refuses it)")
	}
	if inner, ok := w.Interface.(kube.InterfaceWaitOptions); ok {
		return inner.GetWaiterWithOptions(ws, opts...)
	}
	return w.Interface.GetWaiter(ws)
}

func wantHelmWaitStrategy(t *testing.T, rec *waitRecorder, action string) {
	t.Helper()
	rec.mu.Lock()
	got := append([]kube.WaitStrategy(nil), rec.strategies...)
	rec.mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("%s asked for no waiter, want every waiter built with helmWaitStrategy %q", action, helmWaitStrategy)
	}
	for _, ws := range got {
		if ws != helmWaitStrategy {
			t.Fatalf("%s asked for waiters with strategies %q, want every one helmWaitStrategy %q", action, got, helmWaitStrategy)
		}
	}
}

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

func TestHelmFlowWaitStrategyInstall(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	rec := recordWaits(h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	wantHelmWaitStrategy(t, rec, "install")
	wantDeployed(t, dyn, 1, "1.0.0")
}

func TestHelmFlowWaitStrategyUpgrade(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
	rec := recordWaits(h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	wantHelmWaitStrategy(t, rec, "upgrade")
	wantDeployed(t, dyn, 2, "1.1.0")
	wantLatestRelease(t, h, helmRecord{revision: 2, status: "deployed", chartVersion: "1.1.0"})
}

func TestHelmFlowWaitStrategyDeletionUninstall(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
	rec := recordWaits(h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeleted(t, dyn)

	reconcileFlow(t, dyn)

	wantHelmWaitStrategy(t, rec, "uninstall on deletion")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Errorf("release %s/%s still stored after deletion: %+v", flowNS, flowRelease, hist)
	}
	if hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Error("finalizer kept after the uninstall")
	}
}

func TestHelmFlowWaitStrategyRecoverUninstall(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	rec := recordWaits(h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	wantHelmWaitStrategy(t, rec, "uninstall of a pending first install")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("pending first install still stored after recovery: %+v", hist)
	}
}

func TestHelmFlowWaitStrategyRecoverRollback(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease,
		helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"},
		helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"},
	)
	rec := recordWaits(h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.1.0")})

	reconcileFlow(t, dyn)

	wantHelmWaitStrategy(t, rec, "rollback of a pending upgrade")
	wantLatestRelease(t, h, helmRecord{revision: 3, status: "deployed", chartVersion: "1.0.0"})
}

type deleteRecorder struct{ deleted []kube.ResourceList }

func (r *deleteRecorder) Wait(kube.ResourceList, time.Duration) error         { return nil }
func (r *deleteRecorder) WaitWithJobs(kube.ResourceList, time.Duration) error { return nil }
func (r *deleteRecorder) WatchUntilReady(kube.ResourceList, time.Duration) error {
	return nil
}
func (r *deleteRecorder) WaitForDelete(resources kube.ResourceList, _ time.Duration) error {
	r.deleted = append(r.deleted, resources)
	return nil
}

func configMapInfo(name string, annotations map[string]string) *resource.Info {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetName(name)
	u.SetAnnotations(annotations)
	return &resource.Info{Name: name, Object: u}
}

// Helm v3 waited for hook deletions by delete policy, not for uninstall deletions with Wait=false.
func TestV3NoWaitWaitsForDeletedHooksOnly(t *testing.T) {
	hook := map[string]string{helmrelease.HookAnnotation: "pre-install"}
	for _, tc := range []struct {
		name      string
		resources kube.ResourceList
		wantWait  bool
	}{
		{"hooks deleted by their delete policy", kube.ResourceList{configMapInfo("hook-a", hook), configMapInfo("hook-b", hook)}, true},
		{"release objects deleted by an uninstall", kube.ResourceList{configMapInfo("app", nil)}, false},
		{"no objects", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &deleteRecorder{}
			if err := (v3NoWait{legacy: rec}).WaitForDelete(tc.resources, time.Minute); err != nil {
				t.Fatalf("WaitForDelete: %v", err)
			}
			if got := len(rec.deleted) == 1; got != tc.wantWait {
				t.Fatalf("waited for deletion = %v (calls %d), want %v", got, len(rec.deleted), tc.wantWait)
			}
			if tc.wantWait && len(rec.deleted[0]) != len(tc.resources) {
				t.Fatalf("waited for %d object(s), want all %d", len(rec.deleted[0]), len(tc.resources))
			}
		})
	}
}

// Helm 4.3.1's install asks StatusWatcherStrategy for a chart's crds/; it must map to v3NoWait.
func TestV3WaitClientAnswersTheCRDWaitStrategy(t *testing.T) {
	c := v3WaitClient{kube.New(nil)}
	for _, ws := range []kube.WaitStrategy{helmWaitStrategy, kube.StatusWatcherStrategy} {
		w, err := c.GetWaiterWithOptions(ws)
		if err != nil {
			t.Fatalf("GetWaiterWithOptions(%s): %v", ws, err)
		}
		if _, ok := w.(v3NoWait); !ok {
			t.Errorf("GetWaiterWithOptions(%s) = %T, want v3NoWait", ws, w)
		}
	}
}
