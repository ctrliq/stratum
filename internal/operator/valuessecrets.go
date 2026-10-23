package operator

// HelmChart spec.valuesSecrets: values read from Secrets in the HelmChart's
// namespace, so a value such as a password need not sit in the CR, where
// anyone who can read HelmCharts can read it and encryption at rest does not
// reach it.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

// valuesSecretRef is one spec.valuesSecrets entry: a Secret in the HelmChart's
// namespace and the keys of it that each hold a values.yaml body, merged in
// order.
type valuesSecretRef struct {
	name string
	keys []string
}

// parseValuesSecrets reads spec.valuesSecrets. The CRD requires a name and at
// least one key on every entry; an entry without them is kept as it is, so the
// read fails on it and says so rather than skipping it.
func parseValuesSecrets(v interface{}) []valuesSecretRef {
	items, _ := v.([]interface{})
	out := make([]valuesSecretRef, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]interface{})
		ref := valuesSecretRef{name: str(m["name"], "")}
		keys, _ := m["keys"].([]interface{})
		for _, k := range keys {
			if s, ok := k.(string); ok {
				ref.keys = append(ref.keys, s)
			}
		}
		out = append(out, ref)
	}
	return out
}

// readValuesSecrets reads every listed key of every spec.valuesSecrets entry
// from namespace ns, in order, as values maps. versions identifies what was
// read (each Secret's name, keys, UID and resourceVersion) for specHash, which
// must change when a Secret does without carrying the values: the hash is in
// the HelmChart's status, readable by anyone who can read the HelmChart.
//
// No error carries a Secret's content: a key whose body is not a values map is
// reported by name only, since a parser's message can quote what it read.
func readValuesSecrets(ctx context.Context, reader dynamic.Interface, ns string, refs []valuesSecretRef) (vals []map[string]interface{}, versions []string, err error) {
	for i, ref := range refs {
		if ref.name == "" || len(ref.keys) == 0 {
			return nil, nil, fmt.Errorf("valuesSecrets[%d]: name and keys are required", i)
		}
		// client-go refuses a name with "/" or "%" before sending, with an error
		// no retry can change; an older CRD than this one does not refuse it.
		if errs := validation.IsDNS1123Subdomain(ref.name); len(errs) > 0 {
			return nil, nil, fmt.Errorf("valuesSecrets[%d]: %q is not a Secret name: %s", i, ref.name, strings.Join(errs, "; "))
		}
		sec, err := reader.Resource(secretGVR).Namespace(ns).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("valuesSecrets[%d]: read Secret %s/%s: %w", i, ns, ref.name, err)
		}
		data, _ := sec.Object["data"].(map[string]interface{})
		for _, key := range ref.keys {
			enc, ok := data[key].(string)
			if !ok {
				return nil, nil, fmt.Errorf("valuesSecrets[%d]: Secret %s/%s has no key %q", i, ns, ref.name, key)
			}
			raw, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				return nil, nil, fmt.Errorf("valuesSecrets[%d]: key %q of Secret %s/%s is not base64", i, key, ns, ref.name)
			}
			m, err := parseYAMLValues(string(raw))
			if err != nil {
				return nil, nil, fmt.Errorf("valuesSecrets[%d]: key %q of Secret %s/%s is not a values.yaml map", i, key, ns, ref.name)
			}
			vals = append(vals, m)
			versions = append(versions, fmt.Sprintf("%s\x00%s\x00%s\x00%s", ref.name, key, sec.GetUID(), sec.GetResourceVersion()))
		}
	}
	return vals, versions, nil
}

// secretSpecHash is specHash for a chart with spec.valuesSecrets. Its merged
// values carry the Secrets' content, and the hash is in the status, readable
// wherever the HelmChart is, so it covers the public inputs as written —
// the source, valuesContent, the HelmChartConfig's valuesContent, set and
// setString — and each Secret's version (readValuesSecrets) instead. Hashing
// the values merged without the Secrets is not enough: a set entry over a key
// a Secret overrides changes the release but not that merge, and that merge
// can fail where the full one succeeds. Any edit to a public input therefore
// upgrades such a chart, even one that leaves the merged values the same.
func secretSpecHash(spec helmSpec, hccValues string, secretVersions []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "valuesSecrets\x00%s\x00%s\x00%s\x00", spec.repo, spec.chart, spec.version)
	if spec.chartContent != "" {
		cc := sha256.Sum256([]byte(spec.chartContent))
		h.Write(cc[:])
	}
	// json.Marshal sorts map keys, so set and setString hash canonically.
	if b, err := json.Marshal([]interface{}{spec.valuesContent, hccValues, spec.set, spec.setString}); err == nil {
		h.Write(b)
	}
	for _, v := range secretVersions {
		fmt.Fprintf(h, "\x00%s", v)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// valuesFromSecretsField in a HelmChart's status marks that a release of it
// was rendered with values from spec.valuesSecrets. Helm keeps them in the
// release history, which a recovery can roll back to after the field is gone.
const valuesFromSecretsField = "valuesFromSecrets"

// helmFailure is the status message for a Helm action on ns/name that failed
// with err. Helm's error can quote values: a template's fail, a schema
// violation, the apiserver rejecting a rendered field ("Invalid value: ...").
// For a private chart, one whose spec names valuesSecrets or that carries
// valuesFromSecretsField, they may be a Secret's, and the status is readable
// wherever the HelmChart is, so the error goes to the operator's log only and
// the status says where it is.
func helmFailure(ns, name string, private bool, action string, err error) string {
	if !private {
		return action + ": " + err.Error()
	}
	log.Printf("helmchart %s/%s: %s: %v", ns, name, action, err)
	return action + " failed; Helm's error is only in the operator's log, because it can quote values from spec.valuesSecrets"
}

// privateChart is whether Helm's output for hc can carry Secret values: its
// spec names valuesSecrets, or its status records a release rendered with
// them (valuesFromSecretsField).
func privateChart(hc *unstructured.Unstructured, spec helmSpec) bool {
	return len(spec.valuesSecrets) > 0 || asBool(nested(hc.Object, "status", valuesFromSecretsField))
}

// privateHoldObjects stands in a private chart's uninstall-incomplete hold for
// Helm's list of the objects the uninstall left, which recordUninstallIncomplete
// logs instead: a template names objects, so a name can carry a value from
// spec.valuesSecrets.
const privateHoldObjects = "the objects the operator's log lists for this HelmChart, since their names can carry values from spec.valuesSecrets"

// retryableSecretRead is whether a failed read of spec.valuesSecrets may work
// unchanged on the next pass: the apiserver throttling (429), timing out (408)
// or failing (5xx), the connection failing, or the pass's context ending.
// Anything else (a Secret that is gone or forbidden, a reference client-go
// refuses, one that cannot work as written) will fail the same way again.
func retryableSecretRead(err error) bool {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= http.StatusInternalServerError
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
