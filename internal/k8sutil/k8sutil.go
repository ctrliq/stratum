// Package k8sutil holds small host-side helpers shared across the stratum
// subcommands: running the embedded kubectl against admin.conf, the apiserver
// health gate, node-registration wait, the progress "step" printer, the
// admin-clientset constructor, the etcdctl mTLS env, the node label/stamp
// the upgrade orchestrator relies on, and the cluster maintenance fence the
// command-level seams take. It depends only on internal packages below it in
// the dependency direction (version, pki, maintlease) and client-go, so
// server, ha, agent, addons, token, etcdsnap, and upgrade can all reuse it
// without an import cycle.
package k8sutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Labels/annotations the upgrade machinery reads and writes: `stratum upgrade
// --cluster` orders control planes first by the label, and tracks which nodes
// are done by the annotation.
const (
	ControlPlaneLabel = "stratum.ciq.com/control-plane"
	VersionAnnotation = "stratum.ciq.com/k8s-version"
	// StratumVersionAnnotation is the stratum build the node runs, the version
	// shown for it in cluster views.
	StratumVersionAnnotation = "stratum.ciq.com/version"
	// UpgradeInProgressAnnotation is the version `stratum upgrade --cluster`
	// is taking a node to, set before that node's Job runs and removed once
	// the node is back Ready at it. Still set while the kubelet
	// reports that version means the node was interrupted after its kubelet
	// swap, before its upgrade finished.
	UpgradeInProgressAnnotation = "stratum.ciq.com/upgrade-in-progress"
	// UpgradeCordonAnnotation says who owns a node's cordon during a cluster
	// upgrade, as "<kind>@<holder>". The kind is the target version
	// when the upgrade cordoned a node that was schedulable, so the upgrade may
	// undo it; "preexisting" when the node was already cordoned, so it is left as
	// found; "released" once the per-node Job has undone its own. The holder is
	// the cluster-upgrade lease holder of the run that wrote it, which is how a
	// run tells its own claim from a successor's; a value without one is an older
	// orchestrator's. Only the upgrade's own writes set it.
	UpgradeCordonAnnotation = "stratum.ciq.com/upgrade-cordoned"
)

// RuntimeHandlerLabelPrefix prefixes the label stratum sets on a node for
// each containerd runtime handler it declares, <prefix><handler>="true",
// which the handler's RuntimeClass selects nodes by.
const RuntimeHandlerLabelPrefix = "runtime.stratum.ciq.com/"

// OperatorLeaseName is the Lease the operator campaigns for under leader
// election (kube-system/<name>): only the holder reconciles, so several
// control-plane hosts can all run the operator. The dashboard's SPA reads the
// same Lease to say whether an operator is live, and internal/dashboard's SPA
// contract test holds that copy to this constant — one declaration so the
// campaign and the SPA's lookup cannot drift.
const OperatorLeaseName = "stratum-operator"

// HelmChartGroup and HelmChartVersion are the API group and version of the
// operator's HelmChart and HelmChartConfig CRs: the operator's dynamic client
// lists them at /apis/<group>/<version>/..., and the dashboard's SPA builds
// its HelmChart screens from the same prefix, which the SPA contract test
// holds to this pair. One declaration so the controller and the SPA cannot
// drift apart.
const (
	HelmChartGroup   = "stratum.ciq.com"
	HelmChartVersion = "v1"
)

// UninstalledAnnotation is set by `stratum uninstall` on a worker's own Node,
// to the time it stopped kubelet there (UninstallMark), so the operator can remove
// the Node once the worker is gone: the worker's own credential cannot delete
// it. Only the node itself may set it (an admission policy).
const UninstalledAnnotation = "stratum.ciq.com/uninstalled"

// NodeMarkPolicy names the ValidatingAdmissionPolicy, and its binding, that
// let only a node write UninstalledAnnotation on its own Node
// (internal/addons/manifests/node-mark-policy.yaml). The operator acts on a
// mark only while both are in place.
const NodeMarkPolicy = "stratum-node-uninstall-mark"

// JoinFencePolicy names the ValidatingAdmissionPolicy, and its binding, that
// hold a control-plane join token to its own maintenance fence
// (internal/addons/manifests/join-fence-policy.yaml): it may write only the
// two maintenance leases, only under a holder that names the token, and never
// over a lease another run holds. ha.GrantCertsReader puts both in place
// before it grants that token the leases.
const JoinFencePolicy = "stratum-join-maintenance-fence"

