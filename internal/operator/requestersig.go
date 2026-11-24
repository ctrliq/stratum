package operator

// Requester stamps fail closed: a deleted object's old stamp cannot be replayed on recreation.

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
	annRequesterSig      = group + "/requester-sig"
	annRequesterSigV2    = group + "/requester-sig-v2"
	annRequesterKeyID    = group + "/requester-key-id"
	annRequesterFieldsV2 = group + "/requester-fields-v2"

	// MAC'd length-prefixed first, so a v2 MAC cannot collide with a legacy MAC.
	stampBindingV2 = "v2-hmac-sha256"

	requesterKeySecret           = "stratum-operator-requester-hmac"
	requesterKeyField            = "key"
	requesterBindingVersionField = "binding-version"
	requesterKeysField           = "retained-keys"
	requesterKeyBytes            = 32

	requesterKeyWatchInterval = 30 * time.Second
)

// A variable so tests can shrink the wait; production keeps the ten-second default.
var requesterKeyRetryWait = 10 * time.Second

type requesterSigner struct {
	bindingVersion string
	key            []byte
	// Empty for a signer built without a Secret; the id derives from the key material.
	id   string
	keys map[string][]byte
}

func (s *requesterSigner) current() *requesterSigner {
	return s
}

type signerSnapshot interface {
	current() *requesterSigner
}

// A published snapshot is never mutated, so a reader keeps exactly the signer it took.
type requesterSignerHolder struct {
	cur atomic.Pointer[requesterSigner]
	// Serializes refreshes so a slower one cannot publish an older state after a newer one.
	refreshGate chan struct{}
}

func newRequesterSignerHolder() *requesterSignerHolder {
	return &requesterSignerHolder{refreshGate: make(chan struct{}, 1)}
}

func (h *requesterSignerHolder) current() *requesterSigner {
	return h.cur.Load()
}

func (h *requesterSignerHolder) publish(signer *requesterSigner) {
	h.cur.Store(signer)
}

// uid arrives after CREATE admission; json.Marshal's sorted map keys keep the binding byte-stable.
func requesterBinding(obj *unstructured.Unstructured) string {
	payload, _ := json.Marshal([]interface{}{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), obj.Object["spec"]})
	return string(payload)
}

// Nil strips nothing until live; admission and reconcile read it while the signer owner refreshes.
var schemaDefaultStripper atomic.Value // of func(spec any) any

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

// Released legacy-stamp field set: never CRD-derived; extend only after stored stamps migrate.
func legacyRequesterField(field string) bool {
	switch field {
	case "chart", "repo", "version", "chartContent", "releaseName", "targetNamespace",
		"createNamespace", "valuesContent", "set", "setString", "timeout", "repoCA", "insecureSkipTLSVerify":
		return true
	}
	return false
}

var helmChartCRDName = helmChartGVR.Resource + "." + helmChartGVR.Group

// Best effort: a schema read error keeps the previous source until the next refresh.
func wireSchemaDefaultStripper(ctx context.Context, cs kubernetes.Interface) {
	strip, err := fetchSchemaDefaultStripper(ctx, cs)
	if err != nil {
		log.Printf("stratum-operator: read %s schema defaults (the v2 stamp keeps full spec coverage): %v", helmChartCRDName, err)
		return
	}
	schemaDefaultStripper.Store(strip)
}

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

// Drops every field whose value equals its schema default; keep is false for the value itself.
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

// Exact comparison across representations: float64 would equate neighboring int64s above 2^53.
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

// Shape is frozen: every stamp already stored on a tenant HelmChart was signed this way.
func legacyMac(key []byte, objUID string, info requesterInfo) string {
	groupsJSON, _ := json.Marshal(canonicalGroups(info.Groups))
	mac := hmac.New(sha256.New, key)
	writeField(mac, objUID)
	writeField(mac, info.Username)
	writeField(mac, info.UID)
	writeField(mac, string(groupsJSON))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}

func (s *requesterSigner) sign(objUID string, info requesterInfo) string {
	return legacyMac(s.key, objUID, info)
}

func (s *requesterSigner) verify(objUID string, info requesterInfo, sig string) bool {
	want := s.sign(objUID, info)
	return hmac.Equal([]byte(want), []byte(sig))
}

// A legacy stamp names no key id, so verification must try the whole keyring.
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

func (s *requesterSigner) verifyCompatibleLegacy(obj *unstructured.Unstructured, info requesterInfo, sig string) bool {
	return s.verifyAny(requesterBinding(obj), info, sig) || s.verifyAny(legacyCompatibleBinding(obj), info, sig)
}

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

func (s *requesterSigner) signV2(objUID string, info requesterInfo) string {
	return v2Mac(s.key, objUID, info)
}

func (s *requesterSigner) verifyV2(objUID string, info requesterInfo, sig string, key []byte) bool {
	want := v2Mac(key, objUID, info)
	return hmac.Equal([]byte(want), []byte(sig))
}

func (s *requesterSigner) currentID() string {
	if s.id != "" {
		return s.id
	}
	return derivedKeyID(s.key)
}

func (s *requesterSigner) keyByID(id string) ([]byte, bool) {
	if key, ok := s.keys[id]; ok {
		return key, true
	}
	if id == s.currentID() {
		return s.key, true
	}
	return nil, false
}

// A digest of the key material, so the same key always derives the same id.
func derivedKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return "sha256-" + hex.EncodeToString(sum[:8])
}

// writeField length-prefixes a field so MAC field boundaries are unambiguous.
func writeField(mac interface{ Write([]byte) (int, error) }, s string) {
	var lp [4]byte
	n := len(s)
	lp[0], lp[1], lp[2], lp[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	_, _ = mac.Write(lp[:])
	_, _ = mac.Write([]byte(s))
}

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

// An absent map is not an error: a pre-keying Secret carries only the legacy key field.
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

func encodeKeyring(keyring map[string][]byte) map[string]string {
	encoded := make(map[string]string, len(keyring))
	for id, key := range keyring {
		encoded[id] = base64.StdEncoding.EncodeToString(key)
	}
	return encoded
}

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
	wireSchemaDefaultStripper(ctx, cs)
	sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(ctx, requesterKeySecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if cur := h.current(); cur == nil {
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
		log.Printf("stratum-operator: read requester-signing Secret %s/%s: %v; keeping the current signer state", webhookNamespace, requesterKeySecret, err)
		return
	}
	publishRefreshedSigner(ctx, cs, h, sec)
}

// A deleted Secret is not revocation; recreating from the held key keeps key identity.
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

// A replaced Secret carries only its new key; stamps under previous keys must keep verifying.
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

// Every verifier is retained durably before a replacement key becomes active.
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
