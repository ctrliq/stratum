package k8sutil

import (
	"context"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/pki"
)

func TestEnsureKubeletAPIAdminBinding(t *testing.T) {
	want := wantKubeletAPIAdminBinding()
	for _, tc := range []struct {
		name     string
		existing *rbacv1.ClusterRoleBinding
	}{
		{name: "absent: created"},
		{name: "already right: left alone", existing: want.DeepCopy()},
		{name: "subjects drifted: restored", existing: &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: KubeletAPIAdminBinding},
			RoleRef:    want.RoleRef,
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"},
			},
		}},
		{name: "roleRef differs: recreated", existing: &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: KubeletAPIAdminBinding},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   want.Subjects,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			if tc.existing != nil {
				if _, err := cs.RbacV1().ClusterRoleBindings().Create(context.Background(), tc.existing, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := EnsureKubeletAPIAdminBinding(context.Background(), cs.RbacV1().ClusterRoleBindings()); err != nil {
					t.Fatalf("EnsureKubeletAPIAdminBinding: %v", err)
				}
			}
			got, err := cs.RbacV1().ClusterRoleBindings().Get(context.Background(), KubeletAPIAdminBinding, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.RoleRef != want.RoleRef || !slices.Equal(got.Subjects, want.Subjects) {
				t.Fatalf("binding = roleRef %+v subjects %+v, want roleRef %+v subjects %+v", got.RoleRef, got.Subjects, want.RoleRef, want.Subjects)
			}
		})
	}
}

// TestKubeletClientUserIsTheLeafCN pins the binding's subject to the CN pki
// issues apiserver-kubelet-client with: a rename on either side would leave the
// apiserver unauthorized at every kubelet.
func TestKubeletClientUserIsTheLeafCN(t *testing.T) {
	for _, sp := range pki.LeafSpecs("cp1", "10.0.0.1", nil) {
		if sp.Name != "apiserver-kubelet-client" {
			continue
		}
		if sp.CN != KubeletClientUser {
			t.Fatalf("apiserver-kubelet-client CN = %q, want %q (the user %s binds)", sp.CN, KubeletClientUser, KubeletAPIAdminBinding)
		}
		if len(sp.O) != 0 {
			t.Fatalf("apiserver-kubelet-client O = %v, want none", sp.O)
		}
		return
	}
	t.Fatal("pki issues no apiserver-kubelet-client leaf")
}
