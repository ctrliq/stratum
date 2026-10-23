package cli

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// Surface pinning.

// shippedReadTools is every tool advertised with no flags.
var shippedReadTools = []string{
	"certs_check",
	"cluster_status",
	"context_list",
	"event_list",
	"namespace_list",
	"node_list",
	"pod_get",
	"pod_list",
	"pod_logs",
	"registry_check",
	"resource_get",
	"resource_list",
	"snapshot_list",
	"snapshot_restore_copies",
	"token_list",
	"uninstall_preview",
	"upgrade_plan",
	"version_get",
}

// shippedWriteTools are the tools --allow-write adds.
var shippedWriteTools = []string{
	"registry_apply",
	"resource_apply",
	"resource_delete",
	"resource_scale",
	"snapshot_prune",
	"snapshot_save",
	"snapshot_schedule",
	"token_create",
}

// shippedDestructiveTools are the tools --allow-destructive adds.
var shippedDestructiveTools = []string{
	"certs_migrate_etcd_ca",
	"certs_migrate_kubelet_client_ca",
	"certs_rotate",
	"certs_rotate_ca",
	"ha_repoint",
	"rollback",
	"snapshot_prune_restores",
	"snapshot_rejoin",
	"snapshot_restore",
	"uninstall",
	"upgrade_apply",
}

// shippedSurface builds a server with no contributed tool sets — the
// surface a `stratum mcp` actually serves.
func shippedSurface(t *testing.T, allowWrite, allowDestructive bool) []string {
	t.Helper()

	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
	registerAll(newRegistrar(srv, allowWrite, allowDestructive),
		rootConfig{displayVersion: "v0-test"}, // no toolSets: the engine's own surface
		kube.NewConnector(kube.Options{}), testConnector())

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
	slices.Sort(names)
	return names
}

func sortedUnion(lists ...[]string) []string {
	out := slices.Concat(lists...)
	slices.Sort(out)
	return out
}

func TestShippedSurfaceIsExactlyPinned(t *testing.T) {
	for _, tc := range []struct {
		name               string
		write, destructive bool
		want               []string
	}{
		{"no flags", false, false, sortedUnion(shippedReadTools)},
		{"--allow-write", true, false, sortedUnion(shippedReadTools, shippedWriteTools)},
		{"--allow-destructive", false, true, sortedUnion(shippedReadTools, shippedDestructiveTools)},
		{"both", true, true, sortedUnion(shippedReadTools, shippedWriteTools, shippedDestructiveTools)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shippedSurface(t, tc.write, tc.destructive)
			if !slices.Equal(got, tc.want) {
				t.Errorf("advertised surface differs.\n  added:   %v\n  missing: %v",
					difference(got, tc.want), difference(tc.want, got))
			}
			if len(got) != len(tc.want) {
				t.Errorf("count = %d, want %d", len(got), len(tc.want))
			}
		})
	}
}

// The tier-count table in the cluster-admin MCP page is easy to leave behind
// when tools are added or removed, because nothing else ties its numbers to
// the code. This pins each count to the pinned lists (already proven equal
// to the live surface), so a future tool change fails here until the doc is
// updated too.
func TestDocsTierTableMatchesSurface(t *testing.T) {
	data, err := os.ReadFile("../../docs/docs/cluster-admin/mcp.md")
	if err != nil {
		t.Fatalf("read docs tier table: %v", err)
	}
	text := string(data)
	lines := strings.Split(text, "\n")

	for _, tc := range []struct {
		flag string
		re   *regexp.Regexp
		want int
	}{
		{"no flags", regexp.MustCompile(`^\| no flags \| (\d+) \|`), len(shippedReadTools)},
		{"--allow-write", regexp.MustCompile("^\\| `--allow-write` \\| \\+(\\d+) \\|"), len(shippedWriteTools)},
		{"--allow-destructive", regexp.MustCompile("^\\| `--allow-destructive` \\| \\+(\\d+) \\|"), len(shippedDestructiveTools)},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			for i, line := range lines {
				m := tc.re.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				// Atoi's error needs no branch: it yields 0, which the count
				// comparison below rejects as loudly as a wrong number.
				got, _ := strconv.Atoi(m[1])
				if got != tc.want {
					t.Errorf("docs/docs/cluster-admin/mcp.md:%d says the %s tier has %d tools, but the shipped surface has %d",
						i+1, tc.flag, got, tc.want)
				}
				return
			}
			t.Fatalf("the %s tier-count row was not found in docs/docs/cluster-admin/mcp.md; if the table was reworded, update this test", tc.flag)
		})
	}
}

// The counts, stated separately and in words, because a count is the thing
// a reviewer checks against the documented tier table without reading a list.
func TestShippedSurfaceCounts(t *testing.T) {
	for _, tc := range []struct {
		name               string
		write, destructive bool
		want               int
	}{
		{"read tier advertises 18 tools", false, false, 18},
		{"--allow-write advertises 26", true, false, 26},
		{"--allow-destructive advertises 29", false, true, 29},
		{"both advertise 37", true, true, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(shippedSurface(t, tc.write, tc.destructive)); got != tc.want {
				t.Errorf("got %d tools, want %d", got, tc.want)
			}
		})
	}
}

// The two statements of the surface must agree.
func TestPinnedListsAgreeWithTheTierLists(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pinned, tiers []string
	}{
		{"read", shippedReadTools, engineReadTools},
		{"write", shippedWriteTools, engineWriteTools},
		{"destructive", shippedDestructiveTools, engineDestructiveTools},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinned, tiers := slices.Clone(tc.pinned), slices.Clone(tc.tiers)
			slices.Sort(pinned)
			slices.Sort(tiers)
			if !slices.Equal(pinned, tiers) {
				t.Errorf("the two statements of the %s surface disagree.\n  only in the pin:   %v\n  only in the tiers: %v",
					tc.name, difference(pinned, tiers), difference(tiers, pinned))
			}
		})
	}
}

// No tool may appear at more than one tier.
func TestNoToolAppearsAtTwoTiers(t *testing.T) {
	seen := map[string]string{}
	for tier, list := range map[string][]string{
		"read":        shippedReadTools,
		"write":       shippedWriteTools,
		"destructive": shippedDestructiveTools,
	} {
		for _, name := range list {
			if other, dup := seen[name]; dup {
				t.Errorf("%s is listed at both the %s and %s tiers", name, other, tier)
			}
			seen[name] = tier
		}
	}
}

// Naming is noun_verb throughout (snapshot_list, certs_check), which groups
// tools by subject in a client's alphabetical list.
func TestToolNamesFollowTheConvention(t *testing.T) {
	all := sortedUnion(shippedReadTools, shippedWriteTools, shippedDestructiveTools)
	for _, name := range all {
		t.Run(name, func(t *testing.T) {
			if name != strings.ToLower(name) {
				t.Errorf("not lowercase")
			}
			if strings.Contains(name, "-") || strings.Contains(name, " ") {
				t.Errorf("use underscores, not dashes or spaces")
			}
			// Two whole-subject verbs have no noun to pair with: the subject IS the
			// node.
			singleWordByDesign := []string{"rollback", "uninstall"}
			if !strings.Contains(name, "_") && !slices.Contains(singleWordByDesign, name) {
				t.Errorf("expected noun_verb, got a single word")
			}
			if strings.HasPrefix(name, "get_") || strings.HasPrefix(name, "list_") ||
				strings.HasPrefix(name, "create_") || strings.HasPrefix(name, "delete_") {
				t.Errorf("verb_noun; the convention here is noun_verb so tools group by subject")
			}
		})
	}
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
