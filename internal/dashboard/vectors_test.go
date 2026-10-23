package dashboard

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// The security vectors are the dashboard's security decisions held as data
// rather than repeated across handler tests: origin decisions, the request
// headers that never reach the apiserver, the shapes that escape the proxy
// deadline, and the API paths the parser accepts. A looser decision —
// accepting a plaintext origin behind an HTTPS terminator, say — fails here
// instead of passing silently, and a downstream copy of these handlers can be
// reconciled against the same file.

type originVector struct {
	Name         string `json:"name"`
	PublicOrigin string `json:"publicOrigin"`
	Host         string `json:"host"`
	TLS          bool   `json:"tls"`
	Origin       string `json:"origin"`
	Allowed      bool   `json:"allowed"`
}

type scrubVector struct {
	Name string `json:"name"`
	// StoredAs is the header key exactly as the caller stored it. The prefix
	// scrub must catch family members stored non-canonically, which
	// Header.Del cannot remove.
	StoredAs string `json:"storedAs"`
	Value    string `json:"value"`
}

type streamingVector struct {
	Name      string `json:"name"`
	Method    string `json:"method"`
	URL       string `json:"url"`
	Upgrade   bool   `json:"upgrade"`
	Streaming bool   `json:"streaming"`
}

type pathVector struct {
	Path        string `json:"path"`
	OK          bool   `json:"ok"`
	Group       string `json:"group"`
	Version     string `json:"version"`
	Namespace   string `json:"namespace"`
	Resource    string `json:"resource"`
	Name        string `json:"name"`
	Subresource string `json:"subresource"`
	Verb        string `json:"verb"`
}

type securityVectors struct {
	Origin    []originVector    `json:"origin"`
	Scrubbed  []scrubVector     `json:"scrubbedHeaders"`
	Streaming []streamingVector `json:"streaming"`
	Paths     []pathVector      `json:"paths"`
}

func TestSecurityVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/security-vectors.json")
	if err != nil {
		t.Fatalf("read security vectors: %v", err)
	}
	var v securityVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse security vectors: %v", err)
	}
	if len(v.Origin) == 0 || len(v.Scrubbed) == 0 || len(v.Streaming) == 0 || len(v.Paths) == 0 {
		t.Fatal("the vector file is missing a category; a category with no vectors pins nothing")
	}

	t.Run("origin decisions", func(t *testing.T) {
		for _, tc := range v.Origin {
			t.Run(tc.Name, func(t *testing.T) {
				p := originPolicy{PublicOrigin: tc.PublicOrigin}
				r := httptest.NewRequest(http.MethodGet, "/k8s/api/v1/namespaces/d/pods/p/exec", nil)
				r.Host = tc.Host
				if tc.TLS {
					r.TLS = &tls.ConnectionState{}
				}
				if got := p.allowedOrigin(r, tc.Origin); got != tc.Allowed {
					t.Fatalf("allowedOrigin(%q) with public=%q host=%q tls=%t = %t, want %t",
						tc.Origin, tc.PublicOrigin, tc.Host, tc.TLS, got, tc.Allowed)
				}
			})
		}
	})

	t.Run("scrubbed request headers", func(t *testing.T) {
		out := httptest.NewRequest(http.MethodGet, k8sPrefix+"/api/v1/namespaces/default/pods", nil)
		for _, tc := range v.Scrubbed {
			if tc.StoredAs == "" {
				t.Fatalf("vector %q names no stored key", tc.Name)
			}
			out.Header[tc.StoredAs] = []string{tc.Value}
		}
		rewriteRequest(out, k8sPrefix, target{Scheme: "https", Host: "apiserver:6443"})
		for _, tc := range v.Scrubbed {
			if _, ok := out.Header[tc.StoredAs]; ok {
				t.Errorf("%q (stored as %q) survived the scrub; it must never reach the apiserver", tc.Name, tc.StoredAs)
			}
		}
		// With no session in the context the proxy must install no credential
		// of its own either: the request goes out anonymous.
		if got := out.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want none without a session", got)
		}
		if out.URL.Path != "/api/v1/namespaces/default/pods" || out.URL.Host != "apiserver:6443" || out.URL.Scheme != "https" {
			t.Errorf("retarget produced %s://%s%s", out.URL.Scheme, out.URL.Host, out.URL.Path)
		}
	})

	t.Run("streaming shapes", func(t *testing.T) {
		for _, tc := range v.Streaming {
			t.Run(tc.Name, func(t *testing.T) {
				method := tc.Method
				if method == "" {
					method = http.MethodGet
				}
				r := httptest.NewRequest(method, tc.URL, nil)
				if tc.Upgrade {
					r.Header.Set("Upgrade", "websocket")
				}
				if got := isStreaming(r); got != tc.Streaming {
					t.Fatalf("isStreaming(%s %s, upgrade=%t) = %t, want %t", method, tc.URL, tc.Upgrade, got, tc.Streaming)
				}
			})
		}
	})

	t.Run("parsed API paths", func(t *testing.T) {
		for _, tc := range v.Paths {
			want := k8sRequest{
				Group: tc.Group, Version: tc.Version, Namespace: tc.Namespace,
				Resource: tc.Resource, Name: tc.Name, Subresource: tc.Subresource, Verb: tc.Verb,
			}
			got, ok := parseK8sPath(tc.Path)
			if ok != tc.OK {
				t.Errorf("parseK8sPath(%q) ok = %t, want %t", tc.Path, ok, tc.OK)
				continue
			}
			if ok && got != want {
				t.Errorf("parseK8sPath(%q) = %+v, want %+v", tc.Path, got, want)
			}
		}
	})
}
