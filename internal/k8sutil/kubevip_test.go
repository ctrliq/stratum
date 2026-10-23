package k8sutil

import (
	"context"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/pki"
)

func TestEnsureKubeVipRBAC(t *testing.T) {
	role, binding := wantKubeVipRole(), wantKubeVipRoleBinding()
	meta := metav1.ObjectMeta{Name: KubeVipRBAC, Namespace: kubeVipNamespace}
	for _, tc := range []struct {
		name    string
		role    *rbacv1.Role
		binding *rbacv1.RoleBinding
	}{
		{name: "absent: created"},
		{name: "already right: left alone", role: role.DeepCopy(), binding: binding.DeepCopy()},
		{name: "rules widened: restored", role: &rbacv1.Role{ObjectMeta: meta, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
		}}, binding: binding.DeepCopy()},
		{name: "subjects drifted: restored", role: role.DeepCopy(), binding: &rbacv1.RoleBinding{ObjectMeta: meta, RoleRef: binding.RoleRef,
			Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"}}}},
		{name: "roleRef differs: recreated", role: role.DeepCopy(), binding: &rbacv1.RoleBinding{ObjectMeta: meta,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: binding.Subjects}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cs := fake.NewClientset()
			if tc.role != nil {
				if _, err := cs.RbacV1().Roles(kubeVipNamespace).Create(ctx, tc.role, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.binding != nil {
				if _, err := cs.RbacV1().RoleBindings(kubeVipNamespace).Create(ctx, tc.binding, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := EnsureKubeVipRBAC(ctx, cs); err != nil {
					t.Fatalf("EnsureKubeVipRBAC: %v", err)
				}
			}
			gotRole, err := cs.RbacV1().Roles(kubeVipNamespace).Get(ctx, KubeVipRBAC, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(gotRole.Rules, role.Rules) {
				t.Errorf("role rules = %+v, want %+v", gotRole.Rules, role.Rules)
			}
			gotBinding, err := cs.RbacV1().RoleBindings(kubeVipNamespace).Get(ctx, KubeVipRBAC, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if gotBinding.RoleRef != binding.RoleRef || !slices.Equal(gotBinding.Subjects, binding.Subjects) {
				t.Errorf("binding = roleRef %+v subjects %+v, want roleRef %+v subjects %+v",
					gotBinding.RoleRef, gotBinding.Subjects, binding.RoleRef, binding.Subjects)
			}
		})
	}
}

// TestKubeVipUserIsTheLeafCN pins the binding's subject to the CN pki issues
// kube-vip with: a rename on either side would leave kube-vip unable to hold
// its lease, and no node would hold the VIP.
func TestKubeVipUserIsTheLeafCN(t *testing.T) {
	for _, sp := range pki.LeafSpecs("cp1", "10.0.0.1", nil) {
		if sp.Name != "kube-vip" {
			continue
		}
		if sp.CN != KubeVipUser {
			t.Fatalf("kube-vip CN = %q, want %q (the user %s binds)", sp.CN, KubeVipUser, KubeVipRBAC)
		}
		if len(sp.O) != 0 {
			t.Fatalf("kube-vip O = %v, want none", sp.O)
		}
		return
	}
	t.Fatal("pki issues no kube-vip leaf")
}
