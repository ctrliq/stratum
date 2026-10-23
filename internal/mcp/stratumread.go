package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/registry"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/uninstall"
	"github.com/ctrliq/stratum/internal/upgrade"
	"github.com/ctrliq/stratum/internal/version"
)

// The Stratum read toolset — the half a generic Kubernetes MCP
// server cannot provide, because it knows nothing about the distribution
// underneath the cluster.

// errNotStratumHost is what every tool here reports when the machine is not
// a Stratum node.
var errNotStratumHost = errors.New(
	"this machine is not a Stratum node: " + version.ConfDir + " and " + version.DataDir +
		" are both absent. These tools read the distribution's own state from the host, " +
		"so they only answer on a machine running Stratum. The generic Kubernetes tools " +
		"(pod_list, resource_get and the rest) work against any cluster and are unaffected")

// stratumHostDirs are the paths whose presence means "this is a Stratum
// node".
var stratumHostDirs = []string{version.ConfDir, version.DataDir}

// SetStratumHostDirsForTest makes the host check look for dirs instead, and returns a func that restores the previous ones.
func SetStratumHostDirsForTest(dirs []string) (restore func()) {
	prev := stratumHostDirs
	stratumHostDirs = dirs
	return func() { stratumHostDirs = prev }
}

// onStratumHost reports whether this machine looks like a Stratum node.
func onStratumHost() bool {
	for _, dir := range stratumHostDirs {
		if _, err := os.Stat(dir); err == nil {
			return true
		}
	}
	return false
}

func requireStratumHost() error {
	if !onStratumHost() {
		return errNotStratumHost
	}
	return nil
}

// ------------------------------------------------------------ cluster_status

// ClusterStatusIn is cluster_status's (empty) input.
type ClusterStatusIn struct{}

// ProcRow is one supervised process.
type ProcRow struct {
	Name    string `json:"name"`
	PID     int    `json:"pid,omitempty" jsonschema:"process id, absent when not running"`
	Running bool   `json:"running"`
	Wanted  bool   `json:"wanted" jsonschema:"whether the supervisor intends this process to be running"`
	Foreign bool   `json:"foreign,omitempty" jsonschema:"the process's socket is served by another process, so a running entry is not this component"`
}

// ClusterStatusOut is cluster_status's result.
type ClusterStatusOut struct {
	Supervisor      string    `json:"supervisor" jsonschema:"running or unreachable"`
	Socket          string    `json:"socket"`
	Procs           []ProcRow `json:"procs"`
	Healthy         bool      `json:"healthy" jsonschema:"true when every wanted process is running"`
	SupervisorStale bool      `json:"supervisorStale,omitempty" jsonschema:"the supervisor runs a binary other than the installed stratum, so the processes it starts get the previous build's arguments and paths until it restarts"`
	Note            string    `json:"note,omitempty"`
}

