package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/certs"
	clustercfg "github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/registry"
	"github.com/ctrliq/stratum/internal/token"
)

type SnapshotSaveIn struct {
	Name   string `json:"name,omitempty" jsonschema:"file name for the snapshot; defaults to a timestamped scheduled name"`
	Retain int    `json:"retain,omitempty" jsonschema:"after saving, keep only this many of this host's scheduled snapshots; 0 keeps all"`
	SkipS3 bool   `json:"skip_s3,omitempty" jsonschema:"write locally only, even when an object store is configured"`
}

type SnapshotSaveOut struct {
	Path     string   `json:"path" jsonschema:"where the snapshot was written on this node"`
	Uploaded bool     `json:"uploaded" jsonschema:"whether it was also copied to the configured object store"`
	Progress string   `json:"progress,omitempty" jsonschema:"progress output from the snapshot subsystem, if any"`
	Note     string   `json:"note,omitempty"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"prune failures the save's prune step returned, as data beside the note; the snapshot itself succeeded"`
}

// Save joins unconfined; this server usually runs as root; .db-only spares other files in the dir.
func checkSnapshotName(name, dir string) error {
	if name == "" {
		return nil // Save generates a timestamped scheduled name.
	}
	if name != filepath.Base(name) || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) || !strings.HasSuffix(name, ".db") {
		return fmt.Errorf("name %q must be a bare file name ending in .db, with no path separators or parent references: "+
			"snapshots are always written to %s", name, dir)
	}
	return nil
}

// SnapshotSaveHandler takes an etcd snapshot.
func SnapshotSaveHandler() sdk.ToolHandlerFor[SnapshotSaveIn, SnapshotSaveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in SnapshotSaveIn) (*sdk.CallToolResult, SnapshotSaveOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, SnapshotSaveOut{}, err
		}

		cfg, err := loadSnapshotConfig()
		if err != nil {
			return nil, SnapshotSaveOut{}, fmt.Errorf("read snapshot config: %w", err)
		}
		if err := checkSnapshotName(in.Name, cfg.Dir); err != nil {
			return nil, SnapshotSaveOut{}, err
		}
		opts := etcdsnap.SaveOptions{
			Dir:    cfg.Dir,
			Name:   in.Name,
			Retain: in.Retain,
		}
		if !in.SkipS3 {
			opts.S3 = cfg.S3Target()
		}

		var pruneWarns []error
		opts.PruneWarn = func(e error) { pruneWarns = append(pruneWarns, e) }
		// Nothing redirects os.Stdout: on the stdio transport it is the JSON-RPC stream.
		var buf bytes.Buffer
		path, serr := snapshotSave(ctx, &buf, opts)
		if serr != nil {
			return nil, SnapshotSaveOut{}, fmt.Errorf("save snapshot: %w", serr)
		}

		out := SnapshotSaveOut{
			Path:     path,
			Uploaded: opts.S3 != nil && opts.S3.Enabled(),
			Progress: strings.TrimSpace(buf.String()),
		}
		if !out.Uploaded {
			out.Note = "Written to this node only. A snapshot on the same machine as the etcd it backs up " +
				"does not survive losing that machine; configure an object store with snapshot_schedule."
		}
		if len(pruneWarns) > 0 {
			out.Warnings = pruneWarnStrings(pruneWarns)
			out.Note = "The snapshot saved, but the prune reported failures: " + pruneErrText(pruneWarns) +
				". Some old snapshots may not have been removed."
		}
		return nil, out, nil
	}
}

// Retention only: the object-store destination is deliberately not settable.
type SnapshotScheduleIn struct {
	Retain int `json:"retain,omitempty" jsonschema:"how many scheduled snapshots to keep per node; omit to leave unchanged"`
}

