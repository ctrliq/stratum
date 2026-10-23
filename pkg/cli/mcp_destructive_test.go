package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// Protocol-boundary tests for the Stratum destructive toolset.

// tierSession returns a live session at the given tiers.
func tierSession(t *testing.T, allowWrite, allowDestructive bool) *sdk.ClientSession {
	t.Helper()

	conn := kube.NewConnector(kube.Options{})
	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
	registerAll(newRegistrar(srv, allowWrite, allowDestructive),
		rootConfig{displayVersion: "v0-test"}, conn, testConnector())

	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func advertised(t *testing.T, cs *sdk.ClientSession) map[string]*sdk.Tool {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

// Asserted per tool, at both lower tiers —
// --allow-write must NOT be enough to reach any of these.
func TestDestructiveToolsAreAbsentBelowTheirTier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write bool
	}{
		{"no flags", false},
		{"--allow-write only", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := tierSession(t, tc.write, false)
			tools := advertised(t, cs)

			for _, name := range engineDestructiveTools {
				if _, present := tools[name]; present {
					t.Errorf("%s is advertised with %s; it must not be registered at all", name, tc.name)
				}
				// Absence is the control.
				if _, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name}); err == nil {
					t.Errorf("calling unregistered %s succeeded", name)
				}
			}
		})
	}
}

func TestDestructiveToolsAppearWithTheirFlag(t *testing.T) {
	tools := advertised(t, tierSession(t, false, true))
	for _, name := range engineDestructiveTools {
		if _, present := tools[name]; !present {
			t.Errorf("%s is missing with --allow-destructive", name)
		}
	}
}

// DestructiveHint: true on every one.
func TestDestructiveToolsCarryTheHint(t *testing.T) {
	tools := advertised(t, tierSession(t, true, true))
	for _, name := range engineDestructiveTools {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", name)
			continue
		}
		if tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is annotated read-only", name)
		}
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("%s does not carry DestructiveHint: a client cannot warn on it", name)
		}
	}
}

// Each tool's description states the blast radius in the first sentence,
// because that description is what a model reads before deciding.
func TestDestructiveDescriptionsLeadWithBlastRadius(t *testing.T) {
	tools := advertised(t, tierSession(t, true, true))

	for _, name := range engineDestructiveTools {
		t.Run(name, func(t *testing.T) {
			tool := tools[name]
			if tool == nil {
				t.Fatalf("%s missing", name)
			}
			desc := tool.Description
			if desc == "" {
				t.Fatal("no description")
			}

			first, _, _ := strings.Cut(desc, ".")
			if first == "" {
				t.Fatalf("description has no first sentence: %q", desc)
			}

			// The description must OPEN with the consequence, in capitals.
			words := strings.Fields(first)
			lead := strings.TrimFunc(words[0], func(r rune) bool { return !unicode.IsLetter(r) })
			if len(lead) < 2 || lead != strings.ToUpper(lead) {
				t.Errorf("the description does not open with the consequence in capitals; it opens with %q:\n  %q",
					words[0], first)
			}

			// And the opening word must name a consequence, not merely shout.
			stems := []string{
				"DRAIN", "REVERT", "RESTART", "REPLACE", "WIPE",
				"REWRITE", "REPOINT", "REMOVE", "PERMANENTLY",
			}
			if !slices.ContainsFunc(stems, func(stem string) bool { return strings.HasPrefix(lead, stem) }) {
				t.Errorf("the opening word %q does not name what the tool does to the cluster:\n  %q", lead, first)
			}
		})
	}
}

// Tools that must never be registered, each with the reason it is refused.
func TestForbiddenToolsAreNotRegistered(t *testing.T) {
	tools := advertised(t, tierSession(t, true, true))

	for name, why := range map[string]string{
		"up":                 "stands up host state and has no confirmation gate at all",
		"etcdctl":            "raw datastore access, including member remove",
		"operator":           "a daemon, not request/response",
		"dash":               "a daemon, not request/response",
		"cluster_up":         "same as up",
		"etcd_member_remove": "removing an etcd member is how a cluster loses quorum",
	} {
		if _, present := tools[name]; present {
			t.Errorf("%s is registered, but must never be: %s", name, why)
		}
	}
}