// summarizeProcs turns the supervisor's roster into rows, a health verdict and
// the note saying why it is not healthy. A component is unhealthy when it is
// wanted but not running, or when another process serves its socket (Foreign).
func summarizeProcs(procs []supervisor.ProcStatus) (rows []ProcRow, healthy bool, note string) {
	healthy = true
	var down, foreign []string
	for _, p := range procs {
		rows = append(rows, ProcRow{Name: p.Name, PID: p.Pid, Running: p.Running, Wanted: p.Wanted, Foreign: p.Foreign})
		switch {
		case p.Foreign:
			foreign = append(foreign, p.Name)
		case p.Wanted && !p.Running:
			down = append(down, p.Name)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	sort.Strings(down)
	sort.Strings(foreign)
	var notes []string
	if len(down) > 0 {
		notes = append(notes, "Wanted but not running: "+strings.Join(down, ", "))
	}
	if len(foreign) > 0 {
		notes = append(notes, "Socket served by another process (not the one stratum installed): "+strings.Join(foreign, ", "))
	}
	return rows, len(notes) == 0, strings.Join(notes, ". ")
}

// runningSupervisor is comps.Supervisor, indirected for tests.
var runningSupervisor = comps.Supervisor

// noteStaleSupervisor marks out when the supervisor runs a binary other than
// the installed stratum. A supervisor that cannot be read adds nothing: the
// process table already says whether it answers.
func noteStaleSupervisor(out *ClusterStatusOut, read func() (comps.RunningSupervisor, error)) {
	cur, err := read()
	if err != nil || !cur.Stale() {
		return
	}
	out.SupervisorStale = true
	stale := fmt.Sprintf("The supervisor (pid %d) runs %s, not the installed %s, until it restarts",
		cur.PID, cur.Exe, version.StratumInstallPath)
	if out.Note == "" {
		out.Note = stale
		return
	}
	out.Note += ". " + stale
}

// ClusterStatusHandler reports the supervised process table.
func ClusterStatusHandler() sdk.ToolHandlerFor[ClusterStatusIn, ClusterStatusOut] {
	return func(context.Context, *sdk.CallToolRequest, ClusterStatusIn) (*sdk.CallToolResult, ClusterStatusOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, ClusterStatusOut{}, err
		}

		out := ClusterStatusOut{Socket: version.ControlSocket, Supervisor: "running"}

		procs, err := supervisor.Dial(version.ControlSocket, "status", "")
		if err != nil {
			// A socket that will not answer is a real answer: the node is
			// installed but its supervisor is down, which is a different
			// and more alarming state than "not a Stratum node".
			out.Supervisor = "unreachable"
			out.Note = fmt.Sprintf("Could not reach the supervisor at %s (%v). "+
				"Stratum is installed here but its supervisor is not answering — "+
				"the control socket is root-only, so check privileges before concluding it is down.",
				version.ControlSocket, err)
			return nil, out, nil
		}

		out.Procs, out.Healthy, out.Note = summarizeProcs(procs)
		noteStaleSupervisor(&out, runningSupervisor)
		return nil, out, nil
	}
}

// -------------------------------------------------------------- certs_check

// CertsCheckIn is certs_check's input.
//
// WarnDays' description states the renewal window's value. A struct tag must
// be a string literal, so it cannot cite version.CertRenewWindowDays; the
// number is written here and TestCertsCheckSchemaStatesTheRenewalWindow
// fails the build when the tag and the constant drift apart.
type CertsCheckIn struct {
	WarnDays int `json:"warn_days,omitempty" jsonschema:"how many days ahead counts as expiring; defaults to 45"`
}

// CertRow is one certificate's expiry.
type CertRow struct {
	Name           string `json:"name"`
	NotAfter       string `json:"notAfter" jsonschema:"RFC 3339 expiry"`
	DaysLeft       int    `json:"daysLeft"`
	IsCA           bool   `json:"isCA"`
	Expiring       bool   `json:"expiring" jsonschema:"within the warning window"`
	Rotatable      bool   `json:"rotatable" jsonschema:"a leaf certs_rotate can renew; false for CAs, which need certs_rotate_ca, and for the kubelet client cert"`
	Kubelet        bool   `json:"kubeletClient" jsonschema:"a worker's kubelet client cert, which the kubelet renews itself and certs_rotate cannot"`
	RenewalOverdue bool   `json:"renewalOverdue" jsonschema:"the kubelet client cert is past its latest expected automatic renewal point"`
}

// CertsCheckOut is certs_check's result.
type CertsCheckOut struct {
	Items    []CertRow `json:"items"`
	Expiring int       `json:"expiring" jsonschema:"how many are inside the warning window"`
	Expired  int       `json:"expired"`
	Note     string    `json:"note,omitempty"`
}

// CertsCheckHandler reports certificate expiry on this node.
func CertsCheckHandler() sdk.ToolHandlerFor[CertsCheckIn, CertsCheckOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in CertsCheckIn) (*sdk.CallToolResult, CertsCheckOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, CertsCheckOut{}, err
		}

		// WarnDays 0 means "use the package default"
		// (version.CertRenewWindowDays), which is exactly what an omitted
		// argument should do, so it passes straight through.
		statuses, err := certs.Check(certs.Options{WarnDays: in.WarnDays, Quiet: true})
		if err != nil {
			return nil, CertsCheckOut{}, fmt.Errorf("check certificates: %w", err)
		}
		return nil, summarizeCerts(statuses), nil
	}
}

