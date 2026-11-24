package operator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRequesterSignerSignVerify(t *testing.T) {
	s := testSigner()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a", "system:authenticated"}}

	sig := s.sign("uid-1", info)
	if !s.verify("uid-1", info, sig) {
		t.Fatal("a signature must verify against its own inputs")
	}
	if s.verify("uid-2", info, sig) {
		t.Error("signature must not verify against a different object UID")
	}
	if s.verify("uid-1", requesterInfo{Username: "bob"}, sig) {
		t.Error("signature must not verify against a different identity")
	}
	a := s.sign("uid-1", requesterInfo{Username: "a", Groups: []string{"b"}})
	b := s.sign("uid-1", requesterInfo{Username: "ab", Groups: []string{}})
	if a == b {
		t.Error("length-prefixing must keep distinct field splits from colliding")
	}
	// A different key produces a different, non-verifying signature.
	other := &requesterSigner{key: []byte("a-completely-different-hmac-key-value")}
	if other.verify("uid-1", info, sig) {
		t.Error("a signature must not verify under a different key")
	}
}

func TestEnsureRequesterSigner(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()

	s1, err := ensureRequesterSigner(ctx, cs)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(s1.key) != requesterKeyBytes {
		t.Fatalf("key length = %d, want %d", len(s1.key), requesterKeyBytes)
	}
	// The key is persisted to a kube-system Secret.
	if _, err := cs.CoreV1().Secrets(webhookNamespace).Get(ctx, requesterKeySecret, metav1.GetOptions{}); err != nil {
		t.Fatalf("signing Secret not created: %v", err)
	}
	// Idempotent: a second call returns the SAME key (so every operator agrees).
	s2, err := ensureRequesterSigner(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	if string(s1.key) != string(s2.key) {
		t.Error("ensureRequesterSigner must return the same persisted key on subsequent calls")
	}
}

// literalStamp stands for stamps already stored on tenant HelmCharts; never regenerate.
const literalStamp = "qGR5/gX3C+ZeEqoAFoBXibGE60aHXstXnWGcoofZT2g"

func TestRequesterStamp_AStampWrittenTodayStillVerifies(t *testing.T) {
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a", "system:authenticated"}}
	hc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata": map[string]any{
			"namespace": "team-a",
			"name":      "app",
			"uid":       "uid-1",
			"annotations": map[string]any{
				annRequester:       info.Username,
				annRequesterUID:    info.UID,
				annRequesterGroups: `["team-a","system:authenticated"]`,
				annRequesterSig:    literalStamp,
			},
		},
	}}

	if !testSigner().verify(requesterBinding(hc), info, literalStamp) {
		t.Error("today's literal stamp must verify with today's key and binding")
	}
	got, authentic, reason := verifiedRequester(hc, testSigner())
	if !authentic {
		t.Fatalf("a stamp written by this release must still verify; refused: %s", reason)
	}
	if got.Username != info.Username || got.UID != info.UID {
		t.Errorf("verifiedRequester returned identity %+v, want %+v", got, info)
	}
	if !slices.Equal(got.Groups, info.Groups) {
		t.Errorf("verifiedRequester returned groups %v, want %v", got.Groups, info.Groups)
	}
}

// Reads the golden fixture, written once and never regenerated.
func loadStampedFixture(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "stamped-helmchart.json"))
	if err != nil {
		t.Fatalf("read golden stamp fixture: %v", err)
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, obj); err != nil {
		t.Fatalf("decode golden stamp fixture: %v", err)
	}
	return obj
}

// fixtureStamp stands for the legacy stored stamps; never regenerate it or the fixture.
const fixtureStamp = "9cHHuoCJeU+pN435a4q1fH6X97EShVgi2yBjztE5W6c"

func TestRequesterStampFromThisReleaseStillVerifies(t *testing.T) {
	hc := loadStampedFixture(t)

	bound := requesterInfo{Username: "alice", UID: "alice-uid", Groups: []string{"system:authenticated"}}
	ann := hc.GetAnnotations()
	if ann[annRequester] != bound.Username || ann[annRequesterUID] != bound.UID {
		t.Fatalf("golden fixture records identity %q/%q, want %q/%q", ann[annRequester], ann[annRequesterUID], bound.Username, bound.UID)
	}
	if ann[annRequesterGroups] != `["system:authenticated"]` || ann[annRequesterSig] != fixtureStamp {
		t.Fatalf("golden fixture groups/signature were regenerated: groups=%s sig=%s", ann[annRequesterGroups], ann[annRequesterSig])
	}
	if nsName := hc.GetNamespace() + "/" + hc.GetName(); nsName != "team-a/app" {
		t.Fatalf("golden fixture binds %s, want team-a/app", nsName)
	}
	if hc.Object["spec"] == nil {
		t.Fatal("golden fixture records no spec, so the stamp binds no desired state")
	}

	got, authentic, reason := verifiedRequester(hc, testSigner())
	if !authentic {
		t.Fatalf("a stamp written by this release must still verify; refused: %s", reason)
	}
	if got.Username != bound.Username || got.UID != bound.UID || !slices.Equal(got.Groups, bound.Groups) {
		t.Errorf("verifiedRequester returned identity %+v, want %+v", got, bound)
	}

	for _, tc := range []struct {
		name   string
		tamper func(o *unstructured.Unstructured)
	}{
		{"username replaced", func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[annRequester] = "mallory"
			o.SetAnnotations(a)
		}},
		{"uid replaced", func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[annRequesterUID] = "mallory-uid"
			o.SetAnnotations(a)
		}},
		{"groups extended", func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[annRequesterGroups] = `["system:authenticated","team-a"]`
			o.SetAnnotations(a)
		}},
		{"spec changed", func(o *unstructured.Unstructured) {
			o.Object["spec"].(map[string]interface{})["valuesContent"] = "replicas: 2"
		}},
		{"namespace changed", func(o *unstructured.Unstructured) { o.SetNamespace("team-b") }},
		{"name changed", func(o *unstructured.Unstructured) { o.SetName("other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := hc.DeepCopy()
			tc.tamper(tampered)
			if _, ok, _ := verifiedRequester(tampered, testSigner()); ok {
				t.Errorf("%s: tampered stamp verified; the binding must reject it", tc.name)
			}
		})
	}
}

