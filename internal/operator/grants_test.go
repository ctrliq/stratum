// grants_test.go — the operator's grant pruner: the stratum-certs Secret
// carries one ciphertext entry per live grant keyed by token ID beside the
// legacy entry, and the operator's pass — riding its reconcile loop, through
// internal/ha's rule — drops the entries whose bootstrap token is gone, with
// read-modify-write updates that carry the resourceVersion they read.
package operator

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testGrantSecret is kube-system/stratum-certs as a keyed mint leaves it under
// the multi-entry policy: the legacy `certs` entry and the at-rest-key proof
// beside one ciphertext entry per grant, and one owner reference per
// bootstrap-token Secret, at the resourceVersion the apiserver last stored.
// The entries' bytes are opaque here: which entries live is decided by the
// presence of the bootstrap token, never by opening the ciphertext.
func testGrantSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "stratum-certs",
			Namespace:       "kube-system",
			ResourceVersion: "7",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Secret", Name: "bootstrap-token-abc123", UID: "uid-abc"},
				{APIVersion: "v1", Kind: "Secret", Name: "bootstrap-token-def456", UID: "uid-def"},
			},
		},
		Data: map[string][]byte{
			"certs":                []byte("legacy bundle"),
			"encryption-key-proof": []byte("proof sealed under the at-rest key"),
			"certs-abc123":         []byte("ciphertext under the live token's certificate-key"),
			"certs-def456":         []byte("ciphertext under the expired token's certificate-key"),
		},
	}
}

// testLiveTokenSecret creates kube-system/bootstrap-token-<id>, the Secret the
// apiserver deletes when a bootstrap token expires or is revoked.
func testLiveTokenSecret(id string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + id, Namespace: "kube-system"},
	}
}

// TestExpiredGrantEntryIsPrunedWhileLiveOnesStay: the grant pruner drops
// exactly the stratum-certs entries whose bootstrap token is gone — an expired
// or revoked grant — and leaves the live grants' entries, the legacy `certs`
// entry and every owner reference alone. A fake clientset runs no Kubernetes
// garbage collection, so the pruner itself must consult the bootstrap-token
// Secrets rather than wait for the apiserver to tidy anything.
func TestExpiredGrantEntryIsPrunedWhileLiveOnesStay(t *testing.T) {
	cs := fake.NewSimpleClientset(
		testGrantSecret(),
		testLiveTokenSecret("abc123"),
		// bootstrap-token-def456 is gone: its token expired and the apiserver
		// garbage-collected its Secret.
	)
	reconcileGrantEntries(context.Background(), cs)
	sec, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), "stratum-certs", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get stratum-certs after the prune: %v", err)
	}
	if _, ok := sec.Data["certs-def456"]; ok {
		t.Error("the expired token's grant entry is still published; the pruner must drop it")
	}
	for _, entry := range []string{"certs-abc123", "certs", "encryption-key-proof"} {
		if _, ok := sec.Data[entry]; !ok {
			t.Errorf("the prune dropped %q; only the expired grant's entry may go", entry)
		}
	}
	want := map[string]bool{"uid-abc": true, "uid-def": true}
	for _, r := range sec.OwnerReferences {
		delete(want, string(r.UID))
	}
	if len(want) != 0 {
		t.Errorf("owner references %v lost an owner the prune had no business removing", sec.OwnerReferences)
	}
}

