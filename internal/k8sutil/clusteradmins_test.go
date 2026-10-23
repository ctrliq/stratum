package k8sutil

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func TestEnsureClusterAdminsBinding(t *testing.T) {
	want := wantClusterAdminsBinding()
	marker := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ClusterAdminsMarker, Namespace: "kube-system"}, Data: data}
	}
	narrowed := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: ClusterAdminsBinding},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"}, Subjects: want.Subjects}
	bound := map[string]string{"state": clusterAdminsBound, "holder": "cp2"}
	for _, tc := range []struct {
		name        string
		binding     *rbacv1.ClusterRoleBinding
		marker      *corev1.ConfigMap
		wantBinding *rbacv1.ClusterRoleBinding
		wantErr     bool
		wantState   string
	}{
		{name: "a new cluster: created", wantBinding: want, wantState: clusterAdminsBound},
		{name: "revoked after creation: stays revoked", marker: marker(bound), wantState: clusterAdminsBound},
		{name: "a marker in an unknown state: fails closed", marker: marker(nil)},
		{name: "narrowed by hand before the marker: left alone", binding: narrowed, wantBinding: narrowed, wantState: clusterAdminsBound},
		{name: "narrowed by hand after creation: left alone", binding: narrowed, marker: marker(bound), wantBinding: narrowed, wantState: clusterAdminsBound},
		{name: "this controller's unfinished claim: finished",
			marker: marker(map[string]string{"state": clusterAdminsClaimed, "holder": "cp1"}), wantBinding: want, wantState: clusterAdminsBound},
		{name: "another controller's unfinished claim: fails closed",
			marker: marker(map[string]string{"state": clusterAdminsClaimed, "holder": "cp2"}), wantErr: true, wantState: clusterAdminsClaimed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cs := fake.NewClientset()
			if tc.binding != nil {
				if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, tc.binding.DeepCopy(), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker != nil {
				if _, err := cs.CoreV1().ConfigMaps("kube-system").Create(ctx, tc.marker.DeepCopy(), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				err := EnsureClusterAdminsBinding(ctx, cs, "cp1")
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), "cp2") {
						t.Fatalf("EnsureClusterAdminsBinding = %v, want an error naming the holder cp2", err)
					}
				} else if err != nil {
					t.Fatalf("EnsureClusterAdminsBinding: %v", err)
				}
			}
			checkClusterAdmins(t, cs, tc.wantBinding, tc.wantState)
		})
	}
}

// TestEnsureClusterAdminsBindingStaleReadCannotRestore replays a controller
// that read no marker, stalled while another created the binding and an
// administrator revoked it, then resumed: its claim must fail on the marker
// and the binding stay revoked.
func TestEnsureClusterAdminsBindingStaleReadCannotRestore(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	if err := EnsureClusterAdminsBinding(ctx, cs, "cp1"); err != nil {
		t.Fatal(err)
	}
	if err := cs.RbacV1().ClusterRoleBindings().Delete(ctx, ClusterAdminsBinding, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	stale := true
	cs.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !stale || action.(k8stesting.GetAction).GetName() != ClusterAdminsMarker {
			return false, nil, nil
		}
		stale = false
		return true, nil, apierrors.NewNotFound(corev1.Resource("configmaps"), ClusterAdminsMarker)
	})
	if err := EnsureClusterAdminsBinding(ctx, cs, "cp2"); err != nil {
		t.Fatalf("EnsureClusterAdminsBinding after a stale read: %v", err)
	}
	if stale {
		t.Fatal("the stale read was never served")
	}
	checkClusterAdmins(t, cs, nil, clusterAdminsBound)
}

func checkClusterAdmins(t *testing.T, cs *fake.Clientset, wantBinding *rbacv1.ClusterRoleBinding, wantState string) {
	t.Helper()
	ctx := context.Background()
	got, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, ClusterAdminsBinding, metav1.GetOptions{})
	switch {
	case wantBinding == nil:
		if !apierrors.IsNotFound(err) {
			t.Errorf("binding = %+v (err %v), want none: a revoked admin.conf must stay revoked", got, err)
		}
	case err != nil:
		t.Fatal(err)
	case got.RoleRef != wantBinding.RoleRef || !slices.Equal(got.Subjects, wantBinding.Subjects):
		t.Errorf("binding = roleRef %+v subjects %+v, want roleRef %+v subjects %+v",
			got.RoleRef, got.Subjects, wantBinding.RoleRef, wantBinding.Subjects)
	}
	m, err := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, ClusterAdminsMarker, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("marker: %v, want it recorded", err)
	}
	if got := m.Data["state"]; got != wantState {
		t.Errorf("marker state = %q, want %q", got, wantState)
	}
}

// TestClusterAdminsBindingNamesTheAdminLeafsGroup pins the binding's subject
// to the group pki issues the admin leaf with, and that leaf to no other.
func TestClusterAdminsBindingNamesTheAdminLeafsGroup(t *testing.T) {
	want := wantClusterAdminsBinding()
	for _, sp := range pki.LeafSpecs("cp1", "10.0.0.1", nil) {
		if sp.Name != "admin" {
			continue
		}
		if !slices.Equal(sp.O, []string{want.Subjects[0].Name}) {
			t.Fatalf("admin O = %v, want only %q (the group %s binds)", sp.O, want.Subjects[0].Name, ClusterAdminsBinding)
		}
		return
	}
	t.Fatal("pki issues no admin leaf")
}

