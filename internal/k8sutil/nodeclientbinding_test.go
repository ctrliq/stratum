package k8sutil

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestDisableNodeClientAutoApproveRacingTokenCreate(t *testing.T) {
	cs := fake.NewSimpleClientset()
	crbs := cs.RbacV1().ClusterRoleBindings()
	createdByOtherController := false
	cs.PrependReactor("create", "clusterrolebindings", func(ktesting.Action) (bool, runtime.Object, error) {
		if createdByOtherController {
			return false, nil, nil
		}
		createdByOtherController = true
		if err := cs.Tracker().Add(&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: NodeClientAutoApproveBinding},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"},
			Subjects:   []rbacv1.Subject{{Kind: "Group", Name: BootstrapGroup, APIGroup: rbacv1.GroupName}},
		}); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"}, NodeClientAutoApproveBinding)
	})
	if err := DisableNodeClientAutoApprove(context.Background(), crbs); err != nil {
		t.Fatal(err)
	}
	binding, err := crbs.Get(context.Background(), NodeClientAutoApproveBinding, metav1.GetOptions{})
	if err != nil || len(binding.Subjects) != 0 {
		t.Fatalf("racing token creation left auto-approval enabled: binding %+v, err %v", binding, err)
	}
	for _, action := range cs.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatalf("operator freed the binding name, allowing another token create to regrant it: %v", action)
		}
	}
}