// versionedGrantSecret makes the fake apiserver keep stratum-certs'
// resourceVersion the way the real one does. The fake tracker stores whatever
// it is handed, so a write that dropped the version it read would pass every
// other test. An update carrying the stored version is stored under the next
// one, an update carrying another version conflicts, and one carrying none is
// unconditional — the apiserver's rule for an update with no precondition,
// and exactly the write that clobbers a concurrent one. concurrent runs before
// each update's check with the stored Secret, so a test can land a mint
// between the pruner's read and its write; it reports whether it changed
// anything. The returned slice records the version each update carried.
// Reactors run under the fake's lock, so this one reaches the store through
// the tracker only.
func versionedGrantSecret(cs *fake.Clientset, concurrent func(call int, stored *corev1.Secret) bool) *[]string {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	next := func(v string) string {
		n, _ := strconv.Atoi(v)
		return strconv.Itoa(n + 1)
	}
	carried := []string{}
	cs.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sec, ok := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if !ok || sec.Name != "stratum-certs" {
			return false, nil, nil
		}
		carried = append(carried, sec.ResourceVersion)
		obj, err := cs.Tracker().Get(gvr, "kube-system", "stratum-certs")
		if err != nil {
			return true, nil, err
		}
		stored := obj.(*corev1.Secret).DeepCopy()
		if concurrent(len(carried), stored) {
			stored.ResourceVersion = next(stored.ResourceVersion)
			if err := cs.Tracker().Update(gvr, stored, "kube-system"); err != nil {
				return true, nil, err
			}
		}
		if sec.ResourceVersion != "" && sec.ResourceVersion != stored.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "stratum-certs",
				errors.New("the object has been modified; please apply your changes to the latest version and try again"))
		}
		out := sec.DeepCopy()
		out.ResourceVersion = next(stored.ResourceVersion)
		if err := cs.Tracker().Update(gvr, out, "kube-system"); err != nil {
			return true, nil, err
		}
		return true, out, nil
	})
	return &carried
}

// TestGrantPrunerRetriesAConflictingUpdate: a `token create --control-plane`
// can land between the pruner's read and its write, appending its own owner
// reference and grant entry. The pruner's update is a read-modify-write that
// carries the resourceVersion it read, so that write conflicts, and the pruner
// must re-read and converge instead of failing the pass or clobbering the
// concurrent mint's owner.
func TestGrantPrunerRetriesAConflictingUpdate(t *testing.T) {
	setup := func() (*fake.Clientset, *[]string) {
		cs := fake.NewSimpleClientset(
			testGrantSecret(),
			testLiveTokenSecret("abc123"),
			// The concurrent mint's token Secret: created before its
			// stratum-certs write lands, as every token mint orders it.
			testLiveTokenSecret("ghi789"),
		)
		carried := versionedGrantSecret(cs, func(call int, stored *corev1.Secret) bool {
			if call != 1 {
				return false
			}
			// The concurrent mint lands its owner reference and grant entry
			// while the pruner holds its (now stale) read.
			stored.OwnerReferences = append(stored.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "Secret", Name: "bootstrap-token-ghi789", UID: "uid-ghi"})
			stored.Data["certs-ghi789"] = []byte("the concurrent grant")
			return true
		})
		return cs, carried
	}

	t.Run("the retry keeps the concurrent mint", func(t *testing.T) {
		cs, carried := setup()
		reconcileGrantEntries(context.Background(), cs)
		sec, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), "stratum-certs", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get stratum-certs after the retried prune: %v", err)
		}
		if _, ok := sec.Data["certs-def456"]; ok {
			t.Error("the expired token's grant entry survived the prune")
		}
		if _, ok := sec.Data["certs-ghi789"]; !ok {
			t.Error("the concurrent mint's grant entry is gone: the pruner wrote from a stale read instead of re-reading after the conflict")
		}
		uids := map[string]bool{}
		for _, r := range sec.OwnerReferences {
			uids[string(r.UID)] = true
		}
		for _, uid := range []string{"uid-abc", "uid-def", "uid-ghi"} {
			if !uids[uid] {
				t.Errorf("owner reference %s missing after the retried prune: %v", uid, sec.OwnerReferences)
			}
		}
		if want := []string{"7", "8"}; !slices.Equal(*carried, want) {
			t.Errorf("the pruner's updates carried resourceVersions %q, want %q: the version read, then the re-read one after the conflict", *carried, want)
		}
	})

	t.Run("the store catches a write that drops the version it read", func(t *testing.T) {
		// Without this, the test above could pass against a fake that accepts
		// anything: a pruner that drops the version it read must lose the
		// concurrent mint's grant here.
		cs, _ := setup()
		ctx := context.Background()
		secrets := cs.CoreV1().Secrets("kube-system")
		cur, err := secrets.Get(ctx, "stratum-certs", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cur.ResourceVersion = ""
		delete(cur.Data, "certs-def456")
		if _, err := secrets.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("the blind write: %v", err)
		}
		sec, err := secrets.Get(ctx, "stratum-certs", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := sec.Data["certs-ghi789"]; ok {
			t.Fatal("the concurrent grant survived a write that carried no resourceVersion: the versioned store cannot tell a blind write from a compare-and-swap")
		}
	})
}