func summarizeCerts(statuses []certs.CertStatus) CertsCheckOut {
	out := CertsCheckOut{Items: make([]CertRow, 0, len(statuses))}
	var managedDue, caDue, kubeletWindow, kubeletOverdue, kubeletExpired bool
	for _, s := range statuses {
		out.Items = append(out.Items, CertRow{
			Name:           s.Name,
			NotAfter:       s.NotAfter.UTC().Format(time.RFC3339),
			DaysLeft:       s.DaysLeft,
			IsCA:           s.IsCA,
			Expiring:       s.Expiring,
			Rotatable:      s.Rotatable,
			Kubelet:        s.Kubelet,
			RenewalOverdue: s.RenewalOverdue,
		})
		due := s.Expiring || s.DaysLeft < 0
		managedDue = managedDue || (due && s.Rotatable)
		caDue = caDue || (due && s.IsCA)
		kubeletWindow = kubeletWindow || (due && s.Kubelet)
		kubeletOverdue = kubeletOverdue || (s.Kubelet && s.RenewalOverdue)
		kubeletExpired = kubeletExpired || (s.Kubelet && s.DaysLeft < 0)
		if s.DaysLeft < 0 {
			out.Expired++
		} else if s.Expiring {
			out.Expiring++
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].DaysLeft < out.Items[j].DaysLeft })

	switch {
	case out.Expired > 0:
		out.Note = fmt.Sprintf("%d certificate(s) have already expired. The cluster may be partly or wholly unavailable.", out.Expired)
	case out.Expiring > 0:
		out.Note = fmt.Sprintf("%d certificate(s) are inside the warning window.", out.Expiring)
	}
	appendNote := func(msg string) {
		if out.Note != "" {
			out.Note += " "
		}
		out.Note += msg
	}
	if managedDue {
		appendNote("Stratum-managed leaves can be renewed with certs_rotate.")
	}
	if caDue {
		appendNote("CAs need certs_rotate_ca and a node roll.")
	}
	if kubeletWindow || kubeletOverdue {
		appendNote("The kubelet-client certificate is renewed by the kubelet itself, not by certs_rotate.")
		switch {
		case kubeletExpired:
			appendNote("It has expired; remove and rejoin the worker with a new token.")
		case kubeletOverdue:
			appendNote("It is past its latest expected renewal point; check the kubelet's log and its node-client CSRs.")
		}
	}
	return out
}

// ----------------------------------------------------------- snapshot_list

// SnapshotListIn is snapshot_list's input.
type SnapshotListIn struct {
	Continue string `json:"continue_token,omitempty" jsonschema:"continuation from the preceding page; restart if the snapshot inventory changes"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum snapshots to return, newest first; defaults to 100, capped at 500"`
}

// SnapshotRow is one etcd snapshot.
type SnapshotRow struct {
	Name     string `json:"name"`
	Location string `json:"location" jsonschema:"local or s3"`
	Size     int64  `json:"size"`
	Modified string `json:"modified" jsonschema:"RFC 3339"`
}

// SnapshotListOut is snapshot_list's result.
type SnapshotListOut struct {
	ListMeta
	Items []SnapshotRow `json:"items"`
	Dir   string        `json:"dir" jsonschema:"the local snapshot directory"`
}

// SnapshotListHandler lists etcd snapshots, local and in S3.
func SnapshotListHandler() sdk.ToolHandlerFor[SnapshotListIn, SnapshotListOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SnapshotListIn) (*sdk.CallToolResult, SnapshotListOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, SnapshotListOut{}, err
		}

		limit := ListOptions{Limit: in.Limit}.limit()
		cfg, err := loadSnapshotConfig()
		if err != nil {
			return nil, SnapshotListOut{}, fmt.Errorf("read snapshot config: %w", err)
		}

		// The listing names the directory it listed: the one the resolved
		// settings carry, which snapshot_save writes and snapshot_restore
		// resolves a bare name in.
		items, err := snapshotList(cfg.Dir, cfg.S3Target())
		out := SnapshotListOut{Dir: cfg.Dir}

		// A local, not out.Note: SnapshotListOut embeds ListMeta and
		// declares no Note, so the ListMeta assignment below overwrites it.
		var partial string
		if err != nil {
			// List returns local results alongside an S3 error.
			if len(items) == 0 {
				return nil, SnapshotListOut{}, fmt.Errorf("list snapshots: %w", err)
			}
			partial = fmt.Sprintf("Local snapshots only: the remote store could not be listed (%v).", err)
		}

		items, cont, err := snapshotPage(items, limit, in.Continue)
		if err != nil {
			return nil, SnapshotListOut{}, err
		}
		for _, it := range items {
			out.Items = append(out.Items, SnapshotRow{
				Name:     it.Name,
				Location: it.Location,
				Size:     it.Size,
				Modified: it.Modified.UTC().Format(time.RFC3339),
			})
		}

		out.ListMeta = newListMeta(len(out.Items), limit, cont)
		var notes []string
		if partial != "" {
			notes = append(notes, partial)
		}
		if out.Note != "" {
			notes = append(notes, out.Note)
		}
		out.Note = strings.Join(notes, " ")
		return nil, out, nil
	}
}

