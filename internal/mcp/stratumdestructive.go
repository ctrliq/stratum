package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/certs"
	clustercfg "github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/uninstall"
	"github.com/ctrliq/stratum/internal/upgrade"
	"github.com/ctrliq/stratum/internal/version"
)

// Registered only with --allow-destructive.

type destructiveOut struct {
	Action   string   `json:"action"`
	Done     bool     `json:"done"`
	Progress string   `json:"progress,omitempty" jsonschema:"output from the subsystem that performed the work"`
	Next     string   `json:"next,omitempty" jsonschema:"what the operator must do next, when the procedure is not self-completing"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"steps that did not complete while the work itself succeeded; a result carrying them is partial, so done is false"`
}

// Operations rewrite the same host state, so no two may run at once; opMu is their sole exclusion.
var opMu sync.Mutex

// run holds opMu and buffers progress; on the stdio transport os.Stdout is the JSON-RPC stream.
func run(action string, fn func(io.Writer) ([]string, error)) (destructiveOut, error) {
	if err := requireStratumHost(); err != nil {
		return destructiveOut{}, err
	}
	opMu.Lock()
	defer opMu.Unlock()

	var buf bytes.Buffer
	warnings, err := fn(&buf)
	out := destructiveOut{Action: action, Progress: strings.TrimSpace(buf.String()), Warnings: warnings}
	switch {
	case err != nil:
		// A half-finished destructive procedure is when the operator most needs the progress.
		return out, fmt.Errorf("%s failed after the following output:\n%s\n\n%w", action, out.Progress, err)
	case len(out.Warnings) > 0:
		out.Done = false
	default:
		out.Done = true
	}
	return out, nil
}

// DestructiveOut is the result shape every destructive tool returns.
type DestructiveOut = destructiveOut

// RunExclusive runs fn as a destructive operation: host check, operation lock, shared result.
func RunExclusive(action string, fn func(io.Writer) ([]string, error)) (DestructiveOut, error) {
	return run(action, fn)
}

// Operations may rewrite kubeconfigs the connectors cache, so drop the cache however the run ends.
func invalidateConnectors(connectors ...*kube.Connector) {
	for _, conn := range connectors {
		if conn != nil {
			conn.Invalidate()
		}
	}
}

// Handlers call no operation except through it; each writes progress to its io.Writer, not stdout.
type destructiveDeps struct {
	upgradeNode             func(io.Writer, upgrade.NodeOptions) error
	upgradeCluster          func(io.Writer, upgrade.ClusterOptions) error
	rollbackNode            func(io.Writer, upgrade.RollbackOptions) error
	certsRotate             func(context.Context, io.Writer, certs.Options) (bool, error)
	rotateCA                func(io.Writer, certs.CARotateOptions) error
	rotateCACluster         func(context.Context, io.Writer, certs.CARotateOptions) error
	migrateEtcdCA           func(io.Writer, certs.EtcdCAOptions) error
	migrateEtcdCACluster    func(context.Context, io.Writer, certs.EtcdCAOptions) error
	migrateKubeletCA        func(io.Writer, certs.KubeletCAOptions) error
	migrateKubeletCACluster func(context.Context, io.Writer, certs.KubeletCAOptions) error
	restoreLocalSeed        func(io.Writer, string) error
	removeRestoreCopies     func([]etcdsnap.RestoreCopy) ([]etcdsnap.RestoreCopy, error)
	rejoinEtcd              func(context.Context, io.Writer, string, bool) error
	nodeRepoint             func(io.Writer, string) ([]string, error)
	repointCilium           func(context.Context, io.Writer, kubernetes.Interface, string) ([]string, error)
	uninstallRun            func(io.Writer, bool) error
}

// Reloads per resolution: a CA rotation reissues the embedded credentials between phases.
func nodeClientBuilder(conn *kube.Connector) func() (kubernetes.Interface, error) {
	if conn == nil {
		conn = kube.NewConnector(kube.Options{KubeconfigPath: k8sutil.InternalAdminConf})
	}
	return func() (kubernetes.Interface, error) {
		cl, err := conn.ReloadCluster("")
		if err != nil {
			return nil, err
		}
		return cl.Typed, nil
	}
}

func defaultDestructiveDeps() destructiveDeps {
	return destructiveDepsFor(nodeClientBuilder(nil))
}

