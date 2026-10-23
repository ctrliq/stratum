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
	// Bound to the UID.
	if s.verify("uid-2", info, sig) {
		t.Error("signature must not verify against a different object UID")
	}
	// Bound to the identity.
	if s.verify("uid-1", requesterInfo{Username: "bob"}, sig) {
		t.Error("signature must not verify against a different identity")
	}
	// Field boundaries are unambiguous: (user "a", group "b") != (user "ab", group "").
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

// literalStamp is the authentic signature of testSigner() over a fixed object
// binding (API version stratum.ciq.com/v1, kind HelmChart, namespace team-a,
// name app, no spec; metadata UID is excluded) and the fixed
// identity (alice/u1, groups team-a + system:authenticated), computed once when
// this guard landed. It stands for every requester stamp already stored on a
// tenant HelmChart in a cluster and MUST NEVER BE REGENERATED: if a change to
// the signer alters what the signature covers or how it is encoded, this
// literal stops verifying and this test fails.
const literalStamp = "qGR5/gX3C+ZeEqoAFoBXibGE60aHXstXnWGcoofZT2g"

// TestRequesterStamp_AStampWrittenTodayStillVerifies: a stamp written by
// today's release still verifies after a key-format change, or every tenant
// HelmChart already in the cluster would fail verification and keep its
// finalizer.
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

	// The literal verifies against the key it was signed with.
	if !testSigner().verify(requesterBinding(hc), info, literalStamp) {
		t.Error("today's literal stamp must verify with today's key and binding")
	}
	// And the object-level path the reconciler uses accepts it and returns the
	// stamped identity.
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

// loadStampedFixture reads the golden stamp fixture: the exact object today's
// admission webhook produces for one CREATE — namespace/name/spec binding,
// stamped requester identity and canonical groups, and the signature in the
// current canonical form. It is written once and never regenerated.
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

// fixtureStamp is the signature the golden fixture carries. It was produced by
// this release's signer and MUST NEVER BE REGENERATED alongside the fixture:
// the fixture stands for the legacy stamps already stored on tenant HelmCharts,
// so replacing it would make this test confirm the new format instead of the
// old one.
const fixtureStamp = "9cHHuoCJeU+pN435a4q1fH6X97EShVgi2yBjztE5W6c"

