package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestStorageMutationPolicy(t *testing.T) {
	for _, resource := range []string{"persistentvolumes", "persistentvolumeclaims"} {
		gvr := schema.GroupVersionResource{Version: "v1", Resource: resource}
		for _, destructive := range []bool{false, true} {
			p := WritePolicy{AllowDestructive: destructive}
			if err := p.check(gvr, false); err == nil {
				t.Fatalf("allowed apply %s", resource)
			}
			if err := p.check(gvr, true); (err == nil) != destructive {
				t.Fatalf("delete %s: %v", resource, err)
			}
		}
	}
}

func TestSnapshotPaging(t *testing.T) {
	items := []etcdsnap.SnapItem{{Name: "b", Location: "s3", Modified: time.Unix(1, 0)}, {Name: "a", Location: "local", Modified: time.Unix(1, 0)}, {Name: "new", Modified: time.Unix(2, 0)}}
	cursor := ""
	var names []string
	for {
		page, next, err := snapshotPage(items, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page {
			names = append(names, item.Name)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(names, ",") != "new,a,b" {
		t.Fatal(names)
	}
	for _, bad := range []string{"garbage", "hash:-1", cursor + "999999999999999999999999999999"} {
		if _, _, err := snapshotPage(items, 1, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	items[0].Size++
	if _, _, err := snapshotPage(items, 1, cursor); err == nil {
		t.Fatal("accepted stale inventory")
	}
	page, next, err := snapshotPage(nil, 10, "")
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("empty: %v %q %v", page, next, err)
	}
}

func TestTokenListPagingAndBounds(t *testing.T) {
	fakeStratumHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/kube-system/secrets" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("fieldSelector") != "type=bootstrap.kubernetes.io/token" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		cont := `"continue":"page2"`
		id := "YWJjZGVm" // abcdef
		if r.URL.Query().Get("continue") == "page2" {
			cont = `"continue":""`
			id = "Z2hpamts"
		}
		fmt.Fprintf(w, `{"apiVersion":"v1","kind":"SecretList","metadata":{%s},"items":[{"metadata":{"name":"bootstrap-token"},"type":"bootstrap.kubernetes.io/token","data":{"token-id":%q,"token-secret":"c2VjcmV0"}}]}`, cont, id)
	}))
	defer server.Close()
	path := kubetest.Kubeconfig(t, &rest.Config{Host: server.URL})
	handler := tokenListWith(kube.NewConnector(kube.Options{Kubeconfig: path}))
	_, first, err := handler(t.Context(), nil, TokenListIn{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].ID != "abcdef" || first.Continue != "page2" || !first.Truncated {
		t.Fatalf("first %+v", first)
	}
	_, last, err := handler(t.Context(), nil, TokenListIn{Limit: 1, Continue: first.Continue})
	if err != nil || len(last.Items) != 1 || last.Items[0].ID != "ghijkl" || last.Truncated {
		t.Fatalf("last %+v %v", last, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := handler(ctx, nil, TokenListIn{Limit: 1}); err == nil {
		t.Fatal("ignored cancellation")
	}
	capped := tokenListWith(kube.NewConnector(kube.Options{Kubeconfig: path, MaxResponseBytes: 16}))
	if _, _, err := capped(t.Context(), nil, TokenListIn{Limit: 1}); err == nil {
		t.Fatal("ignored response cap")
	}
}

// TestCredentialInvalidation_DestructiveToolsStillInvalidate pins the
// invalidation wiring: certs_rotate, certs_rotate_ca and ha_repoint can
// rewrite the credentials and endpoints cached by the server's configurable
// and node connectors,
// and such an operation ends three ways — success, partial failure, or
// cancellation mid-run. However it ends, the handler must leave both caches
// invalidated, so the next tool call re-resolves the kubeconfigs as they now
// stand instead of reusing a client built from the pre-rotation files. The
// kubeconfigs here never change while a subtest runs, so the connector's
// content fingerprints alone would hand the captured cluster straight back:
// only the invalidation the operation runs can make the second Cluster call
// rebuild. Invalidation changes what is handed out next; a cluster handed out
// before the operation keeps serving requests already in flight.
func TestCredentialInvalidation_DestructiveToolsStillInvalidate(t *testing.T) {
	// capturedCluster builds a connector on a kubeconfig that stays put for
	// the whole subtest and returns the cluster it caches.
	capturedCluster := func(t *testing.T) (*kube.Connector, *kube.Cluster) {
		t.Helper()
		// TEST-NET-1 is unreachable by design: building a cluster contacts
		// nothing, and anything that dialed would fail instead of hang.
		path := kubetest.Kubeconfig(t, &rest.Config{Host: "https://192.0.2.1:6443"})
		conn := kube.NewConnector(kube.Options{Kubeconfig: path})
		cl, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster: %v", err)
		}
		return conn, cl
	}
	// requireInvalidated fails the subtest when the connector still serves
	// the cluster captured before the operation ran.
	requireInvalidated := func(t *testing.T, conn *kube.Connector, before *kube.Cluster) {
		t.Helper()
		after, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster after the operation: %v", err)
		}
		if after == before {
			t.Error("the operation left the pre-operation cluster in the cache: the next " +
				"tool call would run on the credentials and endpoint read before it")
		}
	}

	t.Run("ha_repoint invalidates after a failed repoint", func(t *testing.T) {
		fakeStratumHost(t)
		// The repoint fails at its first step, the VIP check, which is a
		// var so a test can stand in for a VIP that does not answer.
		oldCheck := ha.VIPServes
		ha.VIPServes = func(string) error { return errors.New("the VIP does not answer") }
		t.Cleanup(func() { ha.VIPServes = oldCheck })

		conn, before := capturedCluster(t)
		nodeConn, nodeBefore := capturedCluster(t)
		var err error
		_ = captureStderrDiscard(func() {
			_, _, err = HARepointHandler(conn, nodeConn)(t.Context(), nil, HARepointIn{VIP: "10.0.0.1"})
		})
		if err == nil {
			t.Fatal("the repoint succeeded against a VIP that does not answer")
		}
		requireInvalidated(t, conn, before)
		requireInvalidated(t, nodeConn, nodeBefore)
	})

	t.Run("ha_repoint invalidates after a cancelled Cilium repoint", func(t *testing.T) {
		fakeStratumHost(t)
		// clusterOfThisNode reads this node's admin.conf beside the
		// connector's credential; point it at a real kubeconfig whose API is
		// never reached, so the only failure is the dead request context.
		oldConf := version.AdminConf
		version.AdminConf = kubetest.Kubeconfig(t, &rest.Config{Host: "https://192.0.2.1:6443"})
		t.Cleanup(func() { version.AdminConf = oldConf })
		setThisNodeName(t, "cp1")

		conn, before := capturedCluster(t)
		nodeConn, nodeBefore := capturedCluster(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var err error
		_ = captureStderrDiscard(func() {
			_, _, err = HARepointHandler(conn, nodeConn)(ctx, nil, HARepointIn{VIP: "10.0.0.1", Cilium: true})
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the cancellation honored before any cluster-wide work", err)
		}
		requireInvalidated(t, conn, before)
		requireInvalidated(t, nodeConn, nodeBefore)
	})

	t.Run("certs_rotate invalidates after a cancelled rotation", func(t *testing.T) {
		fakeStratumHost(t)
		conn, before := capturedCluster(t)
		nodeConn, nodeBefore := capturedCluster(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var err error
		_ = captureStderrDiscard(func() {
			_, _, err = CertsRotateHandler(conn, nodeConn)(ctx, nil, CertsRotateIn{})
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the cancellation honored before the rotation started", err)
		}
		requireInvalidated(t, conn, before)
		requireInvalidated(t, nodeConn, nodeBefore)
	})

	t.Run("certs_rotate_ca invalidates after a cancelled phase", func(t *testing.T) {
		fakeStratumHost(t)
		// Point admin.conf at a path that cannot exist, as the cancellation
		// pin for these tools does: a phase that read credentials before
		// honoring the context would fail with the missing file instead.
		oldConf := version.AdminConf
		version.AdminConf = filepath.Join(t.TempDir(), "missing-admin.conf")
		t.Cleanup(func() { version.AdminConf = oldConf })

		conn, before := capturedCluster(t)
		nodeConn, nodeBefore := capturedCluster(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var err error
		_ = captureStderrDiscard(func() {
			_, _, err = CertsRotateCAHandler(conn, nodeConn)(ctx, nil, CertsRotateCAIn{Cluster: true})
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the cancellation honored before any phase ran", err)
		}
		requireInvalidated(t, conn, before)
		requireInvalidated(t, nodeConn, nodeBefore)
	})
}

// TestTokenList_RidesTheNodeConnectorsTimeout is the internal/mcp companion
// to pkg/cli's TestNodeConnector_CarriesTheServersRequestTimeout. The
// registered token_list must build on the node connector the server passes
// the catalog — one carrying the server's --request-timeout — not on a
// private admin.conf connector with the 30-second default. pkg/cli cannot
// make that call:
// token_list's Stratum host check fails off a node, so the behavioural half
// of the guard lives here, where fakeStratumHost can satisfy it.
func TestTokenList_RidesTheNodeConnectorsTimeout(t *testing.T) {
	fakeStratumHost(t)

	// The API server behind admin.conf answers token_list's secrets list
	// after 2s: which connector served the call decides whether the call
	// fails fast or succeeds late.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"SecretList","metadata":{},"items":[]}`)
	}))
	t.Cleanup(slow.Close)

	oldConf := version.AdminConf
	version.AdminConf = kubetest.Kubeconfig(t, &rest.Config{Host: slow.URL})
	t.Cleanup(func() { version.AdminConf = oldConf })

	t.Run("tokenListWith applies the connector's request timeout", func(t *testing.T) {
		// The seam the registered tool rides, checked directly against the
		// guard's pairing: a 100ms connector against a server that answers
		// after 2s must fail the call well before the server answers.
		conn := kube.NewConnector(kube.Options{Kubeconfig: version.AdminConf, Timeout: 100 * time.Millisecond})
		start := time.Now()
		_, _, err := tokenListWith(conn)(t.Context(), nil, TokenListIn{})
		if err == nil {
			t.Fatal("token_list answered through a 100ms connector against a server that answers after 2s: the connector's request timeout does not bound the call")
		}
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Fatalf("token_list failed only after %s: the call outlived the connector's 100ms timeout", elapsed)
		}
	})

	t.Run("the registered token_list rides the node connector", func(t *testing.T) {
		// Registered exactly as pkg/cli registers it: the catalog's token_list
		// row builds its handler on the node connector it is given, so a
		// 100ms node connector must bound the call. A row that goes back to a
		// private connector lets the call answer at 2s instead — the drift
		// this pin fails on.
		nodeConn := kube.NewConnector(kube.Options{Kubeconfig: version.AdminConf, Timeout: 100 * time.Millisecond})
		cat := Catalog("v0-test", kube.NewConnector(kube.Options{}), nodeConn, WritePolicy{})
		var row *Tool
		for i := range cat {
			if cat[i].Name == "token_list" {
				row = &cat[i]
			}
		}
		if row == nil {
			t.Fatal("the catalog carries no token_list row")
		}

		srv := NewServer(Options{Version: "v0-test"})
		row.Add(srv, &sdk.Tool{Name: "token_list", Description: row.Description})
		serverT, clientT := sdk.NewInMemoryTransports()
		go func() { _ = srv.Run(t.Context(), serverT) }()
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).
			Connect(t.Context(), clientT, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = cs.Close() })

		start := time.Now()
		res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "token_list"})
		if err == nil && !res.IsError {
			t.Fatal("token_list answered a 2s API server: the registered tool built on a private connector with the default timeout instead of the node connector")
		}
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Fatalf("token_list's call failed only after %s: it did not ride the node connector's 100ms request timeout", elapsed)
		}
	})
}

