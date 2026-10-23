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

// The Stratum write toolset: mutating but recoverable.
// Registered only with --allow-write.

// ------------------------------------------------------------ snapshot_save

// SnapshotSaveIn is snapshot_save's input.
type SnapshotSaveIn struct {
	Name   string `json:"name,omitempty" jsonschema:"file name for the snapshot; defaults to a timestamped scheduled name"`
	Retain int    `json:"retain,omitempty" jsonschema:"after saving, keep only this many of this host's scheduled snapshots; 0 keeps all"`
	SkipS3 bool   `json:"skip_s3,omitempty" jsonschema:"write locally only, even when an object store is configured"`
}

// SnapshotSaveOut is snapshot_save's result.
type SnapshotSaveOut struct {
	Path     string   `json:"path" jsonschema:"where the snapshot was written on this node"`
	Uploaded bool     `json:"uploaded" jsonschema:"whether it was also copied to the configured object store"`
	Progress string   `json:"progress,omitempty" jsonschema:"progress output from the snapshot subsystem, if any"`
	Note     string   `json:"note,omitempty"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"prune failures the save's prune step returned, as data beside the note; the snapshot itself succeeded"`
}

// checkSnapshotName rejects anything that is not a bare file name ending in .db.
//
// etcdsnap.Save does filepath.Join(dir, name), which cleans the result but
// does not confine it: "../../etc/cron.d/x" resolves outside the snapshot
// directory, and this server usually runs as root on a node. That turns a
// write-tier tool into an arbitrary-file-write primitive. The CLI keeps
// accepting a path, where an operator typing one means it; the constraint
// belongs at the tool boundary, which is the untrusted input. The .db suffix
// keeps a custom snapshot dir shared with other files (spec.snapshots.dir may
// be any directory) from having one of them replaced by name.
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

		// The settings come first: the save writes the directory they resolve
		// (spec.snapshots.dir), and the name refusal names that directory.
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

		// The request's context bounds the save, exactly as it bounds the
		// sibling certs_rotate's rotation: a session that goes away aborts the
		// etcdctl run (and the S3 upload) instead of leaving it running to
		// completion with nobody reading the result.
		// The save prints a failed prune for the CLI and the scheduled loop
		// and hands the same failures here as data, so the result reports the
		// warning beside the path rather than the captured text being searched.
		var pruneWarns []error
		opts.PruneWarn = func(e error) { pruneWarns = append(pruneWarns, e) }
		// The save takes the writer itself: a per-call buffer, whose content
		// becomes the result's Progress. Nothing redirects os.Stdout — on the
		// stdio transport it is the JSON-RPC stream.
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
			// The prune is a warning, not an error: the snapshot itself
			// succeeded, and the result names the failure beside the path —
			// as prose in the note and as the returned failures in Warnings.
			out.Warnings = pruneWarnStrings(pruneWarns)
			out.Note = "The snapshot saved, but the prune reported failures: " + pruneErrText(pruneWarns) +
				". Some old snapshots may not have been removed."
		}
		return nil, out, nil
	}
}

// -------------------------------------------------------- snapshot_schedule

// SnapshotScheduleIn is snapshot_schedule's input. Retention only: where
// snapshots go is deliberately not reachable from this tool.
type SnapshotScheduleIn struct {
	Retain int `json:"retain,omitempty" jsonschema:"how many scheduled snapshots to keep per node; omit to leave unchanged"`
}

// SnapshotScheduleOut is snapshot_schedule's result.
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

// applySchedule merges the supplied fields into cfg.
func applySchedule(cfg etcdsnap.Config, in SnapshotScheduleIn) etcdsnap.Config {
	if in.Retain > 0 {
		cfg.Retain = in.Retain
	}
	return cfg
}

// SnapshotScheduleHandler updates the snapshot retention policy. The
// object store is reported but not settable.
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

// ----------------------------------------------------------- snapshot_prune