// UninstallMark is the value UninstalledAnnotation carries for t: RFC 3339
// to the nanosecond, the Lease renewal's own precision, so the operator can
// tell a renewal in the same second before the mark from one after it. One
// function so the writer and the reader cannot drift; time.RFC3339 parses it.
func UninstallMark(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// BootstrapGroup is the bootstrap-token auth group token.Create puts every
// joiner in, worker or control plane: the group the CSR bindings name.
const BootstrapGroup = "system:bootstrappers:stratum:nodes"

// NodeClientAutoApproveBinding is Stratum's binding to the upstream nodeclient
// auto-approval ClusterRole. DisableNodeClientAutoApprove keeps it present with
// no subjects; see there for why it is never deleted.
const NodeClientAutoApproveBinding = "stratum-csr-autoapprove"

// ControlPlaneBootstrapGroup is the auth group token.Create adds, beside
// BootstrapGroup, only to a control-plane join token, and the only group
// ha.UploadCerts grants read on the shared-certs Secret, so a worker token
// cannot read it. One constant so the two ends cannot drift.
const ControlPlaneBootstrapGroup = "system:bootstrappers:stratum:control-planes"

// Step prints a green progress header through w: a CLI caller passes the
// process's stdout, an MCP handler the buffer it files as the tool result's
// progress. On the MCP stdio transport stdout is the JSON-RPC stream, so no
// caller may point Step at it implicitly.
func Step(w io.Writer, msg string) { fmt.Fprintf(w, "\n\033[1;32m==>\033[0m %s\n", msg) }

// The cluster-wide maintenance fence: the Lease pair internal/maintlease.
// AcquireCompatible holds — the common kube-system/stratum-cluster-maintenance
// lease `rotate-ca --cluster`, `migrate-etcd-ca` and `stratum upgrade
// --cluster` keep renewed for their whole run, plus the released
// kube-system/stratum-cluster-upgrade name an old upgrade holds alone.
//
// The fence is taken at the command-level seams — `stratum token create
// --control-plane` and the MCP token_create (the mint re-keys and republishes
// the shared cert bundle), the join's member-add window (member admission
// races the very bundle and staged-key writes those runs own), and a control
// plane's CIS at-rest key mint (the key the join bundle carries) — and NEVER
// inside the internal/ha helpers, which the orchestrators themselves call
// mid-run and which must keep working without refusing against their own
// fence. AcquireCompatible refuses rather than waits, so a mint or a join that
// lands while a maintenance run holds the fence fails fast naming the live
// holder and its renewal age: re-keying the bundle or admitting a member under
// a running rotation locks the rotation's follower control planes out of the
// material they decrypt.
const (
	// maintenanceLeaseName is the common maintenance lease's name. The name is
	// a cluster-wide contract, so it is spelled as a literal here, in
	// internal/certs and internal/upgrade (their own acquires), and in the
	// leases RBAC grant internal/ha publishes, rather than behind one
	// package's constant an older binary could not follow.
	maintenanceLeaseName = "stratum-cluster-maintenance"
	// maintenanceLeaseNamespace is the namespace every maintenance lease lives
	// in.
	maintenanceLeaseNamespace = "kube-system"
)

// MaintenanceHolder is this run's identity on the maintenance Lease, in the
// host/pid/nanoseconds shape internal/certs's orchestrators use: the
// nanoseconds make two runs from one host distinct holders, so a second run
// never mistakes the first run's still-live lease for its own. One identity
// per fence take, spelled the same way whichever command took it, so what an
// operator reads off a held lease always means the same thing; a join's
// bootstrap token takes it behind the token's username (JoinMaintenanceHolder).
func MaintenanceHolder() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown-host"
	}
	return fmt.Sprintf("%s/%d/%d", h, os.Getpid(), time.Now().UnixNano())
}

// MaintenanceLease carries ownership confirmation and release for a held
// maintenance fence. Confirm must succeed after slow work and before the next
// mutation: a process pause or API outage can let a successor take the leases.
type MaintenanceLease struct {
	Guard   *maintlease.Guard
	Release func()
}

