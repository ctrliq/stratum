package operator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	helmrelease "helm.sh/helm/v3/pkg/release"
	"k8s.io/client-go/rest"
)

// TestNewActionConfigImpersonates proves the requester-impersonation wiring end-to-end at the
// client level: when newActionConfig is given an impersonation config, EVERY
// Kubernetes call it makes — including the release-storage Secret writes — carries
// the Impersonate-User / Impersonate-Group headers, so the apiserver authorizes
// as the requester, not as the operator's system:masters. With no impersonation
// config, no such headers are sent (privileged-namespace / operator installs).
func TestNewActionConfigImpersonates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		imp        *rest.ImpersonationConfig
		wantUser   string
		wantGroups []string
	}{
		{
			name:       "impersonated",
			imp:        &rest.ImpersonationConfig{UserName: "alice", Groups: []string{"team-a", "system:authenticated"}, UID: "u1"},
			wantUser:   "alice",
			wantGroups: []string{"team-a", "system:authenticated"},
		},
		{
			name: "operator identity (nil)",
			imp:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotUser, gotUID string
			var gotGroups []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/version" {
					_, _ = io.WriteString(w, `{"gitVersion":"v1.35.0"}`)
					return
				}
				// Capture impersonation headers off the release-storage write.
				gotUser = r.Header.Get("Impersonate-User")
				gotUID = r.Header.Get("Impersonate-Uid")
				gotGroups = r.Header.Values("Impersonate-Group")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"sh.helm.release.v1.app.v1","namespace":"team-a"}}`)
			}))
			defer server.Close()

			kubeconfig := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\n    namespace: team-a\ncurrent-context: test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg, err := newActionConfig(ctx, kubeconfig, "team-a", tc.imp)
			if err != nil {
				t.Fatal(err)
			}
			rel := &helmrelease.Release{Name: "app", Namespace: "team-a", Version: 1, Info: &helmrelease.Info{Status: helmrelease.StatusDeployed}}
			if err := cfg.Releases.Create(rel); err != nil {
				t.Fatalf("release storage create: %v", err)
			}
			if gotUser != tc.wantUser {
				t.Errorf("Impersonate-User = %q, want %q", gotUser, tc.wantUser)
			}
			if tc.imp == nil {
				if len(gotGroups) != 0 || gotUID != "" {
					t.Errorf("operator identity must send no impersonation headers; got user=%q uid=%q groups=%v", gotUser, gotUID, gotGroups)
				}
				return
			}
			if gotUID != tc.imp.UID {
				t.Errorf("Impersonate-Uid = %q, want %q", gotUID, tc.imp.UID)
			}
			for _, g := range tc.wantGroups {
				if !contains(gotGroups, g) {
					t.Errorf("Impersonate-Group missing %q; got %v", g, gotGroups)
				}
			}
		})
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