// snapshotPage ties the cursor to the sorted inventory: changes between pages
// require a restart rather than silently skipping or duplicating snapshots.
func snapshotPage(items []etcdsnap.SnapItem, limit int, cursor string) ([]etcdsnap.SnapItem, string, error) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].Modified.Equal(items[j].Modified) {
			return items[i].Modified.After(items[j].Modified)
		}
		if items[i].Location != items[j].Location {
			return items[i].Location < items[j].Location
		}
		return items[i].Name < items[j].Name
	})
	return fingerprintPage(items, items, limit, cursor, "snapshot_list")
}

// -------------------------------------------------------------- token_list

// TokenListIn is token_list's paging input (the cluster is always local).
type TokenListIn struct {
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum tokens to return; defaults to 100, capped at 500"`
	Continue string `json:"continue_token,omitempty" jsonschema:"continuation from the preceding page"`
}

// TokenRow is one bootstrap token.
type TokenRow struct {
	ID        string `json:"id" jsonschema:"the token id; the secret half is deliberately never returned"`
	Expires   string `json:"expires,omitempty" jsonschema:"RFC 3339, absent when the token does not expire"`
	Malformed bool   `json:"malformed" jsonschema:"the stored expiry is unparseable; the controller treats such a token as invalid, not as non-expiring"`
}

// TokenListOut is token_list's result.
type TokenListOut struct {
	ListMeta
	Items []TokenRow `json:"items"`
}

// TokenListHandler lists bootstrap tokens. Tokens are Secrets in
// kube-system, which the server's credential should not be able to read, so
// the tool returns only ids and expiry. The server supplies its shared node
// connector, pinned to the node's own admin credential — super-admin.conf
// where the node has one, admin.conf until its first new-release up writes it
// — independently of the configurable connector, and invalidates both after a
// rotation or repoint.
func TokenListHandler(conn *kube.Connector) sdk.ToolHandlerFor[TokenListIn, TokenListOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in TokenListIn) (*sdk.CallToolResult, TokenListOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, TokenListOut{}, err
		}

		cl, err := cluster(conn, "")
		if err != nil {
			return nil, TokenListOut{}, err
		}
		limit := ListOptions{Limit: in.Limit}.limit()
		infos, cont, err := token.ListPage(ctx, cl.Typed, int64(limit), in.Continue)
		if err != nil {
			return nil, TokenListOut{}, fmt.Errorf("list bootstrap tokens: %w", err)
		}

		out := TokenListOut{Items: make([]TokenRow, 0, len(infos))}
		var malformed int
		for _, i := range infos {
			row := TokenRow{ID: i.ID, Malformed: i.Malformed}
			if i.Expires != nil {
				row.Expires = i.Expires.UTC().Format(time.RFC3339)
			}
			if i.Malformed {
				malformed++
			}
			out.Items = append(out.Items, row)
		}
		sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })

		out.ListMeta = newListMeta(len(out.Items), limit, cont)
		if malformed > 0 {
			out.ListMeta.Note += fmt.Sprintf(" %d token(s) carry an unparseable expiry. The bootstrap-token "+
				"controller treats those as invalid, so they will not let a node join.", malformed)
		}
		return nil, out, nil
	}
}

// ---------------------------------------------------------- registry_check

// RegistryCheckIn is registry_check's (empty) input.
type RegistryCheckIn struct{}

// MirrorRow is one registry mirror.
type MirrorRow struct {
	Registry  string   `json:"registry" jsonschema:"the registry being mirrored, e.g. docker.io"`
	Endpoints []string `json:"endpoints"`
	Rewrites  int      `json:"rewrites,omitempty" jsonschema:"how many path-rewrite rules this mirror carries"`
}