// Confirm proves that both maintenance leases still belong to this run.
// A nil capability represents an operation that needs no cluster fence.
func (l *MaintenanceLease) Confirm(ctx context.Context) error {
	if l == nil {
		return nil
	}
	return l.Guard.Confirm(ctx)
}

// ConfirmWithin is Confirm retried through an API outage of at most budget:
// each attempt is a same-holder renewal, so success still proves nobody held
// the leases meanwhile, and a takeover or another answer retrying cannot
// change returns at once. A nil capability needs no cluster fence.
func (l *MaintenanceLease) ConfirmWithin(ctx context.Context, budget time.Duration) error {
	if l == nil {
		return nil
	}
	return l.Guard.ConfirmWithin(ctx, budget)
}

// MaintenanceFence takes the cluster-wide maintenance fence with the client
// given, and returns its ownership and release capability. The client is the credential the
// lease's API calls ride, renewed in the background until release: an admin
// clientset for the minting commands. The joiner's bootstrap-token client, its
// only working credential in the member-add window, takes the fence through
// MaintenanceFenceAs instead, under JoinMaintenanceHolder. The take
// refuses rather than waits (holding the fence names the run to wait for), so
// callers confirm ownership before subsequent mutations. Release stops renewal
// and deletes the lease only while it is still ours. It must run on every exit,
// success included — a fence left held would refuse the next maintenance run
// until the lease self-expires.
func MaintenanceFence(cs kubernetes.Interface) (*MaintenanceLease, error) {
	return MaintenanceFenceAs(cs, MaintenanceHolder())
}

// MaintenanceFenceAs is MaintenanceFence under the holder identity given: a
// join's bootstrap token takes it as JoinMaintenanceHolder, the only holder
// the JoinFencePolicy admission policy lets that token write.
func MaintenanceFenceAs(cs kubernetes.Interface, holder string) (*MaintenanceLease, error) {
	guard, release, err := maintlease.AcquireCompatible(context.Background(), cs,
		maintenanceLeaseNamespace, maintenanceLeaseName, holder)
	if err != nil {
		return nil, err
	}
	return &MaintenanceLease{Guard: guard, Release: release}, nil
}

// JoinMaintenanceHolder is the holder identity a joining control plane takes
// the maintenance fence under with bootstrap token tokenID: the token's
// username, system:bootstrap:<token-id>, then a slash and MaintenanceHolder.
// The JoinFencePolicy admission policy admits a join token's lease write only
// under a holder with that prefix, so the leases a token can renew, release or
// take over are its own and nobody else's.
func JoinMaintenanceHolder(tokenID string) string {
	return "system:bootstrap:" + tokenID + "/" + MaintenanceHolder()
}

// JoinFenceDenied reports whether err is the JoinFencePolicy admission
// policy's denial of a lease write, as the apiserver's admission dispatcher
// words it ("ValidatingAdmissionPolicy '<policy>' with binding '<binding>'
// denied request: …"). The policy sets no reason, so the status is Invalid,
// never the Forbidden RBAC answers a token the leases are not granted to.
func JoinFenceDenied(err error) bool {
	var st apierrors.APIStatus
	return errors.As(err, &st) && strings.Contains(st.Status().Message, "ValidatingAdmissionPolicy '"+JoinFencePolicy+"'")
}

// AdminMaintenanceFence is MaintenanceFence with this node's admin
// credentials: the clientset the minting commands run under.
func AdminMaintenanceFence() (*MaintenanceLease, error) {
	cs, err := AdminClientset()
	if err != nil {
		return nil, err
	}
	return MaintenanceFence(cs)
}

// InternalAdminConf is the kubeconfig Stratum's own clients use:
// super-admin.conf, or admin.conf on a node whose super-admin.conf the next
// `stratum up` has yet to write.
func InternalAdminConf() string {
	if _, err := os.Stat(version.SuperAdminConf()); err == nil {
		return version.SuperAdminConf()
	}
	return version.AdminConf
}

// Kubectl returns an *exec.Cmd for the embedded kubectl, pointed at
// InternalAdminConf.
func Kubectl(args ...string) *exec.Cmd { return KubectlContext(context.Background(), args...) }

