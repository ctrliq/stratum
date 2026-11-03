package mcp

import (
	"errors"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// tokenPod is a Pod manifest whose spec is containers plus extra.
func tokenPod(extra map[string]any) map[string]any {
	spec := map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}}
	maps.Copy(spec, extra)
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "spec": spec}
}

// tokenDeployment is a Deployment manifest whose template spec is containers
// plus extra.
func tokenDeployment(extra map[string]any) map[string]any {
	pod := tokenPod(extra)
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": pod["spec"]}},
	}
}

func saBinding(ns, name string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "edit"},
		Subjects:   subjects,
	}
}

func saClusterBinding(name string, subjects ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   subjects,
	}
}

func saSubject(ns, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: name}
}

// A pod spec that would mount the token of an account a binding grants
// anything is refused, in a Pod or a workload template, and the refusal names
// the account, the binding and pod_logs.
func TestServiceAccountTokenOfABoundAccountIsRefused(t *testing.T) {
	falseVal := false
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		obj     map[string]any
		want    string // "" means allowed
	}{
		// End to end a bare Pod that gets a token is refused anyway: admission
		// adds a projected serviceAccountToken volume that checkPodSpec refuses.
		// This row is the binding rule alone, which is what workload templates get.
		{"a pod spec whose default account no binding names", nil,
			tokenPod(nil), ""},
		{"a bare pod whose default account a RoleBinding names",
			[]runtime.Object{saBinding("apps", "default-edit", saSubject("apps", "default"))},
			tokenPod(nil), "RoleBinding apps/default-edit"},
		{"a Deployment template naming a cluster-admin account",
			[]runtime.Object{saClusterBinding("op-admin", saSubject("apps", "op"))},
			tokenDeployment(map[string]any{"serviceAccountName": "op"}), "ClusterRoleBinding op-admin"},
		{"the same template with automount false",
			[]runtime.Object{saClusterBinding("op-admin", saSubject("apps", "op"))},
			tokenDeployment(map[string]any{"serviceAccountName": "op", "automountServiceAccountToken": false}), ""},
		{"a pod with automount explicitly false",
			[]runtime.Object{saBinding("apps", "default-edit", saSubject("apps", "default"))},
			tokenPod(map[string]any{"automountServiceAccountToken": false}), ""},
		{"an account that sets automount false itself",
			[]runtime.Object{
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "op"}, AutomountServiceAccountToken: &falseVal},
				saClusterBinding("op-admin", saSubject("apps", "op")),
			},
			tokenPod(map[string]any{"serviceAccountName": "op"}), ""},
		{"a pod that overrides its account's automount false",
			[]runtime.Object{
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "op"}, AutomountServiceAccountToken: &falseVal},
				saClusterBinding("op-admin", saSubject("apps", "op")),
			},
			tokenPod(map[string]any{"serviceAccountName": "op", "automountServiceAccountToken": true}), "ClusterRoleBinding op-admin"},
		{"the deprecated serviceAccount field",
			[]runtime.Object{saClusterBinding("op-admin", saSubject("apps", "op"))},
			tokenPod(map[string]any{"serviceAccount": "op"}), "ClusterRoleBinding op-admin"},
		{"a RoleBinding in another namespace naming the account",
			[]runtime.Object{saBinding("prod", "cross", saSubject("apps", "default"))},
			tokenPod(nil), "RoleBinding prod/cross"},
		{"a ServiceAccount subject with no namespace takes the binding's",
			[]runtime.Object{saBinding("apps", "local", rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "default"})},
			tokenPod(nil), "RoleBinding apps/local"},
		{"a same-named account in another namespace is not this one",
			[]runtime.Object{saClusterBinding("other", saSubject("prod", "default"))},
			tokenPod(nil), ""},
		{"the account's user name",
			[]runtime.Object{saClusterBinding("by-user", rbacv1.Subject{Kind: rbacv1.UserKind, Name: "system:serviceaccount:apps:default"})},
			tokenPod(nil), "ClusterRoleBinding by-user"},
		{"the group of every account in the namespace",
			[]runtime.Object{saClusterBinding("by-ns-group", rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "system:serviceaccounts:apps"})},
			tokenPod(nil), "ClusterRoleBinding by-ns-group"},
		{"a Service beside a bound default account",
			[]runtime.Object{saBinding("apps", "default-edit", saSubject("apps", "default"))},
			map[string]any{"apiVersion": "v1", "kind": "Service", "spec": map[string]any{
				"ports": []any{map[string]any{"port": int64(80)}},
			}}, ""},
		{"a workload's own spec is not read as a pod running as default",
			[]runtime.Object{saBinding("apps", "default-edit", saSubject("apps", "default"))},
			tokenDeployment(map[string]any{"serviceAccountName": "op", "automountServiceAccountToken": false}), ""},
		{"the stock groups that reach every pod are not counted",
			[]runtime.Object{saClusterBinding("discovery",
				rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "system:serviceaccounts"},
				rbacv1.Subject{Kind: rbacv1.GroupKind, Name: "system:authenticated"})},
			tokenPod(nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := k8sfake.NewClientset(tc.objects...)
			err := checkServiceAccountTokens(t.Context(), cs, tc.obj, "apps")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("allowed, want a refusal naming %q", tc.want)
			}
			for _, part := range []string{tc.want, "pod_logs", "automountServiceAccountToken: false"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("refusal %q does not name %q", err, part)
				}
			}
		})
	}
}