// RegistryAuthRow reports THAT a registry has credentials, never what they
// are.
type RegistryAuthRow struct {
	Registry      string `json:"registry"`
	HasAuth       bool   `json:"hasAuth" jsonschema:"credentials are configured; the values are never returned"`
	InsecureHTTPS bool   `json:"insecureSkipVerify,omitempty" jsonschema:"TLS verification is disabled for this registry"`
	HasCA         bool   `json:"hasCA,omitempty" jsonschema:"a custom CA file is configured"`
}

// RegistryCheckOut is registry_check's result.
type RegistryCheckOut struct {
	Path       string            `json:"path"`
	Configured bool              `json:"configured" jsonschema:"false when registries.yaml is absent or configures nothing, which is the common case"`
	Mirrors    []MirrorRow       `json:"mirrors,omitempty"`
	Configs    []RegistryAuthRow `json:"configs,omitempty"`
	Note       string            `json:"note,omitempty"`
}

// RegistryCheckHandler reports the node's registry configuration.
func RegistryCheckHandler() sdk.ToolHandlerFor[RegistryCheckIn, RegistryCheckOut] {
	return func(context.Context, *sdk.CallToolRequest, RegistryCheckIn) (*sdk.CallToolResult, RegistryCheckOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, RegistryCheckOut{}, err
		}

		out := RegistryCheckOut{Path: version.RegistriesConfig}
		cfg, err := registry.Load(version.RegistriesConfig)
		if err != nil {
			return nil, RegistryCheckOut{}, fmt.Errorf("load %s: %w", version.RegistriesConfig, err)
		}
		if cfg == nil || len(registry.Registries(cfg)) == 0 {
			out.Note = registry.NothingConfigured + ". " + registry.DefaultIsNotAFault
			if registry.WildcardCredentialsDropped(cfg) {
				out.Note = registry.WildcardCredentialsIgnored + ". " + out.Note
			}
			return nil, out, nil
		}
		out.Configured = true

		for name, m := range cfg.Mirrors {
			out.Mirrors = append(out.Mirrors, MirrorRow{
				Registry: name, Endpoints: m.Endpoint, Rewrites: len(m.Rewrite),
			})
		}
		sort.Slice(out.Mirrors, func(i, j int) bool { return out.Mirrors[i].Registry < out.Mirrors[j].Registry })

		var insecure, notes []string
		if registry.WildcardCredentialsDropped(cfg) {
			notes = append(notes, registry.WildcardCredentialsIgnored+".")
		}
		for name, c := range cfg.Configs {
			c, _ = registry.Effective(name, c)
			row := RegistryAuthRow{
				Registry: name,
				// Presence only.
				HasAuth:       c.Auth.Username != "" || c.Auth.Password != "" || c.Auth.Auth != "" || c.Auth.IdentityToken != "",
				InsecureHTTPS: c.TLS.InsecureSkipVerify,
				HasCA:         c.TLS.CAFile != "",
			}
			if c.TLS.InsecureSkipVerify {
				insecure = append(insecure, name)
			}
			out.Configs = append(out.Configs, row)
		}
		sort.Slice(out.Configs, func(i, j int) bool { return out.Configs[i].Registry < out.Configs[j].Registry })

		if len(insecure) > 0 {
			sort.Strings(insecure)
			notes = append(notes, "TLS verification is disabled for: "+strings.Join(insecure, ", ")+
				". Images from those registries are not authenticated in transit.")
		}
		out.Note = strings.Join(notes, " ")
		return nil, out, nil
	}
}

// ----------------------------------------------------------- upgrade_plan

// UpgradePlanIn is upgrade_plan's input. There is no context argument, on
// purpose: the plan previews the one cluster upgrade_apply rolls.
type UpgradePlanIn struct {
	Target                string `json:"target" jsonschema:"the Kubernetes version to upgrade to, as vMAJOR.MINOR.PATCH"`
	WorkersMaxUnavailable int    `json:"workers_max_unavailable,omitempty" jsonschema:"how many workers may be upgraded at once; defaults to 1"`
}

