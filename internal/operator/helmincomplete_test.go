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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// TestUninstallIncompleteWriteKeepsFailing pins the fail-safe when the
// "uninstall incomplete:" status write fails for longer than one reconcile's
// retries. Helm has purged the release by then, so only the operator's
// in-memory record of the unwritten status stops the next reconcile, which
// finds no release, from removing the finalizer (deletion) or reinstalling
// over the objects Helm could not verify (pending-install recovery). While the
// writes keep failing, every reconcile keeps the finalizer and reinstalls
// nothing; once a write succeeds, the status is Failed with the message.
func TestUninstallIncompleteWriteKeepsFailing(t *testing.T) {
	t.Run("deletion", func(t *testing.T) {
		forgetUnwrittenIncomplete(t)
		shortIncompleteWriteBackoff(t)
		h := useFakeHelm(t, fakeHelmOptions{})
		h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
		forbidOwnershipCheck(t, h)
		dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
		markFlowHCDeleted(t, dyn)
		outage := failStatusWritesFromIncomplete(t, dyn)

		for _, when := range []string{"after the uninstall", "after the next reconcile"} {
			reconcileFlow(t, dyn)

			if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
				t.Fatalf("%s: finalizer removed while the %q status write was failing; status = %+v", when, uninstallIncompletePrefix, readFlowStatus(t, dyn))
			}
			outage.wantEveryAttemptRejected(t, when)
		}

		outage.end()
		reconcileFlow(t, dyn)

		if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
			t.Fatalf("finalizer removed once the status write succeeded; status = %+v", readFlowStatus(t, dyn))
		}
		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteDelete, "once the status write succeeds")
	})

	t.Run("pending install recovery", func(t *testing.T) {
		forgetUnwrittenIncomplete(t)
		shortIncompleteWriteBackoff(t)
		h := useFakeHelm(t, fakeHelmOptions{})
		h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
		kc := forbidOwnershipCheck(t, h)
		dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
		outage := failStatusWritesFromIncomplete(t, dyn)

		reconcileFlow(t, dyn)

		if hist := h.history(t, flowNS, flowRelease); hist != nil {
			t.Fatalf("release %s/%s still stored after the recovery uninstall, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
		}
		outage.wantEveryAttemptRejected(t, "after the recovery uninstall")

		// The apiserver answers reads again, so a reinstall would now succeed:
		// only the record of the unwritten status can stop it.
		kc.DummyResources = nil
		reconcileFlow(t, dyn)

		if hist := h.history(t, flowNS, flowRelease); hist != nil {
			t.Fatalf("next reconcile reinstalled over the objects Helm could not verify while the %q status write was failing: %+v; status = %+v", uninstallIncompletePrefix, hist, readFlowStatus(t, dyn))
		}
		outage.wantEveryAttemptRejected(t, "after the next reconcile")

		outage.end()
		reconcileFlow(t, dyn)

		if hist := h.history(t, flowNS, flowRelease); hist != nil {
			t.Fatalf("reinstalled over the objects Helm could not verify once the status write succeeded: %+v; status = %+v", hist, readFlowStatus(t, dyn))
		}
		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteRecover, "once the status write succeeds")

		// The written status now holds the reinstall back, so the record must be
		// gone: the spec change the message asks for reinstalls as usual.
		updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
			if err := unstructured.SetNestedField(hc.Object, flowChart(t, "1.1.0"), "spec", "chartContent"); err != nil {
				t.Fatal(err)
			}
		})
		reconcileFlow(t, dyn)

		wantDeployed(t, dyn, 1, "1.1.0")
	})
}

