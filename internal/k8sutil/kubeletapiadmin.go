package k8sutil

import (
	"context"
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// KubeletClientUser is the CN of apiserver-kubelet-client.
const KubeletClientUser = "kube-apiserver-kubelet-client"

// KubeletAPIAdminBinding binds KubeletClientUser to system:kubelet-api-admin.
const KubeletAPIAdminBinding = "stratum:kube-apiserver-kubelet-admin"

const kubeletAPIAdminRole = "system:kubelet-api-admin"

// KubeletAPIAdminBindingAPI is the part of the ClusterRoleBinding client
// EnsureKubeletAPIAdminBinding needs.
type KubeletAPIAdminBindingAPI interface {
	Get(context.Context, string, metav1.GetOptions) (*rbacv1.ClusterRoleBinding, error)
	Create(context.Context, *rbacv1.ClusterRoleBinding, metav1.CreateOptions) (*rbacv1.ClusterRoleBinding, error)
	Update(context.Context, *rbacv1.ClusterRoleBinding, metav1.UpdateOptions) (*rbacv1.ClusterRoleBinding, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

func wantKubeletAPIAdminBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: KubeletAPIAdminBinding},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: kubeletAPIAdminRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: KubeletClientUser}},
	}
}

// EnsureKubeletAPIAdminBinding makes KubeletAPIAdminBinding bind exactly
// KubeletClientUser to system:kubelet-api-admin, recreating it when the
// immutable roleRef differs.
func EnsureKubeletAPIAdminBinding(ctx context.Context, crbs KubeletAPIAdminBindingAPI) error {
	want := wantKubeletAPIAdminBinding()
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
	}, func() error {
		got, err := crbs.Get(ctx, KubeletAPIAdminBinding, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = crbs.Create(ctx, want, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if got.RoleRef != want.RoleRef {
			if err := crbs.Delete(ctx, KubeletAPIAdminBinding, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			_, err = crbs.Create(ctx, want, metav1.CreateOptions{})
			return err
		}
		if slices.Equal(got.Subjects, want.Subjects) {
			return nil
		}
		got = got.DeepCopy()
		got.Subjects = want.Subjects
		_, err = crbs.Update(ctx, got, metav1.UpdateOptions{})
		return err
	})
}
