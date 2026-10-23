package operator

import (
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

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