func TestLoadRequesterSigner_FailsClosedWhenCancelled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("*", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver down"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan *requesterSigner, 1)
	go func() { done <- loadRequesterSignerWithRetry(ctx, cs) }()

	select {
	case s := <-done:
		if s != nil {
			t.Fatal("a cancelled first load must return nil (fail closed), not a signer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loadRequesterSignerWithRetry did not return within 5s of a cancelled context; the retry loop stopped watching it")
	}
}

func testRotationKey(n int) []byte {
	k := make([]byte, requesterKeyBytes)
	copy(k, "rotation-test-key-")
	k[requesterKeyBytes-4], k[requesterKeyBytes-3], k[requesterKeyBytes-2], k[requesterKeyBytes-1] =
		byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return k
}

// Writes the signing Secret the way the migration leaves it and loads a signer.
func keyringFromSecret(t *testing.T, cs kubernetes.Interface, currentID string, keys map[string][]byte) *requesterSigner {
	t.Helper()
	keyring := map[string]string{}
	for id, key := range keys {
		keyring[id] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.Marshal(keyring)
	if err != nil {
		t.Fatalf("encode retained-key map: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      requesterKeySecret,
			Namespace: webhookNamespace,
			Labels:    map[string]string{group + "/managed-by": fieldManager},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			requesterKeyField:            keys[currentID],
			requesterKeysField:           raw,
			requesterBindingVersionField: []byte(stampBindingV2),
		},
	}
	secrets := cs.CoreV1().Secrets(webhookNamespace)
	if _, err := secrets.Create(context.Background(), sec, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if _, err := secrets.Update(context.Background(), sec, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update signing Secret: %v", err)
		}
	} else if err != nil {
		t.Fatalf("create signing Secret: %v", err)
	}
	s, err := ensureRequesterSigner(context.Background(), cs)
	if err != nil {
		t.Fatalf("load signer from keyed Secret: %v", err)
	}
	return s
}

// stampedV2 builds a HelmChart carrying the full migration stamp set.
func stampedV2(ns, name, uid string, info requesterInfo, s *requesterSigner, keyID string, spec map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata":   map[string]any{"namespace": ns, "name": name, "uid": uid},
		"spec":       spec,
	}}
	groups, _ := json.Marshal(info.Groups)
	o.SetAnnotations(map[string]string{
		annRequester:         info.Username,
		annRequesterUID:      info.UID,
		annRequesterGroups:   string(groups),
		annRequesterSig:      s.sign(requesterBinding(o), info),
		annRequesterSigV2:    s.signV2(bindingV2(o), info),
		annRequesterFieldsV2: requesterFieldPolicy(o),
		annRequesterKeyID:    keyID,
	})
	return o
}

