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

// The Stratum destructive toolset. Registered only with
// --allow-destructive.

// destructiveOut is the shared result shape.
type destructiveOut struct {
	Action   string `json:"action"`
	Done     bool   `json:"done"`
	Progress string `json:"progress,omitempty" jsonschema:"output from the subsystem that performed the work"`
	Next     string `json:"next,omitempty" jsonschema:"what the operator must do next, when the procedure is not self-completing"`
	// Warnings are the steps the operation named as not completed while the
	// operation itself succeeded. They travel as data beside the progress —
	// which still carries whatever the operation printed — because a result
	// that reports done over a step that only warned hid the partial
	// outcome from the caller.
	Warnings []string `json:"warnings,omitempty" jsonschema:"steps that did not complete while the work itself succeeded; a result carrying them is partial, so done is false"`
}

// opMu is the explicit operation lock. The destructive tools' operations
// rewrite the same host state — etcd data, PKI, kubeconfigs, the container
// runtime, the supervisor — so no two of them may run at once: upgrade_apply,
// rollback, certs_rotate, certs_rotate_ca, certs_migrate_etcd_ca,
// snapshot_restore, snapshot_rejoin, ha_repoint
// and uninstall. run holds it around every call. The exclusion is this
// lock's own contract — nothing else provides it, so changing how the
// operations report their output must not retire the exclusion with it.
// Uninstall's reset and discovery state sits before the etcd flock that only
// covers the host teardown; internal/uninstall seeds it per run, and this
// lock keeps the MCP serialization baseline on top.
var opMu sync.Mutex

// run is the common body: host check, operation lock, shared result. The
// operation writes its progress through the writer run hands it — a buffer
// under the handlers, whose trimmed text becomes the result's Progress — and
// returns its warnings beside its error: a warning names a step that did not
// complete while the operation as a whole succeeded, and any warning makes
// the reported result partial — done stays false — while a failure keeps
// answering with its error. run swaps no stdout — the operation
// writes the buffer run hands it — and nothing redirects os.Stdout around
// the call: every operation the deps wire takes the writer itself,
// so what it prints lands in the buffer, never on the process's stdout, which
// on the stdio transport is the JSON-RPC stream. opMu is the destructive
// tools' sole exclusion.
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
		// The progress is returned alongside the error, because a
		// half-finished destructive procedure is exactly when the operator
		// needs to know how far it got.
		return out, fmt.Errorf("%s failed after the following output:\n%s\n\n%w", action, out.Progress, err)
	case len(out.Warnings) > 0:
		// The operation succeeded but named steps that did not complete: a
		// partial result, so the caller sees done false with the warnings
		// as data, not a success that folded them away.
		out.Done = false
	default:
		out.Done = true
	}
	return out, nil
}

// DestructiveOut is the result shape every destructive tool returns.
type DestructiveOut = destructiveOut

// RunExclusive runs fn as a destructive operation: the Stratum host check, the operation lock every destructive tool holds, and the shared result.
func RunExclusive(action string, fn func(io.Writer) ([]string, error)) (DestructiveOut, error) {
	return run(action, fn)
}

// invalidateConnectors is how the operations that may rewrite this node's
// credentials or endpoints end. Both the configurable and node connectors
// can cache the kubeconfigs these operations rewrite, so each operation
// drops the cache when it ends — done, failed partway, or cancelled, because
// an operation that stopped early cannot say what it managed to rewrite and
// only re-resolution can. A nil connector has nothing to invalidate: a
// handler built without the server's connector runs the same operation and
// just skips this.
func invalidateConnectors(connectors ...*kube.Connector) {
	for _, conn := range connectors {
		if conn != nil {
			conn.Invalidate()
		}
	}
}

