package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
	"github.com/ctrliq/stratum/internal/version"
)

// Exercise the registered production handler, not a probe of a package-global
// client builder: constructing another server must leave this one's credential
// and request timeout intact.
func TestCatalogNodeCredentialsStayWithTheirServer(t *testing.T) {
	fakeStratumHost(t)
	var callsA, callsB atomic.Int32
	api := func(calls *atomic.Int32, delay time.Duration) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"SecretList","items":[]}`)
		}))
		t.Cleanup(s.Close)
		return s
	}
	a, b := api(&callsA, time.Hour), api(&callsB, 0)
	pathA := kubetest.Kubeconfig(t, &rest.Config{Host: a.URL})
	pathB := kubetest.Kubeconfig(t, &rest.Config{Host: b.URL})
	oldConf := version.AdminConf
	version.AdminConf = pathB
	t.Cleanup(func() { version.AdminConf = oldConf })

	connectCatalog := func(t *testing.T, path string, timeout time.Duration) *sdk.ClientSession {
		srv := NewServer(Options{Version: "test"})
		node := kube.NewConnector(kube.Options{Kubeconfig: path, Timeout: timeout})
		for _, tool := range Catalog("test", kube.NewConnector(kube.Options{}), node, WritePolicy{}) {
			tool.Add(srv, catalogTool(tool.Name, tool.Description, tool.Tier))
		}
		serverT, clientT := sdk.NewInMemoryTransports()
		go func() { _ = srv.Run(t.Context(), serverT) }()
		client, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), clientT, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	first := connectCatalog(t, pathA, 250*time.Millisecond)
	second := connectCatalog(t, pathB, time.Second)
	res, err := first.CallTool(t.Context(), &sdk.CallToolParams{Name: "token_list"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || callsA.Load() == 0 || callsB.Load() != 0 {
		t.Fatalf("first server lost its node connector/timeout: error=%v, calls A=%d B=%d", res.IsError, callsA.Load(), callsB.Load())
	}
	res, err = second.CallTool(t.Context(), &sdk.CallToolParams{Name: "token_list"})
	if err != nil || res.IsError || callsB.Load() != 1 {
		t.Fatalf("second server did not use its own connector: result=%+v err=%v calls B=%d", res, err, callsB.Load())
	}

	for i := range 4 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			client := connectCatalog(t, pathB, time.Second)
			res, err := client.CallTool(t.Context(), &sdk.CallToolParams{Name: "token_list"})
			if err != nil || res.IsError {
				t.Fatalf("independent catalog failed: result=%+v err=%v", res, err)
			}
		})
	}
}