func TestRequesterStampV2Roundtrip(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1)})
	// Unsorted, with a duplicate: the v2 MAC must cover the canonical group set.
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"system:authenticated", "team-a", "team-a"}}

	if stampBindingV2 != "v2-hmac-sha256" {
		t.Fatalf("stampBindingV2 = %q, want the pinned version+algorithm header v2-hmac-sha256", stampBindingV2)
	}
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1", "kind": "HelmChart",
		"metadata": map[string]any{"namespace": "team-a", "name": "app", "uid": "uid-1"},
		"spec":     map[string]any{"chart": "app"},
	}}
	mac := hmac.New(sha256.New, testRotationKey(1))
	writeField(mac, stampBindingV2)
	writeField(mac, requesterBinding(o))
	writeField(mac, info.Username)
	writeField(mac, info.UID)
	groupsJSON, _ := json.Marshal(canonicalGroups(info.Groups))
	writeField(mac, string(groupsJSON))
	if got := s.signV2(requesterBinding(o), info); got != base64.RawStdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Errorf("signV2 = %q, want the MAC over the length-prefixed version header followed by the unchanged legacy fields (%q)", got, requesterBinding(o))
	}

	stamped := stampedV2("team-a", "app", "uid-1", info, s, "k1", map[string]any{"chart": "app"})
	ann := stamped.GetAnnotations()
	if ann[annRequesterKeyID] != "k1" || ann[annRequesterSigV2] == "" || ann[annRequesterSig] == "" {
		t.Fatalf("a migrated stamp must carry the key identifier and BOTH signatures; annotations = %v", ann)
	}
	if ann[annRequesterSig] == ann[annRequesterSigV2] {
		t.Fatal("the v2 and legacy signatures must differ — the version header is inside the MAC")
	}
	if _, ok, reason := verifiedRequester(stamped, s); !ok {
		t.Fatalf("a migrated stamp must verify: %s", reason)
	}
	onlyV2 := stamped.DeepCopy()
	a := onlyV2.GetAnnotations()
	delete(a, annRequesterSig)
	onlyV2.SetAnnotations(a)
	if _, ok, reason := verifiedRequester(onlyV2, s); !ok {
		t.Fatalf("the v2 signature alone must verify through the stamp's key id: %s", reason)
	}
	onlyLegacy := stamped.DeepCopy()
	a = onlyLegacy.GetAnnotations()
	delete(a, annRequesterSigV2)
	delete(a, annRequesterKeyID)
	onlyLegacy.SetAnnotations(a)
	if _, ok, reason := verifiedRequester(onlyLegacy, s); !ok {
		t.Fatalf("the legacy signature alone must still verify (pre-migration stamps): %s", reason)
	}
	deadV2 := stamped.DeepCopy()
	a = deadV2.GetAnnotations()
	a[annRequesterSigV2] = "not-a-real-sig"
	deadV2.SetAnnotations(a)
	if _, ok, reason := verifiedRequester(deadV2, s); !ok {
		t.Fatalf("a failed v2 signature must fall back to the legacy MAC over the current binding: %s", reason)
	}
	for _, tc := range []struct {
		name   string
		tamper func(o *unstructured.Unstructured)
	}{
		{"identity", func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[annRequester] = "mallory"
			o.SetAnnotations(a)
		}},
		{"spec", func(o *unstructured.Unstructured) {
			o.Object["spec"].(map[string]any)["chart"] = "other"
		}},
		{"both signatures", func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[annRequesterSig] = "not-a-real-sig"
			a[annRequesterSigV2] = "not-a-real-sig"
			o.SetAnnotations(a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := stamped.DeepCopy()
			tc.tamper(bad)
			if _, ok, _ := verifiedRequester(bad, s); ok {
				t.Errorf("tampered %s must fail v2 verification", tc.name)
			}
		})
	}
}

func TestRequesterKeyRotationRetainsVerifiers(t *testing.T) {
	cs := fake.NewSimpleClientset()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	spec := map[string]any{"chart": "app"}

	s0 := keyringFromSecret(t, cs, "k0", map[string][]byte{"k0": testRotationKey(0)})
	legacyHC := signedHC("team-a", "app", "uid-0", info, s0)

	signers := []*requesterSigner{s0}
	allKeys := map[string][]byte{"k0": testRotationKey(0)}
	for n := 1; n <= 3; n++ {
		allKeys["k"+fmt.Sprint(n)] = testRotationKey(n)
		signers = append(signers, keyringFromSecret(t, cs, "k"+fmt.Sprint(n), allKeys))
	}
	final := signers[3]

	objects := []*unstructured.Unstructured{legacyHC}
	for i, s := range signers {
		id := "k" + fmt.Sprint(i)
		objects = append(objects, stampedV2("team-a", "app", fmt.Sprintf("uid-%d", i), info, s, id, spec))
	}
	for i, obj := range objects {
		got, ok, reason := verifiedRequester(obj.DeepCopy(), final)
		if !ok {
			t.Errorf("stamp %d refused after three rotations: %s", i, reason)
			continue
		}
		if got.Username != info.Username || got.UID != info.UID {
			t.Errorf("stamp %d verified as %+v, want %+v", i, got, info)
		}
	}
	for i, obj := range objects[1:] {
		if got := obj.GetAnnotations()[annRequesterKeyID]; got != "k"+fmt.Sprint(i) {
			t.Errorf("stamp signed under k%d carries key id %q", i, got)
		}
	}
	imp, refuse, msg := uninstallIdentity(objects[0].DeepCopy(), []string{"kube-system"}, final)
	if refuse || imp == nil || imp.UserName != info.Username {
		t.Fatalf("untouched legacy object must remain uninstallable after three rotations: imp=%+v refuse=%v msg=%s", imp, refuse, msg)
	}
}

