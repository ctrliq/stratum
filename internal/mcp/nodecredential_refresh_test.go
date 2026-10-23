package mcp

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
)

func TestNodeCredentialBuilderReloadsReissuedCredentials(t *testing.T) {
	// CA rotation reissues the embedded admin credentials between phases.
	// Changing the embedded bearer credential exercises the same config reload
	// without a live control plane or a full certificate rotation.
	var authorization atomic.Value
	authorization.Store("Bearer before-rotation")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != authorization.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`)
			return
		}
		fmt.Fprint(w, `{"kind":"SecretList","apiVersion":"v1","items":[]}`)
	}))
	t.Cleanup(srv.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	path := kubetest.Kubeconfig(t, &rest.Config{Host: srv.URL, BearerToken: "before-rotation", TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
	conn := kube.NewConnector(kube.Options{Kubeconfig: path})
	buildClient := nodeClientBuilder(conn)

	for _, token := range []string{"before-rotation", "after-trust-new", "after-sign-new"} {
		reissued := kubetest.Kubeconfig(t, &rest.Config{Host: srv.URL, BearerToken: token, TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
		data, err := os.ReadFile(reissued)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		authorization.Store("Bearer " + token)
		cs, err := buildClient()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CoreV1().Secrets("kube-system").List(t.Context(), metav1.ListOptions{}); err != nil {
			t.Fatalf("rebuilt client after %s did not load the reissued credential: %v", token, err)
		}
		// Tools holding the node connector also need the refreshed client.
		cached, err := conn.Cluster("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cached.Typed.CoreV1().Secrets("kube-system").List(t.Context(), metav1.ListOptions{}); err != nil {
			t.Fatalf("node connector retained the obsolete credential after %s: %v", token, err)
		}
	}
}