// SnapshotPruneIn is snapshot_prune's input.
type SnapshotPruneIn struct {
	Keep int `json:"keep" jsonschema:"how many of this node's scheduled snapshots to keep, newest first; must be at least 1"`
}

// SnapshotPruneOut is snapshot_prune's result.
type SnapshotPruneOut struct {
	Keep     int      `json:"keep"`
	Before   int      `json:"before" jsonschema:"snapshots present before the prune"`
	After    int      `json:"after"`
	Removed  int      `json:"removed"`
	Progress string   `json:"progress,omitempty"`
	Note     string   `json:"note,omitempty"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"the prune's failed targets as the prune returned them, as data beside the note"`
}

// SnapshotPruneHandler applies the retention policy to this node's
// scheduled snapshots.
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

		// Not discarded: a zero S3Target prunes locally only, and reports
		// a clean Removed count for it.
		cfg, err := loadSnapshotConfig()
		if err != nil {
			return nil, SnapshotPruneOut{}, fmt.Errorf("read snapshot config: %w", err)
		}
		s3 := cfg.S3Target()

		hostname, err := host.Hostname()
		if err != nil {
			return nil, SnapshotPruneOut{}, fmt.Errorf("determine this node's name: %w", err)
		}

		// The prune acts in the directory the resolved settings name, and both
		// inventories list the same place, or the counts count the wrong dir.
		dir := cfg.Dir
		before, _ := snapshotList(dir, s3)

		// PruneScheduled returns each failed target as an error, so the
		// result names the failure as data; nothing reads the captured text
		// for a WARN.
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

// pruneErrText joins the prune's returned errors for a tool result note.
func pruneErrText(errs []error) string {
	return strings.Join(pruneWarnStrings(errs), "; ")
}

// pruneWarnStrings renders the prune's returned errors as the strings the
// tool results carry in Warnings, so the failures travel as data the caller
// can read without matching the note's prose.
func pruneWarnStrings(errs []error) []string {
	warns := make([]string, 0, len(errs))
	for _, err := range errs {
		warns = append(warns, err.Error())
	}
	return warns
}

// ------------------------------------------------------------- token_create

// TokenCreateIn is token_create's input.
type TokenCreateIn struct {
	TTLHours     int  `json:"ttl_hours,omitempty" jsonschema:"token lifetime in hours; defaults to 24. Zero is NOT non-expiring here — omit the field for the default"`
	ControlPlane bool `json:"control_plane,omitempty" jsonschema:"mint a control-plane join token instead of a worker token: it can read the shared certificate material, which this uploads for the joiner. Never use it for a worker join"`
}

// TokenCreateOut is token_create's result.
type TokenCreateOut struct {
	Token      string `json:"token" jsonschema:"the join token, returned once and never logged. Treat it as a credential"`
	CACertHash string `json:"caCertHash" jsonschema:"the CA pin the joiner verifies the control plane against"`
	Server     string `json:"server"`
	ExpiresIn  string `json:"expiresIn"`
	CertKey    string `json:"certKey,omitempty" jsonschema:"control-plane join key, present only when control_plane was set. Also a credential"`
	// JoinFlags are the flags the joining node must carry on its join line to
	// match this cluster: the pod and service ranges and the DNS domain when
	// they differ from the defaults, and the CIS profile marker. The network
	// flags (--pod-cidr, --service-cidr, --cluster-domain) go on a worker join
	// line only: a control plane gets its network from the cluster it joins,
	// and `stratum up` refuses those flags without --worker. The profile
	// marker goes on every join line. They travel as data derived by
	// token.LoadJoinFlags, the same derivation `stratum token create`
	// renders, because a worker learns them only from its join line and this
	// result is that line's only source.
	JoinFlags token.JoinFlags `json:"joinFlags"`
	// Delivered records that credential bytes were handed to the caller in
	// this result. token.Issue holds ownership only until this delivery, so a
	// delivered grant is never revoked behind the caller's back; what the
	// caller does with the credential from here is its own to clean up.
	Delivered bool   `json:"delivered" jsonschema:"the credentials in this result were handed to the caller and are not revoked"`
	Note      string `json:"note"`
}

