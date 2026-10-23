package operator

// webhook_policy.go — the privileged-namespace policy: install authorization
// (helmchart.go) and the admission webhook's namespace exemption
// (webhookdeploy.go) both derive from the one Options.PrivilegedNamespaces set,
// so a namespace trusted to install cross-namespace is exactly a namespace
// exempt from requester stamping, and an empty set denies everywhere.

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// normalizePrivilegedNamespaces applies splitNamespaces semantics to the
// configured set: entries are trimmed, empties dropped, duplicates dropped
// (first occurrence wins), order otherwise preserved. It is idempotent.
// Run normalizes Options.PrivilegedNamespaces once, before either consumer
// sees it; the selector derivation below re-applies it so a direct caller of
// buildWebhookConfig gets the same answer.
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

// privilegedNamespaceSelector derives the MutatingWebhookConfiguration's
// NamespaceSelector from the normalized privileged set: a NotIn on
// kubernetes.io/metadata.name exempts exactly those namespaces from requester
// stamping. An empty set returns nil — the selector is omitted so the webhook
// matches every namespace and unstamped writes fail closed, matching the
// reconciler's deny-all reading of an empty set; a valueless NotIn expression
// would fail apiserver validation.
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
