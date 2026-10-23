package operator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRequesterBindingKeepsAdmittedDefaultsAcrossSchemaChanges(t *testing.T) {
	defaults := map[string]any{"timeout": "5m"}
	setSchemaDefaultStripper(t, stripDefaultsFor(defaults))
	s := testSigner()
	obj := plainRequesterHC("replicas: 1")
	obj.Object["spec"].(map[string]any)["timeout"] = "5m"
	created := admitObject(t, obj, nil, "alice", s)
	defaults["priority"] = int64(3)
	served := created.DeepCopy()
	served.Object["spec"].(map[string]any)["priority"] = int64(3)
	if _, ok, reason := verifiedRequester(served, s); !ok {
		t.Fatalf("additive default must preserve the stamp: %s", reason)
	}
	defaults["timeout"] = "10m"
	if _, ok, reason := verifiedRequester(served, s); !ok {
		t.Fatalf("changing an existing default must not change the admitted binding: %s", reason)
	}
	served.Object["spec"].(map[string]any)["timeout"] = "10m"
	if _, ok, _ := verifiedRequester(served, s); ok {
		t.Fatal("a newly current default cannot replace the admitted timeout")
	}
}

func TestLegacyRequesterBindingSurvivesAdditiveSchemaDefault(t *testing.T) {
	setSchemaDefaultStripper(t, stripDefaultsFor(map[string]any{"priority": int64(3)}))
	obj := loadStampedFixture(t)
	obj.Object["spec"].(map[string]any)["priority"] = int64(3)
	if _, ok, reason := verifiedRequester(obj, testSigner()); !ok {
		t.Fatalf("an untouched legacy stamp must survive an additive default: %s", reason)
	}
	obj.Object["spec"].(map[string]any)["priority"] = int64(4)
	if _, ok, _ := verifiedRequester(obj, testSigner()); ok {
		t.Fatal("an added nondefault field must not pass legacy verification")
	}
}

func TestRequesterPolicyBindsFieldsIntroducedAfterTheRelease(t *testing.T) {
	defaults := map[string]any{"priority": int64(3)}
	setSchemaDefaultStripper(t, stripDefaultsFor(defaults))
	obj := plainRequesterHC("replicas: 1")
	obj.Object["spec"].(map[string]any)["priority"] = int64(3)
	created := admitObject(t, obj, nil, "alice", testSigner())
	defaults["priority"], defaults["weight"] = int64(4), int64(1)
	served := created.DeepCopy()
	served.Object["spec"].(map[string]any)["weight"] = int64(1)
	if _, ok, reason := verifiedRequester(served, testSigner()); !ok {
		t.Fatalf("an admitted later default must retain its binding after another schema extension: %s", reason)
	}
	for _, change := range []func(){
		func() { served.Object["spec"].(map[string]any)["priority"] = int64(4) },
		func() {
			a := served.GetAnnotations()
			a[annRequesterFieldsV2] = `["chart","valuesContent"]`
			delete(a, annRequesterSig)
			served.SetAnnotations(a)
		},
	} {
		served = created.DeepCopy()
		served.Object["spec"].(map[string]any)["weight"] = int64(1)
		change()
		if _, ok, _ := verifiedRequester(served, testSigner()); ok {
			t.Fatal("an altered admitted value or authenticated field policy must be refused")
		}
	}
}

func TestRequesterSchemaDefaultsCompareLargeIntegersExactly(t *testing.T) {
	strip, err := schemaDefaultStripperFromCRD([]byte(`{"spec":{"versions":[{"storage":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"priority":{"type":"integer","default":9007199254740993}}}}}}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []int64{9007199254740992, 9007199254740993} {
		projected := strip(map[string]any{"priority": value}).(map[string]any)
		_, retained := projected["priority"]
		if retained != (value != 9007199254740993) {
			t.Errorf("integer %d retained=%v: only the exact schema default may be excluded", value, retained)
		}
	}
}

func TestRequesterNewSigningRequiresExplicitActivation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s, err := ensureRequesterSigner(t.Context(), cs)
	if err != nil {
		t.Fatal(err)
	}
	created := admitObject(t, plainRequesterHC("replicas: 1"), nil, "alice", s)
	if created.GetAnnotations()[annRequesterSigV2] != "" {
		t.Fatal("new-format signing must remain disabled until administrator activation after verifier adoption")
	}
	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(context.Background(), requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec.Data["binding-version"] = []byte(stampBindingV2)
	if _, err := cs.CoreV1().Secrets(webhookNamespace).Update(t.Context(), sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s, err = ensureRequesterSigner(t.Context(), cs)
	if err != nil {
		t.Fatal(err)
	}
	created = admitObject(t, plainRequesterHC("replicas: 2"), nil, "alice", s)
	if created.GetAnnotations()[annRequesterSigV2] == "" {
		t.Fatal("explicit activation must enable the supported versioned signer")
	}
}

func TestRequesterRotationPersistsVerifiersBeforeActivation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	old := keyringFromSecret(t, cs, "old", map[string][]byte{"old": testRotationKey(0)})
	legacy := loadStampedFixture(t)
	info, _, _, _ := requesterFromCR(legacy)
	ann := legacy.GetAnnotations()
	ann[annRequesterSig] = old.sign(requesterBinding(legacy), info)
	legacy.SetAnnotations(ann)
	h := newRequesterSignerHolder()
	h.publish(old)
	writeRequesterSecret(t, cs, "new", map[string][]byte{"new": testRotationKey(1)})
	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(t.Context(), requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	failing := true
	cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New("retention write failed")
		}
		return false, nil, nil
	})
	publishRefreshedSigner(t.Context(), cs, h, sec)
	if h.current() != old {
		t.Fatal("replacement signing must wait until every held verifier is persisted")
	}
	failing = false
	publishRefreshedSigner(t.Context(), cs, h, sec)
	if h.current() == old {
		t.Fatal("the owner must activate the replacement once retention succeeds")
	}
	for rotation := 2; rotation <= 3; rotation++ {
		id := fmt.Sprintf("new-%d", rotation)
		writeRequesterSecret(t, cs, id, map[string][]byte{id: testRotationKey(rotation)})
		sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(t.Context(), requesterKeySecret, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		publishRefreshedSigner(t.Context(), cs, h, sec)
	}
	restarted, err := ensureRequesterSigner(t.Context(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, reason := verifiedRequester(legacy, restarted); !ok {
		t.Fatalf("the untouched legacy object must verify after restart: %s", reason)
	}
}

func TestRequesterActivationRefreshesWithoutKeyRotation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s, err := ensureRequesterSigner(t.Context(), cs)
	if err != nil {
		t.Fatal(err)
	}
	h := newRequesterSignerHolder()
	h.publish(s)
	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(t.Context(), requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec.Data[requesterBindingVersionField] = []byte(stampBindingV2)
	publishRefreshedSigner(t.Context(), cs, h, sec)
	if h.current().bindingVersion != stampBindingV2 {
		t.Fatal("binding activation must refresh even when the key and ring are unchanged")
	}

}

func TestRequesterRotationRejectsReusedKeyIdentifier(t *testing.T) {
	cs := fake.NewSimpleClientset()
	old := keyringFromSecret(t, cs, "stable", map[string][]byte{"stable": testRotationKey(0)})
	h := newRequesterSignerHolder()
	h.publish(old)
	writeRequesterSecret(t, cs, "stable", map[string][]byte{"stable": testRotationKey(1)})
	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(t.Context(), requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	publishRefreshedSigner(t.Context(), cs, h, sec)
	if h.current() != old {
		t.Fatal("reusing a retained key ID for new material must not discard the old verifier")
	}
}
