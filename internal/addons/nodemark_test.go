package addons

import (
	"bytes"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// TestNodeMarkPolicyIsTheOneTheOperatorChecks: the operator deletes
// a marked Node only while the policy and a denying binding named
// k8sutil.NodeMarkPolicy exist, so the shipped manifest must use that name,
// bind that policy, deny, and name the annotation the node writes.
func TestNodeMarkPolicyIsTheOneTheOperatorChecks(t *testing.T) {
	docs := bytes.Split(RenderNodeMarkPolicy(), []byte("\n---\n"))
	if len(docs) != 2 {
		t.Fatalf("node-mark stack has %d documents, want the policy and its binding", len(docs))
	}
	var p admissionregistrationv1.ValidatingAdmissionPolicy
	var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
	if err := yaml.UnmarshalStrict(docs[0], &p); err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(docs[1], &b); err != nil {
		t.Fatal(err)
	}
	if p.Name != k8sutil.NodeMarkPolicy || b.Name != k8sutil.NodeMarkPolicy || b.Spec.PolicyName != k8sutil.NodeMarkPolicy {
		t.Errorf("policy %q, binding %q for policy %q; want all %q", p.Name, b.Name, b.Spec.PolicyName, k8sutil.NodeMarkPolicy)
	}
	if len(b.Spec.ValidationActions) != 1 || b.Spec.ValidationActions[0] != admissionregistrationv1.Deny {
		t.Errorf("binding actions = %v, want Deny", b.Spec.ValidationActions)
	}
	if len(p.Spec.Variables) == 0 || p.Spec.Variables[0].Expression != "'"+k8sutil.UninstalledAnnotation+"'" {
		t.Errorf("policy guards %v, want the %s annotation", p.Spec.Variables, k8sutil.UninstalledAnnotation)
	}
}
