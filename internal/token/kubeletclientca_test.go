package token

import (
	"context"
	"os"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// kubeletTrustHost writes a root bundle and a kubelet client-CA bundle that is
// split from it or a copy of it.
func kubeletTrustHost(t *testing.T, split bool) {
	t.Helper()
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	if _, err := pki.NewCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteCABundle("ca", "ca"); err != nil {
		t.Fatal(err)
	}
	kubeletTrust := "ca"
	if split {
		if _, err := pki.NewCA(pki.KubeletClientCAName, "stratum-kubelet-client-ca"); err != nil {
			t.Fatal(err)
		}
		kubeletTrust = pki.KubeletClientCAName
	}
	if err := pki.WriteCABundle(pki.KubeletClientBundleBase, kubeletTrust); err != nil {
		t.Fatal(err)
	}
}

// A split cluster publishes its kubelets' client-CA bundle for joining workers,
// and keeps it current; one that predates the split publishes nothing, so its
// workers keep trusting the root.
func TestPublishKubeletClientCA(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		kubeletTrustHost(t, true)
		f := &fakeConfigMaps{}
		if err := publishKubeletClientCA(context.Background(), f, nil); err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(pki.KubeletClientBundlePath())
		if err != nil {
			t.Fatal(err)
		}
		if f.cm == nil || f.cm.Name != clusterinfo.KubeletClientCAConfigMap || f.cm.Data[clusterinfo.KubeletClientCAKey] != string(want) {
			t.Fatalf("published %+v, want %s carrying the kubelet bundle", f.cm, clusterinfo.KubeletClientCAConfigMap)
		}
		if err := publishKubeletClientCA(context.Background(), f, nil); err != nil {
			t.Fatal(err)
		}
		if f.updates != 0 {
			t.Errorf("an unchanged bundle was rewritten %d times", f.updates)
		}
		f.cm.Data = map[string]string{clusterinfo.KubeletClientCAKey: "stale"}
		if err := publishKubeletClientCA(context.Background(), f, nil); err != nil {
			t.Fatal(err)
		}
		if f.cm.Data[clusterinfo.KubeletClientCAKey] != string(want) {
			t.Error("a stale published bundle was not brought back in step")
		}
	})
	t.Run("migrating", func(t *testing.T) {
		kubeletTrustHost(t, true)
		if err := pki.WriteCABundle(pki.KubeletClientBundleBase, pki.KubeletClientCAName, "ca"); err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(pki.KubeletClientBundlePath())
		if err != nil {
			t.Fatal(err)
		}
		f := &fakeConfigMaps{}
		if err := publishKubeletClientCA(context.Background(), f, nil); err != nil {
			t.Fatal(err)
		}
		if f.cm == nil || f.cm.Data[clusterinfo.KubeletClientCAKey] != string(want) {
			t.Fatalf("a migrating cluster published %+v, want the kubelet-client CA beside the root for its workers", f.cm)
		}
	})
	t.Run("predates the split", func(t *testing.T) {
		kubeletTrustHost(t, false)
		f := &fakeConfigMaps{}
		if err := publishKubeletClientCA(context.Background(), f, nil); err != nil {
			t.Fatal(err)
		}
		if f.cm != nil || f.creates != 0 {
			t.Errorf("an unsplit cluster published %+v", f.cm)
		}
	})
}

type fakeRoles struct{ role *rbacv1.Role }

func (f *fakeRoles) Get(_ context.Context, name string, _ metav1.GetOptions) (*rbacv1.Role, error) {
	if f.role == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, name)
	}
	return f.role.DeepCopy(), nil
}

func (f *fakeRoles) Create(_ context.Context, r *rbacv1.Role, _ metav1.CreateOptions) (*rbacv1.Role, error) {
	f.role = r.DeepCopy()
	return r, nil
}

func (f *fakeRoles) Update(_ context.Context, r *rbacv1.Role, _ metav1.UpdateOptions) (*rbacv1.Role, error) {
	f.role = r.DeepCopy()
	return r, nil
}

// The Role that lets a joining node read cluster-info anonymously also covers
// the kubelet client-CA ConfigMap, including on a cluster whose Role an earlier
// release created with cluster-info alone.
func TestEnsureClusterInfoRoleCoversTheKubeletClientCA(t *testing.T) {
	for name, f := range map[string]*fakeRoles{
		"absent": {},
		"from an earlier release": {role: &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "stratum:cluster-info", Namespace: "kube-public"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"configmaps"},
				ResourceNames: []string{"cluster-info"}, Verbs: []string{"get"},
			}},
		}},
	} {
		if err := ensureClusterInfoRole(context.Background(), f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := []string{"cluster-info", clusterinfo.KubeletClientCAConfigMap}
		if len(f.role.Rules) != 1 || !reflect.DeepEqual(f.role.Rules[0].ResourceNames, want) || !reflect.DeepEqual(f.role.Rules[0].Verbs, []string{"get"}) {
			t.Errorf("%s: Role rules = %+v, want get on %v only", name, f.role.Rules, want)
		}
	}
}