func destructiveDepsFor(buildClient func() (kubernetes.Interface, error)) destructiveDeps {
	return destructiveDeps{
		upgradeNode: func(w io.Writer, o upgrade.NodeOptions) error {
			return upgrade.UpgradeNode(w, o)
		},
		upgradeCluster: func(w io.Writer, o upgrade.ClusterOptions) error {
			return upgrade.UpgradeCluster(w, o)
		},
		rollbackNode: func(w io.Writer, o upgrade.RollbackOptions) error {
			return upgrade.RollbackNode(w, o)
		},
		certsRotate: func(ctx context.Context, w io.Writer, o certs.Options) (bool, error) {
			return certs.RotateContext(ctx, w, o)
		},
		rotateCA: certs.RotateCA,
		// Cluster orchestrators get the node connector's builder (admin.conf), not the server's connector.
		rotateCACluster: func(ctx context.Context, w io.Writer, o certs.CARotateOptions) error {
			return certs.RotateCAClusterContext(ctx, w, o, buildClient)
		},
		migrateEtcdCA: certs.MigrateEtcdCA,
		migrateEtcdCACluster: func(ctx context.Context, w io.Writer, o certs.EtcdCAOptions) error {
			return certs.MigrateEtcdCAClusterContext(ctx, w, o, buildClient)
		},
		migrateKubeletCA: certs.MigrateKubeletClientCA,
		migrateKubeletCACluster: func(ctx context.Context, w io.Writer, o certs.KubeletCAOptions) error {
			return certs.MigrateKubeletClientCAClusterContext(ctx, w, o, buildClient)
		},
		restoreLocalSeed: func(w io.Writer, path string) error {
			return etcdsnap.RestoreLocalSeed(w, path)
		},
		removeRestoreCopies: etcdsnap.RemoveRestoreCopies,
		rejoinEtcd:          ha.RejoinEtcd,
		nodeRepoint: func(w io.Writer, vip string) ([]string, error) {
			return ha.NodeRepoint(w, vip)
		},
		repointCilium: func(ctx context.Context, w io.Writer, cs kubernetes.Interface, vip string) ([]string, error) {
			return ha.RepointCiliumWith(ctx, w, cs, vip)
		},
		uninstallRun: func(w io.Writer, force bool) error {
			return uninstall.Run(w, force)
		},
	}
}

// Package var so a test can install recording or failing operations for a call's length.
var destructiveOps = defaultDestructiveDeps()

// UpgradeApplyIn is upgrade_apply's input.
type UpgradeApplyIn struct {
	Scope                 string `json:"scope" jsonschema:"node to upgrade only this node, or cluster to roll every node in the cluster"`
	Target                string `json:"target,omitempty" jsonschema:"Kubernetes version as vMAJOR.MINOR.PATCH; defaults to the version this binary carries"`
	From                  string `json:"from,omitempty" jsonschema:"for scope=node, only the currently running binary is accepted; run a different staged binary directly with its CLI. Required for scope=cluster: the name of a binary an operator staged in /var/lib/stratum/upgrade (or its full path there), which must exist at that same path on every node"`
	WorkersMaxUnavailable int    `json:"workers_max_unavailable,omitempty" jsonschema:"how many workers may be upgraded at once; defaults to 1"`
	SoakSeconds           int    `json:"soak_seconds,omitempty" jsonschema:"seconds to hold each node healthy before moving on; defaults to 15, and 0 means the default rather than no soak"`
	SkipSnapshot          bool   `json:"skip_snapshot,omitempty" jsonschema:"skip the pre-upgrade etcd snapshot; strongly discouraged"`
}

// UpgradeApplyHandler: a cluster rollout runs on conn's credential, whose RBAC bounds it.
func UpgradeApplyHandler(conn *kube.Connector) sdk.ToolHandlerFor[UpgradeApplyIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in UpgradeApplyIn) (*sdk.CallToolResult, destructiveOut, error) {
		switch in.Scope {
		case "node":
			out, err := run("upgrade node", func(w io.Writer) ([]string, error) {
				return nil, destructiveOps.upgradeNode(w, upgrade.NodeOptions{
					Target: in.Target, From: in.From,
					SoakSec: in.SoakSeconds, SkipSnapshot: in.SkipSnapshot,
				})
			})
			if err == nil {
				out.Next = "This node only. Other nodes are still on the old version; a cluster is " +
					"supported skewed but should not be left that way."
			}
			return nil, out, err

		case "cluster":
			if in.From == "" {
				return nil, destructiveOut{}, fmt.Errorf(
					"scope=cluster requires from: the new stratum binary must already be staged in %s, "+
						"at that same path on every node, because each node upgrades itself from it",
					version.UpgradeStageDir)
			}
			from, err := stagedUpgradeBinary(in.From)
			if err != nil {
				return nil, destructiveOut{}, err
			}
			if err := requireStratumHost(); err != nil {
				return nil, destructiveOut{}, err
			}
			cs, err := clusterOfThisNode(ctx, conn, repointNodeCredential)
			if err != nil {
				return nil, destructiveOut{}, err
			}
			target := in.Target
			if target == "" {
				target = version.K8sVersion
			}
			out, err := run("upgrade cluster", func(w io.Writer) ([]string, error) {
				return nil, destructiveOps.upgradeCluster(w, upgrade.ClusterOptions{
					Target: target, From: from,
					WorkersMaxUnavailable: in.WorkersMaxUnavailable,
					SoakSec:               in.SoakSeconds, SkipSnapshot: in.SkipSnapshot,
					VerifyStagedOnNode: true,
					Client:             cs,
				})
			})
			return nil, out, err

		case "":
			return nil, destructiveOut{}, fmt.Errorf(
				"scope is required: \"node\" upgrades this node, \"cluster\" drains and upgrades every " +
					"node. There is no default, because the difference is a whole cluster")
		default:
			return nil, destructiveOut{}, fmt.Errorf("unknown scope %q: want \"node\" or \"cluster\"", in.Scope)
		}
	}
}