func TestRequesterLegacyStampsStillVerifyUnderKeyring(t *testing.T) {
	cs := fake.NewSimpleClientset()
	retained := testSigner().key // the key the golden stamps were signed with
	s := keyringFromSecret(t, cs, "k2", map[string][]byte{
		"k1": retained,
		"k2": testRotationKey(2),
	})

	hc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata": map[string]any{
			"namespace": "team-a",
			"name":      "app",
			"uid":       "uid-1",
			"annotations": map[string]any{
				annRequester:       "alice",
				annRequesterUID:    "u1",
				annRequesterGroups: `["team-a","system:authenticated"]`,
				annRequesterSig:    literalStamp,
			},
		},
	}}
	got, ok, reason := verifiedRequester(hc, s)
	if !ok {
		t.Fatalf("literal stamp must still verify when its key is retained in the keyring: %s", reason)
	}
	if got.Username != "alice" {
		t.Errorf("literal stamp verified as %q, want alice", got.Username)
	}

	fixture := loadStampedFixture(t)
	got, ok, reason = verifiedRequester(fixture, s)
	if !ok {
		t.Fatalf("golden fixture must still verify under a signer that retains its key: %s", reason)
	}
	if got.Username != "alice" || got.UID != "alice-uid" {
		t.Errorf("golden fixture verified as %+v, want alice/alice-uid", got)
	}
}

func TestRequesterSignerLoadsKeyedSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1), "k0": testRotationKey(0)})
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}

	o := stampedV2("team-a", "app", "uid-1", info, s, "k1", map[string]any{"chart": "app"})
	if o.GetAnnotations()[annRequesterKeyID] != "k1" {
		t.Fatalf("signer must sign under the Secret's current key id k1, stamped %q", o.GetAnnotations()[annRequesterKeyID])
	}
	if _, ok, reason := verifiedRequester(o, s); !ok {
		t.Fatalf("own v2 stamp must verify: %s", reason)
	}
	// A stamp signed under the retained key k0 verifies too.
	old := stampedV2("team-a", "app", "uid-2", info, keyringFromSecret(t, fake.NewSimpleClientset(), "k0", map[string][]byte{"k0": testRotationKey(0)}), "k0", map[string]any{"chart": "app"})
	if _, ok, reason := verifiedRequester(old, s); !ok {
		t.Fatalf("a stamp signed under retained key k0 must verify under the loaded keyring: %s", reason)
	}
}

// plainRequesterHC builds an unstamped HelmChart CR for admission to stamp.
func plainRequesterHC(values string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata":   map[string]any{"namespace": "team-a", "name": "app"},
		"spec":       map[string]any{"chart": "app", "valuesContent": values},
	}}
}

func admissionCreateRequest(t *testing.T, obj *unstructured.Unstructured, user string) *admissionv1.AdmissionRequest {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return &admissionv1.AdmissionRequest{
		UID:       "req-1",
		Kind:      metav1.GroupVersionKind{Group: group, Version: "v1", Kind: "HelmChart"},
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: user, UID: user + "-uid", Groups: []string{"system:authenticated"}},
	}
}

func admitThrough(t *testing.T, handler http.HandlerFunc, obj *unstructured.Unstructured, user string) (stamped *unstructured.Unstructured, allowed bool, msg string) {
	t.Helper()
	body, err := json.Marshal(admissionv1.AdmissionReview{Request: admissionCreateRequest(t, obj, user)})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body)))
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil {
		t.Fatalf("decode admission response: %v (%s)", err, rr.Body.String())
	}
	if review.Response == nil || !review.Response.Allowed {
		if review.Response != nil && review.Response.Result != nil {
			return nil, false, review.Response.Result.Message
		}
		return nil, false, rr.Body.String()
	}
	patch, err := jsonpatch.DecodePatch(review.Response.Patch)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := patch.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := &unstructured.Unstructured{}
	if err := json.Unmarshal(patched, out); err != nil {
		t.Fatal(err)
	}
	return out, true, ""
}

func writeRequesterSecret(t *testing.T, cs kubernetes.Interface, currentID string, keys map[string][]byte) {
	t.Helper()
	keyring := map[string]string{}
	for id, key := range keys {
		keyring[id] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.Marshal(keyring)
	if err != nil {
		t.Fatalf("encode retained-key map: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      requesterKeySecret,
			Namespace: webhookNamespace,
			Labels:    map[string]string{group + "/managed-by": fieldManager},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			requesterKeyField:            keys[currentID],
			requesterKeysField:           raw,
			requesterBindingVersionField: []byte(stampBindingV2),
		},
	}
	secrets := cs.CoreV1().Secrets(webhookNamespace)
	if _, err := secrets.Create(context.Background(), sec, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if _, err := secrets.Update(context.Background(), sec, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update signing Secret: %v", err)
		}
	} else if err != nil {
		t.Fatalf("create signing Secret: %v", err)
	}
}

// waitForSigner polls the holder until cond holds on the current snapshot.
func waitForSigner(t *testing.T, h *requesterSignerHolder, cond func(*requesterSigner) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := h.current(); cond(s) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if s := h.current(); s == nil {
		t.Fatalf("%s; the holder never published a signer", what)
	} else {
		t.Fatalf("%s; the holder's signer still has key id %q", what, s.currentID())
	}
}

func TestRequesterSignerRetryKeepsSixAttempts(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var gets atomic.Int32
	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gets.Add(1)
		return true, nil, apierrors.NewInternalError(errors.New("apiserver down"))
	})
	old := requesterKeyRetryWait
	requesterKeyRetryWait = time.Millisecond
	defer func() { requesterKeyRetryWait = old }()

	start := time.Now()
	s := loadRequesterSignerWithRetry(context.Background(), cs)
	if s != nil {
		t.Fatal("exhausted retries must fail closed (nil), not return a signer")
	}
	if got := gets.Load(); got != 6 {
		t.Errorf("key load made %d attempts, want the six-attempt pattern", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("six attempts took %s; the retry loop must honor requesterKeyRetryWait", elapsed)
	}
}