// destructiveDeps is the one struct of operation functions the destructive
// handlers run their node work through, the way tokenDeps serves token_create:
// every operation a destructive handler can call is a field, so a test can
// record or replace each one and pin the argument-to-operation mapping
// without a node. The handlers call no operation except through it.
//
// Every field reports its progress through an io.Writer — its first
// argument, or the one after its context — and writes that progress there,
// never to os.Stdout, which on the stdio transport is the JSON-RPC stream.
// A new field follows the same convention. removeRestoreCopies is the one
// exception: it prints nothing and takes no writer.
type destructiveDeps struct {
	upgradeNode          func(io.Writer, upgrade.NodeOptions) error
	upgradeCluster       func(io.Writer, upgrade.ClusterOptions) error
	rollbackNode         func(io.Writer, upgrade.RollbackOptions) error
	certsRotate          func(context.Context, io.Writer, certs.Options) (bool, error)
	rotateCA             func(io.Writer, certs.CARotateOptions) error
	rotateCACluster      func(context.Context, io.Writer, certs.CARotateOptions) error
	migrateEtcdCA        func(io.Writer, certs.EtcdCAOptions) error
	migrateEtcdCACluster func(context.Context, io.Writer, certs.EtcdCAOptions) error
	migrateKubeletCA     func(io.Writer, certs.KubeletCAOptions) error
	// migrateKubeletCACluster is nil-safe: certs_migrate_kubelet_client_ca
	// refuses a cluster run before it, and the flag validator owns that
	// refusal, so no handler path reaches a nil field.
	migrateKubeletCACluster func(context.Context, io.Writer, certs.KubeletCAOptions) error
	restoreLocalSeed        func(io.Writer, string) error
	removeRestoreCopies     func([]etcdsnap.RestoreCopy) ([]etcdsnap.RestoreCopy, error)
	rejoinEtcd              func(context.Context, io.Writer, string, bool) error
	nodeRepoint             func(io.Writer, string) ([]string, error)
	repointCilium           func(context.Context, io.Writer, kubernetes.Interface, string) ([]string, error)
	uninstallRun            func(io.Writer, bool) error
}

// nodeClientBuilder captures one server's node connector. Each resolution
// reloads from disk because a CA orchestrator reissues the embedded credentials
// between phases; replacing this server's cached client also refreshes tools
// holding the same connector. Direct handler callers get the node credential
// without relying on another server's registration.
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

// defaultDestructiveDeps wires every field to the real operation. Each
// operation takes the handler's writer itself, so the wiring is a
// straight pass-through: the progress in the result is what the operation
// wrote through the buffer run handed it, and no print is captured off a
// redirected stdout.
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
		// The cluster orchestrators acquire their API client through a client
		// builder; the tools hand them the node connector's builder —
		// admin.conf — not the server's configurable connector.
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

// destructiveOps is the deps the handlers read, a package variable so a test
// installs recording or failing operations for the length of a call.
var destructiveOps = defaultDestructiveDeps()

// ------------------------------------------------------------ upgrade_apply

// UpgradeApplyIn is upgrade_apply's input.
type UpgradeApplyIn struct {
	Scope                 string `json:"scope" jsonschema:"node to upgrade only this node, or cluster to roll every node in the cluster"`
	Target                string `json:"target,omitempty" jsonschema:"Kubernetes version as vMAJOR.MINOR.PATCH; defaults to the version this binary carries"`
	From                  string `json:"from,omitempty" jsonschema:"for scope=node, only the currently running binary is accepted; run a different staged binary directly with its CLI. Required for scope=cluster: the name of a binary an operator staged in /var/lib/stratum/upgrade (or its full path there), which must exist at that same path on every node"`
	WorkersMaxUnavailable int    `json:"workers_max_unavailable,omitempty" jsonschema:"how many workers may be upgraded at once; defaults to 1"`
	SoakSeconds           int    `json:"soak_seconds,omitempty" jsonschema:"seconds to hold each node healthy before moving on; defaults to 15, and 0 means the default rather than no soak"`
	SkipSnapshot          bool   `json:"skip_snapshot,omitempty" jsonschema:"skip the pre-upgrade etcd snapshot; strongly discouraged"`
}