type SnapshotScheduleOut struct {
	Retain            int    `json:"retain"`
	Bucket            string `json:"bucket,omitempty"`
	Endpoint          string `json:"endpoint,omitempty" jsonschema:"the object-store endpoint currently recorded; reported for confirmation, and not settable through this server"`
	Region            string `json:"region,omitempty"`
	Prefix            string `json:"prefix,omitempty"`
	PathStyle         bool   `json:"path_style"`
	ConfigPath        string `json:"configPath"`
	CredentialsOnDisk bool   `json:"credentialsOnDisk" jsonschema:"the config file already holds object-store credentials; they were left untouched"`
	Note              string `json:"note"`
}

func applySchedule(cfg etcdsnap.Config, in SnapshotScheduleIn) etcdsnap.Config {
	if in.Retain > 0 {
		cfg.Retain = in.Retain
	}
	return cfg
}

func SnapshotScheduleHandler() sdk.ToolHandlerFor[SnapshotScheduleIn, SnapshotScheduleOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SnapshotScheduleIn) (*sdk.CallToolResult, SnapshotScheduleOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, SnapshotScheduleOut{}, err
		}
		if in.Retain < 0 {
			return nil, SnapshotScheduleOut{}, fmt.Errorf("retain cannot be negative, got %d", in.Retain)
		}

		cfg, err := clustercfg.LoadSnapshotConfig()
		if err != nil {
			return nil, SnapshotScheduleOut{}, fmt.Errorf("read snapshot config: %w", err)
		}
		cfg = applySchedule(cfg, in)

		if err := etcdsnap.SaveConfig(io.Discard, cfg); err != nil {
			return nil, SnapshotScheduleOut{}, fmt.Errorf("save snapshot config: %w", err)
		}

		out := SnapshotScheduleOut{
			Retain:            cfg.Retain,
			Bucket:            cfg.S3.Bucket,
			Endpoint:          cfg.S3.Endpoint,
			Region:            cfg.S3.Region,
			Prefix:            cfg.S3.Prefix,
			PathStyle:         cfg.S3.PathStyle,
			ConfigPath:        etcdsnap.ConfigPath,
			CredentialsOnDisk: cfg.HasPersistedCredentials(),
			Note: "Saved. The controller's snapshot loop reads this config on its next tick, so there is " +
				"no timer to restart. The object store itself is reported here but cannot be changed " +
				"through this server: a snapshot is the whole cluster, so its destination is set by an " +
				"operator in the config file or the environment, not per call.",
		}
		if out.CredentialsOnDisk {
			out.Note += " This config file already holds credentials on disk; they were left unchanged."
		}
		return nil, out, nil
	}
}

type SnapshotPruneIn struct {
	Keep int `json:"keep" jsonschema:"how many of this node's scheduled snapshots to keep, newest first; must be at least 1"`
}

type SnapshotPruneOut struct {
	Keep     int      `json:"keep"`
	Before   int      `json:"before" jsonschema:"snapshots present before the prune"`
	After    int      `json:"after"`
	Removed  int      `json:"removed"`
	Progress string   `json:"progress,omitempty"`
	Note     string   `json:"note,omitempty"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"the prune's failed targets as the prune returned them, as data beside the note"`
}

func SnapshotPruneHandler() sdk.ToolHandlerFor[SnapshotPruneIn, SnapshotPruneOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SnapshotPruneIn) (*sdk.CallToolResult, SnapshotPruneOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, SnapshotPruneOut{}, err
		}
		if in.Keep < 1 {
			return nil, SnapshotPruneOut{}, fmt.Errorf(
				"keep must be at least 1, got %d: this tool applies a retention policy, and keeping zero "+
					"would delete every snapshot this node has. Deleting a specific snapshot is not something "+
					"this server does", in.Keep)
		}

		// A zero S3Target prunes locally only.
		cfg, err := loadSnapshotConfig()
		if err != nil {
			return nil, SnapshotPruneOut{}, fmt.Errorf("read snapshot config: %w", err)
		}
		s3 := cfg.S3Target()

		hostname, err := host.Hostname()
		if err != nil {
			return nil, SnapshotPruneOut{}, fmt.Errorf("determine this node's name: %w", err)
		}

		// The inventories and the prune must act in the same dir, or the counts count the wrong one.
		dir := cfg.Dir
		before, _ := snapshotList(dir, s3)

		var pruneErrs []error
		pruneErrs = snapshotPrune(dir, hostname, in.Keep, s3)

		after, _ := snapshotList(dir, s3)

		out := SnapshotPruneOut{
			Keep:    in.Keep,
			Before:  len(before),
			After:   len(after),
			Removed: max(0, len(before)-len(after)),
		}
		if len(pruneErrs) > 0 {
			out.Warnings = pruneWarnStrings(pruneErrs)
			out.Note = "The prune reported failures: " + pruneErrText(pruneErrs) +
				". Some snapshots may not have been removed."
		}
		return nil, out, nil
	}
}

