// The pruner's read-modify-write updates carry the resourceVersion they read.
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

// The entries' bytes are opaque here: an entry lives exactly while its bootstrap token does.
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

// testLiveTokenSecret creates the bootstrap-token Secret the apiserver deletes on expiry.
func testLiveTokenSecret(id string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + id, Namespace: "kube-system"},
	}
}

func TestExpiredGrantEntryIsPrunedWhileLiveOnesStay(t *testing.T) {
	cs := fake.NewSimpleClientset(
		testGrantSecret(),
		testLiveTokenSecret("abc123"),
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

// An update carrying another version conflicts; a dropped version would pass the other tests.
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

func TestGrantPrunerRetriesAConflictingUpdate(t *testing.T) {
	setup := func() (*fake.Clientset, *[]string) {
		cs := fake.NewSimpleClientset(
			testGrantSecret(),
			testLiveTokenSecret("abc123"),
			testLiveTokenSecret("ghi789"),
		)
		carried := versionedGrantSecret(cs, func(call int, stored *corev1.Secret) bool {
			if call != 1 {
				return false
			}
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

func TestGrantPrunerLeavesAnAbsentBundleAbsent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	reconcileGrantEntries(context.Background(), cs)
	if _, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), "stratum-certs", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get stratum-certs after pruning nothing: %v, want NotFound", err)
	}
}

// The pass runs before the first tick, so the first pass must already prune.
func TestReconcilePassPrunesExpiredGrantEntries(t *testing.T) {
	cs := fake.NewSimpleClientset(
		testGrantSecret(),
		testLiveTokenSecret("abc123"),
	)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			runLeaderLoop(ctx, testInterval, false, false, func(context.Context) {}, nil, nil, cleanupStepFor(cs))
		}()
		// Once every goroutine in the bubble is durably blocked, the first pass is done.
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