// TestRequesterStampFromThisReleaseStillVerifies: the golden fixture
// testdata/stamped-helmchart.json (today's admitObject output, written once)
// must keep verifying under the verifier of every future release, and
// verification must keep rejecting anything that alters what the stamp is
// bound to — the requester identity, and the desired state it authorizes.
func TestRequesterStampFromThisReleaseStillVerifies(t *testing.T) {
	hc := loadStampedFixture(t)

	// The fixture binds exactly the identity and desired state admission saw.
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

	// It verifies, and verification returns the stamped identity.
	got, authentic, reason := verifiedRequester(hc, testSigner())
	if !authentic {
		t.Fatalf("a stamp written by this release must still verify; refused: %s", reason)
	}
	if got.Username != bound.Username || got.UID != bound.UID || !slices.Equal(got.Groups, bound.Groups) {
		t.Errorf("verifiedRequester returned identity %+v, want %+v", got, bound)
	}

	// Any change to what the signature covers stops it verifying.
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

// TestLoadRequesterSigner_FailsClosedWhenCancelled: a first key load whose
// context is already cancelled returns nil promptly, instead of hanging in the
// retry loop — an operator shutting down must not wait on an apiserver that
// never answers.
func TestLoadRequesterSigner_FailsClosedWhenCancelled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("*", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver down"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan *requesterSigner, 1)
	go func() { done <- loadRequesterSignerWithRetry(ctx, cs) }()

	// One second is well inside the ten-second retry interval: the loop must
	// see the cancellation between attempts, not only after the timer fires.
	select {
	case s := <-done:
		if s != nil {
			t.Fatal("a cancelled first load must return nil (fail closed), not a signer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loadRequesterSignerWithRetry did not return within 5s of a cancelled context; the retry loop stopped watching it")
	}
}

// testRotationKey returns a deterministic 32-byte key for rotation n; distinct n
// give distinct keys, so a test can name exactly which key signed a stamp.
func testRotationKey(n int) []byte {
	k := make([]byte, requesterKeyBytes)
	copy(k, "rotation-test-key-")
	k[requesterKeyBytes-4], k[requesterKeyBytes-3], k[requesterKeyBytes-2], k[requesterKeyBytes-1] =
		byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return k
}

// keyringFromSecret writes the requester-signing Secret the way the migration
// leaves it — the legacy key field holding the CURRENT key, plus the retained-key
// map (a JSON object of key id to std-base64 key) holding every verification key —
// and loads a signer from it. Calling it again with a larger map is a key rotation.
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

// stampedV2 builds a HelmChart carrying the full migration stamp set: the identity
// annotations, the legacy signature, the v2 signature, and the key identifier of
// the key that produced the v2 signature — the annotation set admission writes
// once the versioned binding lands.
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

// TestRequesterStampV2Roundtrip: the versioned binding round-trips through
// sign/verify. The v2 MAC is the legacy field sequence with the version+algorithm
// header length-prefixed FIRST — that header is what keeps a v2 MAC from colliding
// with a legacy MAC over the same payload — and a migrated stamp carries BOTH
// signatures, so a pre-migration verifier can still verify it.
func TestRequesterStampV2Roundtrip(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := keyringFromSecret(t, cs, "k1", map[string][]byte{"k1": testRotationKey(1)})
	// Unsorted, with a duplicate: the v2 MAC must cover the canonical group set.
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"system:authenticated", "team-a", "team-a"}}

	// The v2 MAC construction is the cross-release contract: length-prefixed
	// version+algorithm header first, then the unchanged legacy fields.
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
	// Each half verifies on its own: the v2 signature through the stamp's key id,
	// the legacy signature through the legacy path a pre-migration release reads.
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
	// A v2 signature that fails no longer dooms the object on its own: the
	// legacy MAC over the current binding authenticates it (the mixed-version
	// fallback). Tampering BOTH signatures is what verification must refuse.
	deadV2 := stamped.DeepCopy()
	a = deadV2.GetAnnotations()
	a[annRequesterSigV2] = "not-a-real-sig"
	deadV2.SetAnnotations(a)
	if _, ok, reason := verifiedRequester(deadV2, s); !ok {
		t.Fatalf("a failed v2 signature must fall back to the legacy MAC over the current binding: %s", reason)
	}
	// Anything the v2 stamp binds is rejected when it changes.
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

// TestRequesterKeyRotationRetainsVerifiers: three key rotations later, stamps
// signed under every retained key still verify, and an untouched legacy object —
// stamped once, never re-admitted — still uninstalls. The retained-key map exists
// so rotation never orphans a stamp that is already stored.
func TestRequesterKeyRotationRetainsVerifiers(t *testing.T) {
	cs := fake.NewSimpleClientset()
	info := requesterInfo{Username: "alice", UID: "u1", Groups: []string{"team-a"}}
	spec := map[string]any{"chart": "app"}

	s0 := keyringFromSecret(t, cs, "k0", map[string][]byte{"k0": testRotationKey(0)})
	legacyHC := signedHC("team-a", "app", "uid-0", info, s0) // legacy stamp only, never re-admitted

	signers := []*requesterSigner{s0}
	allKeys := map[string][]byte{"k0": testRotationKey(0)}
	for n := 1; n <= 3; n++ {
		allKeys["k"+fmt.Sprint(n)] = testRotationKey(n)
		signers = append(signers, keyringFromSecret(t, cs, "k"+fmt.Sprint(n), allKeys))
	}
	final := signers[3]

	// objects[0] is the untouched legacy object; objects[1..4] carry v2 stamps
	// signed under k0..k3 respectively.
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
	// The untouched legacy object must still uninstall: its legacy stamp verifies
	// against the retained keys even though none of them is the signer's current key.
	imp, refuse, msg := uninstallIdentity(objects[0].DeepCopy(), []string{"kube-system"}, final)
	if refuse || imp == nil || imp.UserName != info.Username {
		t.Fatalf("untouched legacy object must remain uninstallable after three rotations: imp=%+v refuse=%v msg=%s", imp, refuse, msg)
	}
}

// TestRequesterLegacyStampsStillVerifyUnderKeyring: the retained-key map must not
// disturb legacy verification. The literalStamp guard stamp and the golden
// fixture keep verifying under a signer whose CURRENT key is not the key they were
// signed with — that key is merely retained in the map.
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

// TestRequesterSignerLoadsKeyedSecret: the Secret's retained-key map is the
// signer's keyring — it signs under the map's current key and verifies under every
// retained key — and a Secret that still carries only the legacy key field loads
// with a key identifier that stays stable across reloads (see
// TestAdmissionStampSurvivesSignerReload for the stamp-level consequence).
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
	// A stamp signed under the RETAINED key k0 verifies too — the map is a
	// keyring, not just the current key.
	old := stampedV2("team-a", "app", "uid-2", info, keyringFromSecret(t, fake.NewSimpleClientset(), "k0", map[string][]byte{"k0": testRotationKey(0)}), "k0", map[string]any{"chart": "app"})
	if _, ok, reason := verifiedRequester(old, s); !ok {
		t.Fatalf("a stamp signed under retained key k0 must verify under the loaded keyring: %s", reason)
	}
}

