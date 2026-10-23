package k8sutil

import (
	"context"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// NodeClientBindingAPI is the part of the ClusterRoleBinding client needed to
// disable upstream node-client approval without freeing its binding name.
type NodeClientBindingAPI interface {
	Get(context.Context, string, metav1.GetOptions) (*rbacv1.ClusterRoleBinding, error)
	Create(context.Context, *rbacv1.ClusterRoleBinding, metav1.CreateOptions) (*rbacv1.ClusterRoleBinding, error)
	Update(context.Context, *rbacv1.ClusterRoleBinding, metav1.UpdateOptions) (*rbacv1.ClusterRoleBinding, error)
}

// DisableNodeClientAutoApprove keeps NodeClientAutoApproveBinding present with
// no subjects, so the upstream approver never approves a bootstrap client CSR.
// Releases before this one create the binding with BootstrapGroup on `stratum
// token create` and treat AlreadyExists as success: an occupied, empty binding
// turns that create into a no-op, where a deleted one would let an older
// control plane grant takeover again. Subjects are cleared by Update, never by
// deleting the binding.
func DisableNodeClientAutoApprove(ctx context.Context, crbs NodeClientBindingAPI) error {
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error {
		binding, err := crbs.Get(ctx, NodeClientAutoApproveBinding, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = crbs.Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: NodeClientAutoApproveBinding},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
				},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil || len(binding.Subjects) == 0 {
			return err
		}
		binding = binding.DeepCopy()
		binding.Subjects = nil
		_, err = crbs.Update(ctx, binding, metav1.UpdateOptions{})
		return err
	})
}
