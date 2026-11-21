package k8sutil

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/pki"
)

// Deleting it revokes every admin.conf.
const ClusterAdminsBinding = pki.ClusterAdminsGroup

// Controllers agree through it on who creates the binding; a deleted binding stays deleted.
const ClusterAdminsMarker = "stratum-cluster-admins-binding"

// The holder named in "claimed" alone may create the binding; bound records it.
const (
	clusterAdminsClaimed = "claimed"
	clusterAdminsBound   = "bound"
)

var clusterAdminsMu sync.Mutex // two tasks ensure the binding under one holder; serialize them

func wantClusterAdminsBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterAdminsBinding},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: pki.ClusterAdminsGroup}},
	}
}

// One binding per cluster: claim the marker first, and a stalled claim fails closed.
func EnsureClusterAdminsBinding(ctx context.Context, cs kubernetes.Interface, holder string) error {
	clusterAdminsMu.Lock()
	defer clusterAdminsMu.Unlock()
	cms := cs.CoreV1().ConfigMaps("kube-system")
	marker, err := cms.Get(ctx, ClusterAdminsMarker, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		marker, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ClusterAdminsMarker, Namespace: "kube-system"},
			Data: map[string]string{
				"binding": ClusterAdminsBinding,
				"state":   clusterAdminsClaimed,
				"holder":  holder,
				"claimed": time.Now().UTC().Format(time.RFC3339),
			},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			marker, err = cms.Get(ctx, ClusterAdminsMarker, metav1.GetOptions{})
		}
	}
	if err != nil {
		return err
	}
	if marker.Data["state"] != clusterAdminsClaimed {
		return nil
	}
	if marker.Data["holder"] != holder {
		return fmt.Errorf("controller %s claimed the %s binding at %s and has not created it; "+
			"admin.conf is refused until it does, or until the binding is created by hand",
			marker.Data["holder"], ClusterAdminsBinding, marker.Data["claimed"])
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, wantClusterAdminsBinding(), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	marker = marker.DeepCopy()
	marker.Data["state"] = clusterAdminsBound
	marker.Data["created"] = time.Now().UTC().Format(time.RFC3339)
	_, err = cms.Update(ctx, marker, metav1.UpdateOptions{})
	return err
}