// UpgradeStep is one step of the rolling plan.
type UpgradeStep struct {
	Order   int      `json:"order"`
	Nodes   []string `json:"nodes" jsonschema:"the nodes upgraded together in this step"`
	Role    string   `json:"role" jsonschema:"control-plane or worker"`
	Serial  bool     `json:"serial" jsonschema:"true when this step must not run alongside another"`
	Comment string   `json:"comment,omitempty"`
}

// UpgradePlanOut is upgrade_plan's result.
type UpgradePlanOut struct {
	Target    string        `json:"target"`
	Allowed   bool          `json:"allowed" jsonschema:"false when a preflight gate rejects this upgrade"`
	Blocker   string        `json:"blocker,omitempty" jsonschema:"why the upgrade is refused, when it is"`
	Steps     []UpgradeStep `json:"steps,omitempty"`
	NodeCount int           `json:"nodeCount"`
	Note      string        `json:"note,omitempty"`
}

// UpgradePlanHandler previews a rolling cluster upgrade.
func UpgradePlanHandler(conn *kube.Connector) sdk.ToolHandlerFor[UpgradePlanIn, UpgradePlanOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in UpgradePlanIn) (*sdk.CallToolResult, UpgradePlanOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, UpgradePlanOut{}, err
		}
		if in.Target == "" {
			return nil, UpgradePlanOut{}, fmt.Errorf("target is required, as vMAJOR.MINOR.PATCH")
		}

		// The node connector's client, in its default context — the same
		// connector upgrade_apply's cluster scope proves this node with
		// before it rolls anything. A plan is a preview of that rollout, so
		// reading it through --kubeconfig or a per-call context would let the
		// plan describe an upgrade the apply refuses to run: a narrowed
		// credential can point anywhere, and the two tools must not split.
		cl, err := cluster(conn, "")
		if err != nil {
			return nil, UpgradePlanOut{}, err
		}
		// The orchestrator's own plan (internal/upgrade.Plan): one
		// classification, preflight and ordering for `upgrade --cluster`'s
		// dry run and this tool, so the two cannot drift.
		plan, err := upgrade.Plan(ctx, cl.Typed, in.Target, in.WorkersMaxUnavailable)
		if err != nil {
			return nil, UpgradePlanOut{}, err
		}

		out := UpgradePlanOut{Target: in.Target, NodeCount: plan.NodeCount, Blocker: plan.Blocker}
		if !plan.Allowed {
			return nil, out, nil
		}
		out.Allowed = true
		for _, s := range plan.Steps {
			step := UpgradeStep{Order: s.Order, Nodes: s.Nodes, Role: s.Role, Serial: s.Serial}
			if s.Role == "control-plane" {
				step.Comment = "control planes are never upgraded concurrently: two etcd members down at once loses quorum"
			}
			out.Steps = append(out.Steps, step)
		}
		out.Note = "This is a preview. Nothing has changed. Control planes are always upgraded one at a " +
			"time so two etcd members are never down together."
		return nil, out, nil
	}
}

// ------------------------------------------------------ uninstall_preview

// UninstallPreviewIn is uninstall_preview's (empty) input.
type UninstallPreviewIn struct{}

// UninstallTarget is one path uninstall would remove.
type UninstallTarget struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists" jsonschema:"whether it is present on this node right now"`
	IsDir  bool   `json:"isDir,omitempty"`
	Size   int64  `json:"size,omitempty" jsonschema:"bytes, for files only"`
}

// UninstallPreviewOut is uninstall_preview's result.
type UninstallPreviewOut struct {
	Targets []UninstallTarget `json:"targets"`
	Present int               `json:"present" jsonschema:"how many of them exist on this node"`
	// Conditionals is the shared preview's items, unchanged: the same
	// uninstall.Preview() the CLI dry run renders, so the two surfaces
	// enumerate the same effects. Schema change reviewed under api-compat.
	Conditionals []uninstall.PreviewItem `json:"conditionals" jsonschema:"the effects beyond the plain path deletions — the netns, Cilium, cgroup, legacy-kubelet and local-path directory layout, plus the host-dependent conditionals; an item whose probe could not answer reports that error in unknown, since the effect would be left in place"`
	Note         string                  `json:"note"`
}

