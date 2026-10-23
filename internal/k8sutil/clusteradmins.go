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

// ClusterAdminsBinding binds pki.ClusterAdminsGroup, the admin leaf's group,
// to cluster-admin. Deleting it revokes every admin.conf.
const ClusterAdminsBinding = pki.ClusterAdminsGroup

// ClusterAdminsMarker is the kube-system ConfigMap through which the
// controllers agree on which of them creates ClusterAdminsBinding, and which
// records that it was created, so a binding deleted afterwards stays deleted.
const ClusterAdminsMarker = "stratum-cluster-admins-binding"

// The marker's states: claimed by the controller its holder key names, which
// alone may create the binding, then bound.
const (
	clusterAdminsClaimed = "claimed"
	clusterAdminsBound   = "bound"
)

var clusterAdminsMu sync.Mutex

func wantClusterAdminsBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterAdminsBinding},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: pki.ClusterAdminsGroup}},
	}
}

// EnsureClusterAdminsBinding creates ClusterAdminsBinding once per cluster.
// Absent ClusterAdminsMarker it claims the marker for holder, a controller's
// node name; the controller holding the claim creates the binding unless one
// exists, then marks it bound. A bound marker, or one in a state it does not
// know, means nothing is done, whatever became of the binding.
//
// The claim comes before the binding, so a controller that read no marker and
// then stalled cannot recreate a binding revoked meanwhile: its claim fails on
// the existing marker. A claim whose holder never finishes fails closed: the
// binding stays absent, and every other controller returns an error naming the
// holder, until the holder's next `stratum up` finishes it or an administrator
// creates the binding or deletes the marker.
//
// Calls in one process run one at a time. A controller ensures the binding
// from more than one task under the same holder, and two concurrent calls
// could both read the marker as claimed: after the first marked it bound, the
// second would create the binding again, undoing a revocation made in
// between. Serialized, the second reads the marker as bound.
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
