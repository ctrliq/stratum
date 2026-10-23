package token

import (
	"io"

	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	sigKey = clusterinfo.JWSSignatureKeyPrefix + "abcdef"
	kcKey  = clusterinfo.KubeConfigKey
)

// fakeConfigMaps is a hand-rolled configMapAPI. It exists instead of
// k8s.io/client-go/kubernetes/fake because that package would add a module to
// the require set, and this package adds none — see configMapAPI's doc comment.
// It also buys precise control over the conflict path, which the fake makes
// awkward.
type fakeConfigMaps struct {
	cm      *corev1.ConfigMap // nil = not found
	gets    int
	updates int
	creates int
	// conflicts is how many Updates to reject with a Conflict before accepting.
	conflicts int
	// getErr, when set, is returned by every Get.
	getErr error
	// failFirst is how many leading Gets fail with getErr before clean reads
	// resume. Only meaningful together with getErr.
	failFirst int
	// signAfterGets simulates the bootstrapsigner: once Get has been called this
	// many times, the signature appears.
	signAfterGets int
	// loseCreateRaceWith, when set, makes the object appear between our Get
	// (NotFound) and our Create (AlreadyExists) — another writer won the race.
	loseCreateRaceWith map[string]string
	// hangUntilCtxDone models a stalled apiserver: Get blocks until the caller's
	// context is cancelled, which is what a real typed client does when the
	// connection is accepted but never answered.
	hangUntilCtxDone bool
}

func (f *fakeConfigMaps) Get(ctx context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	f.gets++
	if f.hangUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.getErr != nil && (f.failFirst == 0 || f.gets <= f.failFirst) {
		return nil, f.getErr
	}
	if f.signAfterGets > 0 && f.gets >= f.signAfterGets && f.cm != nil {
		f.cm.Data[sigKey] = "the-signature"
	}
	if f.cm == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return f.cm.DeepCopy(), nil
}

func (f *fakeConfigMaps) Create(_ context.Context, cm *corev1.ConfigMap, _ metav1.CreateOptions) (*corev1.ConfigMap, error) {
	f.creates++
	if f.loseCreateRaceWith != nil {
		f.cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
			Data:       f.loseCreateRaceWith,
		}
		f.loseCreateRaceWith = nil
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, cm.Name)
	}
	if f.cm != nil {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, cm.Name)
	}
	f.cm = cm.DeepCopy()
	return f.cm, nil
}

func (f *fakeConfigMaps) Update(_ context.Context, cm *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	f.updates++
	if f.conflicts > 0 {
		f.conflicts--
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, nil)
	}
	f.cm = cm.DeepCopy()
	return f.cm, nil
}

func existing(data map[string]string) *fakeConfigMaps {
	return &fakeConfigMaps{cm: &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
		Data:       data,
	}}
}

// TestPutClusterInfoKubeconfigPreservesUnknownKeys is the companion
// regression test for signed cluster-info.
//
// A wholesale replace of the ConfigMap would destroy anything else living
// in that object on every republish. It is a shared object; this function
// owns exactly one key.
//
// The jws-* keys are the one deliberate exception; see the test below.
func TestPutClusterInfoKubeconfigPreservesUnknownKeys(t *testing.T) {
	f := existing(map[string]string{
		kcKey:           "old kubeconfig",
		"unrelated-key": "written by something else",
	})

	if err := putClusterInfoKubeconfig(context.Background(), f, "new kubeconfig", nil); err != nil {
		t.Fatal(err)
	}

	if got := f.cm.Data[kcKey]; got != "new kubeconfig" {
		t.Errorf("kubeconfig = %q, want it updated", got)
	}
	if got := f.cm.Data["unrelated-key"]; got != "written by something else" {
		t.Errorf("unrelated key erased (%q); this function owns only data.kubeconfig", got)
	}
}

// TestPutClusterInfoKubeconfigDropsStaleSignaturesOnChange is the counterpart to
// the test above, and the two together state the whole rule.
//
// Preserving a signature across a CONTENT change is worse than deleting it: the
// signature covers the old kubeconfig, so a joiner recomputes the MAC, mismatches,
// and hits the "possible MITM" path — which DiscoverCA deliberately never
// retries, so the join aborts. Deleting it yields NoSignatureError, which IS
// retried until the signer republishes. RepinCA runs this twice per CA rotation
// (--phase=trust-new and --phase=finalize), right after the controller-manager
// has been bounced.
func TestPutClusterInfoKubeconfigDropsStaleSignaturesOnChange(t *testing.T) {
	f := existing(map[string]string{
		kcKey:           "old kubeconfig",
		sigKey:          "signature-over-the-OLD-kubeconfig",
		"unrelated-key": "must survive",
	})

	if err := putClusterInfoKubeconfig(context.Background(), f, "new kubeconfig", nil); err != nil {
		t.Fatal(err)
	}

	if got, ok := f.cm.Data[sigKey]; ok {
		t.Errorf("stale signature %q survived a content change; a joiner would read it as a MITM and abort", got)
	}
	if f.cm.Data["unrelated-key"] != "must survive" {
		t.Error("an unrelated key was collateral damage; only jws-* may be dropped")
	}
}

