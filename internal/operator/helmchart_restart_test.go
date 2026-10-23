package operator

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

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