// KubectlContext is Kubectl killed when ctx ends.
func KubectlContext(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, filepath.Join(version.BinDir, "kubectl"), args...)
	c.Env = append(os.Environ(), "KUBECONFIG="+InternalAdminConf())
	return c
}

// AdminRESTConfig loads InternalAdminConf's rest.Config (client-go consumers
// that need more than a Clientset, e.g. the dynamic applier).
func AdminRESTConfig() (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", InternalAdminConf())
	if err != nil {
		return nil, fmt.Errorf("load admin kubeconfig: %w", err)
	}
	return cfg, nil
}

// AdminClientset returns a typed clientset built from InternalAdminConf.
func AdminClientset() (*kubernetes.Clientset, error) {
	cfg, err := AdminRESTConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// EtcdctlEnv presets the v3 mTLS env so etcdctl talks to the local etcd
// (`stratum etcdctl` and the etcdsnap snapshot save path share it).
func EtcdctlEnv() []string {
	return []string{
		// No ETCDCTL_API=3. The bundled etcdctl is v3-only and does not
		// recognise the variable — it answers every invocation with
		// `unrecognized environment variable ETCDCTL_API=3` on stderr. That was
		// invisible while this env only reached the snapshot timer's log; it is
		// not once `stratum etcdctl` injects it in front of an operator. The
		// binary and its etcdctl ship together, so there is no older client to
		// keep it for.
		fmt.Sprintf("ETCDCTL_ENDPOINTS=https://127.0.0.1:%d", version.EtcdClientPort),
		"ETCDCTL_CACERT=" + etcdCACertPath(),
		"ETCDCTL_CERT=" + version.PKIDir + "/apiserver-etcd-client.crt",
		"ETCDCTL_KEY=" + version.PKIDir + "/apiserver-etcd-client.key",
	}
}

// etcdCACertPath picks the trust file etcdctl should verify the LOCAL etcd's
// server certificate against, preferring the etcd CA bundle.
//
// This deliberately does not hardcode ca.crt any more. That was already subtly
// wrong — the plain root cert rather than the rotating ca-bundle.crt — and it
// becomes an outage once etcd's leaves move to their own CA: `stratum etcdctl`
// and, far worse, the scheduled snapshot timer (etcdsnap shells out with this
// env) would fail to verify the server and stop producing backups. Being
// timer-driven, nobody would be paged for it.
//
// The ordered list itself is NOT repeated here. pki.EtcdTrustFile answers the
// same question — what does a client verify an etcd server against — and its
// doc comment carries the security constraint on the list's shape (why the
// Kubernetes bundle must never be unioned in). A second copy would drift.
//
// The blind ca.crt fallback keeps EtcdctlEnv's signature: a node with none of
// the three files has no PKI at all, and etcdctl's own error naming the missing
// file reports that better than a half-built environment would.
func etcdCACertPath() string {
	if p, err := pki.EtcdTrustFile(); err == nil {
		return p
	}
	return filepath.Join(version.PKIDir, "ca.crt")
}

// WaitHealthz polls the apiserver /healthz until it returns "ok" or
// attempts × HealthzPollInterval elapses. Callers size their own deadlines
// from that window, so a probe that hangs is cut off at it instead of holding
// the caller forever.
func WaitHealthz(attempts int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(attempts)*HealthzPollInterval)
	defer cancel()
	return WaitHealthzContext(ctx)
}

// WaitHealthzContext is WaitHealthz bounded by ctx instead of an attempt count:
// the kubectl call is killed and the retry wait cut short when ctx ends, and at
// least one request is made only if ctx still has time.
func WaitHealthzContext(ctx context.Context) error {
	return waitHealthz(ctx, func(ctx context.Context) ([]byte, error) {
		return KubectlContext(ctx, "get", "--raw", "/healthz").Output()
	})
}

// WaitHealthzClient is WaitHealthzContext through cs instead of this node's
// admin.conf. A credential the API server refuses ends the wait at once:
// waiting cannot grant it the rights to read /healthz.
func WaitHealthzClient(ctx context.Context, cs kubernetes.Interface) error {
	return waitHealthz(ctx, func(ctx context.Context) ([]byte, error) {
		return cs.Discovery().RESTClient().Get().AbsPath("/healthz").DoRaw(ctx)
	})
}

