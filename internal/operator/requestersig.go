package operator

// Requester stamps authenticate desired state and identity. Admission fails
// closed so a deleted object's old stamp cannot be replayed on recreation.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"log"
	"math/big"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// annRequesterSig is the base64 HMAC that proves the requester stamp was
	// authored by the webhook, not forged by a client.
	annRequesterSig = group + "/requester-sig"
	// annRequesterSigV2 authenticates the admitted field policy and values under
	// the versioned binding, with the key annRequesterKeyID names. Signing it
	// requires explicit activation after all supported verifiers have adopted.
	// The legacy signature remains alongside it for compatibility.
	annRequesterSigV2 = group + "/requester-sig-v2"
	// annRequesterKeyID names the key that produced the v2 signature, so
	// verification survives rotations that retain the older keys.
	annRequesterKeyID = group + "/requester-key-id"
	// annRequesterFieldsV2 is a sorted JSON list of admitted spec field names,
	// authenticated inside the versioned MAC so defaults cannot change coverage.
	annRequesterFieldsV2 = group + "/requester-fields-v2"

	// stampBindingV2 is the version+algorithm header of the versioned binding.
	// It is MAC'd length-prefixed FIRST, which is what keeps a v2 MAC from
	// colliding with a legacy MAC over the same payload.
	stampBindingV2 = "v2-hmac-sha256"

	requesterKeySecret = "stratum-operator-requester-hmac"
	requesterKeyField  = "key"
	// Set only after every supported operator runs a verifier for this binding.
	// An absent value keeps legacy-only signing during the reader rollout.
	requesterBindingVersionField = "binding-version"
	// requesterKeysField holds the retained-key map in the signing Secret: a
	// JSON object of key id to std-base64 key, covering every key whose stamps
	// may still be stored on cluster objects.
	requesterKeysField = "retained-keys"
	requesterKeyBytes  = 32

	// requesterKeyWatchInterval is how often the signer owner polls the signing
	// Secret so a replaced Secret's key is reloaded without a process restart.
	requesterKeyWatchInterval = 30 * time.Second
)

// requesterKeyRetryWait spaces the startup key load's attempts. It is a variable
// so tests can shrink the wait; production keeps the ten-second default.
var requesterKeyRetryWait = 10 * time.Second

// requesterSigner signs requester stamps with its current key and verifies
// stamps under its whole keyring: the current key plus every retained one.
type requesterSigner struct {
	bindingVersion string
	// key is the CURRENT signing key; every signature this signer produces
	// uses it.
	key []byte
	// id is the identifier the signing Secret records for key. Empty for a
	// signer built without a Secret, whose id is derived from the key
	// material on demand.
	id string
	// keys is the retained-key ring by id: every key whose stamps may still be
	// stored on cluster objects. Always contains the current key; nil for a
	// signer built without a Secret.
	keys map[string][]byte
}

// current lets a fixed signer serve where a snapshot source is expected: it is
// its own snapshot.
func (s *requesterSigner) current() *requesterSigner {
	return s
}

// signerSnapshot is the per-use source of the signer snapshot: the admission
// handler reads it once per admission. *requesterSignerHolder hands out the
// latest published snapshot; a fixed *requesterSigner is its own.
type signerSnapshot interface {
	current() *requesterSigner
}

// requesterSignerHolder publishes immutable signer snapshots to concurrent
// readers: the admission webhook reads one per admission and the reconciler one
// per pass. Publication is an atomic pointer swap — a plain pointer field races
// with readers — and a snapshot is never mutated once published, so a reader
// that took one keeps exactly the signer it took.
type requesterSignerHolder struct {
	cur atomic.Pointer[requesterSigner]
	// Serialize Secret reads through publication so a slower refresh cannot
	// publish an older signing state after a newer refresh has completed.
	refreshGate chan struct{}
}

// newRequesterSignerHolder returns an empty holder: current() is nil (fail
// closed) until the first snapshot is published.
func newRequesterSignerHolder() *requesterSignerHolder {
	return &requesterSignerHolder{refreshGate: make(chan struct{}, 1)}
}

// current returns the latest published snapshot, or nil before one is.
func (h *requesterSignerHolder) current() *requesterSigner {
	return h.cur.Load()
}

// publish makes signer the latest snapshot. Earlier snapshots are untouched.
func (h *requesterSignerHolder) publish(signer *requesterSigner) {
	h.cur.Store(signer)
}

