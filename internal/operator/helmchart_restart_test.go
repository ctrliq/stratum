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

// TestHelmChartRestartMidRecoveryHolds pins that the hold on a pending-install
// recovery survives an operator restart between Helm's purge and the
// "uninstall incomplete:" status write. The recovery uninstalls a release with
// nothing that served, Helm purges it and leaves objects whose ownership it
// could not verify, and the status write that would hold the reinstall keeps
// failing, so the operator parks that write in its own memory. A restart
// discards that memory, and the release is gone with it: a next reconcile that
// finds no hold reinstalls over the objects Helm could not verify. The outage
// lets the writes a reconcile makes before it decides on the release through,
// so the post-restart reconcile reaches the release lookup, and the hold must
// rest on status.unrecordedRecovery — the marker write's standing record, not
// the in-memory one: nothing reinstalls, and deleting the HelmChart keeps its
// finalizer until the objects are gone.
func TestHelmChartRestartMidRecoveryHolds(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	outage := failIncompleteWrites(t, dyn)

	// One reconcile: the pre-Helm Pending write succeeds, the recovery
	// uninstall purges the release and leaves app-cm behind, and the
	// "uninstall incomplete:" write fails for every retry, so the record of it
	// is parked in the operator's memory.
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored after the recovery uninstall, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}
	outage.wantEveryAttemptRejected(t, "after the recovery uninstall")
	if _, ok := unwrittenIncompleteFor(getFlowHC(t, dyn)); !ok {
		t.Fatalf("no unwritten recovery record parked for this HelmChart; status = %+v", readFlowStatus(t, dyn))
	}

	// The operator restarts before the parked record reaches the status: the
	// process, and with it the record, is gone. Helm's reads of the cluster
	// answer again, so a reinstall would now succeed; the write outage
	// continues until the deletion below.
	forgetUnwrittenIncomplete(t)
	kc.DummyResources = nil

	reconcileFlow(t, dyn)

	// The hold must survive the restart on status.unrecordedRecovery: the
	// reconcile reached the release lookup — the outage rejected its hold
	// write — and reinstalls nothing, staying off Deployed.
	outage.wantEveryAttemptRejected(t, "after the post-restart reconcile")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Errorf("after the restart, the next reconcile reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	if st := readFlowStatus(t, dyn); st.phase == hcPhaseDeployed {
		t.Errorf("after the restart, the next reconcile marked the HelmChart Deployed over the objects Helm could not verify: %+v", st)
	}

	// Deleting the same HelmChart then keeps its finalizer: the restart must
	// not let the deletion conclude while the objects are still there. The
	// outage ends first, so the rewrite the deletion drives can reach the
	// status.
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

// TestHelmChartSecondRestartKeepsTheHold pins that the hold outlives a second
// restart. After the first restart the reconcile records the hold from the
// standing recovery evidence, but its "uninstall incomplete:" write fails and
// parks in the operator's memory. A second restart discards that memory, so
// the next reconcile must hold on what the status still says: it reinstalls
// nothing, stays off Deployed, and deleting the HelmChart keeps its finalizer.
// The writes a reconcile makes before it decides on the release must not
// destroy that evidence.
func TestHelmChartSecondRestartKeepsTheHold(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	outage := failIncompleteWrites(t, dyn)

	// One reconcile: the pre-uninstall marker write gets through, the recovery
	// uninstall purges the release and leaves app-cm behind, and the
	// "uninstall incomplete:" write fails for every retry, so the record of it
	// is parked in the operator's memory.
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored after the recovery uninstall, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}
	outage.wantEveryAttemptRejected(t, "after the recovery uninstall")

	// First restart: the parked record is gone. The recovery evidence in the
	// status must drive the hold, and its write fails again and parks.
	forgetUnwrittenIncomplete(t)
	kc.DummyResources = nil

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("after the first restart, the next reconcile reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	outage.wantEveryAttemptRejected(t, "after the first post-restart reconcile")

	// Second restart: the parked record is gone again. The status is all that
	// is left, and it must still hold.
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

	// Deleting the same HelmChart once the outage ends keeps its finalizer.
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

// TestHelmChartDeletionHoldsWhenMarkerMessageWasReplaced pins the deletion
// path's recovery check on status.unrecordedRecovery, the field that stands
// when the marker message itself no longer does. The sequence it pins is
// reachable: the recovery's pre-uninstall marker write gets through, the
// operator restarts, and a Failed write the next reconcile makes before the
// release lookup — a load-chart error, or the containment refusal — replaces
// the marker message while the field it wrote survives. Deleting the HelmChart
// then must hold on the field: the uninstall finds the release gone, which
// proves nothing about what the interrupted uninstall left, so the finalizer
// stays and the deletion wording names the release.
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

// A clean recovery result is still known to this process if only its status
// write fails. Retrying that result must clear the pre-uninstall marker before
// a missing Helm release is mistaken for objects that might remain.
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

	// While the outcome write still fails, this process must retain what it
	// observed and neither install nor invent an uninstall-incomplete hold.
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

// incompleteOutage rejects, until end, every HelmChart status write whose
// message starts with uninstallIncompletePrefix, and counts the rejections
// since the last wantEveryAttemptRejected.
type incompleteOutage struct {
	ended    bool
	rejected int
}

// failIncompleteWrites starts an incompleteOutage on the HelmChart in dyn.
// Unlike failStatusWritesFromIncomplete it lets every other status write
// through, so a reconcile after a restart reaches the hold write instead of
// stopping at its pre-Helm writes: the outage models an apiserver failure that
// began when the first "uninstall incomplete:" write ran.
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

// end lets every later status write through.
func (o *incompleteOutage) end() { o.ended = true }

// wantEveryAttemptRejected fails unless the outage rejected exactly
// incompleteWriteAttempts "uninstall incomplete:" writes since the last call,
// so the reconcile just run tried the write, used up its retries, and left the
// status unwritten.
func (o *incompleteOutage) wantEveryAttemptRejected(t *testing.T, when string) {
	t.Helper()
	if o.rejected != incompleteWriteAttempts {
		t.Fatalf("%s: rejected %d %q status writes, want %d (one reconcile's attempts)", when, o.rejected, uninstallIncompletePrefix, incompleteWriteAttempts)
	}
	o.rejected = 0
}