// thisNodeName is a var for tests; the kubelet registers a lowercased hostname, so lowercase here.
var thisNodeName = func() (string, error) {
	st, err := nodestate.LoadState()
	if err != nil {
		return "", fmt.Errorf("read this node's state: %w", err)
	}
	if st.Hostname == "" {
		return "", fmt.Errorf("this node's state records no node name")
	}
	return strings.ToLower(st.Hostname), nil
}

// localCluster is a var for tests; the reference the server's credential is checked against.
var localCluster = func() (kubernetes.Interface, string, error) {
	cfg, err := k8sutil.AdminRESTConfig()
	if err != nil {
		return nil, "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", err
	}
	return cs, cfg.Host, nil
}

// The refusal's remediation advice differs by which connector the handler holds.
const (
	repointByFlags        = "point --kubeconfig and --context at it"
	repointNodeCredential = "the node credential is this node's admin.conf, which --kubeconfig and --context cannot move; check what /etc/kubernetes/admin.conf addresses"
)

// clusterOfThisNode checks the credential's cluster is this node's own before cluster-wide work.
func clusterOfThisNode(ctx context.Context, conn *kube.Connector, remediation string) (kubernetes.Interface, error) {
	cl, err := cluster(conn, "")
	if err != nil {
		return nil, err
	}
	local, localHost, err := localCluster()
	if err != nil {
		return nil, fmt.Errorf("read this node's own cluster through super-admin.conf, to check the MCP credential addresses it: %w", err)
	}
	if err := requireThisNodeIn(ctx, cl.Typed, redactHost(cl.Host), local, redactHost(localHost), remediation); err != nil {
		return nil, err
	}
	return cl.Typed, nil
}

// Refuses unless cs's cluster is local's; a matching node name alone is not proof.
func requireThisNodeIn(ctx context.Context, cs kubernetes.Interface, host string, local kubernetes.Interface, localHost string, remediation string) error {
	name, err := thisNodeName()
	if err != nil {
		return err
	}
	if _, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("this node, %s, is not in the cluster the MCP credential addresses (%s); this "+
				"node's cluster is at %s. A cluster-wide operation runs only on this node's own cluster: "+
				"%s", name, host, localHost, remediation)
		}
		return fmt.Errorf("find this node, %s, in the cluster the MCP credential addresses (%s): %w", name, host, err)
	}
	theirs, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read kube-system in the cluster the MCP credential addresses (%s), to check it is "+
			"this node's cluster: %w", host, err)
	}
	ours, err := local.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read kube-system in this node's own cluster (%s): %w", localHost, err)
	}
	if theirs.UID != ours.UID {
		return fmt.Errorf("the cluster the MCP credential addresses (%s) is not this node's cluster (%s), "+
			"though both have a node named %s: a cluster-wide operation runs only on this node's own "+
			"cluster; %s", host, localHost, name, remediation)
	}
	return nil
}

// redactHost is a server URL without any user information it carries.
func redactHost(host string) string {
	u, err := url.Parse(host)
	if err != nil || u.User == nil {
		return host
	}
	u.User = nil
	return u.String()
}

// upgradeStageOwner is the owner a staged binary must have, a var for tests.
var upgradeStageOwner uint32

// The model may only run a binary an operator staged, never name another program to run as root.
func stagedUpgradeBinary(from string) (string, error) {
	dir := filepath.Clean(version.UpgradeStageDir)
	path := from
	if !strings.Contains(from, "/") {
		path = filepath.Join(dir, from)
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) != dir || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return "", fmt.Errorf("from %q is not a binary in %s: a cluster upgrade through MCP runs only a "+
			"binary an operator staged there. Stage it in %s on every node, or run `stratum upgrade "+
			"--cluster --from` from a shell", from, dir, dir)
	}
	if err := checkStaged(dir, true); err != nil {
		return "", err
	}
	if err := checkStaged(path, false); err != nil {
		return "", err
	}
	return path, nil
}

// checkStaged refuses a symlink, group or other write, or a non-root owner.
func checkStaged(path string, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("staged upgrade binary: %w", err)
	}
	switch {
	case dir && !fi.IsDir():
		return fmt.Errorf("staged upgrade binary: %s is not a directory", path)
	case !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("staged upgrade binary: %s is not a regular file (a symlink is not accepted)", path)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("staged upgrade binary: %s is writable by group or others (mode %v); "+
			"only its owner may write it", path, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != upgradeStageOwner {
		return fmt.Errorf("staged upgrade binary: %s is not owned by root", path)
	}
	return nil
}

// RollbackIn is rollback's input.
type RollbackIn struct {
	SoakSeconds  int    `json:"soak_seconds,omitempty" jsonschema:"seconds to hold the node healthy before declaring the rollback good; defaults to 15, and 0 means the default rather than no soak"`
	EtcdSnapshot string `json:"etcd_snapshot,omitempty" jsonschema:"also restore etcd from this snapshot; a name as snapshot_list reports it, or an absolute path. Single control plane only, and it discards every change since the snapshot"`
}

// RollbackHandler uses this node's super-admin.conf: RBAC neither enables nor limits it.
func RollbackHandler() sdk.ToolHandlerFor[RollbackIn, destructiveOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in RollbackIn) (*sdk.CallToolResult, destructiveOut, error) {
		snapshot := in.EtcdSnapshot
		if snapshot != "" {
			var err error
			if snapshot, err = resolveSnapshotPath(snapshot); err != nil {
				return nil, destructiveOut{}, err
			}
		}

		out, err := run("rollback node", func(w io.Writer) ([]string, error) {
			return nil, destructiveOps.rollbackNode(w, upgrade.RollbackOptions{
				SoakSec: in.SoakSeconds, EtcdSnapshot: snapshot,
			})
		})
		if err == nil && in.EtcdSnapshot != "" {
			out.Next = "etcd was restored from a snapshot: every change made after that snapshot is gone. " +
				"Check that workloads created since then are still expected to exist.\n\n" +
				"The rollback restarted what holds a watch on this node and in the cluster, and, if it could not " +
				"restart something, named it in the progress above. What is left on the other nodes:\n" +
				etcdsnap.StillToDo(false)
		}
		return nil, out, err
	}
}