// healthzAttemptTimeout bounds ONE /healthz probe. A probe the API server
// accepts and never answers must cost one attempt, not the whole window, or a
// single hung request ends the wait with no later attempt. It matches
// apiCallTimeout in internal/upgrade/k8sclient.go, which bounds one API
// request for the same reason.
const healthzAttemptTimeout = 10 * time.Second

// HealthzPollInterval is the wait between /healthz probes, and the unit of
// WaitHealthz's window.
const HealthzPollInterval = 2 * time.Second

// probeAttempt runs one probe under its own deadline, a child of the window's
// ctx: a probe that hangs is cut off at healthzAttemptTimeout while the window
// keeps running, and the timeout is named in the error so the wait's final
// "last check" says why the probe failed instead of a bare deadline.
func probeAttempt(ctx context.Context, probe func(context.Context) ([]byte, error)) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, healthzAttemptTimeout)
	defer cancel()
	// Test the attempt ctx, not the error: a kubectl the deadline kills exits
	// with a plain *exec.ExitError ("signal: killed"), which does not wrap
	// context.DeadlineExceeded, while a probe that gave up on its own returns
	// one. Only an attempt that the deadline cut off is reworded; a fast
	// failure (e.g. connection refused) keeps its own error, and an expired
	// window is excluded by the ctx.Err() check in the if below.
	out, err := probe(attemptCtx)
	if err != nil && ctx.Err() == nil && attemptCtx.Err() != nil {
		return out, fmt.Errorf("no answer within %s: %w", healthzAttemptTimeout, err)
	}
	return out, err
}

// waitHealthz polls probe every HealthzPollInterval until it answers "ok" or ctx
// ends, and names the last answer it got when it gives up.
func waitHealthz(ctx context.Context, probe func(context.Context) ([]byte, error)) error {
	var last error
	for {
		if ctx.Err() != nil {
			if last != nil {
				return fmt.Errorf("apiserver /healthz did not become ok (last check: %v): %w", last, ctx.Err())
			}
			return fmt.Errorf("apiserver /healthz did not become ok: %w", ctx.Err())
		}
		out, err := probeAttempt(ctx, probe)
		if strings.TrimSpace(string(out)) == "ok" {
			return nil
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return fmt.Errorf("apiserver /healthz refused the credential in use, which needs get on the "+
				"nonResourceURL /healthz: %w", err)
		}
		if ctx.Err() == nil {
			if err != nil {
				last = err
			} else {
				last = fmt.Errorf("answered %q", strings.TrimSpace(string(out)))
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(HealthzPollInterval):
		}
	}
}

// WaitNodeRegistered polls until the named node object exists, giving up after
// attempts. It ends with ctx at a poll boundary: the poll in flight
// may finish, the sleep between polls is cut short, and no further poll
// starts — the same shape WaitHealthzContext already has — so a run group
// that owns the wait joins it promptly instead of waiting the attempt budget
// out. No sleep follows the last attempt, so with attempts=1 it is one
// registration check and nothing else: the node-registered task's per-tick
// check calls it that way, because the tick is the poll.
func WaitNodeRegistered(ctx context.Context, hostname string, attempts int) error {
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("node %s did not register: %w", hostname, err)
		}
		if KubectlContext(ctx, "get", "node", hostname).Run() == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("node %s did not register: %w", hostname, err)
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("node %s did not register: %w", hostname, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("node %s did not register", hostname)
}

// LabelControlPlaneContext marks a node control-plane (the upgrade orchestrator
// upgrades CP nodes serially-first), bounded by ctx: the kubectl run is killed
// when ctx ends, so the node-registered task that calls it cannot hold its run
// group's shutdown join open against a hung apiserver. Best-effort; requires
// admin.conf, so only callable on a control-plane node.
func LabelControlPlaneContext(ctx context.Context, node string) error {
	return KubectlContext(ctx, "label", "node", node, ControlPlaneLabel+"=true", "--overwrite").Run()
}

// LabelControlPlane marks a node control-plane without a bound; new callers
// want LabelControlPlaneContext.
func LabelControlPlane(node string) error {
	return LabelControlPlaneContext(context.Background(), node)
}

// skeVersionSuffix is what the build appends to the kubelet's version
// (KUBE_GIT_VERSION=v<upstream>-<stratum_name>, build/components/Makefile).
const skeVersionSuffix = "-stratum"

// ParseVersion extracts the version token from a `--version` line such as
// "Kubernetes v1.36.2-stratum". SKE kubelets carry the build suffix, while
// Target is compared against version.K8sVersion, the bare upstream version --
// so the suffix is trimmed here or the node would never look "already at
// target" and every successful upgrade would fail its final verification.
//
// Only that suffix: anything else after a "-" is the upstream version's own
// prerelease or vendor part ("v<k8s>-rc.1"), which is not the release it
// precedes. Cutting at the first "-" read such a kubelet as at target, and
// the cluster upgrade then skipped it.
func ParseVersion(s string) (string, error) {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "v") && strings.Count(f, ".") >= 1 {
			return strings.TrimSuffix(f, skeVersionSuffix), nil
		}
	}
	return "", fmt.Errorf("could not parse a version from %q", strings.TrimSpace(s))
}

