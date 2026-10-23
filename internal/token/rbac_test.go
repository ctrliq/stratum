package token

import (
	"context"
	"errors"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// fakeCRBs is a hand-rolled k8sutil.NodeClientBindingAPI, for the same reason as fakeSecrets.
type fakeCRBs struct {
	existing  map[string]bool
	created   map[string]*rbacv1.ClusterRoleBinding
	updated   map[string]*rbacv1.ClusterRoleBinding
	updateErr error
}

func (f *fakeCRBs) Create(_ context.Context, b *rbacv1.ClusterRoleBinding, _ metav1.CreateOptions) (*rbacv1.ClusterRoleBinding, error) {
	if f.existing[b.Name] || f.created[b.Name] != nil || f.updated[b.Name] != nil {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"}, b.Name)
	}
	if f.created == nil {
		f.created = map[string]*rbacv1.ClusterRoleBinding{}
	}
	f.created[b.Name] = b
	return b, nil
}

func (f *fakeCRBs) Get(_ context.Context, name string, _ metav1.GetOptions) (*rbacv1.ClusterRoleBinding, error) {
	if b := f.created[name]; b != nil {
		return b.DeepCopy(), nil
	}
	if b := f.updated[name]; b != nil {
		return b.DeepCopy(), nil
	}
	if f.existing[name] {
		return crb(name, "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
			rbacv1.Subject{Kind: "Group", Name: k8sutil.BootstrapGroup, APIGroup: rbacv1.GroupName}), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"}, name)
}

func (f *fakeCRBs) Update(_ context.Context, b *rbacv1.ClusterRoleBinding, _ metav1.UpdateOptions) (*rbacv1.ClusterRoleBinding, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.updated == nil {
		f.updated = map[string]*rbacv1.ClusterRoleBinding{}
	}
	f.updated[b.Name] = b.DeepCopy()
	return b, nil
}

func boundToBootstrapGroup(b *rbacv1.ClusterRoleBinding) bool {
	return slices.ContainsFunc(b.Subjects, func(s rbacv1.Subject) bool { return s.Name == k8sutil.BootstrapGroup })
}

func TestEnsureBootstrapRBACClearsAGrantedBinding(t *testing.T) {
	crbs := &fakeCRBs{existing: map[string]bool{k8sutil.NodeClientAutoApproveBinding: true}}
	if err := ensureBootstrapRBAC(context.Background(), crbs); err != nil {
		t.Fatal(err)
	}
	for _, b := range crbs.created {
		if b.RoleRef.Name == "system:certificates.k8s.io:certificatesigningrequests:nodeclient" {
			t.Errorf("token create granted nodeclient auto-approval via %s", b.Name)
		}
	}
	if b := crbs.created["stratum-node-bootstrapper"]; b == nil || !boundToBootstrapGroup(b) {
		t.Errorf("bootstrap group cannot create CSRs: %+v", b)
	}
	if b := crbs.created["stratum-csr-renewal"]; b == nil || b.Subjects[0].Name != "system:nodes" {
		t.Errorf("renewal binding for system:nodes missing: %+v", b)
	}
	if b := crbs.updated[k8sutil.NodeClientAutoApproveBinding]; b == nil || len(b.Subjects) != 0 {
		t.Errorf("token create did not clear the subjects on %s: %+v", k8sutil.NodeClientAutoApproveBinding, b)
	}

	again := &fakeCRBs{}
	if err := ensureBootstrapRBAC(context.Background(), again); err != nil {
		t.Fatalf("absent auto-approve binding must not fail token create: %v", err)
	}
	if b := again.created[k8sutil.NodeClientAutoApproveBinding]; b == nil || len(b.Subjects) != 0 {
		t.Errorf("token create did not reserve %s with no subjects: %+v", k8sutil.NodeClientAutoApproveBinding, b)
	}
}

func TestEnsureBootstrapRBACCreatesAnEmptyBinding(t *testing.T) {
	crbs := &fakeCRBs{existing: map[string]bool{"stratum-node-bootstrapper": true}}
	if err := ensureBootstrapRBAC(context.Background(), crbs); err != nil {
		t.Fatal(err)
	}
	b := crbs.created[k8sutil.NodeClientAutoApproveBinding]
	if b == nil || len(b.Subjects) != 0 {
		t.Fatalf("token create must reserve %s with no subjects: %+v", k8sutil.NodeClientAutoApproveBinding, b)
	}
	if len(crbs.updated) != 0 {
		t.Errorf("token create updated a binding it had just created empty: %+v", crbs.updated)
	}
}

func TestEnsureBootstrapRBACLeavesAnEmptyBindingEmpty(t *testing.T) {
	crbs := &fakeCRBs{created: map[string]*rbacv1.ClusterRoleBinding{
		k8sutil.NodeClientAutoApproveBinding: {
			ObjectMeta: metav1.ObjectMeta{Name: k8sutil.NodeClientAutoApproveBinding},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"},
		},
	}}
	if err := ensureBootstrapRBAC(context.Background(), crbs); err != nil {
		t.Fatal(err)
	}
	if b := crbs.created[k8sutil.NodeClientAutoApproveBinding]; len(b.Subjects) != 0 {
		t.Fatalf("token create regranted auto-approval on an empty binding: %+v", b)
	}
}

func TestEnsureBootstrapRBACUpdateFailure(t *testing.T) {
	boom := errors.New("apiserver down")
	crbs := &fakeCRBs{existing: map[string]bool{k8sutil.NodeClientAutoApproveBinding: true}, updateErr: boom}
	if err := ensureBootstrapRBAC(context.Background(), crbs); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the update failure surfaced so the token is revoked", err)
	}
}