// CertsRotateIn is certs_rotate's input.
type CertsRotateIn struct {
	IfExpiringDays int `json:"if_expiring_days,omitempty" jsonschema:"only rotate when a leaf is within this many days of expiry; omit to rotate regardless"`
}

// CertsRotateHandler rewrites PKI on this node using its own super-admin.conf credentials.
func CertsRotateHandler(conn ...*kube.Connector) sdk.ToolHandlerFor[CertsRotateIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsRotateIn) (*sdk.CallToolResult, destructiveOut, error) {
		// Rotation rewrites kubeconfigs the server's connectors cache, so drop the cache however it ends.
		defer invalidateConnectors(conn...)
		// Not Quiet: the printed output is the only proof a real rotation happened.
		out, err := run("rotate leaf certificates", func(w io.Writer) ([]string, error) {
			_, err := destructiveOps.certsRotate(ctx, w, certs.Options{IfExpiringDays: in.IfExpiringDays})
			return nil, err
		})
		if err == nil {
			out.Next = "Leaves only. The CAs are untouched — certs_check reports which certificates " +
				"remain close to expiry, and a CA needs certs_rotate_ca. Read progress to see " +
				"whether anything was reissued: with if_expiring_days set, a run that found nothing " +
				"close to expiry also succeeds."
		}
		return nil, out, err
	}
}

// CertsRotateCAIn is certs_rotate_ca's input.
type CertsRotateCAIn struct {
	Phase   string `json:"phase,omitempty" jsonschema:"one node: trust-new, sign-new or finalize — the phases run in that order and each must complete cluster-wide before the next; leave empty with cluster"`
	Cluster bool   `json:"cluster,omitempty" jsonschema:"run every phase across every node, as the CLI's --cluster does; leave phase empty"`
	Force   bool   `json:"force,omitempty" jsonschema:"rotate even though an old CA or service-account key is gone; the old CA certificates (ca.crt, front-proxy-ca.crt, and kubelet-client-ca.crt where the kubelets have their own client CA) and the service-account public key (sa.pub, or sa.key to derive it) are still required; for compromise recovery only"`

	RestartWorkloads  bool `json:"restart_workloads,omitempty" jsonschema:"cluster only: restart the Deployments, DaemonSets and StatefulSets that use in-cluster API credentials once the new CA is published, before sign-new; without it a cluster rotation refuses to start when there are any"`
	SkipWorkloadCheck bool `json:"skip_workload_check,omitempty" jsonschema:"do not check the workloads that use in-cluster API credentials; they fail with x509 errors from sign-new until restarted"`
	SkipTokenCheck    bool `json:"skip_token_check,omitempty" jsonschema:"finalize without checking that no mounted ServiceAccount token is still signed by the old key, which gets 401 once the old key is dropped; for compromise recovery, so it needs force"`
}

// caPhases is shared with the CLI via internal/certs so the two cannot drift.
var caPhases = certs.CARotatePhases

// Connectors are invalidated after every phase; a phase may rewrite their kubeconfig inputs.
func CertsRotateCAHandler(conn ...*kube.Connector) sdk.ToolHandlerFor[CertsRotateCAIn, destructiveOut] {
	return certsRotateCAWith(destructiveOps, conn...)
}

