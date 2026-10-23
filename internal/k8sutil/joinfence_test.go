package k8sutil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

// TestJoinMaintenanceHolderNamesTheToken: a join's holder is its bootstrap
// token's username and a slash, the prefix the join fence policy requires,
// followed by MaintenanceHolder's host/pid/nanoseconds, so two takes on one
// host are still distinct holders.
func TestJoinMaintenanceHolderNamesTheToken(t *testing.T) {
	a, b := JoinMaintenanceHolder("abc123"), JoinMaintenanceHolder("abc123")
	for _, h := range []string{a, b} {
		rest, ok := strings.CutPrefix(h, "system:bootstrap:abc123/")
		if !ok || len(strings.Split(rest, "/")) != 3 {
			t.Errorf("JoinMaintenanceHolder = %q, want system:bootstrap:abc123/<host>/<pid>/<nanoseconds>", h)
		}
	}
	if a == b {
		t.Errorf("two takes share the holder %q", a)
	}
}

// TestMaintenanceFenceHolders: the admin fence keeps MaintenanceHolder's
// host/pid/nanoseconds, with no token prefix, and MaintenanceFenceAs writes
// the holder given on both leases.
func TestMaintenanceFenceHolders(t *testing.T) {
	holderOn := func(t *testing.T, cs *fake.Clientset, name string) string {
		t.Helper()
		l, err := cs.CoordinationV1().Leases("kube-system").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *l.Spec.HolderIdentity
	}
	cs := fake.NewSimpleClientset()
	lease, err := MaintenanceFence(cs)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"} {
		if h := holderOn(t, cs, name); strings.HasPrefix(h, "system:bootstrap:") || len(strings.Split(h, "/")) != 3 {
			t.Errorf("the admin fence's %s holder = %q, want <host>/<pid>/<nanoseconds>", name, h)
		}
	}
	lease.Release()

	cs = fake.NewSimpleClientset()
	lease, err = MaintenanceFenceAs(cs, "system:bootstrap:abc123/h/1/2")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	for _, name := range []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"} {
		if h := holderOn(t, cs, name); h != "system:bootstrap:abc123/h/1/2" {
			t.Errorf("%s holder = %q, want the one given", name, h)
		}
	}
}

// TestJoinFenceDenied: only the join fence policy's own denial, as the
// admission dispatcher spells it, counts — wrapped or not; RBAC's Forbidden,
// another policy's denial and a plain error do not.
func TestJoinFenceDenied(t *testing.T) {
	denial := func(policy string) error {
		m := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: no", policy, policy)
		err := apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "stratum-cluster-upgrade", errors.New(m))
		err.ErrStatus.Reason, err.ErrStatus.Code = metav1.StatusReasonInvalid, http.StatusUnprocessableEntity
		return err
	}
	rbac := apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "stratum-cluster-upgrade", errors.New("no rule"))
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"the policy's denial", denial(JoinFencePolicy), true},
		{"the policy's denial, wrapped", fmt.Errorf("take the lease kube-system/stratum-cluster-upgrade: %w", denial(JoinFencePolicy)), true},
		{"another policy's denial", denial(NodeMarkPolicy), false},
		{"RBAC's Forbidden", rbac, false},
		{"a plain error naming the policy", errors.New("ValidatingAdmissionPolicy '" + JoinFencePolicy + "'"), false},
		{"nil", nil, false},
	} {
		if got := JoinFenceDenied(c.err); got != c.want {
			t.Errorf("%s: JoinFenceDenied = %v, want %v", c.name, got, c.want)
		}
	}
}
