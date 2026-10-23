package k8sutil

import (
	"context"
	"fmt"
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/ctrliq/stratum/internal/version"
)

// KubeVipUser is the CN of the kube-vip leaf.
const KubeVipUser = "system:kube-vip"

// KubeVipLease is the Lease kube-vip's control-plane election holds, kube-vip's default.
const KubeVipLease = "plndr-cp-lock"

// KubeVipRBAC names the Role and RoleBinding in kube-system that let KubeVipUser hold KubeVipLease.
const KubeVipRBAC = "stratum:kube-vip"

const kubeVipNamespace = "kube-system"

func wantKubeVipRole() *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: KubeVipRBAC, Namespace: kubeVipNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{KubeVipLease}, Verbs: []string{"get", "update"}},
		},
	}
}

func wantKubeVipRoleBinding() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: KubeVipRBAC, Namespace: kubeVipNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: KubeVipRBAC},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: KubeVipUser}},
	}
}

func retryable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
}

// EnsureKubeVipRBAC makes the kube-vip Role and RoleBinding exactly what
// KubeVipUser needs to hold KubeVipLease, recreating the binding when its
// immutable roleRef differs.
func EnsureKubeVipRBAC(ctx context.Context, cs kubernetes.Interface) error {
	roles := cs.RbacV1().Roles(kubeVipNamespace)
	role := wantKubeVipRole()
	if err := retry.OnError(retry.DefaultRetry, retryable, func() error {
		got, err := roles.Get(ctx, KubeVipRBAC, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = roles.Create(ctx, role, metav1.CreateOptions{})
			return err
		}
		if err != nil || equality.Semantic.DeepEqual(got.Rules, role.Rules) {
			return err
		}
		got = got.DeepCopy()
		got.Rules = role.Rules
		_, err = roles.Update(ctx, got, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("role %s/%s: %w", kubeVipNamespace, KubeVipRBAC, err)
	}

	bindings := cs.RbacV1().RoleBindings(kubeVipNamespace)
	binding := wantKubeVipRoleBinding()
	if err := retry.OnError(retry.DefaultRetry, retryable, func() error {
		got, err := bindings.Get(ctx, KubeVipRBAC, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = bindings.Create(ctx, binding, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if got.RoleRef != binding.RoleRef {
			if err := bindings.Delete(ctx, KubeVipRBAC, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			_, err = bindings.Create(ctx, binding, metav1.CreateOptions{})
			return err
		}
		if slices.Equal(got.Subjects, binding.Subjects) {
			return nil
		}
		got = got.DeepCopy()
		got.Subjects = binding.Subjects
		_, err = bindings.Update(ctx, got, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("rolebinding %s/%s: %w", kubeVipNamespace, KubeVipRBAC, err)
	}
	return nil
}

// LocalAdminClientset is AdminClientset against this node's own apiserver
// rather than the server admin.conf names, which may be a VIP.
func LocalAdminClientset() (*kubernetes.Clientset, error) {
	cfg, err := AdminRESTConfig()
	if err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Host = fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
	return kubernetes.NewForConfig(cfg)
}