// --- refreshing the requester signing key without a restart -------------------
//
// The signer is no longer loaded once and frozen for the process lifetime. A
// background owner keeps the signing Secret under observation and publishes
// immutable signer snapshots through an atomic holder; the admission webhook
// reads a snapshot per request and the reconciler per pass. The tests below pin
// that contract: the owner surface (maintainRequesterSigner), the holder
// (newRequesterSignerHolder / publish / current), the tunable inter-attempt
// wait of the startup retry (requesterKeyRetryWait), and an admissionHandler
// that accepts the holder and re-reads the snapshot on every admission.

// plainRequesterHC builds an unstamped HelmChart CR for admission to stamp.
func plainRequesterHC(values string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata":   map[string]any{"namespace": "team-a", "name": "app"},
		"spec":       map[string]any{"chart": "app", "valuesContent": values},
	}}
}

// admissionCreateRequest builds the wire request admission would see for one
// CREATE of obj by user.
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

// admitThrough posts one CREATE AdmissionReview to handler, applies the returned
// JSON Patch like the apiserver would, and returns the stamped object. allowed
// is false when admission refused; msg then carries the refusal reason.
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

// writeRequesterSecret writes (or replaces) the signing Secret with the given
// current key id and retained-key ring.
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

// TestRequesterSignerRetryKeepsSixAttempts: the startup key load keeps its
// six-attempt pattern against a transient apiserver, fails closed (nil) when
// they are exhausted, and spaces the attempts by requesterKeyRetryWait — which
// must be honored, or no test could ever observe the pattern.
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

// TestRequesterSignerHolderPublishesImmutableSnapshots: the holder starts
// keyless (fail closed), hands out the latest published snapshot, and never
// lets a later publish change an earlier one — a snapshot handed out stays
// byte-for-byte the signer it was when the reader took it.
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

// TestRequesterSignerHolderSwapRaceUnderAdmissionAndReconcile: concurrent
// admission (buildAdmissionResponse) and reconcile (verifiedRequester) readers
// taking snapshots from the holder while publishers swap signers in must keep
// stamping and verifying without a torn read — run under -race, this is the
// test that condemns a plain (non-atomic) pointer swap. Every published signer
// retains the whole keyring, so verification must succeed on every read.
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