// requesterBinding uses fields available during CREATE admission. Kubernetes
// assigns metadata.uid only afterwards. JSON encoding sorts map keys.
func requesterBinding(obj *unstructured.Unstructured) string {
	payload, _ := json.Marshal([]interface{}{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), obj.Object["spec"]})
	return string(payload)
}

// schemaDefaultStripper holds the versioned binding's schema source: the
// function that projects a spec onto what the v2 stamp covers. The operator
// wires it from its clientset (wireSchemaDefaultStripper) when it loads or
// refreshes its signer; nil strips nothing, so until the source is live the v2
// binding covers the full spec — the legacy binding's exact coverage. Guarded
// by atomic.Value because admission and reconciliation read it per use while
// the signer owner refreshes the served schema.
var schemaDefaultStripper atomic.Value // of func(spec any) any

// bindingV2 authenticates the exact field set admitted with the object, then
// every admitted field's value. A later default may be projected away only for
// a field absent from that authenticated set. Mutable defaults can therefore
// never remove coverage of a value present when the requester authorized it.
func bindingV2(obj *unstructured.Unstructured) string {
	policy := obj.GetAnnotations()[annRequesterFieldsV2]
	if policy == "" {
		policy = requesterFieldPolicy(obj)
	}
	payload, _ := json.Marshal([]interface{}{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), policy, requesterProjectedSpec(obj.Object["spec"], requesterPolicyFields(policy))})
	return string(payload)
}

func requesterFieldPolicy(obj *unstructured.Unstructured) string {
	spec, _ := obj.Object["spec"].(map[string]any)
	fields := make([]string, 0, len(spec))
	for field := range spec {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func requesterPolicyFields(policy string) map[string]bool {
	var fields []string
	if json.Unmarshal([]byte(policy), &fields) != nil {
		return nil
	}
	known := make(map[string]bool, len(fields))
	for _, field := range fields {
		known[field] = true
	}
	return known
}

func requesterProjectedSpec(spec any, admitted map[string]bool) any {
	values, ok := spec.(map[string]any)
	if !ok {
		return spec
	}
	known, future := make(map[string]any), make(map[string]any)
	for field, value := range values {
		if admitted[field] {
			known[field] = value
		} else {
			future[field] = value
		}
	}
	if strip, ok := schemaDefaultStripper.Load().(func(spec any) any); ok && strip != nil {
		if projected, ok := strip(future).(map[string]any); ok {
			future = projected
		}
	}
	for field, value := range future {
		known[field] = value
	}
	return known
}

func legacyCompatibleBinding(obj *unstructured.Unstructured) string {
	spec, _ := obj.Object["spec"].(map[string]any)
	baseline := make(map[string]bool)
	for field := range spec {
		if legacyRequesterField(field) {
			baseline[field] = true
		}
	}
	payload, _ := json.Marshal([]interface{}{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), requesterProjectedSpec(obj.Object["spec"], baseline)})
	return string(payload)
}

// This released field set is the compatibility policy for legacy stamps that
// carry no admitted-field metadata. Never derive it from the mutable CRD or
// controller field table. Migrate legacy stamps before extending their schema.
func legacyRequesterField(field string) bool {
	switch field {
	case "chart", "repo", "version", "chartContent", "releaseName", "targetNamespace",
		"createNamespace", "valuesContent", "set", "setString", "timeout", "repoCA", "insecureSkipTLSVerify":
		return true
	}
	return false
}

// helmChartCRDName is the CustomResourceDefinition whose currently served
// schema decides the defaults the versioned binding projects away.
var helmChartCRDName = helmChartGVR.Resource + "." + helmChartGVR.Group

// wireSchemaDefaultStripper points the versioned binding's schema source at the
// HelmChart CRD as the cluster currently serves it. The operator wires it from
// its clientset whenever it loads or refreshes its signer, so a schema change
// the apiserver already applies on read is picked up without a restart. Best
// effort, never fail-closed: when the CRD cannot be read the previous source is
// kept (initially nil — nothing stripped, full spec coverage). A schema read
// error delays additive-default compatibility until the next refresh.
func wireSchemaDefaultStripper(ctx context.Context, cs kubernetes.Interface) {
	strip, err := fetchSchemaDefaultStripper(ctx, cs)
	if err != nil {
		log.Printf("stratum-operator: read %s schema defaults (the v2 stamp keeps full spec coverage): %v", helmChartCRDName, err)
		return
	}
	schemaDefaultStripper.Store(strip)
}

// fetchSchemaDefaultStripper reads the HelmChart CRD as the cluster currently
// serves it and returns its default-stripper without installing it — for a
// caller that wants to re-derive the source per use or at a chosen moment.
func fetchSchemaDefaultStripper(ctx context.Context, cs kubernetes.Interface) (func(spec any) any, error) {
	rc := cs.Discovery().RESTClient()
	if rc == nil {
		return nil, fmt.Errorf("no discovery client")
	}
	raw, err := rc.Get().AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + helmChartCRDName).Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	return schemaDefaultStripperFromCRD(raw)
}