func pruneErrText(errs []error) string {
	return strings.Join(pruneWarnStrings(errs), "; ")
}

func pruneWarnStrings(errs []error) []string {
	warns := make([]string, 0, len(errs))
	for _, err := range errs {
		warns = append(warns, err.Error())
	}
	return warns
}

type TokenCreateIn struct {
	TTLHours     int  `json:"ttl_hours,omitempty" jsonschema:"token lifetime in hours; defaults to 24. Zero is NOT non-expiring here — omit the field for the default"`
	ControlPlane bool `json:"control_plane,omitempty" jsonschema:"mint a control-plane join token instead of a worker token: it can read the shared certificate material, which this uploads for the joiner. Never use it for a worker join"`
}

type TokenCreateOut struct {
	Token      string `json:"token" jsonschema:"the join token, returned once and never logged. Treat it as a credential"`
	CACertHash string `json:"caCertHash" jsonschema:"the CA pin the joiner verifies the control plane against"`
	Server     string `json:"server"`
	ExpiresIn  string `json:"expiresIn"`
	CertKey    string `json:"certKey,omitempty" jsonschema:"control-plane join key, present only when control_plane was set. Also a credential"`
	// A worker learns these only from its join line; this result is that line's only source.
	JoinFlags token.JoinFlags `json:"joinFlags"`
	// token.Issue revokes a token unless delivery is reported here; then cleanup is the caller's.
	Delivered bool   `json:"delivered" jsonschema:"the credentials in this result were handed to the caller and are not revoked"`
	Note      string `json:"note"`
}

// tokenDeps are token_create's callees, injectable so the handler is testable without a cluster.
type tokenDeps struct {
	create func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error)
	// A mint re-keys or republishes trust, so it must refuse mid-rotation. Injected for testing.
	rekeyGuard func() error
	// Re-keying under a running rotation locks followers out of the bundle. Injected for testing.
	maintenanceFence func() (*k8sutil.MaintenanceLease, error)
	newCertKey       func() (string, error)
	uploadCerts      func(certKeyHex string, owner *metav1.OwnerReference, confirm func(context.Context) error) error
	nodeIP           func() (string, error)
	// Revokes a minted token when a later step fails before the caller sees it.
	revoke func(tok string, cause error) error
}

func defaultTokenDeps() tokenDeps {
	return tokenDepsFor(nodeClientBuilder(nil))
}

func tokenDepsFor(buildClient func() (kubernetes.Interface, error)) tokenDeps {
	return tokenDeps{
		// The mint rides this node's admin.conf credential, never the server's configurable connector.
		create: func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			cs, err := buildClient()
			if err != nil {
				return "", "", nil, err
			}
			return token.Create(w, cs, serverURL, ttl, controlPlane, confirm)
		},
		rekeyGuard: func() error {
			cs, err := buildClient()
			if err != nil {
				return err
			}
			return certs.GuardRekeyWithClient(cs)
		},
		maintenanceFence: func() (*k8sutil.MaintenanceLease, error) {
			cs, err := buildClient()
			if err != nil {
				return nil, err
			}
			return k8sutil.MaintenanceFence(cs)
		},
		newCertKey:  ha.NewCertKey,
		uploadCerts: ha.UploadCerts,
		nodeIP:      nodestate.NodeIP,
		revoke:      token.RevokeOnFailure,
	}
}