// TestRequesterSignerOwnerReloadsReplacedSecret: deleting then recreating the
// signing Secret must reload the key WITHOUT a process restart — the same
// admission handler stamps with the new key on the next admission, the
// reconciler verifies under the re-published snapshot, and stamps already
// stored under the replaced key still verify (the previous verifier set is
// retained, even though the recreated Secret carries only the new key, and the
// retained set is written back so it survives a restart).
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
	// One handler instance for the whole test: it must re-read the snapshot per
	// admission, which is what lets it stamp with both keys below.
	handler := admissionHandler(h)

	// A stamp already stored on a tenant HelmChart, signed under the original key.
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

	// Replace the Secret: delete, then recreate carrying ONLY the new key.
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

	// The retained verifier set must outlive this process: the owner persists
	// the merged keyring, or the next restart loses every stamp signed under
	// the replaced key.
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

// TestRequesterSignerOwnerKeepsSetWhenSecretDeleted: a deleted signing Secret
// must NOT churn the key — the current verifier set keeps signing and
// verifying across many owner ticks, and admission through the holder keeps
// stamping with the same key.
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
	// ~25 owner ticks: the snapshot must hold steady and the stored stamp must
	// keep verifying the whole time.
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

	// Admission through the holder still stamps, with the SAME key.
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

// TestRequesterSignerOwnerRecreatesDeletedSecretFromHeldKey: a signing Secret
// deleted while a key is loaded must be re-created from the held snapshot.
// Nothing else in the cluster restores it, and the next restart over a missing
// Secret mints a fresh random key (ensureRequesterSigner) — which would strand
// every stored stamp as "forged or tampered", the outcome the key identifiers
// exist to prevent.
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

	// The owner re-creates the deleted Secret from the held key within a few
	// ticks, carrying the same key material.
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

	// The restart the recreation exists for: a fresh load must return the SAME
	// key — a fresh random one would fail every stored stamp at boot.
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

	// The re-created Secret carries the retained ring, so stamps signed under
	// other retained keys survive the restart too.
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

// TestRequesterSignerOwnerKeepsSetOnTransientErrors: while the apiserver
// returns transient errors the owner must keep the current verifier set
// working — and once the errors clear, a Secret that changed underneath must
// still be picked up.
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

	// While reads fail, the Secret is replaced; the owner can only learn that
	// once the errors clear.
	writeRequesterSecret(t, cs, "k2", map[string][]byte{"k1": testRotationKey(1), "k2": testRotationKey(2)})
	failing.Store(false)

	waitForSigner(t, h, func(s *requesterSigner) bool { return s != nil && bytes.Equal(s.key, testRotationKey(2)) },
		"a Secret replaced during the transient errors must be picked up once they clear")
	if _, ok, reason := verifiedRequester(oldObj, h.current()); !ok {
		t.Errorf("stored stamps signed under the replaced key must still verify after the reload: %s", reason)
	}
}

// TestRequesterSignerOwnerNeverStaysSilentlyKeyless: a first load that cannot
// reach the apiserver must leave the holder nil (fail closed, admission
// refused) — and the owner must keep retrying in the background so the key is
// published the moment it becomes reachable, with no process restart.
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

	// Keyless: nothing published, admission refused — not an invented signer.
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

	// The apiserver becomes reachable: the key loads WITHOUT a restart.
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

// TestRequesterSignerOwnerStopsOnCancellation: the owner must respect
// ctx.Done throughout — including while its first load or a ticker wait is
// pending — and return promptly instead of leaking a goroutine.
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

// setSchemaDefaultStripper replaces the versioned binding's schema source for
// one test and restores the previous value at cleanup.
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

// stripDefaultsFor builds a schema source that strips every field named in the
// (mutable) defaults map — a stand-in for the CRD-backed stripper whose served
// schema can GROW the way the apiserver's does, so the map is read at strip
// time, not materialized once.
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