// checkApplyResult, which judges the object resource_apply would store, runs
// the token check and says the refusal is about that object; and the fix the
// refusal names gets through.
func TestApplyResultIsCheckedForBoundServiceAccountTokens(t *testing.T) {
	cs := k8sfake.NewClientset(saClusterBinding("op-admin", saSubject("apps", "op")))
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	err := checkApplyResult(t.Context(), cs, gvr, tokenDeployment(map[string]any{"serviceAccountName": "op"}), "apps")
	if err == nil || !strings.Contains(err.Error(), "ClusterRoleBinding op-admin") || !strings.Contains(err.Error(), "would store") {
		t.Fatalf("err = %v, want the token refusal on the stored object", err)
	}
	fixed := tokenDeployment(map[string]any{"serviceAccountName": "op", "automountServiceAccountToken": false})
	if err := checkApplyResult(t.Context(), cs, gvr, fixed, "apps"); err != nil {
		t.Fatalf("the fix the refusal names was refused: %v", err)
	}
}

// An object with no namespace, such as a cluster-scoped resource carrying a
// pod spec, is checked against the accounts in default.
func TestServiceAccountTokenCheckWithNoNamespaceUsesDefault(t *testing.T) {
	cs := k8sfake.NewClientset(saBinding("default", "default-edit", saSubject("default", "default")))
	err := checkServiceAccountTokens(t.Context(), cs, tokenPod(nil), "")
	if err == nil || !strings.Contains(err.Error(), "RoleBinding default/default-edit") {
		t.Fatalf("err = %v, want the refusal naming the binding in default", err)
	}
}

// A service account that cannot be read refuses the apply rather than let it
// through: the account might switch automount off, or might not.
func TestServiceAccountTokenCheckRefusesWhenTheAccountCannotBeRead(t *testing.T) {
	cs := k8sfake.NewClientset()
	cs.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	err := checkServiceAccountTokens(t.Context(), cs, tokenPod(nil), "apps")
	if err == nil || !strings.Contains(err.Error(), `read service account "default"`) {
		t.Fatalf("err = %v, want the read failure", err)
	}
}

// Bindings that cannot be read refuse the apply rather than let it through.
func TestServiceAccountTokenCheckRefusesWhenBindingsCannotBeRead(t *testing.T) {
	cs := k8sfake.NewClientset()
	cs.PrependReactor("list", "clusterrolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	err := checkServiceAccountTokens(t.Context(), cs, tokenPod(nil), "apps")
	if err == nil || !strings.Contains(err.Error(), "list cluster role bindings") {
		t.Fatalf("err = %v, want the list failure", err)
	}
}