func certsRotateCAWith(deps destructiveDeps, conn ...*kube.Connector) sdk.ToolHandlerFor[CertsRotateCAIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsRotateCAIn) (*sdk.CallToolResult, destructiveOut, error) {
		// Cancellation is checked before validation, so a canceled request fails with its cause.
		if err := ctx.Err(); err != nil {
			invalidateConnectors(conn...)
			return nil, destructiveOut{}, err
		}
		// The rules and their wording are the CLI validator's, in the CLI's flag names.
		f := certs.CARotateFlags{
			Phase: in.Phase, Cluster: in.Cluster, Force: in.Force,
			RestartWorkloads: in.RestartWorkloads, SkipWorkloadCheck: in.SkipWorkloadCheck,
			SkipTokenCheck: in.SkipTokenCheck,
		}
		if err := f.Validate(); err != nil {
			return nil, destructiveOut{}, err
		}
		opts := f.Options()
		opts.Quiet = true
		if err := opts.CheckWorkloadFlags(); err != nil {
			return nil, destructiveOut{}, err
		}
		action := "rotate CA phase " + in.Phase
		if in.Cluster {
			action = "rotate CA across the cluster"
		}
		// From here the phase may rewrite PKI, so drop the connector caches however it ends.
		defer invalidateConnectors(conn...)
		out, err := run(action, func(w io.Writer) ([]string, error) {
			if in.Cluster {
				return nil, deps.rotateCACluster(ctx, w, opts)
			}
			return nil, deps.rotateCA(w, opts)
		})
		if err == nil {
			if in.Cluster {
				out.Next = certs.CARotateClusterWarning
			} else {
				out.Next = certs.CARotatePhaseWarning + caRotationNext(in.Phase)
			}
		}
		return nil, out, err
	}
}

// CertsMigrateEtcdCAIn is certs_migrate_etcd_ca's input.
type CertsMigrateEtcdCAIn struct {
	Phase   string `json:"phase" jsonschema:"one node: trust-both, then issue-new, then drop-root, in that order; rollback is not a fourth step but the way back, un-migrating a node instead of advancing it; leave empty with cluster"`
	Cluster bool   `json:"cluster,omitempty" jsonschema:"run every phase across every control plane, as the CLI's --cluster does; leave phase empty"`
	Resume  bool   `json:"resume,omitempty" jsonschema:"proceed on a cluster whose nodes are at different phases"`
	Force   bool   `json:"force,omitempty" jsonschema:"rollback only: un-migrate a node that already completed drop-root"`
}

// CertsMigrateEtcdCAHandler rewrites etcd PKI on this node using its own super-admin.conf.
func CertsMigrateEtcdCAHandler() sdk.ToolHandlerFor[CertsMigrateEtcdCAIn, destructiveOut] {
	return certsMigrateEtcdCAWith(destructiveOps)
}

func certsMigrateEtcdCAWith(deps destructiveDeps) sdk.ToolHandlerFor[CertsMigrateEtcdCAIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsMigrateEtcdCAIn) (*sdk.CallToolResult, destructiveOut, error) {
		// Cancellation is checked before validation, so a canceled request fails with its cause.
		if err := ctx.Err(); err != nil {
			return nil, destructiveOut{}, err
		}
		// The validator owns every rule, in the CLI's words.
		f := certs.EtcdCAFlags{Cluster: in.Cluster, Phase: in.Phase, Resume: in.Resume, Force: in.Force}
		if err := f.Validate(); err != nil {
			return nil, destructiveOut{}, err
		}
		opts := f.Options()
		opts.Quiet = true
		action := "migrate etcd CA phase " + in.Phase
		if in.Cluster {
			action = "migrate the etcd CA across the cluster"
		}
		out, err := run(action, func(w io.Writer) ([]string, error) {
			if in.Cluster {
				return nil, deps.migrateEtcdCACluster(ctx, w, opts)
			}
			return nil, deps.migrateEtcdCA(w, opts)
		})
		// Only a single-node forward phase leaves phases to run.
		if err == nil && !in.Cluster && in.Phase != "drop-root" && in.Phase != "rollback" {
			out.Next = "The migration is not finished. Run the remaining phases in order on every " +
				"control plane; etcd restarts at each one."
		}
		return nil, out, err
	}
}

// CertsMigrateKubeletClientCAIn is certs_migrate_kubelet_client_ca's input.
type CertsMigrateKubeletClientCAIn struct {
	Cluster  bool   `json:"cluster,omitempty" jsonschema:"run every phase on every node, control planes first (recommended)"`
	Rollback bool   `json:"rollback,omitempty" jsonschema:"with cluster: move every kubelet back to trusting the cluster root"`
	Phase    string `json:"phase,omitempty" jsonschema:"run one phase on this node instead: trust-both, issue-new, drop-root, or re-add-root, reissue-root, remove-ca to undo them"`
	Force    bool   `json:"force,omitempty" jsonschema:"rollback only: re-add the cluster root to kubelets that completed drop-root"`
}

// CertsMigrateKubeletClientCAHandler moves kubelet client trust via this node's super-admin.conf.
func CertsMigrateKubeletClientCAHandler() sdk.ToolHandlerFor[CertsMigrateKubeletClientCAIn, destructiveOut] {
	return certsMigrateKubeletCAWith(destructiveOps)
}