// KubeletVersion reports the node version marker: `kubelet --version`.
// kubelet-only on purpose — the control plane runs as static pods,
// so there is no apiserver binary on disk to ask.
func KubeletVersion() (string, error) { return KubeletVersionContext(context.Background()) }

// probeOutput runs a version probe, killed when ctx ends. Its stdout is an
// unlinked file, not a pipe, so Run returns as soon as the probe exits or is
// killed: a pipe leaves Wait copying until every child holding it exits, and a
// WaitDelay to cut that short also failed probes that exited cleanly on a
// loaded node. Stderr stays nil (/dev/null) for the same reason.
func probeOutput(ctx context.Context, path string, args ...string) ([]byte, error) {
	f, err := os.CreateTemp("", "stratum-probe-")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_ = os.Remove(f.Name())
	c := exec.CommandContext(ctx, path, args...)
	c.Stdout = f
	if err := c.Run(); err != nil {
		return nil, err
	}
	// Read what the probe wrote by position, not through f's offset: a child
	// that outlived it shares that offset, so a Seek would move its next write
	// back over the version line.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.NewSectionReader(f, 0, fi.Size()))
}

// KubeletVersionContext is KubeletVersion with the probe killed when ctx ends.
func KubeletVersionContext(ctx context.Context) (string, error) {
	out, err := probeOutput(ctx, filepath.Join(version.BinDir, "kubelet"), "--version")
	if err != nil {
		return "", fmt.Errorf("kubelet --version: %w", err)
	}
	return ParseVersion(string(out))
}

// stratumPath and selfExe are vars so tests can point them at stand-in
// binaries.
var (
	stratumPath = version.StratumInstallPath
	selfExe     = os.Executable
)

// BinaryVersion is the version a stratum binary reports: the first field of
// its `version` top line ("<version> <os>/<arch>"). Asking the binary, rather
// than reading version.StratumVersion, honours a version override (an embedding
// binary reports its own release) and answers for a binary that is not this process.
func BinaryVersion(path string) (string, error) {
	return BinaryVersionContext(context.Background(), path)
}