// TestUninstallIncompleteRecordRewrittenForDeletion pins what deleting a
// HelmChart does while the record of an unwritten recovery status is still
// pending. The pending-install recovery uninstalls the release, Helm cannot
// verify the ownership of an object it left, and the "uninstall incomplete:"
// status write keeps failing, so the operator parks the record in memory and
// retries it each reconcile. The parked message asks for a spec change to
// reinstall, which does nothing once the HelmChart is being deleted, so the
// retry the deletion drives must ask for the finalizer instead: the written
// status carries the deletion wording, keeping Helm's list of objects, and the
// finalizer stays until a person has removed them.
func TestUninstallIncompleteRecordRewrittenForDeletion(t *testing.T) {
	forgetUnwrittenIncomplete(t)
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	outage := failStatusWritesFromIncomplete(t, dyn)

	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored after the recovery uninstall, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}
	outage.wantEveryAttemptRejected(t, "after the recovery uninstall")
	rec, ok := unwrittenIncompleteFor(getFlowHC(t, dyn))
	if !ok {
		t.Fatalf("no unwritten recovery record parked for this HelmChart; status = %+v", readFlowStatus(t, dyn))
	}
	if !strings.HasPrefix(rec.msg, uninstallIncompleteRecover) {
		t.Fatalf("parked recovery record message = %q, want the recovery wording %q", rec.msg, uninstallIncompleteRecover)
	}

	// The apiserver answers again, and the HelmChart is deleted before the
	// parked status ever reached it: the retry must carry the deletion wording.
	kc.DummyResources = nil
	outage.end()
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		hc.SetDeletionTimestamp(&deleted)
	})
	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("finalizer removed although Helm left app-cm in place; status = %+v", readFlowStatus(t, dyn))
	}
	wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteDelete, "after the delete")
	if _, ok := unwrittenIncompleteFor(getFlowHC(t, dyn)); ok {
		t.Fatalf("unwritten recovery record kept after its status write succeeded; status = %+v", readFlowStatus(t, dyn))
	}

	// The hold the written status carries keeps the finalizer on the next
	// reconcile, still asking for the finalizer.
	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatalf("finalizer removed once the deletion wording was written; status = %+v", readFlowStatus(t, dyn))
	}
	wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteDelete, "after the next reconcile")
}

// incompleteWriteAttempts is how many times one reconcile tries the
// "uninstall incomplete:" status write under shortIncompleteWriteBackoff.
const incompleteWriteAttempts = 3

// shortIncompleteWriteBackoff gives each reconcile incompleteWriteAttempts
// tries at the "uninstall incomplete:" status write, a millisecond apart, for
// the rest of the test, so a write that keeps failing exhausts a reconcile's
// retries without the production backoff's 1.5s wait.
func shortIncompleteWriteBackoff(t *testing.T) {
	t.Helper()
	prev := incompleteWriteBackoff
	incompleteWriteBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: incompleteWriteAttempts}
	t.Cleanup(func() { incompleteWriteBackoff = prev })
}

// forgetUnwrittenIncomplete empties the operator's in-memory record of
// unwritten "uninstall incomplete:" statuses now and when the test ends.
// newFlowHC's HelmChart has no UID, so every flow test shares one key into
// that record, and an entry left behind would change how the next one runs.
func forgetUnwrittenIncomplete(t *testing.T) {
	t.Helper()
	forget := func() {
		unwrittenIncompleteMu.Lock()
		defer unwrittenIncompleteMu.Unlock()
		clear(unwrittenIncomplete)
	}
	forget()
	t.Cleanup(forget)
}

// statusOutage fails the HelmChart's status writes as an apiserver does while
// it cannot reach etcd, from the first "uninstall incomplete:" write until
// end. The outage cannot start earlier: the pending-install recovery first
// writes the release it is about to touch, and it stops before Helm work when
// that write fails, so its uninstall would never run.
type statusOutage struct {
	started, ended bool
	// rejected counts the "uninstall incomplete:" writes failed since the last
	// wantEveryAttemptRejected.
	rejected int
}

// failStatusWritesFromIncomplete starts a statusOutage on the HelmChart in dyn.
func failStatusWritesFromIncomplete(t *testing.T, dyn dynamic.Interface) *statusOutage {
	t.Helper()
	fake, ok := dyn.(*dynfake.FakeDynamicClient)
	if !ok {
		t.Fatalf("dynamic client is %T, want *dynfake.FakeDynamicClient", dyn)
	}
	o := &statusOutage{}
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
		msg, _, _ := unstructured.NestedString(obj.Object, "status", "message")
		incomplete := strings.HasPrefix(msg, uninstallIncompletePrefix)
		if incomplete {
			o.started = true
			o.rejected++
		}
		if !o.started {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	})
	return o
}

// end lets every later status write through.
func (o *statusOutage) end() { o.ended = true }

// wantEveryAttemptRejected fails unless the outage rejected exactly
// incompleteWriteAttempts "uninstall incomplete:" writes since the last call,
// so the reconcile just run tried the write, used up its retries, and left the
// status unwritten.
func (o *statusOutage) wantEveryAttemptRejected(t *testing.T, when string) {
	t.Helper()
	if o.rejected != incompleteWriteAttempts {
		t.Fatalf("%s: rejected %d %q status writes, want %d (one reconcile's attempts)", when, o.rejected, uninstallIncompletePrefix, incompleteWriteAttempts)
	}
	o.rejected = 0
}
