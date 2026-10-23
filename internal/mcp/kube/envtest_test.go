package kube

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
)

// The envtest suite runs against a real kube-apiserver and etcd.

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m)) }

// startEnv is a thin alias so the cases below read plainly.
func startEnv(t *testing.T) *rest.Config { return kubetest.Start(t) }

// writeEnvKubeconfig writes a kubeconfig for the running control plane.
func writeEnvKubeconfig(t *testing.T, cfg *rest.Config, contexts ...string) string {
	return kubetest.Kubeconfig(t, cfg, contexts...)
}

// connectEnv returns a Cluster built the way production builds one: from a
// kubeconfig on disk.
func connectEnv(t *testing.T, o Options) *Cluster {
	t.Helper()
	cfg := startEnv(t)
	isolateHome(t)
	if o.Kubeconfig == "" {
		o.Kubeconfig = writeEnvKubeconfig(t, cfg)
	}
	cl, err := NewConnector(o).Cluster(o.Context)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return cl
}

func TestEnvDiscovery(t *testing.T) {
	cl := connectEnv(t, Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	got, err := cl.APIResources(ctx)
	if err != nil {
		t.Fatalf("APIResources: %v", err)
	}

	index := map[string]APIResource{}
	for _, r := range got {
		key := r.Resource
		if r.Group != "" {
			key = r.Resource + "." + r.Group
		}
		index[key] = r
	}

	for _, want := range []struct {
		key        string
		kind       string
		namespaced bool
	}{
		{"pods", "Pod", true},
		{"namespaces", "Namespace", false},
		{"nodes", "Node", false},
		{"deployments.apps", "Deployment", true},
	} {
		r, ok := index[want.key]
		if !ok {
			t.Errorf("%s missing from discovery", want.key)
			continue
		}
		if r.Kind != want.kind {
			t.Errorf("%s kind = %q, want %q", want.key, r.Kind, want.kind)
		}
		if r.Namespaced != want.namespaced {
			t.Errorf("%s namespaced = %v, want %v", want.key, r.Namespaced, want.namespaced)
		}
	}

	// Subresources are filtered: "pods/log" is not independently listable and
	// only confuses a model choosing a resource to query.
	for _, r := range got {
		if strings.Contains(r.Resource, "/") {
			t.Errorf("subresource %q leaked into the resource list", r.Resource)
		}
	}
}

// The spellings here are the ones a model or an operator actually produces.
func TestEnvResolveResource(t *testing.T) {
	cl := connectEnv(t, Options{})
	ctx := t.Context()

	for _, tc := range []struct {
		in         string
		want       schema.GroupVersionResource
		namespaced bool
	}{
		{"pods", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, true},
		{"pod", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, true},
		{"po", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, true},
		{"Pod", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, true},
		{"nodes", schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, false},
		{"no", schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, false},
		{"ns", schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, false},
		{"deployments", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true},
		{"deploy", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true},
		{"deployments.apps", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true},
		{"Deployment", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true},
		{"configmaps", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, true},
		{"cm", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			gvr, namespaced, err := cl.ResolveResource(ctx, tc.in)
			if err != nil {
				t.Fatalf("ResolveResource(%q): %v", tc.in, err)
			}
			if gvr != tc.want {
				t.Errorf("gvr = %v, want %v", gvr, tc.want)
			}
			if namespaced != tc.namespaced {
				t.Errorf("namespaced = %v, want %v", namespaced, tc.namespaced)
			}
		})
	}

	t.Run("unknown resource names itself in the error", func(t *testing.T) {
		_, _, err := cl.ResolveResource(ctx, "wibbles")
		if err == nil {
			t.Fatal("resolved a resource the cluster does not serve")
		}
		if !strings.Contains(err.Error(), "wibbles") {
			t.Errorf("error %q does not name the input", err)
		}
	})

	t.Run("empty is rejected", func(t *testing.T) {
		if _, _, err := cl.ResolveResource(ctx, ""); err == nil {
			t.Error("resolved the empty string")
		}
	})
}

// The round trip the generic toolset is built on: create through the typed
// client, read it back through the dynamic one.
func TestEnvDynamicRoundTrip(t *testing.T) {
	cl := connectEnv(t, Options{})
	ctx := t.Context()

	const name = "kube-roundtrip"
	_, err := cl.Typed.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string]string{"hello": "world"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create configmap: %v", err)
	}
	t.Cleanup(func() {
		_ = cl.Typed.CoreV1().ConfigMaps("default").Delete(context.Background(), name, metav1.DeleteOptions{})
	})

	gvr, namespaced, err := cl.ResolveResource(ctx, "cm")
	if err != nil {
		t.Fatalf("resolve cm: %v", err)
	}
	if !namespaced {
		t.Fatal("configmaps reported as cluster-scoped")
	}

	t.Run("get", func(t *testing.T) {
		got, err := cl.Dynamic.Resource(gvr).Namespace("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("dynamic get: %v", err)
		}
		data, found, err := unstructured.NestedString(got.Object, "data", "hello")
		if err != nil || !found {
			t.Fatalf("data.hello not found: found=%v err=%v", found, err)
		}
		if data != "world" {
			t.Errorf("data.hello = %q, want world", data)
		}
	})

	t.Run("list", func(t *testing.T) {
		list, err := cl.Dynamic.Resource(gvr).Namespace("default").List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("dynamic list: %v", err)
		}
		for _, item := range list.Items {
			if item.GetName() == name {
				return
			}
		}
		t.Errorf("%s not in the dynamic list of %d configmaps", name, len(list.Items))
	})
}

// The cap has to hold against a real apiserver, not just an httptest server:
// the apiserver streams list responses with no Content-Length, which is the
// path the unit test can only simulate.
func TestEnvResponseLimitAgainstRealAPIServer(t *testing.T) {
	cl := connectEnv(t, Options{MaxResponseBytes: 512})
	ctx := t.Context()

	// Namespaces alone exceed 512 bytes of JSON on any cluster.
	_, err := cl.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if !IsResponseTooLarge(err) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
}

func TestEnvContextSelection(t *testing.T) {
	cfg := startEnv(t)
	isolateHome(t)
	path := writeEnvKubeconfig(t, cfg, "first", "second")

	c := NewConnector(Options{Kubeconfig: path})
	contexts, err := c.Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if len(contexts) != 2 {
		t.Fatalf("got %d contexts, want 2", len(contexts))
	}

	// Both contexts address the same envtest server, so the assertion is
	// that naming either one connects — not that they differ.
	for _, name := range []string{"first", "second"} {
		cl, err := c.Cluster(name)
		if err != nil {
			t.Fatalf("Cluster(%q): %v", name, err)
		}
		if cl.ContextName != name {
			t.Errorf("ContextName = %q, want %q", cl.ContextName, name)
		}
		if cl.Source != SourceExplicit {
			t.Errorf("Source = %q, want %q", cl.Source, SourceExplicit)
		}
	}

	// Caching is per context, and must not hand back the wrong one.
	a, _ := c.Cluster("first")
	b, _ := c.Cluster("first")
	if a != b {
		t.Error("Cluster did not cache per context name")
	}
	if other, _ := c.Cluster("second"); other == a {
		t.Error("two different contexts share a cached Cluster")
	}
}
