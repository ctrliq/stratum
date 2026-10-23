package operator

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"helm.sh/helm/v4/pkg/release/common"
	helmrelease "helm.sh/helm/v4/pkg/release/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestContextTransportCancelsInflightRequest(t *testing.T) {
	for _, cancelOwner := range []bool{false, true} {
		name := "request"
		if cancelOwner {
			name = "owner"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner, stopOwner := context.WithCancel(context.Background())
				defer stopOwner()
				request, stopRequest := context.WithCancel(context.Background())
				defer stopRequest()
				started := make(chan struct{})
				tr := &contextTransport{ctx: owner, next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					close(started)
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}
				req, err := http.NewRequestWithContext(request, http.MethodPost, "http://cluster/resource", nil)
				if err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() { _, err := tr.RoundTrip(req); result <- err }()
				<-started
				if cancelOwner {
					stopOwner()
				} else {
					stopRequest()
				}
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want context cancellation", err)
				}
			})
		})
	}
}

func TestContextTransportKeepsBodyReadableUntilClosed(t *testing.T) {
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	var requestCtx context.Context
	tr := &contextTransport{ctx: owner, next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestCtx = r.Context()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("response"))}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "http://cluster/resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if requestCtx.Err() != nil {
		t.Fatal("cancelled the request before its body could be read")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatal("closing the response body did not clean up its context")
	}
}

// Exercise the actual Helm configuration, not just the transport helper:
// both release storage and Kubernetes clients must use the lifetime wrapper.
func TestHelmClientsCannotWriteAfterCancellation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			_, _ = io.WriteString(w, `{"gitVersion":"test"}`)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/secrets") {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"sh.helm.release.v1.test.v1","namespace":"test"}}`)
			return
		}
		t.Errorf("unexpected Helm API request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusNotFound)
	}))
	defer server.Close()
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\n    namespace: test\ncurrent-context: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg, err := newActionConfig(ctx, kubeconfig, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.KubeClient.IsReachable(); err != nil {
		t.Fatalf("active Helm Kubernetes client failed: %v", err)
	}
	release := &helmrelease.Release{Name: "test", Namespace: "test", Version: 1, Info: &helmrelease.Info{Status: common.StatusDeployed}}
	if err := cfg.Releases.Create(release); err != nil {
		t.Fatalf("active Helm release storage failed: %v", err)
	}
	cancel()
	before := requests.Load()
	if err := cfg.KubeClient.IsReachable(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Helm Kubernetes client ignored cancellation: %v", err)
	}
	release.Version = 2
	if err := cfg.Releases.Create(release); !errors.Is(err, context.Canceled) {
		t.Fatalf("Helm storage ignored cancellation: %v", err)
	}
	if after := requests.Load(); after != before {
		t.Fatalf("cancelled Helm clients issued %d more requests", after-before)
	}
}