// tokenDeps are the functions token_create calls, injectable so the
// handler is testable without a cluster.
type tokenDeps struct {
	create func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error)
	// rekeyGuard is the one re-key entry point (certs.GuardRekey in
	// production): a control-plane mint re-keys the shared cert material and
	// a worker mint republishes cluster-info from local PKI, so both must
	// refuse while a CA rotation or etcd-CA migration is staging the key the
	// flow's follower control planes decrypt stratum-certs with — a worker
	// mint from a controller that has not joined the active rotation would
	// replace the published old‖new trust with old-only. The same guard
	// `stratum token create` answers to, so both front ends refuse the same
	// request the same way. Injected for testing.
	rekeyGuard func() error
	// maintenanceFence takes the cluster-wide maintenance fence
	// (k8sutil.AdminMaintenanceFence in production: both maintenance leases,
	// via maintlease.AcquireCompatible) around the mint, the same fence
	// `stratum token create` takes before its mint — the fence
	// `stratum upgrade --cluster`, `rotate-ca --cluster` and
	// `migrate-etcd-ca` hold for their whole runs. A mint that lands mid-run
	// refuses fast, naming the live holder and its renewal age: re-keying
	// the shared bundle under a running rotation locks the rotation's
	// follower control planes out of it, and a worker mint under one
	// republishes cluster-info from local PKI, rolling the published
	// old‖new trust back to what this node holds.
	// Injected for testing.
	maintenanceFence func() (*k8sutil.MaintenanceLease, error)
	newCertKey       func() (string, error)
	uploadCerts      func(certKeyHex string, owner *metav1.OwnerReference, confirm func(context.Context) error) error
	nodeIP           func() (string, error)
	// revoke takes back a token this handler minted, when a later step
	// fails before the caller ever sees it.
	revoke func(tok string, cause error) error
}

func defaultTokenDeps() tokenDeps {
	return tokenDepsFor(nodeClientBuilder(nil))
}

func tokenDepsFor(buildClient func() (kubernetes.Interface, error)) tokenDeps {
	return tokenDeps{
		// token.Create takes its client as a parameter: the MCP handler mints
		// through the node connector's client — built on this node's
		// admin.conf, the credential the CLI's token create uses — never
		// through the server's configurable connector.
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

// TokenCreateHandler mints a join token. Minting writes a bootstrap Secret,
// ClusterRoleBindings and the shared control-plane certificates, which the
// server's credential must never be able to write, so the mint rides the node
// connector, built on this node's super-admin.conf: the tier flag and root on
// this node are its controls.
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
		// Reject before multiplying. time.Duration is int64 nanoseconds, so
		// hours above this wrap negative -- and a non-positive TTL means
		// "never expires", so the overflow mints a permanent join credential
		// for a caller who asked for the opposite.
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

		// The join flags describe the cluster the joiner is about to join, so
		// they are derived BEFORE the mint, the ordering `stratum token
		// create` also applies: a cluster whose network configuration cannot
		// be read must fail with nothing minted, not with a live credential
		// for a join that would point the joining node's kubelet at the wrong
		// ranges and domain.
		flags, err := token.LoadJoinFlags()
		if err != nil {
			return nil, TokenCreateOut{}, fmt.Errorf("derive the join flags the result must carry: %w", err)
		}

		// The join URL is derived here, never taken from the caller. It is
		// published in cluster-info, so it is the address a joiner sends
		// this bootstrap credential to: a caller-supplied URL would let a
		// model point the next node at a host it controls, and the victim
		// is whoever follows the join instructions. An operator who needs a
		// load-balancer name uses `stratum token create --server`,
		// where the value comes from a person. The rule is netcfg's remote
		// grant rule — the VIP when this node records one, else this node's
		// public address, refusing every address GrantServerURL's doc
		// lists. `stratum token create` prints by the same rule, so both
		// front ends name the same endpoint.
		ip, err := deps.nodeIP()
		if err != nil {
			return nil, TokenCreateOut{}, fmt.Errorf("determine this node's IP for the join URL: %w", err)
		}
		serverURL, err := netcfg.GrantServerURL(ip)
		if err != nil {
			return nil, TokenCreateOut{}, fmt.Errorf("derive the join URL: %w", err)
		}

		// token.Issue owns the credential from the mint to the handover: it
		// mints, runs the certificate phase for a control-plane token, and
		// revokes the minted token unless this handover reports the
		// credential delivered. The mint takes the writer itself, so its
		// signing progress lands in a per-call buffer instead of a stdout
		// redirect — on the stdio transport stdout is the JSON-RPC stream.
		var lease *k8sutil.MaintenanceLease
		// Every mint republishes cluster trust from this node's local PKI
		// (token.Create → ensureClusterInfo), so the fence is not
		// control-plane-scoped: a worker mint from a controller the rotation
		// has not reached must refuse fast against a live maintenance run too.
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
			// Returning the result IS the delivery: the credential bytes
			// reach the caller the moment this returns and the call
			// succeeds, so the handover reports delivered and the owner
			// never revokes past this point.
			return true, nil
		})
		if ierr != nil {
			return nil, TokenCreateOut{}, ierr
		}
		return nil, out, nil
	}
}

