package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// The engine's tool catalog, pinned as data beside the handlers.
//
// Catalog returns one descriptor per tool the engine ships: the name and
// description the client sees in tools/list, the tier that decides whether
// the tool registers at all, and Add, which adds the tool's typed handler
// once the host registrar has admitted the descriptor. pkg/cli's registerAll
// walks the catalog through an unexported registrar method that applies the
// same tier and duplicate-name checks AddTool applies to contributed sets:
// the checks stay with the registrar, the rows live beside the handlers.
//
// These tests restate the shipped tool names because internal/mcp cannot
// import pkg/cli to reuse its pinned lists — pkg/cli imports this package.
// pkg/cli/mcp_surface_test.go stays the integration check: registering the
// catalog must advertise exactly the shipped surface, unchanged.

// catalogReadTools is every read-tier descriptor the catalog carries: the
// engine's own tools plus the generic Kubernetes read toolset. Restated in
// today's registration order; the comparisons sort.
var catalogReadTools = []string{
	"version_get",
	"cluster_status",
	"certs_check",
	"snapshot_list",
	"token_list",
	"registry_check",
	"upgrade_plan",
	"snapshot_restore_copies",
	"uninstall_preview",
	"context_list",
	"resource_list",
	"resource_get",
	"pod_list",
	"pod_get",
	"pod_logs",
	"event_list",
	"namespace_list",
	"node_list",
}

// catalogWriteTools is every write-tier descriptor: mutating but
// recoverable, so absent without --allow-write.
var catalogWriteTools = []string{
	"resource_apply",
	"resource_scale",
	"resource_delete",
	"snapshot_save",
	"snapshot_schedule",
	"snapshot_prune",
	"token_create",
	"registry_apply",
}

// catalogDestructiveTools is every destructive-tier descriptor: absent
// unless --allow-destructive.
var catalogDestructiveTools = []string{
	"upgrade_apply",
	"rollback",
	"certs_rotate",
	"certs_rotate_ca",
	"certs_migrate_etcd_ca",
	"certs_migrate_kubelet_client_ca",
	"snapshot_restore",
	"snapshot_prune_restores",
	"snapshot_rejoin",
	"ha_repoint",
	"uninstall",
}

// testCatalog builds the catalog the way a server startup does: a version
// for version_get to report, the connector the Kubernetes-facing handlers
// talk through, the node connector on admin.conf the node-local handlers
// (token_list) talk through, and the write policy the generic Kubernetes
// write tools enforce.
func testCatalog() []Tool {
	return Catalog("v0-test", kube.NewConnector(kube.Options{}), kube.NewConnector(kube.Options{}), WritePolicy{})
}

// TestCatalogShipsEveryEngineTool pins the catalog's rows: every shipped
// tool, at the tier it ships at, no name twice, and every row complete —
// a description travels to the client, and a row without a handler-adder
// would silently ship a tool that never registers.
func TestCatalogShipsEveryEngineTool(t *testing.T) {
	cat := testCatalog()

	seen := map[string]bool{}
	var gotRead, gotWrite, gotDestructive []string
	for _, d := range cat {
		if d.Name == "" {
			t.Fatal("a descriptor carries no name")
		}
		if seen[d.Name] {
			t.Errorf("%s is in the catalog twice: the registrar refuses a second claim on a name, so the catalog must never carry one", d.Name)
		}
		seen[d.Name] = true
		if d.Description == "" {
			t.Errorf("%s: empty description: the description travels to the client in tools/list", d.Name)
		}
		if d.Add == nil {
			t.Errorf("%s: no handler-adder: the tool would never register", d.Name)
		}
		switch d.Tier {
		case TierRead:
			gotRead = append(gotRead, d.Name)
		case TierWrite:
			gotWrite = append(gotWrite, d.Name)
		case TierDestructive:
			gotDestructive = append(gotDestructive, d.Name)
		default:
			t.Errorf("%s: tier %v is none of read, write or destructive", d.Name, d.Tier)
		}
	}

	for _, tc := range []struct {
		tier      string
		got, want []string
	}{
		{"read", gotRead, catalogReadTools},
		{"write", gotWrite, catalogWriteTools},
		{"destructive", gotDestructive, catalogDestructiveTools},
	} {
		got, want := slices.Clone(tc.got), slices.Clone(tc.want)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("the %s tier's descriptors differ from the shipped tools.\n  in the catalog only: %v\n  missing: %v",
				tc.tier, difference(got, want), difference(want, got))
		}
	}
}