func TestRequesterSignerHolderPublishesImmutableSnapshots(t *testing.T) {
	h := newRequesterSignerHolder()
	if got := h.current(); got != nil {
		t.Fatal("an empty holder must current() nil (fail closed), not a signer")
	}

	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	s1 := &requesterSigner{key: testRotationKey(1), id: "k1", keys: map[string][]byte{"k1": testRotationKey(1)}}
	h.publish(s1)
	got := h.current()
	if got == nil || !bytes.Equal(got.key, testRotationKey(1)) {
		t.Fatal("current() must return the published snapshot")
	}
	sig1 := got.sign("uid-1", info)

	s2 := &requesterSigner{key: testRotationKey(2), id: "k2", keys: map[string][]byte{"k2": testRotationKey(2)}}
	h.publish(s2)
	cur := h.current()
	if cur == nil || !bytes.Equal(cur.key, testRotationKey(2)) {
		t.Fatal("current() must return the latest published snapshot")
	}
	if !bytes.Equal(got.key, testRotationKey(1)) || got.currentID() != "k1" {
		t.Error("a snapshot taken before a later publish changed; snapshots must be immutable")
	}
	if !got.verify("uid-1", info, sig1) {
		t.Error("a signature made under an earlier snapshot must still verify under it after a later publish")
	}
}

func TestRequesterSignerHolderSwapRaceUnderAdmissionAndReconcile(t *testing.T) {
	h := newRequesterSignerHolder()
	keys := map[string][]byte{
		"k0": testRotationKey(0), "k1": testRotationKey(1),
		"k2": testRotationKey(2), "k3": testRotationKey(3),
	}
	snapshots := make([]*requesterSigner, 0, 4)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("k%d", i)
		snapshots = append(snapshots, &requesterSigner{key: keys[id], id: id, keys: keys})
	}
	h.publish(snapshots[0])

	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	objects := make([]*unstructured.Unstructured, 0, 4)
	for i, s := range snapshots {
		objects = append(objects, stampedV2("team-a", "app", fmt.Sprintf("uid-%d", i), info, s, fmt.Sprintf("k%d", i), map[string]any{"chart": "app"}))
	}
	req := admissionCreateRequest(t, plainRequesterHC("replicas: 1"), "bob")

	var wg sync.WaitGroup
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				h.publish(snapshots[i%len(snapshots)])
			}
		}()
	}
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				resp := buildAdmissionResponse(req, h.current())
				if resp == nil || !resp.Allowed || len(resp.Patch) == 0 {
					t.Error("admission during a snapshot swap must keep stamping")
					return
				}
			}
		}()
	}
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if _, ok, reason := verifiedRequester(objects[i%len(objects)], h.current()); !ok {
					t.Errorf("reconcile verification during a snapshot swap failed: %s", reason)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestRequesterSignerOwnerReloadsReplacedSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	s1 := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1)})

	h := newRequesterSignerHolder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go maintainRequesterSigner(ctx, cs, h, 2*time.Millisecond)

	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(1)) },
		"the owner must publish the signer loaded from the signing Secret")
	handler := admissionHandler(h)

	oldObj := stampedV2("team-a", "app", "uid-1", info, s1, "k1", map[string]any{"chart": "app", "valuesContent": "replicas: 1"})
	if _, ok, reason := verifiedRequester(oldObj, h.current()); !ok {
		t.Fatalf("the stored stamp must verify before the Secret is replaced: %s", reason)
	}
	pre, allowed, msg := admitThrough(t, handler, plainRequesterHC("replicas: 1"), "alice")
	if !allowed {
		t.Fatalf("admission through the holder refused before the replacement: %s", msg)
	}
	if got := pre.GetAnnotations()[annRequesterKeyID]; got != "k1" {
		t.Fatalf("the webhook must stamp with the loaded key id k1, stamped %q", got)
	}

	if err := cs.CoreV1().Secrets(webhookNamespace).Delete(context.Background(), requesterKeySecret, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	writeRequesterSecret(t, cs, "k2", map[string][]byte{"k2": testRotationKey(2)})

	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(2)) },
		"deleting then recreating the Secret must reload the key without a process restart")

	post, allowed, msg := admitThrough(t, handler, plainRequesterHC("replicas: 2"), "bob")
	if !allowed {
		t.Fatalf("admission through the refreshed holder refused: %s", msg)
	}
	if got := post.GetAnnotations()[annRequesterKeyID]; got != "k2" {
		t.Errorf("the webhook must stamp with the reloaded key id k2 on the next admission, stamped %q", got)
	}
	cur := h.current()
	if got, ok, reason := verifiedRequester(post, cur); !ok || got.Username != "bob" {
		t.Errorf("the reconciler must verify a stamp written under the reloaded key: ok=%v (%s)", ok, reason)
	}
	if _, ok, reason := verifiedRequester(oldObj, cur); !ok {
		t.Errorf("stored stamps signed under the replaced key must still verify: %s", reason)
	}

	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(context.Background(), requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := decodeKeyring(sec.Data[requesterKeysField])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keyring["k1"]; !ok {
		t.Errorf("the recreated Secret must record the retained verifier set, got %v", encodeKeyring(keyring))
	}
}