// UpgradeApplyHandler performs a real upgrade. A cluster rollout runs on the
// credential conn resolves, so its RBAC bounds the drains, Jobs and lease; a
// node upgrade is host work on this node and uses its own credentials.
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

// thisNodeName is the Kubernetes name this node registered under, a var for
// tests. The kubelet lowercases the hostname it registers, so this does too.
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

// localCluster is this node's own cluster, through its super-admin.conf, with the
// address it serves on: the reference the server's credential is checked
// against. A var for tests.
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

// The advice a cluster-proof refusal gives for re-pointing the credential
// depends on which connector the handler holds: the configurable one answers
// to the operator's flags, while the node connector rides this node's
// admin.conf, which those flags cannot move.
const (
	repointByFlags        = "point --kubeconfig and --context at it"
	repointNodeCredential = "the node credential is this node's admin.conf, which --kubeconfig and --context cannot move; check what /etc/kubernetes/admin.conf addresses"
)

// clusterOfThisNode is the typed client conn resolves for its default
// context, after checking that the cluster it addresses is this node's own:
// a cluster-wide operation started from this node must not land on another
// cluster. remediation is the advice the refusal gives for re-pointing the
// credential, which differs by connector (repointByFlags,
// repointNodeCredential).
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

// requireThisNodeIn refuses the cluster cs addresses, at host, unless it is the
// one local addresses at localHost: the same kube-system namespace, by UID,
// and a Node registered as this node. A Node name alone is not proof, since
// hostnames repeat across clusters. remediation names how the caller's
// credential can be re-pointed, which differs by connector.
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

// upgradeStageOwner is the owner a staged upgrade binary and its directory
// must have, a var for tests.
var upgradeStageOwner uint32

// stagedUpgradeBinary is the path of the upgrade binary from names, which must
// be a regular file directly in version.UpgradeStageDir, owned by root and
// writable by no one else, in a directory that is the same. from is the
// file's name or its full path there. The model chooses among binaries an
// operator staged; it cannot name another program for every node to run as
// root.
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

// checkStaged refuses path unless it is a directory (dir) or regular file,
// not a symlink, owned by upgradeStageOwner, with no group or other write.
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

// ----------------------------------------------------------------- rollback

// RollbackIn is rollback's input.
type RollbackIn struct {
	SoakSeconds  int    `json:"soak_seconds,omitempty" jsonschema:"seconds to hold the node healthy before declaring the rollback good; defaults to 15, and 0 means the default rather than no soak"`
	EtcdSnapshot string `json:"etcd_snapshot,omitempty" jsonschema:"also restore etcd from this snapshot; a name as snapshot_list reports it, or an absolute path. Single control plane only, and it discards every change since the snapshot"`
}

// RollbackHandler reverts this node to its previous version. It is host work
// on this node, and its API calls use this node's super-admin.conf, not the
// server's credential: RBAC neither enables nor limits it.
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

// ------------------------------------------------------------- certs_rotate

// CertsRotateIn is certs_rotate's input.
type CertsRotateIn struct {
	IfExpiringDays int `json:"if_expiring_days,omitempty" jsonschema:"only rotate when a leaf is within this many days of expiry; omit to rotate regardless"`
}