// The mint writes what the server's credential must never write, so it rides super-admin.conf.
func TokenCreateHandler() sdk.ToolHandlerFor[TokenCreateIn, TokenCreateOut] {
	return tokenCreateWith(defaultTokenDeps())
}

func tokenCreateWith(deps tokenDeps) sdk.ToolHandlerFor[TokenCreateIn, TokenCreateOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in TokenCreateIn) (*sdk.CallToolResult, TokenCreateOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, TokenCreateOut{}, err
		}
		if in.TTLHours < 0 {
			return nil, TokenCreateOut{}, fmt.Errorf("ttl_hours cannot be negative, got %d", in.TTLHours)
		}
		// time.Duration is int64 ns; an overflow wraps negative and mints a non-expiring token.
		const maxTTLHours = int(math.MaxInt64 / int64(time.Hour))
		if in.TTLHours > maxTTLHours {
			return nil, TokenCreateOut{}, fmt.Errorf(
				"ttl_hours %d is too large: the maximum is %d (about %d years)",
				in.TTLHours, maxTTLHours, maxTTLHours/24/365)
		}

		// An omitted ttl_hours means the default, not "never expires".
		ttl := token.DefaultTokenTTL
		if in.TTLHours > 0 {
			ttl = time.Duration(in.TTLHours) * time.Hour
		}

		// Derive the flags before the mint: a failure here must leave nothing to revoke.
		flags, err := token.LoadJoinFlags()
		if err != nil {
			return nil, TokenCreateOut{}, fmt.Errorf("derive the join flags the result must carry: %w", err)
		}

		// Never take the join URL from the caller: a joiner sends the bootstrap credential to it.
		ip, err := deps.nodeIP()
		if err != nil {
			return nil, TokenCreateOut{}, fmt.Errorf("determine this node's IP for the join URL: %w", err)
		}
		serverURL, err := netcfg.GrantServerURL(ip)
		if err != nil {
			return nil, TokenCreateOut{}, fmt.Errorf("derive the join URL: %w", err)
		}

		var lease *k8sutil.MaintenanceLease
		// Every mint republishes cluster trust, so the fence is not control-plane-scoped.
		if deps.maintenanceFence != nil {
			var err error
			lease, err = deps.maintenanceFence()
			if err != nil {
				return nil, TokenCreateOut{}, err
			}
			defer lease.Release()
		}
		var out TokenCreateOut
		var mint bytes.Buffer
		_, ierr := token.Issue(&mint, token.IssueDeps{
			Confirm:     func() error { return lease.Confirm(ctx) },
			RekeyGuard:  deps.rekeyGuard,
			Create:      deps.create,
			NewCertKey:  deps.newCertKey,
			UploadCerts: deps.uploadCerts,
			Revoke:      deps.revoke,
		}, serverURL, ttl, in.ControlPlane, flags, func(grant token.Grant) (bool, error) {
			out = TokenCreateOut{
				Token:      grant.Token,
				CACertHash: grant.CACertHash,
				Server:     grant.ServerURL,
				ExpiresIn:  ttl.String(),
				CertKey:    grant.CertKey,
				JoinFlags:  grant.JoinFlags,
				Delivered:  true,
				Note: "This token is a credential and is returned once. It is not logged and cannot be " +
					"retrieved again — token_list shows only ids. Anything that can read it can join a node " +
					"to this cluster.",
			}
			if in.ControlPlane {
				out.Note += " The control-plane key decrypts the shared certificate material and is a second, " +
					"more sensitive credential; it expires with the token. The token itself can read that material, " +
					"so use it only for a control-plane join and mint a separate token for a worker."
			}
			return true, nil
		})
		if ierr != nil {
			return nil, TokenCreateOut{}, ierr
		}
		return nil, out, nil
	}
}

type RegistryApplyIn struct{}