// UninstallPreviewHandler reports what an uninstall would remove.
func UninstallPreviewHandler() sdk.ToolHandlerFor[UninstallPreviewIn, UninstallPreviewOut] {
	return func(context.Context, *sdk.CallToolRequest, UninstallPreviewIn) (*sdk.CallToolResult, UninstallPreviewOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, UninstallPreviewOut{}, err
		}

		// One preview for both surfaces: the conditionals come from the same
		// uninstall.Preview() the CLI dry run renders its prose from, so the
		// tool cannot enumerate a different set of effects.
		report := uninstall.Preview()

		out := UninstallPreviewOut{Conditionals: report.Items}
		for _, path := range report.Targets {
			t := UninstallTarget{Path: path}
			if fi, err := os.Stat(path); err == nil {
				t.Exists = true
				t.IsDir = fi.IsDir()
				if !fi.IsDir() {
					t.Size = fi.Size()
				}
				out.Present++
			}
			out.Targets = append(out.Targets, t)
		}
		sort.Slice(out.Targets, func(i, j int) bool { return out.Targets[i].Path < out.Targets[j].Path })

		// The note is about the directory the resolved snapshot settings name —
		// the one snapshot_list lists — not one this handler resolved on its
		// own. A settings read that failed leaves the bare-name resolver's dir
		// as the fallback, so the note still says something true.
		dir := ""
		if cfg, err := loadSnapshotConfig(); err == nil {
			dir = cfg.Dir
		}
		if dir == "" {
			dir = snapshotDir()
		}

		out.Note = fmt.Sprintf("Preview only: nothing has been removed. An uninstall would delete %d of these "+
			"%d paths, which is irreversible and takes the cluster's state with it — including etcd data and "+
			"the PKI. It would also have the %d conditional effect(s) listed in conditionals; one whose probe "+
			"could not answer says so in unknown. %s",
			out.Present, len(out.Targets), len(out.Conditionals), snapshotDirNote(dir, report.Targets))
		return nil, out, nil
	}
}

// snapshotDirNote says whether an uninstall removes the snapshots in dir.
// spec.snapshots.dir may be any absolute path, including one inside a removal
// target such as /var/lib/stratum, or a symlink into or out of one.
//
// dir is resolved fully. A target is resolved up to its parent only: removal
// follows every parent component, but unlinks a final component that is a
// symlink without following it.
func snapshotDirNote(dir string, targets []string) string {
	hedge := func(what string, err error) string {
		return fmt.Sprintf("Snapshots under %s are listed by snapshot_list, but %s could not be resolved (%v), "+
			"so whether an uninstall removes them is unknown: copy them off the node first.", dir, what, err)
	}
	real, err := resolveExisting(dir)
	if err != nil {
		return hedge("that directory", err)
	}
	for _, t := range targets {
		parent, err := resolveExisting(filepath.Dir(t))
		if err != nil {
			return hedge(t, err)
		}
		rt := filepath.Join(parent, filepath.Base(t))
		if rel, err := filepath.Rel(rt, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Sprintf("Snapshots under %s are listed by snapshot_list, but that directory is inside %s, "+
				"which an uninstall deletes: they WOULD be removed. Copy them off the node first.", dir, t)
		}
	}
	return fmt.Sprintf("Snapshots under %s are listed by snapshot_list and are NOT part of this set.", dir)
}