func TestRequesterSignerOwnerKeepsSetWhenSecretDeleted(t *testing.T) {
	cs := fake.NewSimpleClientset()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	s1 := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1)})

	h := newRequesterSignerHolder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go maintainRequesterSigner(ctx, cs, h, 2*time.Millisecond)
	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(1)) },
		"the owner must publish the signer loaded from the signing Secret")

	oldObj := stampedV2("team-a", "app", "uid-1", info, s1, "k1", map[string]any{"chart": "app"})
	handler := admissionHandler(h)

	if err := cs.CoreV1().Secrets(webhookNamespace).Delete(context.Background(), requesterKeySecret, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		cur := h.current()
		if cur == nil || !bytes.Equal(cur.key, testRotationKey(1)) {
			t.Fatalf("deleting the signing Secret must keep the current signer working; holder = %v", cur)
		}
		if _, ok, reason := verifiedRequester(oldObj, cur); !ok {
			t.Fatalf("deleting the signing Secret must keep stored stamps verifying: %s", reason)
		}
		time.Sleep(2 * time.Millisecond)
	}

	stamped, allowed, msg := admitThrough(t, handler, plainRequesterHC("replicas: 3"), "alice")
	if !allowed {
		t.Fatalf("admission after the Secret was deleted refused: %s", msg)
	}
	if got := stamped.GetAnnotations()[annRequesterKeyID]; got != "k1" {
		t.Errorf("admission after the Secret was deleted must keep stamping with key id k1, stamped %q", got)
	}
	if _, ok, reason := verifiedRequester(stamped, h.current()); !ok {
		t.Errorf("a stamp written after the Secret was deleted must verify: %s", reason)
	}
}

func TestRequesterSignerOwnerRecreatesDeletedSecretFromHeldKey(t *testing.T) {
	cs := fake.NewSimpleClientset()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	s1 := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1)})

	h := newRequesterSignerHolder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go maintainRequesterSigner(ctx, cs, h, 2*time.Millisecond)
	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(1)) },
		"the owner must publish the signer loaded from the signing Secret")

	stored := stampedV2("team-a", "app", "uid-1", info, s1, "k1", map[string]any{"chart": "app"})

	if err := cs.CoreV1().Secrets(webhookNamespace).Delete(context.Background(), requesterKeySecret, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(context.Background(), requesterKeySecret, metav1.GetOptions{})
		if err == nil {
			if !bytes.Equal(sec.Data[requesterKeyField], testRotationKey(1)) {
				t.Fatal("the re-created signing Secret must carry the held key, not new material")
			}
			break
		}
		if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the owner must re-create a deleted signing Secret from the held key")
		}
		time.Sleep(2 * time.Millisecond)
	}

	restarted, err := ensureRequesterSigner(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restarted.key, testRotationKey(1)) {
		t.Fatal("a restart after the deletion must reload the held key, not mint a fresh random one")
	}
	if _, ok, reason := verifiedRequester(stored, restarted); !ok {
		t.Errorf("a stored stamp must verify under the signer a restarted operator loads: %s", reason)
	}

	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(context.Background(), requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := decodeKeyring(sec.Data[requesterKeysField])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keyring["k1"]; !ok {
		t.Errorf("the re-created Secret must record the retained verifier set, got %v", encodeKeyring(keyring))
	}
}

