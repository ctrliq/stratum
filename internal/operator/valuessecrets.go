package operator

// spec.valuesSecrets keeps Secret-held values out of the CR anyone can read.

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

type valuesSecretRef struct {
	name string
	keys []string
}

// An entry without name and keys is kept so the read fails on it, not skipped.
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

// No error carries Secret content: errors name the entry, never quote what was read.
func readValuesSecrets(ctx context.Context, reader dynamic.Interface, ns string, refs []valuesSecretRef) (vals []map[string]interface{}, versions []string, err error) {
	for i, ref := range refs {
		if ref.name == "" || len(ref.keys) == 0 {
			return nil, nil, fmt.Errorf("valuesSecrets[%d]: name and keys are required", i)
		}
		// client-go refuses '/' or '%' names before sending; an older CRD does not refuse them.
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

// The hash is in the readable status, so it carries Secret versions, not values.
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

// Marks a release rendered with Secret values; Helm's history keeps them after.
const valuesFromSecretsField = "valuesFromSecrets"

// Helm's error can quote Secret values, so a private chart's failure goes to the log.
func helmFailure(ns, name string, private bool, action string, err error) string {
	if !private {
		return action + ": " + err.Error()
	}
	log.Printf("helmchart %s/%s: %s: %v", ns, name, action, err)
	return action + " failed; Helm's error is only in the operator's log, because it can quote values from spec.valuesSecrets"
}

func privateChart(hc *unstructured.Unstructured, spec helmSpec) bool {
	return len(spec.valuesSecrets) > 0 || asBool(nested(hc.Object, "status", valuesFromSecretsField))
}

// Stands in for the uninstall-left object list, whose names can carry Secret values.
const privateHoldObjects = "the objects the operator's log lists for this HelmChart, since their names can carry values from spec.valuesSecrets"

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
