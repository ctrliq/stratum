package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// Protocol-boundary tests for the Stratum read toolset.

// stratumToolset is the eight tools of the Stratum read toolset, version_get
// included.
var stratumToolset = []string{
	"certs_check",
	"cluster_status",
	"registry_check",
	"snapshot_list",
	"token_list",
	"uninstall_preview",
	"upgrade_plan",
	"version_get",
}

// newHostSession is a session with no cluster configured at all: these
// tools do not use one.
func newHostSession(t *testing.T) *sdk.ClientSession {
	t.Helper()

	conn := kube.NewConnector(kube.Options{})
	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
	registerAll(newRegistrar(srv, false, false),
		rootConfig{displayVersion: "v0-test"}, conn, testConnector())

	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// All nine present with no flags.
func TestStratumToolsetIsPresentAtEveryTier(t *testing.T) {
	for _, tc := range []struct {
		name               string
		write, destructive bool
	}{
		{"no flags", false, false},
		{"--allow-write", true, false},
		{"--allow-destructive", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := kube.NewConnector(kube.Options{})
			srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
			registerAll(newRegistrar(srv, tc.write, tc.destructive),
				rootConfig{displayVersion: "v0-test"}, conn, testConnector())

			serverT, clientT := sdk.NewInMemoryTransports()
			go func() { _ = srv.Run(t.Context(), serverT) }()
			cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).
				Connect(t.Context(), clientT, nil)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer cs.Close()

			res, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			var names []string
			for _, tool := range res.Tools {
				names = append(names, tool.Name)
			}
			for _, want := range stratumToolset {
				if !slices.Contains(names, want) {
					t.Errorf("%s is missing", want)
				}
			}
		})
	}
}

// A protocol-boundary call for each of the nine.
func TestStratumToolsExplainThemselvesOffANode(t *testing.T) {
	cs := newHostSession(t)

	args := map[string]map[string]any{
		"upgrade_plan": {"target": "v1.37.0"}, // required argument
	}

	for _, name := range stratumToolset {
		t.Run(name, func(t *testing.T) {
			res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{
				Name: name, Arguments: args[name],
			})
			if err != nil {
				t.Fatalf("transport error calling %s: %v", name, err)
			}

			if name == "version_get" {
				if res.IsError {
					t.Fatalf("version_get failed: %+v", res.Content)
				}
				return
			}

			if !res.IsError {
				// Only reachable if the test machine really is a Stratum
				// node, in which case the tool answered for real and there
				// is nothing wrong.
				raw, _ := json.Marshal(res.StructuredContent)
				t.Skipf("%s answered, so this machine looks like a Stratum node: %s", name, raw)
			}

			var msg strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*sdk.TextContent); ok {
					msg.WriteString(tc.Text)
				}
			}
			got := msg.String()
			for _, want := range []string{"not a Stratum node", "generic Kubernetes tools"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s failed with a message that does not explain itself.\ngot:  %s\nwant it to contain: %q",
						name, got, want)
				}
			}
		})
	}
}

// Every Stratum tool must be read-only annotated, or the registry drops it
// from every tier including read.
func TestStratumToolsAreAnnotatedReadOnly(t *testing.T) {
	res, err := newHostSession(t).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		if !slices.Contains(stratumToolset, tool.Name) {
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
}

// upgrade_plan's target is required, and the failure has to say so rather
// than reporting a plan for the empty string.
func TestUpgradePlanRequiresATarget(t *testing.T) {
	cs := newHostSession(t)
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "upgrade_plan"})
	if err != nil {
		return // schema rejected it before the handler; equally fine
	}
	if !res.IsError {
		t.Fatal("upgrade_plan produced a plan with no target")
	}
}

// TestSnapshotScheduleAdvertisesNoDestination.
//
// A schema-level assertion, because this is the half that matters: a model
// only attempts what the advertised input offers. An etcd snapshot is the
// whole cluster, and the endpoint additionally carries the operator's
// object-store credentials to whatever host it names, so neither is
// reachable from a tool call.
func TestSnapshotScheduleAdvertisesNoDestination(t *testing.T) {
	tool, ok := advertised(t, tierSession(t, true, false))["snapshot_schedule"]
	if !ok {
		t.Fatal("snapshot_schedule is not advertised with --allow-write")
	}
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	for _, banned := range []string{"endpoint", "bucket", "region", "prefix", "path_style", "access", "secret"} {
		if strings.Contains(string(schema), `"`+banned+`"`) {
			t.Errorf("snapshot_schedule advertises %q; the snapshot destination is an operator decision: %s",
				banned, schema)
		}
	}
	if !strings.Contains(string(schema), `"retain"`) {
		t.Errorf("snapshot_schedule no longer advertises retain: %s", schema)
	}
}

// TestTokenCreateAdvertisesNoServerURL. The schema is the half that matters:
// a model only attempts what the advertised input offers. The join URL is
// published in cluster-info and is where a joiner sends its bootstrap
// credential, so it is derived from the node rather than accepted from a call.
func TestTokenCreateAdvertisesNoServerURL(t *testing.T) {
	tool, ok := advertised(t, tierSession(t, true, false))["token_create"]
	if !ok {
		t.Fatal("token_create is not advertised with --allow-write")
	}
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	for _, banned := range []string{"server", "url", "endpoint", "apiserver"} {
		if strings.Contains(strings.ToLower(string(schema)), `"`+banned+`"`) {
			t.Errorf("token_create advertises %q; the join URL is derived from the node: %s", banned, schema)
		}
	}
	if !strings.Contains(string(schema), `"ttl_hours"`) {
		t.Errorf("token_create no longer advertises ttl_hours: %s", schema)
	}
}