// Arguments whose absence would make a destructive call ambiguous are
// required rather than defaulted.
func TestDestructiveToolsRequireTheirDecisiveArgument(t *testing.T) {
	cs := tierSession(t, true, true)

	for _, tc := range []struct {
		tool, missing string
	}{
		{"upgrade_apply", "scope"},
		{"certs_rotate_ca", "phase"},
		{"certs_migrate_etcd_ca", "phase"},
		{"snapshot_restore", "snapshot"},
		{"snapshot_prune_restores", "copies"},
		{"snapshot_rejoin", "server"},
		{"ha_repoint", "vip"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: tc.tool})
			if err != nil {
				return // the schema rejected it before the handler; equally good
			}
			if !res.IsError {
				t.Fatalf("%s ran with no %s", tc.tool, tc.missing)
			}
			var msg strings.Builder
			for _, c := range res.Content {
				if tc2, ok := c.(*sdk.TextContent); ok {
					msg.WriteString(tc2.Text)
				}
			}
			if !strings.Contains(msg.String(), tc.missing) {
				t.Errorf("the error does not name the missing argument %q: %s", tc.missing, msg.String())
			}
		})
	}
}

// TestUninstallTakesNoConfirmationArgument.
//
// uninstall.Run(false) prints a preview and removes nothing, so exposing the
// caller's own force flag made the tool a no-op that still reported "Stratum
// is gone from this node". Starting the server with --allow-destructive is
// the confirmation, and uninstall_preview is the dry run, so the tool takes
// no arguments at all.
func TestUninstallTakesNoConfirmationArgument(t *testing.T) {
	tool, ok := advertised(t, tierSession(t, true, true))["uninstall"]
	if !ok {
		t.Fatal("uninstall is not advertised with both flags")
	}
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	for _, banned := range []string{"force", "yes", "confirm"} {
		if strings.Contains(string(schema), `"`+banned+`"`) {
			t.Errorf("uninstall advertises a %q argument; the tier flag is the confirmation: %s", banned, schema)
		}
	}
}

// TestDestructiveToolInputsArePinned.
//
// TestShippedSurfaceIsExactlyPinned (mcp_surface_test.go) fails when a tool is
// added or removed, but checks names only: it cannot see an input renamed or
// dropped, which breaks every AI client that sends it. This pin holds each
// destructive tool's input schema to the exact property set AI clients already
// code against, and which of them are required.
//
// certs_rotate_ca's set includes the cluster-rotation inputs
// restart_workloads, skip_workload_check and skip_token_check (CertsRotateCAIn).
// The pin asserts the full current set, so a later rename or removal of any
// input fails here.
func TestDestructiveToolInputsArePinned(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       []string
		required []string
	}{
		{"upgrade_apply",
			[]string{"scope", "target", "from", "workers_max_unavailable", "soak_seconds", "skip_snapshot"},
			[]string{"scope"}},
		{"rollback", []string{"soak_seconds", "etcd_snapshot"}, nil},
		{"certs_rotate", []string{"if_expiring_days"}, nil},
		{"certs_rotate_ca",
			[]string{"phase", "cluster", "force", "restart_workloads", "skip_workload_check", "skip_token_check"},
			nil},
		{"certs_migrate_etcd_ca", []string{"phase", "cluster", "resume", "force"}, []string{"phase"}},
		{"certs_migrate_kubelet_client_ca", []string{"cluster", "rollback", "phase", "force"}, nil},
		{"snapshot_restore", []string{"snapshot"}, []string{"snapshot"}},
		{"snapshot_prune_restores", []string{"copies"}, []string{"copies"}},
		{"snapshot_rejoin", []string{"server"}, []string{"server"}},
		{"ha_repoint", []string{"vip", "cilium"}, []string{"vip"}},
		{"uninstall", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := advertised(t, tierSession(t, true, true))[tc.name]
			if !ok {
				t.Fatalf("%s is not advertised with both mutating tiers", tc.name)
			}
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal input schema: %v", err)
			}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatalf("decode input schema %s: %v", raw, err)
			}

			var got []string
			for prop := range schema.Properties {
				got = append(got, prop)
			}
			slices.Sort(got)
			want := slices.Clone(tc.in)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s's input schema drifted.\n  added:   %v\n  removed: %v\n  schema: %s",
					tc.name, difference(got, want), difference(want, got), raw)
			}

			gotRequired := slices.Clone(schema.Required)
			slices.Sort(gotRequired)
			wantRequired := slices.Clone(tc.required)
			slices.Sort(wantRequired)
			if !slices.Equal(gotRequired, wantRequired) {
				t.Errorf("%s's required inputs drifted.\n  added:   %v\n  removed: %v",
					tc.name, difference(gotRequired, wantRequired), difference(wantRequired, gotRequired))
			}
		})
	}
}