// CertsRotateHandler renews this node's leaf certificates. It rewrites PKI on
// this node and uses this node's super-admin.conf for its API calls, not the
// server's credential: RBAC neither enables nor limits it.
func CertsRotateHandler(conn ...*kube.Connector) sdk.ToolHandlerFor[CertsRotateIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsRotateIn) (*sdk.CallToolResult, destructiveOut, error) {
		// The rotation regenerates this node's kubeconfigs, which the
		// server's connectors fingerprint and cache: however the run ends,
		// both caches are dropped on the way out.
		defer invalidateConnectors(conn...)
		// Not Quiet: Rotate's output is the only thing separating a real
		// rotation from a no-op, and the wiring files what it printed through
		// the writer run handed the operation. The request's context bounds
		// the rotation at its step boundaries.
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

// ---------------------------------------------------------- certs_rotate_ca

// CertsRotateCAIn is certs_rotate_ca's input.
type CertsRotateCAIn struct {
	Phase   string `json:"phase,omitempty" jsonschema:"one node: trust-new, sign-new or finalize — the phases run in that order and each must complete cluster-wide before the next; leave empty with cluster"`
	Cluster bool   `json:"cluster,omitempty" jsonschema:"run every phase across every node, as the CLI's --cluster does; leave phase empty"`
	Force   bool   `json:"force,omitempty" jsonschema:"rotate even though an old CA or service-account key is gone; the old CA certificates (ca.crt, front-proxy-ca.crt, and kubelet-client-ca.crt where the kubelets have their own client CA) and the service-account public key (sa.pub, or sa.key to derive it) are still required; for compromise recovery only"`

	RestartWorkloads  bool `json:"restart_workloads,omitempty" jsonschema:"cluster only: restart the Deployments, DaemonSets and StatefulSets that use in-cluster API credentials once the new CA is published, before sign-new; without it a cluster rotation refuses to start when there are any"`
	SkipWorkloadCheck bool `json:"skip_workload_check,omitempty" jsonschema:"do not check the workloads that use in-cluster API credentials; they fail with x509 errors from sign-new until restarted"`
	SkipTokenCheck    bool `json:"skip_token_check,omitempty" jsonschema:"finalize without checking that no mounted ServiceAccount token is still signed by the old key, which gets 401 once the old key is dropped; for compromise recovery, so it needs force"`
}

// caPhases is the CA-rotation phase list, shared with the CLI through the
// internal/certs export; the MCP server keeps no list of its own to drift.
var caPhases = certs.CARotatePhases

// CertsRotateCAHandler runs one phase of a CA rotation. Every phase rewrites
// PKI on this node, and the cluster rotation drives the other nodes through
// this node's super-admin.conf, not the server's credential: RBAC neither enables
// nor limits it.
//
// Supplied connectors are invalidated after every phase, including failures
// and cancellations, because the phase may rewrite their kubeconfig inputs.
func CertsRotateCAHandler(conn ...*kube.Connector) sdk.ToolHandlerFor[CertsRotateCAIn, destructiveOut] {
	return certsRotateCAWith(destructiveOps, conn...)
}

func certsRotateCAWith(deps destructiveDeps, conn ...*kube.Connector) sdk.ToolHandlerFor[CertsRotateCAIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsRotateCAIn) (*sdk.CallToolResult, destructiveOut, error) {
		// Request cancellation is honored before anything, argument validation
		// included: the cluster operation's first check is the caller's context,
		// and a canceled request must not come back as a refusal instead of its
		// cause.
		if err := ctx.Err(); err != nil {
			invalidateConnectors(conn...)
			return nil, destructiveOut{}, err
		}
		// As the CLI: --cluster drives every phase, --phase runs one on this
		// node. The rules and their wording are the CLI validator's, spelled
		// with the CLI's flag names.
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
		// From here the phase may rewrite this node's PKI: however it ends,
		// the connectors' caches are dropped on the way out. The refusals above
		// run the phase never, so they skip it.
		defer invalidateConnectors(conn...)
		out, err := run(action, func(w io.Writer) ([]string, error) {
			if in.Cluster {
				return nil, deps.rotateCACluster(ctx, w, opts)
			}
			return nil, deps.rotateCA(w, opts)
		})
		if err == nil {
			// The x509 window around sign-new, the same text the CLI prints,
			// then what remains of the rotation for a --phase run.
			if in.Cluster {
				out.Next = certs.CARotateClusterWarning
			} else {
				out.Next = certs.CARotatePhaseWarning + caRotationNext(in.Phase)
			}
		}
		return nil, out, err
	}
}

