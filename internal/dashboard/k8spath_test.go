package dashboard

import (
	"net/url"
	"testing"
)

func TestParseK8sPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
		want k8sRequest
	}{
		{path: "/api/v1/pods", ok: true, want: k8sRequest{Version: "v1", Resource: "pods"}},
		{path: "/api/v1/namespaces/default/pods", ok: true, want: k8sRequest{Version: "v1", Namespace: "default", Resource: "pods"}},
		{path: "/api/v1/namespaces/default/pods/web-0", ok: true, want: k8sRequest{Version: "v1", Namespace: "default", Resource: "pods", Name: "web-0"}},
		{path: "/api/v1/namespaces/default/pods/web-0/exec", ok: true, want: k8sRequest{Version: "v1", Namespace: "default", Resource: "pods", Name: "web-0", Subresource: "exec"}},
		{path: "/api/v1/namespaces/default", ok: true, want: k8sRequest{Version: "v1", Resource: "namespaces", Name: "default"}},
		{path: "/apis/apps/v1/namespaces/kube-system/deployments/coredns", ok: true,
			want: k8sRequest{Group: "apps", Version: "v1", Namespace: "kube-system", Resource: "deployments", Name: "coredns"}},
		// The legacy watch form addresses the same resource by a different
		// path. Reading "watch" as the resource name is the bug this parser
		// exists to prevent, and the node-shell gate depends on it.
		{path: "/api/v1/watch/namespaces/default/pods", ok: true,
			want: k8sRequest{Version: "v1", Verb: "watch", Namespace: "default", Resource: "pods"}},
		{path: "/api/v1/watch/pods", ok: true, want: k8sRequest{Version: "v1", Verb: "watch", Resource: "pods"}},
		// Not resource requests.
		{path: "/version"},
		{path: "/healthz"},
		{path: "/"},
		{path: ""},
		{path: "/api/v1"},
		{path: "/apis/apps/v1"},
		{path: "/api/v1/watch"},
	} {
		got, ok := parseK8sPath(tc.path)
		if ok != tc.ok {
			t.Errorf("parseK8sPath(%q) ok = %t, want %t", tc.path, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseK8sPath(%q) = %+v, want %+v", tc.path, got, tc.want)
		}
	}
}

func TestStripPrefixKeepsTheEncodedForm(t *testing.T) {
	for _, tc := range []struct{ in, wantPath, wantRaw string }{
		{in: "/k8s/api/v1/pods", wantPath: "/api/v1/pods"},
		{in: "/k8s", wantPath: "/"},
		{in: "/k8s/", wantPath: "/"},
		// A %2F in a resource name must survive: unescaped it would address a
		// different object, or none.
		{in: "/k8s/api/v1/namespaces/default/configmaps/a%2Fb", wantPath: "/api/v1/namespaces/default/configmaps/a/b", wantRaw: "/api/v1/namespaces/default/configmaps/a%2Fb"},
	} {
		u := mustParseURL(t, tc.in)
		stripPrefix(u, k8sPrefix)
		if u.Path != tc.wantPath {
			t.Errorf("stripPrefix(%q).Path = %q, want %q", tc.in, u.Path, tc.wantPath)
		}
		if u.RawPath != tc.wantRaw {
			t.Errorf("stripPrefix(%q).RawPath = %q, want %q", tc.in, u.RawPath, tc.wantRaw)
		}
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}