func certsMigrateKubeletCAWith(deps destructiveDeps) sdk.ToolHandlerFor[CertsMigrateKubeletClientCAIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsMigrateKubeletClientCAIn) (*sdk.CallToolResult, destructiveOut, error) {
		// The validator owns every rule, in the CLI's words.
		f := certs.KubeletCAFlags{Cluster: in.Cluster, Phase: in.Phase, Rollback: in.Rollback, Force: in.Force}
		if err := f.Validate(); err != nil {
			return nil, destructiveOut{}, err
		}
		opts := f.Options()
		opts.Quiet = true
		action := "migrate the kubelet client CA phase " + in.Phase
		if in.Cluster {
			action = "migrate the kubelet client CA across the cluster"
		}
		out, err := run(action, func(w io.Writer) ([]string, error) {
			if in.Cluster {
				return nil, deps.migrateKubeletCACluster(ctx, w, opts)
			}
			return nil, deps.migrateKubeletCA(w, opts)
		})
		// Only a single-node forward phase leaves phases to run.
		if err == nil && !in.Cluster && in.Phase != "drop-root" && in.Phase != "remove-ca" {
			out.Next = "The migration is not finished. Run the remaining phases in order on every node, " +
				"control planes first, or run it with cluster."
		}
		return nil, out, err
	}
}

// SnapshotRestoreIn is snapshot_restore's input.
type SnapshotRestoreIn struct {
	Snapshot string `json:"snapshot" jsonschema:"a snapshot name as snapshot_list reports it, resolved inside the snapshot directory, or an absolute path to a .db file elsewhere on this node"`
}

// restoreRestartSet is etcdsnap.RestoreRestartSet, indirected for tests.
var restoreRestartSet = etcdsnap.RestoreRestartSet

// snapshot_rejoin cannot pass --force, so the other control planes must run the CLI instead.
const snapshotRejoinNext = "Every other control plane still holds the OLD etcd data, so the cluster is split until each one is brought " +
	"back. On each of them, run the CLI: `stratum snapshot rejoin --server https://<this-node-ip>:6443 " +
	"--yes --force`. The snapshot_rejoin tool cannot do this: it never overrides the quorum check, and " +
	"a node whose old cluster still has quorum, which is the usual case here, is refused."

const snapshotRestoreNext = "This node is now a single-member cluster seeded from the snapshot. " + snapshotRejoinNext +
	" Everything written after the snapshot is gone."

// SnapshotRestoreHandler is host work on this node's etcd data, via super-admin.conf.
func SnapshotRestoreHandler() sdk.ToolHandlerFor[SnapshotRestoreIn, destructiveOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SnapshotRestoreIn) (*sdk.CallToolResult, destructiveOut, error) {
		if in.Snapshot == "" {
			return nil, destructiveOut{}, fmt.Errorf(
				"snapshot is required: the path to a .db file on this node. snapshot_list reports what " +
					"is available, and there is no default because restoring the wrong one silently " +
					"reverts the cluster to the wrong point in time")
		}

		path, err := resolveSnapshotPath(in.Snapshot)
		if err != nil {
			return nil, destructiveOut{}, err
		}

		out, err := run("restore etcd from snapshot "+path, func(w io.Writer) ([]string, error) {
			return nil, destructiveOps.restoreLocalSeed(w, path)
		})
		set := restoreRestartSet()
		otherNodes := "The kubelet on every other node also holds a pre-restore watch and must be restarted. " +
			"If there are other control planes, repeat the restarts " +
			"after every one has rejoined: until then the VIP can lead to a control plane that still holds the old data. " +
			"On each control plane (kubelet recreates the controller containers once they exit, and the " +
			"supervisor restarts the operator) run `" + strings.Join(set.RepeatCommands(), "`, `") +
			"`, and restart the in-cluster workloads once with `" +
			strings.Join(set.RolloutRestartCommands("stratum kubectl"), "`, `") + "`. Controllers and " +
			"operators you installed yourself (cert-manager, an ingress or GitOps controller, a database " +
			"operator) hold watches too and are not restarted by the restore: restart each with " +
			"`stratum kubectl -n <namespace> rollout restart deploy/<name>` (or ds/ or sts/)."
		switch {
		case err == nil:
			out.Next = snapshotRestoreNext + " " + set.RestartedProse() + " " + otherNodes
		case errors.Is(err, etcdsnap.ErrWatchersNotRestarted):
			// The restore happened; the operator must not run it again.
			err = fmt.Errorf("%w\n\nThe snapshot is restored on this node: do not run the restore again. "+
				"Restart the components named above by hand; the in-cluster workloads on this node with `%s`. %s %s",
				err, strings.Join(etcdsnap.SeedRolloutRestartCommands(), "`, `"), snapshotRejoinNext, otherNodes)
		}
		return nil, out, err
	}
}

// RestoreCopyRef names one copy as snapshot_restore_copies reported it.
type RestoreCopyRef struct {
	Path     string `json:"path"`
	Modified string `json:"modified" jsonschema:"the modified time exactly as snapshot_restore_copies reported it"`
}

// SnapshotPruneRestoresIn is snapshot_prune_restores's input.
type SnapshotPruneRestoresIn struct {
	Copies []RestoreCopyRef `json:"copies" jsonschema:"the copies to remove, each as snapshot_restore_copies reported it; only these are removed, and only while unchanged"`
}

// SnapshotPruneRestoresOut is snapshot_prune_restores's result.
type SnapshotPruneRestoresOut struct {
	Action  string   `json:"action"`
	Done    bool     `json:"done"`
	Removed []string `json:"removed"`
	Left    []string `json:"left,omitempty" jsonschema:"named copies that were not removed, because they changed or are gone since the listing, or are not restore copies"`
}