// ----------------------------------------------------------- registry_apply

// RegistryApplyIn is registry_apply's (empty) input.
type RegistryApplyIn struct{}

// RegistryApplyOut is registry_apply's result.
type RegistryApplyOut struct {
	Configured bool     `json:"configured" jsonschema:"registries.yaml has mirrors or registry credentials/TLS, and they were applied"`
	Changed    bool     `json:"changed" jsonschema:"the containerd drop-in changed, so containerd must be restarted to pick it up"`
	Restarted  bool     `json:"restarted" jsonschema:"always false: this tool applies the configuration only and leaves any containerd restart to the operator"`
	Registries []string `json:"registries,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	Note       string   `json:"note"`
}

// RegistryApplyHandler regenerates containerd's registry configuration
// from registries.yaml.
func RegistryApplyHandler() sdk.ToolHandlerFor[RegistryApplyIn, RegistryApplyOut] {
	return func(context.Context, *sdk.CallToolRequest, RegistryApplyIn) (*sdk.CallToolResult, RegistryApplyOut, error) {
		if err := requireStratumHost(); err != nil {
			return nil, RegistryApplyOut{}, err
		}

		// registry.Apply prints nothing; the result goes through bare.
		res, err := registry.Apply()
		if err != nil {
			return nil, RegistryApplyOut{}, fmt.Errorf("apply registry configuration: %w", err)
		}

		out := RegistryApplyOut{
			Configured: res.Configured,
			Changed:    res.Changed,
			// This handler applies configuration only and never restarts
			// containerd (the changed note below says why), so Restarted is
			// false by construction: the result carries the accurate fact
			// instead of leaving a caller to infer a restart from Changed.
			Restarted:  false,
			Registries: res.Registries,
			Warnings:   res.Warnings,
		}
		switch {
		case !res.Configured:
			out.Note = registry.NothingConfigured + ". Nothing was applied. " + registry.DefaultIsNotAFault
		case res.Changed:
			// Saying this plainly matters: an agent that stops here will
			// report success while the node keeps pulling the old way.
			out.Note = "Configuration changed. containerd must be restarted before it takes effect — " +
				"this tool does not restart it, because that briefly interrupts every image pull on the node."
		default:
			out.Note = "Configuration already matched; nothing changed and no restart is needed."
		}
		return nil, out, nil
	}
}