// TestCatalogToolsRegisterOverTheWire stands in for the registrar: it
// annotates each descriptor the way pkg/cli's ReadTool, WriteTool and
// DestructiveTool build a tool from a tier (pkg/cli cannot be imported here
// for them), adds every typed handler through Add, and lists the result over
// the real protocol. Every descriptor must register a working tool under its
// own name and description, annotated as its tier says. version_get is the
// one descriptor carrying a runtime parameter, so calling it also proves Add
// threads the version the catalog was built with into its handler.
func TestCatalogToolsRegisterOverTheWire(t *testing.T) {
	const version = "v0-test"
	cat := Catalog(version, kube.NewConnector(kube.Options{}), kube.NewConnector(kube.Options{}), WritePolicy{})

	srv := NewServer(Options{Version: version})
	type listed struct {
		description string
		tier        Tier
	}
	want := map[string]listed{}
	for _, d := range cat {
		if d.Add == nil {
			t.Errorf("%s: no handler-adder to add", d.Name)
			continue
		}
		d.Add(srv, catalogTool(d.Name, d.Description, d.Tier))
		want[d.Name] = listed{d.Description, d.Tier}
	}

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

	got := map[string]sdk.Tool{}
	for _, tool := range res.Tools {
		if _, dup := got[tool.Name]; dup {
			t.Errorf("%s is listed twice", tool.Name)
		}
		got[tool.Name] = *tool
	}
	for name, w := range want {
		tool, ok := got[name]
		if !ok {
			t.Errorf("%s never registered", name)
			continue
		}
		if tool.Description != w.description {
			t.Errorf("%s: description %q, want %q", name, tool.Description, w.description)
		}
		switch w.tier {
		case TierRead:
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("%s: registered at the read tier without a read-only hint: %+v", name, tool.Annotations)
			}
		case TierWrite:
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
				t.Errorf("%s: registered at the write tier with a read-only hint: a client may auto-approve it", name)
			}
		case TierDestructive:
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Errorf("%s: registered at the destructive tier without a destructive hint: a client would treat the call as safe", name)
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("tools/list advertised %d tools, want %d", len(got), len(want))
	}

	call, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "version_get"})
	if err != nil {
		t.Fatalf("call version_get: %v", err)
	}
	if call.IsError {
		t.Fatalf("version_get reported an error result: %+v", call.Content)
	}
	var out struct {
		Version string `json:"version"`
	}
	raw, err := json.Marshal(call.StructuredContent)
	if err != nil {
		t.Fatalf("marshal version_get's structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode version_get's output %s: %v", raw, err)
	}
	if out.Version != version {
		t.Errorf("version_get reported %q, want the version the catalog was built with (%s)", out.Version, version)
	}
}