// SnapshotPruneRestoresHandler removes the named copies; there is no remove-everything form.
func SnapshotPruneRestoresHandler() sdk.ToolHandlerFor[SnapshotPruneRestoresIn, SnapshotPruneRestoresOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SnapshotPruneRestoresIn) (*sdk.CallToolResult, SnapshotPruneRestoresOut, error) {
		if len(in.Copies) == 0 {
			return nil, SnapshotPruneRestoresOut{}, fmt.Errorf(
				"copies is required: name each copy to remove as snapshot_restore_copies reported it. There " +
					"is no default, because a pre-restore copy may be the only copy of the data from " +
					"before a restore")
		}
		if err := requireStratumHost(); err != nil {
			return nil, SnapshotPruneRestoresOut{}, err
		}
		shown := make([]etcdsnap.RestoreCopy, 0, len(in.Copies))
		named := map[string]bool{}
		for _, c := range in.Copies {
			if !etcdsnap.IsRestoreCopyPath(c.Path) {
				return nil, SnapshotPruneRestoresOut{}, fmt.Errorf("copy %q is not a restore copy: pass "+
					"each path exactly as snapshot_restore_copies reported it; nothing was removed", c.Path)
			}
			// Named twice, one entry could be removed on the other's stale timestamp.
			if named[c.Path] {
				return nil, SnapshotPruneRestoresOut{}, fmt.Errorf("copy %s is named more than once: name "+
					"each copy once; nothing was removed", c.Path)
			}
			named[c.Path] = true
			mod, err := time.Parse(time.RFC3339Nano, c.Modified)
			if err != nil {
				return nil, SnapshotPruneRestoresOut{}, fmt.Errorf("copy %s: modified %q is not the time "+
					"snapshot_restore_copies reported: %w", c.Path, c.Modified, err)
			}
			shown = append(shown, etcdsnap.RestoreCopy{Path: c.Path, Modified: mod})
		}
		removed, err := destructiveOps.removeRestoreCopies(shown)
		out := SnapshotPruneRestoresOut{Action: "remove etcd restore copies", Removed: []string{}}
		gone := map[string]bool{}
		for _, c := range removed {
			out.Removed = append(out.Removed, c.Path)
			gone[c.Path] = true
		}
		for _, c := range in.Copies {
			if !gone[c.Path] {
				out.Left = append(out.Left, c.Path)
			}
		}
		if err != nil {
			return nil, out, fmt.Errorf("%s stopped after removing %d: %w", out.Action, len(out.Removed), err)
		}
		out.Done = true
		return nil, out, nil
	}
}

// SnapshotRejoinIn is snapshot_rejoin's input.
type SnapshotRejoinIn struct {
	Server string `json:"server" jsonschema:"the restored seed control plane's URL, e.g. https://<seed-ip>:6443"`
}

// SnapshotRejoinHandler wipes and re-adds only when this node is not a resumable member.
func SnapshotRejoinHandler() sdk.ToolHandlerFor[SnapshotRejoinIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in SnapshotRejoinIn) (*sdk.CallToolResult, destructiveOut, error) {
		if in.Server == "" {
			return nil, destructiveOut{}, fmt.Errorf(
				"server is required: the restored seed control plane, e.g. https://<seed-ip>:6443")
		}
		u, err := url.Parse(in.Server)
		if err != nil || u.Hostname() == "" {
			return nil, destructiveOut{}, fmt.Errorf("invalid server %q: want a URL like https://<seed-ip>:6443", in.Server)
		}
		if u.Scheme != "https" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, destructiveOut{}, fmt.Errorf(
				"server %q must be exactly https://<seed-ip>:6443, with no credentials, path or query: "+
					"only the host is used, so anything else would be accepted and then ignored", in.Server)
		}

		out, rerr := run("rejoin restored cluster", func(w io.Writer) ([]string, error) {
			return nil, destructiveOps.rejoinEtcd(ctx, w, u.Hostname(), false)
		})
		if rerr == nil {
			out.Next = "This node's etcd rejoined the cluster restored on " + u.Hostname() +
				". It was wiped and re-added only if it was not already a member that could be resumed; " +
				"the progress output above says which. Repeat on every other control plane that has not yet rejoined."
		}
		return nil, out, rerr
	}
}

// HARepointIn is ha_repoint's input.
type HARepointIn struct {
	VIP    string `json:"vip" jsonschema:"the cluster VIP address this node's clients should use"`
	Cilium bool   `json:"cilium,omitempty" jsonschema:"re-point Cilium cluster-wide INSTEAD of this node's clients. This affects every node and is meant to be done exactly once for the whole cluster; this node's clients are left as they are"`
}

