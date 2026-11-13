package k8sutil

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

func TestOwnNodeUIDUsesNodeAPIAndRequestContext(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(fmt.Sprint(hang), func(t *testing.T) {
			cancelled := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/nodes/cp-1" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				if hang {
					<-r.Context().Done()
					close(cancelled)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Node","metadata":{"name":"cp-1","uid":"immutable-uid"}}`)
			}))
			defer srv.Close()
			old := version.KubeconfigDir
			version.KubeconfigDir = t.TempDir()
			t.Cleanup(func() { version.KubeconfigDir = old })
			config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", srv.URL)
			if err := os.WriteFile(filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			uid, err := OwnNodeUID(ctx, "cp-1")
			if !hang {
				if err != nil || uid != "immutable-uid" {
					t.Fatalf("uid %q, error %v", uid, err)
				}
				return
			}
			if err == nil || uid != "" {
				t.Fatalf("hung API returned uid %q, error %v", uid, err)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("node API did not receive cancellation")
			}
		})
	}
}