// --------------------------------------------------- certs_migrate_etcd_ca

// CertsMigrateEtcdCAIn is certs_migrate_etcd_ca's input.
type CertsMigrateEtcdCAIn struct {
	Phase   string `json:"phase" jsonschema:"one node: trust-both, then issue-new, then drop-root, in that order; rollback is not a fourth step but the way back, un-migrating a node instead of advancing it; leave empty with cluster"`
	Cluster bool   `json:"cluster,omitempty" jsonschema:"run every phase across every control plane, as the CLI's --cluster does; leave phase empty"`
	Resume  bool   `json:"resume,omitempty" jsonschema:"proceed on a cluster whose nodes are at different phases"`
	Force   bool   `json:"force,omitempty" jsonschema:"rollback only: un-migrate a node that already completed drop-root"`
}

// CertsMigrateEtcdCAHandler moves etcd onto its own CA, one phase at a time.
// It rewrites etcd PKI on this node, and the cluster migration drives the
// other nodes through this node's super-admin.conf, not the server's credential:
// RBAC neither enables nor limits it.
func CertsMigrateEtcdCAHandler() sdk.ToolHandlerFor[CertsMigrateEtcdCAIn, destructiveOut] {
	return certsMigrateEtcdCAWith(destructiveOps)
}

func certsMigrateEtcdCAWith(deps destructiveDeps) sdk.ToolHandlerFor[CertsMigrateEtcdCAIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsMigrateEtcdCAIn) (*sdk.CallToolResult, destructiveOut, error) {
		// Request cancellation is honored before anything, argument validation
		// included: the cluster operation's first check is the caller's context,
		// and a canceled request must not come back as a refusal instead of its
		// cause.
		if err := ctx.Err(); err != nil {
			return nil, destructiveOut{}, err
		}
		// As the CLI: --cluster (no phase) drives every phase across every
		// control plane, --phase runs one on this node. The validator owns every
		// rule — the missing phase, an unknown phase, cluster with a phase — in
		// the CLI's words.
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
		// A cluster run finishes the migration itself, as the CLI's --cluster
		// does; only a single-node forward phase leaves phases to run.
		if err == nil && !in.Cluster && in.Phase != "drop-root" && in.Phase != "rollback" {
			out.Next = "The migration is not finished. Run the remaining phases in order on every " +
				"control plane; etcd restarts at each one."
		}
		return nil, out, err
	}
}

// --------------------------------------- certs_migrate_kubelet_client_ca

// CertsMigrateKubeletClientCAIn is certs_migrate_kubelet_client_ca's input.
type CertsMigrateKubeletClientCAIn struct {
	Cluster  bool   `json:"cluster,omitempty" jsonschema:"run every phase on every node, control planes first (recommended)"`
	Rollback bool   `json:"rollback,omitempty" jsonschema:"with cluster: move every kubelet back to trusting the cluster root"`
	Phase    string `json:"phase,omitempty" jsonschema:"run one phase on this node instead: trust-both, issue-new, drop-root, or re-add-root, reissue-root, remove-ca to undo them"`
	Force    bool   `json:"force,omitempty" jsonschema:"rollback only: re-add the cluster root to kubelets that completed drop-root"`
}

// CertsMigrateKubeletClientCAHandler moves the kubelets' client trust onto
// its own CA, or back to the cluster root. Like certs_migrate_etcd_ca it acts
// through this node's super-admin.conf, not the server's credential.
func CertsMigrateKubeletClientCAHandler() sdk.ToolHandlerFor[CertsMigrateKubeletClientCAIn, destructiveOut] {
	return certsMigrateKubeletCAWith(destructiveOps)
}