func TestRequesterSignerOwnerKeepsSetOnTransientErrors(t *testing.T) {
	cs := fake.NewSimpleClientset()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	s1 := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1)})

	var failing atomic.Bool
	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, apierrors.NewInternalError(errors.New("apiserver degraded"))
		}
		return false, nil, nil
	})

	h := newRequesterSignerHolder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go maintainRequesterSigner(ctx, cs, h, 2*time.Millisecond)
	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(1)) },
		"the owner must publish the signer loaded from the signing Secret")

	oldObj := stampedV2("team-a", "app", "uid-1", info, s1, "k1", map[string]any{"chart": "app"})

	failing.Store(true)
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		cur := h.current()
		if cur == nil || !bytes.Equal(cur.key, testRotationKey(1)) {
			t.Fatalf("transient read errors must keep the current signer working; holder = %v", cur)
		}
		if _, ok, reason := verifiedRequester(oldObj, cur); !ok {
			t.Fatalf("transient read errors must keep stored stamps verifying: %s", reason)
		}
		time.Sleep(2 * time.Millisecond)
	}

	writeRequesterSecret(t, cs, "k2", map[string][]byte{"k1": testRotationKey(1), "k2": testRotationKey(2)})
	failing.Store(false)

	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(2)) },
		"a Secret replaced during the transient errors must be picked up once they clear")
	if _, ok, reason := verifiedRequester(oldObj, h.current()); !ok {
		t.Errorf("stored stamps signed under the replaced key must still verify after the reload: %s", reason)
	}
}

func TestRequesterSignerOwnerNeverStaysSilentlyKeyless(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var failing atomic.Bool
	failing.Store(true)
	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, apierrors.NewInternalError(errors.New("apiserver down"))
		}
		return false, nil, nil
	})

	old := requesterKeyRetryWait
	requesterKeyRetryWait = time.Millisecond
	defer func() { requesterKeyRetryWait = old }()

	h := newRequesterSignerHolder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go maintainRequesterSigner(ctx, cs, h, 2*time.Millisecond)

	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s := h.current(); s != nil {
			t.Fatalf("an unreachable signing key must fail closed (nil holder), got %v", s)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, allowed, msg := admitThrough(t, admissionHandler(h), plainRequesterHC("replicas: 1"), "alice"); allowed {
		t.Error("admission while keyless must be refused, not stamped")
	} else if !strings.Contains(msg, "requester-signing key unavailable") {
		t.Errorf("the keyless refusal must say the signing key is unavailable, got: %s", msg)
	}

	failing.Store(false)
	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil },
		"the owner must keep retrying so the key loads the moment the apiserver answers")

	stamped, allowed, msg := admitThrough(t, admissionHandler(h), plainRequesterHC("replicas: 2"), "alice")
	if !allowed {
		t.Fatalf("admission after the key loaded refused: %s", msg)
	}
	if _, ok, reason := verifiedRequester(stamped, h.current()); !ok {
		t.Errorf("a stamp written after the key loaded must verify: %s", reason)
	}
}

func TestRequesterSignerOwnerStopsOnCancellation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver down"))
	})
	old := requesterKeyRetryWait
	requesterKeyRetryWait = time.Millisecond
	defer func() { requesterKeyRetryWait = old }()

	h := newRequesterSignerHolder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		maintainRequesterSigner(ctx, cs, h, 2*time.Millisecond)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the signer owner did not return within 3s of cancellation; it stopped watching ctx.Done")
	}
	if h.current() != nil {
		t.Error("a cancelled owner must not publish a signer")
	}
}

func setSchemaDefaultStripper(t *testing.T, strip func(spec any) any) {
	t.Helper()
	prev := schemaDefaultStripper.Load()
	if strip != nil {
		schemaDefaultStripper.Store(strip)
	} else {
		schemaDefaultStripper = atomic.Value{}
	}
	t.Cleanup(func() {
		if prev != nil {
			schemaDefaultStripper.Store(prev)
		} else {
			schemaDefaultStripper = atomic.Value{}
		}
	})
}

// Reads the defaults map at strip time, like a served schema that can grow.
func stripDefaultsFor(defaults map[string]any) func(spec any) any {
	return func(spec any) any {
		props := make(map[string]any, len(defaults))
		for k, v := range defaults {
			props[k] = map[string]any{"default": v}
		}
		schema := map[string]any{"type": "object", "properties": props}
		out, keep := stripSchemaDefaults(spec, schema)
		if !keep {
			return map[string]any{}
		}
		return out
	}
}

func TestRequesterStampV2BindsAdmittedDefaults(t *testing.T) {
	defaults := map[string]any{"timeout": "5m", "createNamespace": false}
	setSchemaDefaultStripper(t, stripDefaultsFor(defaults))

	s := testSigner()
	info := requesterInfo{Username: "alice", UID: "alice-uid", Groups: []string{"system:authenticated"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1", "kind": "HelmChart",
		"metadata": map[string]any{"namespace": "team-a", "name": "app"},
		"spec": map[string]any{
			"chart": "app", "repo": "https://charts.example.com", "valuesContent": "replicas: 1",
			"timeout": "5m", "createNamespace": false,
		},
	}}
	created := admitObject(t, obj, nil, "alice", s)

	if got := created.GetAnnotations()[annRequesterSigV2]; got != s.signV2(bindingV2(created), info) {
		t.Fatalf("the stamped v2 signature must cover the admitted field policy and spec")
	}
	if !strings.Contains(created.GetAnnotations()[annRequesterFieldsV2], "timeout") {
		t.Fatal("the v2 field policy must include admitted current-default fields")
	}
	if _, ok, reason := verifiedRequester(created, s); !ok {
		t.Fatalf("a stamp over the admitted-field binding must verify: %s", reason)
	}

	defaults["priority"] = int64(3)
	served := created.DeepCopy()
	served.Object["spec"].(map[string]any)["priority"] = int64(3)
	if got, ok, reason := verifiedRequester(served, s); !ok {
		t.Fatalf("a stored object served with a later schema default must still verify: %s", reason)
	} else if got.Username != "alice" || got.UID != "alice-uid" {
		t.Errorf("verified identity %+v, want the bound alice/alice-uid", got)
	}

	tampered := served.DeepCopy()
	tampered.Object["spec"].(map[string]any)["chart"] = "other"
	if _, ok, _ := verifiedRequester(tampered, s); ok {
		t.Error("a tampered non-default value must be refused")
	}

	changed := served.DeepCopy()
	changed.Object["spec"].(map[string]any)["timeout"] = "10m"
	if _, ok, _ := verifiedRequester(changed, s); ok {
		t.Error("replacing a defaulted value with a non-default one must be refused")
	}

	setSchemaDefaultStripper(t, nil)
	if _, ok, _ := verifiedRequester(served, s); ok {
		t.Error("with no schema source the v2 stamp must keep full spec coverage")
	}
}