// schemaDefaultStripperFromCRD builds the default-stripper from one CRD
// document: the storage version's openAPIV3Schema decides the defaults.
func schemaDefaultStripperFromCRD(raw []byte) (func(spec any) any, error) {
	var crd struct {
		Spec struct {
			Versions []struct {
				Name    string `json:"name"`
				Served  bool   `json:"served"`
				Storage bool   `json:"storage"`
				Schema  struct {
					OpenAPIV3Schema json.RawMessage `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &crd); err != nil {
		return nil, fmt.Errorf("decode CRD: %w", err)
	}
	if len(crd.Spec.Versions) == 0 {
		return nil, fmt.Errorf("CRD declares no versions")
	}
	chosen := 0
	for i, v := range crd.Spec.Versions {
		if v.Storage {
			chosen = i
			break
		}
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(crd.Spec.Versions[chosen].Schema.OpenAPIV3Schema))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode CRD schema: %w", err)
	}
	props, _ := root["properties"].(map[string]any)
	specSchema, _ := props["spec"].(map[string]any)
	if specSchema == nil {
		return nil, fmt.Errorf("CRD schema has no spec property to derive defaults from")
	}
	return func(spec any) any {
		out, keep := stripSchemaDefaults(spec, specSchema)
		if !keep {
			return map[string]any{}
		}
		return out
	}, nil
}

// stripSchemaDefaults returns the projection of value that the versioned
// binding covers: value with every field whose value equals its structural
// schema default removed, recursively. keep is false when value itself equals
// its schema default and drops out of the binding whole. Fields the schema does
// not describe carry no default and are always kept, so genuine tampering to a
// non-default value stays covered.
func stripSchemaDefaults(value any, schema map[string]any) (out any, keep bool) {
	if schema == nil {
		return value, true
	}
	if def, ok := schema["default"]; ok && jsonEqual(value, def) {
		return nil, false
	}
	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		additional, _ := schema["additionalProperties"].(map[string]any)
		stripped := make(map[string]any, len(v))
		for k, fv := range v {
			sub, _ := props[k].(map[string]any)
			if sub == nil {
				sub = additional
			}
			if sv, subKeep := stripSchemaDefaults(fv, sub); subKeep {
				stripped[k] = sv
			}
		}
		return stripped, true
	case []any:
		items, _ := schema["items"].(map[string]any)
		stripped := make([]any, 0, len(v))
		for _, ev := range v {
			if sv, subKeep := stripSchemaDefaults(ev, items); subKeep {
				stripped = append(stripped, sv)
			}
		}
		return stripped, true
	}
	return value, true
}

// jsonEqual compares two JSON-decoded values the way the apiserver serves them:
// numbers compare by value across representations (unstructured decoding yields
// int64, schema decoding preserves json.Number). Exact rational comparison
// prevents neighboring int64 values above 2^53 from being treated as equal.
func jsonEqual(a, b any) bool {
	if fa, ok := jsonNumber(a); ok {
		fb, ok := jsonNumber(b)
		return ok && fa.Cmp(fb) == 0
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bw, ok := bv[k]
			if !ok || !jsonEqual(v, bw) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i, v := range av {
			if !jsonEqual(v, bv[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

// jsonNumber reports the value of any JSON number representation.
func jsonNumber(v any) (*big.Rat, bool) {
	var encoded string
	switch n := v.(type) {
	case int:
		encoded = strconv.Itoa(n)
	case int64:
		encoded = strconv.FormatInt(n, 10)
	case float64:
		encoded = strconv.FormatFloat(n, 'g', -1, 64)
	case json.Number:
		encoded = string(n)
	default:
		return nil, false
	}
	value, ok := new(big.Rat).SetString(encoded)
	return value, ok
}

func canonicalGroups(groups []string) []string {
	out := slices.Clone(groups)
	slices.Sort(out)
	return slices.Compact(out)
}

// legacyMac is the pre-migration MAC over (objUID, info): length-delimited
// fields and a canonical group set. It must never change shape — every stamp
// already stored on a tenant HelmChart was signed exactly this way.
func legacyMac(key []byte, objUID string, info requesterInfo) string {
	groupsJSON, _ := json.Marshal(canonicalGroups(info.Groups))
	mac := hmac.New(sha256.New, key)
	writeField(mac, objUID)
	writeField(mac, info.Username)
	writeField(mac, info.UID)
	writeField(mac, string(groupsJSON))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}

// sign uses length-delimited fields and a canonical group set.
func (s *requesterSigner) sign(objUID string, info requesterInfo) string {
	return legacyMac(s.key, objUID, info)
}

// verify reports whether sig is this signer's MAC over (objUID, info), in
// constant time.
func (s *requesterSigner) verify(objUID string, info requesterInfo, sig string) bool {
	want := s.sign(objUID, info)
	return hmac.Equal([]byte(want), []byte(sig))
}

// verifyAny reports whether sig is a legacy MAC under ANY key the signer holds —
// the current key plus every retained one. A legacy stamp names no key id, so
// verification must try the whole keyring: a stamp signed before a rotation
// keeps verifying as long as its key is retained.
func (s *requesterSigner) verifyAny(objUID string, info requesterInfo, sig string) bool {
	if s.verify(objUID, info, sig) {
		return true
	}
	for _, key := range s.keys {
		if hmac.Equal([]byte(legacyMac(key, objUID, info)), []byte(sig)) {
			return true
		}
	}
	return false
}

// verifyCompatibleLegacy first checks the released full-spec binding, then
// applies only the frozen policy's additive-default compatibility projection.
// Known fields retain their exact admitted values under both attempts.
func (s *requesterSigner) verifyCompatibleLegacy(obj *unstructured.Unstructured, info requesterInfo, sig string) bool {
	return s.verifyAny(requesterBinding(obj), info, sig) || s.verifyAny(legacyCompatibleBinding(obj), info, sig)
}

// v2Mac is the versioned binding's MAC: the length-prefixed version+algorithm
// header first, then the unchanged legacy field sequence. The header is what
// keeps a v2 MAC from colliding with a legacy MAC over the same payload.
func v2Mac(key []byte, objUID string, info requesterInfo) string {
	groupsJSON, _ := json.Marshal(canonicalGroups(info.Groups))
	mac := hmac.New(sha256.New, key)
	writeField(mac, stampBindingV2)
	writeField(mac, objUID)
	writeField(mac, info.Username)
	writeField(mac, info.UID)
	writeField(mac, string(groupsJSON))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}

// signV2 signs with the versioned binding under the signer's current key.
func (s *requesterSigner) signV2(objUID string, info requesterInfo) string {
	return v2Mac(s.key, objUID, info)
}

// verifyV2 reports whether sig is the v2 MAC under key over (objUID, info), in
// constant time. The caller resolves the stamp's key identifier to key.
func (s *requesterSigner) verifyV2(objUID string, info requesterInfo, sig string, key []byte) bool {
	want := v2Mac(key, objUID, info)
	return hmac.Equal([]byte(want), []byte(sig))
}

// currentID is the key identifier this signer stamps: the id the signing Secret
// records for the current key, or the material-derived id for a key the Secret
// does not name.
func (s *requesterSigner) currentID() string {
	if s.id != "" {
		return s.id
	}
	return derivedKeyID(s.key)
}

// keyByID resolves a stamp's key identifier to its verification key. A signer
// built without a Secret (nil key ring) still resolves its own derived id, so
// the stamps it signed verify against itself.
func (s *requesterSigner) keyByID(id string) ([]byte, bool) {
	if key, ok := s.keys[id]; ok {
		return key, true
	}
	if id == s.currentID() {
		return s.key, true
	}
	return nil, false
}

// derivedKeyID names a key that outlives reloads: a digest of the key material
// itself, so a signing Secret that predates the keyed layout still stamps a
// stable key identifier (the same key always derives the same id).
func derivedKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return "sha256-" + hex.EncodeToString(sum[:8])
}

// writeField appends a length-prefixed field to the MAC so field boundaries are
// unambiguous (e.g. user "a" + group "b" cannot equal user "ab" + group "").
func writeField(mac interface{ Write([]byte) (int, error) }, s string) {
	var lp [4]byte
	n := len(s)
	lp[0], lp[1], lp[2], lp[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	_, _ = mac.Write(lp[:])
	_, _ = mac.Write([]byte(s))
}

// ensureRequesterSigner loads the shared HMAC key from its kube-system Secret,
// creating it (with a random key) if absent. Every control-plane operator calls
// this; a create race is resolved by re-reading the winner's key.
//
// The Secret carries the current key under requesterKeyField and the retained
// key ring under requesterKeysField. A Secret written before the keyed layout
// has only the legacy field; loading one derives the key's stable identifier
// and records the key ring back into the Secret, so a later rotation retains
// this key's verifiers.
func ensureRequesterSigner(ctx context.Context, cs kubernetes.Interface) (*requesterSigner, error) {
	wireSchemaDefaultStripper(ctx, cs)
	secrets := cs.CoreV1().Secrets(webhookNamespace)
	sec, err := secrets.Get(ctx, requesterKeySecret, metav1.GetOptions{})
	if err == nil {
		return loadRequesterSignerFromSecret(ctx, cs, sec)
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	key := make([]byte, requesterKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	id := derivedKeyID(key)
	raw, err := json.Marshal(map[string]string{id: base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		return nil, err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      requesterKeySecret,
			Namespace: webhookNamespace,
			Labels:    map[string]string{group + "/managed-by": fieldManager},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{requesterKeyField: key, requesterKeysField: raw},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Another operator created it first; use its key.
		sec, gerr := secrets.Get(ctx, requesterKeySecret, metav1.GetOptions{})
		if gerr != nil {
			return nil, gerr
		}
		return loadRequesterSignerFromSecret(ctx, cs, sec)
	}
	if err != nil {
		return nil, err
	}
	return &requesterSigner{key: key, id: id, keys: map[string][]byte{id: key}}, nil
}

// loadRequesterSignerFromSecret records the complete keyring before using it.
func loadRequesterSignerFromSecret(ctx context.Context, cs kubernetes.Interface, sec *corev1.Secret) (*requesterSigner, error) {
	signer, err := requesterSignerFromSecret(sec)
	if err != nil {
		return nil, err
	}
	if err := persistRetainedKeys(ctx, cs, sec, signer.keys); err != nil {
		return nil, err
	}
	return signer, nil
}

// requesterSignerFromSecret decodes one Secret snapshot without mutating it.
// The current key joins the retained ring under its stable material-derived id
// when the Secret does not already name it. The caller persists the complete
// ring before publishing a snapshot that can sign with a replacement key.
func requesterSignerFromSecret(sec *corev1.Secret) (*requesterSigner, error) {
	key := sec.Data[requesterKeyField]
	if len(key) == 0 {
		return nil, fmt.Errorf("requester-signing Secret %s/%s has no %q", webhookNamespace, requesterKeySecret, requesterKeyField)
	}
	keyring, err := decodeKeyring(sec.Data[requesterKeysField])
	if err != nil {
		return nil, err
	}
	currentID := matchKeyringID(keyring, key)
	if currentID == "" {
		currentID = derivedKeyID(key)
		keyring[currentID] = key

	}
	version := string(sec.Data[requesterBindingVersionField])
	if version != "" && version != stampBindingV2 {
		return nil, fmt.Errorf("unsupported requester binding version %q", version)
	}
	return &requesterSigner{key: key, id: currentID, keys: keyring, bindingVersion: version}, nil
}

// decodeKeyring parses the Secret's retained-key map (key id to std-base64
// key). An absent map is not an error: a pre-keying Secret carries only the
// legacy key field.
func decodeKeyring(raw []byte) (map[string][]byte, error) {
	if len(raw) == 0 {
		return map[string][]byte{}, nil
	}
	var encoded map[string]string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, fmt.Errorf("decode requester retained-key map: %w", err)
	}
	keyring := make(map[string][]byte, len(encoded))
	for id, b64 := range encoded {
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("decode requester retained key %q: %w", id, err)
		}
		if len(key) > 0 {
			keyring[id] = key
		}
	}
	return keyring, nil
}

// encodeKeyring renders the retained-key map for the signing Secret.
func encodeKeyring(keyring map[string][]byte) map[string]string {
	encoded := make(map[string]string, len(keyring))
	for id, key := range keyring {
		encoded[id] = base64.StdEncoding.EncodeToString(key)
	}
	return encoded
}

// matchKeyringID returns the id the key ring records for key, preferring the
// lexicographically smallest when several entries share the key material.
func matchKeyringID(keyring map[string][]byte, key []byte) string {
	var ids []string
	for id, k := range keyring {
		if hmac.Equal(k, key) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	slices.Sort(ids)
	return ids[0]
}

// loadRequesterSignerWithRetry loads the signer, retrying a few times against a
// transient apiserver error at startup. Returns nil (fail closed) if it cannot
// load before ctx is cancelled or the attempts are exhausted; ctx is honored
// before and between every attempt, so a shutdown never waits out a retry gap.
func loadRequesterSignerWithRetry(ctx context.Context, cs kubernetes.Interface) *requesterSigner {
	const attempts = 6
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return nil
		}
		signer, err := ensureRequesterSigner(ctx, cs)
		if err == nil {
			return signer
		}
		log.Printf("stratum-operator: load requester-signing key (attempt %d/%d): %v", i+1, attempts, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(requesterKeyRetryWait):
		}
	}
	return nil
}

// maintainRequesterSigner keeps the holder's snapshot current for the lifetime of
// ctx: it loads the signing key immediately, then polls the signing Secret every
// interval (the pattern of the webhook delivery owner in webhookdeploy.go), so a
// replaced Secret's key is reloaded and re-published without a process restart.
// A transient read error changes nothing — the current verifier set keeps
// signing and verifying. A deleted Secret is re-created from the held snapshot
// (see recreateRequesterSecret). A first load with no key at all keeps retrying
// on the same ticker, so the operator is never left keyless-and-silent, and
// every wait watches ctx.Done so shutdown is prompt.
func maintainRequesterSigner(ctx context.Context, cs kubernetes.Interface, h *requesterSignerHolder, interval time.Duration) {
	if interval <= 0 {
		interval = requesterKeyWatchInterval
	}
	refresh := func() { refreshRequesterSigner(ctx, cs, h) }
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// refreshRequesterSigner shares one serialized refresh path between the
// periodic owner and reconciliation recovering from a lagging verifier.
func refreshRequesterSigner(ctx context.Context, cs kubernetes.Interface, h *requesterSignerHolder) {
	select {
	case <-ctx.Done():
		return
	case h.refreshGate <- struct{}{}:
	}
	defer func() { <-h.refreshGate }()
	if ctx.Err() != nil {
		return
	}
	// Re-point the versioned binding's schema source at the CRD as the
	// cluster currently serves it, so a schema change is picked up with
	// the same cadence as the key (best effort; see the function).
	wireSchemaDefaultStripper(ctx, cs)
	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(ctx, requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if cur := h.current(); cur == nil {
				// No key loaded yet and no Secret: bring the shared key up
				// exactly as startup does; a create race converges on the
				// winner's key.
				signer, err := ensureRequesterSigner(ctx, cs)
				if err != nil {
					log.Printf("stratum-operator: load requester-signing key: %v (retrying)", err)
					return
				}
				h.publish(signer)
				log.Printf("stratum-operator: requester-signing key %s published", signer.currentID())
				return
			}
			recreateRequesterSecret(ctx, cs, h.current())
			return
		}
		// A failed read keeps the current verifier set; the next tick retries.
		log.Printf("stratum-operator: read requester-signing Secret %s/%s: %v; keeping the current signer state", webhookNamespace, requesterKeySecret, err)
		return
	}
	publishRefreshedSigner(ctx, cs, h, sec)
}

// recreateRequesterSecret re-creates the signing Secret from the snapshot the
// holder keeps: the current key under requesterKeyField and the retained ring
// under requesterKeysField, exactly the shape ensureRequesterSigner reads.
//
// Nothing in the cluster restores a deleted signing Secret, and the operator
// that holds a key has the only copy: leaving the Secret deleted means the
// next restart mints a fresh random key over the absence, and every stamp
// already stored — legacy and migrated — then fails verification as "forged or
// tampered", wedging tenant installs and uninstalls cluster-wide. Re-creating
// from the held key keeps the key identity stable across that restart; a
// create race against another operator's recreation loses here and the next
// tick reconciles through the normal read path. The deletion is logged loudly:
// an administrator deleting the Secret to revoke a key needs to know the
// running operators put it back (revocation is a stop-all-operators-and-edit
// procedure, not a Secret delete).
func recreateRequesterSecret(ctx context.Context, cs kubernetes.Interface, signer *requesterSigner) {
	log.Printf("stratum-operator: requester-signing Secret %s/%s deleted; re-creating it from the held key %s so stored stamps keep verifying across restarts (deleting the Secret does not revoke a key)", webhookNamespace, requesterKeySecret, signer.currentID())
	raw, err := json.Marshal(encodeKeyring(signer.keys))
	if err != nil {
		log.Printf("stratum-operator: encode retained-key map for Secret %s/%s: %v (keeping the current signer state; the next tick retries)", webhookNamespace, requesterKeySecret, err)
		return
	}
	_, err = cs.CoreV1().Secrets(webhookNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      requesterKeySecret,
			Namespace: webhookNamespace,
			Labels:    map[string]string{group + "/managed-by": fieldManager},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{requesterKeyField: signer.key, requesterKeysField: raw, requesterBindingVersionField: []byte(signer.bindingVersion)},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		log.Printf("stratum-operator: re-create requester-signing Secret %s/%s: %v (keeping the current signer state; the next tick retries)", webhookNamespace, requesterKeySecret, err)
	}
}

// publishRefreshedSigner builds a signer from one read of the signing Secret and
// republishes it when the signing key changed. The previous snapshot's verifier
// set is retained — a replaced Secret carries only its new key, and stamps
// already stored under the previous keys must keep verifying — and the merged
// ring is written back to the Secret so the retained verifiers survive a restart.
func publishRefreshedSigner(ctx context.Context, cs kubernetes.Interface, h *requesterSignerHolder, sec *corev1.Secret) {
	if ctx.Err() != nil {
		return
	}
	fresh, err := requesterSignerFromSecret(sec)
	if err != nil {
		log.Printf("stratum-operator: load requester-signing key: %v (keeping the current verifier set)", err)
		return
	}
	prev := h.current()
	keyring := fresh.keys
	if prev != nil {
		keyring = make(map[string][]byte, len(fresh.keys)+len(prev.keys))
		for id, key := range fresh.keys {
			keyring[id] = key
		}
		for id, key := range prev.keys {
			if replacement, ok := keyring[id]; ok && !hmac.Equal(replacement, key) {
				log.Printf("stratum-operator: retained requester key ID %q was reused with different material; choose a fresh key ID (keeping the current signer state)", id)
				return
			} else if !ok {
				keyring[id] = key
			}
		}
	}
	if err := persistRetainedKeys(ctx, cs, sec, keyring); err != nil {
		log.Printf("stratum-operator: retain requester verification keys before activation: %v (keeping the current signer state)", err)
		return
	}
	if prev != nil && bytes.Equal(prev.key, fresh.key) && sameKeyring(prev.keys, keyring) && prev.bindingVersion == fresh.bindingVersion {
		return
	}
	if ctx.Err() != nil {
		return
	}
	h.publish(&requesterSigner{key: fresh.key, id: fresh.id, keys: keyring, bindingVersion: fresh.bindingVersion})
	log.Printf("stratum-operator: requester-signing key %s published from Secret %s/%s", fresh.currentID(), webhookNamespace, requesterKeySecret)
}

// persistRetainedKeys records every required verifier before a replacement
// signing key can become active. On failure the owner retries with its previous
// immutable snapshot rather than activating a key ahead of durable retention.
func persistRetainedKeys(ctx context.Context, cs kubernetes.Interface, sec *corev1.Secret, keyring map[string][]byte) error {
	have, err := decodeKeyring(sec.Data[requesterKeysField])
	if err == nil && sameKeyring(have, keyring) {
		return nil
	}
	raw, err := json.Marshal(encodeKeyring(keyring))
	if err != nil {
		return fmt.Errorf("encode retained requester keys: %w", err)
	}
	updated := sec.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string][]byte{}
	}
	updated.Data[requesterKeysField] = raw
	if _, err := cs.CoreV1().Secrets(webhookNamespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("persist retained requester keys: %w", err)
	}
	return nil
}

// sameKeyring reports whether two retained-key maps carry identical material.
func sameKeyring(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for id, key := range a {
		other, ok := b[id]
		if !ok || !bytes.Equal(key, other) {
			return false
		}
	}
	return true
}