func certsMigrateKubeletCAWith(deps destructiveDeps) sdk.ToolHandlerFor[CertsMigrateKubeletClientCAIn, destructiveOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in CertsMigrateKubeletClientCAIn) (*sdk.CallToolResult, destructiveOut, error) {
		// As the CLI: --cluster (no phase) drives every phase across every
		// node, --phase runs one on this node. The validator owns every rule,
		// in the CLI's words.
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
		// A cluster run finishes the migration itself, as the CLI's --cluster
		// does; only a single-node forward phase leaves phases to run.
		if err == nil && !in.Cluster && in.Phase != "drop-root" && in.Phase != "remove-ca" {
			out.Next = "The migration is not finished. Run the remaining phases in order on every node, " +
				"control planes first, or run it with cluster."
		}
		return nil, out, err
	}
}

// ---------------------------------------------------------- snapshot_restore

// SnapshotRestoreIn is snapshot_restore's input.
type SnapshotRestoreIn struct {
	Snapshot string `json:"snapshot" jsonschema:"a snapshot name as snapshot_list reports it, resolved inside the snapshot directory, or an absolute path to a .db file elsewhere on this node"`
}

// restoreRestartSet is etcdsnap.RestoreRestartSet, indirected for tests.
var restoreRestartSet = etcdsnap.RestoreRestartSet

// snapshotRejoinNext is what the other control planes need after a restore. They
// still hold the old cluster, which normally still has quorum, so a rejoin without
// --force is refused; snapshot_rejoin has no way to pass it (SnapshotRejoinHandler
// calls ha.RejoinEtcd with force=false), so the next step is the CLI's, run on each
// of those nodes.
const snapshotRejoinNext = "Every other control plane still holds the OLD etcd data, so the cluster is split until each one is brought " +
	"back. On each of them, run the CLI: `stratum snapshot rejoin --server https://<this-node-ip>:6443 " +
	"--yes --force`. The snapshot_rejoin tool cannot do this: it never overrides the quorum check, and " +
	"a node whose old cluster still has quorum, which is the usual case here, is refused."

// snapshotRestoreNext is what a successful restore tells the caller to do, with
// nothing restarted to report.
const snapshotRestoreNext = "This node is now a single-member cluster seeded from the snapshot. " + snapshotRejoinNext +
	" Everything written after the snapshot is gone."

// SnapshotRestoreHandler restores etcd on this node as a new single-member
// cluster. It is host work on this node's etcd data, and its health checks use
// this node's super-admin.conf, not the server's credential: RBAC neither enables nor
// limits it.
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
		// otherNodes is what is left once the node's own data is restored, besides
		// rejoining the other control planes (snapshotRejoinNext), rendered from the
		// restore's restart set: the per-control-plane block, and the
		// once-for-the-cluster block, the set carries.
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
			// The data is restored; the operator still needs the rest, and must not
			// run the restore again.
			err = fmt.Errorf("%w\n\nThe snapshot is restored on this node: do not run the restore again. "+
				"Restart the components named above by hand; the in-cluster workloads on this node with `%s`. %s %s",
				err, strings.Join(etcdsnap.SeedRolloutRestartCommands(), "`, `"), snapshotRejoinNext, otherNodes)
		}
		return nil, out, err
	}
}

// --------------------------------------------------- snapshot_prune_restores

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

// SnapshotPruneRestoresHandler removes the named restore copies. It
// has no "remove everything" form: the caller names each copy as it was
// listed, and one that changed since is left, the same guarantee as
// `stratum snapshot prune-restores --yes`.
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
			// A malformed entry is refused, not quietly left: this request
			// deletes etcd data, so it must name copies as the listing did.
			if !etcdsnap.IsRestoreCopyPath(c.Path) {
				return nil, SnapshotPruneRestoresOut{}, fmt.Errorf("copy %q is not a restore copy: pass "+
					"each path exactly as snapshot_restore_copies reported it; nothing was removed", c.Path)
			}
			// Named twice, one entry could be removed on the other's
			// timestamp, and the result would report the stale one removed.
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