// HARepointHandler runs one of two repoints, never both.
func HARepointHandler(conn *kube.Connector, other ...*kube.Connector) sdk.ToolHandlerFor[HARepointIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in HARepointIn) (*sdk.CallToolResult, destructiveOut, error) {
		if in.VIP == "" {
			return nil, destructiveOut{}, fmt.Errorf("vip is required: the cluster VIP this node's clients should use")
		}
		if err := checkVIP(in.VIP); err != nil {
			return nil, destructiveOut{}, err
		}

		action := "repoint this node at " + in.VIP
		if in.Cilium {
			action = "repoint Cilium cluster-wide at " + in.VIP
		}
		// The repoint rewrites the kubeconfigs the server's connector caches, so drop the cache.
		defer invalidateConnectors(append([]*kube.Connector{conn}, other...)...)
		var cs kubernetes.Interface
		if in.Cilium {
			if err := requireStratumHost(); err != nil {
				return nil, destructiveOut{}, err
			}
			var err error
			if cs, err = clusterOfThisNode(ctx, conn, repointByFlags); err != nil {
				return nil, destructiveOut{}, err
			}
		}
		out, err := run(action, func(w io.Writer) ([]string, error) {
			if in.Cilium {
				return destructiveOps.repointCilium(ctx, w, cs, in.VIP)
			}
			return destructiveOps.nodeRepoint(w, in.VIP)
		})
		if err != nil {
			return nil, out, err
		}
		// On the Cilium path a warning is the only report an unfinished rollout gives.
		if in.Cilium && len(out.Warnings) > 0 {
			out.Next = "Cilium's env now names the VIP, but it did not finish rolling out: agents may be " +
				"dialing a VIP some node cannot reach. Check `stratum kubectl -n kube-system rollout status " +
				"ds/cilium` before relying on it. Do not repeat the repoint."
			return nil, out, nil
		}
		if in.Cilium {
			out.Next = "Cilium was re-pointed for the whole cluster. This is a once-per-cluster " +
				"operation — do not repeat it from another node."
		}
		return nil, out, nil
	}
}

// UninstallIn is uninstall's (empty) input.
type UninstallIn struct{}

// UninstallHandler removes Stratum; --allow-destructive is the confirmation, not a per-call flag.
func UninstallHandler() sdk.ToolHandlerFor[UninstallIn, destructiveOut] {
	return func(_ context.Context, _ *sdk.CallToolRequest, _ UninstallIn) (*sdk.CallToolResult, destructiveOut, error) {
		out, err := run("uninstall Stratum from this node", func(w io.Writer) ([]string, error) {
			return nil, destructiveOps.uninstallRun(w, true)
		})
		if err == nil {
			out.Next = "Stratum is gone from this node, including its etcd data and PKI. If this was a " +
				"control plane, the remaining members have lost a peer and quorum may be at risk."
		}
		return nil, out, err
	}
}

// Names only phases still to run: re-running a finished CA rotation phase is not a no-op.
func caRotationNext(current string) string {
	remaining := phasesAfter(caPhases, current)
	if len(remaining) == 0 {
		return ""
	}
	return fmt.Sprintf("Phase %q is complete here. The rotation is NOT finished: run %s on every "+
		"node, in that order, before the cluster is consistent.",
		current, strings.Join(remaining, " then "))
}

func phasesAfter(phases []string, current string) []string {
	for i, p := range phases {
		if p == current {
			return phases[i+1:]
		}
	}
	return nil
}

// The directory a bare name resolves in; a seam for tests.
var snapshotDir = clustercfg.LoadSnapshotDir

// loadSnapshotConfig is a seam for tests, which must not read the host's S3 target.
var loadSnapshotConfig = clustercfg.LoadSnapshotConfig

// Seams so a test can record the directory each acted in without touching etcd or the object store.
var (
	snapshotList  = etcdsnap.List
	snapshotSave  = etcdsnap.SaveContext
	snapshotPrune = etcdsnap.PruneScheduled
)

// resolveSnapshotPath resolves a bare name inside the snapshot directory, refusing relative paths.
func resolveSnapshotPath(snapshot string) (string, error) {
	if filepath.IsAbs(snapshot) {
		return filepath.Clean(snapshot), nil
	}
	if snapshot == "." || snapshot == ".." {
		return "", fmt.Errorf("snapshot %q names a directory, not a snapshot file: pass a name as "+
			"snapshot_list reports it", snapshot)
	}
	if snapshot != filepath.Base(snapshot) {
		return "", fmt.Errorf("snapshot %q is a relative path: a bare name is resolved inside %s, "+
			"and anything else must be absolute -- this server's working directory is wherever the "+
			"operator started it and is not a base an etcd restore should depend on",
			snapshot, snapshotDir())
	}
	return filepath.Join(snapshotDir(), snapshot), nil
}

// checkVIP refuses anything but a bare IP or hostname: it is interpolated into a URL.
func checkVIP(vip string) error {
	if net.ParseIP(vip) != nil {
		return nil
	}
	bad := func() error {
		return fmt.Errorf("vip %q must be a bare IPv4 or IPv6 address, or a hostname, with no scheme, "+
			"port, path or credentials: it is written verbatim into https://<vip>:%d in every "+
			"kubeconfig on this node, admin.conf included, so anything carrying URL structure "+
			"re-points this node's own clients", vip, version.APIServerPort)
	}
	for _, label := range strings.Split(strings.TrimSuffix(vip, "."), ".") {
		if label == "" || len(label) > 63 {
			return bad()
		}
		for _, r := range label {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
				continue
			}
			return bad()
		}
	}
	return nil
}
