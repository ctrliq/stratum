package operator

import (
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// the hold must rest on status.unrecordedRecovery once a restart discards the parked write.
func TestHelmChartRestartMidRecoveryHolds(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	outage := failIncompleteWrites(t, dyn)

	// the recovery uninstall purges the release; the hold write fails every retry.
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored after the recovery uninstall, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}
	outage.wantEveryAttemptRejected(t, "after the recovery uninstall")
	if _, ok := unwrittenIncompleteFor(getFlowHC(t, dyn)); !ok {
		t.Fatalf("no unwritten recovery record parked for this HelmChart; status = %+v", readFlowStatus(t, dyn))
	}

	// the restart discards the parked record; the write outage continues.
	forgetUnwrittenIncomplete(t)
	kc.DummyResources = nil

	reconcileFlow(t, dyn)

	// the hold must rest on status.unrecordedRecovery, not the parked record.
	outage.wantEveryAttemptRejected(t, "after the post-restart reconcile")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Errorf("after the restart, the next reconcile reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	if st := readFlowStatus(t, dyn); st.phase == hcPhaseDeployed {
		t.Errorf("after the restart, the next reconcile marked the HelmChart Deployed over the objects Helm could not verify: %+v", st)
	}

	// the restart must not let the deletion conclude while the objects remain.
	outage.end()
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		hc.SetDeletionTimestamp(&deleted)
	})
	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Errorf("after the restart, deleting the HelmChart removed the finalizer; status = %+v", readFlowStatus(t, dyn))
	}
}

// the hold outlives a second restart on what the status still says.
func TestHelmChartSecondRestartKeepsTheHold(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	outage := failIncompleteWrites(t, dyn)

	// the recovery uninstall purges the release; the hold write fails and parks.
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored after the recovery uninstall, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}
	outage.wantEveryAttemptRejected(t, "after the recovery uninstall")

	// the status evidence must drive the hold; its write fails again and parks.
	forgetUnwrittenIncomplete(t)
	kc.DummyResources = nil

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("after the first restart, the next reconcile reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	outage.wantEveryAttemptRejected(t, "after the first post-restart reconcile")

	// the status is all that is left, and it must still hold.
	forgetUnwrittenIncomplete(t)

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Errorf("after the second restart, the next reconcile reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	if st := readFlowStatus(t, dyn); st.phase == hcPhaseDeployed {
		t.Errorf("after the second restart, the next reconcile marked the HelmChart Deployed over the objects Helm could not verify: %+v", st)
	}
	if _, ok := unwrittenIncompleteFor(getFlowHC(t, dyn)); !ok {
		t.Errorf("after the second restart, no hold write was parked for this HelmChart; status = %+v", readFlowStatus(t, dyn))
	}

	// deleting the HelmChart once the outage ends keeps its finalizer.
	outage.end()
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		hc.SetDeletionTimestamp(&deleted)
	})
	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Errorf("after the second restart, deleting the HelmChart removed the finalizer; status = %+v", readFlowStatus(t, dyn))
	}
}

// the deletion path must hold on the unrecordedRecovery field once the marker message is gone.
func TestHelmChartDeletionHoldsWhenMarkerMessageWasReplaced(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	useFakeHelm(t, fakeHelmOptions{}) // no release: the interrupted uninstall purged it
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeletedWithStatus(t, dyn, map[string]interface{}{
		"phase":                 hcPhaseFailed,
		"message":               "load chart: decode chartContent: illegal base64 data",
		unrecordedRecoveryField: true,
		"releaseNamespace":      flowNS,
		"releaseName":           flowRelease,
	})

	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("finalizer removed although status.%s recorded an unresolved recovery uninstall; status = %+v", unrecordedRecoveryField, readFlowStatus(t, dyn))
	}
	st := readFlowStatus(t, dyn)
	if !strings.HasPrefix(st.message, uninstallIncompleteDelete) ||
		!strings.Contains(st.message, "any objects of release "+flowNS+"/"+flowRelease) {
		t.Errorf("status = %+v, want Failed with %q naming any objects of release %s/%s", st, uninstallIncompleteDelete, flowNS, flowRelease)
	}
}

// retrying the observed outcome must clear the marker before a missing release reads as a hold.
func TestHelmChartCleanRecoveryOutcomeWriteIsRetried(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	fake := dyn.(*dynfake.FakeDynamicClient)
	rejectOutcome := true
	rejected := 0
	fake.PrependReactor("update", helmChartGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		update, ok := action.(ktesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := update.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		msg, _, _ := unstructured.NestedString(obj.Object, "status", "message")
		if rejectOutcome && msg == recoveringPendingMessage {
			rejected++
			return true, nil, apierrors.NewInternalError(errors.New("outcome status write failed"))
		}
		return false, nil, nil
	})

	reconcileFlow(t, dyn)
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("clean recovery did not purge the pending release: %+v", hist)
	}
	if rejected != 1 {
		t.Fatalf("rejected %d clean outcome writes, want 1", rejected)
	}

	// while the outcome write fails, neither install nor invent a hold.
	reconcileFlow(t, dyn)
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("reinstalled before the clean recovery outcome was recorded: %+v", hist)
	}
	st, _, _ := unstructured.NestedMap(getFlowHC(t, dyn).Object, "status")
	if _, held := incompleteHoldFrom(st); held || strings.HasPrefix(str(st["message"], ""), uninstallIncompletePrefix) {
		t.Fatalf("clean recovery became an uninstall-incomplete hold: %+v", st)
	}
	if rejected != 2 {
		t.Fatalf("rejected %d clean outcome writes, want a retry", rejected)
	}

	rejectOutcome = false
	reconcileFlow(t, dyn)
	st, _, _ = unstructured.NestedMap(getFlowHC(t, dyn).Object, "status")
	if asBool(st[unrecordedRecoveryField]) || str(st["message"], "") != recoveringPendingMessage {
		t.Fatalf("clean outcome was not recorded after the outage: %+v", st)
	}
	reconcileFlow(t, dyn)
	wantLatestRelease(t, h, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
}

// rejects, until end, every status write whose message starts with uninstallIncompletePrefix.
type incompleteOutage struct {
	ended    bool
	rejected int
}

// unlike failStatusWritesFromIncomplete it lets every other status write through.
func failIncompleteWrites(t *testing.T, dyn dynamic.Interface) *incompleteOutage {
	t.Helper()
	fake, ok := dyn.(*dynfake.FakeDynamicClient)
	if !ok {
		t.Fatalf("dynamic client is %T, want *dynfake.FakeDynamicClient", dyn)
	}
	o := &incompleteOutage{}
	fake.PrependReactor("update", helmChartGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" || o.ended {
			return false, nil, nil
		}
		update, ok := action.(ktesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := update.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		if msg, _, _ := unstructured.NestedString(obj.Object, "status", "message"); !strings.HasPrefix(msg, uninstallIncompletePrefix) {
			return false, nil, nil
		}
		o.rejected++
		return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	})
	return o
}

func (o *incompleteOutage) end() { o.ended = true }

// fails unless one reconcile's retries were all rejected and the status left unwritten.
func (o *incompleteOutage) wantEveryAttemptRejected(t *testing.T, when string) {
	t.Helper()
	if o.rejected != incompleteWriteAttempts {
		t.Fatalf("%s: rejected %d %q status writes, want %d (one reconcile's attempts)", when, o.rejected, uninstallIncompletePrefix, incompleteWriteAttempts)
	}
	o.rejected = 0
}
