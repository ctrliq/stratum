// The engine's tool catalog, as data beside the handlers. pkg/cli's
// registerAll walks it through an unexported registrar method applying the
// same tier and duplicate-name checks as for contributed tool sets: the
// checks stay with the registrar, the rows live here, next to the handlers.

package mcp

import (
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ctrliq/stratum/internal/mcp/kube"
)

// Tier is how much damage a tool can do. It decides whether the tool is
// registered at all — the host registrar gates on it — not whether a call
// is allowed.
type Tier int

const (
	// TierRead cannot change anything. Always registered.
	TierRead Tier = iota
	// TierWrite mutates recoverably — a snapshot, a token, a config. The
	// host enables it with --allow-write.
	TierWrite
	// TierDestructive can take a cluster down or lose data. The host enables
	// it with --allow-destructive.
	TierDestructive
)

func (t Tier) String() string {
	switch t {
	case TierRead:
		return "read"
	case TierWrite:
		return "write"
	case TierDestructive:
		return "destructive"
	}
	return "unknown"
}

// CredentialSource names the Kubernetes credential a tool reaches the API
// through. Every catalog row declares its handler's source in Credential, so
// registration records which credential each admitted tool really rides and
// the least-privilege guide grants rights to exactly that credential — never
// to one a registration does not use.
type CredentialSource string

const (
	// CredentialConfigurableConnector marks a tool whose handler talks
	// through the connector the operator scopes with --kubeconfig, --context
	// and --request-timeout, so its RBAC bounds the tool.
	CredentialConfigurableConnector CredentialSource = "configurable connector"
	// CredentialNodeConnector marks a tool whose handler reaches the API
	// through this node's admin.conf — through the catalog's explicit node
	// connector, or through the node-credential client the handler's own
	// default dependencies build. RBAC neither enables nor limits it: the
	// tier flag and running the server as root are its controls.
	CredentialNodeConnector CredentialSource = "node connector"
	// CredentialNone marks a tool whose registration hands the handler no
	// Kubernetes client at all — the host-local tools. Their deeper
	// operations may still load admin.conf on their own.
	CredentialNone CredentialSource = "none"
)

// Tool is one row of the catalog: the name and description the client sees
// in tools/list, the tier that decides whether the tool registers at all,
// and Add, which adds the tool's typed handler once the host registrar has
// admitted the descriptor.
type Tool struct {
	Name        string
	Description string
	Tier        Tier

	// Destructive marks a write-tier row whose tool is annotated as
	// destructive: it registers with --allow-write, but its handler refuses
	// some targets unless the server also runs with --allow-destructive, so a
	// client should treat a call with the care a destructive annotation asks
	// for. Read- and destructive-tier rows never set it: the tier alone
	// already decides their annotation.
	Destructive bool

	// Credential declares which Kubernetes credential this tool's handler
	// reaches the API with: one of the CredentialSource values. Registration
	// records it beside the admitted tool, so a row that reaches the API
	// without declaring a source records an empty one, which pkg/cli's
	// credential-source pin fails on.
	Credential CredentialSource

	// Add adds the tool's typed handler to srv under t. The handler travels
	// with the row — built from the same version, connector and policy the
	// catalog was built with — so the registrar admits a descriptor without
	// ever naming a handler's type parameters.
	Add func(srv *sdk.Server, t *sdk.Tool)
}