// BinaryVersionContext is BinaryVersion with the probe killed when ctx ends.
func BinaryVersionContext(ctx context.Context, path string) (string, error) {
	out, err := probeOutput(ctx, path, "version")
	if err != nil {
		return "", fmt.Errorf("%s version: %w", path, err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", fmt.Errorf("%s version: no output", path)
	}
	return f[0], nil
}

// installedStratumVersion is the version of the stratum binary on disk. The
// running process cannot answer: the supervisor outlives `stratum upgrade`, so
// its own build is the one it started as. Only when nothing is installed yet
// (a first `up`) is the running executable the answer; an installed binary
// that cannot report a version is an error, not a reason to stamp the running
// build over it.
func installedStratumVersion() (string, error) {
	return installedStratumVersionContext(context.Background())
}

// installedStratumVersionContext is installedStratumVersion with the probe
// killed when ctx ends.
func installedStratumVersionContext(ctx context.Context) (string, error) {
	if _, err := os.Stat(stratumPath); errors.Is(err, fs.ErrNotExist) {
		exe, err := selfExe()
		if err != nil {
			return "", fmt.Errorf("find the running stratum: %w", err)
		}
		return BinaryVersionContext(ctx, exe)
	}
	return BinaryVersionContext(ctx, stratumPath)
}

// StampNode records, on the node's annotations, the k8s version (the node
// version marker, kubelet --version) and the stratum version it runs. The one
// stamping point: `up`, a join, every kubelet (re)start a supervisor with this
// hook performs, and a controller's `stratum upgrade` call it. It reads the
// installed binaries rather than this process's build, so a supervisor that
// predates an upgrade still stamps what is now on disk.
//
// When the stratum version cannot be determined the k8s version is still
// stamped (it is what VersionAligned reads), the stratum version is left as it
// was, and the error says so.
func StampNode(node string) error { return StampNodeContext(context.Background(), node) }

// StampNodeContext is StampNode bounded by ctx as a whole: the version probes
// and the kubectl are killed when ctx ends, so neither a stalled probe nor a
// request the API server never answers can hold the caller.
func StampNodeContext(ctx context.Context, node string) error {
	return stamp(ctx, node, func(k8s, stratum string) error {
		c := KubectlContext(ctx, stampArgs(node, k8s, stratum)...)
		var stderr bytes.Buffer
		c.Stderr = &stderr
		if err := c.Run(); err != nil {
			if msg := capStderr(stderr.String()); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return nil
	})
}

// maxStderr caps the kubectl stderr carried in an error: enough for the cause,
// not a whole usage dump.
const maxStderr = 200

// capStderr trims s and cuts it to maxStderr runes.
func capStderr(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxStderr {
		return string(r[:maxStderr]) + "..."
	}
	return s
}

// StampOwnNode is StampNode for a worker, which has no admin.conf: it patches
// its own Node with the kubelet's credential. The Node authorizer lets a node
// update Nodes and NodeRestriction leaves annotations alone, as for
// UninstalledAnnotation; the credential exists only once kubelet's
// TLS bootstrap has written kubelet.kubeconfig.
func StampOwnNode(node string) error {
	cs, err := kubeletClientset()
	if err != nil {
		return fmt.Errorf("stamp %s: %w", node, err)
	}
	return stamp(context.Background(), node, func(k8s, stratum string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, stampPatch(k8s, stratum), metav1.PatchOptions{})
		return err
	})
}

// WaitOwnNodeRegistered polls, as the kubelet, until its Node exists, giving
// up at timeout: one deadline covers every attempt, sleep and request.
func WaitOwnNodeRegistered(node string, timeout time.Duration) error {
	err := pollUntil(timeout, 3*time.Second, func(ctx context.Context) error {
		cs, err := kubeletClientset()
		if err != nil {
			return err
		}
		_, err = cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("node %s did not register within %s: %w", node, timeout, err)
	}
	return nil
}

// pollUntil runs check every interval until it succeeds or timeout passes.
// check gets a context that expires at the deadline, so a hung call cannot
// overrun it; the last check error is returned.
func pollUntil(timeout, interval time.Duration, check func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		err := check(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(interval):
		}
	}
}

func kubeletClientset() (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"))
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 15 * time.Second
	return kubernetes.NewForConfig(cfg)
}

// stamp reads the installed versions and hands them to apply.
func stamp(ctx context.Context, node string, apply func(k8s, stratum string) error) error {
	k8s, err := KubeletVersionContext(ctx)
	if err != nil {
		return fmt.Errorf("stamp %s: %w", node, err)
	}
	stratum, verr := installedStratumVersionContext(ctx)
	if err := apply(k8s, stratum); err != nil {
		return err
	}
	if verr != nil {
		return fmt.Errorf("stamped %s on %s but not %s: %w", VersionAnnotation, node, StratumVersionAnnotation, verr)
	}
	return nil
}

// stampPatch is stampArgs as a merge patch; same omission of an unknown stratum.
func stampPatch(k8s, stratum string) []byte {
	ann := map[string]string{VersionAnnotation: k8s}
	if stratum != "" {
		ann[StratumVersionAnnotation] = stratum
	}
	b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	return b
}

// stampArgs omits the stratum version when it is unknown (""), leaving what the
// node already carries.
func stampArgs(node, k8s, stratum string) []string {
	args := []string{"annotate", "node", node, VersionAnnotation + "=" + k8s}
	if stratum != "" {
		args = append(args, StratumVersionAnnotation+"="+stratum)
	}
	return append(args, "--overwrite")
}