// TestGrantPrunerLeavesACleanSecretAlone: a pass with nothing to prune — every
// grant's token still live, or no grant entries at all — writes nothing. The
// pruner rides a reconcile loop that ticks every interval, and rewriting
// stratum-certs on every pass would bump the resourceVersion of the cluster's
// CA-key ciphertext without changing anything.
func TestGrantPrunerLeavesACleanSecretAlone(t *testing.T) {
	allLive := fake.NewSimpleClientset(
		testGrantSecret(),
		testLiveTokenSecret("abc123"),
		testLiveTokenSecret("def456"),
	)
	legacyOnly := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum-certs", Namespace: "kube-system"},
		Data:       map[string][]byte{"certs": []byte("legacy bundle")},
	})
	for name, cs := range map[string]*fake.Clientset{
		"every token still live": allLive,
		"no grant entries":       legacyOnly,
	} {
		t.Run(name, func(t *testing.T) {
			updates := 0
			cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				updates++
				return false, nil, nil
			})
			reconcileGrantEntries(context.Background(), cs)
			if updates != 0 {
				t.Errorf("secret updates = %d, want 0: nothing to prune, nothing to write", updates)
			}
		})
	}
}

// TestGrantPrunerLeavesAnAbsentBundleAbsent: a cluster with no stratum-certs
// Secret — nothing minted yet, or the bundle deleted deliberately — gains none
// from the pruner.
func TestGrantPrunerLeavesAnAbsentBundleAbsent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	reconcileGrantEntries(context.Background(), cs)
	if _, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), "stratum-certs", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get stratum-certs after pruning nothing: %v, want NotFound", err)
	}
}

// TestReconcilePassPrunesExpiredGrantEntries: the pruning is wired into the
// reconcile pass, not just available to it. Run binds the loop's cleanup step
// with cleanupStepFor; driving the leader loop with that step and a fake
// clientset must drop the expired token's entry in the first pass — the pass
// runs before the first tick — and keep the live grant's entry and the legacy
// one. Without this wiring an expired or revoked token's bundle-sized entry
// stays published until the next mint drops it.
func TestReconcilePassPrunesExpiredGrantEntries(t *testing.T) {
	cs := fake.NewSimpleClientset(
		testGrantSecret(),
		testLiveTokenSecret("abc123"),
		// bootstrap-token-def456 is gone: its token expired and the apiserver
		// garbage-collected its Secret.
	)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			// The pass shape Run uses without a manifests directory: the
			// HelmChart step and the cleanup step, which carries the pruner.
			runLeaderLoop(ctx, testInterval, false, false, func(context.Context) {}, nil, nil, cleanupStepFor(cs))
		}()
		// Once every goroutine in the bubble is durably blocked, the first
		// pass — grants included — has finished: the loop then sits in the
		// ticker select.
		synctest.Wait()
		cancel()
		<-loopDone
	})
	sec, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), "stratum-certs", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get stratum-certs after the pass: %v", err)
	}
	if _, ok := sec.Data["certs-def456"]; ok {
		t.Error("the reconcile pass left the expired token's grant entry published: the loop never prunes")
	}
	for _, entry := range []string{"certs-abc123", "certs"} {
		if _, ok := sec.Data[entry]; !ok {
			t.Errorf("the pass dropped %q; only the expired grant's entry may go", entry)
		}
	}
}