// TestPutClusterInfoKubeconfigCreatesWhenAbsent covers the fresh-cluster path.
func TestPutClusterInfoKubeconfigCreatesWhenAbsent(t *testing.T) {
	f := &fakeConfigMaps{}
	if err := putClusterInfoKubeconfig(context.Background(), f, "kubeconfig", nil); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.cm.Data[kcKey] != "kubeconfig" {
		t.Errorf("creates = %d, data = %v", f.creates, f.cm.Data)
	}
}

// TestPutClusterInfoKubeconfigHandlesCreateRace exercises the
// NotFound -> Create -> AlreadyExists -> Conflict conversion. Without a fake
// that can lose the create race, deleting that whole branch leaves the suite
// green.
func TestPutClusterInfoKubeconfigHandlesCreateRace(t *testing.T) {
	f := &fakeConfigMaps{loseCreateRaceWith: map[string]string{
		kcKey:  "written by the racer",
		sigKey: "the-racer-signature",
	}}

	if err := putClusterInfoKubeconfig(context.Background(), f, "ours", nil); err != nil {
		t.Fatalf("lost the create race and gave up: %v", err)
	}
	if f.cm.Data[kcKey] != "ours" {
		t.Errorf("kubeconfig = %q, want the retry to have applied ours", f.cm.Data[kcKey])
	}
}

// TestPutClusterInfoKubeconfigSkipsNoOpUpdate: `stratum token create` on an
// unchanged cluster must not churn the object. Every write invalidates the
// signatures until the signer recomputes them, so a no-op write is a gratuitous
// window in which existing joiners fail.
func TestPutClusterInfoKubeconfigSkipsNoOpUpdate(t *testing.T) {
	f := existing(map[string]string{kcKey: "same kubeconfig", sigKey: "the-signature"})

	if err := putClusterInfoKubeconfig(context.Background(), f, "same kubeconfig", nil); err != nil {
		t.Fatal(err)
	}
	if f.updates != 0 {
		t.Errorf("issued %d update(s) for an unchanged kubeconfig; that needlessly invalidates live signatures", f.updates)
	}
}

// TestPutClusterInfoKubeconfigRetriesOnConflict: the bootstrapsigner writes this
// same object concurrently, so a Conflict is expected traffic, not an error.
func TestPutClusterInfoKubeconfigRetriesOnConflict(t *testing.T) {
	f := existing(map[string]string{kcKey: "old", "unrelated-key": "keep me"})
	f.conflicts = 2

	if err := putClusterInfoKubeconfig(context.Background(), f, "new", nil); err != nil {
		t.Fatalf("gave up on a conflict the signer routinely causes: %v", err)
	}
	if f.updates != 3 {
		t.Errorf("updates = %d, want 3 (two conflicts then success)", f.updates)
	}
	if f.cm.Data[kcKey] != "new" {
		t.Errorf("kubeconfig = %q, want the retry to have applied ours", f.cm.Data[kcKey])
	}
	if f.cm.Data["unrelated-key"] != "keep me" {
		t.Error("unrelated key lost across the retry")
	}
}