// TestRequesterStampV2BindsAdmittedDefaults: the versioned binding covers the
// admitted fields with their exact values, while subsequently introduced
// defaulted fields may be projected away. Current defaults remain authenticated
// so changing the schema cannot remove coverage of previously admitted values.
func TestRequesterStampV2BindsAdmittedDefaults(t *testing.T) {
	defaults := map[string]any{"timeout": "5m", "createNamespace": false}
	setSchemaDefaultStripper(t, stripDefaultsFor(defaults))

	s := testSigner()
	info := requesterInfo{Username: "alice", UID: "alice-uid", Groups: []string{"system:authenticated"}} // what admitObject stamps
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stratum.ciq.com/v1", "kind": "HelmChart",
		"metadata": map[string]any{"namespace": "team-a", "name": "app"},
		"spec": map[string]any{
			"chart": "app", "repo": "https://charts.example.com", "valuesContent": "replicas: 1",
			"timeout": "5m", "createNamespace": false,
		},
	}}
	created := admitObject(t, obj, nil, "alice", s)

	// The v2 signature binds the admitted field policy and exact admitted values.
	if got := created.GetAnnotations()[annRequesterSigV2]; got != s.signV2(bindingV2(created), info) {
		t.Fatalf("the stamped v2 signature must cover the admitted field policy and spec")
	}
	if !strings.Contains(created.GetAnnotations()[annRequesterFieldsV2], "timeout") {
		t.Fatal("the v2 field policy must include admitted current-default fields")
	}
	if _, ok, reason := verifiedRequester(created, s); !ok {
		t.Fatalf("a stamp over the admitted-field binding must verify: %s", reason)
	}

	// The schema gains a new default after the object exists; reading the old
	// object serves it with "priority": 3, which the stamp never covered.
	defaults["priority"] = int64(3)
	served := created.DeepCopy()
	served.Object["spec"].(map[string]any)["priority"] = int64(3)
	if got, ok, reason := verifiedRequester(served, s); !ok {
		t.Fatalf("a stored object served with a later schema default must still verify: %s", reason)
	} else if got.Username != "alice" || got.UID != "alice-uid" {
		t.Errorf("verified identity %+v, want the bound alice/alice-uid", got)
	}

	// A tamper to a non-default value fails both signatures.
	tampered := served.DeepCopy()
	tampered.Object["spec"].(map[string]any)["chart"] = "other"
	if _, ok, _ := verifiedRequester(tampered, s); ok {
		t.Error("a tampered non-default value must be refused")
	}

	// A write of a NON-default value into a previously-defaulted field is
	// covered by the projection now — and by the legacy MAC's full coverage.
	changed := served.DeepCopy()
	changed.Object["spec"].(map[string]any)["timeout"] = "10m"
	if _, ok, _ := verifiedRequester(changed, s); ok {
		t.Error("replacing a defaulted value with a non-default one must be refused")
	}

	// Without a schema source nothing is stripped (fail-safe full coverage):
	// the same injected default then breaks verification, exactly as the
	// pre-migration binding would.
	setSchemaDefaultStripper(t, nil)
	if _, ok, _ := verifiedRequester(served, s); ok {
		t.Error("with no schema source the v2 stamp must keep full spec coverage")
	}
}

// TestStripSchemaDefaultsWalk pins the projection the versioned binding covers:
// fields equal to their schema default drop out recursively, fields the schema
// does not describe always stay, and numbers compare by value across the int64
// an unstructured decode yields and the float64 a plain JSON decode yields.
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

	// A value that is itself the default drops out whole.
	if _, keep := stripSchemaDefaults("5m", map[string]any{"default": "5m"}); keep {
		t.Error("a scalar equal to its schema default must drop out of the projection")
	}
	// A non-default value in a defaulted field stays.
	if v, keep := stripSchemaDefaults("10m", map[string]any{"default": "5m"}); !keep || v != "10m" {
		t.Errorf("stripSchemaDefaults(10m) = %v keep=%v, want the value kept", v, keep)
	}
}

// TestSchemaDefaultStripperFromCRD: the stripper derives from the storage
// version's openAPIV3Schema — not the first version — and refuses to produce a
// source from a schema without a spec.
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