// TestCatalogTokenCreateSaysTheNetworkFlagsGoOnAWorkerJoinLineOnly pins what
// a client reads about token_create's join flags: the description tools/list
// serves and the joinFlags field descriptions in its served output schema, not
// the Go comments beside them. --pod-cidr, --service-cidr and --cluster-domain
// go on a worker join line only: a control plane gets its network from the
// cluster it joins, and `stratum up` refuses those flags without --worker, so
// a client told nothing builds a control-plane join line that fails. --profile
// goes on every join line, so its description must not say worker-only.
func TestCatalogTokenCreateSaysTheNetworkFlagsGoOnAWorkerJoinLineOnly(t *testing.T) {
	const workerOnly = "worker join line only"

	var desc *Tool
	for _, d := range testCatalog() {
		if d.Name == "token_create" {
			desc = &d
			break
		}
	}
	if desc == nil {
		t.Fatal("the catalog carries no token_create descriptor")
	}
	cs := fencedSession(t, func(s *sdk.Server) {
		desc.Add(s, catalogTool(desc.Name, desc.Description, desc.Tier))
	})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var tool *sdk.Tool
	for _, tl := range res.Tools {
		if tl.Name == "token_create" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatalf("tools/list has no token_create tool: %+v", res.Tools)
	}

	for _, want := range []string{"--pod-cidr", "--service-cidr", "--cluster-domain", workerOnly} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("served token_create description does not say %q: %q", want, tool.Description)
		}
	}
	// Each substring above can appear while the description says the
	// opposite: the network flags on every join line and --profile on a
	// worker's only. So the phrase must bind the three flags to the worker
	// line, --profile must go on every join line, and no sentence that names
	// --profile may call it worker-only.
	for _, want := range []string{
		"--pod-cidr, --service-cidr and --cluster-domain on a " + workerOnly,
		"--profile goes on every join line",
	} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("served token_create description does not put each flag on its join line, missing %q: %q",
				want, tool.Description)
		}
	}
	for _, sentence := range strings.Split(tool.Description, ". ") {
		if strings.Contains(sentence, "--profile") && strings.Contains(sentence, workerOnly) {
			t.Errorf("served token_create description calls --profile worker-only, but it goes on every join line: %q",
				sentence)
		}
	}

	b, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal token_create's served output schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatalf("token_create's served output schema does not parse: %v:\n%s", err, b)
	}
	joinFlags, ok := schema.Properties["joinFlags"]
	if !ok {
		t.Fatalf("token_create's served output schema has no joinFlags property:\n%s", b)
	}
	for _, f := range []struct{ prop, flag string }{
		{"podCidr", "--pod-cidr"},
		{"serviceCidr", "--service-cidr"},
		{"clusterDomain", "--cluster-domain"},
	} {
		p, ok := joinFlags.Properties[f.prop]
		if !ok {
			t.Errorf("served joinFlags schema has no %s property:\n%s", f.prop, b)
			continue
		}
		for _, want := range []string{f.flag, workerOnly} {
			if !strings.Contains(p.Description, want) {
				t.Errorf("served joinFlags.%s description does not say %q: %q", f.prop, want, p.Description)
			}
		}
	}
	profile, ok := joinFlags.Properties["profile"]
	if !ok {
		t.Fatalf("served joinFlags schema has no profile property:\n%s", b)
	}
	if strings.Contains(profile.Description, workerOnly) {
		t.Errorf("served joinFlags.profile description says %q, but --profile goes on every join line: %q",
			workerOnly, profile.Description)
	}
}

// catalogTool builds the annotated tool the CLI registrar builds from a
// descriptor's tier, mirroring ReadTool, WriteTool and DestructiveTool.
// These annotations are what pkg/cli's checkTier holds a registration to.
func catalogTool(name, description string, tier Tier) *sdk.Tool {
	switch tier {
	case TierRead:
		return &sdk.Tool{
			Name:        name,
			Description: description,
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		}
	case TierWrite:
		no := false
		return &sdk.Tool{
			Name:        name,
			Description: description,
			Annotations: &sdk.ToolAnnotations{DestructiveHint: &no},
		}
	case TierDestructive:
		yes := true
		return &sdk.Tool{
			Name:        name,
			Description: description,
			Annotations: &sdk.ToolAnnotations{DestructiveHint: &yes},
		}
	}
	panic(fmt.Sprintf("catalogTool: tier %d is none of read, write or destructive", tier))
}

func difference(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