// TestWaitForClusterInfoSignature covers the control-plane post-condition.
func TestWaitForClusterInfoSignature(t *testing.T) {
	restoreInterval, restoreTimeout := waitForSigningInterval, waitForSigningTimeout
	waitForSigningInterval, waitForSigningTimeout = 5*time.Millisecond, 40*time.Millisecond
	defer func() { waitForSigningInterval, waitForSigningTimeout = restoreInterval, restoreTimeout }()

	t.Run("already signed", func(t *testing.T) {
		f := existing(map[string]string{kcKey: "kc", sigKey: "sig"})
		if err := waitForClusterInfoSignature(context.Background(), io.Discard, f, "abcdef"); err != nil {
			t.Errorf("signed cluster-info rejected: %v", err)
		}
	})

	// The signature is patched in a moment after the ConfigMap is written, so the
	// post-condition must poll rather than read once.
	t.Run("signature appears while polling", func(t *testing.T) {
		f := existing(map[string]string{kcKey: "kc"})
		f.signAfterGets = 3
		if err := waitForClusterInfoSignature(context.Background(), io.Discard, f, "abcdef"); err != nil {
			t.Errorf("gave up before the signer caught up: %v", err)
		}
	})

	// The budget must bound the API CALL, not just the loop. Deriving a
	// deadline from a wall clock and testing it only after Get returns leaves a
	// stalled apiserver blocking inside Get forever, so `stratum token create`
	// hangs and the 30s hard-fail never fires.
	t.Run("a stalled apiserver is bounded by the budget", func(t *testing.T) {
		f := &fakeConfigMaps{hangUntilCtxDone: true}
		done := make(chan error, 1)
		go func() { done <- waitForClusterInfoSignature(context.Background(), io.Discard, f, "abcdef") }()

		// The property is "it terminates", asserted by the select alone. An
		// elapsed-time bound was tried here and removed: with the budget shortened
		// to milliseconds it flaked on a loaded machine, and it added nothing the
		// select does not already prove. A flaky assertion in a concurrency test
		// buys noise, not confidence.
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a stalled apiserver was reported as signed")
			}
		// 2s against a millisecond test budget: wide enough never to flake, tight
		// enough that a context built from the WRONG budget (say a hardcoded 5s)
		// still trips it. A 10s watchdog proved only "it terminates eventually".
		case <-time.After(2 * time.Second):
			t.Fatal("waitForClusterInfoSignature hung, or ran far past its budget: the context must carry waitForSigningTimeout")
		}
	})

	t.Run("persistent API error does not blame the signer", func(t *testing.T) {
		f := &fakeConfigMaps{getErr: apierrors.NewForbidden(
			schema.GroupResource{Resource: "configmaps"}, "cluster-info", errors.New("nope"))}
		err := waitForClusterInfoSignature(context.Background(), io.Discard, f, "abcdef")
		if err == nil {
			t.Fatal("a persistently unreadable cluster-info was reported as signed")
		}
		if strings.Contains(err.Error(), "--controllers=") {
			t.Errorf("sent the operator to restart components for an API failure:\n%v", err)
		}
		if !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("the real cause was dropped: %v", err)
		}
	})

	// One blip inside the budget must not suppress the remediation: a single
	// apiserver hiccup is unremarkable on exactly the freshly-upgraded cluster
	// that needs the --controllers instruction.
	t.Run("transient API error does not mask a genuinely unsigned cluster", func(t *testing.T) {
		f := existing(map[string]string{kcKey: "kc"})
		f.getErr = apierrors.NewServiceUnavailable("apiserver still coming up")
		f.failFirst = 1
		err := waitForClusterInfoSignature(context.Background(), io.Discard, f, "abcdef")
		if err == nil {
			t.Fatal("unsigned cluster-info accepted")
		}
		if !strings.Contains(err.Error(), version.ControllerManagerControllersFlag) {
			t.Errorf("one early blip suppressed the signer remediation:\n%v", err)
		}
	})

	// The budget's context outlives the wall clock by one interval, but nothing
	// guarantees the goroutine runs again inside that interval. A stall between
	// the deadline check and the wait (here, a progress line written to a slow
	// terminal) lets the context expire first; that must still end in the
	// remediation, not a bare context.DeadlineExceeded.
	t.Run("a stall after the deadline check still ends in the remediation", func(t *testing.T) {
		f := existing(map[string]string{kcKey: "kc"})
		w := stallingWriter{d: waitForSigningTimeout + 3*waitForSigningInterval}
		err := waitForClusterInfoSignature(context.Background(), w, f, "abcdef")
		if err == nil {
			t.Fatal("unsigned cluster-info accepted")
		}
		if !strings.Contains(err.Error(), version.ControllerManagerControllersFlag) {
			t.Errorf("the budget's context expiring first dropped the remediation:\n%v", err)
		}
	})

	t.Run("a caller's cancellation is still reported as such", func(t *testing.T) {
		f := existing(map[string]string{kcKey: "kc"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := waitForClusterInfoSignature(ctx, stallingWriter{}, f, "abcdef")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled caller got %v, want context.Canceled", err)
		}
	})

	t.Run("never signed fails with the remediation", func(t *testing.T) {
		f := existing(map[string]string{kcKey: "kc"})
		err := waitForClusterInfoSignature(context.Background(), io.Discard, f, "abcdef")
		if err == nil {
			t.Fatal("unsigned cluster-info accepted; the token handed out would be unusable")
		}
		for _, want := range []string{
			version.ControllerManagerControllersFlag,
			version.ControllerManagerManifest,
			"EVERY control-plane",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("remediation missing %q\ngot: %v", want, err)
			}
		}
	})
}

// stallingWriter blocks each write for d, the way a slow terminal or a full
// pipe would.
type stallingWriter struct{ d time.Duration }

func (w stallingWriter) Write(p []byte) (int, error) {
	time.Sleep(w.d)
	return len(p), nil
}