// ----------------------------------------------------------- snapshot_rejoin

// SnapshotRejoinIn is snapshot_rejoin's input.
type SnapshotRejoinIn struct {
	Server string `json:"server" jsonschema:"the restored seed control plane's URL, e.g. https://<seed-ip>:6443"`
}

// SnapshotRejoinHandler rejoins this node's etcd to the restored cluster. It
// wipes the local etcd data and re-adds the member only when this node is not
// yet a member of the restored cluster, or is a member that is not serving and
// is safe to remove; a member that can be resumed keeps its data. Callers must
// not rely on the wipe happening.
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
		// Only the hostname is used, so anything else in the URL would be
		// accepted and then ignored -- and a caller that spelled out a
		// path or a different scheme is describing a target this tool
		// will not actually contact.
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

// ---------------------------------------------------------------- ha_repoint

// HARepointIn is ha_repoint's input.
type HARepointIn struct {
	VIP    string `json:"vip" jsonschema:"the cluster VIP address this node's clients should use"`
	Cilium bool   `json:"cilium,omitempty" jsonschema:"ALSO re-point Cilium cluster-wide. This affects every node and is meant to be done exactly once for the whole cluster"`
}

// HARepointHandler moves this node's clients onto the cluster VIP. The
// cluster-wide Cilium repoint runs on the credential conn resolves; the node
// repoint rewrites this node's kubeconfigs and needs no API credential.
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
		// The repoint rewrites every kubeconfig on this node, admin.conf
		// included — the files the server's connector caches. From here to
		// the return the cache is dropped, however the operation ends: the
		// cluster-wide Cilium work below runs before the node repoint and
		// can fail after it, and a repoint that got that far has still
		// earned a re-resolve. The refusals above run the operation never,
		// so they skip it.
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
		// A rollout that did not finish comes back as a warning beside a nil
		// error — the env change is already applied — and on the Cilium path
		// that is the only warning the operation returns, so the guidance the
		// old rollout sentinel carried keys off the list.
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

// ---------------------------------------------------------------- uninstall

// UninstallIn is uninstall's (empty) input.
type UninstallIn struct{}

// UninstallHandler removes Stratum from this node.
//
// uninstall.Run(false) prints a preview and removes nothing, so passing the
// caller's own flag here made the tool a no-op that still reported success.
// Starting the server with --allow-destructive is the confirmation; the
// preview is uninstall_preview, which is a separate read-tier tool.
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

// caRotationNext is the guidance after a phase completes, or "" when none
// remains. It names only the phases still to run: listing the whole
// sequence would tell an operator to repeat the phase that just finished,
// and re-running a CA rotation phase is not a no-op.
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

// snapshotDir is the directory a bare name resolves in: spec.snapshots.dir, as
// `stratum snapshot restore NAME` reads it. A seam for tests.
var snapshotDir = clustercfg.LoadSnapshotDir

// loadSnapshotConfig reads the snapshot config the list, save and prune tools
// use: the resolved settings, which carry the directory (spec.snapshots.dir)
// those tools act in. A seam for tests, which must not read the host's S3
// target.
var loadSnapshotConfig = clustercfg.LoadSnapshotConfig

// snapshotList, snapshotSave and snapshotPrune are the three snapshot effects
// the list, save and prune tools drive, behind seams so a test can record the
// directory each one acted in without touching etcd or the object store.
var (
	snapshotList  = etcdsnap.List
	snapshotSave  = etcdsnap.SaveContext
	snapshotPrune = etcdsnap.PruneScheduled
)

// resolveSnapshotPath resolves a bare snapshot name inside the snapshot
// directory, passes an absolute path through, and refuses a relative one.
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

// checkVIP refuses anything that is not a bare IP address or hostname.
// ha.NodeRepoint interpolates the value into https://<vip>:6443 and writes
// that into every kubeconfig on the node.
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
