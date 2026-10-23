package addons

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
)

// TestJoinFencePolicyMatchesItsContract: the policy spells, as CEL literals,
// facts other packages own — the control-plane bootstrap group the mint
// grants the leases to, the lease duration every stratum run writes, and the
// two maintenance lease names — and the mint and the join find the policy and
// its denials by k8sutil.JoinFencePolicy. A drift in any of them would deny
// the join its own fence or leave the grant unconstrained.
func TestJoinFencePolicyMatchesItsContract(t *testing.T) {
	docs := bytes.Split(RenderJoinFencePolicy(), []byte("\n---\n"))
	if len(docs) != 2 {
		t.Fatalf("join fence stack has %d documents, want the policy and its binding", len(docs))
	}
	var p admissionregistrationv1.ValidatingAdmissionPolicy
	var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
	if err := yaml.UnmarshalStrict(docs[0], &p); err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(docs[1], &b); err != nil {
		t.Fatal(err)
	}
	if p.Name != k8sutil.JoinFencePolicy || b.Name != k8sutil.JoinFencePolicy || b.Spec.PolicyName != k8sutil.JoinFencePolicy {
		t.Errorf("policy %q, binding %q for policy %q; want all %q", p.Name, b.Name, b.Spec.PolicyName, k8sutil.JoinFencePolicy)
	}
	if len(b.Spec.ValidationActions) != 1 || b.Spec.ValidationActions[0] != admissionregistrationv1.Deny {
		t.Errorf("binding actions = %v, want Deny", b.Spec.ValidationActions)
	}
	if p.Spec.FailurePolicy == nil || *p.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Errorf("failure policy = %v, want Fail", p.Spec.FailurePolicy)
	}
	group := false
	for _, c := range p.Spec.MatchConditions {
		group = group || strings.Contains(c.Expression, "'"+k8sutil.ControlPlaneBootstrapGroup+"' in request.userInfo.groups")
	}
	if !group {
		t.Errorf("no match condition selects the %s group: %+v", k8sutil.ControlPlaneBootstrapGroup, p.Spec.MatchConditions)
	}
	vars := map[string]string{}
	for _, v := range p.Spec.Variables {
		vars[v.Name] = v.Expression
	}
	if want := strconv.Itoa(int(maintlease.Duration / time.Second)); vars["maxDurationSeconds"] != want {
		t.Errorf("maxDurationSeconds = %q, want maintlease.Duration's %s", vars["maxDurationSeconds"], want)
	}
	names := false
	for _, v := range p.Spec.Validations {
		names = names || v.Expression == "variables.name in ['stratum-cluster-maintenance', 'stratum-cluster-upgrade']"
	}
	if !names {
		t.Errorf("no validation limits the token to the two maintenance leases: %+v", p.Spec.Validations)
	}
	for _, v := range p.Spec.Validations {
		if v.Reason != nil {
			t.Errorf("validation %q sets reason %s; a denial must stay Invalid, never the Forbidden the join reads as a Role that predates the grant", v.Expression, *v.Reason)
		}
	}
}