func TestStripSchemaDefaultsWalk(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"chart":   map[string]any{"type": "string"},
			"timeout": map[string]any{"type": "string", "default": "5m"},
			"enabled": map[string]any{"type": "boolean", "default": false},
			"count":   map[string]any{"type": "integer", "default": 3},
			"nested": map[string]any{"type": "object", "properties": map[string]any{
				"mode": map[string]any{"type": "string", "default": "auto"},
			}},
			"args": map[string]any{"type": "array", "items": map[string]any{
				"type": "string", "default": "x",
			}},
			"set": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		},
	}

	spec := map[string]any{
		"chart":   "app",
		"timeout": "5m",
		"enabled": false,
		"count":   int64(3),   // the object side of a served default
		"count2":  float64(3), // float64 must match the same default
		"nested":  map[string]any{"mode": "auto", "extra": "keep"},
		"args":    []any{"a", "x", "b"},
		"set":     map[string]any{"replicas": "2"},
		"unknown": "keep",
	}
	out, keep := stripSchemaDefaults(spec, schema)
	if !keep {
		t.Fatal("an object spec must survive the projection")
	}
	want := map[string]any{
		"chart":   "app",
		"count2":  float64(3),
		"nested":  map[string]any{"extra": "keep"},
		"args":    []any{"a", "b"},
		"set":     map[string]any{"replicas": "2"},
		"unknown": "keep",
	}
	if !jsonEqual(out, want) {
		t.Errorf("projection = %v, want %v (default-valued fields dropped, unknown and non-default fields kept)", out, want)
	}

	if _, keep := stripSchemaDefaults("5m", map[string]any{"default": "5m"}); keep {
		t.Error("a scalar equal to its schema default must drop out of the projection")
	}
	if v, keep := stripSchemaDefaults("10m", map[string]any{"default": "5m"}); !keep || v != "10m" {
		t.Errorf("stripSchemaDefaults(10m) = %v keep=%v, want the value kept", v, keep)
	}
}

func TestSchemaDefaultStripperFromCRD(t *testing.T) {
	const crd = `{
	  "spec": {"versions": [
	    {"name": "v0", "served": true, "storage": false,
	     "schema": {"openAPIV3Schema": {"type": "object", "properties": {"spec": {"type": "object", "properties": {"chart": {"type": "string"}}}}}}},
	    {"name": "v1", "served": true, "storage": true,
	     "schema": {"openAPIV3Schema": {"type": "object", "properties": {"spec": {"type": "object", "properties": {
	       "chart": {"type": "string"},
	       "timeout": {"type": "string", "default": "5m"},
	       "priority": {"type": "integer", "default": 3}}}}}}}
	  ]}
	}`
	strip, err := schemaDefaultStripperFromCRD([]byte(crd))
	if err != nil {
		t.Fatal(err)
	}
	out := strip(map[string]any{"chart": "app", "timeout": "5m", "priority": int64(3), "extra": "keep"})
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("projection is %T, want a map", out)
	}
	if _, has := got["timeout"]; has {
		t.Error("the defaulted timeout must drop out of the projection")
	}
	if _, has := got["priority"]; has {
		t.Error("the defaulted priority (CRD float64 3 vs object int64 3) must drop out")
	}
	if got["chart"] != "app" || got["extra"] != "keep" {
		t.Errorf("projection = %v, want the non-defaulted fields untouched", got)
	}

	if _, err := schemaDefaultStripperFromCRD([]byte(`{"spec":{"versions":[{"name":"v1","storage":true,"schema":{"openAPIV3Schema":{"type":"object"}}}]}}`)); err == nil {
		t.Error("a schema without a spec property must fail, not produce a blind stripper")
	}
	if _, err := schemaDefaultStripperFromCRD([]byte(`{`)); err == nil {
		t.Error("a malformed CRD must fail to produce a stripper")
	}
}