type RegistryApplyOut struct {
	Configured bool     `json:"configured" jsonschema:"registries.yaml has mirrors or registry credentials/TLS, and they were applied"`
	Changed    bool     `json:"changed" jsonschema:"the containerd drop-in changed, so containerd must be restarted to pick it up"`
	Restarted  bool     `json:"restarted" jsonschema:"always false: this tool applies the configuration only and leaves any containerd restart to the operator"`
	Registries []string `json:"registries,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	Note       string   `json:"note"`
}

func RegistryApplyHandler() sdk.ToolHandlerFor[RegistryApplyIn, RegistryApplyOut] {
	return func(context.Context, *sdk.CallToolRequest, RegistryApplyIn) (*sdk.CallToolResult, RegistryApplyOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, RegistryApplyOut{}, err
		}

		res, err := registry.Apply()
		if err != nil {
			return nil, RegistryApplyOut{}, fmt.Errorf("apply registry configuration: %w", err)
		}

		out := RegistryApplyOut{
			Configured: res.Configured,
			Changed:    res.Changed,
			Restarted:  false,
			Registries: res.Registries,
			Warnings:   res.Warnings,
		}
		switch {
		case !res.Configured:
			out.Note = registry.NothingConfigured + ". Nothing was applied. " + registry.DefaultIsNotAFault
		case res.Changed:
			out.Note = "Configuration changed. containerd must be restarted before it takes effect — " +
				"this tool does not restart it, because that briefly interrupts every image pull on the node."
		default:
			out.Note = "Configuration already matched; nothing changed and no restart is needed."
		}
		return nil, out, nil
	}
}

// No handlers field: a handler names a binary containerd executes, left to the operator.
type RuntimeApplyIn struct{}

type RuntimeApplyOut struct {
	Handlers  map[string]string `json:"handlers,omitempty" jsonschema:"the runtime handlers this node declares, by name"`
	Source    string            `json:"source" jsonschema:"where the handlers came from: a worker's saved handlers, or a control plane's config file"`
	Changed   bool              `json:"changed" jsonschema:"the containerd drop-in changed"`
	Unloaded  bool              `json:"unloaded" jsonschema:"the running containerd has not loaded the drop-in, after this call or an earlier one, so it must be restarted before the change takes effect"`
	Restarted bool              `json:"restarted" jsonschema:"always false: this tool writes the drop-in only and leaves the containerd restart to the operator"`
	Warnings  []string          `json:"warnings,omitempty"`
	Note      string            `json:"note"`
}

func RuntimeApplyHandler() sdk.ToolHandlerFor[RuntimeApplyIn, RuntimeApplyOut] {
	return func(context.Context, *sdk.CallToolRequest, RuntimeApplyIn) (*sdk.CallToolResult, RuntimeApplyOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, RuntimeApplyOut{}, err
		}
		var progress strings.Builder
		res, err := clustercfg.ApplyRuntimeHandlers(&progress, clustercfg.RuntimeApplyOptions{})
		if err != nil {
			return nil, RuntimeApplyOut{}, fmt.Errorf("apply runtime handlers: %w", err)
		}
		out := RuntimeApplyOut{Handlers: res.Handlers, Source: res.Source, Changed: res.Changed, Unloaded: res.Unloaded, Restarted: false}
		for _, line := range strings.Split(progress.String(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out.Warnings = append(out.Warnings, line)
			}
		}
		switch {
		case res.Unloaded:
			out.Note = "containerd has not loaded the drop-in and must be restarted before the change takes effect (`stratum runtime restart` " +
				"on the node) — this tool does not restart it, because that briefly interrupts every image pull on the node. " +
				"The node is labelled for a new handler only once containerd has loaded it."
		case res.Changed:
			out.Note = "The drop-in changed; containerd is not running and loads it when it next starts."
		default:
			out.Note = "The drop-in already matched what containerd loaded; nothing changed and no restart is needed."
		}
		return nil, out, nil
	}
}