// resolveExisting is filepath.EvalSymlinks for a path whose tail may not exist
// yet: the snapshot dir is created on first save. It resolves the nearest
// existing ancestor and re-appends the rest. A dangling symlink is an error.
func resolveExisting(path string) (string, error) {
	rest := ""
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || p == filepath.Dir(p) {
			return "", err
		}
		// A dangling symlink: its target can appear later, so say nothing about it.
		if _, lerr := os.Lstat(p); lerr == nil {
			return "", fmt.Errorf("%s is a dangling symlink", p)
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// ------------------------------------------------ snapshot_restore_copies

// SnapshotRestoreCopiesIn is snapshot_restore_copies's paging input.
type SnapshotRestoreCopiesIn struct {
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum copies to return, in name order; defaults to 100, capped at 500"`
	Continue string `json:"continue_token,omitempty" jsonschema:"continuation from the preceding page; restart if the copies change"`
}

// RestoreCopyOut is one etcd data copy a restore kept beside the data dir.
type RestoreCopyOut struct {
	Path     string `json:"path"`
	Kind     string `json:"kind" jsonschema:"pre-restore: the data from before a restore; failed-restore: what a failed restore wrote"`
	Bytes    int64  `json:"bytes"`
	Size     string `json:"size" jsonschema:"the same size in binary units, for reading"`
	Modified string `json:"modified" jsonschema:"the newest modification time anywhere in the copy, RFC 3339 with nanoseconds; pass it back unchanged to snapshot_prune_restores"`
}

// SnapshotRestoreCopiesOut is snapshot_restore_copies's result.
type SnapshotRestoreCopiesOut struct {
	ListMeta
	Copies    []RestoreCopyOut `json:"copies"`
	PageBytes int64            `json:"pageBytes" jsonschema:"the total size of the copies on this page"`
}

// SnapshotRestoreCopiesHandler lists the etcd data copies snapshot restore
// kept, the MCP side of `stratum snapshot prune-restores`. It pages
// over the copies' names, which is cheap, and walks into only the copies it
// returns, so neither the result nor the work is unbounded.
func SnapshotRestoreCopiesHandler() sdk.ToolHandlerFor[SnapshotRestoreCopiesIn, SnapshotRestoreCopiesOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SnapshotRestoreCopiesIn) (*sdk.CallToolResult, SnapshotRestoreCopiesOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, SnapshotRestoreCopiesOut{}, err
		}
		limit := ListOptions{Limit: in.Limit}.limit()
		named, err := etcdsnap.RestoreCopyPaths()
		if err != nil {
			return nil, SnapshotRestoreCopiesOut{}, err
		}
		page, cont, err := restoreCopyPage(named, limit, in.Continue)
		if err != nil {
			return nil, SnapshotRestoreCopiesOut{}, err
		}
		out := SnapshotRestoreCopiesOut{Copies: []RestoreCopyOut{}}
		for _, c := range page {
			c, err := etcdsnap.StatRestoreCopy(c)
			if err != nil {
				return nil, SnapshotRestoreCopiesOut{}, err
			}
			out.Copies = append(out.Copies, RestoreCopyOut{
				Path: c.Path, Kind: c.Kind, Bytes: c.Bytes, Size: etcdsnap.HumanBytes(c.Bytes),
				Modified: c.Modified.Format(time.RFC3339Nano),
			})
			out.PageBytes += c.Bytes
		}
		out.ListMeta = newListMeta(len(out.Copies), limit, cont)
		note := "Listing only: nothing was removed. Each copy is a full copy of etcd's data on the " +
			"datastore's own disk, kept by snapshot_restore; a pre-restore copy may be the only copy of " +
			"the data from before a restore. Once the cluster is verified, snapshot_prune_restores " +
			"removes the copies passed to it, exactly as listed here."
		if out.Note != "" {
			note += " " + out.Note
		}
		out.Note = note
		return nil, out, nil
	}
}

// restoreCopyPage ties the cursor to the list of copy paths, so a copy added
// or removed between pages forces a restart rather than a skip or a repeat.
func restoreCopyPage(copies []etcdsnap.RestoreCopy, limit int, cursor string) ([]etcdsnap.RestoreCopy, string, error) {
	paths := make([]string, len(copies))
	for i, c := range copies {
		paths[i] = c.Path
	}
	return fingerprintPage(copies, paths, limit, cursor, "snapshot_restore_copies")
}

// fingerprintPage pages items by an offset cursor tied to a fingerprint of
// key, the value that identifies the list: a change to it between pages makes
// the cursor stale, forcing a restart rather than a skip or a repeat. tool
// names the tool the restart message points at.
func fingerprintPage[T any](items []T, key any, limit int, cursor, tool string) ([]T, string, error) {
	data, err := json.Marshal(key)
	if err != nil {
		return nil, "", err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	start := 0
	if cursor != "" {
		digest, offset, ok := strings.Cut(cursor, ":")
		start, err = strconv.Atoi(offset)
		if !ok || err != nil || digest != fingerprint || start < 0 || start >= len(items) {
			return nil, "", fmt.Errorf("invalid or stale continue_token: restart %s without a token", tool)
		}
	}
	end := start + min(limit, len(items)-start)
	next := ""
	if end < len(items) {
		next = fingerprint + ":" + strconv.Itoa(end)
	}
	return items[start:end], next, nil
}