// Catalog returns one descriptor per tool the engine ships, in registration
// order. version is what version_get reports; conn is the connector the
// generic Kubernetes-facing handlers talk through; nodeConn is the explicit
// node connector on admin.conf that the node-local handlers (token_list,
// upgrade_plan, upgrade_apply) talk through, carrying the server's
// --request-timeout; policy is the write policy the generic Kubernetes write
// tools enforce.
//
// Each row also declares, in Credential, which of those credentials its
// handler reaches the API with — including "none" for the host-local tools —
// and pkg/cli's registrar records that declaration for every tool it admits,
// which is what the least-privilege guide is held to.
//
// Names, descriptions and tiers are the shipped surface: pkg/cli's
// mcp_surface_test.go pins what actually reaches the client over the wire.
// A new tool adds its row here.
func Catalog(version string, conn, nodeConn *kube.Connector, policy WritePolicy) []Tool {
	buildClient := nodeClientBuilder(nodeConn)
	ops := destructiveDepsFor(buildClient)
	return []Tool{
		// The engine's own tool.
		{
			Name:        "version_get",
			Description: "Report the Stratum release, OS and architecture of the running binary.",
			Tier:        TierRead,
			Credential:  CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, VersionHandler(version))
			},
		},

		// The Stratum read toolset — the half a generic Kubernetes MCP
		// server cannot provide.
		{
			Name: "cluster_status",
			Description: "Report Stratum's supervised process table: what the supervisor intends to be running and " +
				"what actually is. This is the first tool to call when a node is misbehaving — " +
				"'wanted but not running' is not visible from the Kubernetes API at all.",
			Tier:       TierRead,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ClusterStatusHandler())
			},
		},
		{
			Name: "certs_check",
			Description: "Report expiry for every Stratum-managed certificate on this node, and which are close " +
				"enough to expiry to matter. An expired certificate takes the cluster down, so this is " +
				"worth checking before concluding a cluster is broken for some other reason.",
			Tier:       TierRead,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, CertsCheckHandler())
			},
		},
		{
			Name: "snapshot_list",
			Description: "List etcd snapshots, newest first, local and in the configured object store. " +
				"Call this before any restore to find out what is actually available.",
			Tier:       TierRead,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotListHandler())
			},
		},
		{
			Name: "token_list",
			Description: "List bootstrap token IDs and their expiry for the cluster THIS NODE belongs to. The secret " +
				"half of a token is never returned — it is a joining credential. Like upgrade_plan this tool " +
				"takes no context: bootstrap tokens are Secrets in kube-system, so " +
				"it reads them through the node's own admin.conf, and the server's --request-timeout " +
				"bounds the call.",
			Tier:       TierRead,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, TokenListHandler(nodeConn))
			},
		},
		{
			Name: "registry_check",
			Description: "Report this node's container registry configuration: mirrors, and which registries have " +
				"credentials or disabled TLS verification. Reports configuration, not reachability, and " +
				"never returns credential values.",
			Tier:       TierRead,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, RegistryCheckHandler())
			},
		},
		{
			Name: "upgrade_plan",
			Description: "Preview a rolling cluster upgrade to a target version: the preflight verdict, and the " +
				"exact order nodes would be upgraded in. Changes nothing. If the preflight refuses the " +
				"upgrade, this reports why. Takes no context: it reads this node's own admin.conf, so it " +
				"previews the same cluster upgrade_apply rolls.",
			Tier:       TierRead,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, UpgradePlanHandler(nodeConn))
			},
		},
		{
			Name: "snapshot_restore_copies",
			Description: "List the etcd data copies snapshot_restore kept beside the datastore: the data from before " +
				"each restore, and what each failed restore wrote. Each is a full copy of etcd's data on " +
				"the datastore's own disk. Changes nothing.",
			Tier:       TierRead,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotRestoreCopiesHandler())
			},
		},
		{
			Name: "uninstall_preview",
			Description: "Report exactly which paths an uninstall would delete from this node and which of them " +
				"exist right now. Changes nothing. Read this before ever considering the uninstall tool.",
			Tier:       TierRead,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, UninstallPreviewHandler())
			},
		},

		// The generic Kubernetes read toolset.
		{
			Name: "context_list",
			Description: "List the clusters this server can address. Call this first when more than one cluster " +
				"may be in play: every other tool takes a context argument and this reports the valid values.",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ContextListHandler(conn))
			},
		},
		{
			Name: "resource_list",
			Description: "List any resource the cluster serves, including custom resources. Accepts a plural name " +
				"(pods), singular (pod), short name (po), Kind (Pod) or group-qualified name " +
				"(deployments.apps). Returns summaries; use resource_get for one object in full. " +
				"Prefer pod_list, node_list or namespace_list where they apply — they report the " +
				"status columns this one does not.",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ResourceListHandler(conn))
			},
		},
		{
			Name: "resource_get",
			Description: "Read one object in full, exactly as the API server returns it. Use this after a list " +
				"has identified the object worth looking at.",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ResourceGetHandler(conn))
			},
		},
		{
			Name: "pod_list",
			Description: "List pods with the columns a diagnosis needs: phase, ready count, restarts, node, and " +
				"the waiting reason for a pod that is not running (CrashLoopBackOff, ImagePullBackOff).",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, PodListHandler(conn))
			},
		},
		{
			Name: "pod_get",
			Description: "Read one pod, resolved into per-container state and pod conditions — why each container " +
				"is waiting or what it terminated with.",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, PodGetHandler(conn))
			},
		},
		{
			Name: "pod_logs",
			Description: "Read a container's logs. Returns the most recent lines and says so when older output " +
				"was dropped. Use previous=true to read a container that has already crashed.",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, PodLogsHandler(conn))
			},
		},
		{
			Name: "event_list",
			Description: "List events, most recent first. Set warnings_only to see just the failures, which is " +
				"usually the fastest way to find out why something is not working.",
			Tier:       TierRead,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, EventListHandler(conn))
			},
		},
		{
			Name:        "namespace_list",
			Description: "List namespaces and whether each is Active or Terminating.",
			Tier:        TierRead,
			Credential:  CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, NamespaceListHandler(conn))
			},
		},
		{
			Name:        "node_list",
			Description: "List nodes with readiness, roles, schedulability, kubelet version and internal IP.",
			Tier:        TierRead,
			Credential:  CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, NodeListHandler(conn))
			},
		},

		// The generic Kubernetes write toolset. resource_apply and
		// resource_delete register at the write tier but are annotated
		// destructive: their handlers refuse some targets unless the server
		// also runs with --allow-destructive.
		{
			Name: "resource_apply",
			Description: "Creates or replaces an object from a complete manifest, using server-side apply. Fields " +
				"this server set before and the manifest now omits are REMOVED, so apply a whole " +
				"object, not a fragment. Pass dry_run=true first to have the API server validate the " +
				"manifest against the schema and the admission chain without persisting it. Secrets, " +
				"RBAC, webhook and admission-policy configurations, node objects and other identity " +
				"objects are refused as a matter of policy, as are HelmCharts, HelmChartConfigs and " +
				"host-escaping pod specs. Creating or changing a namespace or a custom resource " +
				"definition additionally requires the server to have been started with " +
				"--allow-destructive.",
			Tier:        TierWrite,
			Destructive: true,
			Credential:  CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ResourceApplyHandler(conn, policy))
			},
		},
		{
			Name: "resource_scale",
			Description: "Set a workload's replica count. Works on deployments, statefulsets, replicasets and any " +
				"custom resource serving a scale subresource.",
			Tier:       TierWrite,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ResourceScaleHandler(conn, policy))
			},
		},
		{
			Name: "resource_delete",
			Description: "DELETES one named object. This cannot be undone. Deleting a workload stops it; deleting an " +
				"object with dependents cascades to them. Pass dry_run=true to confirm the target first. " +
				"Namespaces, persistent volumes and claims, nodes and custom resource definitions " +
				"additionally require the server to have been started with --allow-destructive.",
			Tier:        TierWrite,
			Destructive: true,
			Credential:  CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, ResourceDeleteHandler(conn, policy))
			},
		},

		// The Stratum write toolset: mutating but recoverable.
		{
			Name: "snapshot_save",
			Description: "Take an etcd snapshot now, copying it to the configured object store unless skip_s3 is set. " +
				"Worth doing before any change you might want to undo.",
			Tier:       TierWrite,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotSaveHandler())
			},
		},
		{
			Name: "snapshot_schedule",
			Description: "Set how many scheduled snapshots each node keeps. The object store snapshots are copied to " +
				"is reported but cannot be changed here: a snapshot is the whole cluster, so where it is " +
				"sent is an operator decision made in the config file or the environment.",
			Tier:       TierWrite,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotScheduleHandler())
			},
		},
		{
			Name: "snapshot_prune",
			Description: "Apply the retention policy to this node's scheduled snapshots, keeping the newest `keep` of " +
				"them. keep must be at least 1.",
			Tier:       TierWrite,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotPruneHandler())
			},
		},
		{
			Name: "token_create",
			Description: "Mint a join token so a new node can join this cluster. The token is returned once and is " +
				"never logged — treat it as a credential. Set control_plane to mint a control-plane join " +
				"token instead; it can read the shared certificate material, which this uploads, so it is " +
				"never for a worker join. The joinFlags it returns become --pod-cidr, --service-cidr and " +
				"--cluster-domain on a worker join line only: a control plane joining with --join takes the " +
				"cluster's network, and stratum up refuses those flags without --worker. --profile goes on " +
				"every join line.",
			Tier:       TierWrite,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, tokenCreateWith(tokenDepsFor(buildClient)))
			},
		},
		{
			Name: "registry_apply",
			Description: "Regenerate containerd's registry configuration from this node's existing registries.yaml. " +
				"Takes no configuration itself. Reports whether containerd needs restarting.",
			Tier:       TierWrite,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, RegistryApplyHandler())
			},
		},

		// The Stratum destructive toolset.
		{
			Name: "upgrade_apply",
			Description: "DRAINS AND UPGRADES nodes, restarting their workloads. scope=cluster rolls every node in " +
				"the cluster one at a time and takes control planes down in turn; scope=node does this " +
				"node only. Run upgrade_plan first — it previews the same cluster this rolls, reading " +
				"this node's own admin.conf. scope=cluster runs on that admin.conf and refuses any " +
				"cluster but this node's own, so --kubeconfig and --context cannot point it elsewhere.",
			Tier:       TierDestructive,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, UpgradeApplyHandler(nodeConn))
			},
		},
		{
			Name: "rollback",
			Description: "REVERTS this node to its previous version, restarting its components. If etcd_snapshot is " +
				"given it also restores etcd, which permanently discards every change made since that " +
				"snapshot.",
			Tier:       TierDestructive,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, RollbackHandler())
			},
		},
		{
			Name: "certs_rotate",
			Description: "RESTARTS this node's control-plane components to pick up renewed leaf certificates. Brief " +
				"API unavailability on a single-control-plane cluster. Does not touch the CAs.",
			Tier:       TierDestructive,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, CertsRotateHandler(conn, nodeConn))
			},
		},
		{
			Name: "certs_rotate_ca",
			Description: "REPLACES THE CLUSTER'S CERTIFICATE AUTHORITY, one phase at a time. Running a phase out of " +
				"order, or before the previous finished on every node, leaves nodes that no longer " +
				"trust each other. Phases are trust-new, sign-new, finalize, in that order.",
			Tier:       TierDestructive,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, certsRotateCAWith(ops, conn, nodeConn))
			},
		},
		{
			Name: "certs_migrate_etcd_ca",
			Description: "RESTARTS ETCD ON EVERY CONTROL PLANE as it moves etcd onto its own CA, one phase at a " +
				"time. A mistimed phase can leave etcd members unable to authenticate to each other, " +
				"which is a quorum outage. Phases are trust-both, issue-new, drop-root, in that " +
				"order; rollback is the way back, not a fourth step.",
			Tier:       TierDestructive,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, certsMigrateEtcdCAWith(ops))
			},
		},
		{
			Name: "certs_migrate_kubelet_client_ca",
			Description: "RESTARTS THE KUBELET ON EVERY NODE AND THE API SERVER ON EVERY CONTROL PLANE as it moves " +
				"the kubelets' client trust off the cluster root onto its own CA. A phase run out of order " +
				"can leave the API servers unable to reach the kubelets (no logs, exec or port-forward). " +
				"Phases are trust-both, issue-new, drop-root, in that order; run it with cluster.",
			Tier:       TierDestructive,
			Credential: CredentialNodeConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, certsMigrateKubeletCAWith(ops))
			},
		},
		{
			Name: "snapshot_restore",
			Description: "WIPES THIS NODE'S ETCD and re-seeds it from a snapshot, discarding everything written " +
				"since that snapshot was taken. The other control planes are left holding the old data " +
				"and the cluster is split until each one is rejoined with the CLI, `stratum snapshot " +
				"rejoin --yes --force`; snapshot_rejoin cannot override its quorum check. Check " +
				"snapshot_list first.",
			Tier:       TierDestructive,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotRestoreHandler())
			},
		},
		{
			Name: "snapshot_prune_restores",
			Description: "PERMANENTLY REMOVES the etcd data copies you name, each exactly as snapshot_restore_copies " +
				"reported it. A pre-restore copy may be the only copy of the data from before a restore, " +
				"so remove one only once the cluster is verified. A copy that changed since the listing " +
				"is left.",
			Tier:       TierDestructive,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotPruneRestoresHandler())
			},
		},
		{
			Name: "snapshot_rejoin",
			Description: "WIPES THIS NODE'S ETCD DATA unless it is already a member of the restored cluster that " +
				"can be resumed, and rejoins it to a cluster that was restored on another node. A resumed " +
				"member keeps its data. Only meaningful after snapshot_restore ran on that seed node. Before " +
				"a wipe that could lose data it refuses unless this node's own etcd cluster is confirmed to " +
				"have lost quorum, and it cannot override that: use the CLI, `stratum snapshot rejoin --yes " +
				"--force`, when the data is to be discarded.",
			Tier:       TierDestructive,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, SnapshotRejoinHandler())
			},
		},
		{
			Name: "ha_repoint",
			Description: "REPOINTS THIS NODE'S CLIENTS at the cluster VIP, restarting local components. With " +
				"cilium=true it also repoints Cilium ACROSS THE WHOLE CLUSTER from this one node — a " +
				"once-per-cluster operation that affects every node's networking. The cluster-wide part " +
				"runs on the server's own credential and default context, which RBAC bounds, and refuses " +
				"any cluster but this node's own.",
			Tier:       TierDestructive,
			Credential: CredentialConfigurableConnector,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, HARepointHandler(conn, nodeConn))
			},
		},
		{
			Name: "uninstall",
			Description: "PERMANENTLY REMOVES STRATUM FROM THIS NODE, including its etcd data and PKI. This cannot " +
				"be undone and no snapshot brings the node back. On a control plane it also costs the " +
				"cluster a member, which may break quorum. Run uninstall_preview first.",
			Tier:       TierDestructive,
			Credential: CredentialNone,
			Add: func(srv *sdk.Server, t *sdk.Tool) {
				sdk.AddTool(srv, t, UninstallHandler())
			},
		},
	}
}