// TestInternalAdminConfPrefersSuperAdmin pins that Stratum's own clients use
// super-admin.conf, and admin.conf only on a node that has none yet.
func TestInternalAdminConfPrefersSuperAdmin(t *testing.T) {
	oldDir, oldAdmin := version.KubeconfigDir, version.AdminConf
	t.Cleanup(func() { version.KubeconfigDir, version.AdminConf = oldDir, oldAdmin })
	version.KubeconfigDir = t.TempDir()
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	if got := InternalAdminConf(); got != version.AdminConf {
		t.Errorf("without super-admin.conf: %s, want %s", got, version.AdminConf)
	}
	if err := os.WriteFile(version.SuperAdminConf(), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InternalAdminConf(); got != version.SuperAdminConf() {
		t.Errorf("with super-admin.conf: %s, want %s", got, version.SuperAdminConf())
	}
}

// TestEnsureClusterAdminsBindingSerializesOneHolder replays two concurrent
// calls under the same holder, as the cluster-admins task and the leaf-subject
// migration make: the second starts once the first, having claimed the marker,
// is held before its binding create, which waits until the second has read
// the marker, and the second's binding create is held until
// the first has marked the marker bound. Unserialized, the second creates the
// binding after the marker says bound, which recreates a binding an
// administrator revoked in between. Serialized, the second only starts once
// the first is done, reads bound, and creates nothing. The holds sit outside
// the fake clientset, whose reactors run under one lock.
func TestEnsureClusterAdminsBindingSerializesOneHolder(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientset()
	var mu sync.Mutex
	var markerGets, bindingCreates int
	var createdWhileBound bool
	firstHeld, secondRead, firstBound := make(chan struct{}), make(chan struct{}), make(chan struct{})
	closeOnce := func(c chan struct{}) {
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-c:
		default:
			close(c)
		}
	}
	wait := func(c chan struct{}) {
		select {
		case <-c:
		case <-time.After(500 * time.Millisecond):
		}
	}
	markerState := func() string {
		o, err := fc.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), "kube-system", ClusterAdminsMarker)
		if err != nil {
			return ""
		}
		return o.(*corev1.ConfigMap).Data["state"]
	}
	fc.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		markerGets++
		n := markerGets
		mu.Unlock()
		if n == 2 {
			closeOnce(secondRead)
		}
		return false, nil, nil
	})
	cs := &holdingClientset{Interface: fc, beforeBindingCreate: func() {
		mu.Lock()
		bindingCreates++
		n := bindingCreates
		mu.Unlock()
		if n == 1 {
			closeOnce(firstHeld)
			wait(secondRead)
		} else {
			wait(firstBound)
		}
		if markerState() == clusterAdminsBound {
			mu.Lock()
			createdWhileBound = true
			mu.Unlock()
		}
	}, afterMarkerUpdate: func() {
		if markerState() == clusterAdminsBound {
			closeOnce(firstBound)
		}
	}}

	var wg sync.WaitGroup
	wg.Go(func() { _ = EnsureClusterAdminsBinding(ctx, cs, "cp1") })
	wait(firstHeld)
	wg.Go(func() { _ = EnsureClusterAdminsBinding(ctx, cs, "cp1") })
	wg.Wait()
	if createdWhileBound {
		t.Error("a concurrent call created the binding after the marker was bound, which would undo a revocation made in between")
	}
	if got := markerState(); got != clusterAdminsBound {
		t.Errorf("marker state = %q, want %q", got, clusterAdminsBound)
	}
}

// holdingClientset runs beforeBindingCreate ahead of each ClusterRoleBinding
// create, and afterMarkerUpdate after each ConfigMap update, outside the fake
// clientset's reactor lock.
type holdingClientset struct {
	kubernetes.Interface
	beforeBindingCreate, afterMarkerUpdate func()
}

func (c *holdingClientset) RbacV1() rbacv1client.RbacV1Interface {
	return holdingRbac{c.Interface.RbacV1(), c.beforeBindingCreate}
}

func (c *holdingClientset) CoreV1() corev1client.CoreV1Interface {
	return holdingCore{c.Interface.CoreV1(), c.afterMarkerUpdate}
}

type holdingRbac struct {
	rbacv1client.RbacV1Interface
	before func()
}

func (r holdingRbac) ClusterRoleBindings() rbacv1client.ClusterRoleBindingInterface {
	return holdingBindings{r.RbacV1Interface.ClusterRoleBindings(), r.before}
}

type holdingBindings struct {
	rbacv1client.ClusterRoleBindingInterface
	before func()
}

func (b holdingBindings) Create(ctx context.Context, crb *rbacv1.ClusterRoleBinding, o metav1.CreateOptions) (*rbacv1.ClusterRoleBinding, error) {
	b.before()
	return b.ClusterRoleBindingInterface.Create(ctx, crb, o)
}

type holdingCore struct {
	corev1client.CoreV1Interface
	after func()
}

func (c holdingCore) ConfigMaps(ns string) corev1client.ConfigMapInterface {
	return holdingConfigMaps{c.CoreV1Interface.ConfigMaps(ns), c.after}
}

type holdingConfigMaps struct {
	corev1client.ConfigMapInterface
	after func()
}

func (m holdingConfigMaps) Update(ctx context.Context, cm *corev1.ConfigMap, o metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	out, err := m.ConfigMapInterface.Update(ctx, cm, o)
	m.after()
	return out, err
}