// TestNodeCredentialOperations_RideTheNodeConnectorsTimeout checks that the
// reload-capable builder preserves its connector's request timeout. Registered
// handler isolation is covered by TestCatalogNodeCredentialsStayWithTheirServer.
func TestNodeCredentialOperations_RideTheNodeConnectorsTimeout(t *testing.T) {
	// The API server behind admin.conf answers a secrets list after 2s:
	// which builder served the call decides whether it fails fast or
	// succeeds late.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"SecretList","metadata":{},"items":[]}`)
	}))
	t.Cleanup(slow.Close)

	oldConf := version.AdminConf
	version.AdminConf = kubetest.Kubeconfig(t, &rest.Config{Host: slow.URL})
	t.Cleanup(func() { version.AdminConf = oldConf })

	buildClient := nodeClientBuilder(kube.NewConnector(kube.Options{Kubeconfig: version.AdminConf, Timeout: 100 * time.Millisecond}))

	cs, err := buildClient()
	if err != nil {
		t.Fatalf("resolve the node credential: %v", err)
	}
	start := time.Now()
	_, err = cs.CoreV1().Secrets("kube-system").List(t.Context(), v1.ListOptions{})
	if err == nil {
		t.Fatal("the node-credential builder answered a 2s API server: the builder lost the node connector's request timeout")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("the node-credential call failed only after %s: it did not ride the node connector's 100ms request timeout", elapsed)
	}
}
