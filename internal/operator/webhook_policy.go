package operator

// One Options.PrivilegedNamespaces set governs install authorization and stamping exemption.

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func normalizePrivilegedNamespaces(namespaces []string) []string {
	var out []string
	seen := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	return out
}

// Nil for an empty set: a valueless NotIn fails apiserver validation, empty fails closed.
func privilegedNamespaceSelector(privileged []string) *metav1.LabelSelector {
	if len(privileged) == 0 {
		return nil
	}
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      corev1.LabelMetadataName,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   privileged,
		}},
	}
}
